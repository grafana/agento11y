"""Tests for the experiment surface (sigil_sdk.experiments).

Exercises the ergonomic Experiment/Trial API against a fake client that captures
exported scores, and asserts the bilingual OTel telemetry (trial span identity
attributes + gen_ai.evaluation.result events) via an in-memory span exporter.
"""

from __future__ import annotations

import sys
from pathlib import Path
from types import ModuleType

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from sigil_sdk.experiments import (
    Evaluator,
    Experiment,
    TestCase,
    TestSuite,
    Trial,
    TrialRef,
    otel,
    score,
)
from sigil_sdk.models import CreateExperimentRequest, ScoreItem


class FakeClient:
    """Captures upsert/score/finalize calls instead of hitting the network."""

    def __init__(self, *, use_experimental_otel: bool = False) -> None:
        self.use_experimental_otel = use_experimental_otel
        self.upserts: list[CreateExperimentRequest] = []
        self.scores: list[ScoreItem] = []
        self.finalized: list[tuple[str, str, int]] = []
        self.generations: list[str] = []
        self.trials: list[tuple] = []
        self.trial_updates: list[tuple] = []
        self.artifacts: list[tuple] = []
        self.calls: list[str] = []

    def upsert_experiment(self, request: CreateExperimentRequest):
        self.upserts.append(request)
        return None

    def export_scores(self, scores, *, raise_on_reject: bool = True) -> int:
        self.calls.append("export_scores")
        self.scores.extend(scores)
        return len(scores)

    def record_generation(self, generation_id, **kwargs) -> str:
        self.generations.append(generation_id)
        return generation_id

    def flush_generations(self) -> None:
        self.calls.append("flush_generations")

    def upsert_trial(self, experiment_id, *, trial_id, **kwargs) -> dict:
        self.trials.append((experiment_id, trial_id, kwargs.get("status")))
        return {"trial_id": trial_id}

    def update_trial(self, experiment_id, trial_id, **kwargs) -> dict:
        self.trial_updates.append((experiment_id, trial_id, kwargs.get("status")))
        return {"trial_id": trial_id}

    def upload_artifact(
        self,
        *,
        parent_id,
        name,
        kind,
        content,
        mime="",
        parent_kind="test_case_trial",
        experiment_id="",
    ) -> dict:
        self.artifacts.append((experiment_id, parent_id, name, kind, mime, content))
        return {"artifact_id": f"art_{name}", "name": name, "kind": kind}

    def finalize(self, experiment_id, status="succeeded", *, score_count=None, error=""):
        self.finalized.append((experiment_id, status, score_count or 0))
        return None

    def experiment_url(self, experiment_id: str) -> str:
        return f"http://ui/{experiment_id}"


class FailingExportClient(FakeClient):
    def export_scores(self, scores, *, raise_on_reject: bool = True) -> int:
        self.calls.append("export_scores")
        raise RuntimeError("score export failed")


def _span_exporter() -> InMemorySpanExporter:
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    return exporter


def _suite() -> TestSuite:
    return TestSuite(
        suite_id="smoke",
        name="Smoke Suite",
        version="1.2.0",
        test_cases=[TestCase(test_case_id="add", name="Addition", input="2+2", expected="4")],
    )


def test_experiment_lifecycle_and_score_wire_fields() -> None:
    client = FakeClient()
    suite = _suite()
    verifier = Evaluator(evaluator_id="exact", version="1", kind="deterministic")

    with Experiment(client, experiment_id="run-1", name="smoke run", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(1.0, passed=True, explanation="matched", evaluator=verifier)
            trial.check_score("json_valid", passed=True)
            trial.rubric_score("helpfulness", 0.9, explanation="clear")

    # Upsert sent experiment_id (not run_id) and source=external.
    assert len(client.upserts) == 1
    assert client.upserts[0].run_id == "run-1"

    # Three scores, all attributed to the run + trial + test case.
    assert len(client.scores) == 3
    keys = {s.score_key for s in client.scores}
    assert keys == {"final", "json_valid", "helpfulness"}
    # One typed trial was created (so the report rolls up) and finalized.
    assert len(client.trials) == 1
    assert client.trials[0][0] == "run-1"
    assert client.trial_updates and client.trial_updates[0][2] == "completed"

    for s in client.scores:
        assert s.experiment_id == "run-1"
        assert s.test_case_id == "add"
        assert s.resolved_experiment_id == "run-1"
        # The typed trial_id attributes the score (the trial was created on enter).
        assert s.trial_id == client.scores[0].trial_id
        assert s.trial_id  # non-empty
        assert s.metadata["task_id"] == "add"
        # No record_io()/bind_generation(): scores rely on trial_id, no generation.
        assert s.generation_id == ""
    assert client.generations == []

    # Headline score carries the verdict; evaluator kinds mapped to OTel set.
    final = next(s for s in client.scores if s.score_key == "final")
    assert final.passed is True
    assert final.evaluator_kind == "deterministic"
    rubric = next(s for s in client.scores if s.score_key == "helpfulness")
    assert rubric.evaluator_kind == "llm_judge"

    # Finalized completed (the backend's terminal-success status) with the count.
    assert client.finalized == [("run-1", "completed", 3)]


def test_trial_span_emits_otel_eval_telemetry() -> None:
    exporter = _span_exporter()
    client = FakeClient(use_experimental_otel=True)
    suite = _suite()
    verifier = Evaluator(evaluator_id="exact", version="2", kind="deterministic")

    with Experiment(client, experiment_id="run-2", name="telemetry run", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(0.5, passed=False, explanation="off by one", evaluator=verifier)

    spans = exporter.get_finished_spans()
    assert len(spans) == 1
    span = spans[0]
    assert span.name == "eval.trial add"

    attrs = dict(span.attributes)
    # test.* identity, emitted directly (no sigil.* mirror).
    assert attrs[otel.TEST_SUITE_RUN_ID] == "run-2"
    assert attrs[otel.TEST_CASE_ID] == "add"
    assert attrs[otel.TEST_SUITE_VERSION] == "1.2.0"
    assert attrs[otel.TEST_SUITE_RUN_STATUS] == "in_progress"
    # Proposed per-attempt trial identity.
    assert attrs[otel.TEST_CASE_RUN_ATTEMPT] == 1
    assert attrs[otel.TEST_CASE_RUN_ID]  # the trial id
    # Verdict maps onto the merged pass|fail enum (this trial failed).
    assert attrs[otel.TEST_CASE_RESULT_STATUS] == "fail"
    # The schema-version marker is the one sigil.* attribute we keep.
    assert attrs[otel.ATTR_SCHEMA_VERSION] == otel.SCHEMA_VERSION
    # No sigil.* mirrors are emitted.
    assert not any(str(k).startswith("sigil.eval.experiment") for k in attrs)
    assert not any(str(k).startswith("sigil.test_case_trial") for k in attrs)

    # The eval result event uses OTel names; the verdict is the score label.
    events = [e for e in span.events if e.name == otel.EVENT_EVAL_RESULT]
    assert len(events) == 1
    eattrs = dict(events[0].attributes)
    assert eattrs[otel.EVAL_NAME] == "final"
    assert eattrs[otel.EVAL_SCORE_VALUE] == 0.5
    assert eattrs[otel.EVAL_SCORE_LABEL] == "fail"
    assert eattrs[otel.EVAL_EVALUATOR_ID] == "exact"
    assert eattrs[otel.EVAL_EVALUATOR_TYPE] == "deterministic"
    assert "sigil.eval.score.passed" not in eattrs  # verdict is the label, no mirror
    assert eattrs[otel.EVAL_EXPLANATION] == "off by one"


def test_trial_span_is_disabled_by_default() -> None:
    exporter = _span_exporter()
    client = FakeClient()
    suite = _suite()

    with Experiment(client, experiment_id="run-no-otel", name="no telemetry", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(1.0, passed=True)

    assert not exporter.get_finished_spans()


def test_trial_from_ref_cross_process() -> None:
    client = FakeClient()
    ref = TrialRef(experiment_id="run-3", test_case_id="case-x", attempt=2, suite_id="s1")
    trial = Trial.from_ref(client, ref)
    with trial:
        trial.bind_conversation("conv-123")
        trial.final_score(True)
    assert len(client.scores) == 1
    s = client.scores[0]
    assert s.experiment_id == "run-3"
    assert s.test_case_id == "case-x"
    assert s.conversation_id == "conv-123"
    assert s.value.boolean is True
    assert s.passed is True  # boolean final score derives the verdict


def test_trial_ref_env_round_trip() -> None:
    ref = TrialRef(experiment_id="run-4", test_case_id="c1", attempt=3, suite_id="s", suite_version="2.0.0")
    env = ref.to_env()
    assert env["SIGIL_EXPERIMENT_ID"] == "run-4"
    assert env["SIGIL_ATTEMPT"] == "3"
    restored = TrialRef.from_env(env)
    assert restored is not None
    assert restored.experiment_id == "run-4"
    assert restored.test_case_id == "c1"
    assert restored.attempt == 3


def test_trial_from_ref_requires_ref() -> None:
    with pytest.raises(ValueError, match="trial ref is required"):
        Trial.from_ref(FakeClient(), None)


def test_no_synthetic_conversation_id_when_unbound() -> None:
    # Regression: a trial that never binds/records a real conversation must NOT
    # emit a conversation_id, or the experiments UI offers an "Open conversation"
    # link that 404s (no backing generation).
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-x", name="x", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(1.0, passed=True)
        assert trial.conversation_id == ""
    assert all(s.conversation_id == "" for s in client.scores)
    assert client.generations == []  # nothing ingested, so nothing to link


def test_record_io_mints_real_conversation() -> None:
    # When a real generation IS recorded, the trial gets a conversation id and the
    # scores carry it (the conversation is openable).
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-y", name="y", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.record_io(input="2+2", output="4")
            trial.final_score(1.0, passed=True)
        conv = trial.conversation_id
    assert conv != ""
    assert client.generations == [trial.generation_id]
    assert all(s.conversation_id == conv for s in client.scores)


def test_trial_without_final_score_fails() -> None:
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-no-final", name="x", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.check_score("json_valid", passed=True)
    assert trial.status == "failed"
    assert trial.error == "trial exited without a final score"
    assert client.trial_updates and client.trial_updates[0][2] == "completed"


def test_trial_cleanup_runs_when_flush_fails() -> None:
    client = FailingExportClient()
    suite = _suite()
    with pytest.raises(RuntimeError, match="score export failed"):
        with Experiment(client, experiment_id="run-flush-fail", name="x", suite=suite) as exp:
            with exp.trial(suite.test_cases[0]) as trial:
                trial.final_score(1.0, passed=True)
    assert client.trial_updates and client.trial_updates[0][2] == "completed"
    assert len(trial._buffer) == 1
    assert client.calls[:2] == ["flush_generations", "export_scores"]


def test_trial_flush_flushes_generation_client_before_scores() -> None:
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-flush-order", name="x", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.bind_generation("gen-existing", conversation_id="conv-existing")
            trial.final_score(1.0, passed=True)
    assert client.calls[:2] == ["flush_generations", "export_scores"]


def test_score_value_helpers() -> None:
    assert score.number(0.5).number == 0.5
    assert score.boolean(True).boolean is True
    assert score.string("x").string == "x"


def test_suite_from_dict_and_cases_alias() -> None:
    suite = TestSuite.from_dict(
        {
            "id": "regression",
            "name": "Dashboard regression",
            "version": "2.0.0",
            "cases": [
                {"id": "panels", "input": "add panels", "expected": "ok", "tags": ["dash"]},
                {"test_case_id": "annot", "name": "Annotation"},
            ],
        }
    )
    assert suite.suite_id == "regression"
    assert suite.version == "2.0.0"
    assert [c.test_case_id for c in suite.cases] == ["panels", "annot"]
    assert suite.cases is suite.test_cases
    assert suite.case("panels").tags == ["dash"]


def test_suite_from_yaml(tmp_path) -> None:
    path = tmp_path / "suite.yaml"
    path.write_text(
        "suite_id: y\nname: From YAML\ncases:\n  - id: a\n    input: hi\n  - id: b\n",
        encoding="utf-8",
    )
    suite = TestSuite.from_yaml(str(path))
    assert suite.suite_id == "y"
    assert [c.test_case_id for c in suite.cases] == ["a", "b"]


def test_candidate_dict_coercion_into_experiment() -> None:
    client = FakeClient()
    suite = _suite()
    with Experiment(
        client,
        experiment_id="run-cand",
        name="cand",
        suite=suite,
        candidate={"git_sha": "abc123", "model_name": "gpt-5", "unknown": "ignored"},
    ) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.score("final", value=0.8, passed=True, explanation="good")
    md = client.upserts[0].metadata
    assert md["git_sha"] == "abc123"
    assert md["model_name"] == "gpt-5"
    final = next(s for s in client.scores if s.score_key == "final")
    assert final.passed is True
    assert final.value.number == 0.8


def test_trial_artifact_upload() -> None:
    client = FakeClient()
    suite = _suite()
    with Experiment(client, experiment_id="run-art", name="art", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            trial.final_score(1.0, passed=True)
            rec_json = trial.artifact("grading-details", data={"passed": True}, kind="json")
            trial.artifact("notes", text="looks good")
    assert rec_json["artifact_id"] == "art_grading-details"
    names = {a[2] for a in client.artifacts}
    assert names == {"grading-details", "notes"}
    json_upload = next(a for a in client.artifacts if a[2] == "grading-details")
    assert json_upload[0] == "run-art"
    assert json_upload[3] == "json" and json_upload[4] == "application/json"
    assert trial.artifacts[0]["artifact_id"] == "art_grading-details"


def test_experiment_factory_uses_supplied_client() -> None:
    client = FakeClient()
    suite = _suite()
    from sigil_sdk.experiments import experiment as experiment_factory

    with experiment_factory("factory run", suite=suite, client=client, experiment_id="run-f") as exp:
        assert exp._owns_client is False
        with exp.trial(suite.test_cases[0]) as trial:
            trial.score("final", value=True)
    assert client.finalized and client.finalized[0][0] == "run-f"


def test_final_score_primitive_derives_boolean_verdict() -> None:
    client = FakeClient()
    suite = _suite()

    with Experiment(client, experiment_id="run-primitive-final", name="x", suite=suite) as exp:
        with exp.trial(suite.test_cases[0]) as trial:
            final = trial.score("final", value=True)

    assert final.passed is True
    assert trial.status == "passed"
    assert client.scores[0].passed is True


def test_artifact_content_kind_inference() -> None:
    from sigil_sdk.experiments.experiment import _artifact_content, _kind_from_mime

    assert _kind_from_mime("image/png") == "image"
    assert _kind_from_mime("application/pdf") == "pdf"
    assert _kind_from_mime("text/markdown") == "markdown"
    assert _kind_from_mime("application/octet-stream") == "binary"
    content, kind, mime = _artifact_content("", {"a": 1}, "", "", "")
    assert kind == "json" and mime == "application/json" and b'"a"' in content


def test_trial_result_status_only_pass_fail() -> None:
    # test.case.result.status is the merged pass|fail enum; non-verdict states omit it.
    assert otel.trial_status_telemetry("passed") == "pass"
    assert otel.trial_status_telemetry("completed") == "pass"
    assert otel.trial_status_telemetry("failed") == "fail"
    for state in ("running", "errored", "skipped", ""):
        assert otel.trial_status_telemetry(state) == ""


def test_non_verdict_states_omit_result_status() -> None:
    # errored/skipped/running carry no pass|fail verdict, so the attribute is omitted.
    for state in ("errored", "skipped", "running"):
        attrs = otel.trial_identity_attributes(
            experiment_id="run-e", test_case_id="c1", trial_id="t1", trial_status=state
        )
        assert otel.TEST_CASE_RESULT_STATUS not in attrs
    # a passed trial does carry it.
    passed = otel.trial_identity_attributes(experiment_id="r", test_case_id="c", trial_status="passed")
    assert passed[otel.TEST_CASE_RESULT_STATUS] == "pass"


def test_parse_report_matches_backend_shape() -> None:
    # Regression: the report keys the run under `experiment` and cost under
    # `total_cost` (older drafts used `run` / `total_cost_usd`).
    from sigil_sdk._experiments_transport import _parse_report

    payload = {
        "experiment": {"experiment_id": "run-9", "name": "nightly", "status": "completed"},
        "summary": {
            "test_case_count": 2,
            "trial_count": 6,
            "completed_count": 6,
            "pass_rate": 0.5,
            "pass_at_k": {"1": 0.5, "3": 0.83},
            "pass_power_k": {"1": 0.5},
            "final_score_avg": 0.71,
            "total_cost": 0.0123,
            "total_tokens": 4096,
        },
        "rows": [{"test_case_id": "add", "trials": []}],
    }
    report = _parse_report(payload)
    assert report.run.run_id == "run-9"  # not blank (parsed from the `experiment` key)
    assert report.run.name == "nightly"
    assert report.summary.total_cost == 0.0123  # not dropped
    assert report.summary.total_tokens == 4096
    assert report.summary.pass_at_k == {"1": 0.5, "3": 0.83}
    assert report.summary.pass_power_k == {"1": 0.5}
    assert report.summary.final_score_avg == 0.71
    assert report.rows and report.rows[0]["test_case_id"] == "add"


def test_example_json_fallbacks_handle_malformed_slices(monkeypatch: pytest.MonkeyPatch) -> None:
    example_root = Path(__file__).resolve().parents[2] / "examples" / "experiments" / "python"
    fake_anthropic = ModuleType("anthropic")
    fake_anthropic.Anthropic = object
    monkeypatch.setitem(sys.modules, "anthropic", fake_anthropic)
    sys.path.insert(0, str(example_root))
    try:
        from app.agent import _parse_grade
        from app.dashboard_agent import _parse_json
    finally:
        sys.path.pop(0)

    assert _parse_grade("prefix {not json} suffix") == {
        "score": 0.0,
        "passed": False,
        "explanation": "graded by LLM judge",
    }
    assert _parse_json("prefix {not json} suffix") == {}

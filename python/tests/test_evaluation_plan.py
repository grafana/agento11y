from copy import deepcopy
from types import SimpleNamespace

import pytest
from agento11y import conversation_id_from_context
from agento11y.errors import ConflictError, NotFoundError
from agento11y.experiments import (
    AgentOutput,
    Check,
    EvaluationPlan,
    EvaluatorsClient,
    ExactMatch,
    LLMJudge,
    RegexJudge,
    StoredEvaluator,
    TestSuite,
    case_value,
    run_evals,
    text_case,
)
from agento11y.experiments.cases import render_case_prompt
from test_experiments import FakeClient


def test_text_case_and_nested_placeholders_preserve_types_and_do_not_reexpand():
    case = text_case("a", "question", expected="{{output}}", rubric="be precise", context=["source"])
    assert case.metadata["case_format"] == "grafana.text.v1"
    assert case_value(case, "input.context.0") == "source"
    assert (
        render_case_prompt("{{test_case.expected.assistant_response}} / {{output}}", case, "answer")
        == "{{output}} / answer"
    )
    assert render_case_prompt("{input}", case, "answer").startswith('{"prompt":')
    with pytest.raises(ValueError, match="missing"):
        case_value(case, "expected.answer")
    with pytest.raises(ValueError, match="invalid"):
        case_value(case, "expected[0]")


def test_mixed_local_plan_has_one_primary_and_separate_grader_evidence():
    client = FakeClient()
    seen = []
    judge = LLMJudge.for_case(
        "correctness",
        lambda prompt: seen.append(prompt) or '{"score":1,"passed":true}',
        model_name="model",
        model_provider="provider",
    )
    plan = EvaluationPlan(
        [
            Check("reference", judge),
            Check("exact", ExactMatch()),
            Check("style", RegexJudge("style", "never"), required=False),
        ]
    )
    suite = TestSuite("suite", test_cases=[text_case("a", "q", expected="yes"), text_case("b", "q", expected="no")])

    def target(_):
        assert conversation_id_from_context()
        return "yes"

    run_evals(suite, target, plan=plan, client=client)
    assert conversation_id_from_context() is None
    primary = [s for s in client.scores if s.report_role == "primary_verdict"]
    assert [s.passed for s in primary] == [True, False]
    assert len(client.scores) == 8
    assert len(client.generations) == 2  # Judge evidence only; no invented agent generations.
    assert all("Candidate: yes" in prompt for prompt in seen)
    assert client.upserts[0].planned_trial_count == 2
    assert len(client.upserts[0].metadata["evaluation_plan"]) == 3


def test_validate_all_cases_before_target_or_provisioning():
    judge = StoredEvaluator.llm_judge("quality", provider="provider", model="model")
    plan = EvaluationPlan([Check("quality", judge)])
    client = FakeClient()
    suite = TestSuite("suite", test_cases=[text_case("ok", "q", expected="a"), text_case("missing", "q")])

    def forbidden(*_):
        pytest.fail("must validate before invoking target or provisioning")

    with pytest.raises(ValueError, match="missing.*expected.assistant_response"):
        run_evals(suite, forbidden, client=client, plan=plan, evaluators_client=SimpleNamespace(ensure=forbidden))
    assert client.upserts == []


@pytest.mark.parametrize("required", [True, False])
def test_judge_error_keeps_sibling_results_and_is_never_a_negative_judgment(required):
    def fail(_):
        raise RuntimeError("judge unavailable")

    client = FakeClient()
    plan = EvaluationPlan(
        [Check("broken", LLMJudge("broken", fail, "model"), required=required), Check("exact", ExactMatch())]
    )
    run_evals(
        TestSuite("suite", test_cases=[text_case("a", "q", expected="a")]), lambda _: "a", client=client, plan=plan
    )
    assert any(s.score_key == "exact" and s.passed for s in client.scores)
    error = next(s for s in client.scores if s.score_key == "broken.error")
    assert error.passed is None
    assert bool([s for s in client.scores if s.report_role == "primary_verdict"]) is not required
    assert client.trial_updates[-1][2] == ("failed" if required else "completed")


def test_target_error_and_flush_error_do_not_discard_other_cases():
    client = FakeClient()
    suite = TestSuite("suite", test_cases=[text_case(str(i), str(i), expected="ok") for i in range(3)])

    def target(prompt):
        if prompt == "0":
            raise RuntimeError("provider down")
        if prompt == "1":

            def flush():
                raise RuntimeError("export failed")

            return AgentOutput("ok", flush=flush)
        return "ok"

    run_evals(suite, target, client=client, plan=EvaluationPlan([Check("exact", ExactMatch())]))
    assert [u[2] for u in client.trial_updates] == ["failed", "failed", "completed"]


class Control:
    def __init__(self):
        self.stored = {}
        self.posts = 0

    def _request(self, method, path, *, payload=None):
        if method == "GET":
            if path.split("/")[-1] not in self.stored:
                raise NotFoundError("missing")
            return deepcopy(self.stored[path.split("/")[-1]])
        self.posts += 1
        current = self.stored.get(payload["evaluator_id"])
        if current and current["version"] == payload["version"]:
            raise ConflictError("exists")
        self.stored[payload["evaluator_id"]] = deepcopy(payload)
        return deepcopy(payload)


def test_provisioning_reuses_identical_definitions_and_never_overwrites():
    control = Control()
    client = EvaluatorsClient(control=control)
    first = StoredEvaluator.llm_judge("quality", provider="provider", model="a")
    second = StoredEvaluator.llm_judge("quality", provider="provider", model="b")
    assert first.evaluator_id != second.evaluator_id
    assert first.version != second.version
    client.ensure(first)
    client.ensure(first)
    assert control.posts == 1
    client.ensure(second)
    changed = StoredEvaluator(first.evaluator_id, first.version, {"different": True}, first.output_keys)
    with pytest.raises(ValueError, match="differs"):
        client.ensure(changed)
    assert control.posts == 2


def test_stored_evaluator_definition_is_deeply_immutable():
    config = {"model": "original", "nested": {"temperatures": [0]}}
    output_keys = [{"key": "quality", "options": {"pass": True}}]
    definition = StoredEvaluator("quality", "v1", config, output_keys)

    config["model"] = "changed"
    config["nested"]["temperatures"][0] = 1
    output_keys[0]["key"] = "changed"
    output_keys[0]["options"]["pass"] = False
    assert definition.payload()["config"]["model"] == "original"
    assert definition.payload()["config"]["nested"]["temperatures"] == [0]
    assert definition.payload()["output_keys"][0]["key"] == "quality"
    assert definition.payload()["output_keys"][0]["options"]["pass"] is True
    with pytest.raises(TypeError):
        definition.config["model"] = "changed"
    with pytest.raises(TypeError):
        definition.config["nested"]["temperatures"][0] = 1
    with pytest.raises(TypeError):
        definition.output_keys[0]["key"] = "changed"


def test_exact_match_can_select_case_metadata():
    client = FakeClient()
    case = text_case("metadata-reference", "question", metadata={"reference": "answer"})
    run_evals(
        TestSuite("suite", test_cases=[case]),
        lambda _: "answer",
        client=client,
        plan=EvaluationPlan([Check("exact", ExactMatch("metadata.reference"))]),
    )
    assert [score.passed for score in client.scores if score.report_role == "primary_verdict"] == [True]


def test_remote_diagnostic_is_aggregated_after_agent_flush_and_local_check():
    definition = StoredEvaluator("quality", "v1", {}, [{"key": "ok", "type": "bool"}])
    client = FakeClient()
    stored = []

    def trigger(run_id, trial_id, evaluator_id, version, **kwargs):
        assert client.evaluation_order[-1] == "flush"
        assert kwargs["report_role"] == "diagnostic"
        stored.append(
            dict(trial_id=trial_id, evaluator_id=evaluator_id, evaluator_version=version, score_key="ok", passed=False)
        )
        return client.evaluation_result

    client.trigger_trial_evaluation = trigger
    client.list_scores = lambda *args, **kwargs: (stored, None)
    plan = EvaluationPlan([Check("remote", definition), Check("exact", ExactMatch())])
    run_evals(
        TestSuite("suite", test_cases=[text_case("a", "q", expected="a")]),
        lambda _: AgentOutput("a", "original", flush=lambda: client.calls.append("target_flush")),
        client=client,
        plan=plan,
        evaluators_client=EvaluatorsClient(control=Control()),
    )
    assert client.trial_updates[-1][3]["conversation_id"] == "original"
    assert [s.passed for s in client.scores if s.report_role == "primary_verdict"] == [False]


@pytest.mark.parametrize("mode", ["rubric", "reference_free"])
def test_explicit_no_reference_modes(mode):
    judge = LLMJudge.for_case(
        "judge", lambda _: '{"score":1,"passed":true}', model_name="m", model_provider="p", mode=mode
    )
    plan = EvaluationPlan([Check("quality", judge)])
    plan.validate([text_case("case", "question", rubric="be relevant")])


def test_custom_input_does_not_expose_expected_or_mutate_snapshot():
    case = text_case("a", "question", context=["source"], expected="secret reference")
    received = []

    def target(value):
        received.append(deepcopy(value))
        value["context"].clear()
        return "secret reference"

    run_evals(
        TestSuite("suite", test_cases=[case]),
        target,
        input_selector="input",
        client=FakeClient(),
        plan=EvaluationPlan([Check("exact", ExactMatch())]),
    )
    assert received == [{"prompt": "question", "context": ["source"]}]
    assert case.input["context"] == ["source"]


@pytest.mark.parametrize("score", ["NaN", "Infinity", "true"])
def test_invalid_judge_numbers_never_become_passes(score):
    judge = LLMJudge("judge", lambda _: '{"score":' + score + ',"passed":true}', "model")
    with pytest.raises(ValueError, match="numeric"):
        judge.evaluate_output(input="q", output="a", expected="a")


def test_provisioning_accepts_only_known_server_default_normalization():
    desired = StoredEvaluator(
        "regex", "1", {"target": "response", "pattern": "yes"}, [{"key": "ok", "type": "bool"}], kind="regex"
    ).payload()
    stored = deepcopy(desired)
    stored["config"].pop("target")
    EvaluatorsClient._verify(stored, desired)
    stored["config"]["pattern"] = "no"
    with pytest.raises(ValueError):
        EvaluatorsClient._verify(stored, desired)


def test_setup_reuses_application_providers_and_requires_endpoint_for_new_ones(monkeypatch):
    from agento11y.experiments import setup_evals
    from opentelemetry import metrics, trace

    existing_trace, existing_meter = object(), object()
    monkeypatch.setattr(trace, "get_tracer_provider", lambda: existing_trace)
    monkeypatch.setattr(metrics, "get_meter_provider", lambda: existing_meter)
    setup_evals().shutdown()  # Must neither replace nor call shutdown on these objects.
    monkeypatch.setattr(trace, "get_tracer_provider", lambda: trace.ProxyTracerProvider())
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_ENDPOINT", raising=False)
    monkeypatch.delenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", raising=False)
    with pytest.raises(ValueError, match="OTEL_EXPORTER_OTLP_ENDPOINT"):
        setup_evals()


def test_setup_only_shuts_down_owned_provider(monkeypatch):
    from agento11y.experiments import setup_evals
    from opentelemetry import metrics, trace
    from opentelemetry.exporter.otlp.proto.http import trace_exporter
    from opentelemetry.sdk import trace as sdk_trace
    from opentelemetry.sdk.trace import export

    calls = []
    owned = SimpleNamespace(
        add_span_processor=lambda _: None,
        force_flush=lambda: calls.append("flush"),
        shutdown=lambda: calls.append("shutdown"),
    )
    monkeypatch.setattr(trace, "get_tracer_provider", lambda: trace.ProxyTracerProvider())
    monkeypatch.setattr(metrics, "get_meter_provider", lambda: object())
    monkeypatch.setattr(sdk_trace, "TracerProvider", lambda **_: owned)
    monkeypatch.setattr(trace, "set_tracer_provider", lambda value: calls.append(value))
    monkeypatch.setattr(export, "BatchSpanProcessor", lambda value: value)
    monkeypatch.setattr(trace_exporter, "OTLPSpanExporter", lambda **kw: calls.append(kw) or object())
    helper = setup_evals(otlp_endpoint="http://localhost:4318")
    helper.flush()
    helper.shutdown()
    helper.shutdown()
    assert calls == [{"endpoint": "http://localhost:4318/v1/traces"}, owned, "flush", "shutdown"]

"""Tests for the LangGraph experiment runner."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any
from uuid import uuid4

import pytest
from opentelemetry import trace
from sigil_sdk import (
    Client,
    ClientConfig,
    ExperimentStatus,
    ExportScoreResult,
    ExportScoresResponse,
    GenerationExportConfig,
    ScoreExportError,
    ScoreItem,
    ScoreValue,
)
from sigil_sdk.exporters import NoopGenerationExporter
from sigil_sdk.models import ExportGenerationResult, ExportGenerationsResponse
from sigil_sdk_langgraph import (
    DatasetItem,
    ExperimentRun,
    ExperimentRunner,
    ScoreOutput,
    TargetResult,
    experiment,
)
from sigil_sdk_langgraph.handler import SigilLangGraphHandler


@dataclass
class _FakeClient:
    """Captures experiment lifecycle and score-export calls."""

    created: list[Any] = field(default_factory=list)
    exported: list[list[ScoreItem]] = field(default_factory=list)
    completed: list[tuple[str, str, int | None, str | None]] = field(default_factory=list)
    flushes: int = 0
    complete_failures: int = 0
    reject_scores: bool = False

    def create_experiment(self, request):
        self.created.append(request)
        return request

    def flush(self):
        self.flushes += 1

    def export_scores(self, scores):
        self.exported.append(list(scores))
        return ExportScoresResponse(
            results=[
                ExportScoreResult(s.score_id, not self.reject_scores, "rejected" if self.reject_scores else "")
                for s in scores
            ]
        )

    def complete_experiment(self, run_id, status, *, score_count=None, error=None):
        if self.complete_failures > 0:
            self.complete_failures -= 1
            raise RuntimeError("complete failed")
        status_value = getattr(status, "value", status)
        self.completed.append((run_id, status_value, score_count, error))

    def experiment_url(self, run_id):
        return f"https://sigil.example/a/grafana-sigil-app/evaluation/experiments/{run_id}"

    def get_experiment_report(self, run_id):  # pragma: no cover - unused unless fetch_report
        raise NotImplementedError


def _reward_scorer(item: DatasetItem, result: TargetResult):
    return [
        ScoreOutput(
            evaluator_id="smoke.reward",
            evaluator_version="2026-05-28",
            score_key="reward",
            value=ScoreValue(number=1.0 if result.output == item.expected else 0.0),
            passed=result.output == item.expected,
            metadata={"task_id": item.metadata.get("task_id", item.id)},
        )
    ]


def test_make_handler_propagates_run_id_to_tags_and_metadata() -> None:
    client = Client(
        ClientConfig(
            generation_exporter=NoopGenerationExporter(),
            tracer=trace.get_tracer("sigil-langgraph-experiment-test"),
        )
    )
    try:
        run = ExperimentRun(
            client=client,
            run_id="run_abc",
            name="PR 123",
            dataset=None,
            candidate=None,
            upload="continuous",
            handler_kwargs={"agent_name": "support-bot"},
            async_handler=False,
        )
        handler = run.make_handler(
            extra_tags={"experiment.run_id": "wrong"},
            extra_metadata={"experiment_run_id": "wrong"},
        )
        assert isinstance(handler, SigilLangGraphHandler)
        assert handler._extra_tags["experiment.run_id"] == "run_abc"
        assert handler._extra_metadata["experiment_run_id"] == "run_abc"

        config = run.langgraph_config({"configurable": {"thread_id": "t1"}})
        callbacks = config["callbacks"]
        assert any(isinstance(cb, SigilLangGraphHandler) for cb in callbacks)
        assert config["configurable"] == {"thread_id": "t1"}
    finally:
        client.shutdown()


class _CapturingExporter:
    def __init__(self) -> None:
        self.requests: list[Any] = []

    def export_generations(self, request):
        self.requests.append(request)
        return ExportGenerationsResponse(
            results=[ExportGenerationResult(generation_id=g.id, accepted=True) for g in request.generations]
        )

    def shutdown(self) -> None:
        return


def test_langgraph_config_tags_generations_and_captures_ids() -> None:
    exporter = _CapturingExporter()
    client = Client(
        ClientConfig(
            generation_export=GenerationExportConfig(batch_size=10, flush_interval=timedelta(seconds=60)),
            generation_exporter=exporter,
            tracer=trace.get_tracer("sigil-langgraph-experiment-e2e"),
        )
    )
    try:
        run = ExperimentRun(
            client=client,
            run_id="run_e2e",
            name="e2e",
            dataset=None,
            candidate=None,
            upload="continuous",
            handler_kwargs={"agent_name": "support-bot"},
            async_handler=False,
        )
        config = run.langgraph_config()
        handler = config["callbacks"][0]
        # langgraph_config injects a conversation id into the config metadata.
        conv_id = config["metadata"]["conversation_id"]
        assert conv_id != ""
        assert run.active_conversation_id == conv_id

        llm_run_id = uuid4()
        # LangChain propagates the config metadata to the callback; mirror that so
        # the generation picks up the injected conversation id.
        handler.on_chat_model_start(
            {"name": "ChatOpenAI"},
            [[{"type": "human", "content": "hello"}]],
            run_id=llm_run_id,
            invocation_params={"model": "gpt-5"},
            metadata={"conversation_id": conv_id},
        )
        handler.on_llm_end(
            {"generations": [[{"text": "world"}]], "llm_output": {"model_name": "gpt-5"}},
            run_id=llm_run_id,
        )
        client.flush()

        generation = exporter.requests[0].generations[0]
        assert generation.tags["experiment.run_id"] == "run_e2e"
        assert generation.metadata["experiment_run_id"] == "run_e2e"
        # generation carries the injected conversation id so scores can link to it
        assert generation.conversation_id == conv_id
        # the run captured the produced generation id for score attribution
        assert run.produced_generation_ids == [generation.id]
    finally:
        client.shutdown()


def test_runner_links_scores_to_generated_conversation_when_target_omits_it() -> None:
    from sigil_sdk_langgraph import stable_id

    client = _FakeClient()
    items = [DatasetItem(id="it1", input="x", expected="y")]

    def target(item: DatasetItem, run: ExperimentRun) -> TargetResult:
        # Target does not set conversation_id (the common case); the run assigns
        # one and langgraph_config would tag generations with it.
        return TargetResult(output="y", generation_ids=["gen-1"])

    runner = ExperimentRunner(
        client=client,
        run_id="run_link",
        name="link",
        fetch_report=False,
        print_url=False,
    )
    runner.run(items, target, [_reward_scorer])

    score = client.exported[0][0]
    assert score.conversation_id == stable_id("conv", "run_link", "it1")
    assert score.conversation_id != ""


def test_continuous_run_exports_scores_and_finalizes_succeeded() -> None:
    client = _FakeClient()
    items = [
        DatasetItem(id="it1", input="2+2", expected="4", metadata={"task_id": "math"}),
        DatasetItem(id="it2", input="cap of FR", expected="Paris"),
    ]

    def target(item: DatasetItem, run: ExperimentRun) -> TargetResult:
        return TargetResult(output=item.expected, generation_ids=[f"gen-{item.id}"], conversation_id=f"conv-{item.id}")

    runner = ExperimentRunner(
        client=client,
        run_id="run_1",
        name="smoke",
        dataset={"id": "support_smoke", "version": "2026-05-28"},
        candidate={"git_sha": "abc123"},
        tags=["smoke"],
        fetch_report=False,
        print_url=False,
    )
    result = runner.run(items, target, [_reward_scorer])

    assert client.created[0].run_id == "run_1"
    assert client.created[0].source == "external"
    assert client.created[0].metadata["dataset_id"] == "support_smoke"
    # continuous mode flushes + exports per item
    assert client.flushes == 2
    assert len(client.exported) == 2
    score = client.exported[0][0]
    assert score.run_id == "run_1"
    assert score.generation_id == "gen-it1"
    assert score.conversation_id == "conv-it1"
    assert score.value.number == 1.0
    assert score.source is not None and score.source.kind == "experiment"
    assert score.metadata["dataset_id"] == "support_smoke"
    assert score.metadata["item_id"] == "it1"
    assert score.metadata["candidate"] == {"git_sha": "abc123"}
    # deterministic, stable score id
    assert score.score_id.startswith("score-")
    # finalize succeeded with accepted count
    assert client.completed == [("run_1", "succeeded", 2, None)]
    assert result.accepted_scores == 2
    assert result.run_id == "run_1"
    assert "run_1" in result.url


def test_bulk_mode_defers_export_until_finish() -> None:
    client = _FakeClient()
    items = [DatasetItem(id="it1", input="x", expected="y")]

    def target(item: DatasetItem, run: ExperimentRun) -> TargetResult:
        return TargetResult(output="y", generation_ids=["gen-1"])

    runner = ExperimentRunner(
        client=client,
        run_id="run_bulk",
        name="bulk",
        upload="bulk",
        fetch_report=False,
        print_url=False,
    )
    runner.run(items, target, [_reward_scorer])

    # exactly one export at the end carrying all buffered scores
    assert len(client.exported) == 1
    assert len(client.exported[0]) == 1
    assert client.completed == [("run_bulk", "succeeded", 1, None)]


def test_exception_finalizes_failed_and_reraises() -> None:
    client = _FakeClient()

    with pytest.raises(RuntimeError, match="boom"):
        with experiment(client=client, run_id="run_fail", name="fail", print_url=False):
            raise RuntimeError("boom")

    assert len(client.completed) == 1
    run_id, status, _score_count, error = client.completed[0]
    assert (run_id, status) == ("run_fail", "failed")
    assert error == "boom"


def test_keyboard_interrupt_finalizes_failed_and_reraises() -> None:
    client = _FakeClient()

    with pytest.raises(KeyboardInterrupt):
        with experiment(client=client, run_id="run_cancel", name="cancel", print_url=False):
            raise KeyboardInterrupt

    assert client.completed == [("run_cancel", "failed", 0, "interrupted")]


def test_manual_mode_leaves_run_open_until_publish() -> None:
    client = _FakeClient()
    item = DatasetItem(id="it1", input="x", expected="y")

    with experiment(client=client, run_id="run_manual", name="manual", upload="manual", print_url=False) as run:
        run.add_scores(_reward_scorer(item, TargetResult(output="y", generation_ids=["gen-1"])), item=item)

    # nothing exported or finalized automatically
    assert client.exported == []
    assert client.completed == []
    # caller drives publish + finalize
    published = run.publish()
    run.finalize(ExperimentStatus.SUCCEEDED)
    assert published == 1
    assert len(client.exported) == 1
    assert client.completed == [("run_manual", "succeeded", 1, None)]


def test_add_scores_defaults_to_captured_generation_ids() -> None:
    client = _FakeClient()
    item = DatasetItem(id="it1", input="x", expected="y")
    run = ExperimentRun(
        client=client,
        run_id="run_capture",
        name="capture",
        dataset=None,
        candidate=None,
        upload="continuous",
        handler_kwargs={},
        async_handler=False,
    )
    run._active_sink = ["gen-captured"]

    run.add_scores(_reward_scorer(item, TargetResult(output="y")), item=item)

    assert client.exported[0][0].generation_id == "gen-captured"


def test_rejected_scores_raise_instead_of_silent_success() -> None:
    client = _FakeClient(reject_scores=True)
    item = DatasetItem(id="it1", input="x", expected="y")

    with pytest.raises(ScoreExportError, match="rejected 1 score"):
        with experiment(client=client, run_id="run_reject", name="reject", print_url=False) as run:
            run.add_scores(
                _reward_scorer(item, TargetResult(output="y", generation_ids=["gen-1"])),
                item=item,
                generation_ids=["gen-1"],
            )

    assert len(client.completed) == 1
    assert client.completed[0][0:3] == ("run_reject", "failed", 0)
    assert client.completed[0][3] is not None and "sigil score export rejected 1 score" in client.completed[0][3]


def test_finalize_can_be_retried_after_transport_failure() -> None:
    client = _FakeClient(complete_failures=1)
    run = ExperimentRun(
        client=client,
        run_id="run_retry",
        name="retry",
        dataset=None,
        candidate=None,
        upload="manual",
        handler_kwargs={},
        async_handler=False,
    )

    with pytest.raises(RuntimeError, match="complete failed"):
        run.finalize(ExperimentStatus.SUCCEEDED)

    run.finalize(ExperimentStatus.SUCCEEDED)
    assert client.completed == [("run_retry", "succeeded", 0, None)]


def test_multiple_generations_require_explicit_generation_id() -> None:
    client = _FakeClient()
    item = DatasetItem(id="it1", input="x", expected="y")

    with pytest.raises(ValueError, match="must set ScoreOutput.generation_id"):
        with experiment(client=client, run_id="run_multi", name="multi", print_url=False) as run:
            run.add_scores(
                _reward_scorer(item, TargetResult(output="y")),
                item=item,
                generation_ids=["gen-1", "gen-2"],
            )

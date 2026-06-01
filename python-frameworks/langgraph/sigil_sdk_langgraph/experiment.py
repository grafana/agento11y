"""LangGraph experiment runner for Sigil offline evaluation.

This module rides on top of the existing LangGraph callback handler so that
generations emitted while an agent runs are automatically tagged with an
experiment ``run_id``. It provides:

- :func:`experiment` — a context manager that creates the run, wires the
  ``run_id`` into the LangGraph callbacks, and finalizes the run on exit
  (``succeeded`` normally, ``failed`` on error, ``canceled`` on Ctrl-C).
- :class:`ExperimentRunner` — a thin loop over a dataset that invokes a user
  target and one or more user scorers, exporting scores per item.

The flow is generation-first and publishes continuously by default: create the
run, then for each item run the agent (exporting generations), grade locally,
and export scores attributed to the same ``run_id``. Grading is entirely
user-supplied: a scorer is any callable returning :class:`ScoreOutput` records.
"""

from __future__ import annotations

import hashlib
import secrets
from collections.abc import Callable, Iterator, Sequence
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Literal

from sigil_sdk import (
    Client,
    CreateExperimentRequest,
    ExperimentReport,
    ExperimentStatus,
    ScoreExportError,
    ScoreItem,
    ScoreSource,
    ScoreValue,
)

from .handler import SigilAsyncLangGraphHandler, SigilLangGraphHandler

# Score metadata keys read by the Sigil experiment report / plugin UI.
RESERVED_METADATA_KEYS = (
    "dataset_id",
    "dataset_version",
    "item_id",
    "task_id",
    "task_category",
    "trial_id",
)

UploadMode = Literal["continuous", "bulk", "manual"]


@dataclass(slots=True)
class DatasetItem:
    """A user-owned input plus stable identity for one experiment example."""

    id: str
    input: Any = None
    expected: Any = None
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class TargetResult:
    """Output of running the agent under test for one dataset item.

    ``generation_ids`` should list the Sigil generation ids produced while the
    agent ran (so scores can attach to them). When you instrument the graph with
    :meth:`ExperimentRun.langgraph_config`, the run captures these ids for you and
    the runner fills them in automatically; callers may also set them explicitly.
    """

    output: Any = None
    generation_ids: list[str] = field(default_factory=list)
    conversation_id: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class ScoreOutput:
    """A single grading result produced by a user scorer."""

    evaluator_id: str
    evaluator_version: str
    score_key: str
    value: ScoreValue
    generation_id: str = ""
    passed: bool | None = None
    explanation: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class ExperimentResult:
    """Summary returned by :meth:`ExperimentRunner.run`."""

    run_id: str
    accepted_scores: int
    url: str
    report: ExperimentReport | None = None


class _GenerationCaptureMixin:
    """Records generation ids the handler produces into a caller-owned sink.

    The base framework handler already tracks per-run generation ids; this mixin
    forwards each newly created id to a list the :class:`ExperimentRun` owns so
    the runner can attach scores without the caller plumbing ids by hand.
    """

    def __init__(self, *args: Any, _sink: list[str] | None = None, **kwargs: Any) -> None:
        self._sigil_experiment_sink: list[str] = _sink if _sink is not None else []
        super().__init__(*args, **kwargs)

    def _track_run_generation_id(self, run_key: str, generation_id: str) -> None:
        super()._track_run_generation_id(run_key, generation_id)  # type: ignore[misc]
        if generation_id and generation_id not in self._sigil_experiment_sink:
            self._sigil_experiment_sink.append(generation_id)


class _CapturingSyncHandler(_GenerationCaptureMixin, SigilLangGraphHandler):
    """Sync LangGraph handler that captures produced generation ids."""


class _CapturingAsyncHandler(_GenerationCaptureMixin, SigilAsyncLangGraphHandler):
    """Async LangGraph handler that captures produced generation ids."""


def stable_id(prefix: str, *parts: Any) -> str:
    """Returns a deterministic id from ``parts`` for idempotent retries."""

    joined = "\x1f".join("" if p is None else str(p) for p in parts)
    digest = hashlib.sha1(joined.encode("utf-8")).hexdigest()[:16]
    return f"{prefix}-{digest}"


# Target and scorer callable signatures (documented as type aliases).
DatasetTarget = Callable[[DatasetItem, "ExperimentRun"], TargetResult]
DatasetScorer = Callable[[DatasetItem, TargetResult], "Sequence[ScoreOutput] | None"]


class ExperimentRun:
    """An open experiment run bound to a Sigil client and a ``run_id``.

    Obtain one via :func:`experiment`. Use :meth:`langgraph_config` to wire the
    experiment into a LangGraph invocation, and :meth:`add_scores` to grade and
    publish scores for a completed item.
    """

    def __init__(
        self,
        *,
        client: Client,
        run_id: str,
        name: str,
        dataset: dict[str, Any] | None,
        candidate: dict[str, Any] | None,
        upload: UploadMode,
        handler_kwargs: dict[str, Any],
        async_handler: bool,
    ) -> None:
        self._client = client
        self.run_id = run_id
        self.name = name
        self._dataset = dataset or {}
        self._candidate = candidate or {}
        self._upload = upload
        self._handler_kwargs = handler_kwargs
        self._async_handler = async_handler
        self._buffer: list[ScoreItem] = []
        self._accepted = 0
        self._finalized = False
        self._active_sink: list[str] = []
        self._active_conversation_id: str = ""

    # --- LangGraph wiring -------------------------------------------------- #

    def make_handler(self, **overrides: Any) -> SigilLangGraphHandler | SigilAsyncLangGraphHandler:
        """Builds a LangGraph callback handler pre-tagged with this run_id.

        Each call starts a fresh generation-id capture sink, exposed via
        :attr:`produced_generation_ids`.
        """

        kwargs = {**self._handler_kwargs, **overrides}
        extra_tags = {**dict(kwargs.pop("extra_tags", {}) or {}), "experiment.run_id": self.run_id}
        extra_metadata = {**dict(kwargs.pop("extra_metadata", {}) or {}), "experiment_run_id": self.run_id}
        self._active_sink = []
        cls = _CapturingAsyncHandler if self._async_handler else _CapturingSyncHandler
        return cls(
            client=self._client,
            extra_tags=extra_tags,
            extra_metadata=extra_metadata,
            _sink=self._active_sink,
            **kwargs,
        )

    @property
    def produced_generation_ids(self) -> list[str]:
        """Generation ids captured since the most recent :meth:`langgraph_config` call."""

        return list(self._active_sink)

    @property
    def active_conversation_id(self) -> str:
        """The conversation id wired into the most recent :meth:`langgraph_config`.

        Used to keep the agent's generations and the exported scores on the same
        conversation so they link in Sigil.
        """

        return self._active_conversation_id

    def langgraph_config(
        self,
        config: dict[str, Any] | None = None,
        *,
        conversation_id: str | None = None,
        **overrides: Any,
    ) -> dict[str, Any]:
        """Returns a LangGraph invocation config with this run's callbacks attached.

        Pass the result as ``graph.invoke(state, config=run.langgraph_config())`` so
        every generation the graph emits carries the experiment ``run_id`` and a
        shared ``conversation_id``. The conversation id is injected into the config
        ``metadata`` (where the Sigil handler reads it) and reused for the scores
        you publish for this item, so generations and scores link in the UI.

        Resolution order: explicit ``conversation_id`` > one already present in
        ``config['metadata']`` > the run's active id (set by the runner per item) >
        a freshly generated id.
        """

        merged = dict(config or {})
        metadata = dict(merged.get("metadata") or {})
        existing_conv = str(metadata.get("conversation_id") or "").strip()
        conv_id = (conversation_id or existing_conv or self._active_conversation_id or "").strip()
        if conv_id == "":
            conv_id = stable_id("conv", self.run_id, secrets.token_hex(8))
        self._active_conversation_id = conv_id
        metadata["conversation_id"] = conv_id
        merged["metadata"] = metadata

        existing = merged.get("callbacks")
        if isinstance(existing, list):
            callbacks = list(existing)
        elif existing is None:
            callbacks = []
        else:
            callbacks = [existing]
        # Remove any existing Sigil handlers and replace with experiment-specific one
        callbacks = [
            item
            for item in callbacks
            if not isinstance(item, (SigilLangGraphHandler, SigilAsyncLangGraphHandler))
        ]
        callbacks.append(self.make_handler(**overrides))
        merged["callbacks"] = callbacks
        return merged

    # --- Scoring ----------------------------------------------------------- #

    def add_scores(
        self,
        scores: Sequence[ScoreOutput] | None,
        *,
        item: DatasetItem | None = None,
        generation_ids: Sequence[str] | None = None,
        conversation_id: str = "",
        trial_id: str | None = None,
    ) -> int:
        """Normalizes and publishes scores for one completed item.

        Flushes queued generations first so strict score ingest can find them.
        In ``continuous`` mode scores are exported immediately and the accepted
        count is returned; in ``bulk``/``manual`` mode they are buffered and the
        buffered count is returned (publish later with :meth:`publish`).
        """

        if not scores:
            return 0
        gen_ids = list(generation_ids if generation_ids is not None else self.produced_generation_ids)
        # Default to the conversation id wired into langgraph_config so scores
        # link to the same conversation as the generations they grade.
        conv_id = (conversation_id or self._active_conversation_id or "").strip()
        items = [self._build_score_item(s, item, gen_ids, conv_id, trial_id) for s in scores]

        if self._upload == "continuous":
            self._client.flush()
            response = self._client.export_scores(items)
            accepted = _accepted_or_raise(response)
            self._accepted += accepted
            return accepted
        self._buffer.extend(items)
        return len(items)

    def publish(self) -> int:
        """Flushes and exports any buffered scores (bulk/manual modes)."""

        if not self._buffer:
            return 0
        self._client.flush()
        response = self._client.export_scores(self._buffer)
        accepted = _accepted_or_raise(response)
        self._accepted += accepted
        self._buffer.clear()
        return accepted

    @property
    def accepted_scores(self) -> int:
        """Number of scores the server has accepted so far for this run."""

        return self._accepted

    @property
    def url(self) -> str:
        """Best-effort deep link to this experiment in the Sigil UI."""

        return self._client.experiment_url(self.run_id)

    def report(self) -> ExperimentReport:
        """Fetches the aggregated report for this run."""

        return self._client.get_experiment_report(self.run_id)

    def finalize(
        self,
        status: ExperimentStatus | str = ExperimentStatus.SUCCEEDED,
        *,
        error: str | None = None,
    ) -> None:
        """Marks the run terminal. Safe to call once; later calls are no-ops."""

        if self._finalized:
            return
        self._client.complete_experiment(self.run_id, status, score_count=self._accepted, error=error)
        self._finalized = True

    # --- internals --------------------------------------------------------- #

    def _build_score_item(
        self,
        score: ScoreOutput,
        item: DatasetItem | None,
        generation_ids: list[str],
        conversation_id: str,
        trial_id: str | None,
    ) -> ScoreItem:
        generation_id = score.generation_id
        if generation_id == "":
            if len(generation_ids) == 1:
                generation_id = generation_ids[0]
            elif len(generation_ids) > 1:
                raise ValueError(
                    "sigil experiment: target produced multiple generations; "
                    f"scorer '{score.evaluator_id}' must set ScoreOutput.generation_id explicitly"
                )
        metadata = self._score_metadata(score, item, trial_id)
        score_id = stable_id(
            "score",
            self.run_id,
            item.id if item else "",
            generation_id,
            score.evaluator_id,
            score.evaluator_version,
            score.score_key,
            trial_id or "",
        )
        return ScoreItem(
            score_id=score_id,
            generation_id=generation_id,
            conversation_id=conversation_id,
            run_id=self.run_id,
            evaluator_id=score.evaluator_id,
            evaluator_version=score.evaluator_version,
            score_key=score.score_key,
            value=score.value,
            passed=score.passed,
            explanation=score.explanation,
            metadata=metadata,
            source=ScoreSource(kind="experiment", id=self.run_id),
        )

    def _score_metadata(
        self,
        score: ScoreOutput,
        item: DatasetItem | None,
        trial_id: str | None,
    ) -> dict[str, Any]:
        metadata: dict[str, Any] = {}
        if self._dataset.get("id"):
            metadata["dataset_id"] = self._dataset["id"]
        if self._dataset.get("version"):
            metadata["dataset_version"] = self._dataset["version"]
        if self._candidate:
            metadata["candidate"] = dict(self._candidate)
        if item is not None:
            metadata["item_id"] = item.id
            metadata.update(item.metadata)
        if trial_id is not None:
            metadata["trial_id"] = trial_id
        metadata.update(score.metadata)
        return metadata


@contextmanager
def experiment(
    *,
    client: Client,
    run_id: str,
    name: str,
    description: str = "",
    tags: list[str] | None = None,
    metadata: dict[str, Any] | None = None,
    dataset: dict[str, Any] | None = None,
    candidate: dict[str, Any] | None = None,
    upload: UploadMode = "continuous",
    print_url: bool = True,
    async_handler: bool = False,
    **handler_kwargs: Any,
) -> Iterator[ExperimentRun]:
    """Opens an external experiment run and finalizes it on exit.

    On normal exit the run is finalized ``succeeded`` (buffered scores are
    published first in ``bulk`` mode). On an exception the run is finalized
    ``failed``; on ``KeyboardInterrupt`` it is ``canceled``. In ``manual`` mode
    the run is left open on success so the caller can inspect, then call
    :meth:`ExperimentRun.publish` and :meth:`ExperimentRun.finalize` themselves.

    Extra keyword args are forwarded to the LangGraph handler (e.g.
    ``agent_name``, ``agent_version``, ``provider_resolver``).
    """

    create_metadata = _run_metadata(metadata, dataset, candidate)
    client.create_experiment(
        CreateExperimentRequest(
            run_id=run_id,
            name=name,
            source="external",
            description=description,
            tags=list(tags or []),
            metadata=create_metadata,
        )
    )
    run = ExperimentRun(
        client=client,
        run_id=run_id,
        name=name,
        dataset=dataset,
        candidate=candidate,
        upload=upload,
        handler_kwargs=handler_kwargs,
        async_handler=async_handler,
    )
    try:
        yield run
    except KeyboardInterrupt:
        _safe(lambda: client.cancel_experiment(run_id))
        raise
    except BaseException as exc:  # noqa: BLE001 - finalize then re-raise
        error_text = str(exc)
        _safe(lambda: run.finalize(ExperimentStatus.FAILED, error=error_text))
        raise
    else:
        if upload == "manual":
            if print_url:
                print(
                    f"[sigil] experiment '{run_id}' left open (manual mode): "
                    f"{len(run._buffer)} score(s) buffered. "
                    "Call run.publish() then run.finalize() to upload."
                )
            return
        run.publish()
        run.finalize(ExperimentStatus.SUCCEEDED)
        if print_url:
            print(f"[sigil] experiment '{run_id}' finished ({run.accepted_scores} scores): {run.url}")


class ExperimentRunner:
    """Runs an agent over a dataset and publishes scores under one run.

    A/B testing is two runners with different ``run_id``/``tags`` over the same
    items and scorers. Concurrency is fixed to 1 in this first iteration.
    """

    def __init__(
        self,
        *,
        client: Client,
        run_id: str,
        name: str,
        description: str = "",
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        dataset: dict[str, Any] | None = None,
        candidate: dict[str, Any] | None = None,
        upload: UploadMode = "continuous",
        print_url: bool = True,
        fetch_report: bool = True,
        **handler_kwargs: Any,
    ) -> None:
        self._client = client
        self._run_id = run_id
        self._name = name
        self._description = description
        self._tags = list(tags or [])
        self._metadata = dict(metadata or {})
        self._dataset = dict(dataset or {})
        self._candidate = dict(candidate or {})
        self._upload = upload
        self._print_url = print_url
        self._fetch_report = fetch_report
        self._handler_kwargs = handler_kwargs

    def run(
        self,
        items: Sequence[DatasetItem],
        target: DatasetTarget,
        scorers: Sequence[DatasetScorer],
    ) -> ExperimentResult:
        """Executes ``target`` for each item, grades with ``scorers``, publishes scores."""

        with experiment(
            client=self._client,
            run_id=self._run_id,
            name=self._name,
            description=self._description,
            tags=self._tags,
            metadata=self._metadata,
            dataset=self._dataset,
            candidate=self._candidate,
            upload=self._upload,
            print_url=self._print_url,
            **self._handler_kwargs,
        ) as run:
            for item in items:
                # Assign one stable conversation id per item before running the
                # target. langgraph_config() picks it up and tags the agent's
                # generations with it; the scores below reuse it, so generations
                # and scores share a conversation and link in the UI.
                run._active_conversation_id = stable_id("conv", run.run_id, item.id)
                result = target(item, run)
                if result is None:
                    result = TargetResult()
                # Fall back to ids captured by the run's LangGraph handler when the
                # target did not report them explicitly.
                generation_ids = result.generation_ids or run.produced_generation_ids
                outputs: list[ScoreOutput] = []
                for scorer in scorers:
                    produced = scorer(item, result)
                    if produced:
                        outputs.extend(produced)
                run.add_scores(
                    outputs,
                    item=item,
                    generation_ids=generation_ids,
                    conversation_id=result.conversation_id or run.active_conversation_id,
                )

        report = None
        if self._fetch_report:
            report = _safe(run.report)
        return ExperimentResult(
            run_id=run.run_id,
            accepted_scores=run.accepted_scores,
            url=run.url,
            report=report,
        )


def _run_metadata(
    metadata: dict[str, Any] | None,
    dataset: dict[str, Any] | None,
    candidate: dict[str, Any] | None,
) -> dict[str, Any]:
    out: dict[str, Any] = dict(metadata or {})
    if dataset:
        if dataset.get("id"):
            out.setdefault("dataset_id", dataset["id"])
        if dataset.get("version"):
            out.setdefault("dataset_version", dataset["version"])
        if dataset.get("uri"):
            out.setdefault("dataset_uri", dataset["uri"])
    if candidate:
        out.setdefault("candidate", dict(candidate))
    out.setdefault("created_at", datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"))
    return out


def _safe(fn: Callable[[], Any]) -> Any:
    """Runs ``fn`` and swallows exceptions (used on finalize/cancel paths)."""

    try:
        return fn()
    except Exception:  # noqa: BLE001 - best-effort finalize/cancel/report
        return None


def _accepted_or_raise(response: Any) -> int:
    rejected = getattr(response, "rejected", [])
    if rejected:
        details = "; ".join(f"{r.score_id}: {r.error or 'rejected'}" for r in rejected)
        raise ScoreExportError(f"sigil score export rejected {len(rejected)} score(s): {details}")
    return int(getattr(response, "accepted_count", 0))

"""The cloud experiments ergonomic surface: ``Experiment`` and ``Trial``.

This is the high-level API benchmark harnesses instrument against. It writes over
the v1 one-token ingest path (run upsert, score export, finalize). Experimental
OpenTelemetry eval telemetry is available behind ``use_experimental_otel``.

Typical in-process use::

    with Experiment(client, experiment_id="run-1", name="nightly", suite=suite) as exp:
        for case in suite.test_cases:
            with exp.trial(case) as trial:
                answer = run_agent(case.input)
                trial.final_score(grade(answer), passed=grade(answer) >= 0.7,
                                  explanation="...", evaluator=verifier)
                trial.check_score("json_valid", passed=is_json(answer))

Out-of-process use (e.g. a verifier container) opens a trial from a
:class:`~sigil_sdk.experiments.types.TrialRef` without the parent ``Experiment``::

    ref = TrialRef.from_env()
    if ref is None:
        raise RuntimeError("missing Sigil trial environment")
    trial = Trial.from_ref(client, ref)
    trial.final_score(0.82, passed=True)
    trial.flush()
"""

from __future__ import annotations

import hashlib
import json
import mimetypes
import os
import secrets
import time
from types import TracebackType
from typing import TYPE_CHECKING, Any

try:  # OpenTelemetry is optional and gated by use_experimental_otel.
    from opentelemetry import context as otel_context
    from opentelemetry import trace as otel_trace
    from opentelemetry.trace import SpanKind, Status, StatusCode, set_span_in_context

    _OTEL_AVAILABLE = True
except Exception:  # pragma: no cover - exercised only in minimal vendored envs
    otel_context = None  # type: ignore[assignment]
    otel_trace = None  # type: ignore[assignment]
    SpanKind = Status = StatusCode = set_span_in_context = None  # type: ignore[assignment]
    _OTEL_AVAILABLE = False

from ..models import (
    CreateExperimentRequest,
    ExperimentReport,
    ScoreItem,
    ScoreSource,
    ScoreValue,
)
from . import otel
from .types import (
    Candidate,
    Evaluator,
    EvaluatorKind,
    ExperimentStatus,
    TestCase,
    TestSuite,
    TrialRef,
    TrialStatus,
)

if TYPE_CHECKING:  # avoid an import cycle at runtime
    from .client import Client


def stable_id(prefix: str, *parts: Any) -> str:
    """Deterministic id from ``parts`` so retries are idempotent."""

    joined = "\x1f".join("" if p is None else str(p) for p in parts)
    digest = hashlib.sha1(joined.encode("utf-8")).hexdigest()[:16]
    return f"{prefix}-{digest}"


def _coerce_value(value: ScoreValue | float | bool | str) -> ScoreValue:
    if isinstance(value, ScoreValue):
        return value
    if isinstance(value, bool):
        return ScoreValue(boolean=value)
    if isinstance(value, (int, float)):
        return ScoreValue(number=float(value))
    return ScoreValue(string=str(value))


def _event_value(value: ScoreValue) -> float | bool | str | None:
    if value.number is not None:
        return value.number
    if value.boolean is not None:
        return value.boolean
    if value.string is not None:
        return value.string
    return None


def _kind_from_mime(mime: str) -> str:
    """Maps a MIME type to a Sigil artifact kind."""

    m = (mime or "").lower()
    if m.startswith("image/"):
        return "image"
    if m == "application/json":
        return "json"
    if m in {"text/markdown", "text/x-markdown"}:
        return "markdown"
    if m == "application/pdf":
        return "pdf"
    if m == "text/csv":
        return "csv"
    if m.startswith("text/"):
        return "text"
    return "binary"


def _artifact_content(path: str, data: Any, text: str, kind: str, mime: str) -> tuple[bytes, str, str]:
    """Resolves an artifact's (bytes, kind, mime) from a file, object, or text."""

    if path:
        with open(path, "rb") as handle:
            content = handle.read()
        resolved_mime = mime or (mimetypes.guess_type(path)[0] or "application/octet-stream")
        return content, kind or _kind_from_mime(resolved_mime), resolved_mime
    if data is not None:
        return json.dumps(data, default=str).encode("utf-8"), kind or "json", mime or "application/json"
    if text:
        return text.encode("utf-8"), kind or "text", mime or "text/plain"
    raise ValueError("artifact requires one of path=, data=, or text=")


class Trial:
    """One attempt at one test case: records scores and emits eval telemetry.

    Use as a context manager. Scores are buffered and exported to Sigil on exit
    (or on an explicit :meth:`flush`). Each score also emits a
    ``gen_ai.evaluation.result`` OTel event on the trial span.
    """

    def __init__(
        self,
        client: Client,
        ref: TrialRef,
        *,
        experiment: Experiment | None = None,
        candidate: Candidate | None = None,
        default_evaluator: Evaluator | None = None,
        metadata: dict[str, Any] | None = None,
        use_experimental_otel: bool | None = None,
    ) -> None:
        self._client = client
        self.ref = ref
        self._experiment = experiment
        self._candidate = candidate
        self._default_evaluator = default_evaluator or Evaluator(evaluator_id="sdk", version="0")
        self._metadata = dict(metadata or {})
        self._use_experimental_otel = (
            bool(use_experimental_otel)
            if use_experimental_otel is not None
            else bool(getattr(client, "use_experimental_otel", False))
        )

        self.trial_id = stable_id("trial", ref.experiment_id, ref.test_case_id, ref.attempt)
        self.status: str = TrialStatus.RUNNING.value
        self.conversation_id: str = ""
        self.trace_id: str = ""
        self.span_id: str = ""
        self.error: str = ""

        # Scores attach to the typed trial; a generation is optional, exported only
        # when the harness binds one or supplies I/O via record_io.
        self.generation_id: str = stable_id("gen", ref.experiment_id, ref.test_case_id, ref.attempt)
        self._generation_bound = False
        self._generation_exported = False
        self._has_generation = False
        self._io: dict[str, Any] = {}
        self._trial_created = False
        self._usage: dict[str, Any] = {}
        self._started_monotonic: float | None = None

        self._span: Any = None
        self._otel_token: Any = None
        self._buffer: list[ScoreItem] = []
        self._accepted = 0
        self._has_final = False
        self._final_passed: bool | None = None
        self.artifacts: list[dict[str, Any]] = []

    # --- lifecycle -------------------------------------------------------- #

    @classmethod
    def from_ref(
        cls,
        client: Client,
        ref: TrialRef | None,
        *,
        candidate: Candidate | None = None,
        default_evaluator: Evaluator | None = None,
        use_experimental_otel: bool | None = None,
    ) -> Trial:
        """Opens a standalone trial bound to ``client`` (no parent Experiment)."""

        if ref is None:
            raise ValueError("trial ref is required; set SIGIL_EXPERIMENT_ID and SIGIL_TEST_CASE_ID")
        return cls(
            client,
            ref,
            candidate=candidate,
            default_evaluator=default_evaluator,
            use_experimental_otel=use_experimental_otel,
        )

    def __enter__(self) -> Trial:
        self._started_monotonic = time.perf_counter()
        self._start_span()
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> bool:
        if exc is not None and self.status == TrialStatus.RUNNING.value:
            self.status = TrialStatus.ERRORED.value
            self.error = str(exc) or exc_type.__name__ if exc_type else str(exc)
        elif self.status == TrialStatus.RUNNING.value:
            # No explicit verdict: derive from the final score if present.
            if self._has_final:
                self.status = TrialStatus.PASSED.value if self._final_passed else TrialStatus.FAILED.value
            else:
                self.status = TrialStatus.FAILED.value
                self.error = "trial exited without a final score"
        try:
            self.flush()
        finally:
            self._finalize_trial()
            self._end_span()
        return False  # never suppress

    def _finalize_trial(self) -> None:
        """Patches the typed trial with its terminal status and usage rollups."""

        if not self._trial_created:
            return
        # Trial status is the lifecycle (completed/failed); the pass/fail verdict
        # lives in the final score's `passed`, which drives the report pass-rate.
        backend_status = "failed" if self.status == TrialStatus.ERRORED.value else "completed"
        self._client.update_trial(
            self.ref.experiment_id,
            self.trial_id,
            status=backend_status,
            error=self.error,
            cost=self._usage.get("cost"),
            input_tokens=self._usage.get("input_tokens"),
            output_tokens=self._usage.get("output_tokens"),
            duration_ms=self._duration_ms(),
            conversation_id=self.conversation_id,
            trace_id=self.trace_id,
        )

    def _duration_ms(self) -> int | None:
        if self._started_monotonic is None:
            return None
        return max(0, int((time.perf_counter() - self._started_monotonic) * 1000))

    def _start_span(self) -> None:
        # OpenTelemetry eval telemetry is experimental. Without the opt-in we
        # still create the trial and record scores; only spans/events are skipped.
        if self._use_experimental_otel and _OTEL_AVAILABLE:
            tracer = otel_trace.get_tracer(otel.INSTRUMENTATION_NAME)
            span = tracer.start_span(
                f"{otel.TRIAL_SPAN_NAME} {self.ref.test_case_id}",
                kind=SpanKind.INTERNAL,
                attributes=self._identity_attrs(),
            )
            self._span = span
            # Make the trial span active so generations the agent emits inside the
            # block become children (same trace) and auto-correlate with the trial.
            self._otel_token = otel_context.attach(set_span_in_context(span))
            ctx = span.get_span_context()
            if ctx is not None and ctx.trace_id:
                self.trace_id = format(ctx.trace_id, "032x")
                self.span_id = format(ctx.span_id, "016x")
        # conversation_id stays empty until a real conversation is bound.
        self._create_trial()

    def _create_trial(self) -> None:
        """Creates the typed trial so scores resolve and the report rolls up."""

        if self._trial_created:
            return
        self._client.upsert_trial(
            self.ref.experiment_id,
            trial_id=self.trial_id,
            test_case_id=self.ref.test_case_id,
            attempt=self.ref.attempt,
            status=TrialStatus.RUNNING.value,
            conversation_id=self.conversation_id,
            trace_id=self.trace_id,
            span_id=self.span_id,
            metadata={"test_case_name": self.ref.test_case_name} if self.ref.test_case_name else None,
        )
        self._trial_created = True

    def _end_span(self) -> None:
        if self._span is None:
            return
        if self._otel_token is not None:
            otel_context.detach(self._otel_token)
            self._otel_token = None
        self._span.set_attributes(self._identity_attrs())
        if self.error:
            self._span.set_attribute(otel.EVAL_EXPLANATION, self.error)
            self._span.set_status(Status(StatusCode.ERROR, self.error))
        else:
            self._span.set_status(Status(StatusCode.OK))
        self._span.end()
        self._span = None

    def _identity_attrs(self) -> dict[str, Any]:
        run_status = self._experiment.status if self._experiment is not None else "running"
        attrs = otel.trial_identity_attributes(
            experiment_id=self.ref.experiment_id,
            experiment_name=(self._experiment.name if self._experiment else ""),
            suite_id=self.ref.suite_id,
            suite_version=self.ref.suite_version,
            suite_name=self.ref.suite_name,
            test_case_id=self.ref.test_case_id,
            test_case_name=self.ref.test_case_name,
            attempt=self.ref.attempt,
            trial_id=self.trial_id,
            run_status=run_status,
            trial_status=self.status,
            conversation_id=self.conversation_id,
            operation_name="invoke_agent",
        )
        if self._candidate is not None:
            for key, value in {
                "gen_ai.agent.name": self._candidate.agent_name,
                "gen_ai.agent.version": self._candidate.agent_version,
                "gen_ai.provider.name": self._candidate.model_provider,
                "gen_ai.request.model": self._candidate.model_name,
            }.items():
                if value:
                    attrs[key] = value
        return attrs

    # --- binding (out-of-band execution) ---------------------------------- #

    def bind_trace(self, trace_id: str, span_id: str = "") -> Trial:
        """Links this trial's scores to a trace produced elsewhere."""

        self.trace_id = (trace_id or "").strip()
        self.span_id = (span_id or "").strip()
        return self

    def bind_conversation(self, conversation_id: str) -> Trial:
        """Links this trial's scores to a specific conversation."""

        self.conversation_id = (conversation_id or "").strip()
        return self

    def bind_generation(self, generation_id: str, *, conversation_id: str = "") -> Trial:
        """Attaches this trial's scores to an existing generation.

        Use this when the harness already exported the attempt's LLM
        generation(s) (e.g. through a provider wrapper or its own OTel pipeline).
        The trial will not record an anchor generation of its own.
        """

        gid = (generation_id or "").strip()
        if gid:
            self.generation_id = gid
            self._generation_bound = True
            self._generation_exported = True
            self._has_generation = True
        if conversation_id:
            self.conversation_id = conversation_id.strip()
        return self

    def record_io(
        self,
        *,
        input: Any = None,
        output: Any = None,
        model_provider: str = "",
        model_name: str = "",
        agent_name: str = "",
        input_tokens: int | None = None,
        output_tokens: int | None = None,
    ) -> Trial:
        """Records the attempt's input/output for the anchor generation.

        Stored now and exported as one generation when scores flush, so the
        attempt's conversation is visible in Sigil and the scores attach to it.
        """

        self._has_generation = True
        # A real generation will back this trial; mint a conversation id if unbound.
        if not self.conversation_id:
            self.conversation_id = stable_id("conv", self.ref.experiment_id, self.ref.test_case_id, self.ref.attempt)
        if input is not None:
            self._io["input_text"] = input if isinstance(input, str) else str(input)
        if output is not None:
            self._io["output_text"] = output if isinstance(output, str) else str(output)
        if model_provider:
            self._io["model_provider"] = model_provider
        if model_name:
            self._io["model_name"] = model_name
        if agent_name:
            self._io["agent_name"] = agent_name
        if input_tokens is not None:
            self._io["input_tokens"] = int(input_tokens)
        if output_tokens is not None:
            self._io["output_tokens"] = int(output_tokens)
        return self

    def set_usage(
        self,
        *,
        input_tokens: int | None = None,
        output_tokens: int | None = None,
        cost: float | None = None,
    ) -> Trial:
        """Records token usage and an optional cost override.

        Tokens are emitted on the span as the standard ``gen_ai.usage.*`` signal.
        ``cost`` is an optional override (USD): when provided it is sent as the
        trial's reported cost; when omitted, Sigil derives the trial's cost at
        ingestion from token usage and model-card pricing. Cost has no OTel
        attribute, so it is never put on the span. Pass ``cost`` only when a
        framework computes it itself (e.g. provider-billed cost) — leave it unset
        rather than passing ``0`` when unknown.
        """

        if input_tokens is not None:
            self._usage["input_tokens"] = int(input_tokens)
        if output_tokens is not None:
            self._usage["output_tokens"] = int(output_tokens)
        if cost is not None:
            self._usage["cost"] = float(cost)
        if self._span is None:
            return self
        if input_tokens is not None:
            self._span.set_attribute("gen_ai.usage.input_tokens", int(input_tokens))
        if output_tokens is not None:
            self._span.set_attribute("gen_ai.usage.output_tokens", int(output_tokens))
        return self

    def running(self, metadata: dict[str, Any] | None = None) -> Trial:
        """Marks the trial as in-progress (compat shim; status is RUNNING)."""

        if metadata:
            self._metadata.update(metadata)
        self.status = TrialStatus.RUNNING.value
        return self

    # --- scoring ---------------------------------------------------------- #

    def score(
        self,
        score_key: str,
        value: ScoreValue | float | bool | str,
        *,
        evaluator: Evaluator | None = None,
        passed: bool | None = None,
        explanation: str = "",
        generation_id: str = "",
        grader_conversation_id: str = "",
        grader_generation_id: str = "",
        grader_trace_id: str = "",
        metadata: dict[str, Any] | None = None,
    ) -> ScoreItem:
        """Records a score for this trial. The general primitive.

        Prefer :meth:`final_score`, :meth:`check_score`, or :meth:`rubric_score`
        for the common headline / deterministic-check / rubric-criterion cases.
        """

        ev = evaluator or self._default_evaluator
        sv = _coerce_value(value)
        score_id = stable_id("score", self.ref.experiment_id, self.trial_id, score_key, ev.evaluator_id)
        # The score carries the typed trial_id so the backend attributes it and the
        # report rolls up per case; metadata mirrors the ids for grouping.
        score_metadata = {
            "task_id": self.ref.test_case_id,
            "trial_id": self.trial_id,
            "attempt": self.ref.attempt,
            **self._metadata,
            **(metadata or {}),
        }
        item = ScoreItem(
            score_id=score_id,
            evaluator_id=ev.evaluator_id,
            evaluator_version=ev.version,
            evaluator_kind=ev.normalized_kind(),
            score_key=score_key,
            value=sv,
            # generation_id only when one exists; trial_id is what attributes the score.
            generation_id=generation_id or (self.generation_id if self._has_generation else ""),
            trial_id=self.trial_id,
            conversation_id=self.conversation_id,
            trace_id=self.trace_id,
            span_id=self.span_id,
            experiment_id=self.ref.experiment_id,
            test_case_id=self.ref.test_case_id,
            grader_conversation_id=grader_conversation_id,
            grader_generation_id=grader_generation_id,
            grader_trace_id=grader_trace_id,
            passed=passed,
            explanation=explanation,
            metadata=score_metadata,
            source=ScoreSource(kind="experiment", id=self.ref.experiment_id),
        )
        self._buffer.append(item)
        self._emit_event(
            score_key,
            sv,
            ev,
            passed=passed,
            explanation=explanation,
            response_id=generation_id,
        )
        if score_key == "final":
            self._has_final = True
            self._final_passed = passed
        return item

    def final_score(
        self,
        value: ScoreValue | float | bool | str,
        *,
        passed: bool | None = None,
        explanation: str = "",
        evaluator: Evaluator | None = None,
        generation_id: str = "",
        metadata: dict[str, Any] | None = None,
    ) -> ScoreItem:
        """The headline score + trial verdict (``score_key="final"``).

        ``passed`` is the trial's pass/fail verdict used by the Sigil report
        rollup. When omitted and ``value`` is boolean, the boolean is the verdict.
        """

        if passed is None:
            coerced = _coerce_value(value)
            if coerced.boolean is not None:
                passed = coerced.boolean
        return self.score(
            "final",
            value,
            evaluator=evaluator,
            passed=passed,
            explanation=explanation,
            generation_id=generation_id,
            metadata=metadata,
        )

    def check_score(
        self,
        name: str,
        *,
        passed: bool,
        value: ScoreValue | float | bool | str | None = None,
        explanation: str = "",
        evaluator: Evaluator | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> ScoreItem:
        """A deterministic check (pass/fail), e.g. ``json_valid`` or ``tool_used``."""

        ev = evaluator or Evaluator(
            evaluator_id=f"{self._default_evaluator.evaluator_id}.{name}",
            version=self._default_evaluator.version,
            kind=EvaluatorKind.DETERMINISTIC.value,
        )
        return self.score(
            name,
            value if value is not None else passed,
            evaluator=ev,
            passed=passed,
            explanation=explanation,
            metadata=metadata,
        )

    def rubric_score(
        self,
        name: str,
        value: ScoreValue | float | bool | str,
        *,
        explanation: str = "",
        passed: bool | None = None,
        evaluator: Evaluator | None = None,
        grader_conversation_id: str = "",
        grader_generation_id: str = "",
        metadata: dict[str, Any] | None = None,
    ) -> ScoreItem:
        """An LLM-judge rubric criterion score."""

        ev = evaluator or Evaluator(
            evaluator_id=f"{self._default_evaluator.evaluator_id}.{name}",
            version=self._default_evaluator.version,
            kind=EvaluatorKind.LLM_JUDGE.value,
        )
        return self.score(
            name,
            value,
            evaluator=ev,
            passed=passed,
            explanation=explanation,
            grader_conversation_id=grader_conversation_id,
            grader_generation_id=grader_generation_id,
            metadata=metadata,
        )

    # --- artifacts -------------------------------------------------------- #

    def artifact(
        self,
        name: str,
        *,
        path: str = "",
        data: Any = None,
        text: str = "",
        kind: str = "",
        mime: str = "",
    ) -> dict[str, Any]:
        """Attaches an artifact to this trial: a file, a JSON object, or text.

        Provide exactly one of ``path`` (a file to read), ``data`` (any
        JSON-serializable object), or ``text``. ``kind`` and ``mime`` are inferred
        when omitted. Returns the created artifact record (with ``artifact_id``).
        """

        content, resolved_kind, resolved_mime = _artifact_content(path, data, text, kind, mime)
        record = self._client.upload_artifact(
            experiment_id=self.ref.experiment_id,
            parent_id=self.trial_id,
            name=name,
            kind=resolved_kind,
            content=content,
            mime=resolved_mime,
        )
        self.artifacts.append({"name": name, "kind": resolved_kind, "artifact_id": record.get("artifact_id", "")})
        if self._span is not None:
            self._span.add_event(
                "sigil.eval.artifact",
                attributes={"sigil.eval.artifact.name": name, "sigil.eval.artifact.kind": resolved_kind},
            )
        return record

    def succeed(self) -> Trial:
        """Marks the trial passed (compat shim)."""

        self.status = TrialStatus.PASSED.value
        return self

    def fail(self, error: str = "") -> Trial:
        """Marks the trial failed."""

        self.status = TrialStatus.FAILED.value
        if error:
            self.error = error
        return self

    def _emit_event(
        self,
        score_key: str,
        value: ScoreValue,
        evaluator: Evaluator,
        *,
        passed: bool | None,
        explanation: str,
        response_id: str,
    ) -> None:
        if self._span is None:
            return
        attrs = otel.score_event_attributes(
            name=score_key,
            value=_event_value(value),
            passed=passed,
            explanation=explanation,
            evaluator_id=evaluator.evaluator_id,
            evaluator_version=evaluator.version,
            evaluator_kind=evaluator.normalized_kind(),
            reference_set_id=evaluator.reference_set_id,
            reference_set_version=evaluator.reference_set_version,
            response_id=response_id,
        )
        self._span.add_event(otel.EVENT_EVAL_RESULT, attributes=attrs)

    # --- export ----------------------------------------------------------- #

    def _ensure_generation(self) -> None:
        """Exports the anchor generation when ``record_io`` supplied content.

        Generations are optional: the typed trial already attributes scores. We
        only export one when the harness gave us input/output to make the
        attempt's conversation visible in Sigil.
        """

        if self._generation_exported or self._generation_bound or not self._io:
            return
        case_input = ""
        if self._experiment is not None and self._experiment.suite is not None:
            tc = self._experiment.suite.case(self.ref.test_case_id)
            if tc is not None and tc.input is not None:
                case_input = tc.input if isinstance(tc.input, str) else str(tc.input)
        cand = self._candidate
        self._client.record_generation(
            self.generation_id,
            conversation_id=self.conversation_id,
            input_text=self._io.get("input_text", case_input),
            output_text=self._io.get("output_text", ""),
            model_provider=self._io.get("model_provider", (cand.model_provider if cand else "") or "eval"),
            model_name=self._io.get("model_name", (cand.model_name if cand else "") or "experiment"),
            agent_name=self._io.get("agent_name", (cand.agent_name if cand else "")),
            input_tokens=self._io.get("input_tokens"),
            output_tokens=self._io.get("output_tokens"),
            tags={"experiment.run_id": self.ref.experiment_id, "task_id": self.ref.test_case_id},
            metadata={
                "experiment_run_id": self.ref.experiment_id,
                "task_id": self.ref.test_case_id,
                "trial_id": self.trial_id,
                "attempt": self.ref.attempt,
            },
        )
        self._generation_exported = True

    def flush(self) -> int:
        """Exports buffered scores to Sigil. Returns the number freshly accepted.

        Anchors the trial's generation first (when not externally bound) so the
        scores' ``generation_id`` resolves under strict score ingest.
        """

        if not self._buffer:
            return 0
        self._ensure_generation()
        pending = list(self._buffer)
        accepted = self._client.export_scores(pending)
        del self._buffer[: len(pending)]
        self._accepted += accepted
        if self._experiment is not None:
            self._experiment._record_accepted(accepted)
        return accepted

    @property
    def accepted_scores(self) -> int:
        return self._accepted


class Experiment:
    """An external experiment run: upserts on enter, finalizes on exit.

    Open trials with :meth:`trial`. On normal exit the run is finalized
    ``completed``; on an exception (including ``KeyboardInterrupt``) it is
    finalized ``failed``.
    """

    def __init__(
        self,
        client: Client,
        *,
        experiment_id: str = "",
        name: str = "",
        suite: TestSuite | None = None,
        candidate: Candidate | dict[str, Any] | None = None,
        default_evaluator: Evaluator | None = None,
        description: str = "",
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        auto_finalize: bool = True,
        use_experimental_otel: bool | None = None,
    ) -> None:
        self._client = client
        self.experiment_id = experiment_id or stable_id("exp", name, secrets.token_hex(8))
        self.name = name or self.experiment_id
        self.suite = suite
        self._candidate = Candidate.from_obj(candidate)
        self._default_evaluator = default_evaluator
        self.description = description
        self._tags = list(tags or [])
        self._metadata = dict(metadata or {})
        self._auto_finalize = auto_finalize
        self._use_experimental_otel = (
            bool(use_experimental_otel)
            if use_experimental_otel is not None
            else bool(getattr(client, "use_experimental_otel", False))
        )
        self.status = "running"
        self._accepted = 0
        self._finalized = False
        self._owns_client = False  # set by the experiment() factory when it built the client

    # alias: many callers spell the identifier ``run_id``
    @property
    def run_id(self) -> str:
        return self.experiment_id

    @property
    def client(self) -> Client:
        """The underlying client (for agent-side generation ingestion)."""

        return self._client

    def __enter__(self) -> Experiment:
        metadata = dict(self._metadata)
        if self.suite is not None:
            metadata.setdefault("suite_id", self.suite.suite_id)
            metadata.setdefault("suite_version", self.suite.version)
        if self._candidate is not None:
            metadata.update(self._candidate.as_metadata())
        self._client.upsert_experiment(
            CreateExperimentRequest(
                run_id=self.experiment_id,
                name=self.name,
                source="external",
                description=self.description,
                tags=self._tags,
                metadata=metadata,
            )
        )
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> bool:
        try:
            if not self._auto_finalize:
                return False
            if exc is not None:
                self.finalize(ExperimentStatus.FAILED, error=str(exc) or (exc_type.__name__ if exc_type else "error"))
            else:
                self.finalize(ExperimentStatus.COMPLETED)
        finally:
            if self._owns_client:
                self._client.shutdown()
        return False

    def trial(
        self,
        case: TestCase | str,
        *,
        attempt: int = 1,
        trajectory_id: str = "",
        metadata: dict[str, Any] | None = None,
    ) -> Trial:
        """Opens a trial for ``case`` (a :class:`TestCase` or a test-case id)."""

        if isinstance(case, TestCase):
            test_case_id = case.test_case_id
            test_case_name = case.name or case.test_case_id
        else:
            test_case_id = str(case)
            tc = self.suite.case(test_case_id) if self.suite is not None else None
            test_case_name = tc.name if tc is not None else test_case_id
        ref = TrialRef(
            experiment_id=self.experiment_id,
            test_case_id=test_case_id,
            attempt=attempt,
            suite_id=(self.suite.suite_id if self.suite else ""),
            suite_version=(self.suite.version if self.suite else ""),
            suite_name=(self.suite.name if self.suite else self.name),
            test_case_name=test_case_name,
            trajectory_id=trajectory_id,
        )
        return Trial(
            self._client,
            ref,
            experiment=self,
            candidate=self._candidate,
            default_evaluator=self._default_evaluator,
            metadata=metadata,
            use_experimental_otel=self._use_experimental_otel,
        )

    def _record_accepted(self, n: int) -> None:
        self._accepted += n

    @property
    def accepted_scores(self) -> int:
        return self._accepted

    def finalize(self, status: ExperimentStatus | str = ExperimentStatus.COMPLETED, *, error: str = "") -> None:
        """Finalizes the run. Safe to call once; later calls are no-ops."""

        if self._finalized:
            return
        status_value = status.value if isinstance(status, ExperimentStatus) else str(status)
        self.status = status_value
        self._client.finalize(self.experiment_id, status_value, score_count=self._accepted, error=error)
        self._finalized = True

    def report(self) -> ExperimentReport:
        """Fetches the aggregated report for this run."""

        return self._client.get_report(self.experiment_id)

    @property
    def url(self) -> str:
        """Best-effort deep link to the run in the Sigil UI."""

        return self._client.experiment_url(self.experiment_id)


def experiment(
    name: str = "",
    *,
    suite: TestSuite | None = None,
    candidate: Candidate | dict[str, Any] | None = None,
    experiment_id: str = "",
    client: Client | None = None,
    endpoint: str = "",
    tenant_id: str = "",
    ingest_token: str = "",
    actor: str = "",
    grafana_url: str = "",
    use_experimental_otel: bool | None = None,
    default_evaluator: Evaluator | None = None,
    description: str = "",
    tags: list[str] | None = None,
    metadata: dict[str, Any] | None = None,
) -> Experiment:
    """Opens a cloud experiment, building a client from the environment.

    The headline entry point: wrap an already-instrumented run and publish to your
    Grafana Cloud Sigil instance. When ``client`` is omitted one is built from
    ``endpoint``/``ingest_token``/``tenant_id``/``actor``, falling back to
    ``SIGIL_ENDPOINT``, ``SIGIL_AUTH_TOKEN``, ``SIGIL_AUTH_TENANT_ID`` or
    ``SIGIL_TENANT_ID``, and ``SIGIL_INGEST_ACTOR`` environment variables, and
    closed on exit. ``endpoint`` and the ingestion token are required::

        with experiment("nightly", suite=suite, candidate={"model_name": "gpt-5"}) as exp:
            for case in suite.cases:
                with exp.trial(case) as trial:
                    result = run_agent(case.input)
                    trial.score("final", value=result.score, passed=result.passed)
    """

    owns_client = client is None
    if client is None:
        from .client import Client

        resolved_endpoint = (
            endpoint or os.environ.get("SIGIL_ENDPOINT") or os.environ.get("SIGIL_API_ENDPOINT") or ""
        ).strip()
        if not resolved_endpoint:
            raise ValueError(
                "Sigil endpoint is required: pass endpoint= or set SIGIL_ENDPOINT to your Grafana Cloud Sigil URL"
            )
        resolved_tenant = (
            tenant_id or os.environ.get("SIGIL_AUTH_TENANT_ID") or os.environ.get("SIGIL_TENANT_ID") or ""
        ).strip()
        resolved_token = (ingest_token or os.environ.get("SIGIL_AUTH_TOKEN") or "").strip()
        if not resolved_token:
            raise ValueError("ingest_token is required: pass ingest_token= or set SIGIL_AUTH_TOKEN")
        client = Client(
            resolved_endpoint,
            tenant_id=resolved_tenant,
            ingest_token=resolved_token,
            actor=actor or os.environ.get("SIGIL_INGEST_ACTOR", ""),
            grafana_url=grafana_url,
            use_experimental_otel=use_experimental_otel,
        )
    exp = Experiment(
        client,
        experiment_id=experiment_id,
        name=name,
        suite=suite,
        candidate=candidate,
        default_evaluator=default_evaluator,
        description=description,
        tags=tags,
        metadata=metadata,
        use_experimental_otel=use_experimental_otel,
    )
    exp._owns_client = owns_client
    return exp

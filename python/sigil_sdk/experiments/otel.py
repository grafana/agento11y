"""OpenTelemetry GenAI evaluation telemetry for Sigil experiments.

The experiments surface writes scores over the v1 ingest path. When explicitly
enabled, it also emits OpenTelemetry so already-instrumented agents line up with
Sigil's eval model and a future OTLP eval materializer can read the same data.

We emit OpenTelemetry names directly — there is **no parallel ``sigil.*`` mirror**:

- **Merged names** are emitted as-is: the ``gen_ai.evaluation.result`` event and
  its attributes, ``gen_ai.request.model`` / ``gen_ai.agent.*`` on the candidate,
  and the ``test.*`` suite/case identity that already exists in general OTel
  (``test.suite.name``, ``test.suite.run.status``, ``test.case.name``,
  ``test.case.result.status``).
- **Proposed names** Sigil is pushing through the OTel GenAI SIG are emitted as a
  best-effort prediction and **may change until they land upstream**: evaluator
  and reference-set provenance under ``gen_ai.evaluation.*``, suite id/version,
  the run/case ids (#79), and the per-attempt trial identity ``test.case.run.*``.
  ``sigil.eval.schema.version`` stamps which prediction generation a consumer is
  reading, so a rename upstream is a visible version bump, not silent drift.
- Only concepts with **no upstream home at all** keep a ``sigil.*`` name (the
  schema-version marker here, and artifact refs in :mod:`.experiment`). Those are
  product nouns, not mirrors of a proposed standard. Cost is *not* among them: it
  has no OTel attribute and is derived from tokens, so the span carries the
  standard ``gen_ai.usage.*`` tokens and cost is rolled up server-side from REST.

The score verdict is the OTel ``gen_ai.evaluation.score.label`` (``pass``/``fail``)
and, at the case level, ``test.case.result.status`` — there is no separate Sigil
verdict attribute.
"""

from __future__ import annotations

from typing import Any

# --------------------------------------------------------------------------- #
# Schema + instrumentation identity
# --------------------------------------------------------------------------- #

INSTRUMENTATION_NAME = "sigil_sdk.experiments"
# Bumps whenever the predicted (not-yet-merged) attribute set changes, so a
# consumer can tell which generation of the proposed names it is reading.
SCHEMA_VERSION = "experiments-otel-2026-06"
ATTR_SCHEMA_VERSION = "sigil.eval.schema.version"

# Trial span name template.
TRIAL_SPAN_NAME = "eval.trial"

# --------------------------------------------------------------------------- #
# Eval result event — gen_ai.evaluation.result
# --------------------------------------------------------------------------- #

EVENT_EVAL_RESULT = "gen_ai.evaluation.result"  # merged (development)
EVAL_NAME = "gen_ai.evaluation.name"  # merged, Required
EVAL_SCORE_VALUE = "gen_ai.evaluation.score.value"  # merged
EVAL_SCORE_LABEL = "gen_ai.evaluation.score.label"  # merged; carries the pass/fail verdict
EVAL_EXPLANATION = "gen_ai.evaluation.explanation"  # merged
RESPONSE_ID = "gen_ai.response.id"  # merged

# Proposed (experimental — may change until merged): evaluator provenance.
EVAL_EVALUATOR_ID = "gen_ai.evaluation.evaluator.id"
EVAL_EVALUATOR_VERSION = "gen_ai.evaluation.evaluator.version"
EVAL_EVALUATOR_TYPE = "gen_ai.evaluation.evaluator.type"  # llm_judge|deterministic|human|custom
# Proposed (experimental, least settled): reference-set / dataset provenance.
EVAL_REFERENCE_SET_ID = "gen_ai.evaluation.reference_set.id"
EVAL_REFERENCE_SET_VERSION = "gen_ai.evaluation.reference_set.version"

# --------------------------------------------------------------------------- #
# Trial / suite identity on the trial span (test.* family)
# --------------------------------------------------------------------------- #

TEST_SUITE_RUN_ID = "test.suite.run.id"  # proposed for genai (#79) = experiment/run id
TEST_SUITE_NAME = "test.suite.name"  # merged (general OTel)
TEST_SUITE_RUN_STATUS = "test.suite.run.status"  # merged (general OTel)
TEST_SUITE_ID = "test.suite.id"  # proposed (Sigil) — suite identity distinct from name
TEST_SUITE_VERSION = "test.suite.version"  # proposed (Sigil)
TEST_CASE_ID = "test.case.id"  # proposed for genai (#79)
TEST_CASE_NAME = "test.case.name"  # merged (general OTel)
TEST_CASE_RESULT_STATUS = "test.case.result.status"  # merged; we also emit proposed error/skipped
TEST_CASE_RUN_ID = "test.case.run.id"  # proposed (Sigil) = per-attempt trial id
TEST_CASE_RUN_ATTEMPT = "test.case.run.attempt"  # proposed (Sigil) = 1-based attempt

OPERATION_NAME = "gen_ai.operation.name"
CONVERSATION_ID = "gen_ai.conversation.id"

# --------------------------------------------------------------------------- #
# Status maps (API value -> telemetry value)
# --------------------------------------------------------------------------- #

# Experiment/run status (API) -> test.suite.run.status (general OTel enum).
RUN_STATUS_MAP = {
    "running": "in_progress",
    "succeeded": "success",
    "completed": "success",
    "failed": "failure",
    "canceled": "aborted",
    "cancelled": "aborted",
}

# Trial status (API) -> test.case.result.status. The merged registry enum is
# `pass|fail` only, so we map to those and emit nothing for non-verdict states
# (running / errored / skipped) — the trial's terminal state lives on the REST
# trial and the span status. (`error`/`skipped` are a proposed enum extension, P4.)
TRIAL_STATUS_MAP = {
    "completed": "pass",
    "passed": "pass",
    "failed": "fail",
}


def run_status_telemetry(status: str) -> str:
    """Maps an API run status to its ``test.suite.run.status`` telemetry value."""

    return RUN_STATUS_MAP.get((status or "").strip().lower(), "in_progress")


def trial_status_telemetry(status: str) -> str:
    """Maps an API trial status to ``test.case.result.status`` (``pass``/``fail``).

    Returns ``""`` for non-verdict states so the caller omits the attribute (the
    merged enum is ``pass|fail`` only).
    """

    return TRIAL_STATUS_MAP.get((status or "").strip().lower(), "")


def score_label(passed: bool | None) -> str:
    """The OTel ``gen_ai.evaluation.score.label`` for a pass/fail verdict."""

    if passed is None:
        return ""
    return "pass" if passed else "fail"


def _put(attrs: dict[str, Any], key: str, value: Any) -> None:
    """Sets ``key`` only when ``value`` is meaningful (non-empty / not None)."""

    if value is None:
        return
    if isinstance(value, str) and value == "":
        return
    attrs[key] = value


def trial_identity_attributes(
    *,
    experiment_id: str,
    experiment_name: str = "",
    suite_id: str = "",
    suite_version: str = "",
    suite_name: str = "",
    test_case_id: str = "",
    test_case_name: str = "",
    attempt: int = 1,
    trial_id: str = "",
    run_status: str = "running",
    trial_status: str = "running",
    conversation_id: str = "",
    operation_name: str = "invoke_agent",
) -> dict[str, Any]:
    """Builds the ``test.*`` identity attribute set for a trial span."""

    attrs: dict[str, Any] = {ATTR_SCHEMA_VERSION: SCHEMA_VERSION}
    _put(attrs, OPERATION_NAME, operation_name)
    _put(attrs, TEST_SUITE_RUN_ID, experiment_id)
    _put(attrs, TEST_SUITE_NAME, suite_name or experiment_name)
    _put(attrs, TEST_SUITE_RUN_STATUS, run_status_telemetry(run_status))
    _put(attrs, TEST_SUITE_ID, suite_id)
    _put(attrs, TEST_SUITE_VERSION, suite_version)
    _put(attrs, TEST_CASE_ID, test_case_id)
    _put(attrs, TEST_CASE_NAME, test_case_name or test_case_id)
    _put(attrs, TEST_CASE_RESULT_STATUS, trial_status_telemetry(trial_status))
    _put(attrs, TEST_CASE_RUN_ID, trial_id)
    attrs[TEST_CASE_RUN_ATTEMPT] = int(attempt)
    _put(attrs, CONVERSATION_ID, conversation_id)
    return attrs


def score_event_attributes(
    *,
    name: str,
    value: float | bool | str | None,
    passed: bool | None = None,
    explanation: str = "",
    evaluator_id: str = "",
    evaluator_version: str = "",
    evaluator_kind: str = "",
    reference_set_id: str = "",
    reference_set_version: str = "",
    response_id: str = "",
) -> dict[str, Any]:
    """Builds the attribute set for one ``gen_ai.evaluation.result`` event."""

    attrs: dict[str, Any] = {}
    _put(attrs, EVAL_NAME, name)
    # OTel score.value is a double; only emit it for numeric/boolean values.
    if isinstance(value, bool):
        attrs[EVAL_SCORE_VALUE] = 1.0 if value else 0.0
    elif isinstance(value, (int, float)):
        attrs[EVAL_SCORE_VALUE] = float(value)
    # The verdict lives in the label: pass/fail when known, else a categorical value.
    label = score_label(passed)
    if not label and isinstance(value, str):
        label = value
    _put(attrs, EVAL_SCORE_LABEL, label)
    _put(attrs, EVAL_EXPLANATION, explanation)
    _put(attrs, RESPONSE_ID, response_id)
    # Proposed provenance (experimental).
    _put(attrs, EVAL_EVALUATOR_ID, evaluator_id)
    _put(attrs, EVAL_EVALUATOR_VERSION, evaluator_version)
    _put(attrs, EVAL_EVALUATOR_TYPE, evaluator_kind)
    _put(attrs, EVAL_REFERENCE_SET_ID, reference_set_id)
    _put(attrs, EVAL_REFERENCE_SET_VERSION, reference_set_version)
    return attrs

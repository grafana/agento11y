"""Error hierarchy used by agento11y Python SDK."""

from enum import Enum


class Agento11yError(Exception):
    """Base class for SDK-specific errors."""


class ValidationError(Agento11yError):
    """Raised when generation validation fails before enqueue."""


class EnqueueError(Agento11yError):
    """Raised when generation enqueue fails."""


class QueueFullError(EnqueueError):
    """Raised when generation queue is full."""


class ClientShutdownError(EnqueueError):
    """Raised when enqueue happens while shutdown is in progress."""


class MappingError(Agento11yError):
    """Raised when provider mapper logic fails."""


class RatingConflictError(Agento11yError):
    """Raised when rating idempotency key conflicts with a different payload."""


class RatingTransportError(Agento11yError):
    """Raised when rating submission transport fails."""


class NotFoundError(Agento11yError):
    """Raised when a requested resource does not exist (HTTP 404)."""


class ConflictKind(str, Enum):
    """Stable categories for experiment and suite HTTP 409 responses."""

    SCORE_COUNT_MISMATCH = "score_count_mismatch"
    RUNNING_TRIALS = "running_trials"
    TERMINAL = "terminal"
    IMMUTABLE_FIELD = "immutable_field"
    OPEN_DRAFT = "open_draft"
    UNKNOWN = "unknown"


class ConflictError(Agento11yError):
    """Raised when a request conflicts with current resource state (HTTP 409)."""

    def __init__(self, message: str, *, kind: ConflictKind = ConflictKind.UNKNOWN) -> None:
        super().__init__(message)
        self.kind = kind

    @property
    def recoverable(self) -> bool:
        return self.kind in {
            ConflictKind.SCORE_COUNT_MISMATCH,
            ConflictKind.RUNNING_TRIALS,
            ConflictKind.OPEN_DRAFT,
        }


def classify_conflict(message: str) -> ConflictKind:
    """Classifies backend conflict text without making callers parse strings."""

    value = (message or "").lower()
    if "score_count" in value or "score count" in value or ("expected " in value and " scores, found " in value):
        return ConflictKind.SCORE_COUNT_MISMATCH
    if "running trial" in value or ("cannot complete experiment with " in value and " trial" in value):
        return ConflictKind.RUNNING_TRIALS
    if (
        "terminal" in value
        or "already completed" in value
        or "already finalized" in value
        or "already published" in value
    ):
        return ConflictKind.TERMINAL
    if (
        "immutable" in value
        or "cannot change" in value
        or "conflicts with the existing experiment" in value
        or "not a draft" in value
    ):
        return ConflictKind.IMMUTABLE_FIELD
    if "open draft" in value or "draft already exists" in value:
        return ConflictKind.OPEN_DRAFT
    return ConflictKind.UNKNOWN


class ExperimentTransportError(Agento11yError):
    """Raised when an experiment request fails."""


class ScoreExportError(Agento11yError):
    """Raised when a score export request fails at the transport level."""


class HookDeniedError(Agento11yError):
    """Raised when a synchronous hook evaluation responds with action=deny."""

    def __init__(
        self,
        reason: str = "",
        rule_id: str = "",
        evaluations: list | None = None,
    ) -> None:
        normalized_reason = (reason or "").strip()
        if normalized_reason == "":
            normalized_reason = "request blocked by Agent Observability hook rule"
        clean_rule = (rule_id or "").strip()
        if clean_rule != "":
            message = f"agento11y hook denied by rule {clean_rule}: {normalized_reason}"
        else:
            message = f"agento11y hook denied: {normalized_reason}"
        super().__init__(message)
        self.reason = normalized_reason
        self.rule_id = clean_rule
        self.evaluations = list(evaluations) if evaluations else []


class HookTransportError(Agento11yError):
    """Raised when hook evaluation transport fails and fail_open is disabled."""

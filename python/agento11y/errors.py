"""Error hierarchy used by agento11y Python SDK."""


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


class ConflictError(Agento11yError):
    """Raised when a request conflicts with current resource state (HTTP 409)."""


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

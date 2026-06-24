"""Error hierarchy used by Sigil Python SDK."""


class SigilError(Exception):
    """Base class for SDK-specific errors."""


class ValidationError(SigilError):
    """Raised when generation validation fails before enqueue."""


class EnqueueError(SigilError):
    """Raised when generation enqueue fails."""


class QueueFullError(EnqueueError):
    """Raised when generation queue is full."""


class ClientShutdownError(EnqueueError):
    """Raised when enqueue happens while shutdown is in progress."""


class MappingError(SigilError):
    """Raised when provider mapper logic fails."""


class RatingConflictError(SigilError):
    """Raised when rating idempotency key conflicts with a different payload."""


class RatingTransportError(SigilError):
    """Raised when rating submission transport fails."""


class NotFoundError(SigilError):
    """Raised when a requested resource does not exist (HTTP 404)."""


class ConflictError(SigilError):
    """Raised when a request conflicts with current resource state (HTTP 409)."""


class ExperimentTransportError(SigilError):
    """Raised when an experiment request fails."""


class ScoreExportError(SigilError):
    """Raised when a score export request fails at the transport level."""


class HookDeniedError(SigilError):
    """Raised when a synchronous hook evaluation responds with action=deny."""

    def __init__(
        self,
        reason: str = "",
        rule_id: str = "",
        evaluations: list | None = None,
    ) -> None:
        normalized_reason = (reason or "").strip()
        if normalized_reason == "":
            normalized_reason = "request blocked by Sigil hook rule"
        clean_rule = (rule_id or "").strip()
        if clean_rule != "":
            message = f"sigil hook denied by rule {clean_rule}: {normalized_reason}"
        else:
            message = f"sigil hook denied: {normalized_reason}"
        super().__init__(message)
        self.reason = normalized_reason
        self.rule_id = clean_rule
        self.evaluations = list(evaluations) if evaluations else []


class HookTransportError(SigilError):
    """Raised when hook evaluation transport fails and fail_open is disabled."""

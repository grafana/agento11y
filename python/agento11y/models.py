"""Core typed models for the Sigil Python SDK."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any


class GenerationMode(str, Enum):
    """Generation execution mode."""

    SYNC = "SYNC"
    STREAM = "STREAM"


class MessageRole(str, Enum):
    """Allowed message roles."""

    USER = "user"
    ASSISTANT = "assistant"
    TOOL = "tool"


class PartKind(str, Enum):
    """Allowed message part kinds."""

    TEXT = "text"
    THINKING = "thinking"
    TOOL_CALL = "tool_call"
    TOOL_RESULT = "tool_result"


class ContentCaptureMode(str, Enum):
    """Controls what content is included in exported generation payloads and OTel span attributes.

    Note: user-provided metadata and tags are NOT stripped, even in METADATA_ONLY mode.
    Callers are responsible for ensuring these dicts do not contain sensitive content.
    """

    DEFAULT = "default"
    FULL = "full"
    NO_TOOL_CONTENT = "no_tool_content"
    METADATA_ONLY = "metadata_only"
    # FULL_WITH_METADATA_SPANS splits the proto and span paths for generation
    # content. The proto export keeps full content; the OTel span omits
    # ``agento11y.conversation.title``, ``gen_ai.tool.call.arguments``,
    # ``gen_ai.tool.call.result``, and ``gen_ai.embeddings.input_texts``.
    # Use this mode when the gRPC ingest destination is private but the OTel
    # trace/metric destination is shared and must not receive any content.
    # Tool execution and embedding spans behave identically to METADATA_ONLY
    # under this mode (they have no separate gRPC export). Rating comments
    # are preserved.
    FULL_WITH_METADATA_SPANS = "full_with_metadata_spans"


_metadata_key_content_capture_mode = "agento11y.sdk.content_capture_mode"


class ArtifactKind(str, Enum):
    """Allowed raw artifact kinds."""

    REQUEST = "request"
    RESPONSE = "response"
    TOOLS = "tools"
    PROVIDER_EVENT = "provider_event"


class ConversationRatingValue(str, Enum):
    """Allowed conversation rating values."""

    GOOD = "CONVERSATION_RATING_VALUE_GOOD"
    BAD = "CONVERSATION_RATING_VALUE_BAD"


@dataclass(slots=True)
class ModelRef:
    """Provider/model identity."""

    provider: str = ""
    name: str = ""


@dataclass(slots=True)
class ToolDefinition:
    """Tool definition visible to the model."""

    name: str = ""
    description: str = ""
    type: str = ""
    input_schema_json: bytes = b""
    deferred: bool = False


@dataclass(slots=True)
class TokenUsage:
    """Token usage counters for request/response.

    ``input_tokens`` is fresh, non-cached input. Cache-inclusive provider
    adapters subtract ``cache_read_input_tokens`` before setting it.
    ``reasoning_tokens`` is an explanatory sub-bucket and may overlap with
    ``output_tokens`` depending on provider semantics.
    """

    input_tokens: int = 0
    output_tokens: int = 0
    total_tokens: int = 0
    cache_read_input_tokens: int = 0
    cache_write_input_tokens: int = 0
    reasoning_tokens: int = 0
    #: Set by SDK-owned adapters that already normalized this usage to the
    #: disjoint contract (fresh input, additive cache buckets). Consumers must
    #: not re-derive fresh input when true. Manual usage leaves it False.
    input_is_disjoint: bool = False

    def normalize(self) -> TokenUsage:
        """Returns a copy with `total_tokens` auto-filled when missing."""

        normalized = TokenUsage(
            input_tokens=self.input_tokens,
            output_tokens=self.output_tokens,
            total_tokens=self.total_tokens,
            cache_read_input_tokens=self.cache_read_input_tokens,
            cache_write_input_tokens=self.cache_write_input_tokens,
            reasoning_tokens=self.reasoning_tokens,
            input_is_disjoint=self.input_is_disjoint,
        )
        if normalized.total_tokens == 0:
            normalized.total_tokens = (
                normalized.input_tokens
                + normalized.output_tokens
                + normalized.cache_read_input_tokens
                + normalized.cache_write_input_tokens
            )
        return normalized


@dataclass(slots=True)
class PartMetadata:
    """Provider-specific payload metadata."""

    provider_type: str = ""


@dataclass(slots=True)
class ToolCall:
    """Tool call payload for assistant messages."""

    name: str
    id: str = ""
    input_json: bytes = b""


@dataclass(slots=True)
class ToolResult:
    """Tool result payload for tool-role messages."""

    tool_call_id: str = ""
    name: str = ""
    content: str = ""
    content_json: bytes = b""
    is_error: bool = False


@dataclass(slots=True)
class Part:
    """Typed message part."""

    kind: PartKind
    text: str = ""
    thinking: str = ""
    tool_call: ToolCall | None = None
    tool_result: ToolResult | None = None
    metadata: PartMetadata = field(default_factory=PartMetadata)


@dataclass(slots=True)
class Message:
    """Normalized message payload."""

    role: MessageRole
    parts: list[Part]
    name: str = ""


@dataclass(slots=True)
class Artifact:
    """Optional raw provider artifact."""

    kind: ArtifactKind
    name: str = ""
    content_type: str = ""
    payload: bytes = b""
    record_id: str = ""
    uri: str = ""


@dataclass(slots=True)
class GenerationStart:
    """Seed fields used when generation recording starts."""

    model: ModelRef
    id: str = ""
    conversation_id: str = ""
    conversation_title: str = ""
    user_id: str = ""
    agent_name: str = ""
    agent_version: str = ""
    mode: GenerationMode | None = None
    operation_name: str = ""
    system_prompt: str = ""
    max_tokens: int | None = None
    temperature: float | None = None
    top_p: float | None = None
    tool_choice: str | None = None
    thinking_enabled: bool | None = None
    tools: list[ToolDefinition] = field(default_factory=list)
    content_capture: ContentCaptureMode = ContentCaptureMode.DEFAULT
    tags: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    parent_generation_ids: list[str] = field(default_factory=list)
    effective_version: str = ""
    started_at: datetime | None = None


@dataclass(slots=True)
class EmbeddingStart:
    """Seed fields used when embedding recording starts."""

    model: ModelRef
    agent_name: str = ""
    agent_version: str = ""
    dimensions: int | None = None
    encoding_format: str = ""
    tags: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    started_at: datetime | None = None


@dataclass(slots=True)
class EmbeddingResult:
    """Result fields set before an embedding span is finalized."""

    input_count: int = 0
    input_tokens: int = 0
    input_texts: list[str] = field(default_factory=list)
    response_model: str = ""
    dimensions: int | None = None


@dataclass(slots=True)
class Generation:
    """Final normalized generation payload exported by the SDK."""

    id: str = ""
    conversation_id: str = ""
    conversation_title: str = ""
    user_id: str = ""
    agent_name: str = ""
    agent_version: str = ""
    mode: GenerationMode | None = None
    operation_name: str = ""
    trace_id: str = ""
    span_id: str = ""
    model: ModelRef = field(default_factory=ModelRef)
    response_id: str = ""
    response_model: str = ""
    system_prompt: str = ""
    max_tokens: int | None = None
    temperature: float | None = None
    top_p: float | None = None
    tool_choice: str | None = None
    thinking_enabled: bool | None = None
    input: list[Message] = field(default_factory=list)
    output: list[Message] = field(default_factory=list)
    tools: list[ToolDefinition] = field(default_factory=list)
    usage: TokenUsage = field(default_factory=TokenUsage)
    stop_reason: str = ""
    started_at: datetime | None = None
    completed_at: datetime | None = None
    tags: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    artifacts: list[Artifact] = field(default_factory=list)
    call_error: str = ""
    parent_generation_ids: list[str] = field(default_factory=list)
    effective_version: str = ""


@dataclass(slots=True)
class WorkflowStep:
    """Workflow step execution record — separate from Generation."""

    id: str = ""
    conversation_id: str = ""
    step_name: str = ""
    framework: str = ""
    agent_name: str = ""
    agent_version: str = ""
    started_at: datetime | None = None
    completed_at: datetime | None = None
    input_state: dict[str, Any] = field(default_factory=dict)
    output_state: dict[str, Any] = field(default_factory=dict)
    error: str = ""
    tags: dict[str, str] = field(default_factory=dict)
    metadata: dict[str, Any] = field(default_factory=dict)
    linked_generation_ids: list[str] = field(default_factory=list)
    parent_step_ids: list[str] = field(default_factory=list)
    trace_id: str = ""
    span_id: str = ""


@dataclass(slots=True)
class ExportWorkflowStepResult:
    """Per-item workflow step ingest result."""

    step_id: str
    accepted: bool
    error: str = ""


@dataclass(slots=True)
class ExportWorkflowStepsRequest:
    """Workflow step export request payload."""

    workflow_steps: list[WorkflowStep]


@dataclass(slots=True)
class ExportWorkflowStepsResponse:
    """Workflow step export response payload."""

    results: list[ExportWorkflowStepResult]


@dataclass(slots=True)
class ToolExecutionStart:
    """Seed fields for execute_tool span recording."""

    tool_name: str
    tool_call_id: str = ""
    tool_type: str = ""
    tool_description: str = ""
    conversation_id: str = ""
    conversation_title: str = ""
    agent_name: str = ""
    agent_version: str = ""
    request_model: str = ""
    request_provider: str = ""
    include_content: bool = False
    content_capture: ContentCaptureMode = ContentCaptureMode.DEFAULT
    started_at: datetime | None = None


@dataclass(slots=True)
class ToolExecutionEnd:
    """Completion payload for execute_tool span recording."""

    arguments: Any = None
    result: Any = None
    completed_at: datetime | None = None


@dataclass(slots=True)
class ExecuteToolCallsOptions:
    """Options for :meth:`agento11y.client.Client.execute_tool_calls`."""

    conversation_id: str = ""
    conversation_title: str = ""
    agent_name: str = ""
    agent_version: str = ""
    content_capture: ContentCaptureMode = ContentCaptureMode.DEFAULT
    request_model: str = ""
    request_provider: str = ""
    tool_type: str = "function"
    tags: dict[str, str] = field(default_factory=dict)


@dataclass(slots=True)
class ExportGenerationResult:
    """Per-item generation ingest result."""

    generation_id: str
    accepted: bool
    error: str = ""


@dataclass(slots=True)
class ExportGenerationsRequest:
    """Generation export request payload."""

    generations: list[Generation]


@dataclass(slots=True)
class ExportGenerationsResponse:
    """Generation export response payload."""

    results: list[ExportGenerationResult]


@dataclass(slots=True)
class ConversationRatingInput:
    """SDK input payload for submitting a conversation rating."""

    rating_id: str
    rating: ConversationRatingValue
    comment: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)
    generation_id: str = ""
    rater_id: str = ""
    source: str = ""


@dataclass(slots=True)
class ConversationRating:
    """Conversation rating event returned by Sigil."""

    rating_id: str
    conversation_id: str
    rating: ConversationRatingValue
    created_at: datetime
    comment: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)
    generation_id: str = ""
    rater_id: str = ""
    source: str = ""


@dataclass(slots=True)
class ConversationRatingSummary:
    """Aggregated conversation rating summary."""

    total_count: int
    good_count: int
    bad_count: int
    latest_rated_at: datetime
    has_bad_rating: bool
    latest_rating: ConversationRatingValue | None = None
    latest_bad_at: datetime | None = None


@dataclass(slots=True)
class SubmitConversationRatingResponse:
    """Conversation rating create response envelope."""

    rating: ConversationRating
    summary: ConversationRatingSummary


def utc_now() -> datetime:
    """Returns the current UTC timestamp."""

    return datetime.now(timezone.utc)


def text_part(text: str) -> Part:
    """Creates a text part."""

    return Part(kind=PartKind.TEXT, text=text)


def thinking_part(thinking: str) -> Part:
    """Creates a thinking part."""

    return Part(kind=PartKind.THINKING, thinking=thinking)


def tool_call_part(tool_call: ToolCall) -> Part:
    """Creates a tool-call part."""

    return Part(kind=PartKind.TOOL_CALL, tool_call=tool_call)


def tool_result_part(tool_result: ToolResult) -> Part:
    """Creates a tool-result part."""

    return Part(kind=PartKind.TOOL_RESULT, tool_result=tool_result)


def user_text_message(text: str) -> Message:
    """Creates a user message with one text part."""

    return Message(role=MessageRole.USER, parts=[text_part(text)])


def assistant_text_message(text: str) -> Message:
    """Creates an assistant message with one text part."""

    return Message(role=MessageRole.ASSISTANT, parts=[text_part(text)])


def tool_result_message(tool_call_id: str, content: str) -> Message:
    """Creates a tool message with one tool-result part."""

    return Message(
        role=MessageRole.TOOL,
        parts=[
            tool_result_part(
                ToolResult(
                    tool_call_id=tool_call_id,
                    content=content,
                )
            )
        ],
    )


# ---------------------------------------------------------------------------
# Offline evaluation: experiments and scores
#
# These models map to the Sigil experiment and score APIs (HTTP):
#   POST   /api/v1/experiment-runs:upsert
#   POST   /api/v1/experiment-runs/{run_id}:finalize
#   GET    /api/v1/eval/experiments/{run_id}
#   POST   /api/v1/scores:export
#   GET    /api/v1/eval/experiments/{run_id}/report
# ---------------------------------------------------------------------------


class ExperimentStatus(str, Enum):
    """Lifecycle status of an experiment run (server spelling)."""

    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELED = "canceled"


class ExperimentSource(str, Enum):
    """Origin of an experiment run.

    ``EXTERNAL`` runs are driven by SDK/CI callers that export their own
    generations and scores. ``COLLECTION`` runs are fanned out server-side from
    a saved conversation collection.
    """

    EXTERNAL = "external"
    COLLECTION = "collection"


@dataclass(slots=True)
class ScoreValue:
    """A single typed score value. Exactly one field must be set.

    The boolean field serializes to the JSON key ``bool`` to match the Sigil
    score schema, while staying a valid Python attribute name (``boolean``).
    """

    number: float | None = None
    boolean: bool | None = None
    string: str | None = None


@dataclass(slots=True)
class ScoreSource:
    """Provenance for an exported score (e.g. ``kind="experiment"``)."""

    kind: str = ""
    id: str = ""


@dataclass(slots=True)
class ScoreItem:
    """A score to export via ``POST /api/v1/scores:export``.

    The backend keys scores on ``experiment_id`` (the canonical run identifier);
    set it to attach the score to an experiment report. A score must reference a
    ``generation_id`` **or** a ``trial_id``. ``run_id`` is accepted as a
    client-side alias for ``experiment_id`` and is mapped to ``experiment_id`` on
    the wire — the backend has no ``run_id`` field and rejects unknown fields.
    """

    score_id: str
    evaluator_id: str
    evaluator_version: str
    score_key: str
    value: ScoreValue
    generation_id: str = ""
    conversation_id: str = ""
    trace_id: str = ""
    span_id: str = ""
    rule_id: str = ""
    experiment_id: str = ""
    trial_id: str = ""
    test_case_id: str = ""
    grader_conversation_id: str = ""
    grader_generation_id: str = ""
    grader_trace_id: str = ""
    run_id: str = ""  # client-side alias for experiment_id (mapped on the wire)
    evaluator_kind: str = ""
    passed: bool | None = None
    explanation: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)
    created_at: datetime | None = None
    source: ScoreSource | None = None

    @property
    def resolved_experiment_id(self) -> str:
        """The experiment id to send on the wire (``experiment_id`` or ``run_id``)."""

        return (self.experiment_id or self.run_id or "").strip()


@dataclass(slots=True)
class ExportScoreResult:
    """Per-score outcome from a score export request.

    ``accepted`` is true only for a fresh insert. A re-sent ``score_id`` returns
    ``accepted=False`` with ``status="duplicate"`` — inspect ``status`` (one of
    ``accepted`` / ``duplicate`` / ``rejected``) for the precise outcome.
    """

    score_id: str
    accepted: bool
    status: str = ""
    error: str = ""


@dataclass(slots=True)
class ExportScoresResponse:
    """Response envelope from ``POST /api/v1/scores:export``.

    The aggregate counts come straight from the server so a runner can assert
    ``accepted + duplicates == requested`` and detect undercounted exports
    without re-tallying per item.
    """

    results: list[ExportScoreResult] = field(default_factory=list)
    accepted: int = 0
    duplicates: int = 0
    rejected_count: int = 0

    @property
    def accepted_count(self) -> int:
        """Number of fresh scores the server accepted."""

        return self.accepted or sum(1 for r in self.results if r.accepted)

    @property
    def duplicate_count(self) -> int:
        """Number of scores the server treated as idempotent duplicates."""

        return self.duplicates or sum(1 for r in self.results if r.status == "duplicate")

    @property
    def rejected(self) -> list[ExportScoreResult]:
        """Scores the server rejected, with their error detail."""

        return [r for r in self.results if not r.accepted and r.status != "duplicate"]


@dataclass(slots=True)
class ExperimentEvaluator:
    """Pairs an evaluator id with the selector deciding which generations it scores.

    Only relevant for ``COLLECTION`` runs.
    """

    id: str
    selector: str


@dataclass(slots=True)
class CreateExperimentRequest:
    """Request body for ``POST /api/v1/experiment-runs:upsert``."""

    name: str
    source: ExperimentSource | str = ExperimentSource.EXTERNAL
    run_id: str = ""
    description: str = ""
    tags: list[str] = field(default_factory=list)
    collection_id: str = ""
    evaluators: list[ExperimentEvaluator] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class Experiment:
    """An experiment run as returned by Sigil."""

    run_id: str
    name: str
    source: str
    status: str
    tenant_id: str = ""
    description: str = ""
    tags: list[str] = field(default_factory=list)
    collection_id: str = ""
    evaluators: list[ExperimentEvaluator] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)
    score_count: int = 0
    error: str = ""
    created_by: str = ""
    created_at: datetime | None = None
    updated_at: datetime | None = None
    started_at: datetime | None = None
    completed_at: datetime | None = None


@dataclass(slots=True)
class ExperimentReportSummary:
    """Aggregate summary block of an experiment report.

    Mirrors the backend's typed trial rollup (``sigil/internal/eval/types.go``):
    trial counts, pass-rate, pass@k / pass^k, and average final score, plus the
    derived ``total_cost`` and ``total_tokens``.
    """

    test_case_count: int = 0
    trial_count: int = 0
    completed_count: int = 0
    failed_count: int = 0
    canceled_count: int = 0
    pass_rate: float = 0.0
    pass_at_k: dict[str, float] = field(default_factory=dict)
    pass_power_k: dict[str, float] = field(default_factory=dict)
    final_score_avg: float = 0.0
    total_cost: float = 0.0
    total_tokens: int = 0


@dataclass(slots=True)
class ExperimentReport:
    """Aggregated experiment report from
    ``GET /api/v1/eval/experiments/{experiment_id}/report``.

    ``rows`` is the raw per-test-case result list (kept as decoded JSON); ``run``
    and ``summary`` are promoted to typed objects.
    """

    run: Experiment
    summary: ExperimentReportSummary
    rows: list[dict[str, Any]] = field(default_factory=list)

"""Experiment lifecycle and score export transport for the agento11y SDK.

External writes go through the ingest lifecycle on the same tenant token used
for generation export:

  POST   /api/v1/experiment-runs:upsert              create or claim an external run
  POST   /api/v1/experiment-runs/{run_id}:finalize   finalize an external run
  POST   /api/v1/scores:export                       publish scores (attribute via run_id)
  POST   /api/v1/experiment-runs/{run_id}/trials     create or claim a typed trial
  PATCH  /api/v1/experiment-runs/{run_id}/trials/{trial_id}
                                                        update a typed trial
  POST   /api/v1/experiment-runs/{run_id}/trials/{trial_id}/artifacts:upload
                                                        attach a trial artifact

Reads use the Agent Observability query routes with the same configured endpoint and auth:

  GET    /api/v1/eval/experiments/{run_id}           fetch a run
  GET    /api/v1/eval/experiments/{run_id}/scores    list run scores (paginated)
  GET    /api/v1/eval/experiments/{run_id}/report    aggregated run report

The functions here are thin; :class:`agento11y.client.Client` wraps them with
resolved endpoint, insecure flag, and auth headers.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any
from urllib import error as urllib_error
from urllib import parse as urllib_parse
from urllib import request as urllib_request

from .errors import (
    Agento11yError,
    ConflictError,
    ExperimentTransportError,
    NotFoundError,
    ScoreExportError,
    ValidationError,
)
from .models import (
    CreateExperimentRequest,
    Experiment,
    ExperimentEvaluator,
    ExperimentReport,
    ExperimentReportSummary,
    ExperimentStatus,
    ExportScoreResult,
    ExportScoresResponse,
    ScoreItem,
    ScoreValue,
)

_EVAL_EXPERIMENTS_SUFFIX = "/eval/experiments"
_EXPERIMENT_RUNS_UPSERT_PATH = "/api/v1/experiment-runs:upsert"
_EXPERIMENT_RUNS_PREFIX = "/api/v1/experiment-runs"
_SCORES_EXPORT_PATH = "/api/v1/scores:export"
_DEFAULT_PATH_PREFIX = "/api/v1"
_DEFAULT_TIMEOUT = 30.0
_MAX_RESPONSE_BYTES = 8 << 20
_EXPERIMENT_RUN_SOURCE = {"kind": "sdk", "id": "python"}


@dataclass(slots=True)
class RetryPolicy:
    """Retry behavior for experiment and score requests.

    Retries cover request timeouts, connection errors, HTTP 429, and HTTP 5xx,
    using exponential backoff bounded by ``max_backoff``. 4xx responses other
    than 429 are not retried (they are caller errors).
    """

    max_retries: int = 3
    initial_backoff: float = 0.1
    max_backoff: float = 5.0
    timeout: float = _DEFAULT_TIMEOUT


# --------------------------------------------------------------------------- #
# Public API
# --------------------------------------------------------------------------- #


def create_experiment(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    request: CreateExperimentRequest,
    retry: RetryPolicy | None = None,
) -> Experiment:
    """Creates or idempotently claims an external experiment run."""

    name = (request.name or "").strip()
    if name == "":
        raise ValidationError("agento11y experiment validation failed: name is required")
    if _enum_value(request.source) != "external":
        raise ValidationError("agento11y experiment validation failed: experiment-run ingest requires source=external")

    url = _base_url(api_endpoint, insecure) + _EXPERIMENT_RUNS_UPSERT_PATH
    payload = _serialize_upsert_request(request)
    body = _request_json("POST", url, headers, payload, retry, ExperimentTransportError, "experiment create")
    return _parse_experiment_run_response(body)


def get_experiment(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    run_id: str,
    path_prefix: str = _DEFAULT_PATH_PREFIX,
    retry: RetryPolicy | None = None,
) -> Experiment:
    """Fetches a single experiment run by id."""

    url = _experiment_url(api_endpoint, insecure, run_id, path_prefix)
    body = _request_json("GET", url, headers, None, retry, ExperimentTransportError, "experiment get")
    return _parse_experiment(body)


def finalize_experiment(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    run_id: str,
    status: ExperimentStatus | str,
    score_count: int | None = None,
    error: str | None = None,
    retry: RetryPolicy | None = None,
) -> Experiment:
    """Finalizes an external experiment run as ``succeeded`` or ``failed``."""

    normalized_status = _enum_value(status).strip().lower()
    # The backend's terminal success status is `completed`; accept `succeeded` as
    # a friendly alias and map it onto the wire value the server validates.
    if normalized_status in ("succeeded", "completed"):
        normalized_status = "completed"
    elif normalized_status != "failed":
        raise ValidationError("agento11y experiment validation failed: status must be completed or failed")
    normalized_run_id = _validate_run_id(run_id)
    url = (
        _base_url(api_endpoint, insecure)
        + f"{_EXPERIMENT_RUNS_PREFIX}/{urllib_parse.quote(normalized_run_id, safe='')}:finalize"
    )
    payload: dict[str, Any] = {
        "status": normalized_status,
        "source": dict(_EXPERIMENT_RUN_SOURCE),
    }
    if score_count is not None:
        payload["score_count"] = score_count
    if error:
        payload["error"] = error
    body = _request_json("POST", url, headers, payload, retry, ExperimentTransportError, "experiment finalize")
    return _parse_experiment_run_response(body)


def export_scores(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    scores: list[ScoreItem],
    scores_path: str = _SCORES_EXPORT_PATH,
    retry: RetryPolicy | None = None,
) -> ExportScoresResponse:
    """Publishes scores; set ``run_id`` on each score to attribute it to a run.

    ``scores_path`` defaults to the direct ingest path; on Grafana Cloud it is
    the plugin proxy's score route (``.../eval/scores:export``).
    """

    if not scores:
        return ExportScoresResponse(results=[])
    for score in scores:
        _validate_score(score)

    url = _base_url(api_endpoint, insecure) + scores_path
    payload = {"scores": [_serialize_score(score) for score in scores]}
    body = _request_json("POST", url, headers, payload, retry, ScoreExportError, "score export")
    return _parse_export_scores_response(body)


def create_test_case_trial(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    experiment_id: str,
    request: dict[str, Any],
    path_prefix: str = _DEFAULT_PATH_PREFIX,
    retry: RetryPolicy | None = None,
) -> dict[str, Any]:
    """Creates or idempotently upserts a typed trial under an experiment run.

    Typed trials populate the structured experiment report. They are accepted on
    the tenant ingest credential (no Grafana actor required).
    """

    normalized = _validate_run_id(experiment_id)
    url = (
        _base_url(api_endpoint, insecure)
        + f"{_EXPERIMENT_RUNS_PREFIX}/{urllib_parse.quote(normalized, safe='')}/trials"
    )
    body = _request_json("POST", url, headers, request, retry, ExperimentTransportError, "test case trial create")
    return body if isinstance(body, dict) else {}


def update_test_case_trial(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    experiment_id: str,
    trial_id: str,
    request: dict[str, Any],
    path_prefix: str = _DEFAULT_PATH_PREFIX,
    retry: RetryPolicy | None = None,
) -> dict[str, Any]:
    """Patches a typed trial's status / usage rollups."""

    normalized = (trial_id or "").strip()
    if normalized == "":
        raise ValidationError("agento11y test case trial validation failed: trial_id is required")
    run_id = (experiment_id or "").strip()
    if run_id == "":
        raise ValidationError("agento11y test case trial validation failed: experiment_id is required for trial update")
    quoted_run_id = urllib_parse.quote(run_id, safe="")
    quoted_trial_id = urllib_parse.quote(normalized, safe="")
    url = _base_url(api_endpoint, insecure) + f"{_EXPERIMENT_RUNS_PREFIX}/{quoted_run_id}/trials/{quoted_trial_id}"
    body = _request_json("PATCH", url, headers, request, retry, ExperimentTransportError, "test case trial update")
    return body if isinstance(body, dict) else {}


def upload_trial_artifact(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    experiment_id: str,
    trial_id: str,
    name: str,
    kind: str,
    content: bytes,
    mime: str = "",
    retry: RetryPolicy | None = None,
) -> dict[str, Any]:
    """Uploads raw artifact bytes to the experiment-run ingest route."""

    run_id = (experiment_id or "").strip()
    if run_id == "":
        raise ValidationError("agento11y artifact validation failed: experiment_id is required")
    normalized_trial_id = (trial_id or "").strip()
    if normalized_trial_id == "":
        raise ValidationError("agento11y artifact validation failed: trial_id is required")
    normalized_name = (name or "").strip()
    if normalized_name == "":
        raise ValidationError("agento11y artifact validation failed: name is required")
    normalized_kind = (kind or "").strip()
    if normalized_kind == "":
        raise ValidationError("agento11y artifact validation failed: kind is required")
    body = content if isinstance(content, bytes) else bytes(content or b"")
    if not body:
        raise ValidationError("agento11y artifact validation failed: content is required")

    query = urllib_parse.urlencode({"name": normalized_name, "kind": normalized_kind, "mime": (mime or "").strip()})
    url = (
        _base_url(api_endpoint, insecure)
        + f"{_EXPERIMENT_RUNS_PREFIX}/{urllib_parse.quote(run_id, safe='')}/trials/"
        + f"{urllib_parse.quote(normalized_trial_id, safe='')}/artifacts:upload?{query}"
    )
    request_headers = {**(headers or {}), "Content-Type": (mime or "").strip() or "application/octet-stream"}
    response = _request_bytes_json(
        "POST",
        url,
        request_headers,
        body,
        retry,
        ExperimentTransportError,
        "trial artifact upload",
    )
    return response if isinstance(response, dict) else {}


def list_experiment_scores(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    run_id: str,
    limit: int = 50,
    cursor: str | None = None,
    path_prefix: str = _DEFAULT_PATH_PREFIX,
    retry: RetryPolicy | None = None,
) -> tuple[list[dict[str, Any]], str | None]:
    """Lists stored scores for a run. Returns ``(items, next_cursor)``.

    Score items are returned as decoded JSON dicts for this first iteration.
    """

    base = _experiment_url(api_endpoint, insecure, run_id, path_prefix) + "/scores"
    query: dict[str, str] = {"limit": str(limit)}
    if cursor:
        query["cursor"] = cursor
    url = base + "?" + urllib_parse.urlencode(query)
    body = _request_json("GET", url, headers, None, retry, ExperimentTransportError, "experiment scores list")
    items = body.get("items") if isinstance(body, dict) else None
    next_cursor = body.get("next_cursor") if isinstance(body, dict) else None
    return (list(items) if isinstance(items, list) else []), _normalize_cursor(next_cursor)


def get_experiment_report(
    *,
    api_endpoint: str,
    insecure: bool,
    headers: dict[str, str],
    run_id: str,
    path_prefix: str = _DEFAULT_PATH_PREFIX,
    retry: RetryPolicy | None = None,
) -> ExperimentReport:
    """Fetches the aggregated report for a run."""

    url = _experiment_url(api_endpoint, insecure, run_id, path_prefix) + "/report"
    body = _request_json("GET", url, headers, None, retry, ExperimentTransportError, "experiment report")
    return _parse_report(body)


# --------------------------------------------------------------------------- #
# Serialization
# --------------------------------------------------------------------------- #


def _serialize_upsert_request(request: CreateExperimentRequest) -> dict[str, Any]:
    out: dict[str, Any] = {
        "name": request.name.strip(),
        "source": dict(_EXPERIMENT_RUN_SOURCE),
    }
    # The backend ingest route keys on `experiment_id`, not `run_id`, and rejects
    # unknown fields. `CreateExperimentRequest.run_id` is the client-side spelling.
    if request.run_id:
        out["experiment_id"] = request.run_id
    if request.description:
        out["description"] = request.description
    if request.tags:
        out["tags"] = list(request.tags)
    if request.metadata:
        out["metadata"] = dict(request.metadata)
    return out


def _serialize_score(score: ScoreItem) -> dict[str, Any]:
    out: dict[str, Any] = {
        "score_id": score.score_id,
        "evaluator_id": score.evaluator_id,
        "evaluator_version": score.evaluator_version,
        "score_key": score.score_key,
        "value": _serialize_score_value(score.value),
    }
    if score.generation_id:
        out["generation_id"] = score.generation_id
    if score.conversation_id:
        out["conversation_id"] = score.conversation_id
    # `experiment_id` is the canonical run key; `run_id` is a client-side alias.
    experiment_id = score.resolved_experiment_id
    if experiment_id:
        out["experiment_id"] = experiment_id
    if score.trial_id:
        out["trial_id"] = score.trial_id
    if score.test_case_id:
        out["test_case_id"] = score.test_case_id
    if score.trace_id:
        out["trace_id"] = score.trace_id
    if score.span_id:
        out["span_id"] = score.span_id
    if score.grader_conversation_id:
        out["grader_conversation_id"] = score.grader_conversation_id
    if score.grader_generation_id:
        out["grader_generation_id"] = score.grader_generation_id
    if score.grader_trace_id:
        out["grader_trace_id"] = score.grader_trace_id
    if score.rule_id:
        out["rule_id"] = score.rule_id
    if score.passed is not None:
        out["passed"] = score.passed
    if score.explanation:
        out["explanation"] = score.explanation
    if score.metadata:
        out["metadata"] = dict(score.metadata)
    if score.created_at is not None:
        out["created_at"] = _format_ts(score.created_at)
    if score.source is not None and (score.source.kind or score.source.id):
        out["source"] = {"kind": score.source.kind, "id": score.source.id}
    return out


def _serialize_score_value(value: ScoreValue) -> dict[str, Any]:
    if value.number is not None:
        return {"number": value.number}
    if value.boolean is not None:
        return {"bool": value.boolean}
    if value.string is not None:
        return {"string": value.string}
    raise ValidationError("agento11y score validation failed: value must set one of number/boolean/string")


def _validate_score(score: ScoreItem) -> None:
    missing = [
        name
        for name, raw in (
            ("score_id", score.score_id),
            ("evaluator_id", score.evaluator_id),
            ("evaluator_version", score.evaluator_version),
            ("score_key", score.score_key),
        )
        if not (raw or "").strip()
    ]
    if missing:
        raise ValidationError(f"agento11y score validation failed: missing required field(s): {', '.join(missing)}")
    # The backend requires a generation_id OR a trial_id. Mirror that rule
    # client-side for a clearer error.
    if not (score.generation_id or "").strip() and not (score.trial_id or "").strip():
        raise ValidationError("agento11y score validation failed: generation_id or trial_id is required")
    # Raises if no value field is set.
    _serialize_score_value(score.value)


# --------------------------------------------------------------------------- #
# Parsing
# --------------------------------------------------------------------------- #


def _parse_experiment(payload: Any) -> Experiment:
    if not isinstance(payload, dict):
        raise ExperimentTransportError("agento11y experiment transport failed: invalid response payload")
    evaluators = [
        ExperimentEvaluator(id=_str(ev.get("id")), selector=_str(ev.get("selector")))
        for ev in payload.get("evaluators", []) or []
        if isinstance(ev, dict)
    ]
    return Experiment(
        run_id=_str(payload.get("experiment_id")) or _str(payload.get("run_id")),
        name=_str(payload.get("name")),
        source=_str(payload.get("source")),
        status=_str(payload.get("status")),
        tenant_id=_str(payload.get("tenant_id")),
        description=_str(payload.get("description")),
        tags=[_str(t) for t in payload.get("tags", []) or []],
        collection_id=_str(payload.get("collection_id")),
        evaluators=evaluators,
        metadata=_dict(payload.get("metadata")),
        score_count=_int(payload.get("score_count")),
        error=_str(payload.get("error")),
        created_by=_str(payload.get("created_by")),
        created_at=_parse_ts(payload.get("created_at")),
        updated_at=_parse_ts(payload.get("updated_at")),
        started_at=_parse_ts(payload.get("started_at")),
        completed_at=_parse_ts(payload.get("completed_at")),
    )


def _parse_experiment_run_response(payload: Any) -> Experiment:
    if isinstance(payload, dict) and isinstance(payload.get("run"), dict):
        return _parse_experiment(payload["run"])
    return _parse_experiment(payload)


def _parse_export_scores_response(payload: Any) -> ExportScoresResponse:
    if not isinstance(payload, dict):
        raise ScoreExportError("agento11y score export transport failed: invalid response payload")
    results: list[ExportScoreResult] = []
    for entry in payload.get("results", []) or []:
        if not isinstance(entry, dict):
            continue
        results.append(
            ExportScoreResult(
                score_id=_str(entry.get("score_id")),
                accepted=bool(entry.get("accepted")),
                status=_str(entry.get("status")),
                error=_str(entry.get("error")),
            )
        )
    return ExportScoresResponse(
        results=results,
        accepted=_int(payload.get("accepted")),
        duplicates=_int(payload.get("duplicates")),
        rejected_count=_int(payload.get("rejected")),
    )


def _parse_report(payload: Any) -> ExperimentReport:
    if not isinstance(payload, dict):
        raise ExperimentTransportError("agento11y experiment report transport failed: invalid response payload")
    # The backend keys the run under `experiment` (older drafts used `run`).
    run = _parse_experiment(payload.get("experiment") or payload.get("run") or {})
    summary_raw = payload.get("summary") or {}

    def _kmap(raw: Any) -> dict[str, float]:
        return {str(k): _float(v) for k, v in raw.items()} if isinstance(raw, dict) else {}

    summary = ExperimentReportSummary(
        test_case_count=_int(summary_raw.get("test_case_count")),
        trial_count=_int(summary_raw.get("trial_count")),
        completed_count=_int(summary_raw.get("completed_count")),
        failed_count=_int(summary_raw.get("failed_count")),
        canceled_count=_int(summary_raw.get("canceled_count")),
        pass_rate=_float(summary_raw.get("pass_rate")),
        pass_at_k=_kmap(summary_raw.get("pass_at_k")),
        pass_power_k=_kmap(summary_raw.get("pass_power_k")),
        final_score_avg=_float(summary_raw.get("final_score_avg")),
        total_cost=_float(summary_raw.get("total_cost")),
        total_tokens=_int(summary_raw.get("total_tokens")),
    )
    rows = payload.get("rows") if isinstance(payload.get("rows"), list) else []
    return ExperimentReport(run=run, summary=summary, rows=list(rows))


# --------------------------------------------------------------------------- #
# HTTP transport with retries
# --------------------------------------------------------------------------- #


def _request_json(
    method: str,
    url: str,
    headers: dict[str, str],
    payload: dict[str, Any] | None,
    retry: RetryPolicy | None,
    transport_error_cls: type[Agento11yError],
    label: str,
) -> Any:
    policy = retry or RetryPolicy()
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    request_headers = {**(headers or {})}
    if data is not None:
        request_headers.setdefault("Content-Type", "application/json")

    attempt = 0
    backoff = max(policy.initial_backoff, 0.0)
    last_detail = ""
    while True:
        http_request = urllib_request.Request(url, data=data, method=method, headers=request_headers)
        try:
            with urllib_request.urlopen(http_request, timeout=policy.timeout) as response:
                status = response.getcode()
                raw = response.read(_MAX_RESPONSE_BYTES + 1)
            return _decode_success(raw, status, transport_error_cls, label)
        except urllib_error.HTTPError as exc:
            body = _read_error_body(exc)
            if exc.code in (400, 422):
                raise ValidationError(f"agento11y {label} validation failed: {body or exc.code}") from exc
            if exc.code == 404:
                raise NotFoundError(f"agento11y {label} not found: {body or exc.code}") from exc
            if exc.code == 409:
                raise ConflictError(f"agento11y {label} conflict: {body or exc.code}") from exc
            last_detail = f"status {exc.code}: {body or 'unexpected status'}"
            if exc.code == 429 or 500 <= exc.code < 600:
                if attempt < policy.max_retries:
                    attempt, backoff = _sleep_backoff(attempt, backoff, policy)
                    continue
            raise transport_error_cls(f"agento11y {label} transport failed: {last_detail}") from exc
        except (urllib_error.URLError, TimeoutError, OSError) as exc:
            last_detail = str(getattr(exc, "reason", exc) or exc)
            if attempt < policy.max_retries:
                attempt, backoff = _sleep_backoff(attempt, backoff, policy)
                continue
            raise transport_error_cls(f"agento11y {label} transport failed: {last_detail}") from exc


def _request_bytes_json(
    method: str,
    url: str,
    headers: dict[str, str],
    payload: bytes,
    retry: RetryPolicy | None,
    transport_error_cls: type[Agento11yError],
    label: str,
) -> Any:
    policy = retry or RetryPolicy()
    request_headers = {**(headers or {})}

    attempt = 0
    backoff = max(policy.initial_backoff, 0.0)
    last_detail = ""
    while True:
        http_request = urllib_request.Request(url, data=payload, method=method, headers=request_headers)
        try:
            with urllib_request.urlopen(http_request, timeout=policy.timeout) as response:
                status = response.getcode()
                raw = response.read(_MAX_RESPONSE_BYTES + 1)
            return _decode_success(raw, status, transport_error_cls, label)
        except urllib_error.HTTPError as exc:
            body = _read_error_body(exc)
            if exc.code in (400, 422):
                raise ValidationError(f"agento11y {label} validation failed: {body or exc.code}") from exc
            if exc.code == 404:
                raise NotFoundError(f"agento11y {label} not found: {body or exc.code}") from exc
            if exc.code == 409:
                raise ConflictError(f"agento11y {label} conflict: {body or exc.code}") from exc
            last_detail = f"status {exc.code}: {body or 'unexpected status'}"
            if exc.code == 429 or 500 <= exc.code < 600:
                if attempt < policy.max_retries:
                    attempt, backoff = _sleep_backoff(attempt, backoff, policy)
                    continue
            raise transport_error_cls(f"agento11y {label} transport failed: {last_detail}") from exc
        except (urllib_error.URLError, TimeoutError, OSError) as exc:
            last_detail = str(getattr(exc, "reason", exc) or exc)
            if attempt < policy.max_retries:
                attempt, backoff = _sleep_backoff(attempt, backoff, policy)
                continue
            raise transport_error_cls(f"agento11y {label} transport failed: {last_detail}") from exc


def _decode_success(raw: bytes, status: int, transport_error_cls: type[Agento11yError], label: str) -> Any:
    if status < 200 or status >= 300:
        decoded = raw.decode("utf-8", errors="replace").strip()
        raise transport_error_cls(
            f"agento11y {label} transport failed: status {status}: {decoded or 'unexpected status'}"
        )
    if len(raw) > _MAX_RESPONSE_BYTES:
        raise transport_error_cls(f"agento11y {label} transport failed: response too large")
    text = raw.decode("utf-8", errors="replace").strip()
    if text == "":
        return {}
    try:
        return json.loads(text)
    except Exception as exc:  # noqa: BLE001
        raise transport_error_cls(f"agento11y {label} transport failed: invalid JSON response: {exc}") from exc


def _sleep_backoff(attempt: int, backoff: float, policy: RetryPolicy) -> tuple[int, float]:
    if backoff > 0:
        time.sleep(min(backoff, policy.max_backoff))
    next_backoff = backoff * 2 if backoff > 0 else policy.initial_backoff
    return attempt + 1, min(next_backoff, policy.max_backoff)


def _read_error_body(exc: urllib_error.HTTPError) -> str:
    try:
        return exc.read().decode("utf-8", errors="replace").strip()
    except Exception:  # noqa: BLE001
        return ""


# --------------------------------------------------------------------------- #
# URL + value helpers
# --------------------------------------------------------------------------- #


def _experiments_url(endpoint: str, insecure: bool, path_prefix: str) -> str:
    prefix = "/" + (path_prefix or _DEFAULT_PATH_PREFIX).strip().strip("/")
    return _base_url(endpoint, insecure) + prefix + _EVAL_EXPERIMENTS_SUFFIX


def _experiment_url(endpoint: str, insecure: bool, run_id: str, path_prefix: str) -> str:
    normalized = _validate_run_id(run_id)
    return f"{_experiments_url(endpoint, insecure, path_prefix)}/{urllib_parse.quote(normalized, safe='')}"


def _validate_run_id(run_id: str) -> str:
    normalized = (run_id or "").strip()
    if normalized == "":
        raise ValidationError("agento11y experiment validation failed: run_id is required")
    return normalized


def _base_url(endpoint: str, insecure: bool) -> str:
    trimmed = (endpoint or "").strip()
    if trimmed == "":
        raise ExperimentTransportError("agento11y experiment transport failed: api endpoint is required")
    if trimmed.startswith("http://") or trimmed.startswith("https://"):
        parsed = urllib_parse.urlparse(trimmed)
        if not parsed.scheme or not parsed.netloc:
            raise ExperimentTransportError("agento11y experiment transport failed: api endpoint host is required")
        return f"{parsed.scheme}://{parsed.netloc}"
    without_scheme = trimmed[7:] if trimmed.startswith("grpc://") else trimmed
    host = without_scheme.split("/", 1)[0].strip()
    if host == "":
        raise ExperimentTransportError("agento11y experiment transport failed: api endpoint host is required")
    scheme = "http" if insecure else "https"
    return f"{scheme}://{host}"


def _enum_value(value: Any) -> str:
    return str(getattr(value, "value", value))


def _normalize_cursor(value: Any) -> str | None:
    if value is None:
        return None
    text = str(value).strip()
    if text == "" or text == "0":
        return None
    return text


def _format_ts(value: datetime) -> str:
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def _parse_ts(value: Any) -> datetime | None:
    if not isinstance(value, str) or value.strip() == "":
        return None
    normalized = value.strip().replace("Z", "+00:00")
    try:
        parsed = datetime.fromisoformat(normalized)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def _str(value: Any) -> str:
    return value if isinstance(value, str) else ""


def _int(value: Any) -> int:
    if isinstance(value, bool):
        return 0
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value)
    return 0


def _float(value: Any) -> float:
    if isinstance(value, bool):
        return 0.0
    if isinstance(value, (int, float)):
        return float(value)
    return 0.0


def _dict(value: Any) -> dict[str, Any]:
    return dict(value) if isinstance(value, dict) else {}

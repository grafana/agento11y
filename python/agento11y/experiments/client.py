"""``Client`` — a thin, ergonomic client for cloud experiment writes.

It speaks the v1 one-token ingest contract over the retrying stdlib
``experiments`` transport, including generation publishing, so benchmark
verifier containers do not need the OpenTelemetry-backed core client.
"""

from __future__ import annotations

import base64
import copy
import os
import urllib.parse
from typing import Any

from .. import _experiments_transport as _transport
from ..config import _warn_legacy_env
from ..errors import ScoreExportError
from ..models import (
    CreateExperimentRequest,
    Experiment,
    ExperimentReport,
    ScoreItem,
    TokenUsage,
)
from ..redaction import redact_secret_text, redact_secret_value
from .types import _first_nonblank

TENANT_HEADER = "X-Scope-OrgID"
INGEST_ACTOR_HEADER = "X-Sigil-Ingest-Actor"


class Client:
    """Connection + auth for experiment writes (single ingest token).

    All writes (run upsert, trial upsert, score export, finalize) share the same
    tenant ingest credential. There is no separate eval control-plane token.
    """

    def __init__(
        self,
        endpoint: str,
        *,
        tenant_id: str = "",
        ingest_token: str = "",
        actor: str = "",
        trusted: bool = True,
        grafana_url: str = "",
        timeout: float = 30.0,
        generation_endpoint: str = "",
        generation_protocol: str = "http",
        insecure: bool | None = None,
        use_experimental_otel: bool | None = None,
        redact_secrets: bool = True,
    ) -> None:
        _warn_legacy_env()
        if not (endpoint or "").strip():
            raise ValueError("Agent Observability endpoint is required (your Grafana Cloud Agent Observability URL)")
        token = (ingest_token or _first_nonblank(os.environ, "AGENTO11Y_AUTH_TOKEN")).strip()
        if not token:
            raise ValueError("ingest_token is required (your Grafana Cloud ingestion API key)")
        self.endpoint = endpoint.rstrip("/")
        self.tenant_id = tenant_id.strip()
        self.ingest_token = token
        # The backend derives this same identity from the run's sdk/python
        # source. Sending it on every lifecycle request also covers routes such
        # as artifact upload that cannot carry a JSON source object.
        self.actor = actor.strip() or _transport.DEFAULT_INGEST_ACTOR
        self.trusted = trusted
        self.grafana_url = (grafana_url or _first_nonblank(os.environ, "AGENTO11Y_GRAFANA_URL")).rstrip("/")
        self.timeout = timeout
        self.generation_endpoint = generation_endpoint or self.endpoint
        self.generation_protocol = generation_protocol
        self._insecure = insecure if insecure is not None else self.endpoint.startswith("http://")
        self._retry = _transport.RetryPolicy(timeout=timeout)
        self._core: Any | None = None
        self.redact_secrets = bool(redact_secrets)
        self.use_experimental_otel = (
            _env_bool("AGENTO11Y_USE_EXPERIMENTAL_OTEL")
            if use_experimental_otel is None
            else bool(use_experimental_otel)
        )

    # --- connection args -------------------------------------------------- #

    def _headers(self) -> dict[str, str]:
        headers: dict[str, str] = {}
        if self.tenant_id:
            headers[TENANT_HEADER] = self.tenant_id
            creds = base64.b64encode(f"{self.tenant_id}:{self.ingest_token}".encode()).decode()
            headers["Authorization"] = f"Basic {creds}"
        else:
            headers["Authorization"] = _format_bearer(self.ingest_token)
        if (self.actor or "").strip():
            headers[INGEST_ACTOR_HEADER] = self.actor
        return headers

    def _args(self) -> dict[str, Any]:
        return {"api_endpoint": self.endpoint, "insecure": self._insecure, "headers": self._headers()}

    # --- experiment lifecycle -------------------------------------------- #

    def upsert_experiment(self, request: CreateExperimentRequest) -> Experiment:
        """Creates or idempotently claims an external run (one ingest token)."""

        return _transport.create_experiment(**self._args(), request=request, retry=self._retry)

    def finalize(
        self,
        experiment_id: str,
        status: str = "completed",
        *,
        score_count: int | None = None,
        error: str = "",
    ) -> Experiment:
        """Finalizes a run as ``completed`` or ``failed``."""

        return _transport.finalize_experiment(
            **self._args(),
            run_id=experiment_id,
            status=status,
            score_count=score_count,
            error=error or None,
            retry=self._retry,
        )

    # --- trials ----------------------------------------------------------- #

    def upsert_trial(
        self,
        experiment_id: str,
        *,
        trial_id: str,
        test_case_id: str,
        attempt: int = 1,
        status: str = "running",
        conversation_id: str = "",
        trace_id: str = "",
        span_id: str = "",
        test_case: dict[str, Any] | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Creates or idempotently upserts a typed trial under a run."""

        request: dict[str, Any] = {
            "trial_id": trial_id,
            "test_case_id": test_case_id,
            "attempt": attempt,
            "status": status,
        }
        if conversation_id:
            request["conversation_id"] = conversation_id
        if trace_id:
            request["trace_id"] = trace_id
        if span_id:
            request["span_id"] = span_id
        if test_case:
            request["test_case"] = dict(test_case)
        if metadata:
            request["metadata"] = dict(metadata)
        return _transport.create_test_case_trial(
            **self._args(), experiment_id=experiment_id, request=request, retry=self._retry
        )

    def update_trial(
        self,
        experiment_id: str,
        trial_id: str,
        *,
        status: str = "",
        error: str = "",
        cost: float | None = None,
        input_tokens: int | None = None,
        output_tokens: int | None = None,
        duration_ms: int | None = None,
        conversation_id: str = "",
        trace_id: str = "",
        span_id: str = "",
    ) -> dict[str, Any]:
        """Patches a typed trial's status / usage rollups."""

        request: dict[str, Any] = {}
        if status:
            request["status"] = status
        if error:
            request["error"] = error
        if cost is not None:
            request["cost"] = float(cost)
        if input_tokens is not None:
            request["input_tokens"] = int(input_tokens)
        if output_tokens is not None:
            request["output_tokens"] = int(output_tokens)
        if duration_ms is not None:
            request["duration_ms"] = int(duration_ms)
        if conversation_id:
            request["conversation_id"] = conversation_id
        if trace_id:
            request["trace_id"] = trace_id
        if span_id:
            request["span_id"] = span_id
        return _transport.update_test_case_trial(
            **self._args(),
            experiment_id=experiment_id,
            trial_id=trial_id,
            request=request,
            retry=self._retry,
        )

    # --- scores ----------------------------------------------------------- #

    def export_scores(self, scores: list[ScoreItem], *, raise_on_reject: bool = True) -> int:
        """Exports scores; returns the count recorded (fresh + idempotent dup)."""

        if not scores:
            return 0
        exported = copy.deepcopy(scores)
        if self.redact_secrets:
            for score in exported:
                if score.value.string is not None:
                    score.value.string = redact_secret_text(score.value.string)
                score.explanation = redact_secret_text(score.explanation)
                score.metadata = redact_secret_value(score.metadata)
        response = _transport.export_scores(**self._args(), scores=exported, retry=self._retry)
        if raise_on_reject and response.rejected:
            details = "; ".join(f"{r.score_id}: {r.error or 'rejected'}" for r in response.rejected)
            raise ScoreExportError(f"agento11y score export rejected {len(response.rejected)} score(s): {details}")
        return response.accepted_count + response.duplicate_count

    # --- generations (stdlib; for minimal/vendored environments) ---------- #

    def export_generation(
        self,
        *,
        generation_id: str,
        conversation_id: str,
        input_text: str = "",
        output_text: str = "",
        model_provider: str = "eval",
        model_name: str = "experiment",
        agent_name: str = "",
        agent_version: str = "",
        operation_name: str = "invoke_agent",
        input_tokens: int | None = None,
        output_tokens: int | None = None,
        usage: TokenUsage | None = None,
        tags: dict[str, str] | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> str:
        """Ingests a single generation over HTTP using only the stdlib.

        This posts generation JSON directly so a minimal vendored environment
        can ingest the attempt's transcript and create an openable conversation.
        """

        generation: dict[str, Any] = {
            "id": generation_id,
            "conversation_id": conversation_id,
            "operation_name": operation_name,
            "mode": "GENERATION_MODE_SYNC",
            "model": {"provider": model_provider or "eval", "name": model_name or "experiment"},
        }
        if agent_name:
            generation["agent_name"] = agent_name
        if agent_version:
            generation["agent_version"] = agent_version
        if input_text:
            generation["input"] = [{"role": "MESSAGE_ROLE_USER", "parts": [{"text": input_text}]}]
        if output_text:
            generation["output"] = [{"role": "MESSAGE_ROLE_ASSISTANT", "parts": [{"text": output_text}]}]
        if tags:
            generation["tags"] = dict(tags)
        if metadata:
            generation["metadata"] = dict(metadata)
        resolved_usage = usage or TokenUsage(input_tokens=int(input_tokens or 0), output_tokens=int(output_tokens or 0))
        normalized_usage = resolved_usage.normalize()
        if normalized_usage.total_tokens:
            generation["usage"] = {
                "input_tokens": normalized_usage.input_tokens,
                "output_tokens": normalized_usage.output_tokens,
                "total_tokens": normalized_usage.total_tokens,
                "cache_read_input_tokens": normalized_usage.cache_read_input_tokens,
                "cache_write_input_tokens": normalized_usage.cache_write_input_tokens,
                "reasoning_tokens": normalized_usage.reasoning_tokens,
            }
        if self.redact_secrets:
            generation = redact_secret_value(generation)
        _transport.export_generation_json(**self._args(), generation=generation, retry=self._retry)
        return generation_id

    # --- artifacts -------------------------------------------------------- #

    def upload_artifact(
        self,
        *,
        parent_id: str,
        name: str,
        kind: str,
        content: bytes,
        mime: str = "",
        parent_kind: str = "test_case_trial",
        experiment_id: str = "",
    ) -> dict[str, Any]:
        """Uploads an artifact blob and attaches it to a parent entity.

        Trial artifacts post raw bytes to the experiment-run ingest route using
        the tenant ingest credential (and the ingest-actor header when set).
        ``kind`` is one of ``image|json|markdown|text|pdf|csv|binary``. Returns
        the created artifact record (including its ``artifact_id``).

        The same ingestion credential is used here. Upload errors are surfaced
        as SDK transport/validation errors so experiment runs fail loudly.
        """

        if parent_kind != "test_case_trial":
            raise ValueError("only test_case_trial artifacts are supported by the experiments ingest client")
        if self.redact_secrets and (kind in {"json", "markdown", "text", "csv"} or mime.startswith("text/")):
            content = redact_secret_text(content.decode("utf-8")).encode("utf-8")
        return _transport.upload_trial_artifact(
            **self._args(),
            experiment_id=experiment_id,
            trial_id=parent_id,
            name=name,
            kind=kind,
            content=content,
            mime=mime,
            retry=self._retry,
        )

    # --- generations (rich; needs the full Client) ------------------------ #

    def record_generation(
        self,
        generation_id: str,
        *,
        conversation_id: str = "",
        input_text: str = "",
        output_text: str = "",
        model_provider: str = "eval",
        model_name: str = "experiment",
        agent_name: str = "",
        agent_version: str = "",
        operation_name: str = "invoke_agent",
        input_tokens: int | None = None,
        output_tokens: int | None = None,
        usage: TokenUsage | None = None,
        tags: dict[str, str] | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> str:
        """Exports a generation through the lightweight experiment transport."""

        return self.export_generation(
            generation_id=generation_id,
            conversation_id=conversation_id,
            input_text=input_text,
            output_text=output_text,
            model_provider=model_provider,
            model_name=model_name,
            agent_name=agent_name,
            agent_version=agent_version,
            operation_name=operation_name,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            usage=usage,
            tags=tags,
            metadata=metadata,
        )

    def _ensure_core(self) -> Any:
        if self._core is None:
            from ..client import Client
            from ..config import ApiConfig, AuthConfig, ClientConfig, GenerationExportConfig

            self._core = Client(
                ClientConfig(
                    api=ApiConfig(endpoint=self.endpoint),
                    generation_export=GenerationExportConfig(
                        protocol=self.generation_protocol,
                        endpoint=self.generation_endpoint,
                        insecure=self._insecure,
                        auth=AuthConfig(
                            mode="basic" if self.tenant_id else "bearer",
                            tenant_id=self.tenant_id,
                            basic_user=self.tenant_id,
                            basic_password=self.ingest_token,
                            bearer_token=self.ingest_token,
                        ),
                    ),
                    ingest_actor=self.actor or None,
                    use_experimental_otel=self.use_experimental_otel,
                )
            )
        return self._core

    @property
    def core(self) -> Any:
        """The underlying core ``Client`` (built on demand; needs the full SDK)."""

        return self._ensure_core()

    def flush_generations(self) -> None:
        """Flushes the underlying generation client, if one was built."""

        if self._core is not None:
            self._core.flush()

    # --- reads ------------------------------------------------------------ #

    def get_report(self, experiment_id: str) -> ExperimentReport:
        """Fetches the aggregated report for a run."""

        return _transport.get_experiment_report(**self._args(), run_id=experiment_id, retry=self._retry)

    def list_scores(
        self, experiment_id: str, *, limit: int = 50, cursor: str | None = None
    ) -> tuple[list[dict[str, Any]], str | None]:
        """Lists stored scores for a run."""

        return _transport.list_experiment_scores(
            **self._args(), run_id=experiment_id, limit=limit, cursor=cursor, retry=self._retry
        )

    # --- links ------------------------------------------------------------ #

    def experiment_url(self, experiment_id: str) -> str:
        """Best-effort deep link to the run in the Agent Observability UI."""

        quoted = urllib.parse.quote(experiment_id, safe="")
        base = self.grafana_url
        if base:
            return f"{base}/a/grafana-sigil-app/experiments/runs/{quoted}"
        return f"{self.endpoint}/a/grafana-sigil-app/experiments/runs/{quoted}"

    def shutdown(self) -> None:
        """Flushes and closes the underlying client if one was built."""

        if self._core is not None:
            self._core.shutdown()

    def __enter__(self) -> Client:
        return self

    def __exit__(self, *exc: Any) -> bool:
        self.shutdown()
        return False


def _format_bearer(token: str) -> str:
    value = token.strip()
    if value.lower().startswith("bearer "):
        value = value[7:].strip()
    return f"Bearer {value}"


def _env_bool(*names: str) -> bool:
    return _first_nonblank(os.environ, *names).lower() in {"1", "true", "yes", "on"}

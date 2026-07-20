"""``Client`` — a thin, ergonomic client for cloud experiment writes.

It speaks the v1 one-token ingest contract over the stdlib ``experiments``
transport (urllib) directly, so it has no OpenTelemetry / heavy ``Client``
dependency and can be vendored into a benchmark verifier container. Generation
recording (``record_generation``) lazily uses the full ``Client`` when
available, since that path needs the generation exporter.
"""

from __future__ import annotations

import base64
import json
import os
import urllib.parse
import urllib.request
from typing import Any

from .. import _experiments_transport as _transport
from ..errors import ScoreExportError
from ..models import (
    CreateExperimentRequest,
    Experiment,
    ExperimentReport,
    ScoreItem,
)
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
    ) -> None:
        if not (endpoint or "").strip():
            raise ValueError("Sigil endpoint is required (your Grafana Cloud Sigil URL)")
        token = (ingest_token or _first_nonblank(os.environ, "AGENTO11Y_AUTH_TOKEN", "SIGIL_AUTH_TOKEN")).strip()
        if not token:
            raise ValueError("ingest_token is required (your Grafana Cloud ingestion API key)")
        self.endpoint = endpoint.rstrip("/")
        self.tenant_id = tenant_id.strip()
        self.ingest_token = token
        self.actor = actor
        self.trusted = trusted
        self.grafana_url = (
            grafana_url or _first_nonblank(os.environ, "AGENTO11Y_GRAFANA_URL", "SIGIL_GRAFANA_URL")
        ).rstrip("/")
        self.timeout = timeout
        self.generation_endpoint = generation_endpoint or self.endpoint
        self.generation_protocol = generation_protocol
        self._insecure = insecure if insecure is not None else self.endpoint.startswith("http://")
        self._retry = _transport.RetryPolicy(timeout=timeout)
        self._core: Any | None = None
        self.use_experimental_otel = (
            _env_bool("AGENTO11Y_USE_EXPERIMENTAL_OTEL", "SIGIL_USE_EXPERIMENTAL_OTEL")
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
        response = _transport.export_scores(**self._args(), scores=scores, retry=self._retry)
        if raise_on_reject and response.rejected:
            details = "; ".join(f"{r.score_id}: {r.error or 'rejected'}" for r in response.rejected)
            raise ScoreExportError(f"sigil score export rejected {len(response.rejected)} score(s): {details}")
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
        tags: dict[str, str] | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> str:
        """Ingests a single generation over HTTP using only the stdlib.

        Unlike :meth:`record_generation` (which uses the full ``Client`` and its
        OpenTelemetry-backed exporter), this posts a minimal generation JSON
        directly, so a minimal vendored environment (e.g. a verifier container)
        can ingest the attempt's transcript and give the trial a real, openable
        conversation.
        """

        generation: dict[str, Any] = {
            "id": generation_id,
            "conversation_id": conversation_id,
            "operation_name": operation_name,
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
        self._post("/api/v1/generations:export", {"generations": [generation]})
        return generation_id

    def _post(self, path: str, body: dict[str, Any]) -> None:
        request = urllib.request.Request(
            f"{self.endpoint}{path}",
            data=json.dumps(body).encode("utf-8"),
            headers={**self._headers(), "Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=self.timeout):
            pass

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
        tags: dict[str, str] | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> str:
        """Exports a generation to anchor a trial's scores (needs the full SDK).

        Lazily builds the core ``Client`` (which owns the generation exporter and
        pulls in OpenTelemetry). In a minimal vendored environment without those
        deps this raises; the live path ingests generations through the agent's
        own instrumentation instead.
        """

        from ..models import (
            Generation,
            GenerationStart,
            ModelRef,
            TokenUsage,
            assistant_text_message,
            user_text_message,
        )

        client = self._ensure_core()
        model = ModelRef(provider=model_provider or "eval", name=model_name or "experiment")
        usage = TokenUsage(input_tokens=int(input_tokens or 0), output_tokens=int(output_tokens or 0))
        with client.start_generation(
            GenerationStart(
                id=generation_id,
                conversation_id=conversation_id,
                model=model,
                agent_name=agent_name,
                agent_version=agent_version,
                operation_name=operation_name,
                tags=dict(tags or {}),
                metadata=dict(metadata or {}),
            )
        ) as recorder:
            recorder.set_result(
                Generation(
                    id=generation_id,
                    conversation_id=conversation_id,
                    model=model,
                    agent_name=agent_name,
                    agent_version=agent_version,
                    input=[user_text_message(input_text)] if input_text else [],
                    output=[assistant_text_message(output_text)] if output_text else [],
                    usage=usage,
                )
            )
        client.flush()
        return generation_id

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
        """Best-effort deep link to the run in the Sigil UI."""

        quoted = urllib.parse.quote(experiment_id, safe="")
        base = self.grafana_url
        if base:
            return f"{base}/a/grafana-sigil-app/offline-experiments/experiments/{quoted}"
        return f"{self.endpoint}/a/grafana-sigil-app/offline-experiments/experiments/{quoted}"

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

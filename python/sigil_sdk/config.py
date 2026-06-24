"""Runtime configuration for the Sigil Python SDK."""

from __future__ import annotations

import base64
import dataclasses
import logging
import os
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any

from opentelemetry.metrics import Meter
from opentelemetry.trace import Tracer

from .exporters.base import GenerationExporter
from .models import ContentCaptureMode, Generation, utc_now

TENANT_HEADER = "X-Scope-OrgID"
AUTHORIZATION_HEADER = "Authorization"
GenerationSanitizer = Callable[[Generation], Generation]

# Schema-level defaults applied when neither user config nor env provides a value.
_DEFAULT_ENDPOINT = "localhost:4317"
_DEFAULT_PROTOCOL = "grpc"
_DEFAULT_INSECURE = False
_DEFAULT_AUTH_MODE = "none"
_VALID_AUTH_MODES = ("none", "tenant", "bearer", "basic")
_VALID_CONTENT_CAPTURE = ("full", "no_tool_content", "metadata_only", "full_with_metadata_spans")


@dataclass(slots=True)
class AuthConfig:
    """Per-export auth configuration.

    ``mode`` is ``None`` when unset so the resolver can distinguish "user did not
    set this" from "user explicitly chose 'none'".
    """

    mode: str | None = None
    tenant_id: str = ""
    bearer_token: str = ""
    basic_user: str = ""
    basic_password: str = ""


@dataclass(slots=True)
class GenerationExportConfig:
    """Generation ingest export configuration.

    Transport fields default to ``None`` so the resolver can layer in env vars
    (``SIGIL_ENDPOINT`` / ``SIGIL_PROTOCOL`` / ``SIGIL_INSECURE`` / ``SIGIL_HEADERS``)
    before falling back to schema defaults.
    """

    protocol: str | None = None
    endpoint: str | None = None
    headers: dict[str, str] | None = None
    auth: AuthConfig = field(default_factory=AuthConfig)
    insecure: bool | None = None
    batch_size: int = 100
    flush_interval: timedelta = timedelta(seconds=1)
    queue_size: int = 2000
    max_retries: int = 5
    initial_backoff: timedelta = timedelta(milliseconds=100)
    max_backoff: timedelta = timedelta(seconds=5)
    payload_max_bytes: int = 4 << 20


@dataclass(slots=True)
class ApiConfig:
    """Sigil HTTP API settings used by non-ingest helper endpoints."""

    endpoint: str = "http://localhost:8080"


@dataclass(slots=True)
class EmbeddingCaptureConfig:
    """Embedding input capture settings for span attributes."""

    capture_input: bool = False
    max_input_items: int = 20
    max_text_length: int = 1024


@dataclass(slots=True)
class HooksConfig:
    """Synchronous hook evaluation configuration.

    Hooks are disabled by default; callers must opt in by setting
    ``enabled=True``. ``fail_open`` defaults to ``True`` so transport failures
    never block the LLM call unless the caller explicitly chooses fail-closed.
    """

    enabled: bool = False
    phases: list[str] = field(default_factory=lambda: ["preflight"])
    timeout_seconds: float = 15.0
    fail_open: bool = True


@dataclass(slots=True)
class ClientConfig:
    """Top-level SDK runtime configuration.

    Fields default to ``None`` where the resolver can layer in canonical
    ``SIGIL_*`` environment variables. After ``resolve_config`` runs, all fields
    are populated with concrete values.
    """

    generation_export: GenerationExportConfig = field(default_factory=GenerationExportConfig)
    api: ApiConfig = field(default_factory=ApiConfig)
    embedding_capture: EmbeddingCaptureConfig = field(default_factory=EmbeddingCaptureConfig)
    hooks: HooksConfig = field(default_factory=HooksConfig)
    content_capture: ContentCaptureMode | None = None
    content_capture_resolver: Callable[[dict[str, Any]], ContentCaptureMode] | None = None
    generation_sanitizer: GenerationSanitizer | None = None
    tracer: Tracer | None = None
    meter: Meter | None = None
    logger: logging.Logger | None = None
    now: Callable[[], datetime] | None = None
    sleep: Callable[[float], None] | None = None
    generation_exporter: GenerationExporter | None = None

    # Default identity / tags merged into each GenerationStart when the per-call
    # field is unset. Read from SIGIL_AGENT_NAME / SIGIL_AGENT_VERSION /
    # SIGIL_USER_ID / SIGIL_TAGS by ``resolve_config``.
    agent_name: str | None = None
    agent_version: str | None = None
    user_id: str | None = None
    tags: dict[str, str] | None = None

    ingest_actor: str | None = None

    # When True (and ``logger`` is not provided) the SDK constructs a default
    # logger at debug level. Read from ``SIGIL_DEBUG`` by ``resolve_config``.
    debug: bool | None = None

    # Convenience aliases for simpler caller config wiring.
    generation_export_endpoint: str = ""

    @classmethod
    def from_env(cls, env: dict[str, str] | None = None) -> ClientConfig:
        """Returns a fully-resolved config built from canonical SIGIL_* env vars.

        This is a debugging / advanced helper. The recommended path is to call
        ``Client()`` directly which performs the same resolution internally.
        """

        return resolve_config(None, env=env)


def default_config() -> ClientConfig:
    """Returns a fully-resolved baseline configuration with no env applied.

    All transport/auth/content fields are populated with concrete schema
    defaults. Use ``Client()`` for normal app code; this helper exists for
    tests and tools that need to inspect or copy a baseline config.
    """

    return resolve_config(ClientConfig(), env={})


def _env(env: dict[str, str] | None, key: str) -> str | None:
    """Returns the trimmed env var value, or ``None`` when unset/empty."""

    src = env if env is not None else os.environ
    raw = src.get(key)
    if raw is None:
        return None
    val = raw.strip()
    return val or None


def _parse_bool(raw: str) -> bool:
    return raw.strip().lower() in ("1", "true", "yes", "on")


def _parse_csv_kv(raw: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for part in raw.split(","):
        part = part.strip()
        if not part:
            continue
        if "=" not in part:
            continue
        k, v = part.split("=", 1)
        k = k.strip()
        v = v.strip()
        if k:
            out[k] = v
    return out


def resolve_config(
    config: ClientConfig | None,
    *,
    env: dict[str, str] | None = None,
) -> ClientConfig:
    """Resolves caller config against canonical ``SIGIL_*`` env vars and defaults.

    Resolution order: explicit user-provided fields > ``SIGIL_*`` env vars > SDK
    config struct defaults.
    """

    # Clone so resolve_config never mutates the caller's config. Some fields
    # (logger, tracer, exporter) hold non-picklable resources, so we shallow-
    # clone the dataclass tree rather than deepcopying.
    out = _clone_config(config) if config is not None else ClientConfig()

    log = logging.getLogger("sigil_sdk")

    # Transport
    env_endpoint = _env(env, "SIGIL_ENDPOINT")
    if out.generation_export.endpoint is None:
        out.generation_export.endpoint = env_endpoint or _DEFAULT_ENDPOINT
    if out.generation_export.protocol is None:
        out.generation_export.protocol = _env(env, "SIGIL_PROTOCOL") or _DEFAULT_PROTOCOL
    if out.generation_export.insecure is None:
        ev = _env(env, "SIGIL_INSECURE")
        out.generation_export.insecure = _parse_bool(ev) if ev is not None else _DEFAULT_INSECURE
    if out.generation_export.headers is None:
        ev = _env(env, "SIGIL_HEADERS")
        out.generation_export.headers = _parse_csv_kv(ev) if ev is not None else {}

    # Auth. Invalid mode strings are warned and skipped so other valid env
    # vars still apply.
    auth = out.generation_export.auth
    if auth.mode is None:
        env_mode = _env(env, "SIGIL_AUTH_MODE")
        if env_mode is None:
            auth.mode = _DEFAULT_AUTH_MODE
        else:
            normalized = env_mode.lower()
            if normalized in _VALID_AUTH_MODES:
                auth.mode = normalized
            else:
                log.warning("sigil: ignoring invalid SIGIL_AUTH_MODE %r", env_mode)
                auth.mode = _DEFAULT_AUTH_MODE
    if not auth.tenant_id:
        auth.tenant_id = _env(env, "SIGIL_AUTH_TENANT_ID") or ""
    # Set both fields; _resolve_export_headers uses only the one matching the
    # final mode. Lets env's token fill a caller-supplied mode without
    # SIGIL_AUTH_MODE.
    env_token = _env(env, "SIGIL_AUTH_TOKEN")
    if env_token:
        if not auth.bearer_token:
            auth.bearer_token = env_token
        if not auth.basic_password:
            auth.basic_password = env_token
    if auth.mode == "basic" and not auth.basic_user and auth.tenant_id:
        auth.basic_user = auth.tenant_id

    # Agent / user / tags defaults
    if out.agent_name is None:
        out.agent_name = _env(env, "SIGIL_AGENT_NAME") or ""
    if out.agent_version is None:
        out.agent_version = _env(env, "SIGIL_AGENT_VERSION") or ""
    if out.user_id is None:
        out.user_id = _env(env, "SIGIL_USER_ID") or ""
    if out.ingest_actor is None:
        out.ingest_actor = _env(env, "SIGIL_INGEST_ACTOR") or ""
    # Merge env-derived tags as a base layer; caller tags win on key collision.
    # Matches Go and JS SDK behavior.
    ev = _env(env, "SIGIL_TAGS")
    env_tags = _parse_csv_kv(ev) if ev is not None else {}
    caller_tags = out.tags or {}
    out.tags = {**env_tags, **caller_tags}

    # Content capture.
    if out.content_capture is None:
        ev = _env(env, "SIGIL_CONTENT_CAPTURE_MODE")
        if ev is None:
            out.content_capture = ContentCaptureMode.DEFAULT
        else:
            normalized = ev.strip().lower()
            if normalized in _VALID_CONTENT_CAPTURE:
                out.content_capture = _content_capture_from_str(normalized)
            else:
                log.warning("sigil: ignoring invalid SIGIL_CONTENT_CAPTURE_MODE %r", ev)
                out.content_capture = ContentCaptureMode.DEFAULT

    # Debug
    if out.debug is None:
        ev = _env(env, "SIGIL_DEBUG")
        out.debug = _parse_bool(ev) if ev is not None else False

    if out.generation_export_endpoint:
        out.generation_export.endpoint = out.generation_export_endpoint
    if out.api.endpoint.strip() == "":
        out.api.endpoint = env_endpoint or "http://localhost:8080"
    elif env_endpoint and out.api.endpoint == ApiConfig().endpoint:
        out.api.endpoint = env_endpoint

    out.generation_export.headers = _resolve_export_headers(
        out.generation_export.headers,
        out.generation_export.auth,
        "generation export",
    )
    # Defensive copies so later mutations to the caller's dicts don't reach the
    # client. _resolve_export_headers already returns a fresh dict, but env-derived
    # tags shares no aliasing while user-supplied dicts do — copy anyway
    # to keep behaviour uniform.
    if out.tags is not None:
        out.tags = dict(out.tags)

    if out.logger is None:
        # Don't mutate the global logger's level based on cfg.debug — getLogger
        # returns a process-wide singleton, so setLevel would leak into every
        # subsequent client. Applications own their logging configuration; the
        # debug flag is documented as a downstream signal only.
        out.logger = logging.getLogger("sigil_sdk")
    if out.now is None:
        out.now = utc_now
    if out.sleep is None:
        out.sleep = time.sleep

    if out.generation_export.batch_size <= 0:
        out.generation_export.batch_size = 1
    if out.generation_export.queue_size <= 0:
        out.generation_export.queue_size = 1
    if out.generation_export.flush_interval.total_seconds() <= 0:
        out.generation_export.flush_interval = timedelta(milliseconds=1)
    if out.generation_export.max_retries < 0:
        out.generation_export.max_retries = 0
    if out.generation_export.initial_backoff.total_seconds() <= 0:
        out.generation_export.initial_backoff = timedelta(milliseconds=100)
    if out.generation_export.max_backoff.total_seconds() <= 0:
        out.generation_export.max_backoff = timedelta(milliseconds=100)

    if out.embedding_capture.max_input_items <= 0:
        out.embedding_capture.max_input_items = 20
    if out.embedding_capture.max_text_length <= 0:
        out.embedding_capture.max_text_length = 1024

    if out.hooks.timeout_seconds <= 0:
        out.hooks.timeout_seconds = 15.0
    if not out.hooks.phases:
        out.hooks.phases = ["preflight"]

    return out


def _clone_config(cfg: ClientConfig) -> ClientConfig:
    """Returns a shallow-cloned ClientConfig with fresh nested dataclasses.

    Logger/tracer/exporter fields are shared by reference (not safe to deepcopy).
    Mutable containers get fresh copies so resolve_config can populate them
    without aliasing the caller's input.
    """

    out = dataclasses.replace(cfg)
    out.generation_export = dataclasses.replace(cfg.generation_export)
    out.generation_export.auth = dataclasses.replace(cfg.generation_export.auth)
    if cfg.generation_export.headers is not None:
        out.generation_export.headers = dict(cfg.generation_export.headers)
    out.api = dataclasses.replace(cfg.api)
    out.embedding_capture = dataclasses.replace(cfg.embedding_capture)
    out.hooks = dataclasses.replace(cfg.hooks, phases=list(cfg.hooks.phases))
    if cfg.tags is not None:
        out.tags = dict(cfg.tags)
    return out


def _content_capture_from_str(value: str) -> ContentCaptureMode:
    if value == "full":
        return ContentCaptureMode.FULL
    if value == "no_tool_content":
        return ContentCaptureMode.NO_TOOL_CONTENT
    if value == "metadata_only":
        return ContentCaptureMode.METADATA_ONLY
    if value == "full_with_metadata_spans":
        return ContentCaptureMode.FULL_WITH_METADATA_SPANS
    return ContentCaptureMode.DEFAULT


def _resolve_export_headers(headers: dict[str, str], auth: AuthConfig, label: str) -> dict[str, str]:
    """Builds the auth headers for the given mode.

    Mode-irrelevant fields (e.g. tenant_id when mode=bearer) are silently ignored.
    """

    mode = (auth.mode or "none").strip().lower()
    tenant_id = auth.tenant_id.strip()
    bearer_token = auth.bearer_token.strip()
    out = dict(headers)

    if mode == "none":
        return out
    if mode == "tenant":
        if not tenant_id:
            raise ValueError(f"{label} auth mode 'tenant' requires tenant_id")
        if not _has_header(out, TENANT_HEADER):
            out[TENANT_HEADER] = tenant_id
        return out
    if mode == "bearer":
        if not bearer_token:
            raise ValueError(f"{label} auth mode 'bearer' requires bearer_token")
        if not _has_header(out, AUTHORIZATION_HEADER):
            out[AUTHORIZATION_HEADER] = _format_bearer_token(bearer_token)
        return out
    if mode == "basic":
        password = auth.basic_password.strip()
        if not password:
            raise ValueError(f"{label} auth mode 'basic' requires basic_password")
        user = auth.basic_user.strip()
        if not user:
            user = tenant_id
        if not user:
            raise ValueError(f"{label} auth mode 'basic' requires basic_user or tenant_id")
        if not _has_header(out, AUTHORIZATION_HEADER):
            creds = base64.b64encode(f"{user}:{password}".encode()).decode()
            out[AUTHORIZATION_HEADER] = f"Basic {creds}"
        if tenant_id and not _has_header(out, TENANT_HEADER):
            out[TENANT_HEADER] = tenant_id
        return out

    raise ValueError(f"unsupported {label} auth mode {auth.mode!r}")


def _has_header(headers: dict[str, str], key: str) -> bool:
    target = key.lower()
    return any(existing.lower() == target for existing in headers.keys())


def _format_bearer_token(token: str) -> str:
    value = token.strip()
    if value.lower().startswith("bearer "):
        value = value[7:].strip()
    return f"Bearer {value}"

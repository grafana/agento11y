"""Plugin-specific configuration for grafana-agento11y-hermes.

Transport, auth, agent identity, debug, and content-capture-mode resolution
are owned by the SDK's ``Client()`` constructor. See the canonical
``AGENTO11Y_*`` schema (``AGENTO11Y_ENDPOINT``, ``AGENTO11Y_PROTOCOL``,
``AGENTO11Y_AUTH_*``, ``AGENTO11Y_AGENT_NAME``, ``AGENTO11Y_DEBUG``,
``AGENTO11Y_CONTENT_CAPTURE_MODE``).

OTel exporter and resource resolution follow the standard OpenTelemetry env
schema (``OTEL_EXPORTER_OTLP_ENDPOINT``, ``OTEL_EXPORTER_OTLP_HEADERS``,
``OTEL_SERVICE_NAME``, ``OTEL_RESOURCE_ATTRIBUTES``); the OTLP HTTP exporters
read these themselves. The one exception is
``AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT``, the branded alias the sibling
plugins accept, which this module resolves into ``otel_endpoint_override`` for
``_otel.py`` to pass on.

This module resolves plugin-specific knobs under the ``AGENTO11Y_HERMES_*``
prefix (matching the ``AGENTO11Y_PI_*`` / ``AGENTO11Y_COPILOT_*`` convention
used by sibling plugins) and tracks two presence flags driving channel
decisions in ``_client.py`` and ``_otel.py``.

As a convenience it also derives OTLP auth headers from the generations
basic-auth pair (``AGENTO11Y_AUTH_TENANT_ID`` + ``AGENTO11Y_AUTH_TOKEN``).
``_otel.py`` applies these only when the user has not set
``OTEL_EXPORTER_OTLP_HEADERS`` (nor the per-signal overrides). Auth is all they
cover; the endpoint comes from the two endpoint vars above.
"""

from __future__ import annotations

import base64
import logging
import os
from dataclasses import dataclass, field

logger = logging.getLogger(__name__)


@dataclass(slots=True)
class PluginConfig:
    sample_rate: float = 1.0
    max_chars: int = 12000
    otel_auto: bool = True
    error_flush_timeout: float = 2.0
    generations_configured: bool = False
    otel_configured: bool = False
    otel_endpoint_override: str = ""
    otel_auth_headers: dict[str, str] = field(default_factory=dict)
    export_headers: dict[str, str] = field(default_factory=dict)


def _env(name: str) -> str:
    return os.environ.get(name, "").strip()


def _env_bool(name: str, default: bool) -> bool:
    raw = os.environ.get(name, "").strip().lower()
    if not raw:
        return default
    return raw in {"1", "true", "yes", "on"}


def _env_float(name: str, default: float) -> float:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        return float(raw)
    except ValueError:
        logger.warning("grafana-agento11y-hermes: invalid %s=%r, using default %s", name, raw, default)
        return default


def _env_int(name: str, default: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError:
        logger.warning("grafana-agento11y-hermes: invalid %s=%r, using default %s", name, raw, default)
        return default


def _generations_configured() -> bool:
    if _env("AGENTO11Y_AUTH_TOKEN"):
        return True
    mode = _env("AGENTO11Y_AUTH_MODE").lower()
    return bool(mode) and mode != "none"


def _otel_endpoint_override() -> str:
    """The branded OTLP endpoint, but only when the standard env is unset.

    ``AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT`` is the branded alias the sibling
    plugins accept, so setups carrying only that name reach us too. The OTLP
    exporters know the standard name only, so such an install would otherwise
    run with the OTel channel silently off.

    Returned separately from ``otel_configured`` because ``_otel.py`` has to
    pass this value to the exporters explicitly. Empty when the standard env is
    set, so the exporters keep reading it themselves.
    """
    if _env("OTEL_EXPORTER_OTLP_ENDPOINT"):
        return ""
    return _env("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT")


def _otel_configured() -> bool:
    return bool(_env("OTEL_EXPORTER_OTLP_ENDPOINT") or _env("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT"))


def _parse_kv_csv(raw: str) -> dict[str, str]:
    """Parse ``key=value,key=value`` like the SDK's own header parser."""
    out: dict[str, str] = {}
    for part in raw.split(","):
        part = part.strip()
        if not part or "=" not in part:
            continue
        key, value = part.split("=", 1)
        key = key.strip()
        if key:
            out[key] = value.strip()
    return out


def _export_headers() -> dict[str, str]:
    """Extra generation-export headers from ``AGENTO11Y_HEADERS``.

    The SDK reads ``AGENTO11Y_HEADERS`` only when no headers are set on the
    config. Since the plugin sets ``GenerationExportConfig.headers`` explicitly
    to inject its User-Agent (see ``_client``), that lookup is suppressed, so we
    mirror it here and merge the result back in.
    """
    return _parse_kv_csv(_env("AGENTO11Y_HEADERS"))


def _otel_auth_headers() -> dict[str, str]:
    """Basic-auth headers derived from the generations credentials, for OTLP.

    Mirrors the SDK's ``basic`` mode: ``Authorization: Basic base64(tenant:token)``
    plus ``X-Scope-OrgID: tenant``. ``_otel.py`` uses these only when the user has
    not set ``OTEL_EXPORTER_OTLP_HEADERS`` (nor the per-signal overrides).

    Returns an empty dict when either value is missing, or when
    ``AGENTO11Y_AUTH_MODE`` is explicitly ``bearer``. The token is then a bearer
    token, not a basic password, so deriving basic auth from it would be wrong.
    """
    if _env("AGENTO11Y_AUTH_MODE").lower() == "bearer":
        return {}
    tenant = _env("AGENTO11Y_AUTH_TENANT_ID")
    token = _env("AGENTO11Y_AUTH_TOKEN")
    if not (tenant and token):
        return {}
    creds = base64.b64encode(f"{tenant}:{token}".encode()).decode()
    return {"Authorization": f"Basic {creds}", "X-Scope-OrgID": tenant}


def load() -> PluginConfig:
    """Resolve plugin-specific env vars to a config.

    Always returns a config. Channel decisions are driven by
    ``generations_configured`` / ``otel_configured`` rather than ``None``.
    """
    return PluginConfig(
        sample_rate=_env_float("AGENTO11Y_HERMES_SAMPLE_RATE", 1.0),
        max_chars=_env_int("AGENTO11Y_HERMES_MAX_CHARS", 12000),
        otel_auto=_env_bool("AGENTO11Y_HERMES_OTEL_AUTO", True),
        error_flush_timeout=_env_float("AGENTO11Y_HERMES_ERROR_FLUSH_TIMEOUT", 2.0),
        generations_configured=_generations_configured(),
        otel_configured=_otel_configured(),
        otel_endpoint_override=_otel_endpoint_override(),
        otel_auth_headers=_otel_auth_headers(),
        export_headers=_export_headers(),
    )

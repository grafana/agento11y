"""Lazy SDK client construction.

The client is built on first hook invocation. If neither the generations nor
the OTel channel is configured, the plugin is fully no-op. If construction
fails, the failure is cached and handlers never retry.

Transport and auth use SDK resolution. Capture defaults to metadata_only, like
sibling coding-agent plugins. Content capture requires an explicit mode.
"""

from __future__ import annotations

import logging
import os
import threading
from typing import Any

from . import _config, _otel, _tags

logger = logging.getLogger(__name__)

_INIT_FAILED = object()
_CLIENT: Any = None
_CONFIG: _config.PluginConfig | None = None
_LOCK = threading.Lock()


def _generation_headers(cfg: _config.PluginConfig) -> dict[str, str]:
    """Headers for the generation export: user ``AGENTO11Y_HEADERS`` plus our User-Agent.

    Setting headers explicitly suppresses the SDK's own ``AGENTO11Y_HEADERS``
    lookup, so we merge that env-derived dict back in. A user-supplied
    ``User-Agent`` (via ``AGENTO11Y_HEADERS``) wins over the plugin default. Auth
    headers are still layered on top by the SDK's resolver.
    """
    from ._version import plugin_user_agent

    headers = dict(cfg.export_headers)
    if not any(key.lower() == "user-agent" for key in headers):
        headers["User-Agent"] = plugin_user_agent()
    return headers


def _to_client_config(cfg: _config.PluginConfig):
    """Keep SDK transport resolution and apply plugin privacy defaults."""
    from agento11y import ClientConfig, ContentCaptureMode, GenerationExportConfig
    from agento11y.redaction import SecretRedactionOptions, create_secret_redaction_sanitizer

    raw_mode = (
        os.environ.get("AGENTO11Y_CONTENT_CAPTURE_MODE", "").strip()
        or os.environ.get("SIGIL_CONTENT_CAPTURE_MODE", "").strip()
    ).lower()
    try:
        mode = ContentCaptureMode(raw_mode)
    except ValueError:
        mode = ContentCaptureMode.METADATA_ONLY
    if mode == ContentCaptureMode.DEFAULT:
        mode = ContentCaptureMode.METADATA_ONLY
    redact_inputs = (
        os.environ.get("AGENTO11Y_REDACT_INPUT_MESSAGES", "").strip()
        or os.environ.get("SIGIL_REDACT_INPUT_MESSAGES", "").strip()
    ).lower() not in {"0", "false", "no", "off"}
    overrides: dict[str, Any] = {
        "tags": _tags.client_tags(),
        "content_capture": mode,
        "generation_sanitizer": create_secret_redaction_sanitizer(
            SecretRedactionOptions(redact_input_messages=redact_inputs)
        ),
    }

    if cfg.generations_configured:
        return ClientConfig(
            generation_export=GenerationExportConfig(headers=_generation_headers(cfg)),
            **overrides,
        )

    return ClientConfig(
        generation_export=GenerationExportConfig(protocol="none"),
        **overrides,
    )


def _get_client(create_if_missing: bool = True) -> Any:
    """Return the cached client or ``None`` if init has failed or cannot run."""
    global _CLIENT, _CONFIG

    if _CLIENT is _INIT_FAILED:
        return None
    if _CLIENT is not None:
        return _CLIENT
    if not create_if_missing:
        return None

    with _LOCK:
        if _CLIENT is _INIT_FAILED:
            return None
        if _CLIENT is not None:
            return _CLIENT

        cfg = _config.load()
        if not (cfg.generations_configured or cfg.otel_configured):
            logger.warning(
                "grafana-agento11y-hermes: no channel configured. Set AGENTO11Y_AUTH_TOKEN "
                "(with AGENTO11Y_ENDPOINT/AGENTO11Y_PROTOCOL/AGENTO11Y_AUTH_*) for generations, "
                "or OTEL_EXPORTER_OTLP_ENDPOINT for traces+metrics. Telemetry disabled."
            )
            _CLIENT = _INIT_FAILED
            return None

        # OTel setup is independent of the SDK client. Fine if it returns False,
        # the generations channel can still work.
        _otel.setup_if_needed(cfg)

        try:
            from agento11y import Client

            override = _to_client_config(cfg)
            _CLIENT = Client() if override is None else Client(override)
            _CONFIG = cfg
            logger.info(
                "grafana-agento11y-hermes: client initialized (generations=%s, otel=%s)",
                "configured" if cfg.generations_configured else "unconfigured",
                "configured" if cfg.otel_configured else "unconfigured",
            )
            return _CLIENT
        except Exception as exc:
            logger.warning("grafana-agento11y-hermes: failed to initialize client: %s", exc)
            _CLIENT = _INIT_FAILED
            return None


def _get_plugin_config() -> _config.PluginConfig | None:
    """Return the resolved plugin config, or ``None`` if the client never initialized."""
    return _CONFIG


def _flush_otel(timeout_millis: int | None = None) -> None:
    """Force-flush OTel providers we installed. No-op for host-owned providers."""
    _otel.force_flush(timeout_millis)


def _flush_channels(otel_timeout_millis: int | None = None) -> None:
    """Drain both channels. ``Client.flush()`` covers generations only."""
    client = _get_client(create_if_missing=False)
    if client is not None:
        try:
            client.flush()
        except Exception as exc:
            logger.warning("grafana-agento11y-hermes: client.flush failed: %s", exc)
    _flush_otel(otel_timeout_millis)


def flush_bounded(timeout: float) -> bool:
    """Flush both channels, giving up after ``timeout`` seconds.

    For the paths that end the process without a session-end hook. Hermes
    one-shot mode fires no session hook when a turn dies on a provider error,
    and then exits through ``hermes_cli/main.py`` ``_exit_after_oneshot``, which
    calls ``os._exit`` and so skips the SDK's own atexit flush. Without a flush
    here the failed generation is recorded and then dropped.

    The flush runs on a daemon thread and the wait is bounded, because a
    blocking flush against an unreachable endpoint would otherwise stall the
    hermes loop, which the fail-open invariant forbids. Losing the record on
    timeout is the same outcome as not flushing at all.

    Returns True when the flush finished inside the timeout.
    """
    if timeout <= 0:
        return False

    done = threading.Event()

    def run() -> None:
        try:
            _flush_channels(otel_timeout_millis=int(timeout * 1000))
        finally:
            done.set()

    threading.Thread(target=run, name="agento11y-hermes-flush", daemon=True).start()
    if done.wait(timeout):
        return True
    logger.debug("grafana-agento11y-hermes: flush did not finish within %ss", timeout)
    return False


def _reset_for_tests() -> None:
    global _CLIENT, _CONFIG
    with _LOCK:
        _CLIENT = None
        _CONFIG = None
    _otel._reset_for_tests()

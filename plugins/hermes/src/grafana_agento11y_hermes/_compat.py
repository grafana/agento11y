"""Promote supported legacy settings before plugin and SDK configuration is read."""

from __future__ import annotations

import logging
import os
from collections.abc import MutableMapping

logger = logging.getLogger(__name__)

# Keep plugin aliases local: SDK-private rename tables are not an API.
_RENAMES = {
    f"SIGIL_{suffix}": f"AGENTO11Y_{suffix}"
    for suffix in (
        "AGENT_NAME",
        "AGENT_VERSION",
        "ATTEMPT",
        "AUTH_MODE",
        "AUTH_TENANT_ID",
        "AUTH_TOKEN",
        "CONTENT_CAPTURE_MODE",
        "DEBUG",
        "ENDPOINT",
        "EXPERIMENT_ID",
        "GRAFANA_URL",
        "HEADERS",
        "INGEST_ACTOR",
        "INSECURE",
        "PROTOCOL",
        "REDACT_INPUT_MESSAGES",
        "SERVICE_ACCOUNT_TOKEN",
        "SUITE_ID",
        "SUITE_VERSION",
        "TAGS",
        "TEST_CASE_ID",
        "TRAJECTORY_ID",
        "USE_EXPERIMENTAL_OTEL",
        "USER_ID",
        "OTEL_EXPORTER_OTLP_ENDPOINT",
        "HERMES_AGENT_VERSION",
        "HERMES_MAX_CHARS",
        "HERMES_OTEL_AUTO",
        "HERMES_SAMPLE_RATE",
        "HERMES_ERROR_FLUSH_TIMEOUT",
    )
}
_RENAMES.update(
    {
        "SIGIL_API_ENDPOINT": "AGENTO11Y_ENDPOINT",
        "SIGIL_TENANT_ID": "AGENTO11Y_AUTH_TENANT_ID",
    }
)

_applied = False


def renames() -> dict[str, str]:
    """Return a copy so callers cannot change alias precedence."""
    return dict(_RENAMES)


def apply_legacy_env(env: MutableMapping[str, str] | None = None) -> list[str]:
    """Promote any set legacy var to its ``AGENTO11Y_*`` name.

    Returns the old names that were promoted, for tests. Runs its body once per
    process; later calls return an empty list. Pass ``env`` to act on a dict
    instead of ``os.environ``, which also bypasses the once-only guard.
    """
    global _applied

    target: MutableMapping[str, str]
    if env is None:
        if _applied:
            return []
        _applied = True
        target = os.environ
    else:
        target = env

    promoted = []
    for old, new in renames().items():
        value = target.get(old)
        if value is None:
            continue
        if (target.get(new) or "").strip():
            logger.warning(
                "grafana-agento11y-hermes: %s and %s are both set, using %s",
                old,
                new,
                new,
            )
            continue
        target[new] = value
        promoted.append(old)

    if promoted:
        logger.warning(
            "grafana-agento11y-hermes: applied %d renamed env %s for now. Rename %s.",
            len(promoted),
            "var" if len(promoted) == 1 else "vars",
            ", ".join(f"{old} to {new}" for old, new in sorted((o, renames()[o]) for o in promoted)),
        )
    return promoted


def _reset_for_tests() -> None:
    global _applied
    _applied = False

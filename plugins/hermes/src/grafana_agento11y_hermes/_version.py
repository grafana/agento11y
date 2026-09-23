"""Plugin identity for the generation-export User-Agent.

The sibling Agent Observability plugins (claude-code, codex, copilot, cursor,
pi) tag their generation exports with a most-specific-first User-Agent so the
backend can attribute ingest traffic to the integration. We match that
convention:

    agento11y-plugin-hermes/<plugin-version> agento11y-sdk-python/<sdk-version>
"""

from __future__ import annotations

from importlib.metadata import PackageNotFoundError, version

_PLUGIN_PRODUCT = "agento11y-plugin-hermes"
_SDK_PRODUCT = "agento11y-sdk-python"


def _plugin_version() -> str:
    try:
        return version("grafana-agento11y-hermes")
    except PackageNotFoundError:
        return "dev"


def _sdk_user_agent() -> str:
    """SDK product token, e.g. ``agento11y-sdk-python/0.14.0``.

    The SDK exposes this directly; the metadata fallback covers a build that
    does not, using the same format the SDK uses.
    """
    try:
        from agento11y.version import user_agent

        return user_agent()
    except Exception:
        try:
            return f"{_SDK_PRODUCT}/{version('agento11y')}"
        except PackageNotFoundError:
            return f"{_SDK_PRODUCT}/unknown"


def plugin_user_agent() -> str:
    return f"{_PLUGIN_PRODUCT}/{_plugin_version()} {_sdk_user_agent()}"

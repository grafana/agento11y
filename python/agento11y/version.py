"""SDK version and User-Agent product token.

SDK_VERSION is stamped into the default generation-export User-Agent (see
``user_agent``). It is read from the installed package metadata so it always
matches the published version; the ``sdk:py:bump`` release tooling only rewrites
pyproject.toml, and reading metadata here avoids a second copy that would drift.
"""

from __future__ import annotations

from importlib.metadata import PackageNotFoundError, version

try:
    SDK_VERSION = version("agento11y")
except PackageNotFoundError:
    # Running from a source tree without an installed distribution.
    SDK_VERSION = "0.0.0+unknown"

# The User-Agent product token is allowlisted by the ingest server, so it
# intentionally keeps the pre-rename name. Do not update it for the
# sigil-sdk -> agento11y rename without server-side dual-read support.
_SDK_USER_AGENT_PRODUCT = "sigil-sdk-python"


def user_agent() -> str:
    """Return the SDK's default generation-export User-Agent product token,
    ``sigil-sdk-python/<SDK_VERSION>``.
    """
    return f"{_SDK_USER_AGENT_PRODUCT}/{SDK_VERSION}"

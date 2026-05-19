"""Cache diagnostics metadata helpers (Anthropic cache-diagnosis beta).

See Sigil documentation: ``docs/guides/cache-diagnostics.md``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from .client import (
    CACHE_DIAGNOSTICS_MISS_REASON_KEY,
    CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY,
    CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY,
)

if TYPE_CHECKING:
    from .client import GenerationRecorder


def set_cache_diagnostics(
    rec: GenerationRecorder | None,
    miss_reason: str,
    *,
    missed_input_tokens: int | None = None,
    previous_message_id: str | None = None,
) -> None:
    """Stamp ``sigil.cache_diagnostics.*`` metadata on a generation recorder.

    Call before :meth:`GenerationRecorder.end`, typically after the provider
    response is available (before or with :meth:`GenerationRecorder.set_result`).
    """

    if rec is None:
        return
    rec.set_cache_diagnostics(
        miss_reason,
        missed_input_tokens=missed_input_tokens,
        previous_message_id=previous_message_id,
    )


__all__ = [
    "CACHE_DIAGNOSTICS_MISS_REASON_KEY",
    "CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY",
    "CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY",
    "set_cache_diagnostics",
]

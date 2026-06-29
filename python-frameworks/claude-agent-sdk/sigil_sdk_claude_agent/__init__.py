"""Public exports for Sigil Claude Agent SDK instrumentation."""

from __future__ import annotations

from .handler import (
    SigilClaudeAgentHandler,
    SigilClaudeSDKClient,
    create_sigil_claude_agent_handler,
    sigil_query,
    with_sigil_claude_agent_options,
)

__all__ = [
    "SigilClaudeSDKClient",
    "SigilClaudeAgentHandler",
    "create_sigil_claude_agent_handler",
    "sigil_query",
    "with_sigil_claude_agent_options",
]

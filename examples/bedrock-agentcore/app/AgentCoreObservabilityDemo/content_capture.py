from __future__ import annotations

from copy import deepcopy
from dataclasses import replace

from agento11y import Client, ClientConfig, ContentCaptureMode, ToolExecutionStart
from agento11y.client import ToolExecutionRecorder
from agento11y.models import Generation


def omit_tool_content() -> bool:
    return ClientConfig.from_env().content_capture in {
        ContentCaptureMode.DEFAULT,
        ContentCaptureMode.NO_TOOL_CONTENT,
    }


def without_tool_payloads(generation: Generation) -> Generation:
    """Keep chat text and tool identities without exporting tool arguments/results."""
    generation = deepcopy(generation)
    for message in [*generation.input, *generation.output]:
        for part in message.parts:
            if part.tool_call is not None:
                part.tool_call.input_json = b""
            if part.tool_result is not None:
                part.tool_result.content = ""
                part.tool_result.content_json = b""
    return generation


class DemoClient(Client):
    def start_tool_execution(self, start: ToolExecutionStart) -> ToolExecutionRecorder:
        # The adapter opts tools into content capture when chat capture is enabled.
        # Let the SDK's capture mode decide instead: full still includes tool JSON.
        return super().start_tool_execution(replace(start, include_content=False))

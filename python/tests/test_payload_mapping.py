"""Tests for the shared provider payload mapping module."""

from __future__ import annotations

from dataclasses import dataclass

from agento11y.models import MessageRole, PartKind
from agento11y.payload_mapping import content_parts, content_text, tool_definitions


class TestContentParts:
    def test_string_content_is_one_text_part(self):
        parts = content_parts("hello")
        assert [part.kind for part in parts] == [PartKind.TEXT]
        assert parts[0].text == "hello"

    def test_blank_content_is_dropped(self):
        assert content_parts("   ") == []
        assert content_parts("") == []
        assert content_parts(None) == []
        assert content_parts([{"type": "text", "text": "  "}]) == []

    def test_text_is_recorded_unstripped(self):
        # Whitespace decides emptiness; the text itself is what the model saw.
        parts = content_parts([{"type": "text", "text": "  padded\n"}])
        assert parts[0].text == "  padded\n"

    def test_anthropic_blocks_keep_order(self):
        parts = content_parts(
            [
                {"type": "thinking", "thinking": "let me think", "signature": "sig"},
                {"type": "text", "text": "the weather is"},
                {"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Berlin"}},
            ],
            role_hint=MessageRole.ASSISTANT,
        )
        assert [part.kind for part in parts] == [PartKind.THINKING, PartKind.TEXT, PartKind.TOOL_CALL]
        assert parts[0].thinking == "let me think"
        assert parts[2].tool_call.id == "toolu_1"
        assert parts[2].tool_call.name == "get_weather"
        assert parts[2].tool_call.input_json == b'{"city":"Berlin"}'

    def test_redacted_thinking_reads_data(self):
        parts = content_parts([{"type": "redacted_thinking", "data": "encrypted-blob"}])
        assert parts[0].kind == PartKind.THINKING
        assert parts[0].thinking == "encrypted-blob"

    def test_server_and_mcp_tool_use_are_tool_calls(self):
        parts = content_parts(
            [
                {"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search", "input": {"query": "x"}},
                {"type": "mcp_tool_use", "id": "mcptoolu_1", "name": "list_files", "input": {}},
            ]
        )
        assert [part.kind for part in parts] == [PartKind.TOOL_CALL, PartKind.TOOL_CALL]
        assert parts[0].tool_call.name == "web_search"
        assert parts[1].tool_call.input_json == b"{}"

    def test_tool_result_block(self):
        parts = content_parts([{"type": "tool_result", "tool_use_id": "toolu_1", "content": "20C", "is_error": False}])
        assert parts[0].kind == PartKind.TOOL_RESULT
        assert parts[0].tool_result.tool_call_id == "toolu_1"
        assert parts[0].tool_result.content == "20C"
        assert parts[0].tool_result.content_json == b'"20C"'
        assert parts[0].tool_result.is_error is False

    def test_tool_result_error_and_structured_content(self):
        parts = content_parts(
            [
                {
                    "type": "tool_result",
                    "tool_call_id": "call_1",
                    "name": "get_weather",
                    "content": [{"type": "text", "text": "boom"}],
                    "is_error": True,
                }
            ]
        )
        result = parts[0].tool_result
        assert result.tool_call_id == "call_1"
        assert result.name == "get_weather"
        assert result.content == "boom"
        assert result.is_error is True

    def test_tool_role_hint_treats_untyped_block_as_result(self):
        parts = content_parts([{"tool_use_id": "toolu_1", "content": "20C"}], role_hint=MessageRole.TOOL)
        assert parts[0].kind == PartKind.TOOL_RESULT
        assert parts[0].tool_result.tool_call_id == "toolu_1"

    def test_openai_and_responses_text_part_types(self):
        parts = content_parts(
            [
                {"type": "input_text", "text": "question"},
                {"type": "output_text", "text": "answer", "annotations": []},
                {"type": "refusal", "refusal": "I can't help with that."},
            ]
        )
        assert [part.text for part in parts] == ["question", "answer", "I can't help with that."]

    def test_null_text_part_is_dropped(self):
        # LiteLLM's chat-to-Responses bridge emits an output_text part with a
        # null text for a tool-call-only turn.
        assert content_parts([{"type": "output_text", "text": None, "annotations": []}]) == []

    def test_unknown_block_types_are_skipped(self):
        parts = content_parts(
            [
                {"type": "image", "source": {"type": "base64", "data": "..."}},
                {"type": "text", "text": "caption"},
            ]
        )
        assert [part.text for part in parts] == ["caption"]

    def test_bare_string_items(self):
        parts = content_parts(["first", "  ", "second"])
        assert [part.text for part in parts] == ["first", "second"]

    def test_object_blocks(self):
        @dataclass
        class Block:
            type: str
            text: str

        parts = content_parts([Block(type="text", text="from object")])
        assert parts[0].text == "from object"


class TestContentText:
    def test_string_is_stripped(self):
        assert content_text("  hi  ") == "hi"

    def test_blocks_are_joined_by_newline(self):
        assert content_text([{"type": "text", "text": "a"}, {"type": "text", "text": "b"}]) == "a\nb"

    def test_nested_content(self):
        assert content_text({"content": [{"text": "inner"}]}) == "inner"

    def test_bytes_and_none(self):
        assert content_text(b"raw ") == "raw"
        assert content_text(None) == ""


class TestToolDefinitions:
    def test_openai_chat_shape(self):
        tools = tool_definitions(
            [
                {
                    "type": "function",
                    "function": {
                        "name": "get_weather",
                        "description": "Get the weather",
                        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}},
                    },
                }
            ]
        )
        assert len(tools) == 1
        assert tools[0].name == "get_weather"
        assert tools[0].description == "Get the weather"
        assert tools[0].type == "function"
        assert b'"city"' in tools[0].input_schema_json

    def test_responses_shape(self):
        tools = tool_definitions(
            [
                {
                    "type": "function",
                    "name": "get_weather",
                    "description": "Get the weather",
                    "parameters": {"type": "object"},
                }
            ]
        )
        assert tools[0].name == "get_weather"
        assert tools[0].type == "function"
        assert tools[0].input_schema_json == b'{"type":"object"}'

    def test_anthropic_shape(self):
        tools = tool_definitions(
            [
                {
                    "name": "get_weather",
                    "description": "Get the weather",
                    "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}},
                }
            ]
        )
        assert tools[0].name == "get_weather"
        # No type on the wire: a schema-carrying tool is a function.
        assert tools[0].type == "function"
        assert b'"city"' in tools[0].input_schema_json

    def test_builtin_tool_keeps_provider_type_and_empty_schema(self):
        tools = tool_definitions([{"type": "web_search_20250305", "name": "web_search", "max_uses": 3}])
        assert tools[0].name == "web_search"
        assert tools[0].type == "web_search_20250305"
        # Absent schema stays empty rather than the JSON literal null.
        assert tools[0].input_schema_json == b""

    def test_nameless_tool_is_skipped(self):
        assert tool_definitions([{"type": "web_search_preview"}, {"type": "function", "function": {}}]) == []

    def test_non_list_input(self):
        assert tool_definitions(None) == []
        assert tool_definitions({"name": "not-a-list"}) == []

"""Tests for the centralised usage-mapping module."""

from __future__ import annotations

from types import SimpleNamespace

import pytest
from agento11y.models import TokenUsage
from agento11y.usage import (
    from_anthropic,
    from_gemini,
    from_generic,
    from_openai_chat,
    from_openai_responses,
    map_usage,
)


class TestFromAnthropic:
    def test_full(self):
        raw = {
            "input_tokens": 100,
            "output_tokens": 50,
            "total_tokens": 0,
            "cache_read_input_tokens": 10,
            "cache_write_input_tokens": 5,
            "cache_creation_input_tokens": 3,
        }
        usage = from_anthropic(raw)
        assert usage.input_tokens == 100
        assert usage.output_tokens == 50
        assert usage.total_tokens == 150  # normalized
        assert usage.cache_read_input_tokens == 10
        # When both upstream fields are present, prefer cache_write_input_tokens.
        assert usage.cache_write_input_tokens == 5
        assert usage.reasoning_tokens == 0

    def test_cache_creation_only_maps_to_cache_write(self):
        raw = {
            "input_tokens": 10,
            "output_tokens": 5,
            "cache_creation_input_tokens": 7,
        }
        usage = from_anthropic(raw)
        assert usage.cache_write_input_tokens == 7

    def test_explicit_zero_cache_write_not_overridden(self):
        raw = {
            "input_tokens": 10,
            "output_tokens": 5,
            "cache_write_input_tokens": 0,
            "cache_creation_input_tokens": 7,
        }
        usage = from_anthropic(raw)
        assert usage.cache_write_input_tokens == 0

    def test_partial(self):
        raw = {"input_tokens": 40, "output_tokens": 20}
        usage = from_anthropic(raw)
        assert usage.input_tokens == 40
        assert usage.output_tokens == 20
        assert usage.total_tokens == 60

    def test_explicit_zeros(self):
        raw = {
            "input_tokens": 0,
            "output_tokens": 0,
            "cache_read_input_tokens": 0,
        }
        usage = from_anthropic(raw)
        assert usage.input_tokens == 0
        assert usage.output_tokens == 0
        assert usage.total_tokens == 0
        assert usage.cache_read_input_tokens == 0

    def test_none(self):
        assert from_anthropic(None) == TokenUsage()

    def test_simplenamespace(self):
        raw = SimpleNamespace(
            input_tokens=80,
            output_tokens=40,
            cache_read_input_tokens=15,
            cache_creation_input_tokens=7,
        )
        usage = from_anthropic(raw)
        assert usage.input_tokens == 80
        assert usage.output_tokens == 40
        assert usage.total_tokens == 120
        assert usage.cache_read_input_tokens == 15
        assert usage.cache_write_input_tokens == 7


class TestFromOpenAIChat:
    def test_full(self):
        raw = {
            "prompt_tokens": 200,
            "completion_tokens": 100,
            "total_tokens": 300,
            "prompt_tokens_details": {
                "cached_tokens": 50,
                "cache_creation_tokens": 20,
            },
            "completion_tokens_details": {
                "reasoning_tokens": 30,
            },
        }
        usage = from_openai_chat(raw)
        assert usage.input_tokens == 200
        assert usage.output_tokens == 100
        assert usage.total_tokens == 300
        assert usage.cache_read_input_tokens == 50
        assert usage.cache_write_input_tokens == 20
        assert usage.reasoning_tokens == 30

    def test_missing_detail_objects(self):
        raw = {
            "prompt_tokens": 200,
            "completion_tokens": 100,
            "total_tokens": 300,
        }
        usage = from_openai_chat(raw)
        assert usage.input_tokens == 200
        assert usage.output_tokens == 100
        assert usage.total_tokens == 300
        assert usage.cache_read_input_tokens == 0
        assert usage.reasoning_tokens == 0

    def test_total_autofill(self):
        raw = {
            "prompt_tokens": 150,
            "completion_tokens": 75,
            "total_tokens": 0,
        }
        usage = from_openai_chat(raw)
        assert usage.total_tokens == 225

    def test_none(self):
        assert from_openai_chat(None) == TokenUsage()

    def test_simplenamespace_nested(self):
        raw = SimpleNamespace(
            prompt_tokens=200,
            completion_tokens=100,
            total_tokens=300,
            prompt_tokens_details=SimpleNamespace(
                cached_tokens=50,
                cache_creation_tokens=10,
            ),
            completion_tokens_details=SimpleNamespace(
                reasoning_tokens=25,
            ),
        )
        usage = from_openai_chat(raw)
        assert usage.cache_read_input_tokens == 50
        assert usage.cache_write_input_tokens == 10
        assert usage.reasoning_tokens == 25


class TestFromOpenAIResponses:
    def test_full(self):
        raw = {
            "input_tokens": 300,
            "output_tokens": 150,
            "total_tokens": 450,
            "input_tokens_details": {"cached_tokens": 80},
            "output_tokens_details": {"reasoning_tokens": 40},
        }
        usage = from_openai_responses(raw)
        assert usage.input_tokens == 300
        assert usage.output_tokens == 150
        assert usage.total_tokens == 450
        assert usage.cache_read_input_tokens == 80
        assert usage.reasoning_tokens == 40

    def test_missing_detail_objects(self):
        raw = {
            "input_tokens": 300,
            "output_tokens": 150,
            "total_tokens": 450,
        }
        usage = from_openai_responses(raw)
        assert usage.cache_read_input_tokens == 0
        assert usage.reasoning_tokens == 0

    def test_total_autofill(self):
        raw = {"input_tokens": 200, "output_tokens": 80}
        usage = from_openai_responses(raw)
        assert usage.total_tokens == 280

    def test_none(self):
        assert from_openai_responses(None) == TokenUsage()


class TestFromGemini:
    def test_full(self):
        raw = {
            "prompt_token_count": 500,
            "candidates_token_count": 250,
            "total_token_count": 800,
            "cached_content_token_count": 60,
            "cache_write_input_token_count": 20,
            "cache_creation_input_token_count": 10,
            "thoughts_token_count": 35,
            "tool_use_prompt_token_count": 15,
        }
        usage = from_gemini(raw)
        assert usage.input_tokens == 515
        assert usage.output_tokens == 285
        assert usage.total_tokens == 800
        assert usage.cache_read_input_tokens == 60
        # When both upstream fields are present, prefer cache_write_input_token_count.
        assert usage.cache_write_input_tokens == 20
        assert usage.reasoning_tokens == 35

    def test_total_autofill_includes_tool_use_and_thoughts(self):
        raw = {
            "prompt_token_count": 100,
            "candidates_token_count": 50,
            "total_token_count": 0,
            "thoughts_token_count": 20,
            "tool_use_prompt_token_count": 10,
        }
        usage = from_gemini(raw)
        assert usage.input_tokens == 110
        assert usage.output_tokens == 70
        assert usage.total_tokens == 180  # 100 + 50 + 10 + 20

    def test_partial(self):
        raw = {
            "prompt_token_count": 100,
            "candidates_token_count": 50,
        }
        usage = from_gemini(raw)
        assert usage.total_tokens == 150
        assert usage.cache_read_input_tokens == 0

    def test_explicit_zero_cache_write_not_overridden(self):
        raw = {
            "prompt_token_count": 100,
            "candidates_token_count": 50,
            "cache_write_input_token_count": 0,
            "cache_creation_input_token_count": 7,
        }
        usage = from_gemini(raw)
        assert usage.cache_write_input_tokens == 0

    def test_none(self):
        assert from_gemini(None) == TokenUsage()


class TestMapUsage:
    def test_none(self):
        assert map_usage(None) == TokenUsage()

    def test_empty_dict(self):
        usage = map_usage({})
        assert usage == TokenUsage()

    def test_dispatches_gemini(self):
        raw = {
            "prompt_token_count": 100,
            "candidates_token_count": 50,
            "total_token_count": 150,
        }
        usage = map_usage(raw)
        assert usage.input_tokens == 100
        assert usage.output_tokens == 50

    def test_dispatches_openai_chat(self):
        raw = {
            "prompt_tokens": 200,
            "completion_tokens": 100,
            "total_tokens": 300,
            "prompt_tokens_details": {"cached_tokens": 50},
            "completion_tokens_details": {"reasoning_tokens": 30},
        }
        usage = map_usage(raw)
        assert usage.input_tokens == 200
        assert usage.cache_read_input_tokens == 50
        assert usage.reasoning_tokens == 30

    def test_dispatches_openai_responses(self):
        raw = {
            "input_tokens": 300,
            "output_tokens": 150,
            "total_tokens": 450,
            "input_tokens_details": {"cached_tokens": 80},
            "output_tokens_details": {"reasoning_tokens": 40},
        }
        usage = map_usage(raw)
        assert usage.cache_read_input_tokens == 80
        assert usage.reasoning_tokens == 40

    def test_dispatches_anthropic(self):
        raw = {
            "input_tokens": 100,
            "output_tokens": 50,
            "cache_read_input_tokens": 10,
        }
        usage = map_usage(raw)
        assert usage.input_tokens == 100
        assert usage.cache_read_input_tokens == 10

    def test_legacy_fallback(self):
        raw = SimpleNamespace()  # no matching keys at all
        usage = map_usage(raw)
        assert usage == TokenUsage()

    def test_simplenamespace_dispatch(self):
        raw = SimpleNamespace(
            input_tokens=80,
            output_tokens=40,
            cache_read_input_tokens=15,
        )
        usage = map_usage(raw)
        assert usage.input_tokens == 80
        assert usage.cache_read_input_tokens == 15

    def test_explicit_zero_preserved(self):
        raw = {
            "input_tokens": 0,
            "output_tokens": 0,
            "total_tokens": 0,
            "cache_read_input_tokens": 0,
        }
        usage = map_usage(raw)
        assert usage.input_tokens == 0
        assert usage.cache_read_input_tokens == 0

    def test_partial_total_only(self):
        usage = map_usage({"total_tokens": 7})
        assert usage.total_tokens == 7

    def test_partial_completion_only(self):
        usage = map_usage({"completion_tokens": 5})
        assert usage.output_tokens == 5
        assert usage.total_tokens == 5

    def test_partial_output_only(self):
        usage = map_usage({"output_tokens": 3})
        assert usage.output_tokens == 3
        assert usage.total_tokens == 3


class TestFromGeneric:
    def test_none(self):
        assert from_generic(None) == TokenUsage()

    def test_total_only(self):
        usage = from_generic({"total_tokens": 42})
        assert usage.total_tokens == 42
        assert usage.input_tokens == 0

    def test_mixed_key_families(self):
        raw = {
            "prompt_tokens": 10,
            "output_tokens": 5,
            "cache_read_input_tokens": 3,
        }
        usage = from_generic(raw)
        assert usage.input_tokens == 10
        assert usage.output_tokens == 5
        assert usage.total_tokens == 15
        assert usage.cache_read_input_tokens == 3

    def test_prefers_prompt_tokens_over_input_tokens(self):
        raw = {"prompt_tokens": 10, "input_tokens": 20}
        usage = from_generic(raw)
        assert usage.input_tokens == 10

    def test_explicit_zero_prompt_tokens_not_overridden(self):
        raw = {"prompt_tokens": 0, "input_tokens": 50}
        usage = from_generic(raw)
        assert usage.input_tokens == 0

    def test_explicit_zero_completion_tokens_not_overridden(self):
        raw = {"completion_tokens": 0, "output_tokens": 50}
        usage = from_generic(raw)
        assert usage.output_tokens == 0

    def test_explicit_zero_cache_write_not_overridden(self):
        raw = {
            "input_tokens": 10,
            "output_tokens": 5,
            "cache_write_input_tokens": 0,
            "cache_creation_input_tokens": 7,
        }
        usage = from_generic(raw)
        assert usage.cache_write_input_tokens == 0

    def test_flat_reasoning_tokens(self):
        raw = {"input_tokens": 100, "reasoning_tokens": 25}
        usage = from_generic(raw)
        assert usage.reasoning_tokens == 25


class TestHelperEdgeCases:
    @pytest.mark.parametrize(
        "value,expected",
        [
            (True, 0),  # bools → 0
            (False, 0),
            ("42", 42),  # string ints
            ("", 0),
            (3.0, 3),  # exact floats
            (None, 0),
        ],
    )
    def test_as_int_via_anthropic(self, value, expected):
        raw = {"input_tokens": value}
        usage = from_anthropic(raw)
        assert usage.input_tokens == expected

"""Tests for model -> protobuf mapping of generation payloads."""

from __future__ import annotations

from agento11y.models import Generation, TokenInputSemantics, TokenUsage
from agento11y.proto_mapping import generation_to_proto


class TestUsageMapping:
    def test_input_semantics_maps_to_proto_enum(self):
        proto = generation_to_proto(
            Generation(
                usage=TokenUsage(input_tokens=10, input_semantics=TokenInputSemantics.INCLUSIVE),
            )
        )
        assert proto.usage.input_semantics == 1  # TOKEN_INPUT_SEMANTICS_INCLUSIVE

    def test_unspecified_semantics_stays_default(self):
        proto = generation_to_proto(Generation(usage=TokenUsage(input_tokens=10)))
        assert proto.usage.input_semantics == 0  # TOKEN_INPUT_SEMANTICS_UNSPECIFIED

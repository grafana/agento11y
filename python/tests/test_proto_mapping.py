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

    def test_modality_presence_and_normalization(self):
        from agento11y.models import ModalityTokenCounts

        usage = TokenUsage(input_tokens=3, input_by_modality=ModalityTokenCounts(tokens={"image": 3}, complete=True))
        proto = generation_to_proto(Generation(usage=usage.normalize()))
        assert proto.usage.HasField("input_by_modality")
        assert not proto.usage.HasField("output_by_modality")
        assert proto.usage.input_by_modality.tokens["image"] == 3
        assert proto.usage.input_by_modality.complete

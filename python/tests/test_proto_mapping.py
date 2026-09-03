"""Tests for model -> protobuf mapping of generation payloads."""

from __future__ import annotations

from agento11y.models import (
    Generation,
    Media,
    Message,
    MessageRole,
    Part,
    PartKind,
    TokenInputSemantics,
    TokenUsage,
    media_part,
)
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


class TestMediaPartMapping:
    def test_media_part_maps_every_field(self):
        proto = generation_to_proto(
            Generation(
                input=[
                    Message(
                        role=MessageRole.USER,
                        parts=[
                            media_part(
                                Media(
                                    kind="image",
                                    url="data:image/png;base64,abc123",
                                    mime_type="image/png",
                                    name="prompt.png",
                                )
                            )
                        ],
                    )
                ]
            )
        )

        part = proto.input[0].parts[0]
        assert part.WhichOneof("payload") == "media"
        assert part.media.kind == "image"
        assert part.media.url == "data:image/png;base64,abc123"
        assert part.media.mime_type == "image/png"
        assert part.media.name == "prompt.png"

    def test_media_part_without_a_payload_is_dropped(self):
        # Go's codec.partsToProto skips a media part with no payload; it must be
        # omitted, not sent as an empty part the server decodes as empty text.
        proto = generation_to_proto(
            Generation(
                input=[
                    Message(
                        role=MessageRole.USER,
                        parts=[
                            Part(kind=PartKind.MEDIA, media=None),
                            Part(kind=PartKind.TEXT, text="hello"),
                        ],
                    )
                ]
            )
        )

        assert len(proto.input[0].parts) == 1
        assert proto.input[0].parts[0].text == "hello"

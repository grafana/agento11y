"""Type coercion for whatever hermes put on a hook payload.

Hermes mirrors provider shapes rather than normalizing them, so these helpers
are what stands between a provider's idea of a message and the SDK's. Each one
has to absorb the wrong type rather than raise, because they are called inside
``set_result``'s argument list, where raising costs the whole generation.
"""

from __future__ import annotations

from typing import Any

import pytest

from grafana_agento11y_hermes._coerce import as_int, as_optional_float, as_optional_int, coerce_text


class _Block:
    def __repr__(self) -> str:
        return "<block>"


@pytest.mark.parametrize(
    ("content", "expected"),
    (
        (None, ""),
        ("plain text", "plain text"),
        ("", ""),
        ([], ""),
        (["one", "two"], "one\ntwo"),
        # Anthropic-shaped typed blocks.
        ([{"type": "text", "text": "hello"}], "hello"),
        ([{"type": "text", "text": "a"}, {"type": "text", "text": "b"}], "a\nb"),
        # Some providers name the field ``content`` instead.
        ([{"content": "hello"}], "hello"),
        ([{"text": "from-text", "content": "from-content"}], "from-text"),
        # A block with neither is serialized rather than dropped, so a
        # thinking or image block still shows up in the recorded input.
        ([{"type": "image", "source": {"kind": "b64"}}], '{"type": "image", "source": {"kind": "b64"}}'),
        ([_Block()], "<block>"),
        ([7, True], "7\nTrue"),
        (["a", "", "b"], "a\nb"),
        ([{"text": ""}, {"text": "kept"}], "kept"),
        (42, "42"),
        ({"role": "user"}, "{'role': 'user'}"),
    ),
)
def test_content_coerces_to_text(content: Any, expected: str) -> None:
    assert coerce_text(content) == expected


def test_an_unserializable_block_uses_the_default_stringifier() -> None:
    """``json.dumps(..., default=str)`` — a block must never raise out of here."""
    out = coerce_text([{"type": "custom", "value": _Block()}])
    assert "<block>" in out


@pytest.mark.parametrize(
    ("value", "expected"),
    (
        (5, 5),
        ("5", 5),
        (5.9, 5),
        (None, 0),
        ("", 0),
        (0, 0),
        ("not a number", 0),
        ([], 0),
        ({}, 0),
        (object(), 0),
    ),
)
def test_as_int_never_raises(value: Any, expected: int) -> None:
    assert as_int(value) == expected


@pytest.mark.parametrize(
    ("value", "expected"),
    (
        (5, 5),
        ("5", 5),
        (5.9, 5),
        # Zero is a real value here, unlike in as_int's ``value or 0``.
        (0, 0),
        (None, None),
        ("", None),
        ("not a number", None),
        (object(), None),
    ),
)
def test_as_optional_int_returns_none_rather_than_raising(value: Any, expected: int | None) -> None:
    assert as_optional_int(value) == expected


@pytest.mark.parametrize(
    ("value", "expected"),
    (
        (1.5, 1.5),
        ("1.5", 1.5),
        (2, 2.0),
        (0, 0.0),
        (None, None),
        ("", None),
        ("not a number", None),
        (object(), None),
    ),
)
def test_as_optional_float_returns_none_rather_than_raising(value: Any, expected: float | None) -> None:
    assert as_optional_float(value) == expected

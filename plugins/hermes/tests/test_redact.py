"""Tests for the structural payload redactor."""

from __future__ import annotations

import pytest

from grafana_agento11y_hermes import _redact


def test_truncate_long_string_uses_caller_max_chars() -> None:
    long = "a" * 20000
    result = _redact.safe_value(long, max_chars=12000)
    assert result.startswith("a" * 12000)
    assert "truncated" in result


def test_short_max_chars_truncates_aggressively() -> None:
    result = _redact.safe_value("a" * 50, max_chars=10)
    assert result.startswith("a" * 10)
    assert "truncated" in result


def test_max_chars_kwarg_is_required() -> None:
    # The signature mandates max_chars; calling without it must error.
    try:
        _redact.safe_value("hi")  # ty: ignore[missing-argument]
    except TypeError:
        return
    raise AssertionError("safe_value should require max_chars")


def test_depth_limit_caps_at_4() -> None:
    # depth > 4 returns sentinel — top-level dict is depth 0, so the value at l5
    # is encountered at depth 5 and replaced.
    nested = {"l1": {"l2": {"l3": {"l4": {"l5": "deep"}}}}}
    out = _redact.safe_value(nested, max_chars=12000)
    cur = out
    for level in ("l1", "l2", "l3", "l4", "l5"):
        cur = cur[level]
    assert cur == "<max-depth>"


def test_within_depth_limit_preserves_values() -> None:
    nested = {"l1": {"l2": {"l3": "ok"}}}
    out = _redact.safe_value(nested, max_chars=12000)
    assert out == {"l1": {"l2": {"l3": "ok"}}}


def test_dict_entry_cap_is_50() -> None:
    big_dict = {f"k{i}": i for i in range(200)}
    out = _redact.safe_value(big_dict, max_chars=12000)
    assert len(out) == 50


def test_list_entry_cap_is_50() -> None:
    big_list = list(range(200))
    out = _redact.safe_value(big_list, max_chars=12000)
    assert len(out) == 50
    assert out[0] == 0
    assert out[-1] == 49


def test_scalars_pass_through_unchanged() -> None:
    assert _redact.safe_value(None, max_chars=12000) is None
    assert _redact.safe_value(42, max_chars=12000) == 42
    assert _redact.safe_value(3.14, max_chars=12000) == 3.14
    assert _redact.safe_value(True, max_chars=12000) is True


def test_bytes_become_descriptor() -> None:
    out = _redact.safe_value(b"hello", max_chars=12000)
    assert out == {"type": "bytes", "len": 5}


def test_parse_json_strings_when_requested() -> None:
    s = '{"a": 1, "b": [1, 2, 3]}'
    out = _redact.safe_value(s, max_chars=12000, parse_json_strings=True)
    assert out == {"a": 1, "b": [1, 2, 3]}


def test_unparseable_json_string_returned_as_string() -> None:
    s = "not json {{"
    out = _redact.safe_value(s, max_chars=12000, parse_json_strings=True)
    assert out == "not json {{"


def test_a_string_over_max_chars_is_never_parsed_as_json() -> None:
    """The guard exists so a multi-megabyte tool result is not decoded whole."""
    big = '{"a": "' + "x" * 200 + '"}'
    out = _redact.safe_value(big, max_chars=50, parse_json_strings=True)
    assert isinstance(out, str), "over the cap it stays a string and gets truncated"
    assert "truncated" in out


@pytest.mark.parametrize(
    ("value", "expected"),
    (
        # A hermes tool result is often JSON with a hint glued onto the end.
        ('{"ok": true} [Hint: run tests next]', {"ok": True, "_hint": "[Hint: run tests next]"}),
        ('{"ok": true} see also the log', {"ok": True, "_trailing_text": "see also the log"}),
        # A top-level list is wrapped so the trailing text has somewhere to go.
        ("[1, 2] [Hint: more]", {"data": [1, 2], "_hint": "[Hint: more]"}),
        ("[1, 2] tail", {"data": [1, 2], "_trailing_text": "tail"}),
        # The payload already owning the key must not have it overwritten.
        ('{"_hint": "mine"} [Hint: theirs]', {"_hint": "mine", "_trailing_text": "[Hint: theirs]"}),
        # Only whitespace after the document is not trailing text.
        ('{"ok": true}   \n', {"ok": True}),
        (r'{"token": "abc\"def"}', "[REDACTED:invalid-json]"),
        (
            '{"token": "plain-secret"} [Hint: retry]',
            {"token": "[REDACTED:json-secret-field]", "_hint": "[Hint: retry]"},
        ),
    ),
)
def test_text_trailing_a_json_document_is_kept_beside_it(value: str, expected: object) -> None:
    assert _redact.safe_value(value, max_chars=12000, parse_json_strings=True) == expected


def test_the_parse_cap_is_measured_on_the_whole_document() -> None:
    """The boundary the guard draws: at the cap it parses, one char over it does not."""
    doc = '{"a": "' + "x" * 20 + '"}'
    assert isinstance(_redact.safe_value(doc, max_chars=len(doc), parse_json_strings=True), dict)
    assert isinstance(_redact.safe_value(doc, max_chars=len(doc) - 1, parse_json_strings=True), str)


def test_a_set_is_recorded_as_a_list() -> None:
    out = _redact.safe_value({"tags"}, max_chars=12000)
    assert out == ["tags"]


def test_an_object_with_attributes_is_recorded_as_its_dict() -> None:
    class Thing:
        def __init__(self) -> None:
            self.name = "x"

    assert _redact.safe_value(Thing(), max_chars=12000) == {"name": "x"}


def test_object_without_dict_attribute_falls_back_to_repr() -> None:
    class Opaque:
        __slots__ = ()

        def __repr__(self) -> str:
            return "<opaque>"

    out = _redact.safe_value(Opaque(), max_chars=12000)
    assert out == "<opaque>"

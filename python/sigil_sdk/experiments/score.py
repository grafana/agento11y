"""Typed score-value constructors for the experiments surface."""

from __future__ import annotations

from ..models import ScoreValue


def number(value: float) -> ScoreValue:
    """A numeric score (e.g. ``0.82``, token counts, latencies)."""

    return ScoreValue(number=float(value))


def boolean(value: bool) -> ScoreValue:
    """A pass/fail-style boolean score."""

    return ScoreValue(boolean=bool(value))


def string(value: str) -> ScoreValue:
    """A categorical/label score (e.g. ``"relevant"``)."""

    return ScoreValue(string=str(value))

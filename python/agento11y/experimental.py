"""Opt-in gate for SDK features that are not stable yet.

An experimental feature can change or be removed in any release, and is not
covered by the compatibility the rest of the SDK aims for.

This gate is separate from ``AGENTO11Y_USE_EXPERIMENTAL_OTEL``, which stays an
independent opt-in for experimental trial spans and evaluation-result events.
"""

from __future__ import annotations

import os
from enum import Enum

from .errors import ExperimentalFeatureDisabledError

ENV_ENABLE_EXPERIMENTAL_FEATURES = "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES"


class ExperimentalFeature(str, Enum):
    CLOUD_TRIAL_EVALUATION = "cloud trial evaluation"
    OTEL_GENERATION_EXPORT = "otel generation export"


FEATURE_CLOUD_TRIAL_EVALUATION = ExperimentalFeature.CLOUD_TRIAL_EVALUATION.value
FEATURE_OTEL_GENERATION_EXPORT = ExperimentalFeature.OTEL_GENERATION_EXPORT.value

_TRUTHY = frozenset({"1", "true", "yes", "on"})


def experimental_features_enabled() -> bool:
    """Reports whether the experimental opt-in is set to a truthy value."""

    return os.environ.get(ENV_ENABLE_EXPERIMENTAL_FEATURES, "").strip().lower() in _TRUTHY


def require_experimental(feature: str) -> None:
    """Raises :class:`ExperimentalFeatureDisabledError` unless the gate is set."""

    if not experimental_features_enabled():
        raise ExperimentalFeatureDisabledError(feature, ENV_ENABLE_EXPERIMENTAL_FEATURES)


__all__ = [
    "ENV_ENABLE_EXPERIMENTAL_FEATURES",
    "ExperimentalFeature",
    "FEATURE_CLOUD_TRIAL_EVALUATION",
    "FEATURE_OTEL_GENERATION_EXPORT",
    "experimental_features_enabled",
    "require_experimental",
]

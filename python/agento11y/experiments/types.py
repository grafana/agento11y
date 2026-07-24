"""Value types for the experiments surface.

These are lightweight, transport-agnostic descriptions of the experiment data
model: a suite of test cases, the candidate under test, evaluator provenance, and
a serializable reference to a single trial (so a trial can be opened in a separate
process from the one that created the run — e.g. a benchmark verifier container).
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Any

from ..config import _warn_legacy_env


class ExperimentStatus(str, Enum):
    """Terminal status an external run can be finalized to (plus ``running``).

    The backend's terminal-success status is ``completed`` (it rejects
    ``succeeded``). The string ``"succeeded"`` is still accepted as an input alias
    and mapped to ``completed`` on the wire.
    """

    RUNNING = "running"
    COMPLETED = "completed"
    FAILED = "failed"


class TrialStatus(str, Enum):
    """Lifecycle of a single trial (a test case attempt)."""

    RUNNING = "running"
    COMPLETED = "completed"
    PASSED = "passed"
    FAILED = "failed"
    ERRORED = "errored"
    SKIPPED = "skipped"


class EvaluatorKind(str, Enum):
    """The OTel-aligned evaluator type vocabulary.

    Agent Observability-specific kinds map deterministically onto this set for telemetry.
    """

    LLM_JUDGE = "llm_judge"
    DETERMINISTIC = "deterministic"
    HUMAN = "human"
    CUSTOM = "custom"


def normalize_evaluator_kind(kind: str) -> str:
    """Maps a free-form evaluator kind to the OTel-aligned set."""

    k = (kind or "").strip().lower()
    if k in {"llm_judge", "llm-judge", "llm", "judge", "rubric"}:
        return EvaluatorKind.LLM_JUDGE.value
    if k in {"deterministic", "check", "rule", "exact", "code"}:
        return EvaluatorKind.DETERMINISTIC.value
    if k in {"human", "manual", "annotator"}:
        return EvaluatorKind.HUMAN.value
    return EvaluatorKind.CUSTOM.value


@dataclass(slots=True)
class TestCase:
    """One test case (scenario) in a suite."""

    __test__ = False  # not a pytest test class

    test_case_id: str
    name: str = ""
    description: str = ""
    tags: list[str] = field(default_factory=list)
    category: str = ""
    input: Any = None
    expected: Any = None
    weight: float = 1.0
    metadata: dict[str, Any] = field(default_factory=dict)
    artifact_refs: list[dict[str, Any]] = field(default_factory=list)

    @property
    def id(self) -> str:
        """Portable test-case id alias used in suite YAML and examples."""

        return self.test_case_id

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> TestCase:
        """Builds a test case from a mapping, accepting ``id`` or ``test_case_id``."""

        if not isinstance(data, dict):
            raise ValueError(f"test case must be a mapping, got {type(data).__name__}")
        test_case_id = str(data.get("test_case_id") or data.get("id") or "").strip()
        if not test_case_id:
            raise ValueError("test case requires an 'id' (or 'test_case_id')")
        tags = data.get("tags") or []
        if not isinstance(tags, list) or not all(isinstance(tag, str) for tag in tags):
            raise ValueError(f"test case {test_case_id!r} tags must be a list of strings")
        metadata = data.get("metadata") or {}
        if not isinstance(metadata, dict):
            raise ValueError(f"test case {test_case_id!r} metadata must be a mapping")
        try:
            weight = float(data.get("weight", 1.0))
        except (TypeError, ValueError) as exc:
            raise ValueError(f"test case {test_case_id!r} weight must be numeric") from exc
        return cls(
            test_case_id=test_case_id,
            name=str(data.get("name") or ""),
            description=str(data.get("description") or ""),
            tags=list(tags),
            category=str(data.get("category") or ""),
            input=data.get("input"),
            expected=data.get("expected"),
            weight=weight,
            metadata=dict(metadata),
            artifact_refs=[dict(ref) for ref in data.get("artifact_refs", []) or [] if isinstance(ref, dict)],
        )

    def to_dict(self) -> dict[str, Any]:
        """Returns the portable YAML/JSON representation for this test case."""

        out: dict[str, Any] = {"id": self.test_case_id}
        if self.name:
            out["name"] = self.name
        if self.description:
            out["description"] = self.description
        if self.tags:
            out["tags"] = list(self.tags)
        if self.category:
            out["category"] = self.category
        if self.input is not None:
            out["input"] = self.input
        if self.expected is not None:
            out["expected"] = self.expected
        if self.weight != 1.0:
            out["weight"] = self.weight
        if self.metadata:
            out["metadata"] = dict(self.metadata)
        if self.artifact_refs:
            out["artifact_refs"] = [dict(ref) for ref in self.artifact_refs]
        return out


@dataclass(slots=True)
class TestSuite:
    """A named, versioned collection of test cases.

    A suite can be local-only or pulled from the stored-suite control plane. Its
    id and version are stamped onto the experiment and each trial.
    """

    __test__ = False  # not a pytest test class

    suite_id: str
    name: str = ""
    version: str = "1.0.0"
    description: str = ""
    tags: list[str] = field(default_factory=list)
    changelog: str = ""
    test_cases: list[TestCase] = field(default_factory=list)

    @property
    def cases(self) -> list[TestCase]:
        """The suite's test cases (alias for :attr:`test_cases`)."""

        return self.test_cases

    def case(self, test_case_id: str) -> TestCase | None:
        """Returns the test case with ``test_case_id``, if present."""

        for tc in self.test_cases:
            if tc.test_case_id == test_case_id:
                return tc
        return None

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> TestSuite:
        """Builds a suite from a mapping (accepts ``cases`` or ``test_cases``)."""

        suite_id = str(data.get("suite_id") or data.get("id") or "").strip()
        if not suite_id:
            raise ValueError("suite requires a 'suite_id' (or 'id')")
        raw_cases = data.get("cases")
        if raw_cases is None:
            raw_cases = data.get("test_cases") or []
        if not isinstance(raw_cases, list):
            raise ValueError("suite cases must be a list of mappings")
        tags = data.get("tags") or []
        if not isinstance(tags, list) or not all(isinstance(tag, str) for tag in tags):
            raise ValueError("suite tags must be a list of strings")
        return cls(
            suite_id=suite_id,
            name=str(data.get("name") or ""),
            version=str(data.get("version") or "1.0.0"),
            description=str(data.get("description") or ""),
            tags=list(tags),
            changelog=str(data.get("changelog") or ""),
            test_cases=[TestCase.from_dict(c) for c in raw_cases],
        )

    @classmethod
    def from_yaml(cls, path: str) -> TestSuite:
        """Loads a suite from a YAML file (requires PyYAML; host-side only)."""

        import yaml  # lazy: keeps the package importable in minimal vendored envs

        with open(path, encoding="utf-8") as handle:
            data = yaml.safe_load(handle)
        if not isinstance(data, dict):
            raise ValueError(f"suite YAML must be a mapping, got {type(data).__name__}")
        return cls.from_dict(data)

    def to_dict(self) -> dict[str, Any]:
        """Returns the portable YAML/JSON representation for this suite."""

        out: dict[str, Any] = {"suite_id": self.suite_id}
        if self.name:
            out["name"] = self.name
        if self.version:
            out["version"] = self.version
        if self.description:
            out["description"] = self.description
        if self.tags:
            out["tags"] = list(self.tags)
        if self.changelog:
            out["changelog"] = self.changelog
        out["cases"] = [case.to_dict() for case in self.test_cases]
        return out

    def to_yaml(self, path: str | os.PathLike[str] | None = None) -> str:
        """Serializes the suite to YAML and optionally writes it to ``path``."""

        import yaml  # lazy: keeps the package importable in minimal vendored envs

        text = yaml.safe_dump(self.to_dict(), sort_keys=False)
        if path is not None:
            Path(path).write_text(text, encoding="utf-8")
        return text


@dataclass(slots=True)
class Candidate:
    """The thing under test (agent + model + version provenance)."""

    agent_name: str = ""
    agent_version: str = ""
    prompt_version: str = ""
    model_provider: str = ""
    model_name: str = ""
    git_sha: str = ""

    @classmethod
    def from_obj(cls, value: Candidate | dict[str, Any] | None) -> Candidate | None:
        """Coerces a ``Candidate`` or a plain mapping into a ``Candidate``."""

        if value is None or isinstance(value, cls):
            return value
        if isinstance(value, dict):
            known = {f for f in cls.__dataclass_fields__}  # type: ignore[attr-defined]
            return cls(**{k: v for k, v in value.items() if k in known})
        raise TypeError(f"candidate must be a Candidate or dict, got {type(value).__name__}")

    def as_metadata(self) -> dict[str, Any]:
        out: dict[str, Any] = {}
        for k, v in {
            "agent_name": self.agent_name,
            "agent_version": self.agent_version,
            "prompt_version": self.prompt_version,
            "model_provider": self.model_provider,
            "model_name": self.model_name,
            "git_sha": self.git_sha,
        }.items():
            if v:
                out[k] = v
        return out


@dataclass(slots=True)
class Evaluator:
    """Provenance for whatever produced a score."""

    evaluator_id: str
    version: str = "0"
    kind: str = EvaluatorKind.CUSTOM.value
    reference_set_id: str = ""
    reference_set_version: str = ""

    def normalized_kind(self) -> str:
        return normalize_evaluator_kind(self.kind)


# Environment variables used to hand a trial reference across a process or
# container boundary.
ENV_EXPERIMENT_ID = "AGENTO11Y_EXPERIMENT_ID"
ENV_TEST_CASE_ID = "AGENTO11Y_TEST_CASE_ID"
ENV_ATTEMPT = "AGENTO11Y_ATTEMPT"
ENV_SUITE_ID = "AGENTO11Y_SUITE_ID"
ENV_SUITE_VERSION = "AGENTO11Y_SUITE_VERSION"
ENV_TRAJECTORY_ID = "AGENTO11Y_TRAJECTORY_ID"


def _first_nonblank(env: dict[str, str], *keys: str) -> str:
    """Returns the first nonblank (trimmed) value of ``keys`` in ``env``."""

    for key in keys:
        val = (env.get(key) or "").strip()
        if val:
            return val
    return ""


@dataclass(frozen=True)
class TrialRef:
    """A serializable pointer to one trial, openable in any process.

    ``experiment_id`` is the canonical run identifier. ``trajectory_id`` is an
    optional stable per-attempt id used to correlate an out-of-band execution
    (e.g. a Harbor trial) with this trial.
    """

    experiment_id: str
    test_case_id: str
    attempt: int = 1
    suite_id: str = ""
    suite_version: str = ""
    suite_name: str = ""
    test_case_name: str = ""
    trajectory_id: str = ""

    # Backwards-compatible alias: some callers spell the run id ``run_id``.
    @property
    def run_id(self) -> str:
        return self.experiment_id

    def to_json(self) -> dict[str, Any]:
        return {
            "experiment_id": self.experiment_id,
            "test_case_id": self.test_case_id,
            "attempt": self.attempt,
            "suite_id": self.suite_id,
            "suite_version": self.suite_version,
            "suite_name": self.suite_name,
            "test_case_name": self.test_case_name,
            "trajectory_id": self.trajectory_id,
        }

    @classmethod
    def from_json(cls, payload: dict[str, Any]) -> TrialRef:
        experiment_id = str(payload.get("experiment_id") or payload.get("run_id") or "").strip()
        return cls(
            experiment_id=experiment_id,
            test_case_id=str(payload.get("test_case_id") or "").strip(),
            attempt=int(payload.get("attempt") or 1),
            suite_id=str(payload.get("suite_id") or "").strip(),
            suite_version=str(payload.get("suite_version") or "").strip(),
            suite_name=str(payload.get("suite_name") or "").strip(),
            test_case_name=str(payload.get("test_case_name") or "").strip(),
            trajectory_id=str(payload.get("trajectory_id") or "").strip(),
        )

    def to_env(self) -> dict[str, str]:
        env = {
            ENV_EXPERIMENT_ID: self.experiment_id,
            ENV_TEST_CASE_ID: self.test_case_id,
            ENV_ATTEMPT: str(self.attempt),
        }
        if self.suite_id:
            env[ENV_SUITE_ID] = self.suite_id
        if self.suite_version:
            env[ENV_SUITE_VERSION] = self.suite_version
        if self.trajectory_id:
            env[ENV_TRAJECTORY_ID] = self.trajectory_id
        return env

    @classmethod
    def from_env(cls, environ: dict[str, str] | None = None) -> TrialRef | None:
        env = environ if environ is not None else dict(os.environ)
        _warn_legacy_env(env)
        experiment_id = _first_nonblank(env, ENV_EXPERIMENT_ID)
        test_case_id = _first_nonblank(env, ENV_TEST_CASE_ID)
        if not experiment_id or not test_case_id:
            return None
        try:
            attempt = int(_first_nonblank(env, ENV_ATTEMPT) or "1")
        except ValueError:
            attempt = 1
        return cls(
            experiment_id=experiment_id,
            test_case_id=test_case_id,
            attempt=attempt,
            suite_id=_first_nonblank(env, ENV_SUITE_ID),
            suite_version=_first_nonblank(env, ENV_SUITE_VERSION),
            trajectory_id=_first_nonblank(env, ENV_TRAJECTORY_ID),
        )

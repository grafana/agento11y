"""Generic experiment runner for Sigil offline evaluation.

This module rides on top of the core SDK's generation recording so that
generations emitted while an agent runs are automatically tagged with an
experiment ``run_id`` — no framework adapter required. It is the framework-free
counterpart to ``sigil-sdk-langgraph``'s experiment runner: where the LangGraph
version wires the run into ``graph.invoke(config=run.langgraph_config())``, this
version wires it into ``with run.start_generation(...) as rec:`` (a thin wrapper
over :meth:`Client.start_generation`).

It provides:

- :func:`experiment` — a context manager that creates the run, and finalizes it
  on exit (``succeeded`` normally, ``failed`` on error, ``canceled`` on Ctrl-C).
- :class:`ExperimentRunner` — a thin loop over a dataset that invokes a user
  target and one or more user scorers, exporting scores per item.

The flow is generation-first and publishes continuously by default: create the
run, then for each item run the agent (exporting generations through
:meth:`ExperimentRun.start_generation`), grade locally, and export scores
attributed to the same ``run_id``. Grading is entirely user-supplied: a scorer
is any callable returning :class:`ScoreOutput` records.
"""

from __future__ import annotations

import copy
import hashlib
import secrets
from collections.abc import Callable, Iterator, Sequence
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import TYPE_CHECKING, Any, Literal

from .client import Client
from .errors import ScoreExportError
from .models import (
    CreateExperimentRequest,
    ExperimentReport,
    ExperimentStatus,
    GenerationStart,
    ScoreItem,
    ScoreSource,
    ScoreValue,
)

if TYPE_CHECKING:  # avoid importing the recorder at runtime (only used for typing)
    from .client import GenerationRecorder

# Tag and metadata keys carried on every generation a run records, so the Sigil
# experiment report / plugin UI can group generations by experiment.
EXPERIMENT_RUN_ID_TAG = "experiment.run_id"
EXPERIMENT_RUN_ID_METADATA_KEY = "experiment_run_id"

# Score metadata keys read by the Sigil experiment report / plugin UI.
RESERVED_METADATA_KEYS = (
    "dataset_id",
    "dataset_version",
    "item_id",
    "task_id",
    "task_category",
    "trial_id",
)

UploadMode = Literal["continuous", "bulk", "manual"]

# How to turn a saved conversation into a dataset item. ``user_prompt`` re-runs
# the agent from the conversation's initial user prompt (implemented). ``golden``
# will additionally keep the original answer as a reference for LLM-judge
# scoring (reserved; not implemented yet).
DatasetMode = Literal["user_prompt", "golden"]

# Message roles as recorded on conversation generations (proto enum names plus
# their lowercase forms, to be liberal in what we accept).
_USER_ROLES = frozenset({"MESSAGE_ROLE_USER", "user", "USER"})
_SYSTEM_ROLES = frozenset({"MESSAGE_ROLE_SYSTEM", "system", "SYSTEM"})

# Tag prefix linking an experiment run back to the collection its dataset came
# from. The plugin UI and `gcx` filter runs on this.
COLLECTION_ID_TAG_PREFIX = "collectionId:"


@dataclass(slots=True)
class DatasetItem:
    """A user-owned input plus stable identity for one experiment example."""

    id: str
    input: Any = None
    expected: Any = None
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class TargetResult:
    """Output of running the agent under test for one dataset item.

    ``generation_ids`` should list the Sigil generation ids produced while the
    agent ran (so scores can attach to them). When you record generations with
    :meth:`ExperimentRun.start_generation`, the run captures these ids for you
    and the runner fills them in automatically; callers may also set them
    explicitly (e.g. when recording generations by some other path).
    """

    output: Any = None
    generation_ids: list[str] = field(default_factory=list)
    conversation_id: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class ScoreOutput:
    """A single grading result produced by a user scorer."""

    evaluator_id: str
    evaluator_version: str
    score_key: str
    value: ScoreValue
    generation_id: str = ""
    passed: bool | None = None
    explanation: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class ExperimentResult:
    """Summary returned by :meth:`ExperimentRunner.run`."""

    run_id: str
    accepted_scores: int
    url: str
    report: ExperimentReport | None = None


def stable_id(prefix: str, *parts: Any) -> str:
    """Returns a deterministic id from ``parts`` for idempotent retries."""

    joined = "\x1f".join("" if p is None else str(p) for p in parts)
    digest = hashlib.sha1(joined.encode("utf-8")).hexdigest()[:16]
    return f"{prefix}-{digest}"


# Target and scorer callable signatures (documented as type aliases).
DatasetTarget = Callable[[DatasetItem, "ExperimentRun"], "TargetResult | None"]
DatasetScorer = Callable[[DatasetItem, TargetResult], "Sequence[ScoreOutput] | None"]


# --------------------------------------------------------------------------- #
# Building a dataset from an existing Sigil collection
# --------------------------------------------------------------------------- #


def _message_text(message: dict[str, Any]) -> str:
    """Concatenates the text parts of one conversation message."""

    parts = message.get("parts")
    if not isinstance(parts, list):
        return ""
    texts = [p.get("text", "") for p in parts if isinstance(p, dict) and isinstance(p.get("text"), str)]
    return "".join(texts).strip()


def _generation_sort_key(generation: dict[str, Any]) -> str:
    """Sort key picking the chronologically earliest generation (ISO-8601 sorts lexically)."""

    return str(generation.get("started_at") or generation.get("created_at") or "")


def initial_user_prompt(conversation: dict[str, Any]) -> str:
    """Returns the initial user prompt from a fetched conversation.

    Looks at the chronologically earliest generation and returns the text of its
    last user-role input message — i.e. the user turn that kicked the
    conversation off, skipping any leading system prompt (some agents record the
    system prompt as a user-role message, so we keep the *last* user message).
    Falls back to the first non-system message, then to ``""`` when nothing
    usable is present.
    """

    generations = conversation.get("generations")
    if not isinstance(generations, list) or not generations:
        return ""
    earliest = min(generations, key=_generation_sort_key)
    messages = earliest.get("input")
    if not isinstance(messages, list):
        return ""

    user_texts = [
        text
        for m in messages
        if isinstance(m, dict) and str(m.get("role") or "") in _USER_ROLES
        for text in (_message_text(m),)
        if text
    ]
    if user_texts:
        return user_texts[-1]

    for m in messages:
        if isinstance(m, dict) and str(m.get("role") or "") not in _SYSTEM_ROLES:
            text = _message_text(m)
            if text:
                return text
    return ""


def dataset_from_collection(
    client: Client,
    collection_id: str,
    *,
    mode: DatasetMode = "user_prompt",
    limit: int | None = None,
    skip_empty: bool = True,
) -> list[DatasetItem]:
    """Builds experiment :class:`DatasetItem`s from a Sigil collection.

    Lists the collection's saved conversations, fetches each one, and turns it
    into a dataset item keyed by its initial user prompt:

    - ``mode="user_prompt"`` (default, implemented): ``input`` is the
      conversation's initial user prompt, so the experiment target can re-run
      the agent from scratch on that prompt and you score the fresh answer.
      ``expected`` is left ``None``.
    - ``mode="golden"`` (reserved, not yet implemented): will additionally
      capture the original assistant answer as ``expected`` so it can be used as
      a reference in an LLM-as-judge scorer. Raises :class:`NotImplementedError`
      for now.

    Each item carries ``collection_id``, ``conversation_id``, ``saved_id`` and a
    ``task_id`` (defaulting to the saved/conversation id) in ``metadata`` so the
    Sigil report groups scores cleanly. ``limit`` caps how many members are
    pulled; ``skip_empty`` drops conversations with no recoverable user prompt.

    Pair the result with :class:`ExperimentRunner` (passing the same
    ``collection_id`` so the run links back to the collection)::

        ds = dataset_from_collection(client, collection_id)
        runner = ExperimentRunner(client=client, run_id=..., name=...,
                                  collection_id=collection_id)
        runner.run(ds, target, [scorer])
    """

    if mode == "golden":
        raise NotImplementedError(
            "sigil dataset: mode='golden' is not implemented yet; use mode='user_prompt' "
            "(the original answer will later be exposed as DatasetItem.expected)"
        )
    if mode != "user_prompt":
        raise ValueError(f"sigil dataset: unknown mode {mode!r}; expected 'user_prompt' or 'golden'")

    cid = (collection_id or "").strip()
    if cid == "":
        raise ValueError("sigil dataset: collection_id is required")

    members = client.list_collection_members(cid)
    if limit is not None:
        members = members[: max(limit, 0)]

    items: list[DatasetItem] = []
    for member in members:
        conversation_id = str(member.get("conversation_id") or "").strip()
        if conversation_id == "":
            continue
        saved_id = str(member.get("saved_id") or "").strip()
        conversation = client.get_conversation(conversation_id)
        prompt = initial_user_prompt(conversation)
        if skip_empty and prompt == "":
            continue
        item_id = saved_id or conversation_id
        metadata: dict[str, Any] = {
            "collection_id": cid,
            "conversation_id": conversation_id,
            "saved_id": saved_id,
            "task_id": item_id,
            "source": "collection",
        }
        name = str(member.get("name") or "").strip()
        if name:
            metadata["saved_name"] = name
        items.append(DatasetItem(id=item_id, input=prompt, metadata=metadata))
    return items


class ExperimentRun:
    """An open experiment run bound to a Sigil client and a ``run_id``.

    Obtain one via :func:`experiment`. Use :meth:`start_generation` to record a
    generation that is tagged with this run (so it shows up in the experiment),
    and :meth:`add_scores` to grade and publish scores for a completed item.
    """

    def __init__(
        self,
        *,
        client: Client,
        run_id: str,
        name: str,
        dataset: dict[str, Any] | None,
        candidate: dict[str, Any] | None,
        upload: UploadMode,
        agent_name: str = "",
        agent_version: str = "",
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
    ) -> None:
        self._client = client
        self.run_id = run_id
        self.name = name
        self._dataset = dataset or {}
        self._candidate = candidate or {}
        self._upload = upload
        self._agent_name = agent_name
        self._agent_version = agent_version
        self._extra_tags = dict(extra_tags or {})
        self._extra_metadata = dict(extra_metadata or {})
        self._buffer: list[ScoreItem] = []
        self._accepted = 0
        self._finalized = False
        self._recorders: list[GenerationRecorder] = []
        self._tracked_ids: list[str] = []
        self._active_conversation_id: str = ""

    # --- Generation recording --------------------------------------------- #

    def start_generation(self, start: GenerationStart, *, capture: bool = True) -> GenerationRecorder:
        """Starts a non-stream generation tagged with this experiment ``run_id``.

        Wraps :meth:`Client.start_generation`, injecting the experiment ``run_id``
        tag/metadata, this run's agent identity (when the start omits it), and a
        shared ``conversation_id`` so the generation and the scores you publish
        for the item link together in Sigil. Use it as a context manager:

        ``with run.start_generation(GenerationStart(...)) as rec: rec.set_result(...)``

        When ``capture`` is true (the default) the produced generation id is
        recorded into :attr:`produced_generation_ids` so scores attach to it
        automatically.
        """

        recorder = self._client.start_generation(self._prepare_generation(start))
        if capture:
            self._recorders.append(recorder)
        return recorder

    def start_streaming_generation(self, start: GenerationStart, *, capture: bool = True) -> GenerationRecorder:
        """Streaming counterpart to :meth:`start_generation`."""

        recorder = self._client.start_streaming_generation(self._prepare_generation(start))
        if capture:
            self._recorders.append(recorder)
        return recorder

    def track_generation_id(self, generation_id: str) -> None:
        """Manually records a generation id so scores can attach to it.

        Use this when you record a generation outside :meth:`start_generation`
        (for example through a provider wrapper) but still want the runner to
        attribute scores to it automatically.
        """

        generation_id = (generation_id or "").strip()
        if generation_id and generation_id not in self._tracked_ids:
            self._tracked_ids.append(generation_id)

    def reset_capture(self, *, conversation_id: str | None = None) -> str:
        """Clears captured generation ids and sets the active conversation id.

        The runner calls this once per dataset item so :attr:`produced_generation_ids`
        reflects only the current item. Returns the active conversation id (the
        one passed in, or empty to let the next :meth:`start_generation` mint one).
        """

        self._recorders = []
        self._tracked_ids = []
        self._active_conversation_id = (conversation_id or "").strip()
        return self._active_conversation_id

    @property
    def produced_generation_ids(self) -> list[str]:
        """Generation ids captured since the most recent :meth:`reset_capture`.

        Resolved from the recorders started via :meth:`start_generation` (after
        they end) plus any ids passed to :meth:`track_generation_id`.
        """

        ids: list[str] = []
        for recorder in self._recorders:
            generation = recorder.last_generation
            if generation is not None and generation.id and generation.id not in ids:
                ids.append(generation.id)
        for generation_id in self._tracked_ids:
            if generation_id not in ids:
                ids.append(generation_id)
        return ids

    @property
    def active_conversation_id(self) -> str:
        """The conversation id wired into the most recent generation.

        Used to keep the agent's generations and the exported scores on the same
        conversation so they link in Sigil.
        """

        return self._active_conversation_id

    # --- Scoring ----------------------------------------------------------- #

    def add_scores(
        self,
        scores: Sequence[ScoreOutput] | None,
        *,
        item: DatasetItem | None = None,
        generation_ids: Sequence[str] | None = None,
        conversation_id: str = "",
        trial_id: str | None = None,
    ) -> int:
        """Normalizes and publishes scores for one completed item.

        Flushes queued generations first so strict score ingest can find them.
        In ``continuous`` mode scores are exported immediately and the accepted
        count is returned; in ``bulk``/``manual`` mode they are buffered and the
        buffered count is returned (publish later with :meth:`publish`).
        """

        if not scores:
            return 0
        gen_ids = list(generation_ids if generation_ids is not None else self.produced_generation_ids)
        # Default to the active conversation id so scores link to the same
        # conversation as the generations they grade.
        conv_id = (conversation_id or self._active_conversation_id or "").strip()
        items = [self._build_score_item(s, item, gen_ids, conv_id, trial_id) for s in scores]

        if self._upload == "continuous":
            self._client.flush()
            response = self._client.export_scores(items)
            accepted = _accepted_or_raise(response)
            self._accepted += accepted
            return accepted
        self._buffer.extend(items)
        return len(items)

    def publish(self) -> int:
        """Flushes and exports any buffered scores (bulk/manual modes)."""

        if not self._buffer:
            return 0
        self._client.flush()
        response = self._client.export_scores(self._buffer)
        accepted = _accepted_or_raise(response)
        self._accepted += accepted
        self._buffer.clear()
        return accepted

    @property
    def accepted_scores(self) -> int:
        """Number of scores the server has accepted so far for this run."""

        return self._accepted

    @property
    def url(self) -> str:
        """Best-effort deep link to this experiment in the Sigil UI."""

        return self._client.experiment_url(self.run_id)

    def report(self) -> ExperimentReport:
        """Fetches the aggregated report for this run."""

        return self._client.get_experiment_report(self.run_id)

    def finalize(
        self,
        status: ExperimentStatus | str = ExperimentStatus.SUCCEEDED,
        *,
        error: str | None = None,
    ) -> None:
        """Marks the run terminal. Safe to call once; later calls are no-ops."""

        if self._finalized:
            return
        self._client.complete_experiment(self.run_id, status, score_count=self._accepted, error=error)
        self._finalized = True

    # --- internals --------------------------------------------------------- #

    def _prepare_generation(self, start: GenerationStart) -> GenerationStart:
        """Returns a copy of ``start`` tagged for this experiment run.

        Mutating a copy keeps the caller's ``GenerationStart`` untouched. The
        experiment ``run_id`` tag/metadata are authoritative (always set); caller
        values win over run-level ``extra_tags``/``extra_metadata``.
        """

        seed = copy.deepcopy(start)

        conv_id = (seed.conversation_id or self._active_conversation_id or "").strip()
        if conv_id == "":
            conv_id = stable_id("conv", self.run_id, secrets.token_hex(8))
        self._active_conversation_id = conv_id
        seed.conversation_id = conv_id

        seed.tags = {**self._extra_tags, **seed.tags, EXPERIMENT_RUN_ID_TAG: self.run_id}
        seed.metadata = {**self._extra_metadata, **seed.metadata, EXPERIMENT_RUN_ID_METADATA_KEY: self.run_id}

        if seed.agent_name == "" and self._agent_name:
            seed.agent_name = self._agent_name
        if seed.agent_version == "" and self._agent_version:
            seed.agent_version = self._agent_version
        return seed

    def _build_score_item(
        self,
        score: ScoreOutput,
        item: DatasetItem | None,
        generation_ids: list[str],
        conversation_id: str,
        trial_id: str | None,
    ) -> ScoreItem:
        generation_id = score.generation_id
        if generation_id == "":
            if len(generation_ids) == 1:
                generation_id = generation_ids[0]
            elif len(generation_ids) > 1:
                raise ValueError(
                    "sigil experiment: target produced multiple generations; "
                    f"scorer '{score.evaluator_id}' must set ScoreOutput.generation_id explicitly"
                )
        metadata = self._score_metadata(score, item, trial_id)
        score_id = stable_id(
            "score",
            self.run_id,
            item.id if item else "",
            generation_id,
            score.evaluator_id,
            score.evaluator_version,
            score.score_key,
            trial_id or "",
        )
        return ScoreItem(
            score_id=score_id,
            generation_id=generation_id,
            conversation_id=conversation_id,
            run_id=self.run_id,
            evaluator_id=score.evaluator_id,
            evaluator_version=score.evaluator_version,
            score_key=score.score_key,
            value=score.value,
            passed=score.passed,
            explanation=score.explanation,
            metadata=metadata,
            source=ScoreSource(kind="experiment", id=self.run_id),
        )

    def _score_metadata(
        self,
        score: ScoreOutput,
        item: DatasetItem | None,
        trial_id: str | None,
    ) -> dict[str, Any]:
        metadata: dict[str, Any] = {}
        if self._dataset.get("id"):
            metadata["dataset_id"] = self._dataset["id"]
        if self._dataset.get("version"):
            metadata["dataset_version"] = self._dataset["version"]
        if self._candidate:
            metadata["candidate"] = dict(self._candidate)
        if item is not None:
            metadata["item_id"] = item.id
            metadata.update(item.metadata)
        if trial_id is not None:
            metadata["trial_id"] = trial_id
        metadata.update(score.metadata)
        return metadata


@contextmanager
def experiment(
    *,
    client: Client,
    run_id: str,
    name: str,
    description: str = "",
    tags: list[str] | None = None,
    metadata: dict[str, Any] | None = None,
    dataset: dict[str, Any] | None = None,
    candidate: dict[str, Any] | None = None,
    upload: UploadMode = "continuous",
    print_url: bool = True,
    agent_name: str = "",
    agent_version: str = "",
    extra_tags: dict[str, str] | None = None,
    extra_metadata: dict[str, Any] | None = None,
    collection_id: str = "",
) -> Iterator[ExperimentRun]:
    """Opens an external experiment run and finalizes it on exit.

    On normal exit the run is finalized ``succeeded`` (buffered scores are
    published first in ``bulk`` mode). On an exception the run is finalized
    ``failed``; on ``KeyboardInterrupt`` it is ``canceled``. In ``manual`` mode
    the run is left open on success so the caller can inspect, then call
    :meth:`ExperimentRun.publish` and :meth:`ExperimentRun.finalize` themselves.

    ``agent_name``/``agent_version`` and ``extra_tags``/``extra_metadata`` are
    applied to every generation recorded via :meth:`ExperimentRun.start_generation`.

    When ``collection_id`` is set (e.g. the collection a
    :func:`dataset_from_collection` dataset came from), the run is linked to that
    collection and a ``collectionId:<id>`` tag is added so it can be filtered in
    the Sigil UI / ``gcx``.
    """

    cid = (collection_id or "").strip()
    run_tags = list(tags or [])
    if cid:
        collection_tag = f"{COLLECTION_ID_TAG_PREFIX}{cid}"
        if collection_tag not in run_tags:
            run_tags.append(collection_tag)
    create_metadata = _run_metadata(metadata, dataset, candidate)
    # Also stamp the collection id into metadata. The dedicated collection_id
    # field and the collectionId tag are sent too, but some backends don't yet
    # persist those columns — metadata is durable, so the link survives there.
    if cid:
        create_metadata.setdefault("collection_id", cid)
    client.create_experiment(
        CreateExperimentRequest(
            run_id=run_id,
            name=name,
            source="external",
            description=description,
            tags=run_tags,
            collection_id=cid,
            metadata=create_metadata,
        )
    )
    run = ExperimentRun(
        client=client,
        run_id=run_id,
        name=name,
        dataset=dataset,
        candidate=candidate,
        upload=upload,
        agent_name=agent_name,
        agent_version=agent_version,
        extra_tags=extra_tags,
        extra_metadata=extra_metadata,
    )
    try:
        yield run
    except KeyboardInterrupt:
        _safe(lambda: client.cancel_experiment(run_id))
        raise
    except BaseException as exc:  # noqa: BLE001 - finalize then re-raise
        error_text = str(exc)
        _safe(lambda: run.finalize(ExperimentStatus.FAILED, error=error_text))
        raise
    else:
        if upload == "manual":
            if print_url:
                print(
                    f"[sigil] experiment '{run_id}' left open (manual mode): "
                    f"{len(run._buffer)} score(s) buffered. "
                    "Call run.publish() then run.finalize() to upload."
                )
            return
        run.publish()
        run.finalize(ExperimentStatus.SUCCEEDED)
        if print_url:
            print(f"[sigil] experiment '{run_id}' finished ({run.accepted_scores} scores): {run.url}")


class ExperimentRunner:
    """Runs an agent over a dataset and publishes scores under one run.

    A/B testing is two runners with different ``run_id``/``tags`` over the same
    items and scorers. Concurrency is fixed to 1 in this first iteration.
    """

    def __init__(
        self,
        *,
        client: Client,
        run_id: str,
        name: str,
        description: str = "",
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        dataset: dict[str, Any] | None = None,
        candidate: dict[str, Any] | None = None,
        upload: UploadMode = "continuous",
        print_url: bool = True,
        fetch_report: bool = True,
        agent_name: str = "",
        agent_version: str = "",
        extra_tags: dict[str, str] | None = None,
        extra_metadata: dict[str, Any] | None = None,
        collection_id: str = "",
    ) -> None:
        self._client = client
        self._run_id = run_id
        self._name = name
        self._description = description
        self._tags = list(tags or [])
        self._metadata = dict(metadata or {})
        self._dataset = dict(dataset or {})
        self._candidate = dict(candidate or {})
        self._upload = upload
        self._print_url = print_url
        self._fetch_report = fetch_report
        self._agent_name = agent_name
        self._agent_version = agent_version
        self._extra_tags = dict(extra_tags or {})
        self._extra_metadata = dict(extra_metadata or {})
        self._collection_id = collection_id

    def run(
        self,
        items: Sequence[DatasetItem],
        target: DatasetTarget,
        scorers: Sequence[DatasetScorer],
    ) -> ExperimentResult:
        """Executes ``target`` for each item, grades with ``scorers``, publishes scores."""

        with experiment(
            client=self._client,
            run_id=self._run_id,
            name=self._name,
            description=self._description,
            tags=self._tags,
            metadata=self._metadata,
            dataset=self._dataset,
            candidate=self._candidate,
            upload=self._upload,
            print_url=self._print_url,
            agent_name=self._agent_name,
            agent_version=self._agent_version,
            extra_tags=self._extra_tags,
            extra_metadata=self._extra_metadata,
            collection_id=self._collection_id,
        ) as run:
            for item in items:
                # Assign one stable conversation id per item before running the
                # target and reset the capture sink. start_generation() picks the
                # conversation id up and tags the agent's generations with it; the
                # scores below reuse it, so generations and scores share a
                # conversation and link in the UI.
                run.reset_capture(conversation_id=stable_id("conv", run.run_id, item.id))
                result = target(item, run)
                if result is None:
                    result = TargetResult()
                # Fall back to ids captured by the run when the target did not
                # report them explicitly.
                generation_ids = result.generation_ids or run.produced_generation_ids
                outputs: list[ScoreOutput] = []
                for scorer in scorers:
                    produced = scorer(item, result)
                    if produced:
                        outputs.extend(produced)
                run.add_scores(
                    outputs,
                    item=item,
                    generation_ids=generation_ids,
                    conversation_id=result.conversation_id or run.active_conversation_id,
                )

        report = None
        if self._fetch_report:
            report = _safe(run.report)
        return ExperimentResult(
            run_id=run.run_id,
            accepted_scores=run.accepted_scores,
            url=run.url,
            report=report,
        )


def _run_metadata(
    metadata: dict[str, Any] | None,
    dataset: dict[str, Any] | None,
    candidate: dict[str, Any] | None,
) -> dict[str, Any]:
    out: dict[str, Any] = dict(metadata or {})
    if dataset:
        if dataset.get("id"):
            out.setdefault("dataset_id", dataset["id"])
        if dataset.get("version"):
            out.setdefault("dataset_version", dataset["version"])
        if dataset.get("uri"):
            out.setdefault("dataset_uri", dataset["uri"])
    if candidate:
        out.setdefault("candidate", dict(candidate))
    out.setdefault("created_at", datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"))
    return out


def _safe(fn: Callable[[], Any]) -> Any:
    """Runs ``fn`` and swallows exceptions (used on finalize/cancel paths)."""

    try:
        return fn()
    except Exception:  # noqa: BLE001 - best-effort finalize/cancel/report
        return None


def _accepted_or_raise(response: Any) -> int:
    rejected = getattr(response, "rejected", [])
    if rejected:
        details = "; ".join(f"{r.score_id}: {r.error or 'rejected'}" for r in rejected)
        raise ScoreExportError(f"sigil score export rejected {len(rejected)} score(s): {details}")
    return int(getattr(response, "accepted_count", 0))

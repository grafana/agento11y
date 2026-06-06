"""Tests for building experiment datasets from Sigil collections."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest
from sigil_sdk import (
    DatasetItem,
    ExperimentRunner,
    ExportScoresResponse,
    TargetResult,
    dataset_from_collection,
    initial_user_prompt,
)


def _gen(role_texts: list[tuple[str, str]], *, started_at: str = "") -> dict[str, Any]:
    """Builds a generation dict with the given (role, text) input messages."""

    return {
        "started_at": started_at,
        "input": [{"role": role, "parts": [{"text": text}]} for role, text in role_texts],
    }


# --------------------------------------------------------------------------- #
# initial_user_prompt
# --------------------------------------------------------------------------- #


def test_initial_user_prompt_skips_system_recorded_as_user() -> None:
    # The Sir Alex agent records its system prompt as a second USER message; the
    # real prompt is the last user-role message.
    conversation = {
        "generations": [
            _gen(
                [
                    ("MESSAGE_ROLE_USER", "You are Sir Alex, an FPL assistant..."),
                    ("MESSAGE_ROLE_USER", "Who should I captain this week?"),
                ]
            )
        ]
    }
    assert initial_user_prompt(conversation) == "Who should I captain this week?"


def test_initial_user_prompt_handles_system_role() -> None:
    conversation = {
        "generations": [
            _gen(
                [
                    ("MESSAGE_ROLE_SYSTEM", "system prompt"),
                    ("MESSAGE_ROLE_USER", "Compare Salah and Palmer."),
                ]
            )
        ]
    }
    assert initial_user_prompt(conversation) == "Compare Salah and Palmer."


def test_initial_user_prompt_picks_earliest_generation() -> None:
    conversation = {
        "generations": [
            _gen([("MESSAGE_ROLE_USER", "second turn")], started_at="2026-06-03T18:00:00Z"),
            _gen([("MESSAGE_ROLE_USER", "first turn")], started_at="2026-06-03T17:00:00Z"),
        ]
    }
    assert initial_user_prompt(conversation) == "first turn"


def test_initial_user_prompt_empty_when_no_user_message() -> None:
    assert initial_user_prompt({"generations": []}) == ""
    assert initial_user_prompt({}) == ""
    assert initial_user_prompt({"generations": [_gen([("MESSAGE_ROLE_SYSTEM", "only system")])]}) == ""


# --------------------------------------------------------------------------- #
# dataset_from_collection
# --------------------------------------------------------------------------- #


@dataclass
class _FakeReadClient:
    """Serves canned collection members and conversations."""

    members: list[dict[str, Any]] = field(default_factory=list)
    conversations: dict[str, dict[str, Any]] = field(default_factory=dict)
    member_calls: list[str] = field(default_factory=list)
    conversation_calls: list[str] = field(default_factory=list)

    def list_collection_members(self, collection_id: str) -> list[dict[str, Any]]:
        self.member_calls.append(collection_id)
        return list(self.members)

    def get_conversation(self, conversation_id: str) -> dict[str, Any]:
        self.conversation_calls.append(conversation_id)
        return self.conversations.get(conversation_id, {})

    # Unused control-plane surface for the runner test below.
    created: list[Any] = field(default_factory=list)

    def create_experiment(self, request):
        self.created.append(request)
        return request

    def flush(self):
        return

    def export_scores(self, scores):
        return ExportScoresResponse(results=[])

    def complete_experiment(self, run_id, status, *, score_count=None, error=None, metadata=None):
        return

    def cancel_experiment(self, run_id):
        return

    def experiment_url(self, run_id):
        return f"https://sigil.example/exp/{run_id}"


def test_dataset_from_collection_builds_items() -> None:
    client = _FakeReadClient(
        members=[
            {"saved_id": "saved-conv-1", "conversation_id": "conv-1", "name": "captain"},
            {"saved_id": "saved-conv-2", "conversation_id": "conv-2", "name": "compare"},
        ],
        conversations={
            "conv-1": {"generations": [_gen([("MESSAGE_ROLE_USER", "Captain pick?")])]},
            "conv-2": {"generations": [_gen([("MESSAGE_ROLE_USER", "Salah or Palmer?")])]},
        },
    )

    items = dataset_from_collection(client, "coll-123")

    assert [i.id for i in items] == ["saved-conv-1", "saved-conv-2"]
    assert [i.input for i in items] == ["Captain pick?", "Salah or Palmer?"]
    assert items[0].expected is None
    assert items[0].metadata == {
        "collection_id": "coll-123",
        "conversation_id": "conv-1",
        "saved_id": "saved-conv-1",
        "task_id": "saved-conv-1",
        "source": "collection",
        "saved_name": "captain",
    }
    assert client.member_calls == ["coll-123"]
    assert client.conversation_calls == ["conv-1", "conv-2"]


def test_dataset_from_collection_skips_empty_prompts() -> None:
    client = _FakeReadClient(
        members=[
            {"saved_id": "s1", "conversation_id": "conv-1"},
            {"saved_id": "s2", "conversation_id": "conv-2"},
        ],
        conversations={
            "conv-1": {"generations": [_gen([("MESSAGE_ROLE_SYSTEM", "no user msg")])]},
            "conv-2": {"generations": [_gen([("MESSAGE_ROLE_USER", "real prompt")])]},
        },
    )

    items = dataset_from_collection(client, "coll-123")
    assert [i.id for i in items] == ["s2"]

    # skip_empty=False keeps the empty-prompt item.
    items_all = dataset_from_collection(client, "coll-123", skip_empty=False)
    assert [i.id for i in items_all] == ["s1", "s2"]


def test_dataset_from_collection_respects_limit() -> None:
    client = _FakeReadClient(
        members=[{"saved_id": f"s{i}", "conversation_id": f"conv-{i}"} for i in range(5)],
        conversations={f"conv-{i}": {"generations": [_gen([("MESSAGE_ROLE_USER", f"q{i}")])]} for i in range(5)},
    )
    items = dataset_from_collection(client, "coll-123", limit=2)
    assert [i.id for i in items] == ["s0", "s1"]
    # Only fetched the conversations we kept.
    assert client.conversation_calls == ["conv-0", "conv-1"]


def test_dataset_from_collection_golden_not_implemented() -> None:
    client = _FakeReadClient()
    with pytest.raises(NotImplementedError):
        dataset_from_collection(client, "coll-123", mode="golden")


def test_dataset_from_collection_requires_collection_id() -> None:
    with pytest.raises(ValueError):
        dataset_from_collection(_FakeReadClient(), "  ")


# --------------------------------------------------------------------------- #
# ExperimentRunner collection linkage
# --------------------------------------------------------------------------- #


def test_runner_adds_collection_id_tag_and_links_run() -> None:
    client = _FakeReadClient()
    runner = ExperimentRunner(
        client=client,
        run_id="run-1",
        name="collection run",
        tags=["ci"],
        collection_id="coll-123",
        fetch_report=False,
        print_url=False,
    )

    def target(item: DatasetItem, run) -> TargetResult:
        return TargetResult(output="ok")

    runner.run([DatasetItem(id="s1", input="hi")], target, [])

    assert len(client.created) == 1
    request = client.created[0]
    assert request.collection_id == "coll-123"
    assert "collectionId:coll-123" in request.tags
    assert "ci" in request.tags
    # Durable fallback for backends that don't persist the tag/collection_id columns.
    assert request.metadata.get("collection_id") == "coll-123"

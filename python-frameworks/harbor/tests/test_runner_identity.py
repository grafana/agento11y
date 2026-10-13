"""Original conversation IDs must take precedence over ATIF fallback ingestion."""

import pytest
from agento11y_harbor.integration import _conversation_id


def context(conversation_id):
    return {"metadata": {"agento11y.conversation_id": conversation_id}}


@pytest.mark.parametrize(
    ("result", "expected"),
    [
        ({}, ""),
        ({"agent_result": context("original")}, "original"),
        ({"step_results": [{"agent_result": context("original")}]}, "original"),
        ({"agent_result": {}, "step_results": [{"agent_result": context("original")}]}, "original"),
        (
            {"agent_result": {"n_input_tokens": 5}, "step_results": [{"agent_result": context("original")}]},
            "original",
        ),
        (
            {"agent_result": context(" original "), "step_results": [{"agent_result": context("original")}]},
            "original",
        ),
        (
            {
                "agent_result": context(""),
                "step_results": [{"agent_result": None}, {"agent_result": context("original")}],
            },
            "original",
        ),
    ],
)
def test_original_conversation_across_native_result_shapes(result, expected):
    assert _conversation_id(result) == expected


@pytest.mark.parametrize(
    "result",
    [
        {"agent_result": context("top"), "step_results": [{"agent_result": context("different")}]},
        {"step_results": [{"agent_result": context("first")}, {"agent_result": context("second")}]},
        {
            "agent_result": context("original"),
            "step_results": [{"agent_result": context("original")}, {"agent_result": context("different")}],
        },
    ],
)
def test_conflicting_original_ids_fail_before_fallback(result):
    with pytest.raises(ValueError, match="inconsistent"):
        _conversation_id(result)

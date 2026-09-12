#!/usr/bin/env python3
"""Compare a Claude result with stored experiments through gcx (stdlib only)."""

import argparse
import base64
import json
import math
import subprocess
from pathlib import Path


def gcx(context, *command):
    return json.loads(subprocess.check_output(["gcx", "--context", context, *command, "-o", "json"], text=True))


def trace_spans(value):
    if isinstance(value, dict):
        if "spanId" in value:
            yield value
        else:
            for child in value.values():
                yield from trace_spans(child)
    elif isinstance(value, list):
        for child in value:
            yield from trace_spans(child)


def hex_id(value, width):
    if len(value) == width and all(c in "0123456789abcdefABCDEF" for c in value):
        return value.lower()
    return base64.b64decode(value).hex()


def verify_trajectory(args, original, actual, include_content):
    assert args.trace_root and args.tempo_datasource, (
        "Trajectory verification requires --trace-root and --tempo-datasource"
    )
    root = args.trace_root.resolve()
    path = Path(original["tracePath"])
    path = (path if path.is_absolute() else root / path).resolve()
    assert path.is_relative_to(root) and path.name == "trace.jsonl" and path.parent.name == "out"
    assert path.is_file() and path.stat().st_size <= 32 * 1024 * 1024
    events = [json.loads(line) for line in path.read_text().splitlines()]
    terminal = [event for event in events if event["type"] == "result"][-1]
    usage = terminal["usage"]
    expected_usage = {
        "input_tokens": usage["input_tokens"]
        + usage.get("cache_read_input_tokens", 0)
        + usage.get("cache_creation_input_tokens", 0),
        "output_tokens": usage["output_tokens"],
        "cache_read_input_tokens": usage.get("cache_read_input_tokens", 0),
        "cache_write_input_tokens": usage.get("cache_creation_input_tokens", 0),
    }
    trial = actual["trial"]
    assert trial["conversation_id"] == terminal["session_id"]
    assert trial["input_tokens"] == expected_usage["input_tokens"]
    assert trial["output_tokens"] == expected_usage["output_tokens"]
    assert trial["total_tokens"] == trial["input_tokens"] + trial["output_tokens"]
    conversation = gcx(args.context, "agento11y", "conversations", "get", trial["conversation_id"])
    generation_ids = {score["generation_id"] for score in actual["scores"]}
    assert len(generation_ids) == 1
    generations = [g for g in conversation["generations"] if g["generation_id"] in generation_ids]
    assert len(generations) == 1, "Missing or duplicate invocation generation"
    generation = generations[0]
    assert generation["trace_id"] == trial["trace_id"] and generation["span_id"] == trial["span_id"]
    assert generation["operation_name"] == "invoke_agent"
    assert generation["tags"]["trial_id"] == trial["trial_id"]
    assert generation["usage"]["input_semantics"] == "TOKEN_INPUT_SEMANTICS_INCLUSIVE"
    for key, value in expected_usage.items():
        assert int(generation["usage"].get(key, 0)) == value, key
    for score in actual["scores"]:
        assert score["conversation_id"] == trial["conversation_id"]
        assert score["trace_id"] == trial["trace_id"] and score["span_id"] == trial["span_id"]
    messages = json.dumps(generation.get("input", []) + generation.get("output", []))
    model_calls, tool_calls, tool_results = set(), {}, {}
    for event in events:
        if event["type"] not in ("assistant", "user") or event.get("message", {}).get("model") == "<synthetic>":
            continue
        message = event["message"]
        if event["type"] == "assistant":
            model_calls.add((event.get("parent_tool_use_id"), message["id"]))
        content = message["content"]
        if isinstance(content, str):
            if include_content:
                assert json.dumps(content)[1:-1] in messages
            continue
        for block in content:
            if block["type"] == "tool_use":
                tool_calls[block["id"]] = block["input"]
                if include_content:
                    assert block["id"] in messages
            text = block.get("text") or block.get("thinking")
            if block["type"] == "tool_result":
                tool_results[block["tool_use_id"]] = block.get("content")
                if isinstance(block.get("content"), str):
                    text = block["content"]
            if include_content and text:
                assert json.dumps(text)[1:-1] in messages, "Stored conversation lost visible content"
    trace = gcx(args.context, "traces", "get", trial["trace_id"], "-d", args.tempo_datasource)
    spans = list(trace_spans(trace))
    assert len(spans) == 1 + len(model_calls) + len(tool_calls), "Missing/duplicate invocation, model or tool spans"
    span_ids = {hex_id(span["spanId"], 16) for span in spans}
    assert len(span_ids) == len(spans)
    root_span = next(span for span in spans if hex_id(span["spanId"], 16) == trial["span_id"])
    root_attrs = {a["key"]: next(iter(a["value"].values())) for a in root_span["attributes"]}
    assert root_attrs["agento11y.generation.id"] == generation["generation_id"]
    for key, value in expected_usage.items():
        assert int(root_attrs.get("gen_ai.usage." + key, 0)) == value
    for span in spans:
        assert hex_id(span["traceId"], 32) == trial["trace_id"]
        assert int(span["endTimeUnixNano"]) >= int(span["startTimeUnixNano"])
        attrs = {a["key"]: next(iter(a["value"].values())) for a in span["attributes"]}
        assert attrs["gen_ai.conversation.id"] == trial["conversation_id"]
        assert attrs["trial_id"] == trial["trial_id"]
        if attrs["gen_ai.operation.name"] == "execute_tool":
            tool_id = attrs["gen_ai.tool.call.id"]
            assert tool_id in tool_calls
            if include_content:
                assert json.loads(attrs["gen_ai.tool.call.arguments"]) == tool_calls[tool_id]
                if tool_id in tool_results:
                    assert json.loads(attrs["gen_ai.tool.call.result"]) == tool_results[tool_id]
            else:
                assert "gen_ai.tool.call.arguments" not in attrs and "gen_ai.tool.call.result" not in attrs
        if span is not root_span:
            assert hex_id(span["parentSpanId"], 16) in span_ids
            assert not any(key.startswith("gen_ai.usage.") for key in attrs), (
                "Aggregate usage was duplicated onto child calls"
            )
    return {
        "conversation_id": trial["conversation_id"],
        "trace_id": trial["trace_id"],
        "generation_id": generation["generation_id"],
        "spans": len(spans),
        **expected_usage,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("results", type=Path)
    parser.add_argument("comparison_id", help="comparison_id from the import --dry-run JSON")
    parser.add_argument("--context", required=True, help="explicit gcx context")
    parser.add_argument("--trace-root", type=Path, help="authorized retained-trace directory (same as import)")
    parser.add_argument("--tempo-datasource", help="Tempo datasource UID for actual trace retrieval")
    args = parser.parse_args()
    source = json.loads(args.results.read_text())
    summary = {"comparison_id": args.comparison_id, "runs": []}
    case_means = {}
    for arm in ("with", "without"):
        expected = {
            (case["name"], i + 1): attempt
            for case in source["cases"]
            for i, attempt in enumerate(case["arms"].get(arm, []))
        }
        if not expected:
            continue
        run_id = f"{args.comparison_id}-{arm}"
        # Use the raw resource endpoint: older typed gcx report models turn
        # missing token totals into zero and omit coverage/denominator fields.
        report = json.loads(
            subprocess.check_output(
                [
                    "gcx",
                    "--context",
                    args.context,
                    "api",
                    f"/api/plugins/grafana-agento11y-app/resources/eval/experiments/{run_id}/report",
                    "-o",
                    "json",
                ],
                text=True,
            )
        )
        run = report["experiment"]
        incomplete = source.get("partial", False) or any(
            attempt.get("skippedPaidGraders", False) for attempt in expected.values()
        )
        assert run["status"] == ("failed" if incomplete else "completed"), run["status"]
        assert run["metadata"]["comparison_id"] == args.comparison_id
        stored = {
            (row["test_case_snapshot"]["name"], trial["trial"]["attempt"]): trial
            for row in report["rows"]
            for trial in row["trials"]
        }
        assert stored.keys() == expected.keys(), "Missing or extra trials"
        score_count = 0
        trajectories = []
        means = {}
        for key, original in expected.items():
            actual = stored[key]
            scores = {s["score_key"]: s for s in actual["scores"]}
            headline_key = "claude_score_incomplete" if original.get("skippedPaidGraders") else "final"
            headline = scores[headline_key]
            assert math.isclose(headline["value"]["number"], original["score"])
            assert headline["passed"] == original["passed"]
            assert (actual.get("final_score") is None) == bool(original.get("skippedPaidGraders"))
            assert len(scores) == 1 + len(original["graders"])
            for grader in original["graders"]:
                score = scores[f"grader.{grader['name']}"]
                assert score["value"]["bool"] == grader["passed"]
                assert score["metadata"]["scored"] == grader["scored"]
                assert score["metadata"]["weight"] == grader["weight"]
                if run["metadata"]["include_content"]:
                    assert score.get("explanation", "") == grader.get("explanation", "")
                    assert score["metadata"].get("judge_votes") == grader.get("judgeVotes")
            if original.get("costUsd") is not None:
                assert math.isclose(actual["trial"]["cost"], original["costUsd"])
            if run["metadata"].get("include_trajectory"):
                trajectories.append(verify_trajectory(args, original, actual, run["metadata"]["include_content"]))
            score_count += len(scores)
            means.setdefault(key[0], []).append(headline["value"]["number"])
        assert report["summary"]["trial_count"] == len(expected)
        if trajectories:
            assert len({t["conversation_id"] for t in trajectories}) == len(expected)
            assert len({t["trace_id"] for t in trajectories}) == len(expected)
            assert report["summary"]["token_coverage"] == "complete"
            assert report["summary"]["total_tokens"] == sum(
                t["input_tokens"] + t["output_tokens"] for t in trajectories
            )
        listed_scores = json.loads(
            subprocess.check_output(
                [
                    "gcx",
                    "--context",
                    args.context,
                    "agento11y",
                    "experiments",
                    "list-scores",
                    run_id,
                    "--limit",
                    str(score_count + 1),
                    "-o",
                    "json",
                ],
                text=True,
            )
        )
        assert len(listed_scores) == score_count
        assert len({score["score_id"] for score in listed_scores}) == score_count
        case_means[arm] = {name: sum(values) / len(values) for name, values in means.items()}
        suite_score = sum(case_means[arm].values()) / len(case_means[arm])
        summary["runs"].append(
            {
                "experiment_id": run_id,
                "status": run["status"],
                "arm": arm,
                "trials": len(expected),
                "scores": score_count,
                "mean_case_score": suite_score,
                "trial_weighted_score": report["summary"].get("final_score_avg"),
                "cost_usd": report["summary"].get("total_cost"),
                "token_coverage": report["summary"].get("token_coverage"),
                "total_tokens": report["summary"].get("total_tokens"),
                "retrieved_conversations": len(trajectories),
                "retrieved_traces": len(trajectories),
                "retrieved_spans": sum(t["spans"] for t in trajectories),
                "trajectories": trajectories,
                "cases": case_means[arm],
            }
        )
    if "with" in case_means and "without" in case_means and not source.get("partial"):
        deltas = [case_means["with"][name] - case_means["without"][name] for name in case_means["with"]]
        summary["mean_delta"] = sum(deltas) / len(deltas)
        assert math.isclose(summary["mean_delta"], source["aggregates"]["meanDelta"])
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()

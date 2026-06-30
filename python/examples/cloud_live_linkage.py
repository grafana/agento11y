"""Prove the live cloud-experiment linkage: a trial's conversation is real.

This mirrors how a harness should instrument *as it runs*: the agent ingests its
transcript as a generation under a conversation id, and the trial binds that same
conversation when it records scores. The result is an "Open conversation" link in
the experiments UI that actually resolves — unlike a post-hoc publish that invents
a conversation id with no backing generation.

Set SIGIL_ENDPOINT (your Grafana Cloud Sigil URL), SIGIL_AUTH_TOKEN (your
ingestion API key), and optionally SIGIL_AUTH_TENANT_ID (your stack id), then run:

    python examples/cloud_live_linkage.py
"""

from __future__ import annotations

import base64
import json
import os
import time
import urllib.request

from sigil_sdk.experiments import Client, Evaluator, Experiment, TestCase, TestSuite

ENDPOINT = os.environ["SIGIL_ENDPOINT"]
TENANT = os.environ.get("SIGIL_AUTH_TENANT_ID", "")
TOKEN = os.environ["SIGIL_AUTH_TOKEN"]


def get_conversation(conversation_id: str) -> dict:
    headers = {"Authorization": f"Bearer {TOKEN}"}
    if TENANT:
        headers = {
            "Authorization": "Basic " + base64.b64encode(f"{TENANT}:{TOKEN}".encode()).decode(),
            "X-Scope-OrgID": TENANT,
        }
    req = urllib.request.Request(
        f"{ENDPOINT}/api/v1/conversations/{conversation_id}",
        headers=headers,
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read().decode())


def main() -> None:
    exp_id = f"live-linkage-{int(time.time())}"
    client = Client(ENDPOINT, tenant_id=TENANT, ingest_token=TOKEN, actor="cloud-linkage")
    suite = TestSuite(
        suite_id="live-linkage",
        name="Live Linkage",
        test_cases=[TestCase(test_case_id="weather", input="weather in Paris?", expected="rain")],
    )
    verifier = Evaluator(evaluator_id="exact", version="1", kind="deterministic")

    with Experiment(client, experiment_id=exp_id, name="live linkage", suite=suite) as exp:
        case = suite.test_cases[0]
        with exp.trial(case) as trial:
            # The "agent" runs and records its real transcript as a generation under
            # the trial's conversation. record_io makes the trial export exactly that
            # generation, so trial.conversation_id is a real, openable conversation.
            answer = "It is raining in Paris."
            trial.record_io(
                input=case.input,
                output=answer,
                model_provider="anthropic",
                model_name="claude-haiku-4-5",
                input_tokens=12,
                output_tokens=8,
            )
            trial.final_score(1.0, passed=True, explanation="matched", evaluator=verifier)

        conversation_id = trial.conversation_id
        # Flush the generation queue so the conversation is queryable.
        client.core.flush()

    print(f"experiment_id:   {exp_id}")
    print(f"conversation_id: {conversation_id}")

    # The link the UI offers must resolve: fetch the conversation and confirm it is
    # backed by the generation the trial recorded.
    conv = get_conversation(conversation_id)
    generations = conv.get("generations") or []
    print(f"conversation resolved: {bool(conv)} | generations: {len(generations)}")
    if not generations:
        raise SystemExit("FAIL: conversation has no backing generations — link would 404")
    print("OK: trial.conversation_id points at a real, openable conversation")


if __name__ == "__main__":
    main()

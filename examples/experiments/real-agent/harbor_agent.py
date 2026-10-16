"""A real hosted Harbor agent using the same instrumented Anthropic tool loop."""

import json
import shlex
import sys
import uuid

from agent import CASES, HERE, client, run_agent
from agento11y.experiments import TrialRef
from harbor.agents.base import BaseAgent


class LedgerAgent(BaseAgent):
    @staticmethod
    def name():
        return "order-ledger-agent"

    def version(self):
        return "1"

    async def setup(self, environment):
        pass

    async def run(self, instruction, environment, context):
        # run_harbor passes a trial ref; under `agento11y harbor --` the plugin
        # links the conversation from context.metadata instead.
        ref = TrialRef.from_env(self.extra_env)
        run_id = ref.experiment_id if ref else "harbor"
        case_id = ref.test_case_id if ref else str(self.context_id or uuid.uuid4())
        sdk = client()

        async def tool(name, args):
            if name == "read_orders":
                command = "cat /app/orders.json"
            else:
                command = "printf %s " + shlex.quote(args["answer"]) + " > /app/answer.txt"
            response = await environment.exec(command)
            if response.return_code != 0:
                raise RuntimeError(response.stderr)
            return response.stdout or "Answer saved"

        def record(result):
            # Called on failure and timeout too, so Harbor keeps the completed turns.
            context.n_input_tokens = result["input_tokens"]
            context.n_output_tokens = result["output_tokens"]
            context.n_cache_tokens = 0
            context.cost_usd = result["cost"]
            context.metadata = {
                "agento11y.conversation_id": result["conversation_id"],
                "cost_basis": result["cost_basis"],
            }
            try:
                (self.logs_dir / "provider-responses.json").write_text(json.dumps(result, indent=2))
            except OSError as exc:
                print(f"could not write provider-responses.json: {exc}", file=sys.stderr, flush=True)

        try:
            await run_agent(instruction, self.model_name, run_id, case_id, sdk, tool=tool, on_result=record)
        finally:
            sdk.shutdown()


def prepare_tasks(root):
    for case in CASES:
        task = root / case["id"]
        for directory in ("environment", "tests"):
            (task / directory).mkdir(parents=True, exist_ok=True)
        (task / "instruction.md").write_text(case["question"])
        (task / "environment/orders.json").write_text((HERE / "orders.json").read_text())
        (task / "environment/Dockerfile").write_text(
            "FROM python:3.13-slim\nWORKDIR /app\nCOPY orders.json /app/orders.json\n"
        )
        (task / "task.toml").write_text(
            f'schema_version = "1.4"\n[task]\nname = "ledger/{case["id"]}"\nversion = "1.0.0"\n'
            "[agent]\ntimeout_sec = 180.0\n[verifier]\ntimeout_sec = 30.0\n"
            "[environment]\nbuild_timeout_sec = 180.0\ncpus = 1\nmemory_mb = 512\nstorage_mb = 1024\n"
        )
        (task / "tests/test.sh").write_text(
            "#!/bin/sh\nmkdir -p /logs/verifier\n"
            f'if [ "$(cat /app/answer.txt 2>/dev/null)" = "{case["expected"]}" ]; then\n'
            "  echo 1 > /logs/verifier/reward.txt\nelse\n  echo 0 > /logs/verifier/reward.txt\nfi\n"
        )
    return root

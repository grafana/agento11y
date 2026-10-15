"""Run your agent against a published suite version and its pinned evaluators.

Set AGENTO11Y_AGENT_MODULE to an importable module that exports
run_agent(prompt) -> AgentOutput, with flush set to the flush of the client
that instruments your agent.
"""

from __future__ import annotations

import importlib
import os

from agento11y.experiments import run_checkpoint


def main() -> None:
    agent = importlib.import_module(os.environ["AGENTO11Y_AGENT_MODULE"])
    run = run_checkpoint(
        os.environ["AGENTO11Y_SUITE_ID"],
        os.environ["AGENTO11Y_SUITE_VERSION"],
        agent.run_agent,
        experiment_id=os.environ.get("AGENTO11Y_EXPERIMENT_ID", ""),
        candidate={
            "agent_name": os.environ["AGENTO11Y_AGENT_NAME"],
            "agent_version": os.environ["AGENTO11Y_AGENT_VERSION"],
            "git_sha": os.environ.get("GIT_SHA", ""),
            "model_provider": os.environ.get("AGENTO11Y_MODEL_PROVIDER", ""),
            "model_name": os.environ.get("AGENTO11Y_MODEL_NAME", ""),
        },
    )
    print(run.url)


if __name__ == "__main__":
    main()

"""Pull an Agent Observability stored test suite and print or write portable YAML."""

from __future__ import annotations

import os

from agento11y import experiments
from dotenv import load_dotenv


def main() -> None:
    load_dotenv()
    suite_id = os.environ.get("AGENTO11Y_SUITE_ID", "dashboard-regression")
    version = os.environ.get("AGENTO11Y_SUITE_VERSION", "latest_published")
    output_path = os.environ.get("AGENTO11Y_SUITE_OUTPUT", "").strip()

    suites = experiments.TestSuitesClient()
    suite = suites.pull_suite(suite_id, version=version)
    text = suite.to_yaml(output_path or None)

    if output_path:
        print(f"Wrote {suite.suite_id}@{suite.version} to {output_path}")
    else:
        print(text, end="")


if __name__ == "__main__":
    main()

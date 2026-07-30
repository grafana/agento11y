"""Push a local YAML dataset into Agent Observability stored test suites."""

from __future__ import annotations

import os
from pathlib import Path

from agento11y import experiments
from dotenv import load_dotenv


def _env_bool(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}


def main() -> None:
    load_dotenv()
    root = Path(__file__).resolve().parents[1]
    suite_path = Path(os.environ.get("AGENTO11Y_SUITE_YAML") or root / "evals" / "dashboard-regression.yaml")
    suite = experiments.TestSuite.from_yaml(str(suite_path))

    suites = experiments.TestSuitesClient()
    pushed = suites.push_suite(
        suite,
        publish=_env_bool("AGENTO11Y_PUBLISH_SUITE"),
        changelog=os.environ.get("AGENTO11Y_SUITE_CHANGELOG", "pushed from Python SDK example"),
        empty_draft=_env_bool("AGENTO11Y_EMPTY_DRAFT"),
        prune=_env_bool("AGENTO11Y_PRUNE_SUITE"),
    )

    state = "published" if pushed.published else "draft"
    print(f"Pushed {pushed.suite_id}@{pushed.suite_version} ({state}) from {suite_path}")
    if pushed.pruned_case_ids:
        print(f"Pruned remote-only cases: {', '.join(pushed.pruned_case_ids)}")


if __name__ == "__main__":
    main()

"""Publish a deterministic experiment without a local or stored test suite."""

from __future__ import annotations

import os
import time

from agento11y import experiments


def main() -> None:
    experiment_id = os.environ.get("AGENTO11Y_EXPERIMENT_ID", f"suite-free-{int(time.time())}")
    cases = [
        ("capital-fr", "Capital of France?", "Paris"),
        ("capital-jp", "Capital of Japan?", "Tokyo"),
    ]

    with experiments.experiment(
        "Suite-free deterministic example",
        experiment_id=experiment_id,
        planned_trial_count=len(cases),
        tags=["example", "suite-free"],
    ) as exp:
        for case_id, prompt, expected in cases:
            with exp.trial(case_id) as trial:
                answer = expected
                trial.record_io(
                    input=prompt,
                    output=answer,
                    model_provider="example",
                    model_name="deterministic",
                )
                trial.final_score(answer == expected, explanation=f"expected {expected!r}")

    print(f"Published {experiment_id}")
    print(f"View in Agent Observability: {exp.url}")


if __name__ == "__main__":
    main()

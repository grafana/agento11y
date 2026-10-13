"""Publish a completed Harbor job to Agent Observability in Grafana Cloud."""

import argparse

from agento11y.experiments import TestSuitesClient
from agento11y_harbor import publish_harbor_job

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("job_dir", help="Harbor job directory that contains result.json")
parser.add_argument("--experiment-id", default="", help="Base experiment ID (default: the Harbor job ID)")
parser.add_argument(
    "--publish-suite",
    choices=("subset", "replace"),
    help="Also publish the job's tasks as a stored suite checkpoint",
)
args = parser.parse_args()

published = publish_harbor_job(
    args.job_dir,
    experiment_id=args.experiment_id,
    test_suites_client=TestSuitesClient() if args.publish_suite else None,
    suite_publication_policy=args.publish_suite,
)
for run in published.runs:
    print(run.url or run.experiment_id)

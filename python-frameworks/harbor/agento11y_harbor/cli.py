"""Command-line bridge used by ``agento11y harbor``."""

from __future__ import annotations

import argparse
import os
import shutil
import sys
from collections.abc import Sequence

from agento11y.experiments import TestSuitesClient

from .integration import publish_harbor_job


def _export(argv: Sequence[str]) -> int:
    parser = argparse.ArgumentParser(
        prog="agento11y harbor export",
        description="Publish a completed Harbor job directory as one experiment per agent/model candidate.",
    )
    parser.add_argument("job_dir", help="Harbor job directory that contains the job result.json")
    parser.add_argument(
        "--experiment-id",
        default="",
        help="Base experiment ID (default: the Harbor job ID). A candidate hash is appended.",
    )
    parser.add_argument("--name", default="", help="Base experiment name (default: Harbor <job directory name>)")
    parser.add_argument(
        "--suite-id",
        default="",
        help="Test suite ID for the imported cases (default: derived from the Harbor dataset). "
        "Combine with --publish-suite to store the cases in that suite.",
    )
    parser.add_argument(
        "--publish-suite",
        choices=("subset", "replace"),
        default=None,
        help="Publish the job's tasks as a stored suite checkpoint before importing. "
        "subset adds or updates cases; replace also removes cases missing from the job. "
        "Needs AGENTO11Y_GRAFANA_URL and AGENTO11Y_SERVICE_ACCOUNT_TOKEN.",
    )
    parser.add_argument("--main-reward", default="reward", help="Reward that decides pass or fail (default: reward)")
    parser.add_argument(
        "--pass-threshold",
        type=float,
        default=1.0,
        help="Minimum main reward that counts as a pass (default: 1.0)",
    )
    parser.add_argument("--no-artifacts", action="store_true", help="Skip uploading trajectories and verifier logs")
    args = parser.parse_args(list(argv))
    try:
        published = publish_harbor_job(
            args.job_dir,
            experiment_id=args.experiment_id,
            experiment_name=args.name,
            suite_id=args.suite_id,
            test_suites_client=TestSuitesClient() if args.publish_suite else None,
            suite_publication_policy=args.publish_suite,
            main_reward=args.main_reward,
            pass_threshold=args.pass_threshold,
            upload_artifacts=not args.no_artifacts,
        )
    except ValueError as error:
        print(f"agento11y: {error}", file=sys.stderr)
        return 1
    for run in published.runs:
        print(run.url or run.experiment_id)
    for label in published.skipped:
        print(f"agento11y: skipped {label}: its experiment is already finalized", file=sys.stderr)
    for failure in published.failed:
        print(f"agento11y: could not publish {failure}", file=sys.stderr)
    if published.failed:
        print("agento11y: rerun the same command to publish only the missing experiments", file=sys.stderr)
        return 1
    return 0


def _run(argv: Sequence[str]) -> int:
    harbor = shutil.which("harbor")
    if harbor is None:
        print("agento11y: Harbor is not installed or is not on PATH", file=sys.stderr)
        print("Install Harbor, then retry this command.", file=sys.stderr)
        return 1
    args = list(argv)
    if args[:1] == ["--"]:
        args = args[1:]
    plugins = [args[index + 1] for index, arg in enumerate(args[:-1]) if arg == "--plugin"]
    plugins.extend(arg.removeprefix("--plugin=") for arg in args if arg.startswith("--plugin="))
    if "agento11y" not in plugins:
        args = ["--plugin", "agento11y", *args]
    os.execvpe(harbor, [harbor, "run", *args], os.environ)
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    args = list(sys.argv[1:] if argv is None else argv)
    if not args or args[0] in {"-h", "--help", "help"}:
        print(
            "Usage:\n"
            "  agento11y harbor [run] -- <harbor run arguments>\n"
            "  agento11y harbor export <job-directory> [flags]\n"
            "\n"
            "Run 'agento11y harbor export --help' for export flags."
        )
        return 0
    if args[0] == "export":
        return _export(args[1:])
    if args[0] == "run":
        args = args[1:]
    return _run(args)


def console_main() -> None:
    raise SystemExit(main())

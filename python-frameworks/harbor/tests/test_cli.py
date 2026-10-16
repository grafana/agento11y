from __future__ import annotations

from types import SimpleNamespace

from agento11y_harbor import cli
from agento11y_harbor.integration import PublishedHarborJob


def test_export_command_uses_completed_job_import(monkeypatch, capsys):
    calls = []
    monkeypatch.setattr(
        cli,
        "publish_harbor_job",
        lambda path, **kwargs: (
            calls.append((path, kwargs))
            or PublishedHarborJob(
                runs=[SimpleNamespace(url="https://grafana.example/run", experiment_id="run")],
                trial_count=1,
                score_count=1,
            )
        ),
    )

    assert cli.main(["export", "/tmp/job", "--experiment-id", "run"]) == 0
    assert calls[0][0] == "/tmp/job"
    assert calls[0][1]["experiment_id"] == "run"
    assert capsys.readouterr().out.strip() == "https://grafana.example/run"


def test_run_command_execs_harbor_with_plugin(monkeypatch):
    called = []
    monkeypatch.setattr(cli.shutil, "which", lambda name: "/bin/harbor" if name == "harbor" else None)
    monkeypatch.setattr(cli.os, "execvpe", lambda file, argv, env: called.append((file, argv, env)))

    assert cli.main(["run", "--dataset", "terminal-bench@2.0"]) == 0
    assert called[0][0] == "/bin/harbor"
    assert called[0][1] == [
        "/bin/harbor",
        "run",
        "--plugin",
        "agento11y",
        "--dataset",
        "terminal-bench@2.0",
    ]


def test_run_command_keeps_other_plugins(monkeypatch):
    called = []
    monkeypatch.setattr(cli.shutil, "which", lambda _: "/bin/harbor")
    monkeypatch.setattr(cli.os, "execvpe", lambda file, argv, env: called.append(argv))

    assert cli.main(["--plugin", "other", "--dataset", "terminal-bench@2.0"]) == 0
    assert called[0][2:6] == ["--plugin", "agento11y", "--plugin", "other"]


def _capture_export(monkeypatch):
    calls = []
    monkeypatch.setattr(
        cli,
        "publish_harbor_job",
        lambda path, **kwargs: calls.append(kwargs) or PublishedHarborJob(runs=[], trial_count=0, score_count=0),
    )
    return calls


def test_suite_id_alone_does_not_publish_a_checkpoint(monkeypatch):
    calls = _capture_export(monkeypatch)

    assert cli.main(["export", "/tmp/job", "--suite-id", "terminal-bench"]) == 0
    assert calls[0]["suite_id"] == "terminal-bench"
    assert calls[0]["test_suites_client"] is None
    assert calls[0]["suite_publication_policy"] is None


def test_publish_suite_wires_policy_and_suites_client(monkeypatch):
    calls = _capture_export(monkeypatch)
    monkeypatch.setattr(cli, "TestSuitesClient", lambda: "suites-client")

    assert cli.main(["export", "/tmp/job", "--suite-id", "tb", "--publish-suite", "replace"]) == 0
    assert calls[0]["test_suites_client"] == "suites-client"
    assert calls[0]["suite_publication_policy"] == "replace"


def test_export_reports_validation_errors_without_a_traceback(monkeypatch, capsys):
    def fail(path, **kwargs):
        raise ValueError("Experiment 'job-1-abc' is already finalized")

    monkeypatch.setattr(cli, "publish_harbor_job", fail)

    assert cli.main(["export", "/tmp/job"]) == 1
    assert "already finalized" in capsys.readouterr().err


def test_export_reports_partial_failures_and_exits_nonzero(monkeypatch, capsys):
    published = PublishedHarborJob(
        runs=[SimpleNamespace(url="https://grafana.example/run", experiment_id="run")],
        trial_count=1,
        score_count=1,
        skipped=["claude-code / gpt-5 (job-1-aaa)"],
        failed=["codex / gpt-5 (job-1-bbb): ConnectionError: refused"],
    )
    monkeypatch.setattr(cli, "publish_harbor_job", lambda path, **kwargs: published)

    assert cli.main(["export", "/tmp/job"]) == 1
    out, err = capsys.readouterr()
    assert out.strip() == "https://grafana.example/run"
    assert "skipped claude-code / gpt-5 (job-1-aaa)" in err
    assert "could not publish codex / gpt-5 (job-1-bbb): ConnectionError: refused" in err
    assert "rerun the same command" in err

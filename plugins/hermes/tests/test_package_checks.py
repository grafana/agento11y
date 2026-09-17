from __future__ import annotations

import io
import json
import os
import runpy
import subprocess
import sys
import tarfile
import zipfile
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
CHECKS = runpy.run_path(str(ROOT / "scripts/check-package.py"))


@pytest.mark.parametrize("kind", ["wheel", "sdist"])
@pytest.mark.parametrize("version,valid", [("0.10.0", True), ("0.9.0", False)])
def test_artifact_version(tmp_path, kind, version, valid):
    data = f"Name: grafana-agento11y-hermes\nVersion: {version}\n".encode()
    if kind == "wheel":
        artifact = tmp_path / "plugin.whl"
        with zipfile.ZipFile(artifact, "w") as wheel:
            wheel.writestr("grafana_agento11y_hermes.dist-info/METADATA", data)
    else:
        artifact = tmp_path / "plugin.tar.gz"
        with tarfile.open(artifact, "w:gz") as sdist:
            entry = tarfile.TarInfo("grafana_agento11y_hermes/PKG-INFO")
            entry.size = len(data)
            sdist.addfile(entry, io.BytesIO(data))
    if valid:
        CHECKS["check_metadata"](artifact, "0.10.0")
    else:
        with pytest.raises(ValueError, match="Artifact version"):
            CHECKS["check_metadata"](artifact, "0.10.0")


def test_check_runner_clears_credentials(tmp_path):
    uv = tmp_path / "uv"
    uv.write_text(
        f"#!{sys.executable}\n"
        "import json, os, sys\n"
        "if sys.argv[2:4] in (['cache', 'dir'], ['python', 'dir']):\n"
        "    print('/tmp')\n"
        "else:\n"
        "    print(json.dumps({'env': dict(os.environ), 'args': sys.argv[1:]}))\n"
    )
    uv.chmod(0o755)
    result = subprocess.run(
        ["bash", str(ROOT / "scripts/run-check.sh"), "test", "3.12"],
        env={
            **os.environ,
            "PATH": f"{tmp_path}{os.pathsep}{os.environ['PATH']}",
            "HOME": str(tmp_path),
            "AGENTO11Y_AUTH_TOKEN": "dummy-token",
            "SIGIL_AUTH_TOKEN": "dummy-legacy-token",
            "OPENAI_API_KEY": "dummy-provider-key",
            "ANTHROPIC_API_KEY": "dummy-provider-key",
            "OTEL_EXPORTER_OTLP_ENDPOINT": "https://unused.invalid",
            "OTEL_EXPORTER_OTLP_HEADERS": "dummy-header",
            "PYTHONPATH": "dummy-source-path",
            "UV_ENV_FILE": "dummy-env-file",
        },
        text=True,
        capture_output=True,
        check=True,
    )
    captured = json.loads(result.stdout)
    env = captured["env"]
    assert env["HOME"] != str(tmp_path)
    assert not Path(env["HOME"]).exists()
    assert not any(key.startswith(("AGENTO11Y_", "SIGIL_", "OTEL_", "OPENAI_", "ANTHROPIC_")) for key in env)
    assert "PYTHONPATH" not in env
    assert "UV_ENV_FILE" not in env
    assert captured["args"] == [
        "--no-config",
        "run",
        "--locked",
        "--isolated",
        "--no-env-file",
        "--python",
        "3.12",
        "python",
        "-m",
        "pytest",
        "--cov",
    ]

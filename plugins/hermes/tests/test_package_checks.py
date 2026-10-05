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
LICENSE = (ROOT / "LICENSE").read_bytes()


@pytest.mark.parametrize("kind", ["wheel", "sdist"])
@pytest.mark.parametrize(
    "version,name,license_text,error",
    [
        ("0.11.0", "agento11y-hermes", LICENSE, None),
        ("0.10.0", "agento11y-hermes", LICENSE, "Artifact version"),
        ("0.11.0", "grafana-agento11y-hermes", LICENSE, "Unexpected distribution"),
        ("0.11.0", "agento11y-hermes", None, "LICENSE"),
        ("0.11.0", "agento11y-hermes", b"Apache-2.0", "LICENSE differs"),
    ],
)
def test_artifact_metadata(tmp_path, kind, version, name, license_text, error):
    data = f"Name: {name}\nVersion: {version}\n".encode()
    if kind == "wheel":
        artifact = tmp_path / "plugin.whl"
        with zipfile.ZipFile(artifact, "w") as wheel:
            wheel.writestr("agento11y_hermes.dist-info/METADATA", data)
            if license_text is not None:
                wheel.writestr("agento11y_hermes.dist-info/licenses/LICENSE", license_text)
    else:
        artifact = tmp_path / "plugin.tar.gz"
        with tarfile.open(artifact, "w:gz") as sdist:
            for path, content in (("PKG-INFO", data), ("LICENSE", license_text)):
                if content is not None:
                    entry = tarfile.TarInfo(f"agento11y_hermes/{path}")
                    entry.size = len(content)
                    sdist.addfile(entry, io.BytesIO(content))
    if error is None:
        CHECKS["check_metadata"](artifact, "0.11.0", LICENSE)
    else:
        with pytest.raises(ValueError, match=error):
            CHECKS["check_metadata"](artifact, "0.11.0", LICENSE)


def test_license_matches_repository():
    assert LICENSE == (ROOT.parents[1] / "LICENSE").read_bytes()


@pytest.mark.parametrize(
    "command,invocation",
    [
        (["test", "3.12"], ["--python", "3.12", "python", "-m", "pytest", "--cov"]),
        (
            ["build", "--expected-version", "0.11.0", "--output-dir", "/tmp/hermes-dists"],
            [
                "--python",
                "3.11",
                "python",
                "scripts/check-package.py",
                "--expected-version",
                "0.11.0",
                "--output-dir",
                "/tmp/hermes-dists",
            ],
        ),
    ],
)
def test_check_runner_clears_credentials(tmp_path, command, invocation):
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
        ["bash", str(ROOT / "scripts/run-check.sh"), *command],
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
        *invocation,
    ]

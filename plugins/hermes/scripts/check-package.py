from __future__ import annotations

import argparse
import email.parser
import importlib.metadata
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import tomllib
import zipfile
from pathlib import Path

DISTRIBUTION = "grafana-agento11y-hermes"


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValueError(message)


def check_installed(expected: str) -> None:
    import agento11y

    from grafana_agento11y_hermes._version import plugin_user_agent

    dist = importlib.metadata.distribution(DISTRIBUTION)
    require(dist.version == expected, f"Installed version {dist.version} != {expected}")
    entries = [ep for ep in dist.entry_points if ep.group == "hermes_agent.plugins" and ep.name == "agento11y"]
    require(len(entries) == 1, "Expected one Hermes agento11y entry point")
    module = entries[0].load()
    require(callable(module.register), "Hermes register is not callable")
    for loaded in (module, agento11y):
        require(
            Path(loaded.__file__).resolve().is_relative_to(Path(sys.prefix).resolve()),
            "Import escaped installed environment",
        )
    tokens = plugin_user_agent().split()
    require(tokens[0] == f"agento11y-plugin-hermes/{expected}", "Plugin User-Agent version mismatch")
    require(
        tokens[1] == f"agento11y-sdk-python/{importlib.metadata.version('agento11y')}",
        "SDK User-Agent version mismatch",
    )
    print(f"Installed entry point and User-Agent verified: {dist.version}")


def check_metadata(artifact: Path, expected: str) -> None:
    if artifact.suffix == ".whl":
        with zipfile.ZipFile(artifact) as wheel:
            names = [name for name in wheel.namelist() if name.endswith(".dist-info/METADATA")]
            require(len(names) == 1, "Expected one wheel METADATA")
            data = wheel.read(names[0])
    else:
        with tarfile.open(artifact) as sdist:
            names = [name for name in sdist.getnames() if name.count("/") == 1 and name.endswith("/PKG-INFO")]
            require(len(names) == 1, "Expected one sdist PKG-INFO")
            stream = sdist.extractfile(names[0])
            if stream is None:
                raise ValueError("Missing sdist metadata")
            data = stream.read()
    metadata = email.parser.BytesParser().parsebytes(data)
    require(metadata["Name"] == DISTRIBUTION, f"Unexpected distribution in {artifact.name}")
    require(metadata["Version"] == expected, f"Artifact version {metadata['Version']} != {expected}")


def check_build(expected: str | None) -> None:
    root = Path(__file__).resolve().parents[1]
    project = tomllib.loads((root / "pyproject.toml").read_text())["project"]
    expected = expected or project["version"]
    require(project["version"] == expected, f"Project version {project['version']} != {expected}")
    with tempfile.TemporaryDirectory(prefix="hermes-package-") as temporary:
        work = Path(temporary)
        home = work / "home"
        home.mkdir()
        env = {
            "PATH": os.environ["PATH"],
            "HOME": str(home),
            "TMPDIR": str(work),
            "UV_CACHE_DIR": subprocess.check_output(["uv", "--no-config", "cache", "dir"], text=True).strip(),
        }

        def uv(*args: str | Path, cwd: Path = work) -> None:
            subprocess.run(["uv", "--no-config", *map(str, args)], cwd=cwd, env=env, check=True)

        requirements = work / "requirements.txt"
        uv(
            "export",
            "--locked",
            "--no-dev",
            "--no-emit-project",
            "--no-hashes",
            "--python",
            sys.executable,
            "--output-file",
            requirements,
            cwd=root,
        )
        dist = work / "dist"
        uv("build", "--no-sources", "--python", sys.executable, "--sdist", "--wheel", "--out-dir", dist, root)
        (wheel,) = dist.glob("*.whl")
        (sdist,) = dist.glob("*.tar.gz")
        check_metadata(wheel, expected)
        check_metadata(sdist, expected)
        rebuilt = work / "rebuilt"
        uv("build", "--no-sources", "--python", sys.executable, "--wheel", "--out-dir", rebuilt, sdist)
        (rebuilt_wheel,) = rebuilt.glob("*.whl")
        check_metadata(rebuilt_wheel, expected)
        checker = work / "check-package.py"
        shutil.copyfile(__file__, checker)
        for index, artifact in enumerate((wheel, rebuilt_wheel)):
            venv = work / f"venv-{index}"
            uv("venv", "--python", sys.executable, venv)
            python = venv / ("Scripts/python.exe" if os.name == "nt" else "bin/python")
            uv("pip", "install", "--python", python, "--requirements", requirements, artifact)
            subprocess.run([str(python), "-I", str(checker), "--installed", expected], cwd=work, env=env, check=True)
        print("Wheel and rebuilt sdist verified outside the checkout")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--installed", metavar="VERSION")
    parser.add_argument("--expected-version")
    args = parser.parse_args()
    if args.installed:
        check_installed(args.installed)
    else:
        check_build(args.expected_version)

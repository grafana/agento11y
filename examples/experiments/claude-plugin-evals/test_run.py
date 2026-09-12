"""Offline check: preserve exit status and never import interrupted evals."""

import shutil
import subprocess
import tempfile
from pathlib import Path


def main():
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        shutil.copy(Path(__file__).with_name("run.sh"), root / "run.sh")
        claude = root / "claude"
        claude.write_text("""#!/bin/sh
printf '%s\\n' "$@" > "$CLAUDE_ARGS"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--json" ]; then shift; printf '{}' > "$1"; fi
  shift
done
exit "$EVAL_STATUS"
""")
        exporter = root / "exporter"
        exporter.write_text("""#!/bin/sh
printf '%s\\n' "$@" > "$MARKER"
exit "$IMPORT_STATUS"
""")
        claude.chmod(0o700)
        exporter.chmod(0o700)
        for eval_status, import_status, expected in [
            (0, 0, 0),
            (1, 0, 1),
            (2, 0, 2),
            (130, 0, 130),
            (137, 0, 137),
            (143, 0, 143),
            (0, 1, 1),
            (2, 1, 2),
        ]:
            marker = root / f"import-{eval_status}-{import_status}"
            result = subprocess.run(
                ["/bin/bash", str(root / "run.sh")],
                env={
                    "PATH": f"{root}:/usr/bin:/bin",
                    "HOME": str(root),
                    "AGENTO11Y_BIN": str(exporter),
                    "EVAL_STATUS": str(eval_status),
                    "IMPORT_STATUS": str(import_status),
                    "MARKER": str(marker),
                    "CLAUDE_ARGS": str(root / "claude-args"),
                    "AGENTO11Y_EVAL_INCLUDE_CONTENT": "1" if eval_status == 2 else "0",
                    "AGENTO11Y_EVAL_EXPECT_TENANT": "test-tenant",
                },
                capture_output=True,
                text=True,
            )
            assert result.returncode == expected, (result.returncode, expected, result.stderr)
            if eval_status >= 128:
                assert not marker.exists(), "Interrupted eval must not start an upload"
                continue
            imported = marker.read_text().splitlines()
            assert "--trace-root" in imported and "/tmp" in imported
            assert "--keep-temp" in (root / "claude-args").read_text().splitlines()
            assert ("--include-content" in imported) == (eval_status == 2)
            assert "--expect-tenant" in imported and "test-tenant" in imported
    print("CI wrapper: all eight exit-status checks passed (no model calls or uploads)")


if __name__ == "__main__":
    main()

#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
python3 - <<'PYTHON'
from pathlib import Path

workflow = Path(".github/workflows/python-sdks-publish.yml").read_text()
before, auto_merge = workflow.split("  auto-merge:\n", 1)
assert "gh pr merge" not in before
assert "needs: [build, publish-core, publish-dependents]" in auto_merge
assert "if: ${{ !inputs.dry-run }}" in auto_merge
assert "always()" not in auto_merge
assert "continue-on-error:" not in workflow
assert "pull-request-branch: ${{ steps.cpr.outputs.pull-request-branch }}" in before
assert "BRANCH: ${{ needs.build.outputs.pull-request-branch }}" in auto_merge
assert "GH_REPO: ${{ github.repository }}" in auto_merge
assert 'gh pr merge "${BRANCH}" --auto --squash --delete-branch' in auto_merge
print("Python release auto-merge gate checks passed")
PYTHON

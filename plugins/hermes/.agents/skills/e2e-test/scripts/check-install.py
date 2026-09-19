"""Report what was installed and which hooks the plugin registers.

Run with the test install's interpreter. Loading the plugin manager here also
proves the entry point resolves, which `hermes plugins list` cannot show for a
pip-installed plugin.
"""

from __future__ import annotations

import logging
from importlib.metadata import entry_points, version

logging.basicConfig(level=logging.WARNING, format="%(levelname)s %(name)s %(message)s")

print("hermes-agent:", version("hermes-agent"))
print("plugin:", version("grafana-agento11y-hermes"))
for ep in entry_points(group="hermes_agent.plugins"):
    if ep.name == "agento11y":
        print("entry point:", ep.name, "->", ep.value)

from hermes_cli import plugins  # noqa: E402  # ty: ignore[unresolved-import] - only in the test venv

plugins.discover_plugins(force=True)
manager = plugins.get_plugin_manager()

entry = getattr(manager, "_plugins", {}).get("agento11y")
print("registry entry:", "present" if entry is not None else "MISSING (check config.yaml plugins.enabled)")

registered = []
for hook in sorted(plugins.VALID_HOOKS):
    handlers = getattr(manager, "_hooks", {}).get(hook) or []
    if any("agento11y" in getattr(fn, "__module__", "") for fn in handlers):
        registered.append(hook)
print("hooks registered:", ", ".join(registered) or "NONE")

# Hermes Agent

This native Hermes plugin sends model requests and tool executions to Grafana
Agent Observability. With `AGENTO11Y_GUARDS_ENABLED=true`, it evaluates
postflight policy immediately before a Hermes tool runs. A denied response is
translated to Hermes's native `{"action":"block"}` directive, so the tool is
not executed.

```sh
agento11y login
agento11y hermes install
agento11y hermes --local
```

Hermes installs the declared `agento11y` Python dependency into its own Python
environment. If that dependency install was skipped, install
`agento11y>=0.17,<0.18` in the Python environment that runs `hermes`, then run
`agento11y hermes install` again.

For a local checkout before release:

```sh
export AGENTO11Y_HERMES_PLUGIN_SOURCE="file://$PWD#plugins/hermes"
agento11y hermes install
```

The plugin captures metadata only by default. Set
`AGENTO11Y_CONTENT_CAPTURE_MODE=full` only when full prompt, response, tool
argument, and tool-result capture is appropriate for the selected destination.

## Guard boundary

The enforcement seam is Hermes's native `pre_tool_call` hook. It covers tools
that Hermes dispatches through that lifecycle; it does not govern provider
requests before a model selects a tool, nor processes that bypass Hermes's tool
dispatcher. Keep host/container permissions and model-provider controls in
place for those boundaries.

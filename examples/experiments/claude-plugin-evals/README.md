# Claude Code plugin evals → Grafana Experiments

Run evals with Claude Code; compare scores in Grafana Agent Observability and
open each attempt's conversation and trace. The importer does not rerun the
agent, call another judge, or inject hooks into the eval sandbox.

## Example and attribution

This follows Anthropic's [plugin eval walkthrough](https://code.claude.com/docs/en/plugin-evals):
a commit-message skill, a meaning judge, a deterministic format check, and a
`tool_used: Skill` indicator. `evals/rename/prompt.md` uses their documented
`getUser` → `fetchUser` prompt verbatim. The skill implementation, bug-fix case,
unrelated-question case, and concrete rubrics are ours—not an official Anthropic
benchmark. The negative case follows their recommendation to test prompts that
should **not** trigger a skill.

No shell tools, network tools, real MCP servers, or scaffold scripts are granted.
The plugin only supplies instructions; it never creates a Git commit.

## Build and configure

Tested with Claude Code **2.1.269** and the released Go SDK **v0.18.0**. No SDK
upgrade or backend deployment is needed. This is a new CLI command in this
checkout; older installed binaries do not include it.

From the repository root:

```bash
(cd plugins/agento11y && GOWORK=off go build -o /tmp/agento11y-evals ./cmd/agento11y)
/tmp/agento11y-evals claude eval import --help
```

Use the connection saved by `agento11y login`, or the usual `AGENTO11Y_ENDPOINT`,
`AGENTO11Y_AUTH_TENANT_ID`, and `AGENTO11Y_AUTH_TOKEN` environment variables.
Trajectory export also requires the coding-agent OTLP configuration
(`AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT` and its configured authentication), with
permission to write traces and metrics. A missing endpoint or failed upload
fails the import; it does not silently publish broken trace links as a success.

For a new connection, use **Agent Observability → Set up a coding agent** in
Grafana to generate a token and follow the regular coding-agent onboarding flow.
There is no separate eval API key. Claude authentication for running the eval is
separate from these Grafana ingest credentials.

A `gcx` context is for **verification**, not exporter authentication.
`--expect-tenant <stack-id>` guards the experiment/conversation destination;
ensure the separately configured OTLP destination is the same stack. Links use
the saved stack URL, `AGENTO11Y_GRAFANA_URL`, or `--grafana-url`.

## Run and import the full example

From this directory:

```bash
claude plugin eval ./commit-messages \
  --trust-plugin --runs 3 \
  --model claude-sonnet-4-6 --judge-model claude-haiku-4-5 \
  --max-cost-usd 5 --no-publish --keep-temp \
  --output-dir results/demo --json results/demo.json
```

Three cases × three attempts × two arms = **18 agent runs**, plus judge calls.
This costs real model usage. The ceiling is checked between runs, so an in-flight
run can exceed it. `--no-publish` keeps Anthropic's HTML report local.

**`--keep-temp` is required.** Otherwise Claude deletes the per-attempt
`out/trace.jsonl` files: results JSON alone cannot recover conversations or
usage. Claude 2.1.269 retained these under `/tmp` (`/private/tmp` on macOS), even
with `TMPDIR` set. Keep the result and referenced trace files until export and
verification finish. Do not unseal the retained sandbox's `home`/`tmp` directories
or execute anything from them.

Preview exactly what would be uploaded, without credentials or network calls:

```bash
/tmp/agento11y-evals claude eval import results/demo.json \
  --trace-root /tmp --include-content --dry-run > results/plan.json
```

Import with explicit consent for conversation/tool content and judge evidence:

```bash
/tmp/agento11y-evals claude eval import results/demo.json \
  --trace-root /tmp --include-content --expect-tenant <your-stack-id> \
  --grafana-url https://<your-stack>.grafana.net
```

`--trace-root` explicitly authorizes reading retained `out/trace.jsonl` files
inside that directory. Traversal/symlink escapes, missing files, invalid records,
and missing authoritative final usage fail validation before upload. Relative
`tracePath` values resolve against this root; absolute paths must remain inside
it. No sandbox configuration, init records, or arbitrary referenced files are
uploaded.

Without `--include-content`, trajectory capture still exports conversation IDs,
model/tool spans, usage, and timing, but not prompts, responses, tool arguments,
results, or grader evidence. Session capture settings cannot silently enable
content. Selected fields are secret-redacted in both modes. Content can still
contain sensitive prose or paths; review the preview before sharing.

Omit `--trace-root` for **results-only** import. That never follows `tracePath`
and deliberately leaves conversations, traces, and tokens absent. It is also a
way to preserve failure diagnostics when a crashed attempt left no usable trace.

## What is preserved

| Claude output | Grafana representation |
| --- | --- |
| With / without-plugin arms | Separate runs sharing suite identity and `comparison_id` |
| Case and repeat index | Test-case snapshot and typed trial/attempt |
| Weighted score and `passed` | One `final` headline score, copied without regrading |
| Individual graders | `grader.*` scores with weight, `scored`, `with_only`, and opted-in judge votes |
| Case means, suite score, delta, threshold | Original aggregates in run/trial metadata |
| Attempt cost and duration | Trial cost/duration; separate judge-cost metadata |
| Retained agent session | One real `invoke_agent` generation with the visible conversation |
| Assistant requests and tool calls/results | Child model/tool spans with original identifiers and timestamp bounds |
| Terminal result usage | Actual input/output/cache-read/cache-write totals on the invocation; input/output on the trial |
| Correlation | Every trial and score links to its conversation, generation/root span, and trace |
| Errors or mock aborts | Failed execution, retaining any scores Claude produced |

**Granularity matters:** Claude's streamed assistant usage is partial; the final
result contains authoritative invocation totals. We do not invent per-call token
counts or duplicate the invocation's usage onto every model span. Input totals
are inclusive of cache-read and cache-write subsets. Child span timing uses
available transcript boundaries, not exact network request start times. Empty
thinking/signature blocks have no reasoning text to export. Judge calls have
scores, votes, and reported cost, but Claude does not provide their conversation
traces here; none are fabricated.

Claude averages **case means**, not necessarily all trials, for its suite score.
Claude's case threshold, Grafana's report pass rate, and average attempt scores
are different quantities. Preserve the original aggregates for Claude's gate
and delta. The demo has equal repeat counts, so case/trial means agree.

Partial documents become **failed** experiments. `skippedPaidGraders` attempts
use `claude_score_incomplete`, not `final`, and fail the experiment—even if Claude
reports `partial: false`. Complete imports count actual attempts because Claude
2.1.269 retains the case default `runsPerCase` after a `--runs` override. Partial
planned counts remain unknown. Cost is Anthropic's list-price estimate, not an
invoice; it is not reconstructed from tokens.

The same source and options produce stable trial, score, generation, trace, and
span IDs. An interrupted upload can be retried; a completed import is skipped.
A different name/content/trajectory setting creates a different import, so
results-only completion cannot prevent a later full import. Keep source files
immutable. Local-viewer mode requires an explicit `--no-local` Cloud override;
the local viewer has no experiment ingest.

## CI: preserve the quality gate

```bash
AGENTO11Y_BIN=/tmp/agento11y-evals \
AGENTO11Y_EVAL_INCLUDE_CONTENT=1 \
AGENTO11Y_EVAL_EXPECT_TENANT=<your-stack-id> bash run.sh
```

The wrapper creates a fresh output directory, retains traces, and imports even
when Claude's quality gate fails. Signal exit statuses (such as 130 or 143) stop
immediately without uploading. Claude's nonzero exit status is preserved;
when Claude succeeds but export fails, CI fails too. Trajectory capture is on;
content is off unless explicitly enabled above. `AGENTO11Y_EVAL_TRACE_ROOT`
overrides `/tmp`. Extra arguments go to Claude, e.g. `bash run.sh --case rename
--runs 1`. Execution and import must share the retained files if split into CI
steps; `claude ... && agento11y ...` loses failed results.

## Verify actual conversations and traces

Use `comparison_id` from a dry run with the **same name/content/trajectory
options** as the import:

```bash
python3 verify.py results/demo.json <comparison-id> \
  --context <your-gcx-context> --trace-root /tmp \
  --tempo-datasource <your-tempo-datasource-uid>
```

The script fetches both reports and score lists, compares every trial/score,
grader flag, cost, opted-in explanation/vote set, and baseline delta. For a full
import it also **retrieves every conversation and every Tempo trace**, checking
visible content, source usage, cache semantics, linked score IDs, root/child
spans, and absence of duplicated per-call usage. It does not settle for nonempty
IDs. Its JSON summary is suitable for a CI artifact.

Reports use the raw Grafana resource endpoint because older typed `gcx` models
can turn absent tokens into zero and omit coverage fields. For a results-only
import, omit the two trajectory verification flags.

Share experiment links with authorized Grafana users, or share Claude's local
HTML report/screenshots and the verified summary. A private-stack link does not
make the data public.

## Offline checks

```bash
python3 test_run.py  # eight CI/retention/consent/interruption checks; no model calls or uploads
```

Go tests cover native Claude fixtures, usage, safe paths, consent, stable IDs,
and real SDK generation/OTLP transport against local fake servers. Run tests
with an isolated HOME and clean environment: unrelated legacy plugin tests can
inherit live credentials. See the repository's `AGENTS.md` testing guidance.

# Experiment wire fixtures

The experiment ingest routes have no generated stubs, so these files are the only
cross-language contract for their shape. Go, Python, and JavaScript each check
themselves against them:

| Suite | File |
|-------|------|
| Go | `go/agento11y/experiments/conformance_test.go` |
| Python | `python/tests/test_experiments_conformance.py` |
| JavaScript | `js/test/experiments.conformance.test.mjs` |

`mise run test:sdk:experiments-conformance` runs all three, and
`mise run test:sdk:conformance` includes it.

| File | Contents |
|------|----------|
| `inputs.json` | The pinned inputs every suite feeds in: run, case, attempt, evaluator, conversation, and score identity. |
| `ids.json` | `stable_id` vectors. A drift here breaks retry idempotency and cross-process trial handoff. |
| `requests.json` | The method, encoded path, and JSON body each lifecycle call must produce. |
| `responses.json` | Canned server responses every SDK must parse into the same logical result, including the two report envelopes and the two responses that must fail. |

## The SDK id is the one field that differs

`requests.json` writes `"id": "${SDK_ID}"` inside every `source` object. Each suite
substitutes its own value before comparing: `go`, `python`, or `js`. Everything
else in a body is identical across the three SDKs.

Two things are deliberately not pinned here:

- The ingest actor header. Python and JavaScript send `X-Agento11y-Ingest-Actor`;
  Go still sends the pre-rename `X-Sigil-Ingest-Actor` (grafana/agento11y#469
  renamed the others).
- Fractional seconds on `created_at`. Go formats RFC3339Nano, Python uses
  `datetime.isoformat`, and JavaScript uses `toISOString` with an all-zero fraction
  removed. The three agree only on whole seconds, which is why `inputs.json` pins
  `2026-01-01T00:00:00Z`.

## Encodings the proto does not describe

Every dynamic path segment is percent-encoded with no safe characters. Go's
`url.PathEscape` leaves `:` alone, so it escapes the colon separately; Python uses
`quote(..., safe="")`; JavaScript uses `encodeURIComponent`. The reason is in
`trial_evaluate_reserved_trial_id`: the ingest router reads the trailing
`:evaluate` verb off the raw path segment before it decodes the trial id, so an
unescaped colon inside an id would change which route the request reaches.

The run key is `experiment_id` on the wire. Python exposes the same canonical
name. Go and JavaScript retain language-specific client model names, but the
ingest route keys on `experiment_id` and rejects unknown fields.

Blank optional fields are omitted, not sent empty. `trial_create` carries only the
four fields the route requires. `trial_evaluate_latest_version` shows the same
call without `evaluator_version`, which is how a caller asks for the latest active
version.

`evaluator_kind` is not part of the score body. It exists on the score object in
all three SDKs and feeds the OpenTelemetry evaluation event, but no SDK sends it.
`scores_export` is the contract.

A score value is a one-of: `{"number": n}`, `{"bool": b}`, or `{"string": s}`;
never two, never zero. `value.bool` is the key, not `value.boolean`.

An unknown evaluation status is a hard error. `evaluation_unsupported_status` and
`evaluation_missing_id` both have to fail the call. If an SDK read a newer terminal
state as non-terminal, it would poll until the caller's deadline, and a missing id
would turn the next status read into a validation error blaming the caller's
arguments.

Both report envelopes are accepted. The backend keys the run under `experiment`;
older drafts used `run`. `report_experiment_envelope` and
`report_run_envelope` must parse into equal report objects.

## Changing a fixture

Run the new shape through the server's request decoder before committing it. These
files are the contract, and the suites check themselves against the files rather
than against a running server, so an invented shape passes all three suites and
fails only in production.

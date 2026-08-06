# Secret redaction

`patterns.json` is the only editable secret-pattern table in this repository.
Six engines redact secrets. Five of them get a generated table from this file,
and the opencode plugin reuses the JS one:

| Engine | Pattern table |
| --- | --- |
| Go SDK | `go/agento11y/redaction_patterns_gen.go` |
| Python SDK | `python/agento11y/_redaction_patterns.py` |
| JS SDK | `js/src/redaction-patterns.generated.ts` |
| .NET SDK | `dotnet/src/Grafana.Agento11y/RedactionPatterns.g.cs` |
| shared `agento11y` binary | `plugins/agento11y/internal/redact/patterns_gen.go` |
| opencode plugin | imports the JS table through `@grafana/agento11y-core` |

Do not edit a generated file. `mise run check:redaction` regenerates the five
tables into a temporary directory and diffs them against the tree, so a hand
edit fails CI with the file name and the command to run.

The patterns are adapted from [Gitleaks](https://github.com/gitleaks/gitleaks)
(MIT).

## Adding a pattern

1. Add an entry to `patterns.json`.
2. Add at least one positive case to `fixtures/strings.json`. A tier 1 pattern
   needs a case in both `full` and `light` mode. The Go suite fails if a pattern
   has no positive case: a mis-escaped regex in one language would otherwise
   pass every suite.
3. Run `mise run generate:redaction`.
4. Run `mise run test:sdk:redaction-conformance`.

## Pattern fields

```json
{
  "id": "env-secret-value",
  "tier": 2,
  "flags": ["i"],
  "regex": "(^|[^A-Za-z0-9])((?:PASSWORD|SECRET)[ \\t\\n\\f\\r\\xa0]*[=:][ \\t\\n\\f\\r\\xa0]*)([^ \\t\\n\\f\\r\\xa0\"{}\\[\\],]+)",
  "keepGroups": [1, 2]
}
```

- `id` is kebab-case and appears in the output as `[REDACTED:<id>]`.
- `tier` is `1`, `2`, or `"email"`. Tier 1 covers high-confidence formats and
  runs in both redaction modes. Tier 2 covers key/value heuristics and runs only
  in full redaction, because they produce too many false positives in prose. The
  email pattern runs in both modes but only when the caller enables it.
- `flags` may contain `"i"`, on a tier 2 or email pattern. Tier 1 patterns
  reject it: every engine alternates them into one regex that is compiled
  without flags, so a flag there would reach some engines and not others.
- `regex` must stay inside the portable common subset. The generator rejects
  lookahead, lookbehind and backreferences, which Go RE2 cannot compile. It also
  rejects `\s`, `\w` and `\d`, which mean ASCII in Go and JavaScript and Unicode
  in Python and .NET; write the character class out instead. `[\s\S]` is
  allowed, because a class and its complement mean any character everywhere.
  Whitespace is spelled `[ \t\n\f\r\xa0]`: ASCII whitespace plus the
  non-breaking space, which arrives with text pasted from a browser.
- `keepGroups` lists the capturing groups the replacement preserves. The groups
  left out form one contiguous run and are replaced by the marker. For the
  example above the replacement is `$1$2[REDACTED:env-secret-value]`, so
  `DB_PASSWORD=hunter2` becomes `DB_PASSWORD=[REDACTED:env-secret-value]`.
  `json-secret-field` keeps groups 1 and 3 instead, so the closing quote
  survives and the JSON stays parseable. One capturing group must not sit inside
  another, because the replacement reprints the kept groups in order.
- Tier 1 and email patterns must have no capturing groups at all. Every engine
  alternates the tier 1 patterns into one regex and maps the matched group index
  back to a pattern id. An extra group shifts that mapping, and a later match
  then disappears from the output with no marker.

`\b` is allowed. The generator pins it to ASCII in the two engines that read it
as Unicode by default: the Python table carries `re.ASCII` and the C# table
carries `RegexOptions.ECMAScript`. Without that, a `glc_` token pressed against
a CJK character stays in clear text in Python and .NET while Go and JavaScript
redact it. The C# table also carries `RegexOptions.CultureInvariant`, so
case-insensitive matching does not follow the host culture.

## Email redaction is gated per call site

The email pattern is in the shared table, but whether it runs is the caller's
choice. The SDKs redact addresses by default (`RedactEmailAddresses`). The
`agento11y` binary and the opencode plugin pass `false`: agent transcripts
routinely carry commit authors and reviewer addresses, and redacting them costs
more context than it protects.

## Known limitations

- `sort key: name` is a false positive. The tier 2 left boundary stops
  `MONKEY: banana` from matching, but a plain English sentence with `key:` in it
  still matches and its next word is replaced. Fixing this needs a stopword
  list, and nobody has written one. `fixtures/strings.json` pins today's
  behavior, so the day someone adds the list the diff shows every case it
  changes.
- A secret-looking JSON key relabels its value. `{"token":"glc_..."}` comes out
  as `{"token":"[REDACTED:json-secret-field]"}` rather than
  `[REDACTED:grafana-cloud-token]`: `json-secret-field` replaces the whole
  quoted value, including a marker a tier 1 pattern already wrote there. Nothing
  leaks; only the label is coarser.
- Whitespace stops at U+00A0. The wider Unicode spaces (U+2000 to U+200A,
  U+3000, and the rest) do not count as separators, because Go RE2 spells their
  escapes differently from the other three engines and the table holds one
  string per pattern.
- Case-insensitive matching of non-ASCII homoglyphs differs between engines. Go
  and .NET fold U+212A KELVIN SIGN into `k`, JavaScript and Python do not, so
  `TO<U+212A>EN=secret` is redacted by two engines and left in clear text by the
  other two. This affects the tier 2 key names only.
- Making the engines agree does not widen coverage. The table holds 26 patterns
  (22 tier 1, 3 tier 2, 1 email) with no entropy gating, while Gitleaks ships a
  few hundred rules.

## Fixtures

`fixtures/strings.json` holds string-level cases. All six engines load it and
must produce `expected` byte for byte. Each case pins its own `mode` and its own
`emails` setting and runs once.

A fake key that looks real enough for the pattern also looks real to GitHub push
protection, which blocks a push that adds one. Six inputs therefore hide a
single character behind a `\u` escape, for example `\u0054` for the `T` in the
OpenAI `T3BlbkFJ` marker. The JSON parser decodes the escape before any engine
sees the string, so the case still exercises the full key shape, but the bytes
in the file never spell the key out. Write new fake keys the same way. The Go,
.NET and plugin unit tests do the same thing by concatenating string literals.

`fixtures/generations.json` holds generation-level cases for the four SDKs that
expose a generation sanitizer. It is a slot-to-mode matrix: each generation
field is filled with one probe string carrying a tier 1 token, a tier 2
key/value pair, and an email address, so a slot's mode is observable in the
output. `skip` redacts no probe, `light` the token and the address, `full`
every probe. A case that turns email redaction off uses `light-no-email` and
`full-no-email` instead. A mode is only a key into the `probe` object, so a new
mode needs no change in the four harnesses. Each SDK asserts that the slots it
builds are exactly the slots in the matrix, so neither side can drop a field
quietly. Media parts exist only in Go, so they stay out of the matrix and the Go
harness asserts on them directly.

Only positive coverage is enforced. The Go suite fails when a pattern has no
case that triggers it, and nothing requires a case proving a pattern stays off
text it should ignore. For a tier 2 pattern, write that case anyway: every
limitation listed above is over-matching, and a fixture is what makes the next
one visible.

Each SDK builds its own native generation from the slot names, so no language
needs a `Generation` serializer.

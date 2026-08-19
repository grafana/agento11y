# Cursor agent-transcript fixtures

Synthetic JSONL sessions under `transcripts/`. They pin the shapes the
importer expects for `~/.cursor/projects/**/agent-transcripts/<uuid>/<uuid>.jsonl`.
No bytes from a real conversation are in these files.

| File | Covers |
|------|--------|
| `two-turn.jsonl` | Two prompts with `<timestamp>` and `<user_query>`, successful `turn_ended` |
| `tool-use.jsonl` | Assistant `tool_use` with no tool result |
| `turn-ended-error.jsonl` | `turn_ended` with `status: error` |

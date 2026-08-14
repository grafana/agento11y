# Cursor chat store fixtures

Cursor publishes no schema for its chat store and stamps no version into it.
Everything the importer knows was recovered by reading real stores, so any Cursor
release can change the format under it. The reader in
`internal/agents/cursor/chatstore` is the description of that format, and the
time decoding lives in `internal/history/cursor_clock.go`. This file does not
repeat either one.

These fixtures pin the format the importer expects, so a change to the reader or
to the importer shows up as a diff here. They cannot notice Cursor changing the
format: no Cursor byte is involved, because `chatstore/chatstoretest` re-encodes
them on every run. Only the live harness at the end of this file can see a Cursor
format change.

## What is here

`two-turn.json` and `tool-call.json` describe one session each. They are session
*descriptions*, not stores: `cursor_test.go` reads one and drives
`chatstore/chatstoretest` to write a real store under `t.TempDir()`. The
committed file is therefore reviewable text, and the byte-level encoding comes
from one writer that both the reader's tests and the importer's tests use.

`tool-call.json` carries an `issuedAt` beside each ID that encodes a time, so the
fixture states the time in words as well as in the hex that holds it. The two
have to be checked against each other by hand; nothing in the test derives one
from the other, which is the point.

The content is synthetic: every prompt, reply, tool argument and tool result was
written for the fixture. No bytes from a real conversation are in this
repository.

Neither fixture names a Cursor version, because none is recoverable from a
session on disk.

## Checking against a real Cursor

`chatstore` ships an opt-in harness that runs the reader over every store on the
machine and reports counts and role histograms, never message text:

```sh
CURSOR_LIVE_CHATS=~/.cursor/chats go test \
    ./plugins/agento11y/internal/agents/cursor/chatstore \
    -run TestLiveStores -v -count=1
```

Run it after a Cursor upgrade. A drop in `prompts`, a new role or part type, or a
non-zero `failed` means the format moved and the reader is out of date. Its
`providerIDs` line is a histogram of ID prefix and length: a prefix or a length
the decoder does not expect means the times need checking again.

The importer ships the same harness for the times:

```sh
CURSOR_LIVE_CHATS=~/.cursor/chats go test \
    ./plugins/agento11y/internal/history \
    -run TestCursorLiveTimes -v -count=1
```

It fails on a session that spans more than a day, on a turn that ends before it
starts, and on a turn that starts before the one in front of it. A jump in its
`interpolated` count is a provider that stopped dating its IDs.

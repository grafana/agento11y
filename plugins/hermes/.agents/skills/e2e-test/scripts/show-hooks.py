"""Summarize a hooks.jsonl written by the probe plugin.

Prints the hook sequence, then the kwargs each hook carried, then the fields the
plugin needs but cannot get from a hook argument (they live in the sanitized
request body). Use it to check what the running hermes build passes before
trusting any doc.

usage: show-hooks.py [path-to-hooks.jsonl]
"""

from __future__ import annotations

import json
import os
import sys

default_path = os.path.join(os.environ.get("E2E_DIR", "/tmp/agento11y-hermes-e2e"), "hooks.jsonl")
path = sys.argv[1] if len(sys.argv) > 1 else default_path
rows = [json.loads(line) for line in open(path)]

print("== sequence")
for row in rows:
    payload = row.get("payload", {})
    bits = [row["hook"]]
    request_id = str(payload.get("api_request_id") or "")
    if request_id:
        bits.append("id=..." + request_id[-10:])
    for key in ("tool_name", "status", "finish_reason", "status_code", "retry_count", "retryable"):
        if payload.get(key) not in (None, ""):
            bits.append(f"{key}={payload[key]}")
    if payload.get("error"):
        bits.append("error=" + json.dumps(payload["error"])[:80])
    print("  " + " | ".join(str(b) for b in bits))

print("\n== kwargs per hook")
seen: set[str] = set()
for row in rows:
    if row["hook"] in seen:
        continue
    seen.add(row["hook"])
    print(f"  {row['hook']}: {', '.join(row['keys'])}")

print("\n== fields the plugin has to dig out of request.body")
for row in rows:
    if row["hook"] != "pre_api_request":
        continue
    payload = row["payload"]
    history = payload.get("conversation_history") or []
    roles = [m.get("role") for m in history if isinstance(m, dict)]
    body = (payload.get("request") or {}).get("body") if isinstance(payload.get("request"), dict) else None
    print("  conversation_history roles:", roles, "(a system role here would feed system_prompt)")
    print("  system_prompt kwarg:", repr(payload.get("system_prompt")))
    print("  max_tokens kwarg:", repr(payload.get("max_tokens")))
    if isinstance(body, dict):
        print("  request.body keys:", sorted(body))
        print("  request.body.max_tokens:", body.get("max_tokens"))
        print("  request.body has system:", "system" in body)
        tools = body.get("tools")
        print("  request.body tools:", len(tools) if isinstance(tools, list) else tools)
    break

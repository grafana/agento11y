"""Check one exported conversation against what the plugin promises to record.

Reads the JSON of `agento11y conversations get -o json` and reports, per
generation, whether each recorded field arrived. Reports rather than asserts:
what is expected depends on the capture mode, the channels in use and the hermes
version, and the point is to see the whole picture in one place after a run.

usage: check-generations.py <conversation.json>
"""

from __future__ import annotations

import json
import sys


def part_kinds(messages: list) -> list[str]:
    kinds = []
    for message in messages or []:
        role = str(message.get("role", "")).replace("MESSAGE_ROLE_", "").lower()
        for part in message.get("parts") or []:
            kind = next(iter(part), "?") if part else "empty"
            kinds.append(f"{role}:{kind}")
    return kinds


def mark(ok: bool) -> str:
    return "yes" if ok else "NO"


def clip_note(text: str) -> str:
    """Say whether hermes shortened the value before the plugin ever saw it.

    The sanitizer leaves ``...[truncated N chars]`` on a clipped string. Its
    first pass clips at 8000 characters and runs before the payload cap is
    measured, so a long system prompt arrives clipped at every cap.
    """
    if not text:
        return ""
    return " clipped by hermes" if "[truncated" in text[-40:] else " complete"


document = json.load(open(sys.argv[1]))
generations = document.get("generations") or []
print(f"generations: {len(generations)}")

for generation in generations:
    usage = generation.get("usage") or {}
    tags = generation.get("tags") or {}
    metadata = generation.get("metadata") or {}
    estimate = generation.get("context_token_estimate") or {}
    print(f"\n--- {generation.get('generation_id')}")
    print(f"  model            {generation.get('model')} response_model={generation.get('response_model')!r}")
    print(f"  agent            {generation.get('agent_name')} version={generation.get('agent_version')!r}")
    print(f"  capture mode     {metadata.get('agento11y.sdk.content_capture_mode')}")
    print(f"  window           {generation.get('started_at')} -> {generation.get('completed_at')}")
    print(f"  stop_reason      {generation.get('stop_reason')!r}")
    print(f"  error            {json.dumps(generation.get('error'))}")
    print(f"  trace linked     {mark(bool(generation.get('trace_id')))} ({generation.get('trace_id')})")
    print(f"  input parts      {part_kinds(generation.get('input'))}")
    print(f"  output parts     {part_kinds(generation.get('output'))}")
    print("  tokens           " + ", ".join(f"{k}={v}" for k, v in sorted(usage.items())) or "  tokens           none")
    print(f"  cache tokens     {mark(any('cache' in k for k in usage))}")
    print(f"  framework tags   {mark(tags.get('agento11y.framework.name') == 'hermes')}")
    print(f"  cwd/entrypoint   {mark('cwd' in tags and 'entrypoint' in tags)}")
    print(f"  git.branch       {mark('git.branch' in tags)} (only set when the cwd is a git checkout)")
    print(f"  hermes metadata  {mark(all(f'hermes.{k}' in metadata for k in ('task_id', 'session_id', 'turn_id')))}")
    prompt = generation.get("system_prompt") or ""
    tools = generation.get("tools") or []
    print(f"  system_prompt    {mark(bool(prompt))} {len(prompt)} chars{clip_note(prompt)}")
    # An empty list next to a non-zero count is a clipped request payload, not a
    # hermes without tools, which is why the raw count is recorded beside it.
    print(f"  tools recorded   {len(tools)} of hermes.tool_count={metadata.get('hermes.tool_count')}")
    print(
        f"  sampling params  max_tokens={generation.get('max_tokens')} temperature={generation.get('temperature')} "
        f"top_p={generation.get('top_p')} tool_choice={generation.get('tool_choice')!r}"
    )
    # True when any of the prompt, the tools or the params came from an earlier
    # request in the session because this one arrived clipped.
    print(f"  facts reused     {metadata.get('hermes.request_facts_reused')}")
    print(f"  parent gens      {generation.get('parent_generation_ids')}")
    print(f"  token estimate   system_prompt={estimate.get('system_prompt')} tools_total={estimate.get('tools_total')}")

chain = [(g.get("generation_id"), (g.get("parent_generation_ids") or [None])[0]) for g in generations]
linked = sum(1 for _, parent in chain if parent)
print(f"\nchain: {linked} of {len(chain)} generations name a parent")
for generation_id, parent in chain:
    print(f"  {parent or '(root)'} -> {generation_id}")

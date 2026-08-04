# LiteLLM guard reference

Agent Observability guards are rules that run on the request path and can fail a request.
`Agento11yLiteLLMGuardrail` evaluates them inside the LiteLLM proxy.
This topic lists what each phase sees, which routes it covers, what a deny does, and every option.

To turn guards on, refer to [Enforce guards on proxy requests](../README.md#enforce-guards-on-proxy-requests).
To create a rule, refer to [Set up guards](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/guards/).

## Phases

| Phase | LiteLLM `event_hook` | Runs | Can save spend |
| --- | --- | --- | --- |
| Preflight | `pre_call` | Before the provider is called | Yes |
| Postflight | `post_call` | After the provider answers, before the caller gets the response | No |

Two gates must both be open for a phase to evaluate anything:

- LiteLLM: the guardrail is constructed with that `event_hook`. The default is `pre_call` alone.
- The SDK: `HooksConfig.phases` contains that phase. The default is `["preflight"]`.

A guardrail whose `event_hook` has no matching `HooksConfig.phases` entry logs a warning at startup.
It then skips every request for that phase, so it records no verdict either.

`during_call` raises at startup.
LiteLLM runs it in parallel with the provider call, so it can't save spend, and it never receives the response.

## Guarded routes

The guard reads the request body as the client sent it, before LiteLLM translates it to provider format.
Each route keeps its input somewhere else.

| Route | Messages | System prompt |
| --- | --- | --- |
| `/v1/chat/completions` | `messages` | `system` and `developer` messages |
| `/v1/completions` | `prompt`, as a string or a list of strings | None |
| `/v1/images/generations` | `prompt` | None |
| `/v1/messages` | `messages`, including Anthropic content blocks | Top-level `system` |
| `/v1/responses` | `input`, as a string or input items | `instructions` |

The guard skips every other route that LiteLLM runs pre-call hooks on.
That covers embeddings, moderation, rerank, audio, realtime, Model Context Protocol (MCP) tool calls, and native pass-through endpoints such as `/anthropic/v1/messages`.
Those bodies are provider-native or carry no messages, so a content rule has nothing to match.

A skipped request records no verdict at all, and nothing on those routes is ever blocked.
Evaluating them instead would return allow on empty input and record a verdict that reads like a completed check.

Postflight skips the same routes through a different test.
The post-call hook receives no call type, so the guard reads the request body instead.
It skips a body it can't take messages, a prompt, or a system prompt from.

A guarded request whose input maps to no text is skipped the same way.
A token-id `prompt` and content that's entirely images or audio both map to no text.

## Deny outcomes

A deny answers the caller with the guardrail name and the rule's reason.
The status code comes from LiteLLM: `GuardrailRaisedException` carries `status_code=400` from LiteLLM 1.87.0 on.
Before that it carried no status code, so the proxy's fallback answered `500`.

What a postflight deny does depends on how the response is delivered.
These outcomes hold on LiteLLM 1.95.0, checked against a running proxy.

| Delivery | Postflight deny outcome |
| --- | --- |
| Non-streaming, on `/v1/chat/completions`, `/v1/messages`, and `/v1/responses` | The caller gets the error. The provider output never leaves the proxy. |
| Streaming, on `/v1/chat/completions` | The caller already has the whole response. The verdict is recorded and logged at warning level. |
| Streaming, on `/v1/messages` and `/v1/responses` | Nothing runs. No hook request, no verdict, no log line. |

Neither streaming outcome is a configuration choice.
On a streamed chat completion, LiteLLM flushes every chunk, runs post-call guardrails on the assembled response, and swallows whatever they raise.
It attaches that deferred pass only to its `CustomStreamWrapper`, and the other two routes stream through their own iterators.

Treat postflight on streaming traffic as detection that feeds evaluation and alerting.
Don't rely on it for enforcement, and don't rely on it at all for `/v1/messages` or `/v1/responses`.

A preflight deny does stop a streaming request, before the first chunk.

## Rule kinds

Evaluator guards work as the [guards documentation](https://grafana.com/docs/grafana-cloud/observe-and-act/agent-observability/guides/guards/) describes.
Preflight sees messages, the system prompt, and tool definitions.
Postflight adds the response.

Three kinds behave differently through this adapter:

- Redact guards change what the model sees, though not on every route and not in every case. Refer to [Transformed requests](#transformed-requests).
- Preflight tool filter guards match tool calls that are already in the request history, which means the client has run them. They block the next call in an agent loop rather than the tool itself. Put the guard in the postflight phase to block a call the model proposes, before the client runs it.
- `model.provider` match filters are unreliable. LiteLLM's router picks a deployment after the guard runs, so the provider isn't known yet. Match on `agent_name`, `model.name`, or tags instead.

## Transformed requests

A preflight redact guard returns the rewritten request as `transformed_input`.
The guardrail writes that into a new request body and gives the new body to the proxy.
The proxy forwards it to the provider and to any later callback.

The deny check runs before the rewrite, so a denied request never reaches the provider, rewritten or not.
`apply_transforms=False` turns the rewrite off and leaves the deny check in place.

Only preflight rewrites anything.
A postflight transform is ignored, because the provider has already answered and LiteLLM takes no replacement response from the post-call hook.

Redaction always applies to evaluators in later guards, which run server-side against the redacted input.
That holds whether or not the request itself was rewritten.

### Where a transform goes

A redacted system prompt is written back on every route that has a place for one:

- `system` on `/v1/messages`
- `instructions` on `/v1/responses`
- A `system` message on chat routes

`/v1/completions` and `/v1/images/generations` carry a bare `prompt`, so they get no system prompt rewrite.

Redacted messages are written back on chat routes and `/v1/messages` only.
`/v1/responses` keeps its input in `input`, the two `prompt` routes keep theirs in `prompt`, and none of the three takes chat messages.

The rewrite changes text and nothing else.
It copies tool calls, tool call IDs, images, and an Anthropic `cache_control` breakpoint through.
So a message carrying any of them can still be redacted instead of failing the rewrite.

### Skipped rewrites

A rewrite is all or nothing.
When the guardrail can't apply the whole transform, it forwards the request as the client sent it and logs a warning naming the reason.
Every one of those warnings starts with `agento11y: skipping`, so grep the proxy log for that string when you adopt a redact rule.

Nothing marks the generation, so a redact rule whose transform never reached the provider looks the same as a rule that matched and allowed.

The rewrite is skipped when:

- Every message in the request is a system or developer message. Writing the prompt into one place would leave the message list empty, which every route rejects.
- A system prompt or an Anthropic `tool_result` spans more than one content block and a block carries fields of its own. One block is rewritten in place and keeps its fields. More than one has to collapse into a single string, and there's no way to split that string back into per-block values.
- The transform doesn't line up with the request one to one. The guardrail matches messages by position. Both the message count and the count of text values inside each message have to agree with what the guard evaluated.

A rule that rewrites a message or a system prompt to an empty string changes nothing.
An empty value can't be told apart from "no transform" on the wire.

### Reasoning isn't rewritten

The adapter leaves reasoning as the client sent it, whether it arrived as `reasoning_content` or as a thinking block.
A provider validates a reasoning payload against its own signature and rejects a rewritten one.

So a redact rule that rewrites the reasoning of an assistant turn has no effect.
A secret in reasoning text reaches the provider even when the same secret is redacted out of the message text.

## Guardrail options

All options are keyword-only.
`create_agento11y_litellm_guardrail` accepts the same ones.

| Option | Type | Default | Description |
| --- | --- | --- | --- |
| `agent_name` | `str` | `""` | Fallback agent name for a request that carries no agent identity |
| `agent_name_metadata_keys` | `Sequence[str]` | `("agent_name", "agent_id")` | Metadata keys consulted, in order, to name the agent |
| `agent_version` | `str` | `""` | Fallback agent version |
| `apply_transforms` | `bool` | `True` | Write `transformed_input` from a preflight allow verdict into the outgoing request |
| `client` | `agento11y.Client` | Required | SDK client with `hooks.enabled=True` and the phase to evaluate in `hooks.phases` |
| `default_on` | `bool` | `False` | Run on every request instead of only on requests that opt in |
| `event_hook` | `str \| Sequence[str]` | `"pre_call"` | Phases to run: `"pre_call"`, `"post_call"`, or both |
| `extra_tags` | `dict[str, str]` | `None` | Additional tags merged into every hook evaluation context |
| `guardrail_name` | `str` | `"agento11y"` | Name used for per-request opt-in and in the error response |
| `max_concurrent_evaluations` | `int` | `32` | Ceiling on hook evaluations in flight at once |
| `request_timeout_seconds` | `float` | `2.0` | How long the proxy waits for a free thread plus a verdict |

With `default_on=False`, a request opts in with `"guardrails": ["agento11y"]` in its metadata.

Lower `HooksConfig.timeout_seconds` from its 15 second default for proxy use.
It bounds how long a worker thread stays occupied after the guardrail has given up on the evaluation.

## Runtime behavior

- Evaluation runs on a pool of `max_concurrent_evaluations` threads, so it doesn't block the proxy event loop. `request_timeout_seconds` covers waiting for a free thread as well as the evaluation itself. A thread stays busy until its evaluation finishes, so a slow evaluator can keep the pool saturated for longer than that timeout.
- A transport failure, a timeout, or an unexpected error follows `HooksConfig.fail_open`. With `True`, the default, the guardrail allows the request and logs the failure at warning level. With `False`, it raises `HookTransportError`.
- Either failure mode records the verdict as `guardrail_failed_to_respond` rather than `success`, so a dead evaluator shows up in spend logs and logging callbacks.
- Failing closed costs more on postflight than on preflight. The provider has already answered and billed, and the caller gets HTTP 500 instead of the response.
- Hook evaluations correlate to the proxy request span, so a guard verdict lines up with its request in traces.
- Register the logger next to the guardrail. The guardrail exports no generations, and having both in `litellm.callbacks` still exports exactly one generation per request.
- A non-streaming postflight deny costs a generation record. LiteLLM defers success logging until post-call guardrails have run, then drops it when one raises. The failure path doesn't reach this package either.
- A denied request was still called and billed, and its verdict is in `standard_logging_guardrail_information`. A streamed deny keeps its generation, because nothing raises there.
- Postflight reads the response the provider produced, not the copy LiteLLM logs, so `turn_off_message_logging` doesn't apply to it. Enabling postflight sends response content to the hooks API even on a deployment that keeps content out of its logs.

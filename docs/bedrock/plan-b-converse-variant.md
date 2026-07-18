# Plan B — Converse-based provider variant for non-Claude models (SKETCH)

Status: **DESIGN SKETCH ONLY. Lower priority. Do NOT build unless user explicitly
says so.** Native Anthropic invoke stays the default for Claude.

## Why this is optional
The current provider posts native Anthropic Messages to `bedrock-runtime`
`/model/<id>/invoke[-with-response-stream]`. That is the best fit for Claude:
zero translation in, zero translation out for Anthropic-format customers. It only
works for models that speak the Anthropic wire format (Anthropic Claude on
Bedrock).

To resell **Nova / DeepSeek / Llama / Mistral / Titan** etc., those models do NOT
accept the Anthropic Messages body on `/invoke`. The portable path is Bedrock's
unified **Converse** API (`/converse`, `/converse-stream`), same host, same
hand-rolled SigV4 signer — only the request/response schema differs.

## Tradeoff (state plainly)
Converse uses its own schema (`messages[].content[].text|toolUse|toolResult`,
`inferenceConfig`, `stopReason`, `contentBlockDelta`). Our customers speak either
Anthropic or OpenAI. So a Converse path needs a **new Anthropic↔Converse
translation** (and OpenAI↔Converse) that the native-Anthropic path does not need.
That is the whole cost: we trade the zero-translation Claude fit for reach across
non-Claude models.

Decision rule:
- Claude models → keep native Anthropic invoke (default, no translation).
- Non-Claude models → Converse variant (only if we decide to resell them).

## Shape if built (not now)
- **New** `proxy/bedrock_converse.go`:
  - `bedrockConverseEndpoint(region, modelID, streaming) string` →
    `/model/<id>/converse` or `/converse-stream`.
  - `anthropicToConverseBody(anthropicMsgs) → converseBody` — messages, system as
    `system:[{text}]`, tools → `toolConfig.tools[].toolSpec.inputSchema.json`,
    `inferenceConfig.maxTokens/temperature/topP`.
  - Stream: reuse `readBedrockEventStream` framing (Converse stream is the same AWS
    event-stream envelope) but the inner JSON is Converse events
    (`messageStart`, `contentBlockDelta`, `messageStop.stopReason`, `metadata.usage`)
    — convert those → Anthropic SSE (to serve Anthropic customers) or → OpenAI
    chunks (reusing Plan A's OpenAI emitter).
  - Non-stream: `/converse` returns `output.message.content[]` + `usage` → convert
    to Anthropic Messages JSON or OpenAI response.
- **Routing**: `resolveBedrockModelID` (or a sibling) classifies model family; a
  Claude id → native invoke path, a non-Claude id → Converse path. Account config
  could carry an explicit `wireFormat: "anthropic"|"converse"` override to avoid
  guessing by id prefix.
- Billing/failover/secret-hygiene: unchanged — reuse `recordBedrockSuccess`, the
  same pre-stream-error-fails-over contract, same SigV4 signer.

## Mapping table — Converse ↔ our customer formats (for when built)
| Converse | Anthropic-native | OpenAI |
|---|---|---|
| `messageStart.role` | `message_start` | role delta chunk |
| `contentBlockDelta.delta.text` | `content_block_delta` text_delta | `delta.content` |
| `contentBlockDelta.delta.toolUse.input` | `input_json_delta` | `tool_calls[].function.arguments` |
| `contentBlockStart.start.toolUse{toolUseId,name}` | `content_block_start` tool_use | `tool_calls[]` id+name |
| `contentBlockDelta.delta.reasoningContent.text` | `thinking_delta` | `reasoning_content` |
| `messageStop.stopReason` (end_turn/tool_use/max_tokens) | `message_delta.stop_reason` | `finish_reason` (same table as Plan A) |
| `metadata.usage.{inputTokens,outputTokens}` | `usage` | `usage.{prompt,completion}_tokens` |

## Test strategy (for when built)
- Table-driven `anthropicToConverseBody` / `converseToAnthropic` round-trips.
- Synthetic Converse event stream (reuse `buildFrame`) → Anthropic SSE and → OpenAI
  chunks; assert ordering, tool reassembly, stop-reason, usage.
- A real non-Claude smoke (e.g. Nova) behind the live-smoke harness, gated on the
  model being enabled in-region.
- Independent SigV4 recheck of the `/converse` path vs botocore (colon in modelId,
  session token) — same method used in Phase 1.

## Recommendation
Do not build now. Ship Plan A (OpenAI surface for Claude on Bedrock) first. Revisit
Plan B only when there is concrete demand to resell a specific non-Claude model;
at that point pick the exact model and validate Converse against it live.

# Plan A — OpenAI-compatible surface for Bedrock (prompt 03)

Status: **DESIGN ONLY — awaiting approval. No implementation code until user says go.**

## Goal
Route an OpenAI Chat Completions request to a Bedrock account. Today
`handleOpenAIStream` (`proxy/handler.go:1879`) and `handleOpenAINonStream`
(`proxy/handler.go:2305`) `continue` past Bedrock accounts. Wire them so an
OpenAI request served by a Bedrock account works, streaming and non-streaming.

## Seam decision
Bedrock's side is **native Anthropic** (our `readBedrockEventStream` already
yields `content_block_delta`, `message_delta.stop_reason`, `tool_use`). So:

```
OpenAI Chat Completions body
  → (new) openaiToAnthropicBedrockBody()  → Anthropic Messages body
  → buildBedrockBody() (pins anthropic_version, existing)   [REUSE]
  → newBedrockRequest + signSigV4 + invoke                  [REUSE]
  → Anthropic response / SSE
  → (new) Anthropic→OpenAI converter → chat.completion(.chunk) + [DONE]
```

The AWS sample `aws-samples/bedrock-access-gateway`
(`src/api/models/bedrock.py`, MIT-0, reusable without attribution) is mirrored
**only for the OpenAI-target shapes and streaming structure**. Its Bedrock side
is Converse (`contentBlockDelta`, `toolUse`, `stopReason`, `messageStop`) — we do
NOT read those; we read our Anthropic-native events. Only the OpenAI output shape
and the `finish_reason` table are borrowed.

## Reuse inventory (already in repo)
| Need | Existing helper | File |
|------|-----------------|------|
| OpenAI response envelope (id/object/created/usage) | `KiroToOpenAIResponse` shape | translator.go:2305 |
| chat.completion.chunk shape + delta emission pattern | `handleOpenAIStream` chunk block | handler.go:~1927 |
| tool schema sanitize/convert | `convertOpenAITools` | translator.go:2275 |
| OpenAI message text / image extraction | `extractOpenAIMessageText`, `extractImageFromOpenAIPart` | translator.go:1400 / 2138 |
| Bedrock body pin | `buildBedrockBody` | bedrock.go:110 |
| Stream re-emit loop + usage capture | `invokeBedrockStream` (mirror, swap the write callback) | bedrock.go:160 |
| Billing | `recordBedrockSuccess` | bedrock.go:304 |

The OpenAI **request→Anthropic Messages** direction has no exact existing
converter (repo has OpenAI→Kiro, not OpenAI→Anthropic). Build a focused
`openaiToAnthropicBedrockBody`, reusing the content/tool helpers above.

## Files
- **New** `proxy/bedrock_openai.go`:
  - `openaiToAnthropicBedrockBody(rawOpenAI []byte) ([]byte, error)` — OpenAI
    messages→Anthropic messages/system, `tools`→Anthropic `input_schema` tools,
    `max_tokens` (default when absent — Anthropic requires it), temperature/top_p.
  - `(h *Handler) invokeBedrockOpenAIStream(w, flusher, p forwardParams) error` —
    mirrors `invokeBedrockStream`; the `readBedrockEventStream` callback feeds an
    `anthropicToOpenAIStreamState` that writes `chat.completion.chunk` SSE, then
    emits a terminal chunk with `finish_reason` and `data: [DONE]`.
  - `(h *Handler) invokeBedrockOpenAINonStream(w, p forwardParams) error` — invoke
    non-stream, convert the Anthropic Messages JSON → `chat.completion`.
  - `anthropicToOpenAIStreamState` — carries the tool-call index/id/name so
    streamed `input_json_delta` fragments attach to the right `tool_calls[i]`.
  - `anthropicMessageToOpenAIResponse(anthropicJSON []byte, model string) (map, in, out int)`.
- **Edit** `proxy/handler.go`: replace the two Bedrock `continue` skips at the
  OpenAI sites (1879, 2305) with dispatch branches mirroring the Anthropic
  contract — success `return`s; pre-stream error sets `excluded[account.ID]`,
  calls `handleAccountFailure`, `continue`s; post-first-byte error is logged only.
- No change to `bedrock.go` internals beyond exporting nothing new (call existing
  unexported helpers from the same package).

## Mapping table — Anthropic-native event → OpenAI

### Stream (`content_block_*` / `message_*` → `chat.completion.chunk`)
| Anthropic event (our Bedrock side) | OpenAI chunk output |
|---|---|
| `message_start` (usage.input_tokens) | capture input tokens; emit role chunk `delta:{role:"assistant"}` |
| `content_block_start` type `text` | (nothing, or open text) |
| `content_block_delta` `text_delta` | `delta:{content: <text>}` |
| `content_block_delta` `thinking_delta` | `delta:{reasoning_content: <text>}` |
| `content_block_start` type `tool_use` (id,name) | `delta:{tool_calls:[{index,id,type:"function",function:{name,arguments:""}}]}` |
| `content_block_delta` `input_json_delta` (partial_json) | `delta:{tool_calls:[{index,function:{arguments:<partial>}}]}` |
| `content_block_stop` | (close current block; bump tool index) |
| `message_delta` (stop_reason, usage.output_tokens) | capture output tokens + finish_reason |
| `message_stop` | terminal chunk `delta:{}` + `finish_reason` + `data: [DONE]` |

### stop_reason → finish_reason (borrowed verbatim from the sample's table)
| Anthropic `stop_reason` | OpenAI `finish_reason` |
|---|---|
| `end_turn` | `stop` |
| `stop_sequence` | `stop` |
| `max_tokens` | `length` |
| `tool_use` | `tool_calls` |
| (content filter, if surfaced) | `content_filter` |
| unknown | pass through lowercased |

### Non-stream (Anthropic Messages JSON → chat.completion)
| Anthropic field | OpenAI field |
|---|---|
| `content[].text` (joined) | `choices[0].message.content` |
| `content[]` type `tool_use` {id,name,input} | `choices[0].message.tool_calls[]` {id,type:function,function:{name,arguments:json(input)}}; content→null |
| `content[]` type `thinking` | `message.reasoning_content` |
| `stop_reason` | `choices[0].finish_reason` (table above) |
| `usage.input_tokens` | `usage.prompt_tokens` |
| `usage.output_tokens` | `usage.completion_tokens` |

## Test strategy (table-driven + synthetic stream)
`proxy/bedrock_openai_test.go`:
1. `TestOpenAIToAnthropicBedrockBody` — table: plain text, multi-turn, system
   message, tools present (schema sanitized), missing `max_tokens` gets a default,
   temperature/top_p carried.
2. `TestAnthropicMessageToOpenAIResponse` — table: plain answer; tool_use →
   tool_calls + null content + finish_reason `tool_calls`; each stop_reason maps
   correctly; usage mapped.
3. `TestBedrockOpenAIStreamSynthetic` — reuse `buildFrame` helper from
   `bedrock_eventstream_test.go`: feed a synthetic Anthropic frame sequence
   (`message_start` → `content_block_start`(text) → `content_block_delta`×N →
   `content_block_start`(tool_use) → `input_json_delta`×N → `message_delta`
   (stop_reason tool_use) → `message_stop`) through the stream state into an
   `httptest.ResponseRecorder`; assert: chunk ordering, tool_calls index/id/args
   reassembly, terminal `finish_reason`, trailing `data: [DONE]`, and
   input/output token accounting.
4. Failover: pre-stream converter/invoke error returns error (account excluded);
   post-first-byte error logged not failed-over — assert via injected fake reader.
5. `go build ./... && go vet ./... && go test ./...` clean.

## Open questions for you
- Scope of tool-calling on the OpenAI surface — include streamed tool_calls now
  (designed above) or ship text/reasoning first and add tools in a follow-up?
- `reasoning_content` format: match existing `thinkingFormat` config
  (`reasoning_content` vs `<think>` tags) used by `KiroToOpenAIResponseWithReasoning`?

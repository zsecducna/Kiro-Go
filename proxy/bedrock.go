package proxy

// Native Amazon Bedrock Runtime provider.
//
// A "bedrock" account (AuthMethod == "bedrock") holds a static IAM access key +
// region and calls the Bedrock Runtime InvokeModel / InvokeModelWithResponseStream
// endpoints directly, SigV4-signed. Because Bedrock speaks the native Anthropic
// Messages wire format for Claude models, the incoming client request needs only
// light rewriting (drop model/stream, pin anthropic_version) and the streaming
// response's inner events are re-emitted to the client unchanged — the same
// transparent-passthrough shape the custom_api forwarder uses, so it slots into the
// dispatch loop the same way.
//
// This file is self-contained: model-id resolution, request building, the two
// invoke paths, and per-request token billing against the customer API key.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// bedrockService is the SigV4 service name for the Bedrock data plane.
const bedrockService = "bedrock"

// bedrockAnthropicVersion is the required version tag in the request body for
// Anthropic models on Bedrock.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// bedrockHTTPClient is a dedicated client for Bedrock calls. Five-minute timeout
// matches the Kiro path (long streams); per-account outbound proxy is honored via
// GetClientForProxy when the account sets ProxyURL.
func bedrockHTTPClient(account *config.Account) *http.Client {
	if account != nil && strings.TrimSpace(account.ProxyURL) != "" {
		return GetClientForProxy(account.ProxyURL)
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// bedrockHTTPClientFor is the seam invokeBedrockRegional uses to obtain its HTTP
// client. It defaults to bedrockHTTPClient; tests override it to inject a hermetic
// client (avoiding the process-wide http.ProxyFromEnvironment cache).
var bedrockHTTPClientFor = bedrockHTTPClient

// defaultBedrockModelMap maps common Anthropic model aliases the client might send
// to Bedrock inference-profile IDs. It is a CONVENIENCE default only and is fully
// overridable per-account (Account.BedrockModelMap) or globally (env
// BEDROCK_MODEL_MAP, a JSON object). Model IDs on Bedrock change over time and vary
// by region/account enablement, so operators should confirm these against their own
// enabled models; anything already looking like a Bedrock id is passed through
// untouched by resolveBedrockModelID regardless of this map.
var defaultBedrockModelMap = map[string]string{
	"claude-3-5-sonnet": "us.anthropic.claude-3-5-sonnet-20241022-v2:0",
	"claude-3-5-haiku":  "us.anthropic.claude-3-5-haiku-20241022-v1:0",
	"claude-3-7-sonnet": "us.anthropic.claude-3-7-sonnet-20250219-v1:0",
	"claude-sonnet-4":   "us.anthropic.claude-sonnet-4-20250514-v1:0",
	"claude-sonnet-4-5": "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-opus-4":     "us.anthropic.claude-opus-4-20250514-v1:0",
	"claude-opus-4-1":   "us.anthropic.claude-opus-4-1-20250805-v1:0",
	"claude-haiku-4-5":  "us.anthropic.claude-haiku-4-5-20251001-v1:0",
}

// looksLikeBedrockModelID reports whether s is already a Bedrock model or inference
// profile id (so it should be used verbatim). Concrete ids carry a vendor prefix
// segment (optionally region-prefixed) and a version suffix with a colon, e.g.
// "anthropic.claude-...-v2:0", "us.amazon.nova-pro-v1:0",
// "meta.llama3-70b-instruct-v1:0", "us.deepseek.r1-v1:0". Convenience aliases such
// as "claude-3-5-sonnet" have neither a "." nor a ":", so this stays specific
// enough not to swallow them while still matching non-Anthropic ids (needed for
// the Converse path).
func looksLikeBedrockModelID(s string) bool {
	return strings.Contains(s, ":") && strings.Contains(s, ".")
}

// resolveBedrockModelID turns the client-requested model into a Bedrock model id.
// Precedence: per-account map, then env BEDROCK_MODEL_MAP, then pass-through if it
// already looks like a Bedrock id, then the built-in default map. Returns an error
// only when none of these resolve, so the operator gets a clear "configure the map"
// signal instead of a confusing Bedrock 400.
func resolveBedrockModelID(account *config.Account, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", fmt.Errorf("bedrock: empty model")
	}
	if account != nil && account.BedrockModelMap != nil {
		if id, ok := account.BedrockModelMap[requested]; ok && id != "" {
			return id, nil
		}
	}
	if raw := strings.TrimSpace(os.Getenv("BEDROCK_MODEL_MAP")); raw != "" {
		var envMap map[string]string
		if err := json.Unmarshal([]byte(raw), &envMap); err == nil {
			if id, ok := envMap[requested]; ok && id != "" {
				return id, nil
			}
		}
	}
	// Discovered callable models (populated by apiGetAccountModels / background
	// discovery): exact or unique-alias match against what this account can
	// actually invoke. Reads the cache only, never the network.
	if account != nil {
		if id := discoveredBedrockModelFor(account.ID, requested); id != "" {
			return id, nil
		}
	}
	if looksLikeBedrockModelID(requested) {
		return requested, nil
	}
	// Last-resort convenience defaults (may not be callable by this account).
	if id, ok := defaultBedrockModelMap[requested]; ok {
		return id, nil
	}
	return "", fmt.Errorf("bedrock: no model mapping for %q (set account BedrockModelMap or BEDROCK_MODEL_MAP)", requested)
}

// buildBedrockBody rewrites the incoming Anthropic-format request body for Bedrock:
// removes "model" and "stream" (model goes in the URL, stream is chosen by
// endpoint) and pins anthropic_version. All other fields (messages, system,
// max_tokens, temperature, tools, thinking, ...) pass through unchanged.
func buildBedrockBody(rawBody []byte) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &m); err != nil {
		return nil, fmt.Errorf("bedrock: invalid request body: %w", err)
	}
	delete(m, "model")
	delete(m, "stream")
	m["anthropic_version"], _ = json.Marshal(bedrockAnthropicVersion)
	return json.Marshal(m)
}

// bedrockCredsFor extracts the static IAM credentials from a bedrock account.
func bedrockCredsFor(account *config.Account) (awsCredentials, error) {
	ak := strings.TrimSpace(account.BedrockAccessKeyID)
	sk := strings.TrimSpace(account.BedrockSecretAccessKey)
	if ak == "" || sk == "" {
		return awsCredentials{}, fmt.Errorf("bedrock: account %s missing access key or secret", account.ID)
	}
	return awsCredentials{
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		SessionToken:    strings.TrimSpace(account.BedrockSessionToken),
	}, nil
}

// authorizeBedrockRequest attaches authentication to a Bedrock request in place.
//
// Two credential styles are supported. When the account carries a Bedrock API key
// (a bearer token, "ABSK..." — set via BedrockAPIKey), it is used verbatim as an
// `Authorization: Bearer` header and SigV4 is skipped entirely; this is the newer
// Bedrock API-key auth and works for principals whose raw IAM access key is denied
// InvokeModel. Otherwise the account's static IAM access key SigV4-signs the request
// over payload. region is the SigV4 region (also the request host's region).
//
// Returns an error only when neither credential is usable, so callers surface a
// pre-stream failure and fail over.
func authorizeBedrockRequest(account *config.Account, req *http.Request, payload []byte, region string) error {
	if key := strings.TrimSpace(account.BedrockAPIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
		return nil
	}
	creds, err := bedrockCredsFor(account)
	if err != nil {
		return err
	}
	signSigV4(req, payload, creds, region, bedrockService, time.Now())
	return nil
}

// bedrockRegionFor returns the account's region, defaulting to us-east-1.
func bedrockRegionFor(account *config.Account) string {
	if r := strings.TrimSpace(account.Region); r != "" {
		return r
	}
	return "us-east-1"
}

// bedrockEndpoint builds the invoke URL for a model id and streaming flag.
func bedrockEndpoint(region, modelID string, streaming bool) string {
	verb := "invoke"
	if streaming {
		verb = "invoke-with-response-stream"
	}
	// modelID is placed raw here; SigV4 canonicalization percent-encodes the path,
	// and net/http encodes the outgoing request-target consistently because we set
	// URL.Opaque below in the caller. See invokeBedrock*.
	return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/%s", region, modelID, verb)
}

// bedrockTestReply runs a minimal non-streaming Bedrock call for the admin
// account-test panel and returns the assistant's reply text. It uses the Converse
// path for converse accounts and native invoke otherwise, so the test exercises
// the same code path a real request would.
func (h *Handler) bedrockTestReply(account *config.Account, model string) (string, error) {
	p := forwardParams{account: account, model: model}
	anthropicBody := []byte(`{"anthropic_version":"` + bedrockAnthropicVersion + `","max_tokens":16,"messages":[{"role":"user","content":"Reply with exactly: OK"}]}`)

	var resp *http.Response
	var err error
	if accountUsesConverse(account) {
		conv, cerr := anthropicToConverseBody(anthropicBody)
		if cerr != nil {
			return "", cerr
		}
		resp, err = h.doBedrockConverseInvoke(p, conv, false)
	} else {
		resp, err = h.doBedrockInvoke(p, anthropicBody, false)
	}
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// Both paths yield an Anthropic Messages response (Converse is converted); pull
	// the text out for display.
	anthropicJSON := respBody
	if accountUsesConverse(account) {
		converted, _, _, cerr := converseResponseToAnthropicMessage(respBody, model)
		if cerr != nil {
			return "", cerr
		}
		anthropicJSON = converted
	}
	return extractAnthropicReplyText(anthropicJSON), nil
}

// extractAnthropicReplyText joins the text blocks of an Anthropic Messages
// response body.
func extractAnthropicReplyText(body []byte) string {
	var m struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var parts []string
	for _, c := range m.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "")
}

// doBedrockInvoke builds, signs, and sends a Bedrock native-invoke request for an
// already-Anthropic-format body. Model resolution, body rewrite and per-account HTTP
// client live here; region selection, auth, throttle and the 429/access-error
// handling are delegated to invokeBedrockRegional so the account's candidate regions
// are tried and the callable one is learned/cached. The returned response body is
// the caller's to close. All returned errors occur before any client bytes, so
// callers may fail over.
func (h *Handler) doBedrockInvoke(p forwardParams, anthropicBody []byte, streaming bool) (*http.Response, error) {
	modelID, err := resolveBedrockModelID(p.account, p.model)
	if err != nil {
		return nil, err
	}
	body, err := buildBedrockBody(anthropicBody)
	if err != nil {
		return nil, err
	}
	return h.invokeBedrockRegional(p, modelID, body, func(region string) (*http.Request, error) {
		req, err := newBedrockRequest(region, modelID, streaming, body)
		if err != nil {
			return nil, err
		}
		// Bedrock returns AWS event-stream framing for streaming invokes and plain
		// JSON otherwise; set Accept to match so the wire format is unambiguous.
		if streaming {
			req.Header.Set("Accept", "application/vnd.amazon.eventstream")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		return req, nil
	})
}

// invokeBedrockStream performs a streaming Bedrock call and re-emits each inner
// Anthropic event to the client as SSE, then bills the customer key. It mirrors
// forwardToUpstream's contract: returns nil once a response has been (at least
// partially) streamed, or an error before any client bytes so the caller can fail
// over to another account.
func (h *Handler) invokeBedrockStream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error {
	// Non-Anthropic models are served via the Converse API instead of native invoke.
	if accountUsesConverse(p.account) {
		return h.invokeBedrockConverseAnthropicStream(w, flusher, p)
	}
	reqStart := time.Now()

	resp, err := h.doBedrockInvoke(p, p.body, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	var inputTokens, outputTokens int
	var streamedAny bool

	streamErr := readBedrockEventStream(resp.Body, func(eventType string, anthropicJSON []byte) error {
		// Track usage from the native events without disturbing passthrough.
		switch {
		case bytes.Contains(anthropicJSON, []byte(`"message_start"`)):
			if it := extractInputTokens(anthropicJSON); it > 0 {
				inputTokens = it
			}
		case bytes.Contains(anthropicJSON, []byte(`"message_delta"`)):
			if ot := extractOutputTokens(anthropicJSON); ot > 0 {
				outputTokens = ot
			}
		}

		// Derive the SSE event name from the inner event when the frame header
		// didn't carry one (Bedrock sets :event-type "chunk", not the Anthropic type).
		evtName := eventType
		if evtName == "" || evtName == "chunk" {
			evtName = innerEventType(anthropicJSON)
		}
		streamedAny = true
		_, werr := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evtName, anthropicJSON)
		if werr != nil {
			return werr // client disconnected; stop reading
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})

	if streamErr != nil && !streamedAny {
		// Failed before any client bytes → allow account failover.
		return streamErr
	}
	if streamErr != nil {
		// Partial stream: log but treat as served (client already got a prefix).
		logger.Warnf("[Bedrock] stream ended with error after partial output (account %s): %v", p.account.ID, streamErr)
	}

	h.recordBedrockSuccess(p, inputTokens, outputTokens, reqStart)
	return nil
}

// invokeBedrockNonStream performs a non-streaming Bedrock call, writes the JSON
// response to the client, and bills the customer key.
func (h *Handler) invokeBedrockNonStream(w http.ResponseWriter, p forwardParams) error {
	// Non-Anthropic models are served via the Converse API instead of native invoke.
	if accountUsesConverse(p.account) {
		return h.invokeBedrockConverseAnthropicNonStream(w, p)
	}
	reqStart := time.Now()

	resp, err := h.doBedrockInvoke(p, p.body, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	inputTokens, outputTokens := extractNonStreamUsage(respBody)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)

	h.recordBedrockSuccess(p, inputTokens, outputTokens, reqStart)
	return nil
}

// newBedrockRequest builds the HTTP request with the model id carried as an opaque
// request-target so net/http does not re-encode the "%3A" in inference-profile ids.
// URL.Path is set to the decoded path so SigV4's canonicalizer sees the raw ":".
func newBedrockRequest(region, modelID string, streaming bool, body []byte) (*http.Request, error) {
	return newBedrockRequestForURL(bedrockEndpoint(region, modelID, streaming), body)
}

// newBedrockRequestForURL builds a POST request to an arbitrary Bedrock URL with
// the given body. Shared by the invoke and Converse paths so both construct the
// request identically before SigV4 signing.
func newBedrockRequestForURL(rawURL string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bedrock: build request: %w", err)
	}
	return req, nil
}

// recordBedrockSuccess bills the customer API key by tokens and updates account +
// pool stats, mirroring recordCustomApiSuccess. Credits are derived from a
// configurable per-1k-token rate purely for operator accounting/analytics; the
// customer key's TokenLimit is the real quota gate (RecordApiKeyUsage enforces it).
func (h *Handler) recordBedrockSuccess(p forwardParams, inputTokens, outputTokens int, reqStart time.Time) {
	endpoint := "claude"
	if p.endpoint == "openai" || p.endpoint == "responses" {
		endpoint = "openai"
	}
	credits := bedrockCreditsForTokens(inputTokens + outputTokens)
	h.recordSuccessForApiKey(p.apiKeyID, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(p.account.ID)
	h.pool.RecordLatency(p.account.ID, float64(time.Since(reqStart).Milliseconds()))
	h.pool.UpdateStats(p.account.ID, inputTokens+outputTokens, credits)
	h.recordSuccessLog(endpoint, p.model, p.account.ID, p.apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
}

// bedrockCreditsForTokens converts a token count to operator credits using
// BEDROCK_CREDITS_PER_1K_TOKENS (default 0 → credits accounting disabled, pure
// token billing). Token-based keys work regardless of this value.
func bedrockCreditsForTokens(tokens int) float64 {
	rate := envFloatDefault("BEDROCK_CREDITS_PER_1K_TOKENS", 0)
	if rate <= 0 {
		return 0
	}
	return float64(tokens) / 1000.0 * rate
}

// envFloatDefault reads a float from env var name, returning def when unset or
// unparsable.
func envFloatDefault(name string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// ---- small JSON usage extractors (tolerant, allocation-light) ----

// extractInputTokens reads message.usage.input_tokens from a message_start event.
func extractInputTokens(eventJSON []byte) int {
	var e struct {
		Message struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(eventJSON, &e) != nil {
		return 0
	}
	return e.Message.Usage.InputTokens
}

// extractOutputTokens reads usage.output_tokens from a message_delta event.
func extractOutputTokens(eventJSON []byte) int {
	var e struct {
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(eventJSON, &e) != nil {
		return 0
	}
	return e.Usage.OutputTokens
}

// extractNonStreamUsage reads usage.{input,output}_tokens from a full response.
func extractNonStreamUsage(respJSON []byte) (int, int) {
	var r struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(respJSON, &r) != nil {
		return 0, 0
	}
	return r.Usage.InputTokens, r.Usage.OutputTokens
}

// innerEventType reads the "type" field of an Anthropic event for SSE naming.
func innerEventType(eventJSON []byte) string {
	var e struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(eventJSON, &e) != nil || e.Type == "" {
		return "message"
	}
	return e.Type
}

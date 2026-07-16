package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// customApiCreditsPer1kTokens is the operator-configured credit price for traffic
// served by a Custom API (pool-linking) account. The upstream pool's reply carries
// no credit figure, so the customer's key is billed from token usage at this rate —
// which represents the OPERATOR's own resale pricing, independent of what the
// upstream pool charges them. Without this, credit-limited keys would never exhaust
// on forwarded traffic (free usage). Override with CUSTOM_API_CREDITS_PER_1K_TOKENS;
// default 1.0 credit per 1000 tokens.
func customApiCreditsPer1kTokens() float64 {
	if v := strings.TrimSpace(os.Getenv("CUSTOM_API_CREDITS_PER_1K_TOKENS")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return 1.0
}

// custom_api "pool linking" support. A custom_api account is a transparent proxy
// to ANOTHER Kiro-Go pool: incoming Anthropic/OpenAI requests are forwarded to
// that pool's base URL using a key it issued to us, instead of being translated
// into Amazon CodeWhisperer calls. This file holds the add-time quota probe and
// the runtime forwarding path.

// forwardHeader marks a request that has already been forwarded once, so a
// downstream pool that links back to us refuses to add another custom_api hop.
const forwardHeader = "X-KiroGo-Forwarded"

// customApiQuota is the subset of an upstream Kiro-Go pool's GET /api/me
// response used to gate adding a Custom API account. OK is false when the
// upstream returned a non-2xx status (bad/expired key).
type customApiQuota struct {
	CreditsRemaining float64
	TokensRemaining  int64
	CreditLimit      float64
	TokenLimit       int64
	OK               bool
}

// probeCustomApiQuota calls {baseURL}/api/me with the supplied bearer token and
// returns the parsed quota. It is a package var so tests can stub the round-trip.
// A 5s timeout keeps a dead upstream from hanging the add-account request.
var probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
	url := strings.TrimRight(baseURL, "/") + "/api/me"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	// Direct transport (Proxy nil): pool-to-pool calls go straight out and, unlike the
	// shared http.DefaultTransport, never touch the process-global ProxyFromEnvironment
	// cache — keeping this add-time probe independent of egress-proxy env state.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return &customApiQuota{OK: false}, fmt.Errorf("upstream /api/me returned %d", resp.StatusCode)
	}
	var raw struct {
		CreditsRemaining float64 `json:"creditsRemaining"`
		TokensRemaining  int64   `json:"tokensRemaining"`
		CreditLimit      float64 `json:"creditLimit"`
		TokenLimit       int64   `json:"tokenLimit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return &customApiQuota{OK: false}, fmt.Errorf("upstream /api/me: bad JSON: %w", err)
	}
	return &customApiQuota{
		CreditsRemaining: raw.CreditsRemaining,
		TokensRemaining:  raw.TokensRemaining,
		CreditLimit:      raw.CreditLimit,
		TokenLimit:       raw.TokenLimit,
		OK:               true,
	}, nil
}

// customApiQuotaAcceptable is the add-time gate: accept when the upstream key is
// valid and either has remaining quota, or is unlimited (both limits zero).
func customApiQuotaAcceptable(q *customApiQuota) bool {
	if q == nil || !q.OK {
		return false
	}
	if q.CreditLimit == 0 && q.TokenLimit == 0 {
		return true // unlimited key
	}
	return q.CreditsRemaining > 0 || q.TokensRemaining > 0
}

// normalizeBaseURL validates and canonicalizes an upstream base URL so it is always
// safe to concatenate with /v1/... paths. Requires an http(s) scheme and a non-empty
// host, and rejects query/fragment (a base URL must not carry them). Returns
// scheme://host + trimmed path.
func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("baseUrl is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("baseUrl must start with http:// or https://")
	}
	if u.Host == "" {
		return "", fmt.Errorf("baseUrl must include a host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("baseUrl must not contain a query or fragment")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), nil
}

// forwardParams bundles everything forwardToUpstream needs from a dispatch handler.
type forwardParams struct {
	account   *config.Account
	body      []byte
	endpoint  string // "anthropic" | "openai"
	streaming bool
	model     string
	apiKeyID  string
	forwarded bool // incoming request already carried forwardHeader
}

// upstreamPath maps the incoming API surface to the upstream pool's path.
func upstreamPath(endpoint string) string {
	switch endpoint {
	case "openai":
		return "/v1/chat/completions"
	case "responses":
		return "/v1/responses"
	default:
		return "/v1/messages"
	}
}

// forwardUpstreamRequest performs the actual round-trip to the upstream pool. It
// is a package var so tests can stub it; the default does a real HTTP call.
var forwardUpstreamRequest = func(method, url, apiKey string, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set(forwardHeader, "1")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	// Direct transport (Proxy nil): pool-to-pool forwarding goes straight out and
	// avoids coupling to the process-global ProxyFromEnvironment cache that
	// http.DefaultTransport shares. Streaming replies are long-lived so they get no
	// overall timeout; non-stream replies get a ceiling so a hung upstream can't pin
	// the request forever.
	client := &http.Client{Transport: &http.Transport{}}
	if !stream {
		client.Timeout = 120 * time.Second
	}
	return client.Do(req)
}

// parseUpstreamUsage best-effort extracts token counts from a JSON reply (or a
// single SSE data payload). Anthropic uses usage.input_tokens/output_tokens;
// OpenAI uses usage.prompt_tokens/completion_tokens.
func parseUpstreamUsage(endpoint string, body []byte) (int, int) {
	var raw struct {
		Usage struct {
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, 0
	}
	if endpoint == "openai" {
		return raw.Usage.PromptTokens, raw.Usage.CompletionTokens
	}
	return raw.Usage.InputTokens, raw.Usage.OutputTokens
}

// forwardToUpstream proxies the raw request to the account's upstream pool. On any
// failure BEFORE a 2xx reply it returns an error and writes nothing, so the caller
// can fail over to another account. After a 2xx it writes the reply and returns nil.
func (h *Handler) forwardToUpstream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error {
	// Loop guard: never add a second hop to an already-forwarded request.
	if p.forwarded {
		return fmt.Errorf("custom_api: refusing to forward an already-forwarded request (loop guard)")
	}

	reqStart := time.Now()
	url := strings.TrimRight(p.account.BaseURL, "/") + upstreamPath(p.endpoint)
	resp, err := forwardUpstreamRequest(http.MethodPost, url, p.account.KiroApiKey, p.body, p.streaming)
	if err != nil {
		return fmt.Errorf("custom_api upstream connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Log the upstream body for the operator, but return a status-only error: this
		// error can bubble to the end customer on failover exhaustion, and the linked
		// pool's raw error body must not leak downstream. The status code is preserved
		// so the ban classifier still sees 401/403 correctly. No client write here, so
		// the caller can fail over to another account.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		logger.Warnf("[CustomApi] upstream %s HTTP %d: %s", p.account.ID, resp.StatusCode, strings.TrimSpace(string(msg)))
		return fmt.Errorf("custom_api upstream HTTP %d", resp.StatusCode)
	}

	if p.streaming {
		return h.streamUpstream(w, flusher, resp, p, reqStart)
	}

	// Non-streaming: copy the JSON reply verbatim, then meter. Cap the read so a
	// misbehaving upstream cannot balloon memory (32 MiB is far above any real reply).
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("custom_api upstream read: %w", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(body)

	in, out := parseUpstreamUsage(p.endpoint, body)
	h.recordCustomApiSuccess(p, in, out, reqStart)
	return nil
}

// streamUpstream copies the upstream SSE stream to the client byte-for-byte,
// flushing as data arrives, and taps token usage from the stream on the way past.
// Called only after a 2xx upstream status, so headers are safe to commit here.
func (h *Handler) streamUpstream(w http.ResponseWriter, flusher http.Flusher, resp *http.Response, p forwardParams, reqStart time.Time) error {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// tail keeps a bounded window of the most recent stream bytes so we can parse
	// the terminal usage event without holding the entire stream in memory.
	var tail bytes.Buffer
	const tailCap = 64 * 1024
	buf := make([]byte, 16*1024)
	var streamErr error
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := w.Write(chunk); werr != nil {
				return nil // client gone; nothing to fail over to
			}
			if flusher != nil {
				flusher.Flush()
			}
			tail.Write(chunk)
			if tail.Len() > tailCap {
				trimmed := append([]byte(nil), tail.Bytes()[tail.Len()-tailCap:]...)
				tail.Reset()
				tail.Write(trimmed)
			}
		}
		if readErr != nil {
			// io.EOF is a clean end; any other error is a mid-stream upstream drop.
			if readErr != io.EOF {
				streamErr = readErr
			}
			break
		}
	}

	// Mid-stream failure: the client already has committed headers and a partial
	// stream, so we cannot fail over. Emit a best-effort terminal error event (as the
	// Kiro path does) and record a FAILURE — never a success — so pool health reflects
	// reality. Tokens from a truncated stream are not metered.
	if streamErr != nil {
		msg := "custom_api upstream stream interrupted: " + streamErr.Error()
		if p.endpoint == "anthropic" {
			// Anthropic SSE error event shape.
			fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%q}}\n\n", msg)
		} else {
			// OpenAI (chat + responses) SSE error chunk shape.
			fmt.Fprintf(w, "data: {\"error\":{\"type\":\"api_error\",\"message\":%q}}\n\n", msg)
		}
		if flusher != nil {
			flusher.Flush()
		}
		endpoint := "claude"
		if p.endpoint == "openai" || p.endpoint == "responses" {
			endpoint = "openai"
		}
		h.pool.RecordError(p.account.ID, false)
		h.recordFailureWithDetails(endpoint, p.model, p.account.ID, p.apiKeyID, streamErr)
		return nil
	}

	in, out := parseStreamUsage(p.endpoint, tail.Bytes())
	h.recordCustomApiSuccess(p, in, out, reqStart)
	return nil
}

// parseStreamUsage scans SSE tail bytes for the last usage object. Anthropic emits
// usage in message_delta; OpenAI emits it in the final chunk when stream_options
// include_usage is set. Best-effort: returns 0,0 if none found.
func parseStreamUsage(endpoint string, tail []byte) (int, int) {
	lines := bytes.Split(tail, []byte("\n"))
	var lastIn, lastOut int
	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if !bytes.HasPrefix(ln, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(ln, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if in, out := parseUpstreamUsage(endpoint, payload); in != 0 || out != 0 {
			if in != 0 {
				lastIn = in
			}
			if out != 0 {
				lastOut = out
			}
		}
	}
	return lastIn, lastOut
}

// recordCustomApiSuccess mirrors the Kiro success path's metering so custom_api
// traffic shows up in pool stats, per-key usage, and request logs. Credits are
// left at 0 (the upstream pool bills us separately); token counts drive local stats.
func (h *Handler) recordCustomApiSuccess(p forwardParams, inputTokens, outputTokens int, reqStart time.Time) {
	endpoint := "claude"
	if p.endpoint == "openai" || p.endpoint == "responses" {
		endpoint = "openai"
	}
	// The upstream reply has no credit figure; bill the customer key from tokens at
	// the operator's configured resale rate so credit-limited keys actually deplete.
	credits := float64(inputTokens+outputTokens) / 1000.0 * customApiCreditsPer1kTokens()
	h.recordSuccessForApiKey(p.apiKeyID, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(p.account.ID)
	h.pool.RecordLatency(p.account.ID, float64(time.Since(reqStart).Milliseconds()))
	h.pool.UpdateStats(p.account.ID, inputTokens+outputTokens, credits)
	h.recordSuccessLog(endpoint, p.model, p.account.ID, p.apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
}

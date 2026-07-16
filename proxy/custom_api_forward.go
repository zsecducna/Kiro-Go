package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kiro-go/config"
)

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

// normalizeBaseURL trims a trailing slash and requires an http(s) scheme so a
// stored base URL is always safe to concatenate with /v1/... paths.
func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return "", fmt.Errorf("baseUrl must start with http:// or https://")
	}
	return strings.TrimRight(s, "/"), nil
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
	if endpoint == "openai" {
		return "/v1/chat/completions"
	}
	return "/v1/messages"
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
	// No client timeout: streaming replies are long-lived. Failover on connect
	// errors is handled by the caller. Direct transport (Proxy nil): pool-to-pool
	// forwarding goes straight out and avoids coupling to the process-global
	// ProxyFromEnvironment cache that http.DefaultTransport shares.
	client := &http.Client{Transport: &http.Transport{}}
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
		// Drain a little for the error message; do NOT write to the client so the
		// caller can fail over to another account.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("custom_api upstream HTTP %d: %s", resp.StatusCode, string(msg))
	}

	if p.streaming {
		return h.streamUpstream(w, flusher, resp, p, reqStart)
	}

	// Non-streaming: copy the JSON reply verbatim, then meter.
	body, err := io.ReadAll(resp.Body)
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
			break // io.EOF or upstream drop; stream already delivered what it had
		}
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
	if p.endpoint == "openai" {
		endpoint = "openai"
	}
	credits := 0.0
	h.recordSuccessForApiKey(p.apiKeyID, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(p.account.ID)
	h.pool.RecordLatency(p.account.ID, float64(time.Since(reqStart).Milliseconds()))
	h.pool.UpdateStats(p.account.ID, inputTokens+outputTokens, credits)
	h.recordSuccessLog(endpoint, p.model, p.account.ID, p.apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
}

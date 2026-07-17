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
	"sync"
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
	CreditsUsed      float64
	TokensRemaining  int64
	TokensUsed       int64
	CreditLimit      float64
	TokenLimit       int64
	OK               bool
}

// toAccountInfo maps an upstream /api/me quota onto the AccountInfo usage fields the
// admin panel renders (main quota = usageCurrent/usageLimit). Credit-based, matching
// how these accounts are sold. now is the LastRefresh timestamp.
func (q *customApiQuota) toAccountInfo(now int64) config.AccountInfo {
	info := config.AccountInfo{
		SubscriptionType: "Custom API",
		UsageCurrent:     q.CreditsUsed,
		UsageLimit:       q.CreditLimit,
		LastRefresh:      now,
	}
	if q.CreditLimit > 0 {
		info.UsagePercent = q.CreditsUsed / q.CreditLimit
	}
	return info
}

// quotaFields is the credit/token shape both upstream quota endpoints share
// (/api/me on a Kiro-Go pool, /checkkey/info on other pool software). `found` is
// only present on /checkkey/info; a false value means the key does not exist.
type quotaFields struct {
	Found            *bool   `json:"found"`
	CreditsRemaining float64 `json:"creditsRemaining"`
	CreditsUsed      float64 `json:"creditsUsed"`
	TokensRemaining  int64   `json:"tokensRemaining"`
	TokensUsed       int64   `json:"tokensUsed"`
	CreditLimit      float64 `json:"creditLimit"`
	TokenLimit       int64   `json:"tokenLimit"`
}

// toQuota converts the shared field shape into a customApiQuota.
func (f quotaFields) toQuota() *customApiQuota {
	return &customApiQuota{
		CreditsRemaining: f.CreditsRemaining,
		CreditsUsed:      f.CreditsUsed,
		TokensRemaining:  f.TokensRemaining,
		TokensUsed:       f.TokensUsed,
		CreditLimit:      f.CreditLimit,
		TokenLimit:       f.TokenLimit,
		OK:               true,
	}
}

// customApiHTTPClient is a direct-transport client (Proxy nil) so pool-to-pool
// probes go straight out and never touch the process-global ProxyFromEnvironment
// cache — keeping add-time probes independent of egress-proxy env state.
func customApiHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
}

// quotaEndpointPref caches which quota endpoint answered for a given baseURL. The
// per-request real-cost probe calls probeCustomApiQuota on the hot path, so without
// this a pool that only serves /checkkey/info would eat a wasted /api/me 404 on
// every single request. baseURL -> "apime" | "checkkey".
var quotaEndpointPref sync.Map

// probeCustomApiQuota fetches the upstream key's quota. It tries GET {baseURL}/api/me
// (Kiro-Go pools) first; if that is unavailable it falls back to POST
// {baseURL}/checkkey/info with {"key": apiKey} (other pool software). The endpoint
// that last worked for a baseURL is cached and tried first. Package var so tests can
// stub the round-trip.
var probeCustomApiQuota = func(baseURL, apiKey string) (*customApiQuota, error) {
	// Fast path: reuse the endpoint that last answered for this pool.
	if pref, ok := quotaEndpointPref.Load(baseURL); ok && pref == "checkkey" {
		if q, err := probeQuotaViaCheckKey(baseURL, apiKey); err == nil {
			return q, nil
		}
		// Cached endpoint stopped answering — fall through to the full probe.
	}
	q, err := probeQuotaViaApiMe(baseURL, apiKey)
	if err == nil {
		quotaEndpointPref.Store(baseURL, "apime")
		return q, nil
	}
	// /api/me unavailable — try the /checkkey/info fallback.
	q2, err2 := probeQuotaViaCheckKey(baseURL, apiKey)
	if err2 == nil {
		quotaEndpointPref.Store(baseURL, "checkkey")
		return q2, nil
	}
	return &customApiQuota{OK: false}, fmt.Errorf("quota check failed (api/me: %v; checkkey/info: %v)", err, err2)
}

// probeQuotaViaApiMe reads GET {baseURL}/api/me with the key as a bearer token.
func probeQuotaViaApiMe(baseURL, apiKey string) (*customApiQuota, error) {
	url := strings.TrimRight(baseURL, "/") + "/api/me"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	resp, err := customApiHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/api/me returned %d", resp.StatusCode)
	}
	var raw quotaFields
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("/api/me: bad JSON: %w", err)
	}
	return raw.toQuota(), nil
}

// probeQuotaViaCheckKey reads POST {baseURL}/checkkey/info with {"key": apiKey}. A
// "found": false response means the key does not exist on that upstream.
func probeQuotaViaCheckKey(baseURL, apiKey string) (*customApiQuota, error) {
	url := strings.TrimRight(baseURL, "/") + "/checkkey/info"
	reqBody, _ := json.Marshal(map[string]string{"key": apiKey})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := customApiHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/checkkey/info returned %d", resp.StatusCode)
	}
	var raw quotaFields
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("/checkkey/info: bad JSON: %w", err)
	}
	if raw.Found != nil && !*raw.Found {
		return nil, fmt.Errorf("/checkkey/info: key not found")
	}
	return raw.toQuota(), nil
}

// probeCustomApiModels fetches the model list from the upstream pool's OpenAI-style
// GET {baseURL}/v1/models (data:[{id}]) using the upstream key, and returns them as
// ModelInfo. Custom API accounts serve whatever the linked pool serves, so the model
// list must come from the upstream, not from Kiro/AWS. Package var so tests can stub.
var probeCustomApiModels = func(baseURL, apiKey string) ([]ModelInfo, error) {
	url := strings.TrimRight(baseURL, "/") + "/v1/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream /v1/models returned %d", resp.StatusCode)
	}
	var raw struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("upstream /v1/models: bad JSON: %w", err)
	}
	models := make([]ModelInfo, 0, len(raw.Data))
	for _, m := range raw.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		models = append(models, ModelInfo{ModelId: id, ModelName: id})
	}
	return models, nil
}

// customApiTestReply sends a minimal chat request through the linked upstream pool
// and returns the assistant reply text, so the admin "test account" button verifies
// the whole path (auth + routing) end-to-end. model defaults when empty.
func customApiTestReply(baseURL, apiKey, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		model = "claude-sonnet-4"
	}
	reqBody := map[string]interface{}{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "say ok"}},
		"max_tokens": 16,
		"stream":     false,
	}
	body, _ := json.Marshal(reqBody)
	url := strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
	resp, err := forwardUpstreamRequest(http.MethodPost, url, apiKey, body, false)
	if err != nil {
		return "", fmt.Errorf("custom_api test connect: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("custom_api upstream HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("custom_api test: bad JSON: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("custom_api test: empty response")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
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
// traffic shows up in pool stats, per-key usage, and request logs. Runs AFTER the
// client already has the full response, so the real-cost probe it does adds no
// client-visible latency.
func (h *Handler) recordCustomApiSuccess(p forwardParams, inputTokens, outputTokens int, reqStart time.Time) {
	endpoint := "claude"
	if p.endpoint == "openai" || p.endpoint == "responses" {
		endpoint = "openai"
	}
	// Bill the REAL cost the upstream pool deducted for this request, read as the
	// delta of its reported creditsUsed, instead of a flat token-derived price.
	credits := h.billCustomApiRealCost(p, inputTokens, outputTokens)
	h.recordSuccessForApiKey(p.apiKeyID, inputTokens, outputTokens, credits)
	h.pool.RecordSuccess(p.account.ID)
	h.pool.RecordLatency(p.account.ID, float64(time.Since(reqStart).Milliseconds()))
	h.pool.UpdateStats(p.account.ID, inputTokens+outputTokens, credits)
	h.recordSuccessLog(endpoint, p.model, p.account.ID, p.apiKeyID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())
}

// customApiRateTTL bounds how long a cached upstream price is reused before the next
// request re-probes. The pool's blended per-token rate moves slowly (it is a
// cumulative average), so a couple of minutes keeps billing current without a probe
// per request.
const customApiRateTTL = 2 * time.Minute

// customApiCreditLedger caches, per custom_api account, the upstream pool's blended
// per-token price (its cumulative creditsUsed / tokensUsed from /checkkey/info). Each
// request is billed reqTokens * that rate.
//
// Why a cached pool rate rather than per-request deltas or a running token-share: many
// customer keys share ONE upstream pool key and the pool reports only the key-wide
// cumulative creditsUsed. Charging the delta-since-last-probe hands one request the
// concurrent spend of every other customer (random huge/zero charges); a running
// realTotal/sumTokens average does not telescope to real spend when per-request rates
// vary (mixed models), so it drifts. The pool's own creditsUsed/tokensUsed ratio is a
// stable, authoritative price: billing each request its own tokens at that rate is
// smooth, fair across customers, needs no per-account accumulation, and has no
// seed/reset/ordering races. Guarded by a mutex covering only the cache map.
type customApiCreditLedger struct {
	mu    sync.Mutex
	rates map[string]customApiRate
}

type customApiRate struct {
	perToken float64   // upstream credits per token
	at       time.Time // when this rate was probed
}

func newCustomApiCreditLedger() *customApiCreditLedger {
	return &customApiCreditLedger{rates: make(map[string]customApiRate)}
}

// billCustomApiRealCost returns the credits to charge this request: its token count
// times the upstream pool's blended per-token price. The price is cached per account
// (customApiRateTTL); on a miss it probes the pool's quota API — after the client
// already has the response, so no client latency.
//
// Fallbacks: a stale cached rate if the refresh probe fails; otherwise the flat token
// estimate (CUSTOM_API_CREDITS_PER_1K_TOKENS) when no rate is known at all. Set that
// env near the pool's real per-1k rate so a cold cache does not spike.
//
// Accuracy note: aggregate billed = ourTokens * poolRate, where poolRate is computed
// from the pool's OWN token count. If our token counting under-reports relative to the
// pool's, this under-bills proportionally (safe direction). Fixing that requires
// aligning our token counts with the pool's, tracked separately.
func (h *Handler) billCustomApiRealCost(p forwardParams, inputTokens, outputTokens int) float64 {
	tokens := float64(inputTokens + outputTokens)
	estimate := tokens / 1000.0 * customApiCreditsPer1kTokens()

	l := h.customApiLedger
	if l == nil { // defensive: handlers built outside NewHandler (older tests)
		return estimate
	}

	now := time.Now()
	l.mu.Lock()
	cached, ok := l.rates[p.account.ID]
	fresh := ok && now.Sub(cached.at) < customApiRateTTL
	if !fresh && ok {
		// Claim this refresh window: bump the timestamp (keeping the old rate) before
		// probing so concurrent requests reuse the cached rate instead of all probing
		// at once, and so a FAILING probe backs off for a full TTL instead of every
		// request re-probing throughout an outage. A successful probe below overwrites
		// the rate. (Cold start with no cached entry still lets a one-time burst probe.)
		l.rates[p.account.ID] = customApiRate{perToken: cached.perToken, at: now}
	}
	l.mu.Unlock()
	if fresh {
		return tokens * cached.perToken
	}

	// Refresh the rate. Probe runs outside the lock — never hold the mutex across
	// network I/O.
	q, err := probeCustomApiQuota(p.account.BaseURL, p.account.KiroApiKey)
	if err == nil && q != nil && q.OK && q.TokensUsed > 0 && q.CreditsUsed > 0 {
		rate := q.CreditsUsed / float64(q.TokensUsed)
		l.mu.Lock()
		l.rates[p.account.ID] = customApiRate{perToken: rate, at: now}
		l.mu.Unlock()
		return tokens * rate
	}

	// Probe failed or reported no usable rate. Prefer a stale cached rate over the flat
	// estimate — it is still the pool's real price, just not freshly confirmed.
	if ok {
		logger.Warnf("[CustomApi] rate refresh failed for %s, reusing cached rate %.6g: %v", p.account.ID, cached.perToken, err)
		return tokens * cached.perToken
	}
	logger.Warnf("[CustomApi] no upstream rate for %s, billing estimate %.4f: %v", p.account.ID, estimate, err)
	return estimate
}

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

// customApiCreditLedger tracks, per custom_api account, the cumulative upstream
// credits already billed to customers. On each request it is reconciled against the
// upstream's authoritative creditsUsed so the sum billed converges exactly on what
// the pool actually deducted. Guarded by its own mutex; safe for concurrent requests
// sharing one upstream account.
//
// Attribution caveat: the reconciliation is per-ACCOUNT, but the charge posts to the
// requesting customer key. When multiple customer keys share one upstream account,
// each request is billed the whole delta accrued since the previous reconciliation —
// so concurrent requests from different keys can misattribute cost between them. The
// aggregate billed per account is still exact; only the per-key split is approximate,
// and only under shared-account concurrency (the common resale case is one customer
// key per upstream account, where it is exact).
type customApiCreditLedger struct {
	mu     sync.Mutex
	billed map[string]float64 // accountID -> cumulative upstream creditsUsed already billed
	seeded map[string]bool    // accountID -> baseline established
}

func newCustomApiCreditLedger() *customApiCreditLedger {
	return &customApiCreditLedger{
		billed: make(map[string]float64),
		seeded: make(map[string]bool),
	}
}

// billCustomApiRealCost returns the credits to charge this request. It probes the
// upstream quota API for the account's current creditsUsed and bills the delta since
// the last reconciliation. The ledger's stored value is the running total already
// billed, so a successful probe both charges (now - billed) and snaps the total to
// the pool's truth — self-correcting any earlier estimate drift (including downward:
// a negative delta clamps to 0, refunding prior over-bills as free requests).
//
// Fallback (probe failure, or first request with no baseline): the flat token-based
// estimate. On probe failure the estimate is added to the running total so it is
// reconciled away at the next successful probe; set CUSTOM_API_CREDITS_PER_1K_TOKENS
// close to the pool's real rate so this rare path does not spike.
func (h *Handler) billCustomApiRealCost(p forwardParams, inputTokens, outputTokens int) float64 {
	estimate := float64(inputTokens+outputTokens) / 1000.0 * customApiCreditsPer1kTokens()

	l := h.customApiLedger
	if l == nil { // defensive: handlers built outside NewHandler (older tests)
		return estimate
	}

	q, err := probeCustomApiQuota(p.account.BaseURL, p.account.KiroApiKey)
	l.mu.Lock()
	defer l.mu.Unlock()

	// Probe failed: bill the flat estimate and leave the baseline UNTOUCHED. The
	// request's real cost stays in the upstream's creditsUsed and is captured by the
	// next successful probe's delta (safe direction — the operator never under-bills
	// across a quota-endpoint outage). Never add estimate to billed: billed is in
	// upstream-credit units, estimate is a resale-credit figure — mixing them would
	// corrupt every subsequent delta.
	if err != nil || q == nil || !q.OK {
		logger.Warnf("[CustomApi] real-cost probe failed for %s, billing estimate %.4f: %v", p.account.ID, estimate, err)
		return estimate
	}

	prev, seeded := l.billed[p.account.ID], l.seeded[p.account.ID]

	// (Re)seed on the first probe for this account, or when creditsUsed has dropped
	// far below our baseline — an upstream recharge/reset. Seeding snaps the baseline
	// to the current truth and bills this one request by estimate, since no trustworthy
	// delta spans the gap. The 0.5 ratio separates a real reset (drops toward zero)
	// from a tiny out-of-order probe race (a sub-credit dip), which the clamp handles.
	if !seeded || (prev > 0 && q.CreditsUsed < prev*0.5) {
		l.seeded[p.account.ID] = true
		l.billed[p.account.ID] = q.CreditsUsed
		return estimate
	}

	// Steady state: bill the real delta since the last reconciliation. The baseline
	// only ever advances — never regresses — so a lower reading from a raced probe
	// bills 0 rather than re-charging credits already billed. Aggregate billed
	// converges on the pool's real spend.
	spent := q.CreditsUsed - prev
	if spent < 0 {
		spent = 0
	}
	if q.CreditsUsed > prev {
		l.billed[p.account.ID] = q.CreditsUsed
	}
	return spent
}

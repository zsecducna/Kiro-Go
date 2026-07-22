package proxy

// Region-aware Bedrock routing.
//
// Bedrock model access is granted PER REGION: the same credential that returns
// HTTP 400 "Operation not allowed" for a model in us-east-1 can return 200 for it
// in eu-west-1 (verified live). Neither ListFoundationModels nor
// GetFoundationModelAvailability reflects this — only an actual invoke does. So the
// server LEARNS which region a model is callable in from real invoke outcomes and
// routes there.
//
// Two mechanisms:
//   - Lazy routing (invokeBedrockRegional): a request tries the account's candidate
//     regions (a learned-good region first) until one is callable, then caches the
//     winning region per (account, model). "Operation not allowed"-class errors hop
//     to the next region; a genuine error (bad request, real 400) is returned as-is.
//   - Prewarm (prewarmBedrockRegions): an opt-in background canary that invokes each
//     discovered model once per candidate region to populate the same cache, so the
//     panel can show which region each model is callable in without waiting for live
//     traffic.
//
// An account with a single candidate region (the default) never hops and behaves
// exactly as before.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// bedrockRegionTTL bounds how long a learned (account,model)->region verdict is
// reused. bedrockRegionNegativeTTL is shorter so a not-callable-anywhere verdict
// recovers quickly once the operator grants access / fixes the region list.
const (
	bedrockRegionTTL         = 30 * time.Minute
	bedrockRegionNegativeTTL = 3 * time.Minute
	bedrockCanaryConcurrency = 6
)

// bedrockRegionRoute is a learned routing verdict for one (account, model).
type bedrockRegionRoute struct {
	region   string // the callable region; "" when callable==false
	callable bool
	expires  time.Time
}

var (
	bedrockRegionMu    sync.RWMutex
	bedrockRegionCache = map[string]map[string]bedrockRegionRoute{} // accountID -> modelID -> route
)

// candidateRegions returns the ordered, deduped region list to consider for an
// account: the primary Region first, then any extra BedrockRegions. This is the
// full search space for lazy routing and prewarm.
func candidateRegions(account *config.Account) []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		r = strings.TrimSpace(r)
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	add(bedrockRegionFor(account)) // primary (defaults to us-east-1)
	for _, r := range account.BedrockRegions {
		add(r)
	}
	return out
}

// cleanRegionList trims, drops blanks, and dedupes a region list (order-preserving).
func cleanRegionList(regions []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range regions {
		r = strings.TrimSpace(r)
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// getBedrockRoute returns a fresh learned route for (account, model), if any.
func getBedrockRoute(accountID, modelID string) (bedrockRegionRoute, bool) {
	bedrockRegionMu.RLock()
	defer bedrockRegionMu.RUnlock()
	m, ok := bedrockRegionCache[accountID]
	if !ok {
		return bedrockRegionRoute{}, false
	}
	r, ok := m[modelID]
	if !ok || time.Now().After(r.expires) {
		return bedrockRegionRoute{}, false
	}
	return r, true
}

// recordBedrockRegion stores a learned verdict for (account, model). A callable
// verdict pins the winning region; a not-callable verdict (region "") is cached
// briefly so repeated requests fail over fast instead of re-hopping every region.
func recordBedrockRegion(accountID, modelID, region string, callable bool) {
	ttl := bedrockRegionTTL
	if !callable {
		ttl = bedrockRegionNegativeTTL
	}
	bedrockRegionMu.Lock()
	defer bedrockRegionMu.Unlock()
	if bedrockRegionCache[accountID] == nil {
		bedrockRegionCache[accountID] = map[string]bedrockRegionRoute{}
	}
	bedrockRegionCache[accountID][modelID] = bedrockRegionRoute{region: region, callable: callable, expires: time.Now().Add(ttl)}
}

// clearBedrockRegionRoutes drops all learned routes for an account (used when its
// region list or credentials change, alongside clearBedrockModelCache).
func clearBedrockRegionRoutes(accountID string) {
	bedrockRegionMu.Lock()
	delete(bedrockRegionCache, accountID)
	bedrockRegionMu.Unlock()
}

// orderedBedrockRegions returns the regions to try for (account, model), learned
// winner first. It returns (nil, true) when a fresh verdict says the model is not
// callable in ANY candidate region, so the caller can fail over immediately.
func orderedBedrockRegions(account *config.Account, modelID string) (regions []string, knownDead bool) {
	cands := candidateRegions(account)
	if r, ok := getBedrockRoute(account.ID, modelID); ok {
		if !r.callable {
			return nil, true
		}
		// Winner first, then the rest as fallback (in case access moved).
		out := []string{r.region}
		for _, c := range cands {
			if c != r.region {
				out = append(out, c)
			}
		}
		return out, false
	}
	return cands, false
}

// bedrockAccessError reports whether a non-2xx Bedrock response means "this model is
// not callable in THIS region" (an access/region verdict) as opposed to a genuine
// request error. These are the messages that should trigger a region hop; anything
// else (e.g. a real ValidationException) is a true error surfaced to the caller.
func bedrockAccessError(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusForbidden && status != http.StatusNotFound {
		return false
	}
	s := strings.ToLower(string(body))
	for _, needle := range []string{
		"operation not allowed",
		"model identifier is invalid",
		"throughput isn't supported",  // "...on-demand throughput isn't supported, use an inference profile"
		"throughput is not supported", // spelling variant (anchored on "throughput" so a
		//                                request-shape "X is not supported" 400 doesn't false-hop)
		"not authorized", // AccessDenied variants
		"access denied",
		"accessdenied",
		"don't have access",
		"do not have access",
		"has reached the end of its life", // model retired in this region
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// invokeBedrockRegional runs a Bedrock request across the account's candidate
// regions, hopping on access/region errors and caching the winning region. buildReq
// constructs the fully-formed (Accept-set) request for a given region; payload is
// the exact bytes to authorize over. It centralizes the throttle check, per-region
// auth, and the 429/200/access-error classification for both the native invoke and
// Converse paths.
//
// Contract preserved: on a 200 the LIVE response is returned (body not consumed) so
// the caller streams it; a 429 arms the throttle and returns errBedrockThrottled; a
// genuine non-2xx is returned as a response with its body intact so the caller
// surfaces it; and when no candidate region is callable the caller gets a non-2xx
// (or error) before any client bytes, so failover works.
func (h *Handler) invokeBedrockRegional(p forwardParams, modelID string, payload []byte, buildReq func(region string) (*http.Request, error)) (*http.Response, error) {
	// Adaptive throttle: skip a model still in a 429 cooldown (region-independent,
	// keyed on the resolved model id) so the caller fails over.
	if p.account != nil && bedrockThrottle.remaining(p.account.ID, modelID) > 0 {
		return nil, errBedrockThrottled
	}

	regions, knownDead := orderedBedrockRegions(p.account, modelID)
	if knownDead {
		return nil, fmt.Errorf("bedrock: model %s not callable in any configured region for account %s", modelID, p.account.ID)
	}

	var lastResp *http.Response
	var lastErr error
	hops := 0 // count of regions that returned a clean access/region "not callable here"
	for _, region := range regions {
		req, err := buildReq(region)
		if err != nil {
			return nil, err
		}
		if err := authorizeBedrockRequest(p.account, req, payload, region); err != nil {
			return nil, err
		}
		resp, err := bedrockHTTPClientFor(p.account).Do(req)
		if err != nil {
			// Transport failure: NOT a region verdict. Try the next region, keep the
			// error and any prior access-error fallback (don't clear lastResp).
			lastErr = err
			logger.Warnf("[Bedrock] %s %s request failed: %v", region, modelID, err)
			continue
		}
		switch resp.StatusCode {
		case http.StatusOK:
			recordBedrockRegion(p.account.ID, modelID, region, true)
			return resp, nil
		case http.StatusTooManyRequests:
			// Callable (just throttled): learn the region, arm the cooldown, fail over.
			recordBedrockRegion(p.account.ID, modelID, region, true)
			noteBedrockResponseThrottle(p.account.ID, modelID, resp)
			resp.Body.Close()
			return nil, errBedrockThrottled
		default:
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			if len(regions) > 1 && bedrockAccessError(resp.StatusCode, errBody) {
				// Not callable HERE — hop to the next region, keeping this as a fallback
				// response in case EVERY region says the same. The negative verdict is
				// recorded only after the whole sweep (below), never mid-loop: a genuine
				// error or transport blip in a later region must not pin the model as
				// dead when an untried region might be callable.
				hops++
				lastResp = resp
				continue
			}
			// Genuine error (or single-region account): surface it as-is. Do NOT
			// negative-cache — this is not a region verdict.
			return resp, nil
		}
	}
	// Every candidate region returned a clean access/region error → the model is
	// genuinely not callable anywhere for this account. Cache that (short TTL) so
	// subsequent requests fail over fast instead of re-sweeping every region.
	if hops == len(regions) {
		recordBedrockRegion(p.account.ID, modelID, "", false)
	}
	if lastResp != nil {
		return lastResp, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("bedrock: request failed in all candidate regions for account %s: %w", p.account.ID, lastErr)
	}
	return nil, fmt.Errorf("bedrock: request failed in all candidate regions for account %s", p.account.ID)
}

// --- prewarm canary ---------------------------------------------------------

// BedrockRegionModel is one row of the prewarm result: a model and the region it is
// callable in (empty region = not callable in any candidate region).
type BedrockRegionModel struct {
	ModelID string `json:"modelId"`
	Region  string `json:"region"`
}

// prewarmBedrockRegions canary-invokes every discovered model across the account's
// candidate regions and records the callable region per model into the route cache.
// It returns the resolved rows for display. Bounded concurrency; runs in the
// background off an explicit refresh, so its per-model token cost (max_tokens 1) is
// paid only when the operator asks.
func (h *Handler) prewarmBedrockRegions(account *config.Account) []BedrockRegionModel {
	models := h.cachedOrDiscoverBedrockModels(account)
	cands := candidateRegions(account)
	rows := make([]BedrockRegionModel, len(models))

	sem := make(chan struct{}, bedrockCanaryConcurrency)
	var wg sync.WaitGroup
	for i, modelID := range models {
		i, modelID := i, modelID
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			winner := ""
			anyReached := false
			for _, region := range cands {
				callable, reached := h.canaryBedrockRegion(account, modelID, region)
				if reached {
					anyReached = true
				}
				if callable {
					winner = region
					break
				}
			}
			// Only record a verdict if at least one canary actually reached AWS. If
			// every attempt was a transport error, leave the cache cold so lazy routing
			// decides from real traffic instead of pinning the model as dead.
			if anyReached {
				recordBedrockRegion(account.ID, modelID, winner, winner != "")
			}
			rows[i] = BedrockRegionModel{ModelID: modelID, Region: winner}
		}()
	}
	wg.Wait()
	callable := 0
	for _, r := range rows {
		if r.Region != "" {
			callable++
		}
	}
	logger.Infof("[Bedrock] prewarm for account %s: %d/%d models callable across %d region(s)", account.ID, callable, len(rows), len(cands))
	return rows
}

// canaryBedrockRegion sends one minimal invoke for modelID in region. It reports
// callable (access passed there) and reached (an HTTP exchange completed). A
// transport error returns (false, false) so the caller can distinguish "not
// callable here" from "couldn't tell". It uses the account's configured wire path
// (Converse vs native invoke) so the canary exercises the same authorization the
// real request would. 200/429 and any non-access error mean "access ok"; an
// access/region error means not callable here.
func (h *Handler) canaryBedrockRegion(account *config.Account, modelID, region string) (callable, reached bool) {
	var req *http.Request
	var payload []byte
	var err error
	if accountUsesConverse(account) {
		payload = []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}],"inferenceConfig":{"maxTokens":1}}`)
		req, err = newBedrockRequestForURL(bedrockConverseEndpoint(region, modelID, false), payload)
	} else {
		payload = []byte(`{"anthropic_version":"` + bedrockAnthropicVersion + `","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
		req, err = newBedrockRequest(region, modelID, false, payload)
	}
	if err != nil {
		return false, false
	}
	req.Header.Set("Accept", "application/json")
	if err := authorizeBedrockRequest(account, req, payload, region); err != nil {
		return false, false
	}
	resp, err := bedrockHTTPClient(account).Do(req)
	if err != nil {
		return false, false // transport error: couldn't determine
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTooManyRequests {
		return true, true
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// Callable if the failure is NOT an access/region verdict (e.g. a body
	// ValidationException means auth passed).
	return !bedrockAccessError(resp.StatusCode, body), true
}

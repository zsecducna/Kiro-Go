package proxy

// Bedrock model auto-discovery.
//
// A model is only listed if it is actually callable on-demand. Two signals drive
// this, and they are NOT the same:
//
//   - inferenceTypesSupported (from ListFoundationModels): trusted. A bare
//     foundation id is invocable on-demand only when this contains "ON_DEMAND".
//     INFERENCE_PROFILE-only and PROVISIONED-only ids 400 on a bare invoke, so
//     they are excluded here — the INFERENCE_PROFILE ones come back via their
//     us./eu. inference-profile id from ListInferenceProfiles instead.
//   - GetFoundationModelAvailability authorizationStatus: NOT trusted. This
//     gateway reports NOT_AUTHORIZED even for models that invoke fine, so probing
//     it would hide usable models. It is deliberately not called.
//
// Discovery therefore = ListFoundationModels (kept: ON_DEMAND text) UNION
// ListInferenceProfiles (the callable cross-region ids), deduped. Results are
// cached per account so the hot path (resolveBedrockModelID) and apiGetAccountModels
// read the cache, never the network. All calls reuse the same hand-rolled SigV4
// signer against the bedrock (control-plane) host.
//
// NOTE: ON_DEMAND is the static "callable as a bare id" gate. Per-model throughput
// limits (RPM/TPM) are a separate concern surfaced at invoke time as HTTP 429;
// exposing them would require the Service Quotas API and is a future follow-up.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// bedrockDiscoveryTTL bounds how long a discovered callable-model set is reused.
const bedrockDiscoveryTTL = 30 * time.Minute

// bedrockDiscoveryNegativeTTL is the (short) TTL for a failed/empty discovery so a
// transient control-plane error recovers quickly rather than being pinned.
const bedrockDiscoveryNegativeTTL = 2 * time.Minute

// bedrockControlPlaneTimeout bounds a single control-plane request so a hung
// endpoint cannot pin an HTTP handler for the invoke client's long timeout.
const bedrockControlPlaneTimeout = 20 * time.Second

// bedrockControlPlaneBase is the control-plane host (NOT bedrock-runtime).
func bedrockControlPlaneBase(region string) string {
	return fmt.Sprintf("https://bedrock.%s.amazonaws.com", region)
}

// --- response shapes (subset) ----------------------------------------------

type bedrockFoundationModelSummary struct {
	ModelID          string   `json:"modelId"`
	ProviderName     string   `json:"providerName"`
	OutputModalities []string `json:"outputModalities"`
	ModelLifecycle   struct {
		Status string `json:"status"`
	} `json:"modelLifecycle"`
	InferenceTypesSupported []string `json:"inferenceTypesSupported"`
}

type bedrockListModelsResponse struct {
	ModelSummaries []bedrockFoundationModelSummary `json:"modelSummaries"`
}

type bedrockInferenceProfilesResponse struct {
	InferenceProfileSummaries []struct {
		InferenceProfileID string `json:"inferenceProfileId"`
		Status             string `json:"status"`
	} `json:"inferenceProfileSummaries"`
}

// --- pure helpers (unit-tested) --------------------------------------------

// onDemandTextModelIDs returns the ids of ACTIVE, text-output foundation models
// that are callable on-demand via their BARE foundation id — i.e. those whose
// inferenceTypesSupported includes "ON_DEMAND".
//
// Two deliberate exclusions (both "listed but unusable" as a bare id):
//   - INFERENCE_PROFILE-only models: the bare foundation id returns HTTP 400
//     "on-demand throughput isn't supported, use an inference profile" on invoke.
//     The callable us./eu. profile id for these is added separately by the caller
//     from ListInferenceProfiles, so the model is still reachable — just not under
//     its bare id.
//   - PROVISIONED-only models: not invocable on-demand at all.
//
// This filter reads inferenceTypesSupported, which is static metadata from
// ListFoundationModels itself — NOT the GetFoundationModelAvailability probe, which
// this gateway reports unreliably (NOT_AUTHORIZED for models that invoke fine). A
// model with an empty/absent inferenceTypesSupported is kept (unknown, don't hide).
func onDemandTextModelIDs(resp bedrockListModelsResponse) []string {
	var ids []string
	for _, m := range resp.ModelSummaries {
		if m.ModelLifecycle.Status != "ACTIVE" {
			continue
		}
		isText := false
		for _, o := range m.OutputModalities {
			if o == "TEXT" {
				isText = true
				break
			}
		}
		if !isText {
			continue
		}
		// Keep only bare ids callable on-demand. Empty list = unknown, keep.
		if len(m.InferenceTypesSupported) > 0 {
			onDemand := false
			for _, t := range m.InferenceTypesSupported {
				if t == "ON_DEMAND" {
					onDemand = true
					break
				}
			}
			if !onDemand {
				continue
			}
		}
		ids = append(ids, m.ModelID)
	}
	return ids
}

// inferenceProfileIDs returns the invocable inference-profile ids (e.g.
// us.anthropic.claude-..., us.amazon.nova-...) from a ListInferenceProfiles
// response — these are the ids that work for on-demand cross-region invoke of
// models that don't support a bare foundation-id call.
func inferenceProfileIDs(resp bedrockInferenceProfilesResponse) []string {
	var ids []string
	for _, p := range resp.InferenceProfileSummaries {
		if p.Status != "" && p.Status != "ACTIVE" {
			continue
		}
		if p.InferenceProfileID != "" {
			ids = append(ids, p.InferenceProfileID)
		}
	}
	return ids
}

// --- per-account cache ------------------------------------------------------

type bedrockDiscoveryEntry struct {
	models  []string
	expires time.Time
}

var (
	bedrockDiscoveryMu    sync.RWMutex
	bedrockDiscoveryCache = map[string]bedrockDiscoveryEntry{}
)

// getCachedBedrockModels returns a DEFENSIVE COPY of the cached set so callers
// (which append fallback ids) can never mutate the shared backing array.
func getCachedBedrockModels(accountID string) ([]string, bool) {
	bedrockDiscoveryMu.RLock()
	defer bedrockDiscoveryMu.RUnlock()
	e, ok := bedrockDiscoveryCache[accountID]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return append([]string(nil), e.models...), true
}

func setCachedBedrockModels(accountID string, models []string) {
	setCachedBedrockModelsTTL(accountID, models, bedrockDiscoveryTTL)
}

// clearBedrockModelCache drops any cached discovery for an account so the next
// lookup re-discovers. Used by the explicit "refresh models" admin action.
func clearBedrockModelCache(accountID string) {
	bedrockDiscoveryMu.Lock()
	delete(bedrockDiscoveryCache, accountID)
	bedrockDiscoveryMu.Unlock()
}

// setCachedBedrockModelsTTL stores a COPY of models with an explicit TTL (a short
// TTL is used for negative results so a transient control-plane failure recovers
// quickly instead of being pinned for the full success TTL).
func setCachedBedrockModelsTTL(accountID string, models []string, ttl time.Duration) {
	bedrockDiscoveryMu.Lock()
	defer bedrockDiscoveryMu.Unlock()
	bedrockDiscoveryCache[accountID] = bedrockDiscoveryEntry{
		models:  append([]string(nil), models...),
		expires: time.Now().Add(ttl),
	}
}

// --- network discovery ------------------------------------------------------

// signedBedrockControlGet performs a SigV4-signed GET against a control-plane path
// (e.g. "/foundation-models") and returns the status and body.
func (h *Handler) signedBedrockControlGet(account *config.Account, path string) (int, []byte, error) {
	creds, err := bedrockCredsFor(account)
	if err != nil {
		return 0, nil, err
	}
	region := bedrockRegionFor(account)
	req, err := newBedrockRequestForURL(bedrockControlPlaneBase(region)+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Method = http.MethodGet
	req.Body = nil
	req.ContentLength = 0
	req.Header.Set("Accept", "application/json")
	// Bound the call so a hung control plane cannot pin the caller for the invoke
	// client's long timeout.
	ctx, cancel := context.WithTimeout(context.Background(), bedrockControlPlaneTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	// GET has no body: sign over the empty payload.
	signSigV4(req, []byte{}, creds, region, bedrockService, time.Now())
	resp, err := bedrockHTTPClient(account).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// Control-plane responses are small; cap defensively.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// discoverBedrockModels returns the callable catalog for an account: every ACTIVE
// text foundation model that is on-demand-invocable as a bare id (all providers)
// plus every inference-profile id, deduped. Profile-only and provisioned-only bare
// ids are excluded (they 400 on a bare invoke); profile-only models remain
// reachable via their inference-profile id. It does NOT probe per-model
// availability — on this gateway that API is unreliable and would hide usable
// models. Two control-plane calls (ListFoundationModels + ListInferenceProfiles);
// the profiles call is best-effort. The result is cached; a list-models error is
// returned so the caller falls back to static config.
func (h *Handler) discoverBedrockModels(account *config.Account) ([]string, error) {
	code, body, err := h.signedBedrockControlGet(account, "/foundation-models")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("bedrock discovery: list models HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	var list bedrockListModelsResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("bedrock discovery: parse list: %w", err)
	}

	seen := map[string]bool{}
	all := make([]string, 0, len(list.ModelSummaries))
	add := func(ids []string) {
		for _, id := range ids {
			if id != "" && !seen[id] {
				seen[id] = true
				all = append(all, id)
			}
		}
	}
	foundation := onDemandTextModelIDs(list)
	add(foundation)

	// Inference profiles are the invocable ids for on-demand cross-region models
	// (Claude 4.x, etc.). Best-effort: a failure here doesn't sink discovery.
	profiles := 0
	if pc, pb, pe := h.signedBedrockControlGet(account, "/inference-profiles"); pe == nil && pc == http.StatusOK {
		var pr bedrockInferenceProfilesResponse
		if json.Unmarshal(pb, &pr) == nil {
			ids := inferenceProfileIDs(pr)
			profiles = len(ids)
			add(ids)
		}
	}

	sort.Strings(all)
	setCachedBedrockModelsTTL(account.ID, all, bedrockDiscoveryTTL)
	logger.Infof("[Bedrock] discovered %d models for account %s (%d on-demand text foundation + %d inference profiles)",
		len(all), account.ID, len(foundation), profiles)
	return all, nil
}

// cachedOrDiscoverBedrockModels returns the cached callable set, refreshing it via
// discovery when the cache is cold or stale. Errors are logged and result in an
// empty set (callers fall back to static config), so model lookup never blocks on
// a control-plane hiccup.
func (h *Handler) cachedOrDiscoverBedrockModels(account *config.Account) []string {
	if m, ok := getCachedBedrockModels(account.ID); ok {
		return m
	}
	// Singleflight per account: serialize concurrent cold-cache callers so only one
	// runs the list+probe fan-out; the rest read the freshly populated cache.
	lk := bedrockDiscoveryLock(account.ID)
	lk.Lock()
	defer lk.Unlock()
	if m, ok := getCachedBedrockModels(account.ID); ok {
		return m
	}
	m, err := h.discoverBedrockModels(account)
	if err != nil {
		// Negative-cache the failure briefly so a key lacking
		// bedrock:ListFoundationModels doesn't pay a round-trip on every call.
		logger.Warnf("[Bedrock] model discovery failed for account %s: %v", account.ID, err)
		setCachedBedrockModelsTTL(account.ID, nil, bedrockDiscoveryNegativeTTL)
		return nil
	}
	return m
}

// bedrockDiscoveryInflight holds a per-account mutex used to collapse concurrent
// cold-cache discovery into a single control-plane fan-out.
var bedrockDiscoveryInflight sync.Map // accountID -> *sync.Mutex

func bedrockDiscoveryLock(accountID string) *sync.Mutex {
	v, _ := bedrockDiscoveryInflight.LoadOrStore(accountID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// --- resolver integration ---------------------------------------------------

// discoveredBedrockModelFor looks up a client-requested model against an account's
// cached callable set: an exact id match, or an alias match where the requested
// token is a suffix of exactly one callable id (e.g. "claude-haiku-4-5" ->
// "anthropic.claude-haiku-4-5-20251001-v1:0"). Returns "" when the cache is empty
// or the match is ambiguous. Reads the cache only — never the network.
func discoveredBedrockModelFor(accountID, requested string) string {
	models, ok := getCachedBedrockModels(accountID)
	if !ok {
		return ""
	}
	for _, m := range models {
		if m == requested {
			return m
		}
	}
	// Alias match: a unique callable id whose model-name CORE (vendor/region
	// prefix and version/date suffix stripped) exactly equals the requested alias.
	// Core equality — not substring — so "claude-sonnet-4" cannot match
	// "claude-sonnet-4-5-...".
	var matches []string
	for _, m := range models {
		if bedrockModelCore(m) == requested {
			matches = append(matches, m)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// bedrockModelCore reduces a Bedrock model id to its model-name core by stripping
// the leading region/vendor prefix (everything through the last ".") and the
// trailing version/date suffix. Examples:
//
//	anthropic.claude-haiku-4-5-20251001-v1:0 -> claude-haiku-4-5
//	us.amazon.nova-lite-v1:0                 -> nova-lite
//	meta.llama3-8b-instruct-v1:0             -> llama3-8b-instruct
func bedrockModelCore(id string) string {
	core := id
	if k := strings.LastIndex(core, "."); k >= 0 {
		core = core[k+1:]
	}
	// Cut at a "-YYYYMMDD" date segment if present, else at a "-vN" version segment.
	if i := indexDateSegment(core); i >= 0 {
		return core[:i]
	}
	if i := indexVersionSegment(core); i >= 0 {
		return core[:i]
	}
	return core
}

// indexDateSegment returns the index of a "-" that begins an 8-digit date segment
// (e.g. "-20251001"), or -1.
func indexDateSegment(s string) int {
	for i := 0; i+9 <= len(s); i++ {
		if s[i] != '-' {
			continue
		}
		allDigits := true
		for j := i + 1; j < i+9; j++ {
			if s[j] < '0' || s[j] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return i
		}
	}
	return -1
}

// indexVersionSegment returns the index of a "-" that begins a "vN" version
// segment (e.g. "-v1", "-v2:0"), or -1.
func indexVersionSegment(s string) int {
	for i := 0; i+3 <= len(s); i++ {
		if s[i] == '-' && s[i+1] == 'v' && s[i+2] >= '0' && s[i+2] <= '9' {
			return i
		}
	}
	return -1
}

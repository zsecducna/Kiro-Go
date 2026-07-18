package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"kiro-go/config"
)

func TestOnDemandTextModelIDs(t *testing.T) {
	// Shapes taken from a real ListFoundationModels response.
	raw := []byte(`{"modelSummaries":[
		{"modelId":"anthropic.claude-haiku-4-5-20251001-v1:0","providerName":"Anthropic","outputModalities":["TEXT"],"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}},
		{"modelId":"anthropic.claude-3-sonnet-20240229-v1:0","providerName":"Anthropic","outputModalities":["TEXT"],"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"LEGACY"}},
		{"modelId":"amazon.titan-image-generator-v1","providerName":"Amazon","outputModalities":["IMAGE"],"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}},
		{"modelId":"anthropic.claude-sonnet-4-5-20250929-v1:0","providerName":"Anthropic","outputModalities":["TEXT"],"inferenceTypesSupported":["INFERENCE_PROFILE"],"modelLifecycle":{"status":"ACTIVE"}},
		{"modelId":"amazon.provisioned-only-v1:0","providerName":"Amazon","outputModalities":["TEXT"],"inferenceTypesSupported":["PROVISIONED"],"modelLifecycle":{"status":"ACTIVE"}},
		{"modelId":"amazon.unknown-inference-v1:0","providerName":"Amazon","outputModalities":["TEXT"],"modelLifecycle":{"status":"ACTIVE"}},
		{"modelId":"meta.llama3-8b-instruct-v1:0","providerName":"Meta","outputModalities":["TEXT"],"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}}
	]}`)
	var r bedrockListModelsResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	got := onDemandTextModelIDs(r)
	// Only bare ids callable on-demand survive: the two ON_DEMAND text models plus
	// the one with an absent inferenceTypesSupported (unknown -> kept). Excluded:
	// LEGACY, IMAGE, INFERENCE_PROFILE-only (covered by its us. profile id instead),
	// and PROVISIONED-only.
	want := map[string]bool{
		"anthropic.claude-haiku-4-5-20251001-v1:0": true,
		"meta.llama3-8b-instruct-v1:0":             true,
		"amazon.unknown-inference-v1:0":            true,
	}
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 on-demand-callable text models", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected id %q (legacy/non-text/profile-only/provisioned-only should be excluded)", id)
		}
	}
}

func TestAvailabilityIsCallable(t *testing.T) {
	mk := func(agree, ent, region string) bedrockAvailability {
		var a bedrockAvailability
		a.AgreementAvailability.Status = agree
		a.EntitlementAvailability = ent
		a.RegionAvailability = region
		return a
	}
	// All three AVAILABLE -> callable (Nova/Llama shape).
	if !availabilityIsCallable(mk("AVAILABLE", "AVAILABLE", "AVAILABLE")) {
		t.Error("all-AVAILABLE should be callable")
	}
	// Agreement NOT_AVAILABLE -> not callable even though entitlement/region are
	// AVAILABLE and authorizationStatus is ignored (Claude/Cohere shape).
	if availabilityIsCallable(mk("NOT_AVAILABLE", "AVAILABLE", "AVAILABLE")) {
		t.Error("agreement NOT_AVAILABLE must not be callable")
	}
	if availabilityIsCallable(mk("AVAILABLE", "NOT_AVAILABLE", "AVAILABLE")) {
		t.Error("entitlement NOT_AVAILABLE must not be callable")
	}
	if availabilityIsCallable(mk("AVAILABLE", "AVAILABLE", "NOT_AVAILABLE")) {
		t.Error("region NOT_AVAILABLE must not be callable")
	}
}

func TestAvailabilityVerdict(t *testing.T) {
	// Clean all-AVAILABLE 200 -> determinate callable (Nova shape, verified live).
	if r := availabilityVerdict([]byte(`{"agreementAvailability":{"status":"AVAILABLE"},"authorizationStatus":"NOT_AUTHORIZED","entitlementAvailability":"AVAILABLE","regionAvailability":"AVAILABLE"}`)); !r.determinate || !r.callable {
		t.Errorf("all-AVAILABLE = %+v, want determinate+callable", r)
	}
	// Clean agreement-NOT_AVAILABLE 200 -> determinate not-callable (Claude shape).
	if r := availabilityVerdict([]byte(`{"agreementAvailability":{"status":"NOT_AVAILABLE"},"authorizationStatus":"NOT_AUTHORIZED","entitlementAvailability":"AVAILABLE","regionAvailability":"AVAILABLE"}`)); !r.determinate || r.callable {
		t.Errorf("agreement-NOT_AVAILABLE = %+v, want determinate+not-callable", r)
	}
	// Shape change (missing regionAvailability) -> INDETERMINATE, so the model is
	// kept, not silently dropped. This is the catalog-emptying guard.
	if r := availabilityVerdict([]byte(`{"agreementAvailability":{"status":"AVAILABLE"},"entitlementAvailability":"AVAILABLE"}`)); r.determinate {
		t.Errorf("missing-field 200 = %+v, want indeterminate", r)
	}
	// Garbage body -> indeterminate.
	if r := availabilityVerdict([]byte(`not json`)); r.determinate {
		t.Errorf("garbage = %+v, want indeterminate", r)
	}
}

func TestBedrockProfileBaseModel(t *testing.T) {
	cases := map[string]string{
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0":     "anthropic.claude-sonnet-4-5-20250929-v1:0",
		"eu.meta.llama3-70b-instruct-v1:0":                 "meta.llama3-70b-instruct-v1:0",
		"apac.amazon.nova-lite-v1:0":                       "amazon.nova-lite-v1:0",
		"us-gov.anthropic.claude-3-haiku-v1:0":             "anthropic.claude-3-haiku-v1:0",
		"global.anthropic.claude-sonnet-4-5-20250929-v1:0": "anthropic.claude-sonnet-4-5-20250929-v1:0",
		// A bare foundation id (vendor token, not a geo prefix) is unchanged.
		"amazon.nova-lite-v1:0": "amazon.nova-lite-v1:0",
		"anthropic.claude-x":    "anthropic.claude-x",
	}
	for in, want := range cases {
		if got := bedrockProfileBaseModel(in); got != want {
			t.Errorf("base(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterCallableModels(t *testing.T) {
	foundation := []string{
		"amazon.nova-lite-v1:0",                  // AVAILABLE -> keep
		"anthropic.claude-3-haiku-20240307-v1:0", // NOT_AVAILABLE -> drop
		"amazon.titan-text-express-v1",           // indeterminate (400) -> keep
	}
	profiles := []string{
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0", // base NOT_AVAILABLE -> drop
		"us.meta.llama3-70b-instruct-v1:0",             // base AVAILABLE -> keep
		"us.mistral.unprobed-v1:0",                     // base absent from map -> keep
	}
	avail := map[string]availResult{
		"amazon.nova-lite-v1:0":                     {callable: true, determinate: true},
		"anthropic.claude-3-haiku-20240307-v1:0":    {callable: false, determinate: true},
		"amazon.titan-text-express-v1":              {callable: false, determinate: false},
		"anthropic.claude-sonnet-4-5-20250929-v1:0": {callable: false, determinate: true},
		"meta.llama3-70b-instruct-v1:0":             {callable: true, determinate: true},
	}
	got := filterCallableModels(foundation, profiles, avail)
	want := []string{
		"amazon.nova-lite-v1:0",
		"amazon.titan-text-express-v1",
		"us.meta.llama3-70b-instruct-v1:0",
		"us.mistral.unprobed-v1:0",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestInferenceProfileIDs(t *testing.T) {
	raw := []byte(`{"inferenceProfileSummaries":[
		{"inferenceProfileId":"us.anthropic.claude-sonnet-4-5-20250929-v1:0","status":"ACTIVE"},
		{"inferenceProfileId":"us.amazon.nova-lite-v1:0"},
		{"inferenceProfileId":"eu.retired-model","status":"INACTIVE"}
	]}`)
	var r bedrockInferenceProfilesResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	got := inferenceProfileIDs(r)
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 (INACTIVE excluded, missing-status included)", got)
	}
	if got[0] != "us.anthropic.claude-sonnet-4-5-20250929-v1:0" || got[1] != "us.amazon.nova-lite-v1:0" {
		t.Errorf("profile ids = %v", got)
	}
}

func TestDiscoveredBedrockModelFor(t *testing.T) {
	acct := "acct-disc-1"
	setCachedBedrockModels(acct, []string{
		"anthropic.claude-haiku-4-5-20251001-v1:0",
		"amazon.nova-lite-v1:0",
		"meta.llama3-8b-instruct-v1:0",
	})
	defer func() {
		bedrockDiscoveryMu.Lock()
		delete(bedrockDiscoveryCache, acct)
		bedrockDiscoveryMu.Unlock()
	}()

	// Exact match.
	if got := discoveredBedrockModelFor(acct, "amazon.nova-lite-v1:0"); got != "amazon.nova-lite-v1:0" {
		t.Errorf("exact match = %q", got)
	}
	// Unique alias (substring) match.
	if got := discoveredBedrockModelFor(acct, "claude-haiku-4-5"); got != "anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("alias match = %q, want the haiku id", got)
	}
	// No match.
	if got := discoveredBedrockModelFor(acct, "gpt-4o"); got != "" {
		t.Errorf("no-match should be empty, got %q", got)
	}
	// Unknown account (cold cache).
	if got := discoveredBedrockModelFor("nope", "amazon.nova-lite-v1:0"); got != "" {
		t.Errorf("cold cache should be empty, got %q", got)
	}
}

func TestBedrockModelCore(t *testing.T) {
	cases := map[string]string{
		"anthropic.claude-haiku-4-5-20251001-v1:0":  "claude-haiku-4-5",
		"us.amazon.nova-lite-v1:0":                  "nova-lite",
		"meta.llama3-8b-instruct-v1:0":              "llama3-8b-instruct",
		"anthropic.claude-sonnet-4-5-20250929-v1:0": "claude-sonnet-4-5",
		"deepseek.r1-v1:0":                          "r1",
	}
	for in, want := range cases {
		if got := bedrockModelCore(in); got != want {
			t.Errorf("core(%q) = %q, want %q", in, got, want)
		}
	}
}

// Regression: a shorter alias must NOT resolve to a longer versioned sibling.
func TestDiscoveredBedrockModelForNoPrefixShadow(t *testing.T) {
	acct := "acct-shadow"
	setCachedBedrockModels(acct, []string{"anthropic.claude-sonnet-4-5-20250929-v1:0"})
	defer func() {
		bedrockDiscoveryMu.Lock()
		delete(bedrockDiscoveryCache, acct)
		bedrockDiscoveryMu.Unlock()
	}()
	// "claude-sonnet-4" is a substring of the 4-5 id but a different model core.
	if got := discoveredBedrockModelFor(acct, "claude-sonnet-4"); got != "" {
		t.Errorf("claude-sonnet-4 must NOT match claude-sonnet-4-5 id, got %q", got)
	}
	// The exact core alias still resolves.
	if got := discoveredBedrockModelFor(acct, "claude-sonnet-4-5"); got != "anthropic.claude-sonnet-4-5-20250929-v1:0" {
		t.Errorf("claude-sonnet-4-5 should resolve, got %q", got)
	}
}

func TestDiscoveredBedrockModelForAmbiguous(t *testing.T) {
	acct := "acct-disc-2"
	setCachedBedrockModels(acct, []string{
		"amazon.nova-lite-v1:0",
		"amazon.nova-lite-v1:0:300k",
	})
	defer func() {
		bedrockDiscoveryMu.Lock()
		delete(bedrockDiscoveryCache, acct)
		bedrockDiscoveryMu.Unlock()
	}()
	// "nova-lite" is a substring of both -> ambiguous -> no match (avoid guessing).
	if got := discoveredBedrockModelFor(acct, "nova-lite"); got != "" {
		t.Errorf("ambiguous alias should return empty, got %q", got)
	}
	// Exact still wins.
	if got := discoveredBedrockModelFor(acct, "amazon.nova-lite-v1:0"); got != "amazon.nova-lite-v1:0" {
		t.Errorf("exact match should win over ambiguity, got %q", got)
	}
}

func TestResolveBedrockModelIDUsesDiscovered(t *testing.T) {
	acct := &config.Account{ID: "acct-resolve-1"}
	setCachedBedrockModels(acct.ID, []string{"anthropic.claude-haiku-4-5-20251001-v1:0"})
	defer func() {
		bedrockDiscoveryMu.Lock()
		delete(bedrockDiscoveryCache, acct.ID)
		bedrockDiscoveryMu.Unlock()
	}()
	// An alias not in the static default map resolves via discovery.
	got, err := resolveBedrockModelID(acct, "claude-haiku-4-5")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Errorf("resolved = %q, want discovered id", got)
	}
}

func TestBedrockDiscoveryCacheExpiry(t *testing.T) {
	acct := "acct-ttl"
	bedrockDiscoveryMu.Lock()
	bedrockDiscoveryCache[acct] = bedrockDiscoveryEntry{models: []string{"x"}, expires: time.Now().Add(-time.Minute)}
	bedrockDiscoveryMu.Unlock()
	defer func() {
		bedrockDiscoveryMu.Lock()
		delete(bedrockDiscoveryCache, acct)
		bedrockDiscoveryMu.Unlock()
	}()
	if _, ok := getCachedBedrockModels(acct); ok {
		t.Error("expired cache entry should not be returned")
	}
}

package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"kiro-go/config"
)

func TestActiveTextModelIDs(t *testing.T) {
	// Shapes taken from a real ListFoundationModels response.
	raw := []byte(`{"modelSummaries":[
		{"modelId":"anthropic.claude-haiku-4-5-20251001-v1:0","providerName":"Anthropic","outputModalities":["TEXT"],"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}},
		{"modelId":"anthropic.claude-3-sonnet-20240229-v1:0","providerName":"Anthropic","outputModalities":["TEXT"],"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"LEGACY"}},
		{"modelId":"amazon.titan-image-generator-v1","providerName":"Amazon","outputModalities":["IMAGE"],"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}},
		{"modelId":"anthropic.claude-sonnet-4-5-20250929-v1:0","providerName":"Anthropic","outputModalities":["TEXT"],"inferenceTypesSupported":["INFERENCE_PROFILE"],"modelLifecycle":{"status":"ACTIVE"}},
		{"modelId":"meta.llama3-8b-instruct-v1:0","providerName":"Meta","outputModalities":["TEXT"],"inferenceTypesSupported":["ON_DEMAND"],"modelLifecycle":{"status":"ACTIVE"}}
	]}`)
	var r bedrockListModelsResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	got := activeTextModelIDs(r)
	want := map[string]bool{
		"anthropic.claude-haiku-4-5-20251001-v1:0": true,
		"meta.llama3-8b-instruct-v1:0":             true,
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want the 2 ACTIVE text models", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected id %q (legacy/non-text should be excluded)", id)
		}
	}
}

func TestAvailabilityIsCallable(t *testing.T) {
	callable := bedrockAvailabilityResponse{AuthorizationStatus: "AUTHORIZED", RegionAvailability: "AVAILABLE"}
	callable.AgreementAvailability.Status = "AVAILABLE"
	if !availabilityIsCallable(callable) {
		t.Error("AUTHORIZED + AVAILABLE + AVAILABLE should be callable")
	}
	// Real response for a listed-but-uncallable model.
	notAuth := bedrockAvailabilityResponse{AuthorizationStatus: "NOT_AUTHORIZED", RegionAvailability: "AVAILABLE"}
	notAuth.AgreementAvailability.Status = "AVAILABLE"
	if availabilityIsCallable(notAuth) {
		t.Error("NOT_AUTHORIZED must not be callable even when region/entitlement available")
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

package proxy

import (
	"kiro-go/config"
	"testing"
)

// reprobeApiKeyRegion must discover the region a ksk_ key actually serves when the
// stored region is wrong, persist it to config, and mutate the in-memory account so
// the caller's immediate retry targets the new region.
func TestReprobeApiKeyRegionRedetectsAndPersists(t *testing.T) {
	mustInitConfig(t)

	acc := config.Account{
		ID:          "acct-1",
		AuthMethod:  "api_key",
		KiroApiKey:  "ksk_key",
		AccessToken: "ksk_key",
		Region:      "us-east-1", // wrong region — upstream rejects the bearer token here
		Enabled:     true,
	}
	if err := config.AddAccount(acc); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	// The key is only valid in eu-central-1.
	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		if region != "eu-central-1" {
			return nil, errTest("HTTP 403: not served in " + region)
		}
		return &config.AccountInfo{Email: "a@example.com", UserId: "u-1"}, nil
	}

	account := acc // local copy the caller would hold
	region, ok := reprobeApiKeyRegion(&account)
	if !ok {
		t.Fatalf("expected region re-detection to succeed")
	}
	if region != "eu-central-1" {
		t.Fatalf("expected eu-central-1, got %q", region)
	}
	// In-memory account updated so the immediate retry targets the new region.
	if account.Region != "eu-central-1" {
		t.Fatalf("in-memory account region not updated, got %q", account.Region)
	}
	// Persisted to config so future calls skip the reprobe entirely.
	var persisted string
	for _, a := range config.GetAccounts() {
		if a.ID == "acct-1" {
			persisted = a.Region
		}
	}
	if persisted != "eu-central-1" {
		t.Fatalf("persisted region not updated, got %q", persisted)
	}
}

// A genuinely dead key (rejected in every region) must return ok=false and leave the
// stored region untouched, so the caller surfaces the original auth error.
func TestReprobeApiKeyRegionDeadKey(t *testing.T) {
	mustInitConfig(t)

	origProbe := probeKiroApiKey
	defer func() { probeKiroApiKey = origProbe }()
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		return nil, errTest("HTTP 403: The bearer token included in the request is invalid.")
	}

	account := config.Account{
		ID:         "acct-2",
		AuthMethod: "api_key",
		KiroApiKey: "ksk_dead",
		Region:     "us-east-1",
	}
	if region, ok := reprobeApiKeyRegion(&account); ok {
		t.Fatalf("expected no region for a dead key, got %q", region)
	}
	if account.Region != "us-east-1" {
		t.Fatalf("region must be unchanged for a dead key, got %q", account.Region)
	}
}

// Non-api_key accounts must never trigger the api_key reprobe path.
func TestReprobeApiKeyRegionSkipsNonApiKey(t *testing.T) {
	account := &config.Account{ID: "acct-3", AuthMethod: "social", Region: "us-east-1"}
	if _, ok := reprobeApiKeyRegion(account); ok {
		t.Fatalf("expected no reprobe for non-api_key account")
	}
}

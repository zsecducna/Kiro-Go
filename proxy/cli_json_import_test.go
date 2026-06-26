package proxy

import (
	"os"
	"testing"
)

// TestNormalizeRawCredentialSnakeCaseExternalIdp verifies the helper's native
// snake_case external_idp JSON (CLIProxyAPI_*.json) maps onto the canonical
// request with every external-IdP field carried through.
func TestNormalizeRawCredentialSnakeCaseExternalIdp(t *testing.T) {
	body := []byte(`{
		"access_token": "at-1",
		"auth_method": "external_idp",
		"client_id": "azure-client",
		"refresh_token": "rt-1",
		"region": "us-east-1",
		"profile_arn": "arn:aws:codewhisperer:eu-central-1:123:profile/X",
		"issuer_url": "https://login.microsoftonline.com/tid/v2.0",
		"token_endpoint": "https://login.microsoftonline.com/tid/oauth2/v2.0/token",
		"scopes": "api://azure-client/codewhisperer:conversations offline_access",
		"type": "kiro"
	}`)
	req, err := decodeImportRequest(body)
	if err != nil {
		t.Fatalf("decodeImportRequest: %v", err)
	}
	if req.AuthMethod != "external_idp" {
		t.Fatalf("AuthMethod = %q, want external_idp", req.AuthMethod)
	}
	if req.ClientID != "azure-client" {
		t.Fatalf("ClientID = %q", req.ClientID)
	}
	if req.TokenEndpoint == "" || req.IssuerURL == "" || req.Scopes == "" {
		t.Fatalf("external_idp fields dropped: tokenEndpoint=%q issuerUrl=%q scopes=%q",
			req.TokenEndpoint, req.IssuerURL, req.Scopes)
	}
	if req.ProfileArn != "arn:aws:codewhisperer:eu-central-1:123:profile/X" {
		t.Fatalf("ProfileArn = %q", req.ProfileArn)
	}
	if req.Provider != "AzureAD" {
		t.Fatalf("Provider default = %q, want AzureAD", req.Provider)
	}
	if req.AccessToken != "at-1" || req.RefreshToken != "rt-1" {
		t.Fatalf("tokens not mapped: access=%q refresh=%q", req.AccessToken, req.RefreshToken)
	}
}

// TestNormalizeRawCredentialCamelCaseStillWorks guards the existing API/UI
// camelCase shape so the upgrade is backward compatible.
func TestNormalizeRawCredentialCamelCaseStillWorks(t *testing.T) {
	body := []byte(`{
		"refreshToken": "rt-2",
		"clientId": "c",
		"clientSecret": "s",
		"authMethod": "idc",
		"region": "us-east-1"
	}`)
	req, err := decodeImportRequest(body)
	if err != nil {
		t.Fatalf("decodeImportRequest: %v", err)
	}
	if req.AuthMethod != "idc" {
		t.Fatalf("AuthMethod = %q, want idc", req.AuthMethod)
	}
	if req.ClientID != "c" || req.ClientSecret != "s" {
		t.Fatalf("client material not mapped: id=%q secret=%q", req.ClientID, req.ClientSecret)
	}
	if req.Provider != "BuilderId" {
		t.Fatalf("Provider default = %q, want BuilderId", req.Provider)
	}
}

// TestNormalizeAuthMethodInference exercises the inference fallback for empty or
// unknown auth methods.
func TestNormalizeAuthMethodInference(t *testing.T) {
	cases := []struct {
		name                                 string
		raw, tokenEndpoint, clientID, secret string
		want                                 string
	}{
		{"external via tokenEndpoint+clientId", "", "https://x/token", "cid", "", "external_idp"},
		{"idc via clientId+secret", "", "", "cid", "sec", "idc"},
		{"social bare", "", "", "", "", "social"},
		{"explicit azure alias", "azuread", "", "", "", "external_idp"},
		{"explicit m365 alias", "m365", "", "", "", "external_idp"},
		{"explicit github", "github", "", "", "", "social"},
		{"explicit builderid", "builderid", "", "", "", "idc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAuthMethod(tc.raw, tc.tokenEndpoint, tc.clientID, tc.secret)
			if got != tc.want {
				t.Fatalf("normalizeAuthMethod(%q,%q,%q,%q) = %q, want %q",
					tc.raw, tc.tokenEndpoint, tc.clientID, tc.secret, got, tc.want)
			}
		})
	}
}

// TestNormalizeCliJsonArray verifies a JSON array of helper objects parses into
// one request per element.
func TestNormalizeCliJsonArray(t *testing.T) {
	raw := []byte(`[
		{"refresh_token":"r1","client_id":"c1","token_endpoint":"https://login.microsoftonline.com/t/token","auth_method":"external_idp"},
		{"refreshToken":"r2","clientId":"c2","clientSecret":"s2","authMethod":"idc"}
	]`)
	reqs, _, err := normalizeCliJson(raw)
	if err != nil {
		t.Fatalf("normalizeCliJson: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}
	if reqs[0].AuthMethod != "external_idp" || reqs[1].AuthMethod != "idc" {
		t.Fatalf("auth methods = %q,%q", reqs[0].AuthMethod, reqs[1].AuthMethod)
	}
}

// TestNormalizeCliJsonConcatenated verifies several JSON objects in one blob
// (blank-line separated) all parse.
func TestNormalizeCliJsonConcatenated(t *testing.T) {
	raw := []byte(`{"refresh_token":"r1","client_id":"c1","token_endpoint":"https://login.microsoftonline.com/t/token"}

{"refresh_token":"r2","client_id":"c2","token_endpoint":"https://login.microsoftonline.com/t/token"}`)
	reqs, _, err := normalizeCliJson(raw)
	if err != nil {
		t.Fatalf("normalizeCliJson: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}
}

// TestNormalizeCliJsonFixtureFile parses the committed sample fixture to ensure
// the real helper output shape imports as an external_idp account.
func TestNormalizeCliJsonFixtureFile(t *testing.T) {
	raw, err := os.ReadFile("../testdata/CLIProxyAPI_sample_external_idp.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	reqs, _, err := normalizeCliJson(raw)
	if err != nil {
		t.Fatalf("normalizeCliJson(fixture): %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.AuthMethod != "external_idp" {
		t.Fatalf("fixture AuthMethod = %q, want external_idp", req.AuthMethod)
	}
	if req.TokenEndpoint == "" || req.IssuerURL == "" || req.ClientID == "" {
		t.Fatalf("fixture external_idp fields incomplete: %+v", req)
	}
}

// TestNormalizeCliJsonEmpty rejects an empty document with a clear error.
func TestNormalizeCliJsonEmpty(t *testing.T) {
	if _, _, err := normalizeCliJson([]byte("   ")); err == nil {
		t.Fatalf("expected error for empty document")
	}
}

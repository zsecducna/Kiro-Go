package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSigV4KnownAnswer cross-checks signSigV4 against a reference Authorization
// header produced by botocore (AWS's own SigV4 implementation) for an identical
// Bedrock invoke-with-response-stream request. The timestamp, credentials, and
// exact body bytes below are frozen from that botocore run; if the signing math
// drifts, this signature will no longer match.
//
// Reference generation (botocore) signed these exact body bytes with header
// Content-Type: application/json, yielding SignedHeaders=content-type;host;x-amz-date.
func TestSigV4KnownAnswer(t *testing.T) {
	const (
		amzDate  = "20260718T093804Z"
		body     = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		wantAuth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260718/us-east-1/bedrock/aws4_request, " +
			"SignedHeaders=content-type;host;x-amz-date, " +
			"Signature=19351a0cb2932d46650c3f574f9a98e3f5c286c7d53c5022d28890b975c1cebf"
	)

	ts, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		t.Fatalf("parse amzDate: %v", err)
	}

	url := "https://bedrock-runtime.us-east-1.amazonaws.com/model/" +
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0/invoke-with-response-stream"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	creds := awsCredentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	signSigV4(req, []byte(body), creds, "us-east-1", "bedrock", ts)

	got := req.Header.Get("Authorization")
	if got != wantAuth {
		t.Errorf("Authorization mismatch:\n got: %s\nwant: %s", got, wantAuth)
	}
	if req.Header.Get("X-Amz-Date") != amzDate {
		t.Errorf("X-Amz-Date = %q, want %q", req.Header.Get("X-Amz-Date"), amzDate)
	}
}

func TestAWSURIEncodePath(t *testing.T) {
	cases := map[string]string{
		"/model/us.anthropic.claude-sonnet-4-5-20250929-v1:0/invoke": "/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke",
		"/simple/path": "/simple/path",
		"/a b/c":       "/a%20b/c",
		"/tilde~-_.ok": "/tilde~-_.ok",
	}
	for in, want := range cases {
		if got := awsURIEncodePath(in); got != want {
			t.Errorf("awsURIEncodePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveBedrockModelID(t *testing.T) {
	// Pass-through when already a Bedrock id.
	if id, err := resolveBedrockModelID(nil, "us.anthropic.claude-opus-4-1-20250805-v1:0"); err != nil || !strings.Contains(id, "opus-4-1") {
		t.Errorf("passthrough failed: id=%q err=%v", id, err)
	}
	// Default alias map.
	if id, err := resolveBedrockModelID(nil, "claude-sonnet-4-5"); err != nil || id != "us.anthropic.claude-sonnet-4-5-20250929-v1:0" {
		t.Errorf("alias map failed: id=%q err=%v", id, err)
	}
	// Unknown → error.
	if _, err := resolveBedrockModelID(nil, "totally-unknown-model"); err == nil {
		t.Error("expected error for unknown model, got nil")
	}
}

func TestBuildBedrockBody(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := buildBedrockBody(in)
	if err != nil {
		t.Fatalf("buildBedrockBody: %v", err)
	}
	s := string(out)
	if strings.Contains(s, `"model"`) || strings.Contains(s, `"stream"`) {
		t.Errorf("model/stream not stripped: %s", s)
	}
	if !strings.Contains(s, `"anthropic_version":"bedrock-2023-05-31"`) {
		t.Errorf("anthropic_version not pinned: %s", s)
	}
	if !strings.Contains(s, `"max_tokens":32`) {
		t.Errorf("max_tokens lost: %s", s)
	}
}

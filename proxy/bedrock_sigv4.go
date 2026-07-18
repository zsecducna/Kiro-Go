package proxy

// AWS Signature Version 4 signing for the Bedrock Runtime provider.
//
// Hand-rolled with the standard library only, matching this repo's zero-dependency
// convention (the Kiro path likewise hand-rolls its AWS event-stream parser rather
// than pulling in aws-sdk-go). Scope is deliberately narrow: sign a single POST to
// bedrock-runtime with a static IAM access key (optionally a session token). It is
// NOT a general SigV4 implementation — no query signing, no streaming payload
// (SigV4A / chunked) signing, no presigning.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// awsCredentials is a static IAM credential set. SessionToken is empty for
// long-lived access keys and set for STS/temporary credentials.
type awsCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// sha256Hex returns the lowercase hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// awsURIEncodePath percent-encodes a URL path per the SigV4 canonical-URI rule for
// non-S3 services: unreserved characters (A-Z a-z 0-9 - _ . ~) and the path
// separator "/" pass through; everything else (notably ":" in a Bedrock modelId
// such as ...-v2:0) becomes %XX with uppercase hex. The input must already be a raw
// path (leading slash, real segment characters), not a pre-encoded one.
func awsURIEncodePath(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			const hexUpper = "0123456789ABCDEF"
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0f])
		}
	}
	return b.String()
}

// signSigV4 signs req in place, adding X-Amz-Date, Authorization, and (when present)
// X-Amz-Security-Token headers. payload is the exact request body bytes that will be
// sent; host is taken from req.URL.Host. The set of signed headers is fixed to
// host;x-amz-date;content-type (plus x-amz-security-token when a session token is
// present) — additional unsigned headers (e.g. Accept) may still be sent.
//
// now is injectable so tests can pin the timestamp; callers pass time.Now().
func signSigV4(req *http.Request, payload []byte, creds awsCredentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	host := req.URL.Host
	contentType := req.Header.Get("Content-Type")

	// Signed header set (lowercase names, sorted). Keep in sync with canonicalHeaders.
	signed := []string{"content-type", "host", "x-amz-date"}
	headerValue := map[string]string{
		"content-type": contentType,
		"host":         host,
		"x-amz-date":   amzDate,
	}
	if creds.SessionToken != "" {
		signed = append(signed, "x-amz-security-token")
		headerValue["x-amz-security-token"] = creds.SessionToken
	}
	sort.Strings(signed)

	var canonHeaders strings.Builder
	for _, name := range signed {
		canonHeaders.WriteString(name)
		canonHeaders.WriteByte(':')
		// Trim surrounding whitespace; inner runs are left as-is (values here have none).
		canonHeaders.WriteString(strings.TrimSpace(headerValue[name]))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(signed, ";")

	canonicalURI := awsURIEncodePath(req.URL.Path)
	canonicalQuery := req.URL.RawQuery // empty for the invoke endpoints; if set, must already be sorted+encoded

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonHeaders.String(),
		signedHeaders,
		sha256Hex(payload),
	}, "\n")

	credentialScope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	// Derive the signing key: HMAC chain over date, region, service, terminator.
	kDate := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	authorization := "AWS4-HMAC-SHA256 " +
		"Credential=" + creds.AccessKeyID + "/" + credentialScope + ", " +
		"SignedHeaders=" + signedHeaders + ", " +
		"Signature=" + signature
	req.Header.Set("Authorization", authorization)
}

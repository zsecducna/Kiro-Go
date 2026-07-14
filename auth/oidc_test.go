package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
)

// TestRefreshLockCollapsesConcurrentRefreshes verifies that N goroutines
// refreshing the same expired-token account collapse into a single IdP POST:
// the first caller refreshes+persists under the per-account lock, and every
// follower's double-check reads the now-fresh token from config and skips the
// POST. Before the fix each goroutine POSTed independently (refresh-storm +
// lost-update on refresh-token rotation).
func TestRefreshLockCollapsesConcurrentRefreshes(t *testing.T) {
	// The refresh POST goes through postExternalIdpToken, which re-validates the
	// endpoint at the outbound boundary; the httptest server is http+127.0.0.1,
	// which the real allow-list validator rejects. Install the no-op test seam
	// (same pattern as kiro_sso_test.go / import_credentials_test.go).
	restore := SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer SetExternalIdpValidatorForTest(restore)

	var postCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&postCount, 1)
		// Hold the response so the other goroutines are definitely waiting on
		// the lock while the leader POSTs (makes the test robust to scheduling).
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "at-fresh",
			"refresh_token": "rt-rotated",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:            "acct",
		AuthMethod:    "external_idp",
		ClientID:      "cid",
		TokenEndpoint: srv.URL,
		Scopes:        "offline_access",
		RefreshToken:  "rt-old",
		ExpiresAt:     1, // expired → every caller wants a refresh
		Enabled:       true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	tokens := make([]string, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			acc := config.Account{
				ID: "acct", AuthMethod: "external_idp", ClientID: "cid",
				TokenEndpoint: srv.URL, Scopes: "offline_access",
				RefreshToken: "rt-old", ExpiresAt: 1,
			}
			<-start // release all goroutines at once to maximise overlap
			at, _, _, _, err := RefreshToken(&acc)
			tokens[idx] = at
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d refresh failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Fatalf("expected exactly 1 IdP POST (collapse), got %d", got)
	}
	for i, tk := range tokens {
		if tk != "at-fresh" {
			t.Fatalf("goroutine %d got token %q, want the shared fresh token", i, tk)
		}
	}
}

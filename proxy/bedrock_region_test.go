package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
)

// TestInvokeBedrockRegionalLoop drives the region-hop loop against an httptest
// server (bearer auth so no SigV4/host dependency; buildReq encodes the region in
// the query so the mock can answer per region). It locks the review's HIGH fix:
// a genuine error or transport blip mid-sweep must NOT negative-cache the model.
func TestInvokeBedrockRegionalLoop(t *testing.T) {
	h := &Handler{}
	responder := map[string]struct {
		code int
		body string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := responder[r.URL.Query().Get("region")]
		w.WriteHeader(resp.code)
		io.WriteString(w, resp.body)
	}))
	defer srv.Close()

	// Hermetic client with an explicit no-proxy transport so the request never
	// touches http.ProxyFromEnvironment (whose process-wide sync.Once would poison
	// unrelated proxy-env tests). Restore the default afterward.
	prevClient := bedrockHTTPClientFor
	bedrockHTTPClientFor = func(*config.Account) *http.Client {
		return &http.Client{Transport: &http.Transport{Proxy: nil}}
	}
	defer func() { bedrockHTTPClientFor = prevClient }()

	build := func(region string) (*http.Request, error) {
		return http.NewRequest(http.MethodPost, srv.URL+"/?region="+region, bytes.NewReader([]byte("{}")))
	}
	mkParams := func(acctID string, regions []string) forwardParams {
		return forwardParams{account: &config.Account{ID: acctID, Region: regions[0], BedrockRegions: regions[1:], BedrockAPIKey: "ABSKtest"}}
	}
	notAllowed := struct {
		code int
		body string
	}{400, `{"message":"Operation not allowed"}`}

	// Case A: region1 access-error (hop) -> region2 genuine ValidationException.
	// Must surface the validation error and must NOT mark the model knownDead.
	responder["us-east-1"] = notAllowed
	responder["eu-west-1"] = struct {
		code int
		body string
	}{400, `{"message":"ValidationException: bad input"}`}
	pA := mkParams("acctA", []string{"us-east-1", "eu-west-1"})
	respA, errA := h.invokeBedrockRegional(pA, "mA", []byte("{}"), build)
	if errA != nil || respA == nil || respA.StatusCode != 400 {
		t.Fatalf("case A: resp=%v err=%v, want 400 surfaced", respA, errA)
	}
	respA.Body.Close()
	if _, dead := orderedBedrockRegions(pA.account, "mA"); dead {
		t.Error("case A: genuine error mid-sweep must NOT cache knownDead")
	}
	clearBedrockRegionRoutes("acctA")

	// Case B: every region access-errors -> negative cache (knownDead) so the next
	// request fails over fast.
	responder["us-east-1"] = notAllowed
	responder["eu-west-1"] = notAllowed
	pB := mkParams("acctB", []string{"us-east-1", "eu-west-1"})
	respB, _ := h.invokeBedrockRegional(pB, "mB", []byte("{}"), build)
	if respB != nil {
		respB.Body.Close()
	}
	if _, dead := orderedBedrockRegions(pB.account, "mB"); !dead {
		t.Error("case B: all-access-error sweep must cache knownDead")
	}
	clearBedrockRegionRoutes("acctB")

	// Case C: region1 access-error -> region2 200. Winner region2 cached.
	responder["us-east-1"] = notAllowed
	responder["eu-west-1"] = struct {
		code int
		body string
	}{200, `{"ok":true}`}
	pC := mkParams("acctC", []string{"us-east-1", "eu-west-1"})
	respC, errC := h.invokeBedrockRegional(pC, "mC", []byte("{}"), build)
	if errC != nil || respC == nil || respC.StatusCode != 200 {
		t.Fatalf("case C: resp=%v err=%v, want 200", respC, errC)
	}
	respC.Body.Close()
	if r, ok := getBedrockRoute("acctC", "mC"); !ok || r.region != "eu-west-1" || !r.callable {
		t.Errorf("case C: learned route=%+v, want eu-west-1 callable", r)
	}
	clearBedrockRegionRoutes("acctC")
}

func TestCandidateRegions(t *testing.T) {
	// Primary first, extras appended, blanks/dupes removed.
	a := &config.Account{Region: "us-east-1", BedrockRegions: []string{"eu-west-1", " ", "us-east-1", "us-west-2"}}
	got := candidateRegions(a)
	want := []string{"us-east-1", "eu-west-1", "us-west-2"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	// Empty region defaults to us-east-1.
	if r := candidateRegions(&config.Account{}); len(r) != 1 || r[0] != "us-east-1" {
		t.Errorf("default = %v, want [us-east-1]", r)
	}
}

func TestCleanRegionList(t *testing.T) {
	got := cleanRegionList([]string{" eu-west-1 ", "", "us-west-2", "eu-west-1"})
	if len(got) != 2 || got[0] != "eu-west-1" || got[1] != "us-west-2" {
		t.Errorf("cleanRegionList = %v", got)
	}
}

func TestBedrockAccessError(t *testing.T) {
	callableHops := []struct {
		status int
		body   string
	}{
		{400, `{"message":"Operation not allowed"}`},
		{400, `{"message":"The provided model identifier is invalid."}`},
		{400, `{"message":"Invocation with on-demand throughput isn't supported, use an inference profile"}`},
		{403, `{"message":"You are not authorized to perform this operation"}`},
		{404, `{"message":"This model version has reached the end of its life."}`},
	}
	for _, c := range callableHops {
		if !bedrockAccessError(c.status, []byte(c.body)) {
			t.Errorf("expected access-error (hop) for %d %s", c.status, c.body)
		}
	}
	// Genuine request errors must NOT trigger a region hop.
	notHops := []struct {
		status int
		body   string
	}{
		{400, `{"message":"ValidationException: malformed input"}`},
		{400, `{"message":"The requested feature is not supported for this model"}`}, // "not supported" w/o "throughput"
		{500, `{"message":"Operation not allowed"}`},                                 // wrong status class
		{200, `ok`},
	}
	for _, c := range notHops {
		if bedrockAccessError(c.status, []byte(c.body)) {
			t.Errorf("did NOT expect access-error for %d %s", c.status, c.body)
		}
	}
}

func TestOrderedBedrockRegionsAndCache(t *testing.T) {
	a := &config.Account{ID: "acct-region-1", Region: "us-east-1", BedrockRegions: []string{"eu-west-1", "us-west-2"}}
	defer clearBedrockRegionRoutes(a.ID)

	// Cold: full candidate list, primary first.
	got, dead := orderedBedrockRegions(a, "m1")
	if dead || len(got) != 3 || got[0] != "us-east-1" {
		t.Fatalf("cold = %v dead=%v", got, dead)
	}

	// Learn a callable region -> it moves to the front, rest follow as fallback.
	recordBedrockRegion(a.ID, "m1", "eu-west-1", true)
	got, dead = orderedBedrockRegions(a, "m1")
	if dead || got[0] != "eu-west-1" || len(got) != 3 {
		t.Fatalf("after learn = %v dead=%v, want eu-west-1 first", got, dead)
	}
	if r, ok := getBedrockRoute(a.ID, "m1"); !ok || r.region != "eu-west-1" || !r.callable {
		t.Errorf("route = %+v", r)
	}

	// Not-callable-anywhere verdict -> knownDead so the caller fails over fast.
	recordBedrockRegion(a.ID, "m2", "", false)
	got, dead = orderedBedrockRegions(a, "m2")
	if !dead || got != nil {
		t.Errorf("dead verdict = %v dead=%v, want nil+true", got, dead)
	}
}

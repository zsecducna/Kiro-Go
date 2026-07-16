package proxy

import (
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOpenAIStreamEmitsFinishChunkOnMidStreamError drives the REAL
// handleOpenAIStream error-after-started path. The mock upstream returns one
// valid toolUseEvent frame (stop:true) — parseEventStream invokes
// handleToolUseEvent which calls finishToolUse mid-stream, firing OnToolUse so
// handleOpenAIStream sets responseStarted=true and emits a tool_calls chunk —
// followed by truncated garbage, so the next 12-byte prelude read fails and
// parseEventStream returns io.ErrUnexpectedEOF. CallKiroAPI therefore returns a
// non-nil error AFTER the response has already begun.
//
// Before the fix the error path (responseStarted==true) returned without
// emitting anything, silently truncating the client's SSE stream (no
// finish_reason, no [DONE]). After the fix it emits a finish chunk with
// finish_reason:"error" plus a terminating data: [DONE] line.
func TestOpenAIStreamEmitsFinishChunkOnMidStreamError(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if err := config.AddAccount(config.Account{
		ID:          "acct",
		Enabled:     true,
		AccessToken: "token-x",
		ProfileArn:  "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// One valid toolUseEvent with stop:true and a name but no toolUseId:
		// handleToolUseEvent generates an id, then because stop is set it calls
		// finishToolUse immediately → OnToolUse fires mid-stream, setting
		// responseStarted=true in handleOpenAIStream.
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"name":  "mcpTestTool",
			"input": `{"x":"y"}`,
			"stop":  true,
		}))
		// Truncated garbage: the next prelude read cannot complete, so
		// parseEventStream returns io.ErrUnexpectedEOF → CallKiroAPI returns an
		// error AFTER responseStarted is already true.
		_, _ = w.Write([]byte{0xAB, 0xCD, 0x01})
	}))
	defer server.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	defer func() { kiroEndpoints = oldEndpoints }()

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	rec := httptest.NewRecorder()
	h.handleOpenAIStream(rec, payload, "claude-sonnet-4.5", false, 1, "", nil, false)

	body := rec.Body.String()
	if !strings.Contains(body, `"finish_reason":"error"`) {
		t.Fatalf("expected a finish chunk with finish_reason:\"error\", got body=%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected a terminating data: [DONE] line, got body=%s", body)
	}
}

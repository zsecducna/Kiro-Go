package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClaudeToKiroTruncatesOversizedHistory builds a conversation whose history
// far exceeds the upstream input limit and verifies the converted payload is
// trimmed below maxPayloadBytes, that a truncation placeholder is inserted, and
// that the current message is preserved.
func TestClaudeToKiroTruncatesOversizedHistory(t *testing.T) {
	// ~2KB chunk repeated across many turns to blow past the byte limit.
	big := strings.Repeat("lorem ipsum dolor sit amet ", 80) // ~2.1KB

	msgs := []ClaudeMessage{
		{Role: "user", Content: "start the long task"},
	}
	for i := 0; i < 800; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "step result: " + big},
			ClaudeMessage{Role: "user", Content: "next: " + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize everything above"})

	req := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}

	payload := ClaudeToKiro(req, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if len(raw) > maxPayloadBytes {
		t.Fatalf("payload size %d exceeds limit %d after truncation", len(raw), maxPayloadBytes)
	}

	// The current message must be preserved.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL: summarize everything above") {
		t.Fatalf("current message lost after truncation, got %q", cur.Content[:min(80, len(cur.Content))])
	}

	// A truncation placeholder must be present in history.
	foundPlaceholder := false
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			foundPlaceholder = true
			break
		}
	}
	if !foundPlaceholder {
		t.Fatalf("expected a truncation placeholder in history")
	}

	// System priming should still be at the front.
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected priming retained, history too short")
	}
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil || !strings.Contains(primingUser.Content, "helpful assistant") {
		t.Fatalf("expected system priming retained at front")
	}
}

// TestClaudeToKiroSmallPayloadNotTruncated ensures normal-sized conversations
// are left untouched (no placeholder inserted).
func TestClaudeToKiroSmallPayloadNotTruncated(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-opus-4.8",
		System: "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "how are you?"},
		},
	}
	payload := ClaudeToKiro(req, false)
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			t.Fatalf("small payload should not be truncated")
		}
	}
}

// TestMaxPayloadTokens verifies the wire-side token ceiling is 80% of the
// upstream window, and that every sonnet/haiku model is pinned to the 200K
// window Kiro actually serves them behind (regardless of advertised window).
func TestMaxPayloadTokens(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-sonnet-4.5", 160_000},
		{"claude-sonnet-4", 160_000},
		{"claude-haiku-4.5", 160_000},
		{"claude-sonnet-4.6", 160_000}, // advertised 1M, but Kiro serves 200K
		{"claude-sonnet-4.8", 160_000}, // future sonnet stays pinned
		{"claude-haiku-4.9", 160_000},  // future haiku stays pinned
		{"CLAUDE-SONNET-4.6", 160_000}, // case-insensitive
		{"claude-opus-4.8", 800_000},   // opus keeps its 1M window
		{"claude-opus-4.5", 160_000},   // opus 4.5 is a 200K model
		{"unknown-model", 160_000},     // unknown falls back to 200K
	}
	for _, c := range cases {
		if got := maxPayloadTokens(c.model); got != c.want {
			t.Errorf("maxPayloadTokens(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// TestClaudeToKiroTruncatesToTokenWindowUnderByteLimit is the regression test for
// the upstream 400 "Input is too long." It builds a digit-dense conversation that
// stays well under maxPayloadBytes but blows far past sonnet's 200K token window
// — the exact shape the old byte-only check forwarded untouched.
func TestClaudeToKiroTruncatesToTokenWindowUnderByteLimit(t *testing.T) {
	// Digits are estimated at ~2 bytes/token, so ~500KB of digits is ~250K
	// tokens: under the 900KB byte ceiling, over the 160K token ceiling.
	big := strings.Repeat("0123456789", 500) // ~5KB per turn

	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 50; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "result: " + big},
			ClaudeMessage{Role: "user", Content: "next: " + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize"})

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}

	payload := ClaudeToKiro(req, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	// Guard the premise: this input must be under the byte ceiling, so that a
	// failure here means the token ceiling did the work (not the byte one).
	if len(raw) > maxPayloadBytes {
		t.Fatalf("test premise broken: payload %d bytes exceeds byte limit %d", len(raw), maxPayloadBytes)
	}

	limit := maxPayloadTokens("claude-sonnet-4.5")
	if got := estimateKiroPayloadTokens(payload); got > limit {
		t.Fatalf("payload %d tokens exceeds sonnet limit %d after truncation", got, limit)
	}

	// The current message must survive truncation.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL: summarize") {
		t.Fatalf("current message lost after truncation")
	}
}

// TestClaudeToKiroTokenWindowIsModelAware verifies the same conversation is
// trimmed for a 200K sonnet but left intact for a 1M opus.
func TestClaudeToKiroTokenWindowIsModelAware(t *testing.T) {
	big := strings.Repeat("0123456789", 500)

	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 50; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "result: " + big},
			ClaudeMessage{Role: "user", Content: "next: " + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize"})

	build := func(model string) *KiroPayload {
		return ClaudeToKiro(&ClaudeRequest{
			Model:    model,
			System:   "You are a helpful assistant.",
			Messages: msgs,
		}, false)
	}

	sonnet := build("claude-sonnet-4.5")
	opus := build("claude-opus-4.8")

	// Opus has a 1M window and ~250K tokens of history, so nothing is dropped,
	// while sonnet's 160K ceiling forces turns out.
	if len(sonnet.ConversationState.History) >= len(opus.ConversationState.History) {
		t.Fatalf("expected sonnet history (%d) to be trimmed below opus history (%d)",
			len(sonnet.ConversationState.History), len(opus.ConversationState.History))
	}
	if got := estimateKiroPayloadTokens(opus); got > maxPayloadTokens("claude-opus-4.8") {
		t.Fatalf("opus payload %d tokens exceeds its own limit", got)
	}
	// Opus should retain the full conversation untouched (no elision placeholder).
	for _, h := range opus.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			t.Fatalf("opus payload should not be truncated at ~250K tokens")
		}
	}
}

// TestClaudeToKiroTruncatesOversizedCurrentToolResult covers the most common
// real trigger for the upstream 400: a single huge tool result on the outgoing
// turn. It lives in structured toolResults, not in the message text, so trimming
// history or message content alone never brings it under the window.
func TestClaudeToKiroTruncatesOversizedCurrentToolResult(t *testing.T) {
	huge := strings.Repeat("0123456789", 70_000) // ~700KB, ~350K tokens

	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "read the file"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "tool_1", "name": "read", "input": map[string]interface{}{"path": "big.txt"}},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_1", "content": huge},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)

	limit := maxPayloadTokens("claude-sonnet-4.5")
	if got := estimateKiroPayloadTokens(payload); got > limit {
		t.Fatalf("payload %d tokens exceeds sonnet limit %d after truncation", got, limit)
	}
	if raw, _ := json.Marshal(payload); len(raw) > maxPayloadBytes {
		t.Fatalf("payload %d bytes exceeds byte limit %d", len(raw), maxPayloadBytes)
	}

	// The result must be shrunk in place rather than discarded: the tool turn is
	// small, so the budget belongs to the result text. Dropping the exchange and
	// flattening to a summary would also fit, but throws away ~80x more content.
	msgCtx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if msgCtx == nil || len(msgCtx.ToolResults) == 0 {
		t.Fatalf("expected the tool result to be retained (shrunk), not discarded")
	}
	retained := 0
	for _, tr := range msgCtx.ToolResults {
		for _, c := range tr.Content {
			retained += len(c.Text)
		}
	}
	if retained < 100_000 {
		t.Fatalf("expected most of the tool result retained, got %d bytes", retained)
	}
	// ...and it must still answer the assistant turn that requested it.
	if !currentToolResultsMatchLastAssistant(payload.ConversationState.History, collectToolResultIDs(msgCtx.ToolResults)) {
		t.Fatalf("retained tool results are orphaned")
	}
}

// TestClaudeToKiroToolResultsStayPairedAfterTruncation guards the invariant that
// Kiro enforces: structured tool results must answer the structured toolUses of
// the last assistant turn. Truncation may drop that turn, and when it does the
// results must be flattened into text rather than left orphaned.
func TestClaudeToKiroToolResultsStayPairedAfterTruncation(t *testing.T) {
	// A tool_use whose input alone dwarfs the window, forcing the active tool
	// turn out of history.
	hugeInput := strings.Repeat("0123456789", 40_000) // ~400KB, ~200K tokens

	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run it"},
			{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "tool_1", "name": "run", "input": map[string]interface{}{"script": hugeInput}},
				},
			},
			{
				Role: "user",
				Content: []interface{}{
					map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_1", "content": "done"},
				},
			},
		},
	}

	payload := ClaudeToKiro(req, false)

	limit := maxPayloadTokens("claude-sonnet-4.5")
	if got := estimateKiroPayloadTokens(payload); got > limit {
		t.Fatalf("payload %d tokens exceeds sonnet limit %d", got, limit)
	}

	// If structured tool results survived, the last history assistant must still
	// carry the matching toolUses; otherwise upstream rejects the request.
	msgCtx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if msgCtx != nil && len(msgCtx.ToolResults) > 0 {
		ids := collectToolResultIDs(msgCtx.ToolResults)
		if !currentToolResultsMatchLastAssistant(payload.ConversationState.History, ids) {
			t.Fatalf("orphaned structured tool results survived truncation")
		}
	} else {
		// Flattened instead — the result text must not have been silently lost.
		cur := payload.ConversationState.CurrentMessage.UserInputMessage
		if !strings.Contains(cur.Content, "done") {
			t.Fatalf("tool result text lost when flattened, content=%q", cur.Content)
		}
	}
}

// TestClaudeToKiroTruncatesCJKHistory verifies the token ceiling holds for
// CJK-heavy conversations. UTF-8 CJK is 3 bytes/char, so a payload can sit under
// the byte ceiling while carrying far more real tokens than an ASCII payload of
// the same size — the estimator must not undercount it into an upstream 400.
func TestClaudeToKiroTruncatesCJKHistory(t *testing.T) {
	big := strings.Repeat("这是一个很长的中文句子用来测试上下文窗口限制。", 200) // ~4.6K chars/turn

	msgs := []ClaudeMessage{{Role: "user", Content: "开始"}}
	for i := 0; i < 60; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "结果：" + big},
			ClaudeMessage{Role: "user", Content: "继续：" + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "最后：请总结"})

	payload := ClaudeToKiro(&ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}, false)

	limit := maxPayloadTokens("claude-sonnet-4.5")
	if got := estimateKiroPayloadTokens(payload); got > limit {
		t.Fatalf("CJK payload %d tokens exceeds sonnet limit %d", got, limit)
	}

	// Every retained CJK character must cost at least ~1 token against the
	// budget; at 200K+ retained chars the request would 400 upstream.
	cjk := 0
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			cjk += len([]rune(h.UserInputMessage.Content))
		}
		if h.AssistantResponseMessage != nil {
			cjk += len([]rune(h.AssistantResponseMessage.Content))
		}
	}
	if cjk > sonnetHaikuContextWindow {
		t.Fatalf("retained %d CJK chars, over the %d-token window at ~1 token/char", cjk, sonnetHaikuContextWindow)
	}
}

// TestEstimateWireTokensIsConservative verifies the wire estimator never
// undercounts relative to the reporting estimator, since it is used as a hard
// bound rather than a usage number.
func TestEstimateWireTokensIsConservative(t *testing.T) {
	samples := []string{
		strings.Repeat("hello world this is prose ", 50),
		strings.Repeat("这是中文文本内容测试", 50),
		strings.Repeat("0123456789", 50),
		strings.Repeat("{}[]<>!@#$%^&*()", 50),
		strings.Repeat("mixed 混合 text 123 {}", 50),
	}
	for _, s := range samples {
		wire, reported := estimateWireTokens(s), estimateApproxTokens(s)
		if wire < reported {
			t.Errorf("estimateWireTokens undercounts reporting estimate: %d < %d for %.20q", wire, reported, s)
		}
	}
	// CJK must cost at least ~1 token per character on the wire path.
	cjk := strings.Repeat("中", 1000)
	if got := estimateWireTokens(cjk); got < 1000 {
		t.Errorf("estimateWireTokens(%d CJK chars) = %d, want >= 1000", 1000, got)
	}
}

// TestClaudeToKiroDropsImagesOnByteOverflow covers the one content class that is
// byte-heavy but token-cheap: images are estimated at a flat per-image cost, so a
// payload can sit far under the token ceiling while its base64 blows past the
// byte ceiling. Gating the image drop on tokens alone would forward it anyway.
func TestClaudeToKiroDropsImagesOnByteOverflow(t *testing.T) {
	// ~400KB of base64 per image, well under the token ceiling (1600 each).
	blob := strings.Repeat("A", 400_000)
	img := func() interface{} {
		return map[string]interface{}{
			"type":   "image",
			"source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": blob},
		}
	}

	req := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: "You are a helpful assistant.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "describe these"},
				img(), img(), img(), // ~1.2MB of base64 → over maxPayloadBytes
			}},
		},
	}

	payload := ClaudeToKiro(req, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if len(raw) > maxPayloadBytes {
		t.Fatalf("payload %d bytes exceeds byte limit %d — images not dropped on byte overflow", len(raw), maxPayloadBytes)
	}
	if got := estimateKiroPayloadTokens(payload); got > maxPayloadTokens("claude-sonnet-4.5") {
		t.Fatalf("payload %d tokens exceeds limit", got)
	}
}

// TestTruncateStringToBytes verifies the byte cut never splits a multi-byte rune.
func TestTruncateStringToBytes(t *testing.T) {
	// "héllo" — é occupies bytes 1-2.
	s := "héllo"
	for n := 0; n <= len(s); n++ {
		got := truncateStringToBytes(s, n)
		if len(got) > n {
			t.Fatalf("truncateStringToBytes(%q, %d) = %q, longer than limit", s, n, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncateStringToBytes(%q, %d) = %q, invalid UTF-8", s, n, got)
		}
	}
	if got := truncateStringToBytes(s, len(s)+10); got != s {
		t.Fatalf("expected full string when budget exceeds length, got %q", got)
	}
}

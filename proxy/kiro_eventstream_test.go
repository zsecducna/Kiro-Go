package proxy

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestParseEventStreamRejectsOversizedTotalLength verifies parseEventStream
// rejects a frame whose totalLength exceeds the sanity cap BEFORE the
// make([]byte, remaining) allocation. A corrupt/malicious (or bit-flipped)
// 32-bit totalLength near 2^32 would otherwise drive a multi-GB allocation:
// either an alloc panic (net/http recovers per-request → connection dropped
// with no terminal event → client hang) or a multi-GB buffer held to the 5-min
// client timeout (memory-pressure DoS). The cap keeps the test itself from
// allocating gigabytes: it feeds totalLength = cap+1.
func TestParseEventStreamRejectsOversizedTotalLength(t *testing.T) {
	over := maxEventStreamMessageBytes + 1
	prelude := make([]byte, 12) // prelude: total_len(4) + headers_len(4) + crc(4)
	prelude[0] = byte(over >> 24)
	prelude[1] = byte(over >> 16)
	prelude[2] = byte(over >> 8)
	prelude[3] = byte(over)
	// headers_len bytes (4-7) and crc (8-11) left zero.

	err := parseEventStream(bytes.NewReader(prelude), &KiroStreamCallback{})
	if !errors.Is(err, errEventStreamFrameTooLarge) {
		t.Fatalf("oversized totalLength must be rejected before the multi-GB make; got %v", err)
	}
}

// closeTrackingBody is an io.ReadCloser whose Close() is observable, for tests
// that assert an upstream body was returned to the transport pool.
type closeTrackingBody struct {
	data   []byte
	offset int
	closed bool
}

func (b *closeTrackingBody) Read(p []byte) (int, error) {
	if b.offset >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.offset:])
	b.offset += n
	return n, nil
}

func (b *closeTrackingBody) Close() error { b.closed = true; return nil }

// buildAssistantResponseEvent builds a minimal AWS event-stream frame carrying
// an assistantResponseEvent with the given content, so a test can drive
// parseEventStream's OnText callback without a live upstream. The prelude and
// message CRC32 fields are zeroed — parseEventStream does not verify them.
func buildAssistantResponseEvent(content string) []byte {
	payload := []byte(`{"content":"` + content + `"}`)
	const eventType = "assistantResponseEvent"
	// Header: nameLen(1) + ":event-type"(11) + type=7 String(1) + valueLen(2 BE) + value
	var h bytes.Buffer
	h.WriteByte(11)
	h.WriteString(":event-type")
	h.WriteByte(7)
	h.WriteByte(byte(len(eventType) >> 8))
	h.WriteByte(byte(len(eventType)))
	h.WriteString(eventType)
	headers := h.Bytes()
	totalLength := 12 + len(headers) + len(payload) + 4
	var f bytes.Buffer
	f.WriteByte(byte(totalLength >> 24))
	f.WriteByte(byte(totalLength >> 16))
	f.WriteByte(byte(totalLength >> 8))
	f.WriteByte(byte(totalLength))
	f.WriteByte(byte(len(headers) >> 24))
	f.WriteByte(byte(len(headers) >> 16))
	f.WriteByte(byte(len(headers) >> 8))
	f.WriteByte(byte(len(headers)))
	f.Write(make([]byte, 4)) // prelude CRC (zeroed; parseEventStream does not verify)
	f.Write(headers)
	f.Write(payload)
	f.Write(make([]byte, 4)) // message CRC (zeroed; parseEventStream does not verify)
	return f.Bytes()
}

// TestParseAndStreamClosesBodyOnCallbackPanic verifies parseAndStream returns
// the upstream body to the transport pool (resp.Body.Close()) even when a
// streaming callback panics. The 200 path's old code was
// `err = parseEventStream(resp.Body, callback); resp.Body.Close()` — a plain
// statement, so a panic in OnText/OnToolUse unwound past it and leaked the TCP
// connection (repeated panics exhaust MaxIdleConnsPerHost=20 / FDs). The defer
// in parseAndStream closes on every path including panic.
func TestParseAndStreamClosesBodyOnCallbackPanic(t *testing.T) {
	body := &closeTrackingBody{data: buildAssistantResponseEvent("hello")}
	cb := &KiroStreamCallback{
		OnText: func(string, bool) { panic("callback boom") },
	}
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		_ = parseAndStream(body, cb)
	}()
	if !panicked {
		t.Fatal("test setup: OnText did not fire/panic — the fixture did not reach the callback; fix buildAssistantResponseEvent")
	}
	if !body.closed {
		t.Fatal("parseAndStream must close the upstream body even when a callback panics, or the TCP connection leaks / the idle pool exhausts under repeated panics")
	}
}

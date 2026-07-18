package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// encodeStringHeader appends one event-stream string header (value type 7).
func encodeStringHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteByte(byte(len(name)))
	buf.WriteString(name)
	buf.WriteByte(7) // string
	var vl [2]byte
	binary.BigEndian.PutUint16(vl[:], uint16(len(value)))
	buf.Write(vl[:])
	buf.WriteString(value)
}

// buildFrame assembles one AWS event-stream frame with the given headers and
// payload. CRCs are filled with zero bytes because readBedrockEventStream does not
// validate them (it slices them off by fixed width).
func buildFrame(headers map[string]string, payload []byte) []byte {
	var hb bytes.Buffer
	for k, v := range headers {
		encodeStringHeader(&hb, k, v)
	}
	headersLen := hb.Len()
	total := 12 + headersLen + len(payload) + 4

	var f bytes.Buffer
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], uint32(total))
	f.Write(tmp[:])
	binary.BigEndian.PutUint32(tmp[:], uint32(headersLen))
	f.Write(tmp[:])
	f.Write([]byte{0, 0, 0, 0}) // prelude CRC (ignored)
	f.Write(hb.Bytes())
	f.Write(payload)
	f.Write([]byte{0, 0, 0, 0}) // message CRC (ignored)
	return f.Bytes()
}

func TestReadBedrockEventStream_Chunk(t *testing.T) {
	inner := []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`)
	envelope, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(inner)})

	frame := buildFrame(
		map[string]string{":message-type": "event", ":event-type": "chunk", ":content-type": "application/json"},
		envelope,
	)

	var gotType string
	var gotJSON []byte
	err := readBedrockEventStream(bytes.NewReader(frame), func(eventType string, anthropicJSON []byte) error {
		gotType = eventType
		gotJSON = append([]byte(nil), anthropicJSON...)
		return nil
	})
	if err != nil {
		t.Fatalf("readBedrockEventStream: %v", err)
	}
	if gotType != "chunk" {
		t.Errorf("eventType = %q, want chunk", gotType)
	}
	if !bytes.Equal(gotJSON, inner) {
		t.Errorf("decoded inner event mismatch:\n got: %s\nwant: %s", gotJSON, inner)
	}
	// innerEventType should recover the Anthropic SSE name for emission.
	if n := innerEventType(gotJSON); n != "content_block_delta" {
		t.Errorf("innerEventType = %q, want content_block_delta", n)
	}
}

func TestReadBedrockEventStream_Exception(t *testing.T) {
	payload := []byte(`{"message":"model access denied"}`)
	frame := buildFrame(
		map[string]string{":message-type": "exception", ":exception-type": "AccessDeniedException"},
		payload,
	)

	err := readBedrockEventStream(bytes.NewReader(frame), func(string, []byte) error {
		t.Fatal("onEvent should not be called for an exception frame")
		return nil
	})
	if err == nil {
		t.Fatal("expected error for exception frame, got nil")
	}
	be, ok := err.(*bedrockStreamError)
	if !ok {
		t.Fatalf("expected *bedrockStreamError, got %T: %v", err, err)
	}
	if be.EventType != "AccessDeniedException" {
		t.Errorf("EventType = %q, want AccessDeniedException", be.EventType)
	}
	if be.Message != "model access denied" {
		t.Errorf("Message = %q, want %q", be.Message, "model access denied")
	}
}

func TestReadBedrockEventStream_UsageExtraction(t *testing.T) {
	start := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":42}}}`)
	delta := []byte(`{"type":"message_delta","usage":{"output_tokens":17}}`)
	if got := extractInputTokens(start); got != 42 {
		t.Errorf("extractInputTokens = %d, want 42", got)
	}
	if got := extractOutputTokens(delta); got != 17 {
		t.Errorf("extractOutputTokens = %d, want 17", got)
	}
}

package wtwire

import (
	"bytes"
	"testing"
)

func TestCreateSessionReplyMaximumPayload(t *testing.T) {
	msg := &CreateSessionReply{
		Data: make([]byte, MaxCreateSessionReplyDataLength),
	}
	var buf bytes.Buffer
	if _, err := WriteMessage(&buf, msg, 0); err != nil {
		t.Fatalf("write maximum reply: %v", err)
	}
	if got := buf.Len() - 2; got != int(msg.MaxPayloadLength(0)) {
		t.Fatalf(
			"payload size %d, want %d", got,
			msg.MaxPayloadLength(0),
		)
	}
	if _, err := ReadMessage(&buf, 0); err != nil {
		t.Fatalf("read maximum reply: %v", err)
	}
}

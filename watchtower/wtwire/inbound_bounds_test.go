package wtwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestInboundFrameBounds(t *testing.T) {
	tests := []Message{
		&CreateSessionReply{
			Data: make([]byte, MaxCreateSessionReplyDataLength),
		},
		&DeleteSession{},
		&StateUpdateReply{},
	}
	for _, msg := range tests {
		t.Run(msg.MsgType().String(), func(t *testing.T) {
			var frame bytes.Buffer
			if _, err := WriteMessage(&frame, msg, 0); err != nil {
				t.Fatal(err)
			}
			raw := frame.Bytes()
			framed := io.LimitReader(
				bytes.NewReader(raw), int64(len(raw)),
			)
			if _, err := ReadMessage(framed, 0); err != nil {
				t.Fatal(err)
			}
			padding := int(msg.MaxPayloadLength(0)) + 1 -
				(len(raw) - 2)
			oversized := append(append([]byte(nil), raw...),
				make([]byte, padding)...)
			reader := bytes.NewReader(append(oversized, 42))
			framed = io.LimitReader(reader, int64(len(oversized)))
			if _, err := ReadMessage(framed, 0); err == nil {
				t.Fatal("oversized frame accepted")
			}
			if reader.Len() != len(oversized)-2+1 {
				t.Fatalf(
					"read beyond type: %d bytes remain",
					reader.Len(),
				)
			}
		})
	}
}

func TestInboundUnframedSequentialMessages(t *testing.T) {
	var b bytes.Buffer
	for range 2 {
		if _, err := WriteMessage(&b, &DeleteSession{}, 0); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := ReadMessage(&b, 0); err != nil {
			t.Fatal(err)
		}
	}
	if b.Len() != 0 {
		t.Fatal("unconsumed bytes")
	}
}

// TestInboundFramedVarBytesLengthBound rejects lengths larger than the frame
// before trying to read the declared payload.
func TestInboundFramedVarBytesLengthBound(t *testing.T) {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint16(MsgError))
	b.Write([]byte{0, 0, 0xfd, 0x00, 0x10})
	frame := io.LimitReader(&b, int64(b.Len()))
	_, err := ReadMessage(frame, 0)
	if err == nil || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {

		t.Fatalf("expected frame length rejection, got %v", err)
	}
	if b.Len() != 0 {
		t.Fatalf("unexpected remaining bytes: %d", b.Len())
	}
}

func TestInboundVarBytesLengthBound(t *testing.T) {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint16(MsgCreateSessionReply))
	b.Write([]byte{0, 0, 0, 0, 0xfd, 0xff, 0xff})
	_, err := ReadMessage(&b, 0)
	if err == nil || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {

		t.Fatalf(
			"expected length rejection before reading data, got %v",
			err,
		)
	}
}

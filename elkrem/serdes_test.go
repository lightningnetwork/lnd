package elkrem

import (
	"bytes"
	"testing"
)

func ReceiverSerdesTest(t *testing.T, er ElkremReceiver) {
	b, err := er.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Serialized receiver; %d bytes, hex:\n%x\n", len(b), b)

	rcv2, err := ElkremReceiverFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}

	b2, err := rcv2.ToBytes()
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(b, b2) {
		t.Fatalf("First and second serializations different")
	}
}

func SenderSerdesTest(t *testing.T, es ElkremSender) {
	b, err := es.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Serialized sender; %d bytes, hex:\n%x\n", len(b), b)

	sndr2, err := ElkremSenderFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}

	b2, err := sndr2.ToBytes()
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(b, b2) {
		t.Fatalf("First and second serializations different")
	}
}

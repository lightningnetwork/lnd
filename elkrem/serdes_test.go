package elkrem

import (
	"bytes"
	"testing"
)

func ReceiverSerdesTest(t *testing.T, rcv *ElkremReceiver) {
	b, err := rcv.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rcv2 has len %d\n", len(rcv.s))
	t.Logf("Serialized receiver; %d bytes, hex:\n%x\n", len(b), b)

	rcv2, err := ElkremReceiverFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rcv2 has len %d\n", len(rcv2.s))
	b2, err := rcv2.ToBytes()
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(b, b2) {
		t.Fatalf("First and second serializations different")
	}
}

//func SenderSerdesTest(t *testing.T, sndr *ElkremSender) {
//	b, err := sndr.ToBytes()
//	if err != nil {
//		t.Fatal(err)
//	}
//	t.Logf("Serialized sender; %d bytes, hex:\n%x\n", len(b), b)

//	*sndr, err = ElkremSenderFromBytes(b)
//	if err != nil {
//		t.Fatal(err)
//	}

//	b2, err := sndr.ToBytes()
//	if err != nil {
//		t.Fatal(err)
//	}

//	if !bytes.Equal(b, b2) {
//		t.Fatalf("First and second serializations different")
//	}
//}

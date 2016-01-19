package elkrem

import "testing"

func ReceiverSerdesTest(t *testing.T, er ElkremReceiver) {
	b, err := er.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Serialized receiver; %d bytes, hex:\n%x\n", len(b), b)
}

func SenderSerdesTest(t *testing.T, es ElkremSender) {
	b, err := es.ToBytes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Serialized sender; %d bytes, hex:\n%x\n", len(b), b)
}

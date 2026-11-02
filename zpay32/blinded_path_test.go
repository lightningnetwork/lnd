package zpay32

import (
	"bytes"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

func TestDecodeBlindedPaymentRejectsZeroHops(t *testing.T) {
	t.Parallel()

	var encoded bytes.Buffer
	encoded.Write(make([]byte, 26))

	features := lnwire.EmptyFeatureVector()
	if err := features.Encode(&encoded); err != nil {
		t.Fatalf("unable to encode features: %v", err)
	}

	_, blindingPoint := btcec.PrivKeyFromBytes([]byte{1})
	encoded.Write(blindingPoint.SerializeCompressed())
	encoded.WriteByte(0)

	_, err := DecodeBlindedPayment(&encoded)
	if err == nil {
		t.Fatal("expected zero-hop blinded payment path to fail decoding")
	}
	if !strings.Contains(err.Error(), "at least one hop") {
		t.Fatalf("unexpected error: %v", err)
	}
}

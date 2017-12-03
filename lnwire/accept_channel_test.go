package lnwire

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec"
)

// TestDecodeAcceptChannel tests decoding of an accept channel wire message with
// and without the optional upfront shutdown address.
func TestDecodeAcceptChannel(t *testing.T) {
	addr, err := randDeliveryAddress(rand.New(rand.NewSource(123)))
	if err != nil {
		t.Fatalf("cannot create random addresss: %v", err)
	}
	tests := []struct {
		name     string
		shutdown DeliveryAddress
	}{
		{
			name:     "No upfront shutdown",
			shutdown: nil,
		},
		{
			name:     "Upfront shutdown set",
			shutdown: addr,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			priv, err := btcec.NewPrivateKey(btcec.S256())
			if err != nil {
				t.Fatalf("cannot create privkey: %v", err)
			}
			pk := priv.PubKey()

			withField := &AcceptChannel{
				PendingChannelID:      [32]byte{},
				FundingKey:            pk,
				RevocationPoint:       pk,
				PaymentPoint:          pk,
				DelayedPaymentPoint:   pk,
				HtlcPoint:             pk,
				FirstCommitmentPoint:  pk,
				UpfrontShutdownScript: test.shutdown,
			}

			var b []byte
			buf := bytes.NewBuffer(b)
			if _, err := WriteMessage(buf, withField, 0); err != nil {
				t.Fatalf("cannot write message: %v", err)
			}

			msg, err := ReadMessage(buf, 0)
			if err != nil {
				t.Fatalf("cannot read message: %v", err)
			}

			switch e := msg.(type) {
			case *AcceptChannel:
				if !reflect.DeepEqual(e, withField) {
					t.Fatalf("decoded message: %v does not equal encoded message: %v",
						e, withField)
				}
			default:
				t.Fatalf("did not decode accept channel messge")
			}
		})
	}
}

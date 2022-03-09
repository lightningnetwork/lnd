package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// TestDecodeAcceptChannel tests decoding of an accept channel wire message with
// and without the optional upfront shutdown script.
func TestDecodeAcceptChannel(t *testing.T) {
	tests := []struct {
		name           string
		shutdownScript DeliveryAddress
	}{
		{
			name:           "no upfront shutdown script",
			shutdownScript: nil,
		},
		{
			name:           "empty byte array",
			shutdownScript: []byte{},
		},
		{
			name:           "upfront shutdown script set",
			shutdownScript: []byte("example"),
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			priv, err := btcec.NewPrivateKey()
			if err != nil {
				t.Fatalf("cannot create privkey: %v", err)
			}
			pk := priv.PubKey()

			encoded := &AcceptChannel{
				PendingChannelID:      [32]byte{},
				FundingKey:            pk,
				RevocationPoint:       pk,
				PaymentPoint:          pk,
				DelayedPaymentPoint:   pk,
				HtlcPoint:             pk,
				FirstCommitmentPoint:  pk,
				UpfrontShutdownScript: test.shutdownScript,
			}

			buf := &bytes.Buffer{}
			if _, err := WriteMessage(buf, encoded, 0); err != nil {
				t.Fatalf("cannot write message: %v", err)
			}

			msg, err := ReadMessage(buf, 0)
			if err != nil {
				t.Fatalf("cannot read message: %v", err)
			}

			decoded := msg.(*AcceptChannel)
			if !bytes.Equal(
				decoded.UpfrontShutdownScript, encoded.UpfrontShutdownScript,
			) {

				t.Fatalf("decoded script: %x does not equal encoded script: %x",
					decoded.UpfrontShutdownScript, encoded.UpfrontShutdownScript)
			}
		})
	}
}

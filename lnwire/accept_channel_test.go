package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
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

// TestAcceptChannelEncodeDecode tests that a raw byte stream can be
// decoded, then re-encoded to the same exact byte stream.
func TestAcceptChannelEncodeDecode(t *testing.T) {
	t.Parallel()

	// Create a new private key and its corresponding public key.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pk := priv.PubKey()

	// Create a sample AcceptChannel message with all fields populated.
	// The exact values are not important, only that they are of the
	// correct size.
	var rawBytes []byte

	// PendingChannelID
	rawBytes = append(rawBytes, make([]byte, 32)...)

	// DustLimit
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 1}...)

	// MaxValueInFlight
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 2}...)

	// ChannelReserve
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 3}...)

	// HtlcMinimum
	rawBytes = append(rawBytes, []byte{0, 0, 0, 0, 0, 0, 0, 4}...)

	// MinAcceptDepth
	rawBytes = append(rawBytes, []byte{0, 0, 0, 5}...)

	// CsvDelay
	rawBytes = append(rawBytes, []byte{0, 6}...)

	// MaxAcceptedHTLCs
	rawBytes = append(rawBytes, []byte{0, 7}...)

	// FundingKey
	rawBytes = append(rawBytes, pk.SerializeCompressed()...)

	// RevocationPoint
	rawBytes = append(rawBytes, pk.SerializeCompressed()...)

	// PaymentPoint
	rawBytes = append(rawBytes, pk.SerializeCompressed()...)

	// DelayedPaymentPoint
	rawBytes = append(rawBytes, pk.SerializeCompressed()...)

	// HtlcPoint
	rawBytes = append(rawBytes, pk.SerializeCompressed()...)

	// FirstCommitmentPoint
	rawBytes = append(rawBytes, pk.SerializeCompressed()...)

	// Add TLV data, including known and unknown records.
	tlvData := []byte{
		// UpfrontShutdownScript (known, type 0)
		0,          // type
		2,          // length
		0xaa, 0xbb, // value

		// ChannelType (known, type 1)
		1,    // type
		1,    // length
		0x02, // value (feature bit 1 set)

		// Unknown odd-type TLV record.
		0x3,        // type
		0x2,        // length
		0xab, 0xcd, // value

		// LocalNonce (known, type 4)
		4,  // type
		66, // length
		// 66 bytes of dummy data
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11,
		0x11, 0x11,

		// Another unknown odd-type TLV record.
		0x6f,       // type
		0x2,        // length
		0x79, 0x79, // value

		// LeaseExpiry (known, type 65536)
		0xfe, 0x00, 0x01, 0x00, 0x00, // type
		4,                      // length
		0x12, 0x34, 0x56, 0x78, // value
	}
	rawBytes = append(rawBytes, tlvData...)

	// Now, create a new empty message and decode the raw bytes into it.
	msg := &AcceptChannel{}
	r := bytes.NewReader(rawBytes)
	err = msg.Decode(r, 0)
	require.NoError(t, err)

	// Next, encode the message back into a new byte buffer.
	var b bytes.Buffer
	err = msg.Encode(&b, 0)
	require.NoError(t, err)

	// The re-encoded bytes should be exactly the same as the original raw
	// bytes.
	require.Equal(t, rawBytes, b.Bytes())
}

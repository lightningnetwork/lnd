package trampoline

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

const (
	officialCarolPayload = "020405f5e100 04030c3500 " +
		"0e2102edabbd16b41c8371b92ef2f04c1185b4f03b6dcd52ba9b78d9d" +
		"7c89c8f221145"

	officialEvePayload = "020405f5e100 04030c3500 " +
		"08242a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a" +
		"2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a05f5e100"
)

// TestOfficialPayloadRoundTrip verifies the two inner payloads from the BOLT
// proposal encode canonically after being decoded.
func TestOfficialPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hex  string
	}{
		{
			name: "intermediate",
			hex:  officialCarolPayload,
		},
		{
			name: "final",
			hex:  officialEvePayload,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			encoded := mustHex(t, test.hex)
			reader := bytes.NewReader(encoded)
			payload, _, err := DecodePayload(reader)
			require.NoError(t, err)

			var b bytes.Buffer
			require.NoError(t, payload.Encode(&b))
			require.Equal(t, encoded, b.Bytes())
		})
	}
}

// TestPayloadRoundTrip exercises every implemented trampoline payload record.
func TestPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x21}, 32))
	amount := uint64(50_000)
	cltv := uint32(700)
	total := uint64(49_000)
	features := lnwire.NewRawFeatureVector(
		lnwire.TrampolineRoutingOptional,
	)
	packet := mustDecodeOnion(t, officialInnerOnion)
	intro, err := lnwire.NewPubkeyIntro(key.PubKey())
	require.NoError(t, err)
	blindedPaths := &RecipientBlindedPaths{
		Paths: []RecipientBlindedPath{{
			Path: lnwire.BlindedPath{
				IntroductionNode: intro,
				BlindingPoint:    key.PubKey(),
				Hops: []lnwire.BlindedHop{{
					BlindedNodeID: key.PubKey(),
					EncryptedData: []byte{6, 7},
				}},
			},
			PaymentInfo: bolt12.BlindedPayInfo{
				FeeBaseMsat:               1,
				FeeProportionalMillionths: 2,
				CltvExpiryDelta:           3,
				HtlcMinimumMsat:           4,
				HtlcMaximumMsat:           5,
				Features: *lnwire.NewRawFeatureVector(
					lnwire.TrampolineRoutingOptional,
				),
			},
		}},
	}

	payload := &Payload{
		AmountToForward:        &amount,
		OutgoingCLTV:           &cltv,
		PaymentData:            record.NewMPP(49_000, [32]byte{1}),
		EncryptedRecipientData: []byte{2, 3},
		CurrentPathKey:         key.PubKey(),
		OutgoingNodeID:         key.PubKey(),
		PaymentMetadata:        []byte{4, 5},
		TotalAmountMsat:        &total,
		TrampolineOnion:        packet,
		RecipientFeatures:      features,
		RecipientBlindedPaths:  blindedPaths,
	}

	var b bytes.Buffer
	require.NoError(t, payload.Encode(&b))

	decoded, parsed, err := DecodePayload(bytes.NewReader(b.Bytes()))
	require.NoError(t, err)
	require.Len(t, parsed, 11)
	require.Equal(t, amount, *decoded.AmountToForward)
	require.Equal(t, cltv, *decoded.OutgoingCLTV)
	require.Equal(t, payload.PaymentData, decoded.PaymentData)
	require.Equal(
		t, payload.CurrentPathKey.SerializeCompressed(),
		decoded.CurrentPathKey.SerializeCompressed(),
	)
	require.Equal(
		t, payload.OutgoingNodeID.SerializeCompressed(),
		decoded.OutgoingNodeID.SerializeCompressed(),
	)
	require.True(t, decoded.RecipientFeatures.IsSet(
		lnwire.TrampolineRoutingOptional,
	))
	require.Equal(t, blindedPaths, decoded.RecipientBlindedPaths)

	var reencoded bytes.Buffer
	require.NoError(t, decoded.Encode(&reencoded))
	require.Equal(t, b.Bytes(), reencoded.Bytes())
}

// TestDecodePayloadRejectsRequiredAndNonMinimalFields verifies fail-closed
// handling for unknown even records and non-minimal feature vectors.
func TestDecodePayloadRejectsRequiredAndNonMinimalFields(t *testing.T) {
	t.Parallel()

	var unknownEven bytes.Buffer
	require.NoError(t, tlv.WriteVarInt(&unknownEven, 24, new([8]byte)))
	require.NoError(t, tlv.WriteVarInt(&unknownEven, 0, new([8]byte)))
	_, _, err := DecodePayload(bytes.NewReader(unknownEven.Bytes()))
	require.ErrorContains(t, err, "unknown required")

	nonMinimalFeatures, err := hex.DecodeString("150100")
	require.NoError(t, err)
	_, _, err = DecodePayload(bytes.NewReader(nonMinimalFeatures))
	require.ErrorContains(t, err, "non-minimal recipient features")

	var unknownOdd bytes.Buffer
	require.NoError(t, tlv.WriteVarInt(&unknownOdd, 23, new([8]byte)))
	require.NoError(t, tlv.WriteVarInt(&unknownOdd, 0, new([8]byte)))
	_, parsed, err := DecodePayload(bytes.NewReader(unknownOdd.Bytes()))
	require.NoError(t, err)
	require.Contains(t, parsed, tlv.Type(23))
}

// FuzzDecodePayload exercises canonical TLV decoding and re-parses every
// successfully re-encoded known payload.
func FuzzDecodePayload(f *testing.F) {
	f.Add(mustHex(f, officialCarolPayload))
	f.Add(mustHex(f, officialEvePayload))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		payload, _, err := DecodePayload(bytes.NewReader(data))
		if err != nil {
			return
		}

		var b bytes.Buffer
		if err := payload.Encode(&b); err != nil {
			return
		}

		_, _, err = DecodePayload(bytes.NewReader(b.Bytes()))
		require.NoError(t, err)
	})
}

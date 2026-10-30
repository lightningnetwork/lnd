package chanstate

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

var (
	key = [chainhash.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	privKey, pubKey = btcec.PrivKeyFromBytes(key[:])

	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571319d18e" +
		"949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a88121167221b67" +
		"00d72a0ead154c03be696a292d24ae")
	testRScalar = new(btcec.ModNScalar)
	testSScalar = new(btcec.ModNScalar)
	_           = testRScalar.SetByteSlice(testRBytes)
	_           = testSScalar.SetByteSlice(testSBytes)
	testSig     = ecdsa.NewSignature(testRScalar, testSScalar)
)

// TestHTLCsExtraData tests serialization and deserialization of HTLCs
// combined with extra data.
func TestHTLCsExtraData(t *testing.T) {
	t.Parallel()

	mockHtlc := HTLC{
		Signature:     testSig.Serialize(),
		Incoming:      false,
		Amt:           10,
		RHash:         key,
		RefundTimeout: 1,
		OnionBlob:     lnmock.MockOnion(),
	}

	// Add a blinding point to a htlc.
	blindingPointHTLC := HTLC{
		Signature:     testSig.Serialize(),
		Incoming:      false,
		Amt:           10,
		RHash:         key,
		RefundTimeout: 1,
		OnionBlob:     lnmock.MockOnion(),
		BlindingPoint: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
				pubKey,
			),
		),
	}

	// Custom channel data htlc with a blinding point.
	customDataHTLC := HTLC{
		Signature:     testSig.Serialize(),
		Incoming:      false,
		Amt:           10,
		RHash:         key,
		RefundTimeout: 1,
		OnionBlob:     lnmock.MockOnion(),
		BlindingPoint: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
				pubKey,
			),
		),
		CustomRecords: map[uint64][]byte{
			uint64(lnwire.MinCustomRecordsTlvType + 3): {1, 2, 3},
		},
	}

	testCases := []struct {
		name        string
		htlcs       []HTLC
		blindingIdx int
	}{
		{
			// Serialize multiple HLTCs with no extra data to
			// assert that there is no regression for HTLCs with
			// no extra data.
			name: "no extra data",
			htlcs: []HTLC{
				mockHtlc, mockHtlc,
			},
		},
		{
			// Some HTLCs with extra data, some without.
			name: "mixed extra data",
			htlcs: []HTLC{
				mockHtlc,
				blindingPointHTLC,
				mockHtlc,
				customDataHTLC,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			err := SerializeHtlcs(&b, testCase.htlcs...)
			require.NoError(t, err)

			r := bytes.NewReader(b.Bytes())
			htlcs, err := DeserializeHtlcs(r)
			require.NoError(t, err)

			require.EqualValues(t, len(testCase.htlcs), len(htlcs))
			for i, htlc := range htlcs {
				// We use the extra data field when we
				// serialize, so we set to nil to be able to
				// assert on equal for the test.
				htlc.ExtraData = nil
				require.Equal(t, testCase.htlcs[i], htlc)
			}
		})
	}
}

// TestOnionBlobIncorrectLength tests HTLC deserialization in the case where
// the OnionBlob saved on disk is of an unexpected length. This error case is
// only expected in the case of database corruption (or some severe protocol
// breakdown/bug). A HTLC is manually serialized because we cannot force a
// case where we write an onion blob of incorrect length.
func TestOnionBlobIncorrectLength(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer

	var numHtlcs uint16 = 1
	require.NoError(t, WriteElement(&b, numHtlcs))

	require.NoError(t, WriteElements(
		&b,
		// Number of HTLCs.
		numHtlcs,
		// Signature, incoming, amount, Rhash, Timeout.
		testSig.Serialize(), false, lnwire.MilliSatoshi(10), key,
		uint32(1),
		// Write an onion blob that is half of our expected size.
		bytes.Repeat([]byte{1}, lnwire.OnionPacketSize/2),
	))

	_, err := DeserializeHtlcs(&b)
	require.ErrorIs(t, err, ErrOnionBlobLength)
}

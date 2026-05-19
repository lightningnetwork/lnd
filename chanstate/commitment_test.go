package chanstate

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

func TestHTLCCopy(t *testing.T) {
	t.Parallel()

	_, blindingPoint := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{1}, 32))

	var rHash [32]byte
	copy(rHash[:], bytes.Repeat([]byte{2}, len(rHash)))

	var onionBlob [lnwire.OnionPacketSize]byte
	copy(onionBlob[:], bytes.Repeat([]byte{3}, len(onionBlob)))

	htlc := HTLC{
		Signature:     []byte{4, 5, 6},
		RHash:         rHash,
		Amt:           1000,
		RefundTimeout: 144,
		OutputIndex:   3,
		Incoming:      true,
		OnionBlob:     onionBlob,
		HtlcIndex:     42,
		LogIndex:      43,
		ExtraData:     lnwire.ExtraOpaqueData{7, 8, 9},
		BlindingPoint: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[lnwire.BlindingPointTlvType](
				blindingPoint,
			),
		),
		CustomRecords: lnwire.CustomRecords{
			lnwire.MinCustomRecordsTlvType: []byte{10, 11, 12},
		},
	}

	clone := htlc.Copy()
	require.Equal(t, htlc, clone)

	clone.Signature[0] = 0
	require.Equal(t, byte(4), htlc.Signature[0])

	clone.ExtraData[0] = 0
	require.Equal(t, byte(7), htlc.ExtraData[0])

	clone.CustomRecords[lnwire.MinCustomRecordsTlvType] = []byte{0}
	require.Equal(
		t, []byte{10, 11, 12},
		htlc.CustomRecords[lnwire.MinCustomRecordsTlvType],
	)

	clone = htlc.Copy()
	clone.CustomRecords[lnwire.MinCustomRecordsTlvType][0] = 0
	require.Equal(
		t, []byte{10, 11, 12},
		htlc.CustomRecords[lnwire.MinCustomRecordsTlvType],
	)
}

package lnwire

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestUpdateAddHTLCExtraData tests encoding of update_add_htlc with and
// without a blinding point TLV included.
func TestUpdateAddHTLCExtraData(t *testing.T) {
	t.Parallel()

	// First, test an update_add_htlc that does not include a blinding
	// point.
	htlc := UpdateAddHTLC{
		ID:        1,
		Amount:    100,
		Expiry:    25,
		ExtraData: make([]byte, 0),
	}

	var b bytes.Buffer
	require.NoError(t, htlc.Encode(&b, 0))

	var htlcDecoded UpdateAddHTLC
	require.NoError(t, htlcDecoded.Decode(&b, 0))

	require.Equal(t, htlc, htlcDecoded)

	// Next test inclusion of a blinding point.
	pubKeyStr := "036a0c5ea35df8a528b98edf6f290b28676d51d0fe202b073fe677612a39c0aa09" //nolint:lll
	pubHex, err := hex.DecodeString(pubKeyStr)
	require.NoError(t, err, "unable to decode pubkey")

	pubKey, err := btcec.ParsePubKey(pubHex)
	require.NoError(t, err, "unable to parse pubkey")
	blindingPoint := blindingPoint(*pubKey)

	htlc.BlindingPoint = &blindingPoint

	require.NoError(t, htlc.Encode(&b, 0))
	require.NoError(t, htlcDecoded.Decode(&b, 0))
	require.Equal(t, htlc, htlcDecoded)
}

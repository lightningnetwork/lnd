package trampoline

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

const officialRecipientBlindedPaths = "" +
	"032c0b7cf95324a07d05398b240174dc0c2be444d96b159aa6c7f7b1e668680991" +
	"02988face71e92c345a068f740191fd8e53be14f0bb957ef730d3c5f76087b960e" +
	"020295d40514096a8be54859e7dfe947b376eaafea8afe5cb4eb2c13ff857ed0b4" +
	"be002b0ae636dc5963bcfe2a4705538b3b6d2c5cd87dce29374d47cb64d16b3a0" +
	"d95f21b1af81f31f61c01e81a86020e2dbadcc2005e859819ddebbe88a834ae8a" +
	"6d2b049233c07335f15cd1dc5f2200d1bcd747ba974bc6ac175df8d5dbd462acb" +
	"1dc4f3fa1de21da4c5774d233d8ecd9b84b7420175f9ec920f2ef261cdb83dc28" +
	"cc3a0eeb970107b3306489bf771ef5b1213bca811d345285405861d08a655b6c23" +
	"7fa247a8b4491beee20c878a60e9816492026d8feb9dafa84585b253978db6a0aa" +
	"2945df5ef445c61e801fb82f43d59347cc1c013a2351f094cdafb5e0d1f5ccb10" +
	"55d6a5dd086a69cd75d34ea06067659cb7bb02dda9c2d89978dc725168f93ab2f" +
	"e22dff354bce6017b60d0cc5b29b01540595e6d024f3812adda1960b4d000001f4" +
	"000003e800240000000000000001000000001dcd65000000"

// TestOfficialRecipientBlindedPaths verifies the type 22 value from the first
// blinded-payment vector in lightning/bolts#836 at commit 644b82f1.
func TestOfficialRecipientBlindedPaths(t *testing.T) {
	t.Parallel()

	encoded := mustHex(t, officialRecipientBlindedPaths)
	require.Len(t, encoded, 417)

	paths, err := DecodeRecipientBlindedPaths(encoded)
	require.NoError(t, err)
	require.Len(t, paths.Paths, 1)

	path := paths.Paths[0]
	require.Len(t, path.Path.Hops, 2)
	require.Equal(t, uint32(500), path.PaymentInfo.FeeBaseMsat)
	require.Equal(
		t, uint32(1000),
		path.PaymentInfo.FeeProportionalMillionths,
	)
	require.Equal(t, uint16(36), path.PaymentInfo.CltvExpiryDelta)
	require.Equal(t, uint64(1), path.PaymentInfo.HtlcMinimumMsat)
	require.Equal(t, uint64(500_000_000), path.PaymentInfo.HtlcMaximumMsat)
	require.True(t, path.PaymentInfo.Features.IsEmpty())

	var reencoded bytes.Buffer
	require.NoError(t, paths.Encode(&reencoded))
	require.Equal(t, encoded, reencoded.Bytes())
}

// TestRecipientBlindedPathsRejectsInvalidSequences verifies that the decoder
// rejects missing, truncated, and trailing unmatched path data.
func TestRecipientBlindedPathsRejectsInvalidSequences(t *testing.T) {
	t.Parallel()

	_, err := DecodeRecipientBlindedPaths(nil)
	require.ErrorIs(t, err, ErrNoRecipientBlindedPaths)

	encoded := mustHex(t, officialRecipientBlindedPaths)
	for _, cut := range []int{1, 33, 67, len(encoded) - 1} {
		_, err := DecodeRecipientBlindedPaths(encoded[:cut])
		require.Error(t, err)
	}
}

// FuzzDecodeRecipientBlindedPaths exercises the complete untrusted type 22
// parser and asserts successful parses have a stable canonical encoding.
func FuzzDecodeRecipientBlindedPaths(f *testing.F) {
	f.Add(mustHex(f, officialRecipientBlindedPaths))
	f.Add([]byte{})
	f.Add([]byte{0})

	f.Fuzz(func(t *testing.T, data []byte) {
		paths, err := DecodeRecipientBlindedPaths(data)
		if err != nil {
			return
		}

		var encoded bytes.Buffer
		require.NoError(t, paths.Encode(&encoded))
		require.Equal(t, data, encoded.Bytes())
	})
}

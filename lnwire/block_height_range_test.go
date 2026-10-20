package lnwire

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestBlockHeightRangeRejectsShort tests that a block height range shorter
// than its fixed-width first block height is rejected.
func TestBlockHeightRangeRejectsShort(t *testing.T) {
	t.Parallel()

	decode := func(raw []byte) error {
		var rng BlockHeightRange

		return blockHeightRangeDecoder(
			bytes.NewReader(raw), &rng, &[8]byte{},
			uint64(len(raw)),
		)
	}

	for length := range 4 {
		require.ErrorAs(
			t, decode(make([]byte, length)),
			&tlv.ErrTypeForDecoding{},
		)
	}

	// Four bytes is the first block height with an empty block count.
	require.NoError(t, decode(make([]byte, 4)))
}

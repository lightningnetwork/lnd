package offers

import (
	"testing"

	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

// TestMarshalOfferHashLength verifies that marshalOffer rejects a stored
// offer hash that is not 32 bytes long, and accepts one that is.
func TestMarshalOfferHashLength(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 31, 33} {
		_, err := marshalOffer(sqlc.Offer{Hash: make([]byte, n)})
		require.Error(t, err, "length %d", n)
	}

	hash := make([]byte, 32)
	hash[0] = 1
	offer, err := marshalOffer(sqlc.Offer{Hash: hash})
	require.NoError(t, err)
	require.Equal(t, [32]byte{1}, offer.Hash)
}

package bolt12handler

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/bolt12handler/envelope"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestReconstruct_DisabledOffer verifies that the first HTLC for an invoice of
// a disabled offer settles nothing.
func TestReconstruct_DisabledOffer(t *testing.T) {
	t.Parallel()

	r, nodeKey, offer := newTestReconstructor(t)
	offer.IsDisabled = true

	env := buildTestEnvelope(t, nodeKey, &envelope.Data{
		OfferHash: fn.Some(offer.Hash),
		CreatedAt: uint64(time.Now().Unix()),
		Amount:    testOfferPrice,
	})

	_, err := env.reconstruct(t, r)
	require.ErrorIs(t, err, ErrOfferDisabled)
}

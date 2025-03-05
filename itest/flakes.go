package itest

import (
	"time"

	"github.com/lightningnetwork/lnd/lntest"
)

// flakePreimageSettlement documents a flake found when testing the preimage
// extraction logic in a force close. The scenario is,
//   - Alice and Bob have a channel.
//   - Alice sends an HTLC to Bob, and Bob won't settle it.
//   - Alice goes offline.
//   - Bob force closes the channel and claims the HTLC using the preimage using
//     a sweeping tx.
//
// TODO(yy): Expose blockbeat to the link layer so the preimage extraction
// happens in the same block where it's spent.
func flakePreimageSettlement(ht *lntest.HarnessTest) {
	// Mine a block to trigger the sweep. This is needed because the
	// preimage extraction logic from the link is not managed by the
	// blockbeat, which means the preimage may be sent to the contest
	// resolver after it's launched, which means Bob would miss the block to
	// launch the resolver.
	ht.MineEmptyBlocks(1)

	// Sleep for 2 seconds, which is needed to make sure the mempool has the
	// correct tx. The above mined block can cause an RBF, if the preimage
	// extraction has already been finished before the block is mined. This
	// means Bob would have created the sweeping tx - mining another block
	// would cause the sweeper to RBF it.
	time.Sleep(2 * time.Second)
}

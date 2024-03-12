package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testQuiescence tests whether we can come to agreement on quiescence of a
// channel. We initiate quiescence via RPC and if it succeeds we verify that
// the expected initiator is the resulting initiator.
//
// NOTE FOR REVIEW: this could be improved by blasting the channel with HTLC
// traffic on both sides to increase the surface area of the change under test.
func testQuiescence(ht *lntest.HarnessTest) {
	alice, bob := ht.Alice, ht.Bob

	chanPoint := ht.OpenChannel(bob, alice, lntest.OpenChannelParams{
		Amt: btcutil.Amount(1000000),
	})
	defer ht.CloseChannel(bob, chanPoint)

	ht.AssertTopologyChannelOpen(bob, chanPoint)

	res, err := alice.RPC.Quiesce(&lnrpc.QuiescenceRequest{
		ChanId: chanPoint,
	})

	require.NoError(ht, err)
	require.True(ht, res.Initiator)
}

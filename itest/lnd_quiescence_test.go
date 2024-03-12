package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

func testQuiescence(ht *lntest.HarnessTest) {
	alice, bob:= ht.Alice, ht.Bob
	// ht.ConnectNodes(alice, bob)

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
package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

func testCoopCloseWithExternalDelivery(ht *lntest.HarnessTest) {
	ht.Run("set delivery address at open", func(t *testing.T) {
		tt := ht.Subtest(t)
		testCoopCloseWithExternalDeliveryImpl(tt, true)
	})
	ht.Run("set delivery address at close", func(t *testing.T) {
		tt := ht.Subtest(t)
		testCoopCloseWithExternalDeliveryImpl(tt, false)
	})
}

// testCoopCloseWithExternalDeliveryImpl ensures that we have a valid settled
// balance irrespective of whether the delivery address is in LND's wallet or
// not. Some users set this value to be an address in a different wallet and
// this should not affect our ability to accurately report the settled balance.
func testCoopCloseWithExternalDeliveryImpl(ht *lntest.HarnessTest,
	upfrontShutdown bool) {

	alice, bob := ht.Alice, ht.Bob
	ht.ConnectNodes(alice, bob)

	// Here we generate a final delivery address in bob's wallet but set by
	// alice. We do this to ensure that the address is not in alice's LND
	// wallet. We already correctly track settled balances when the address
	// is in the LND wallet.
	addr := bob.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_UNUSED_WITNESS_PUBKEY_HASH,
	})

	// Prepare for channel open.
	openParams := lntest.OpenChannelParams{
		Amt: btcutil.Amount(1000000),
	}

	// If we are testing the case where we set it on open then we'll set the
	// upfront shutdown script in the channel open parameters.
	if upfrontShutdown {
		openParams.CloseAddress = addr.Address
	}

	// Open the channel!
	chanPoint := ht.OpenChannel(alice, bob, openParams)

	// Prepare for channel close.
	closeParams := lnrpc.CloseChannelRequest{
		ChannelPoint: chanPoint,
		TargetConf:   6,
	}

	// If we are testing the case where we set the delivery address on
	// channel close then we will set it in the channel close parameters.
	if !upfrontShutdown {
		closeParams.DeliveryAddress = addr.Address
	}

	// Close the channel!
	closeClient := alice.RPC.CloseChannel(&closeParams)

	// Assert that we got a channel update when we get a closing txid.
	_, err := closeClient.Recv()
	require.NoError(ht, err)

	// Mine the closing transaction.
	ht.MineClosingTx(chanPoint)

	// Assert that we got a channel update when the closing tx was mined.
	_, err = closeClient.Recv()
	require.NoError(ht, err)

	// Here we query our closed channels to conduct the final test
	// assertion. We want to ensure that even though alice's delivery
	// address is set to an address in bob's wallet, we should still show
	// the balance as settled.
	closed := alice.RPC.ClosedChannels(&lnrpc.ClosedChannelsRequest{
		Cooperative: true,
	})

	// The settled balance should never be zero at this point.
	require.NotZero(ht, len(closed.Channels))
	require.NotZero(ht, closed.Channels[0].SettledBalance)
}

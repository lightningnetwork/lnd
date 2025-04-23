package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

var coopCloseWithExternalTestCases = []*lntest.TestCase{
	{
		Name: "set P2WPKH delivery address at open",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, true, false, false,
				lnrpc.AddressType_UNUSED_WITNESS_PUBKEY_HASH,
			)
		},
	},
	{
		Name: "set P2WPKH delivery address at close",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, false, false, false,
				lnrpc.AddressType_UNUSED_WITNESS_PUBKEY_HASH,
			)
		},
	},
	{
		Name: "set P2TR delivery address at open",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, true, false, false,
				lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY,
			)
		},
	},
	{
		Name: "set P2TR delivery address at close",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, false, false, false,
				lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY,
			)
		},
	},
	{
		Name: "set imported P2TR address (ImportTapscript) at open",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, true, true, false,
				lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY,
			)
		},
	},
	{
		Name: "set imported P2TR address (ImportTapscript) at close",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, false, true, false,
				lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY,
			)
		},
	},
	{
		Name: "set imported P2WPKH address at open",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, true, false, true,
				lnrpc.AddressType_UNUSED_WITNESS_PUBKEY_HASH,
			)
		},
	},
	{
		Name: "set imported P2WPKH address at close",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, false, false, true,
				lnrpc.AddressType_UNUSED_WITNESS_PUBKEY_HASH,
			)
		},
	},
	{
		Name: "set imported P2TR address (ImportPublicKey) at open",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, true, false, true,
				lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY,
			)
		},
	},
	{
		Name: "set imported P2TR address (ImportPublicKey) at close",
		TestFunc: func(ht *lntest.HarnessTest) {
			testCoopCloseWithExternalDelivery(
				ht, false, false, true,
				lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY,
			)
		},
	},
}

// testCoopCloseWithExternalDelivery ensures that we have a valid settled
// balance irrespective of whether the delivery address is in LND's wallet or
// not. Some users set this value to be an address in a different wallet and
// this should not affect our ability to accurately report the settled balance.
//
// If importTapscript is set, it imports a Taproot script and internal key to
// Alice's LND using ImportTapscript to make sure it doesn't interfere with
// delivery address. If importPubkey is set, the address is imported using
// ImportPublicKey.
func testCoopCloseWithExternalDelivery(ht *lntest.HarnessTest,
	upfrontShutdown, importTapscript, importPubkey bool,
	deliveryAddressType lnrpc.AddressType) {

	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("bob", nil)
	ht.ConnectNodes(alice, bob)

	// Make fake taproot internal public key (equal to the final).
	// This is only used if importTapscript or importPubkey is set.
	taprootPubkey := [32]byte{1, 2, 3}
	if upfrontShutdown {
		// Make new address for second sub-test not to import
		// the same address twice causing an error.
		taprootPubkey[3] = 1
	}
	pkScriptBytes := append(
		[]byte{txscript.OP_1, txscript.OP_DATA_32},
		taprootPubkey[:]...,
	)
	pkScript, err := txscript.ParsePkScript(pkScriptBytes)
	require.NoError(ht, err)
	taprootAddress, err := pkScript.Address(harnessNetParams)
	require.NoError(ht, err)

	var addr string
	switch {
	// Use ImportTapscript.
	case importTapscript:
		addr = taprootAddress.String()

		// Import the taproot address to LND.
		req := &walletrpc.ImportTapscriptRequest{
			InternalPublicKey: taprootPubkey[:],
			Script: &walletrpc.ImportTapscriptRequest_FullKeyOnly{
				FullKeyOnly: true,
			},
		}
		res := alice.RPC.ImportTapscript(req)
		require.Equal(ht, addr, res.P2TrAddress)

	// Use ImportPublicKey.
	case importPubkey:
		var (
			address     btcutil.Address
			pubKey      []byte
			addressType walletrpc.AddressType
		)
		switch deliveryAddressType {
		case lnrpc.AddressType_UNUSED_WITNESS_PUBKEY_HASH:
			// Make fake public key hash.
			pk := [33]byte{2, 3, 4}
			if upfrontShutdown {
				// Make new address for second sub-test.
				pk[1]++
			}
			address, err = btcutil.NewAddressWitnessPubKeyHash(
				btcutil.Hash160(pk[:]), harnessNetParams,
			)
			require.NoError(ht, err)
			pubKey = pk[:]
			addressType = walletrpc.AddressType_WITNESS_PUBKEY_HASH

		case lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY:
			address = taprootAddress
			pubKey = taprootPubkey[:]
			addressType = walletrpc.AddressType_TAPROOT_PUBKEY

		default:
			ht.Fatalf("not allowed address type: %v",
				deliveryAddressType)
		}

		addr = address.String()

		// Import the address to LND.
		alice.RPC.ImportPublicKey(&walletrpc.ImportPublicKeyRequest{
			PublicKey:   pubKey,
			AddressType: addressType,
		})

	// Here we generate a final delivery address in bob's wallet but set by
	// alice. We do this to ensure that the address is not in alice's LND
	// wallet. We already correctly track settled balances when the address
	// is in the LND wallet.
	default:
		res := bob.RPC.NewAddress(&lnrpc.NewAddressRequest{
			Type: deliveryAddressType,
		})
		addr = res.Address
	}

	// Prepare for channel open.
	openParams := lntest.OpenChannelParams{
		Amt: btcutil.Amount(1000000),
	}

	// If we are testing the case where we set it on open then we'll set the
	// upfront shutdown script in the channel open parameters.
	if upfrontShutdown {
		openParams.CloseAddress = addr
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
		closeParams.DeliveryAddress = addr
	}

	// Close the channel!
	closeClient := alice.RPC.CloseChannel(&closeParams)

	// Assert that we got a channel update when we get a closing txid.
	_, err = closeClient.Recv()
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
	err = wait.NoError(func() error {
		closed := alice.RPC.ClosedChannels(&lnrpc.ClosedChannelsRequest{
			Cooperative: true,
		})

		if len(closed.Channels) == 0 {
			return fmt.Errorf("expected closed channel not found")
		}

		if closed.Channels[0].SettledBalance == 0 {
			return fmt.Errorf("expected settled balance to be zero")
		}

		return nil
	}, defaultTimeout)
	require.NoError(ht, err, "timeout checking closed channels")
}

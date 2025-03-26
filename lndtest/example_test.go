package lndtest_test

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcdtest"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lndtest"
	"github.com/lightningnetwork/lnd/lnrpc"
)

func ExampleHarness() {
	// Create a new btcd harness.
	btcd := btcdtest.New()

	// Stop btcd on exit.
	defer btcd.Stop()

	// Generate 100 blocks.
	_, err := btcd.Generate(100)
	if err != nil {
		panic(err)
	}

	// Create a new lnd harness.
	lnd := lndtest.New(lndtest.WithBtcd(btcd))

	// Stop lnd on exit.
	defer lnd.Stop()

	// Create a `lnrpc` client.
	ln := lnrpc.NewLightningClient(lnd)

	// Create an address.
	res, err := ln.NewAddress(context.TODO(), &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	if err != nil {
		panic(err)
	}

	// Decode the address.
	addr, err := btcutil.DecodeAddress(res.Address, nil)
	if err != nil {
		panic(err)
	}

	// Generate 1 block to the address.
	_, err = btcd.GenerateToAddress(1, addr, nil)
	if err != nil {
		panic(err)
	}

	// Generate 100 blocks to unlock the coinbase.
	_, err = btcd.Generate(100)
	if err != nil {
		panic(err)
	}

	// Sync LND.
	lnd.Sync(context.TODO())

	// Query the address balance.
	balRes, err := ln.WalletBalance(context.TODO(), &lnrpc.WalletBalanceRequest{})
	if err != nil {
		panic(err)
	}

	fmt.Println(balRes.ConfirmedBalance)
	// Output: 5000000000
}

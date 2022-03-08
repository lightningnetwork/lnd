package itest

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testTaproot ensures that the daemon can send to and spend from taproot (p2tr)
// outputs.
func testTaproot(net *lntest.NetworkHarness, t *harnessTest) {
	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// We'll start the test by sending Alice some coins, which she'll use to
	// send to herself on a p2tr output.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, net.Alice)

	// Let's create a p2tr address now.
	p2trResp, err := net.Alice.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	require.NoError(t.t, err)

	// Assert this is a segwit v1 address that starts with bcrt1p.
	require.Contains(
		t.t, p2trResp.Address, net.Miner.ActiveNet.Bech32HRPSegwit+"1p",
	)

	// Send the coins from Alice's wallet to her own, but to the new p2tr
	// address.
	_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:   p2trResp.Address,
		Amount: 0.5 * btcutil.SatoshiPerBitcoin,
	})
	require.NoError(t.t, err)

	txid, err := waitForTxInMempool(net.Miner.Client, defaultTimeout)
	require.NoError(t.t, err)

	// Wait until bob has seen the tx and considers it as owned.
	p2trOutputIndex := getOutputIndex(t, net, txid, p2trResp.Address)
	op := &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	assertWalletUnspent(t, net.Alice, op)

	// Mine a block to clean up the mempool.
	mineBlocks(t, net, 1, 1)

	// Let's sweep the whole wallet to a new p2tr address, making sure we
	// can sign transactions with v0 and v1 inputs.
	p2trResp, err = net.Alice.NewAddress(ctxt, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
	})
	require.NoError(t.t, err)

	_, err = net.Alice.SendCoins(ctxt, &lnrpc.SendCoinsRequest{
		Addr:    p2trResp.Address,
		SendAll: true,
	})
	require.NoError(t.t, err)

	// Wait until the wallet cleaning sweep tx is found.
	txid, err = waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	// Wait until bob has seen the tx and considers it as owned.
	p2trOutputIndex = getOutputIndex(t, net, txid, p2trResp.Address)
	op = &lnrpc.OutPoint{
		TxidBytes:   txid[:],
		OutputIndex: uint32(p2trOutputIndex),
	}
	assertWalletUnspent(t, net.Alice, op)

	// Mine another block to clean up the mempool.
	mineBlocks(t, net, 1, 1)
}

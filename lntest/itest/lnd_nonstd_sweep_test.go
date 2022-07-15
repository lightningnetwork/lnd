package itest

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

func testNonstdSweep(net *lntest.NetworkHarness, t *harnessTest) {
	p2shAddr, err := btcutil.NewAddressScriptHash(
		make([]byte, 1), harnessNetParams,
	)
	require.NoError(t.t, err)

	p2pkhAddr, err := btcutil.NewAddressPubKeyHash(
		make([]byte, 20), harnessNetParams,
	)
	require.NoError(t.t, err)

	p2wshAddr, err := btcutil.NewAddressWitnessScriptHash(
		make([]byte, 32), harnessNetParams,
	)
	require.NoError(t.t, err)

	p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), harnessNetParams,
	)
	require.NoError(t.t, err)

	p2trAddr, err := btcutil.NewAddressTaproot(
		make([]byte, 32), harnessNetParams,
	)
	require.NoError(t.t, err)

	tests := []struct {
		name    string
		address string
	}{
		{
			name:    "p2sh SendCoins standardness",
			address: p2shAddr.EncodeAddress(),
		},
		{
			name:    "p2pkh SendCoins standardness",
			address: p2pkhAddr.EncodeAddress(),
		},
		{
			name:    "p2wsh SendCoins standardness",
			address: p2wshAddr.EncodeAddress(),
		},
		{
			name:    "p2wkh SendCoins standardness",
			address: p2wkhAddr.EncodeAddress(),
		},
		{
			name:    "p2tr SendCoins standardness",
			address: p2trAddr.EncodeAddress(),
		},
	}

	for _, test := range tests {
		test := test
		success := t.t.Run(test.name, func(t *testing.T) {
			h := newHarnessTest(t, net)

			testNonStdSweepInner(net, h, test.address)
		})
		if !success {
			break
		}
	}
}

func testNonStdSweepInner(net *lntest.NetworkHarness, t *harnessTest,
	address string) {

	ctxb := context.Background()

	carol := net.NewNode(t.t, "carol", nil)

	// Give Carol a UTXO so SendCoins will behave as expected.
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)

	// Set the fee estimate to 1sat/vbyte.
	net.SetFeeEstimate(250)

	// Make Carol call SendCoins with the SendAll flag and the created
	// address.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:        address,
		SatPerVbyte: 1,
		SendAll:     true,
	}

	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// If a non-standard transaction was created, then this SendCoins call
	// will fail.
	_, err := carol.SendCoins(ctxt, sendReq)
	require.NoError(t.t, err)

	// Fetch the txid so we can grab the raw transaction.
	txid, err := waitForTxInMempool(net.Miner.Client, minerMempoolTimeout)
	require.NoError(t.t, err)

	tx, err := net.Miner.Client.GetRawTransaction(txid)
	require.NoError(t.t, err)

	msgTx := tx.MsgTx()

	// Fetch the fee of the transaction.
	var (
		inputVal  int
		outputVal int
		fee       int
	)

	for _, inp := range msgTx.TxIn {
		// Fetch the previous outpoint's value.
		prevOut := inp.PreviousOutPoint

		ptx, err := net.Miner.Client.GetRawTransaction(&prevOut.Hash)
		require.NoError(t.t, err)

		pout := ptx.MsgTx().TxOut[prevOut.Index]
		inputVal += int(pout.Value)
	}

	for _, outp := range msgTx.TxOut {
		outputVal += int(outp.Value)
	}

	fee = inputVal - outputVal

	// Fetch the vsize of the transaction so we can determine if the
	// transaction pays >= 1 sat/vbyte.
	rawTx, err := net.Miner.Client.GetRawTransactionVerbose(txid)
	require.NoError(t.t, err)

	// Require fee >= vbytes.
	require.True(t.t, fee >= int(rawTx.Vsize))
}

package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

func testNonstdSweep(ht *lntest.HarnessTest) {
	p2shAddr, err := btcutil.NewAddressScriptHash(
		make([]byte, 1), harnessNetParams,
	)
	require.NoError(ht, err)

	p2pkhAddr, err := btcutil.NewAddressPubKeyHash(
		make([]byte, 20), harnessNetParams,
	)
	require.NoError(ht, err)

	p2wshAddr, err := btcutil.NewAddressWitnessScriptHash(
		make([]byte, 32), harnessNetParams,
	)
	require.NoError(ht, err)

	p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), harnessNetParams,
	)
	require.NoError(ht, err)

	p2trAddr, err := btcutil.NewAddressTaproot(
		make([]byte, 32), harnessNetParams,
	)
	require.NoError(ht, err)

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
		success := ht.Run(test.name, func(t *testing.T) {
			st := ht.Subtest(t)

			testNonStdSweepInner(st, test.address)
		})
		if !success {
			break
		}
	}
}

func testNonStdSweepInner(ht *lntest.HarnessTest, address string) {
	carol := ht.NewNode("carol", nil)

	// Give Carol a UTXO so SendCoins will behave as expected.
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	// Set the fee estimate to 1sat/vbyte.
	ht.SetFeeEstimate(250)

	// Make Carol call SendCoins with the SendAll flag and the created
	// address.
	sendReq := &lnrpc.SendCoinsRequest{
		Addr:        address,
		SatPerVbyte: 1,
		SendAll:     true,
	}

	// If a non-standard transaction was created, then this SendCoins call
	// will fail.
	carol.RPC.SendCoins(sendReq)

	// Fetch the txid so we can grab the raw transaction.
	txid := ht.AssertNumTxsInMempool(1)[0]
	tx := ht.GetRawTransaction(txid)

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
		ptx := ht.GetRawTransaction(prevOut.Hash)

		pout := ptx.MsgTx().TxOut[prevOut.Index]
		inputVal += int(pout.Value)
	}

	for _, outp := range msgTx.TxOut {
		outputVal += int(outp.Value)
	}

	fee = inputVal - outputVal

	// Fetch the vsize of the transaction so we can determine if the
	// transaction pays >= 1 sat/vbyte.
	rawTx := ht.Miner().GetRawTransactionVerbose(txid)

	// Require fee >= vbytes.
	require.True(ht, fee >= int(rawTx.Vsize))

	// Mine a block to keep the mempool clean.
	ht.MineBlocksAndAssertNumTxes(1, 1)
}

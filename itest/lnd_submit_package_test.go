package itest

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testSubmitPackage tests that the WalletKit.SubmitPackage RPC relays a v3
// (TRUC) transaction package: a zero-fee parent that would be rejected by a
// standalone broadcast (below the minimum relay fee) is accepted together with
// a fee-paying CPFP child whose combined package feerate clears policy.
//
// This requires a bitcoind chain backend, as btcd has no submitpackage RPC and
// cannot relay zero-fee v3 transactions; run with backend=bitcoind. The
// zero-fee parent can only enter the mempool via package evaluation (a
// standalone submission is rejected for the min relay fee), so a successful
// SubmitPackage proves the CPFP package path worked end to end.
func testSubmitPackage(ht *lntest.HarnessTest) {
	// submitpackage is a bitcoind RPC: btcd has no equivalent and neutrino
	// has no mempool, so this test only applies to the bitcoind backend.
	if ht.ChainBackendName() != "bitcoind" {
		ht.Skipf("submitpackage requires the bitcoind backend, got %v",
			ht.ChainBackendName())
	}

	alice := ht.NewNodeWithCoins("Alice", nil)

	const (
		fundAmt = int64(btcutil.SatoshiPerBitcoin)

		// childFee is paid by the child for the whole package. It
		// must cover both transactions' weight at >= the min relay
		// fee; a few thousand sats is comfortably above that.
		childFee = int64(20_000)

		// p2wkhKeyFamily is a custom key family so the derived keys
		// (and thus the addresses we control via SignOutputRaw) are
		// independent of the node's normal key usage.
		p2wkhKeyFamily = 44
	)

	// p2wkhKey derives a fresh key and returns it together with the p2wkh
	// address/pkScript it controls, which we can later spend via the
	// SignOutputRaw RPC.
	p2wkhKey := func() (*signrpc.KeyDescriptor, *btcec.PublicKey,
		btcutil.Address, []byte) {

		keyDesc := alice.RPC.DeriveNextKey(&walletrpc.KeyReq{
			KeyFamily: p2wkhKeyFamily,
		})

		pubKey, err := btcec.ParsePubKey(keyDesc.RawKeyBytes)
		require.NoError(ht, err)

		addr, err := btcutil.NewAddressWitnessPubKeyHash(
			btcutil.Hash160(pubKey.SerializeCompressed()),
			harnessNetParams,
		)
		require.NoError(ht, err)

		pkScript, err := txscript.PayToAddrScript(addr)
		require.NoError(ht, err)

		return keyDesc, pubKey, addr, pkScript
	}

	// signP2WKHInput signs input idx of tx (spending a p2wkh output
	// with the given pkScript and value) via SignOutputRaw and attaches
	// the witness.
	signP2WKHInput := func(tx *wire.MsgTx, idx int, pkScript []byte,
		value int64, keyDesc *signrpc.KeyDescriptor,
		pubKey *btcec.PublicKey) {

		var buf bytes.Buffer
		require.NoError(ht, tx.Serialize(&buf))

		signResp := alice.RPC.SignOutputRaw(&signrpc.SignReq{
			RawTxBytes: buf.Bytes(),
			SignDescs: []*signrpc.SignDescriptor{{
				Output: &signrpc.TxOut{
					PkScript: pkScript,
					Value:    value,
				},
				InputIndex:    int32(idx),
				KeyDesc:       keyDesc,
				Sighash:       uint32(txscript.SigHashAll),
				WitnessScript: pkScript,
			}},
		})

		tx.TxIn[idx].Witness = wire.TxWitness{
			append(signResp.RawSigs[0], byte(txscript.SigHashAll)),
			pubKey.SerializeCompressed(),
		}
	}

	serialize := func(tx *wire.MsgTx) []byte {
		var buf bytes.Buffer
		require.NoError(ht, tx.Serialize(&buf))

		return buf.Bytes()
	}

	// Fund a p2wkh output we control: send coins to a key-derived
	// address and confirm it, so the parent has a confirmed input to spend.
	parentInKey, parentInPub, parentInAddr, parentInScript := p2wkhKey()
	alice.RPC.SendCoins(&lnrpc.SendCoinsRequest{
		Addr:       parentInAddr.String(),
		Amount:     fundAmt,
		TargetConf: 6,
	})
	fundTxid := ht.AssertNumTxsInMempool(1)[0]
	fundOutIdx := ht.GetOutputIndex(fundTxid, parentInAddr.String())
	ht.MineBlocksAndAssertNumTxes(1, 1)

	// The child will spend the parent's output, so derive a key we control
	// for it and use its script as the parent's output.
	childInKey, childInPub, _, childInScript := p2wkhKey()

	// Build the zero-fee v3 parent: spend the confirmed input and pay the
	// full value to the child-input script, leaving no fee.
	parent := wire.NewMsgTx(3)
	parent.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  fundTxid,
			Index: uint32(fundOutIdx),
		},
	})
	parent.AddTxOut(wire.NewTxOut(fundAmt, childInScript))
	signP2WKHInput(
		parent, 0, parentInScript, fundAmt, parentInKey, parentInPub,
	)

	// Build the v3 CPFP child: spend the parent's unconfirmed output
	// and pay childFee, which covers the whole package.
	childOut := alice.RPC.NewAddress(&lnrpc.NewAddressRequest{
		Type: AddrTypeWitnessPubkeyHash,
	})
	childOutAddr, err := btcutil.DecodeAddress(
		childOut.Address, harnessNetParams,
	)
	require.NoError(ht, err)
	childOutScript, err := txscript.PayToAddrScript(childOutAddr)
	require.NoError(ht, err)

	child := wire.NewMsgTx(3)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  parent.TxHash(),
			Index: 0,
		},
	})
	child.AddTxOut(wire.NewTxOut(fundAmt-childFee, childOutScript))
	signP2WKHInput(child, 0, childInScript, fundAmt, childInKey, childInPub)

	// Submit the two transactions as a package. A max fee rate of 0
	// disables the fee-rate ceiling so a high-feerate CPFP child is
	// never rejected.
	noFeeLimit := float64(0)
	resp := alice.RPC.SubmitPackage(&walletrpc.SubmitPackageRequest{
		RawTxs:     [][]byte{serialize(parent), serialize(child)},
		MaxFeeRate: &noFeeLimit,
	})

	// The whole package must be accepted, with a per-tx result (keyed by
	// wtxid) for each transaction and no per-tx error.
	require.Equal(ht, "success", resp.PackageMsg)
	require.Len(ht, resp.TxResults, 2)
	for _, txResult := range resp.TxResults {
		require.Emptyf(
			ht, txResult.Error, "tx %s rejected", txResult.Txid,
		)
	}
}

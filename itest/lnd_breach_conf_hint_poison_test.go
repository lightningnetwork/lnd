//go:build integration

package itest

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testBreachConfHintPoisonedByFakeFunding ensures that a fake funding request
// cannot advance the confirmation hint for a revoked commitment with the same
// txid. The breach arbitrator must still find the breach and publish justice.
//
// Run this test with bitcoind without txindex or with neutrino to exercise the
// historical scan that consults the height hint.
func testBreachConfHintPoisonedByFakeFunding(ht *lntest.HarnessTest) {
	const (
		chanAmt     = btcutil.Amount(1_000_000)
		fakeChanAmt = btcutil.Amount(500_000)
		paymentAmt  = 100_000
		numInvoices = 4

		// Production lnd waits at least 3 confirmations before acting
		// on a channel close; integration builds default to 1.
		closeConfs = 3

		// Any output index works, the txid is what matters.
		fakeOutputIndex = 50
	)

	commitType := lnrpc.CommitmentType_ANCHORS

	// Alice is the victim. As in the other breach tests, --nolisten and
	// --minbackoff keep Mallory from reconnecting after she restores her
	// old state, so she can broadcast it instead of triggering DLP.
	aliceArgs := append(
		lntest.NodeArgsForCommitType(commitType),
		"--nolisten", "--minbackoff=1h",
		fmt.Sprintf("--dev.force-channel-close-confs=%d", closeConfs),
	)
	alice := ht.NewNode("Alice", aliceArgs)

	// Mallory holds a real channel with Alice and will breach it.
	mallory := ht.NewNode(
		"Mallory", lntest.NodeArgsForCommitType(commitType),
	)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, mallory)
	ht.ConnectNodes(alice, mallory)

	chanPoint := ht.OpenChannel(
		mallory, alice, lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: commitType,
		},
	)

	// Mallory pays Alice twice and keeps a copy of that state, then pays
	// twice more. The copy becomes a revoked state in which Mallory owns
	// more of the channel.
	alicePayReqs, _, _ := ht.CreatePayReqs(alice, paymentAmt, numInvoices)
	ht.CompletePaymentRequests(mallory, alicePayReqs[:numInvoices/2])
	ht.AssertChannelLocalBalance(
		alice, chanPoint, paymentAmt*numInvoices/2,
	)
	revokedStateNum := ht.QueryChannelByChanPoint(
		mallory, chanPoint,
	).NumUpdates

	ht.BackupDB(mallory)
	ht.EnsureConnected(alice, mallory)
	ht.AssertChannelExists(alice, chanPoint)

	ht.CompletePaymentRequests(mallory, alicePayReqs[numInvoices/2:])
	ht.AssertChannelLocalBalance(alice, chanPoint, paymentAmt*numInvoices)

	ht.RestartNodeAndRestoreDB(mallory)
	ht.AssertChannelCommitHeight(mallory, chanPoint, int(revokedStateNum))

	// Mallory broadcasts the revoked commitment. Mining waits until the
	// poison request is in place.
	_, closeUpd := ht.CloseChannelAssertPending(mallory, chanPoint, true)
	pendingClose := closeUpd.GetClosePending()
	require.NotNil(ht, pendingClose)
	breachTxid, err := chainhash.NewHash(pendingClose.Txid)
	require.NoError(ht, err)

	breachTx := ht.AssertTxInMempool(*breachTxid)
	toLocalOp, toRemoteOp := breachOutpoints(ht, breachTx, *breachTxid)

	// Mallory2, a second node of the attacker, opens an inbound channel
	// to Alice whose funding outpoint is (breach txid, 50). Stock lnd
	// sends any outpoint through a chan point shim, but signs with the
	// shim's remote key, so the test reads Alice's next multisig key over
	// her RPC. A modified peer would take it from accept_channel instead.
	mallory2 := ht.NewNode(
		"Mallory2", lntest.NodeArgsForCommitType(commitType),
	)
	ht.ConnectNodes(alice, mallory2)

	multiSigFamily := int32(keychain.KeyFamilyMultiSig)
	lastAliceKey := alice.RPC.DeriveNextKey(&walletrpc.KeyReq{
		KeyFamily: multiSigFamily,
	})
	aliceFundingKey := alice.RPC.DeriveKey(&signrpc.KeyLocator{
		KeyFamily: multiSigFamily,
		KeyIndex:  lastAliceKey.KeyLoc.KeyIndex + 1,
	})
	mallory2FundingKey := mallory2.RPC.DeriveKey(&signrpc.KeyLocator{
		KeyFamily: 9999,
	})

	fakeChanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: breachTxid[:],
		},
		OutputIndex: fakeOutputIndex,
	}
	mallory2LocalKey := &lnrpc.KeyDescriptor{
		RawKeyBytes: mallory2FundingKey.RawKeyBytes,
		KeyLoc: &lnrpc.KeyLocator{
			KeyFamily: mallory2FundingKey.KeyLoc.KeyFamily,
			KeyIndex:  mallory2FundingKey.KeyLoc.KeyIndex,
		},
	}
	fakeShim := &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_ChanPointShim{
			ChanPointShim: &lnrpc.ChanPointShim{
				Amt:           int64(fakeChanAmt),
				ChanPoint:     fakeChanPoint,
				LocalKey:      mallory2LocalKey,
				RemoteKey:     aliceFundingKey.RawKeyBytes,
				PendingChanId: ht.Random32Bytes(),
			},
		},
	}

	// A static remote key channel spares Mallory2 the anchor wallet
	// reserve, so she needs no coins at all.
	ht.OpenChannelAssertPending(
		mallory2, alice, lntest.OpenChannelParams{
			Amt:            fakeChanAmt,
			FundingShim:    fakeShim,
			CommitmentType: lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		},
	)
	ht.AssertNumPendingOpenChannels(alice, 1)

	// The revoked commitment confirms. Alice's chain
	// watcher registers its own request in this block, so it still finds
	// the breach.
	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, *breachTxid)

	// After closeConfs confirmations the chain watcher hands the breach
	// to the breach arbitrator. The fake funding request remains pending.
	ht.MineBlocks(closeConfs - 1)
	ht.AssertNumPendingOpenChannels(alice, 1)

	// Justice must spend the revoked to_local before Mallory's CSV delay
	// and include Alice's to_remote output in the same transaction.
	justiceTx := ht.AssertOutpointInMempool(toLocalOp)
	var sweepsRemote bool
	for _, txIn := range justiceTx.TxIn {
		if txIn.PreviousOutPoint == toRemoteOp {
			sweepsRemote = true
		}
	}
	require.True(ht, sweepsRemote)

	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, justiceTx.TxHash())
}

// breachOutpoints returns Mallory's to_local and Alice's to_remote outpoints
// on the breach transaction: the largest and second largest outputs, since
// Mallory owns most of the revoked state and anchors are tiny.
func breachOutpoints(ht *lntest.HarnessTest, tx *wire.MsgTx,
	txid chainhash.Hash) (wire.OutPoint, wire.OutPoint) {

	require.GreaterOrEqual(ht, len(tx.TxOut), 2)

	var first, second int
	if tx.TxOut[1].Value > tx.TxOut[0].Value {
		first, second = 1, 0
	}
	for i := 2; i < len(tx.TxOut); i++ {
		switch {
		case tx.TxOut[i].Value > tx.TxOut[first].Value:
			first, second = i, first
		case tx.TxOut[i].Value > tx.TxOut[second].Value:
			second = i
		}
	}

	return wire.OutPoint{Hash: txid, Index: uint32(first)},
		wire.OutPoint{Hash: txid, Index: uint32(second)}
}

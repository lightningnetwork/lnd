package itest

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntemp"
	"github.com/lightningnetwork/lnd/lntemp/node"
	"github.com/lightningnetwork/lnd/lntemp/rpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func testMultiHopHtlcClaims(ht *lntemp.HarnessTest) {
	type testCase struct {
		name string
		test func(ht *lntemp.HarnessTest, alice, bob *node.HarnessNode,
			c lnrpc.CommitmentType, zeroConf bool)
		commitType lnrpc.CommitmentType
	}

	subTests := []testCase{
		// {
		// 	// bob: outgoing our commit timeout
		// 	// carol: incoming their commit watch and see timeout
		// 	name: "local force close immediate expiry",
		// 	test: testMultiHopHtlcLocalTimeout,
		// },
		// {
		// 	// bob: outgoing watch and see, they sweep on chain
		// 	// carol: incoming our commit, know preimage
		// 	name: "receiver chain claim",
		// 	test: testMultiHopReceiverChainClaim,
		// },
		// {
		// 	// bob: outgoing our commit watch and see timeout
		// 	// carol: incoming their commit watch and see timeout
		// 	name: "local force close on-chain htlc timeout",
		// 	test: testMultiHopLocalForceCloseOnChainHtlcTimeout,
		// },
		// {
		// 	// bob: outgoing their commit watch and see timeout
		// 	// carol: incoming our commit watch and see timeout
		// 	name: "remote force close on-chain htlc timeout",
		// 	test: testMultiHopRemoteForceCloseOnChainHtlcTimeout,
		// },
		// {
		// 	// bob: outgoing our commit watch and see, they sweep
		// 	// on chain
		// 	// bob: incoming our commit watch and learn preimage
		// 	// carol: incoming their commit know preimage
		// 	name: "local chain claim",
		// 	test: testMultiHopHtlcLocalChainClaim,
		// },
		// {
		// 	// bob: outgoing their commit watch and see, they sweep
		// 	// on chain
		// 	// bob: incoming their commit watch and learn preimage
		// 	// carol: incoming our commit know preimage
		// 	name: "remote chain claim",
		// 	test: testMultiHopHtlcRemoteChainClaim,
		// },
		// {
		// 	// bob: outgoing and incoming, sweep all on chain
		// 	name: "local htlc aggregation",
		// 	test: testMultiHopHtlcAggregation,
		// },
	}

	commitWithZeroConf := []struct {
		commitType lnrpc.CommitmentType
		zeroConf   bool
	}{
		{
			commitType: lnrpc.CommitmentType_LEGACY,
			zeroConf:   false,
		},
		{
			commitType: lnrpc.CommitmentType_ANCHORS,
			zeroConf:   false,
		},
		{
			commitType: lnrpc.CommitmentType_ANCHORS,
			zeroConf:   true,
		},
		{
			commitType: lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
			zeroConf:   false,
		},
		{
			commitType: lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
			zeroConf:   true,
		},
	}

	runTestCase := func(st *lntemp.HarnessTest, name string, tc testCase,
		zeroConf bool) {

		// Create the nodes here so that separate logs will be created
		// for Alice and Bob.
		args := nodeArgsForCommitType(tc.commitType)
		if zeroConf {
			args = append(
				args, "--protocol.option-scid-alias",
				"--protocol.zero-conf",
			)
		}

		alice := st.NewNode("Alice", args)
		bob := st.NewNode("Bob", args)
		st.ConnectNodes(alice, bob)

		// Start each test with the default static fee estimate.
		st.SetFeeEstimate(12500)

		// Add test name to the logs.
		alice.AddToLogf("Running test case: %s", name)
		bob.AddToLogf("Running test case: %s", name)

		tc.test(st, alice, bob, tc.commitType, zeroConf)
	}

	for _, subTest := range subTests {
		subTest := subTest

		for _, typeAndConf := range commitWithZeroConf {
			typeAndConf := typeAndConf

			subTest.commitType = typeAndConf.commitType

			name := fmt.Sprintf("%s/zeroconf=%v/committype=%v",
				subTest.name, typeAndConf.zeroConf,
				typeAndConf.commitType.String())

			s := ht.Run(name, func(t1 *testing.T) {
				st, cleanup := ht.Subtest(t1)
				defer cleanup()

				runTestCase(
					st, name, subTest, typeAndConf.zeroConf,
				)
			})
			if !s {
				return
			}
		}
	}
}

// waitForInvoiceAccepted waits until the specified invoice moved to the
// accepted state by the node.
func waitForInvoiceAccepted(t *harnessTest, node *lntest.HarnessNode,
	payHash lntypes.Hash) {

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	invoiceUpdates, err := node.SubscribeSingleInvoice(ctx,
		&invoicesrpc.SubscribeSingleInvoiceRequest{
			RHash: payHash[:],
		},
	)
	if err != nil {
		t.Fatalf("subscribe single invoice: %v", err)
	}

	for {
		update, err := invoiceUpdates.Recv()
		if err != nil {
			t.Fatalf("invoice update err: %v", err)
		}
		if update.State == lnrpc.Invoice_ACCEPTED {
			break
		}
	}
}

// checkPaymentStatus asserts that the given node list a payment with the given
// preimage has the expected status.
func checkPaymentStatus(node *lntest.HarnessNode, preimage lntypes.Preimage,
	status lnrpc.Payment_PaymentStatus) error {

	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: true,
	}
	paymentsResp, err := node.ListPayments(ctxt, req)
	if err != nil {
		return fmt.Errorf("error when obtaining Alice payments: %v",
			err)
	}

	payHash := preimage.Hash()
	var found bool
	for _, p := range paymentsResp.Payments {
		if p.PaymentHash != payHash.String() {
			continue
		}

		found = true
		if p.Status != status {
			return fmt.Errorf("expected payment status "+
				"%v, got %v", status, p.Status)
		}

		switch status {
		// If this expected status is SUCCEEDED, we expect the final preimage.
		case lnrpc.Payment_SUCCEEDED:
			if p.PaymentPreimage != preimage.String() {
				return fmt.Errorf("preimage doesn't match: %v vs %v",
					p.PaymentPreimage, preimage.String())
			}

		// Otherwise we expect an all-zero preimage.
		default:
			if p.PaymentPreimage != (lntypes.Preimage{}).String() {
				return fmt.Errorf("expected zero preimage, got %v",
					p.PaymentPreimage)
			}
		}
	}

	if !found {
		return fmt.Errorf("payment with payment hash %v not found "+
			"in response", payHash)
	}

	return nil
}

// TODO(yy): delete.
func createThreeHopNetworkOld(t *harnessTest, net *lntest.NetworkHarness,
	alice, bob *lntest.HarnessNode, carolHodl bool, c lnrpc.CommitmentType,
	zeroConf bool) (
	*lnrpc.ChannelPoint, *lnrpc.ChannelPoint, *lntest.HarnessNode) {

	net.EnsureConnected(t.t, alice, bob)

	// Make sure there are enough utxos for anchoring.
	for i := 0; i < 2; i++ {
		net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, alice)
		net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, bob)
	}

	// We'll start the test by creating a channel between Alice and Bob,
	// which will act as the first leg for out multi-hop HTLC.
	const chanAmt = 1000000
	var aliceFundingShim *lnrpc.FundingShim
	var thawHeight uint32
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		_, minerHeight, err := net.Miner.Client.GetBestBlock()
		require.NoError(t.t, err)
		thawHeight = uint32(minerHeight + 144)
		aliceFundingShim, _, _ = deriveFundingShimOld(
			net, t, alice, bob, chanAmt, thawHeight, true,
		)
	}

	// If a zero-conf channel is being opened, the nodes are signalling the
	// zero-conf feature bit. Setup a ChannelAcceptor for the fundee.
	ctxb := context.Background()

	var (
		cancel context.CancelFunc
		ctxc   context.Context
	)

	if zeroConf {
		ctxc, cancel = context.WithCancel(ctxb)
		acceptStream, err := bob.ChannelAcceptor(ctxc)
		require.NoError(t.t, err)
		go acceptChannel(t.t, true, acceptStream)
	}

	aliceChanPoint := openChannelAndAssert(
		t, net, alice, bob,
		lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			FundingShim:    aliceFundingShim,
			ZeroConf:       zeroConf,
		},
	)

	// Remove the ChannelAcceptor for Bob.
	if zeroConf {
		cancel()
	}

	err := alice.WaitForNetworkChannelOpen(aliceChanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}

	err = bob.WaitForNetworkChannelOpen(aliceChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	// Next, we'll create a new node "carol" and have Bob connect to her. If
	// the carolHodl flag is set, we'll make carol always hold onto the
	// HTLC, this way it'll force Bob to go to chain to resolve the HTLC.
	carolFlags := nodeArgsForCommitType(c)
	if carolHodl {
		carolFlags = append(carolFlags, "--hodl.exit-settle")
	}

	if zeroConf {
		carolFlags = append(
			carolFlags, "--protocol.option-scid-alias",
			"--protocol.zero-conf",
		)
	}

	carol := net.NewNode(t.t, "Carol", carolFlags)

	net.ConnectNodes(t.t, bob, carol)

	// Make sure Carol has enough utxos for anchoring. Because the anchor by
	// itself often doesn't meet the dust limit, a utxo from the wallet
	// needs to be attached as an additional input. This can still lead to a
	// positively-yielding transaction.
	for i := 0; i < 2; i++ {
		net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, carol)
	}

	// We'll then create a channel from Bob to Carol. After this channel is
	// open, our topology looks like:  A -> B -> C.
	var bobFundingShim *lnrpc.FundingShim
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		bobFundingShim, _, _ = deriveFundingShimOld(
			net, t, bob, carol, chanAmt, thawHeight, true,
		)
	}

	// Setup a ChannelAcceptor for Carol if a zero-conf channel open is
	// being attempted.
	if zeroConf {
		ctxc, cancel = context.WithCancel(ctxb)
		acceptStream, err := carol.ChannelAcceptor(ctxc)
		require.NoError(t.t, err)
		go acceptChannel(t.t, true, acceptStream)
	}

	bobChanPoint := openChannelAndAssert(
		t, net, bob, carol,
		lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			FundingShim:    bobFundingShim,
			ZeroConf:       zeroConf,
		},
	)

	// Remove the ChannelAcceptor for Carol.
	if zeroConf {
		cancel()
	}

	err = bob.WaitForNetworkChannelOpen(bobChanPoint)
	if err != nil {
		t.Fatalf("alice didn't report channel: %v", err)
	}
	err = carol.WaitForNetworkChannelOpen(bobChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}
	err = alice.WaitForNetworkChannelOpen(bobChanPoint)
	if err != nil {
		t.Fatalf("bob didn't report channel: %v", err)
	}

	return aliceChanPoint, bobChanPoint, carol
}

// assertAllTxesSpendFrom asserts that all txes in the list spend from the given
// tx.
func assertAllTxesSpendFrom(t *harnessTest, txes []*wire.MsgTx,
	prevTxid chainhash.Hash) {

	for _, tx := range txes {
		if tx.TxIn[0].PreviousOutPoint.Hash != prevTxid {
			t.Fatalf("tx %v did not spend from %v",
				tx.TxHash(), prevTxid)
		}
	}
}

// createThreeHopNetwork creates a topology of `Alice -> Bob -> Carol`.
func createThreeHopNetwork(ht *lntemp.HarnessTest,
	alice, bob *node.HarnessNode, carolHodl bool, c lnrpc.CommitmentType,
	zeroConf bool) (*lnrpc.ChannelPoint,
	*lnrpc.ChannelPoint, *node.HarnessNode) {

	ht.EnsureConnected(alice, bob)

	// We'll create a new node "carol" and have Bob connect to her.
	// If the carolHodl flag is set, we'll make carol always hold onto the
	// HTLC, this way it'll force Bob to go to chain to resolve the HTLC.
	carolFlags := nodeArgsForCommitType(c)
	if carolHodl {
		carolFlags = append(carolFlags, "--hodl.exit-settle")
	}

	if zeroConf {
		carolFlags = append(
			carolFlags, "--protocol.option-scid-alias",
			"--protocol.zero-conf",
		)
	}
	carol := ht.NewNode("Carol", carolFlags)

	ht.ConnectNodes(bob, carol)

	// Make sure there are enough utxos for anchoring. Because the anchor
	// by itself often doesn't meet the dust limit, a utxo from the wallet
	// needs to be attached as an additional input. This can still lead to
	// a positively-yielding transaction.
	for i := 0; i < 2; i++ {
		ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)
		ht.FundCoins(btcutil.SatoshiPerBitcoin, bob)
		ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)
	}

	// We'll start the test by creating a channel between Alice and Bob,
	// which will act as the first leg for out multi-hop HTLC.
	const chanAmt = 1000000
	var aliceFundingShim *lnrpc.FundingShim
	var thawHeight uint32
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		_, minerHeight := ht.Miner.GetBestBlock()
		thawHeight = uint32(minerHeight + 144)
		aliceFundingShim, _, _ = deriveFundingShim(
			ht, alice, bob, chanAmt, thawHeight, true,
		)
	}

	var (
		cancel       context.CancelFunc
		acceptStream rpc.AcceptorClient
	)
	// If a zero-conf channel is being opened, the nodes are signalling the
	// zero-conf feature bit. Setup a ChannelAcceptor for the fundee.
	if zeroConf {
		acceptStream, cancel = bob.RPC.ChannelAcceptor()
		go acceptChannel(ht.T, true, acceptStream)
	}

	aliceParams := lntemp.OpenChannelParams{
		Amt:            chanAmt,
		CommitmentType: c,
		FundingShim:    aliceFundingShim,
		ZeroConf:       zeroConf,
	}
	aliceChanPoint := ht.OpenChannel(alice, bob, aliceParams)

	// Remove the ChannelAcceptor for Bob.
	if zeroConf {
		cancel()
	}

	// We'll then create a channel from Bob to Carol. After this channel is
	// open, our topology looks like:  A -> B -> C.
	var bobFundingShim *lnrpc.FundingShim
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		bobFundingShim, _, _ = deriveFundingShim(
			ht, bob, carol, chanAmt, thawHeight, true,
		)
	}

	// Setup a ChannelAcceptor for Carol if a zero-conf channel open is
	// being attempted.
	if zeroConf {
		acceptStream, cancel = carol.RPC.ChannelAcceptor()
		go acceptChannel(ht.T, true, acceptStream)
	}

	bobParams := lntemp.OpenChannelParams{
		Amt:            chanAmt,
		CommitmentType: c,
		FundingShim:    bobFundingShim,
		ZeroConf:       zeroConf,
	}
	bobChanPoint := ht.OpenChannel(bob, carol, bobParams)

	// Remove the ChannelAcceptor for Carol.
	if zeroConf {
		cancel()
	}

	// Make sure alice and carol know each other's channels.
	ht.AssertTopologyChannelOpen(alice, bobChanPoint)
	ht.AssertTopologyChannelOpen(carol, aliceChanPoint)

	return aliceChanPoint, bobChanPoint, carol
}

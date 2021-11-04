package itest

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
)

func testMultiHopHtlcClaims(ht *lntest.HarnessTest) {
	type testCase struct {
		name string
		test func(ht *lntest.HarnessTest,
			alice, bob *lntest.HarnessNode, c lnrpc.CommitmentType,
		)
	}

	subTests := []testCase{
		{
			// bob: outgoing our commit timeout
			// carol: incoming their commit watch and see timeout
			name: "local force close immediate expiry",
			test: testMultiHopHtlcLocalTimeout,
		},
		{
			// bob: outgoing watch and see, they sweep on chain
			// carol: incoming our commit, know preimage
			name: "receiver chain claim",
			test: testMultiHopReceiverChainClaim,
		},
		{
			// bob: outgoing our commit watch and see timeout
			// carol: incoming their commit watch and see timeout
			name: "local force close on-chain htlc timeout",
			test: testMultiHopLocalForceCloseOnChainHtlcTimeout,
		},
		{
			// bob: outgoing their commit watch and see timeout
			// carol: incoming our commit watch and see timeout
			name: "remote force close on-chain htlc timeout",
			test: testMultiHopRemoteForceCloseOnChainHtlcTimeout,
		},
		{
			// bob: outgoing our commit watch and see, they sweep
			// on chain
			// bob: incoming our commit watch and learn preimage
			// carol: incoming their commit know preimage
			name: "local chain claim",
			test: testMultiHopHtlcLocalChainClaim,
		},
		{
			// bob: outgoing their commit watch and see, they sweep
			// on chain
			// bob: incoming their commit watch and learn preimage
			// carol: incoming our commit know preimage
			name: "remote chain claim",
			test: testMultiHopHtlcRemoteChainClaim,
		},
		{
			// bob: outgoing and incoming, sweep all on chain
			name: "local htlc aggregation",
			test: testMultiHopHtlcAggregation,
		},
	}

	commitTypes := []lnrpc.CommitmentType{
		lnrpc.CommitmentType_LEGACY,
		lnrpc.CommitmentType_ANCHORS,
		lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE,
	}

	for _, commitType := range commitTypes {
		commitType := commitType
		testName := fmt.Sprintf("committype=%v", commitType.String())

		success := ht.Run(testName, func(t *testing.T) {
			ht := ht.Subtest(t)

			for _, subTest := range subTests {
				subTest := subTest

				s := ht.Run(subTest.name, func(t *testing.T) {
					ht := ht.Subtest(t)

					alice, bob := ht.Alice, ht.Bob

					// Create the nodes here so that
					// separate logs will be created for
					// Alice and Bob.
					args := nodeArgsForCommitType(
						commitType,
					)
					ht.RestartNodeWithExtraArgs(
						alice, args,
					)
					ht.RestartNodeWithExtraArgs(
						bob, args,
					)

					alice.AddToLogf("%s/%s", testName,
						subTest.name)
					bob.AddToLogf("%s/%s", testName,
						subTest.name)

					ht.ConnectNodes(alice, bob)

					// Start each test with the default
					// static fee estimate.
					ht.SetFeeEstimate(12500)

					subTest.test(ht, alice, bob, commitType)
				})
				if !s {
					return
				}
			}
		})
		if !success {
			return
		}
	}
}

func createThreeHopNetwork(ht *lntest.HarnessTest,
	alice, bob *lntest.HarnessNode, carolHodl bool,
	c lnrpc.CommitmentType) (*lnrpc.ChannelPoint,
	*lnrpc.ChannelPoint, *lntest.HarnessNode) {

	ht.EnsureConnected(alice, bob)

	// Make sure there are enough utxos for anchoring.
	for i := 0; i < 2; i++ {
		ht.SendCoins(btcutil.SatoshiPerBitcoin, alice)
		ht.SendCoins(btcutil.SatoshiPerBitcoin, bob)
	}

	// We'll start the test by creating a channel between Alice and Bob,
	// which will act as the first leg for out multi-hop HTLC.
	const chanAmt = 1000000
	var aliceFundingShim *lnrpc.FundingShim
	var thawHeight uint32
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		_, minerHeight := ht.GetBestBlock()
		thawHeight = uint32(minerHeight + 144)
		aliceFundingShim, _, _ = deriveFundingShim(
			ht, alice, bob, chanAmt, thawHeight, true,
		)
	}
	aliceChanPoint := ht.OpenChannel(
		alice, bob, lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			FundingShim:    aliceFundingShim,
		},
	)

	// Next, we'll create a new node "carol" and have Bob connect to her.
	// If the carolHodl flag is set, we'll make carol always hold onto the
	// HTLC, this way it'll force Bob to go to chain to resolve the HTLC.
	carolFlags := nodeArgsForCommitType(c)
	if carolHodl {
		carolFlags = append(carolFlags, "--hodl.exit-settle")
	}
	carol := ht.NewNode("Carol", carolFlags)

	ht.ConnectNodes(bob, carol)

	// Make sure Carol has enough utxos for anchoring. Because the anchor by
	// itself often doesn't meet the dust limit, a utxo from the wallet
	// needs to be attached as an additional input. This can still lead to a
	// positively-yielding transaction.
	for i := 0; i < 2; i++ {
		ht.SendCoins(btcutil.SatoshiPerBitcoin, carol)
	}

	// We'll then create a channel from Bob to Carol. After this channel is
	// open, our topology looks like:  A -> B -> C.
	var bobFundingShim *lnrpc.FundingShim
	if c == lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE {
		bobFundingShim, _, _ = deriveFundingShim(
			ht, bob, carol, chanAmt, thawHeight, true,
		)
	}
	bobChanPoint := ht.OpenChannel(
		bob, carol, lntest.OpenChannelParams{
			Amt:            chanAmt,
			CommitmentType: c,
			FundingShim:    bobFundingShim,
		},
	)

	// Alice should notice this open channel too.
	ht.AssertChannelOpen(alice, bobChanPoint)

	return aliceChanPoint, bobChanPoint, carol
}

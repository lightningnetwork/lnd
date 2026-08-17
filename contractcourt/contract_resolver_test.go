package contractcourt

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/stretchr/testify/require"
)

// TestIsSecondLevelSigHashDefault asserts that the pre-signed-tx publish path
// can only ever activate for aux/custom (taproot asset) channels: without a
// tapscript root in the channel type, even sign details carrying the
// (zero-value) SigHashDefault flag must not match.
func TestIsSecondLevelSigHashDefault(t *testing.T) {
	t.Parallel()

	taprootChanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SingleFunderTweaklessBit

	customChanType := taprootChanType | channeldb.TapscriptRootBit

	sigHashDefaultDetails := &input.SignDetails{
		SigHashType: txscript.SigHashDefault,
	}
	standardDetails := &input.SignDetails{
		SigHashType: txscript.SigHashSingle |
			txscript.SigHashAnyOneCanPay,
	}

	testCases := []struct {
		name        string
		signDetails *input.SignDetails
		chanType    channeldb.ChannelType
		expect      bool
	}{{
		// No sign details at all (first-level only): never matches.
		name:        "nil sign details",
		signDetails: nil,
		chanType:    customChanType,
		expect:      false,
	}, {
		// The crux: SigHashDefault is the zero value of SigHashType,
		// so any non-custom channel that never populates the field
		// would false-positively match without the channel-type gate.
		name:        "sighash default, non-custom taproot",
		signDetails: sigHashDefaultDetails,
		chanType:    taprootChanType,
		expect:      false,
	}, {
		name:        "sighash default, custom channel",
		signDetails: sigHashDefaultDetails,
		chanType:    customChanType,
		expect:      true,
	}, {
		name:        "standard sighash, custom channel",
		signDetails: standardDetails,
		chanType:    customChanType,
		expect:      false,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expect, isSecondLevelSigHashDefault(
				tc.signDetails, tc.chanType,
			))
		})
	}
}

// TestPreSignedTxFee asserts that the exact baked-in fee of a pre-signed
// second-level tx is derived from the spent commitment output value minus
// the tx's own outputs.
func TestPreSignedTxFee(t *testing.T) {
	t.Parallel()

	tx := wire.NewMsgTx(2)
	tx.AddTxOut(&wire.TxOut{Value: 99_426})
	tx.AddTxOut(&wire.TxOut{Value: int64(lnwallet.AnchorSize)})

	signDetails := &input.SignDetails{
		SignDesc: input.SignDescriptor{
			Output: &wire.TxOut{Value: 100_000},
		},
	}

	require.Equal(
		t, btcutil.Amount(244), preSignedTxFee(tx, signDetails),
	)
}

// TestSecondLevelAnchorInput asserts the construction of the CPFP anchor
// input for a pre-signed second-level HTLC tx: the anchor outpoint at index
// 1, a key-path (SigHashDefault) sign descriptor for the tweaked delay key's
// anchor tree, the exact baked-in parent fee for package fee-rate math, and
// the budget derived from the protected HTLC value. A parent without an
// anchor or a descriptor without key material yields no input rather than
// an error.
func TestSecondLevelAnchorInput(t *testing.T) {
	t.Parallel()

	delayBase, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	commitPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	singleTweak := input.SingleTweakBytes(
		commitPriv.PubKey(), delayBase.PubKey(),
	)
	signDesc := input.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			PubKey: delayBase.PubKey(),
		},
		SingleTweak: singleTweak,
	}

	budgetCfg := BudgetConfig{
		AnchorCPFP:      10_000,
		AnchorCPFPRatio: 0.5,
	}
	const (
		htlcOutValue = int64(100_000)
		parentFee    = btcutil.Amount(244)
	)

	// A single-output parent (no anchor appended) yields no input.
	oneOutTx := wire.NewMsgTx(2)
	oneOutTx.AddTxOut(&wire.TxOut{Value: htlcOutValue})
	anchor, budget, err := secondLevelAnchorInput(
		oneOutTx, signDesc, parentFee, 100, budgetCfg, log,
	)
	require.NoError(t, err)
	require.Nil(t, anchor)
	require.Zero(t, budget)

	// A parent with an anchor but a descriptor lacking key material also
	// yields no input (logged, not an error): CPFP is unavailable but the
	// parent still publishes.
	twoOutTx := wire.NewMsgTx(2)
	twoOutTx.AddTxOut(&wire.TxOut{Value: htlcOutValue})
	twoOutTx.AddTxOut(&wire.TxOut{
		Value: int64(lnwallet.AnchorSize),
	})
	anchor, _, err = secondLevelAnchorInput(
		twoOutTx, input.SignDescriptor{}, parentFee, 100, budgetCfg,
		log,
	)
	require.NoError(t, err)
	require.Nil(t, anchor)

	// The well-formed case.
	anchor, budget, err = secondLevelAnchorInput(
		twoOutTx, signDesc, parentFee, 100, budgetCfg, log,
	)
	require.NoError(t, err)
	require.NotNil(t, anchor)

	require.Equal(t, wire.OutPoint{
		Hash:  twoOutTx.TxHash(),
		Index: 1,
	}, anchor.OutPoint())
	require.Equal(
		t, input.TaprootAnchorSweepSpend, anchor.WitnessType(),
	)
	require.EqualValues(t, 100, anchor.HeightHint())

	// The budget must be derived from the protected HTLC value via the
	// anchor CPFP configuration (50% of 100k, capped at 10k), plus the
	// anchor's own value.
	require.Equal(t, btcutil.Amount(10_000)+AnchorOutputValue, budget)

	// The parent info must carry the exact baked-in fee for package
	// fee-rate calculation.
	require.NotNil(t, anchor.UnconfParent())
	require.Equal(t, parentFee, anchor.UnconfParent().Fee)

	// The sign descriptor must target the anchor output with a key-path
	// (SigHashDefault) spend of the tweaked delay key's anchor tree.
	sd := anchor.SignDesc()
	require.Equal(t, twoOutTx.TxOut[1], sd.Output)
	require.Equal(t, txscript.SigHashDefault, sd.HashType)

	delayKey := input.TweakPubKeyWithTweak(
		delayBase.PubKey(), singleTweak,
	)
	anchorTree, err := input.NewAnchorScriptTree(delayKey)
	require.NoError(t, err)
	require.Equal(t, anchorTree.TapscriptRoot, sd.TapTweak)
}

// newPreSignedTestResolver builds a timeout resolver in the SigHashDefault
// state over the given mock sweeper, with a two-output pre-signed timeout tx
// (HTLC output + anchor).
func newPreSignedTestResolver(t *testing.T,
	sweeper *mockSweeper) (*htlcTimeoutResolver, *wire.MsgTx) {

	t.Helper()

	customChanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SingleFunderTweaklessBit |
		channeldb.TapscriptRootBit

	delayBase, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	commitPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	timeoutTx := wire.NewMsgTx(2)
	timeoutTx.LockTime = 500
	timeoutTx.AddTxOut(&wire.TxOut{Value: 630})
	timeoutTx.AddTxOut(&wire.TxOut{Value: int64(lnwallet.AnchorSize)})

	cfg := ResolverConfig{
		ChannelArbitratorConfig: ChannelArbitratorConfig{
			ChainArbitratorConfig: ChainArbitratorConfig{
				Sweeper: sweeper,
				Budget:  *DefaultBudgetConfig(),
			},
		},
	}

	resolver := &htlcTimeoutResolver{
		contractResolverKit: *newContractResolverKit(cfg),
		htlcResolution: lnwallet.OutgoingHtlcResolution{
			SignedTimeoutTx: timeoutTx,
			SignDetails: &input.SignDetails{
				SigHashType: txscript.SigHashDefault,
				SignDesc: input.SignDescriptor{
					Output: &wire.TxOut{Value: 1200},
				},
			},
			SweepSignDesc: input.SignDescriptor{
				KeyDesc: keychain.KeyDescriptor{
					PubKey: delayBase.PubKey(),
				},
				SingleTweak: input.SingleTweakBytes(
					commitPriv.PubKey(), delayBase.PubKey(),
				),
			},
		},
		chanType:                 customChanType,
		incomingHTLCExpiryHeight: fn.Some(int32(600)),
		broadcastHeight:          450,
	}
	resolver.initLogger("test")

	return resolver, timeoutTx
}

// TestHtlcTimeoutResolverPreSignedHandOff asserts that launching a
// SigHashDefault timeout resolver hands the exact pre-signed tx, its anchor
// input, the CPFP budget and the deadline to the sweeper in a single
// request, and that the resolver does not publish anything itself.
func TestHtlcTimeoutResolverPreSignedHandOff(t *testing.T) {
	t.Parallel()

	sweeper := newMockSweeper()
	resolver, timeoutTx := newPreSignedTestResolver(t, sweeper)

	require.NoError(t, resolver.Launch())
	require.True(t, resolver.isLaunched())

	req := <-sweeper.preSignedReqs
	require.Equal(t, timeoutTx.TxHash(), req.Tx.TxHash())
	require.NotNil(t, req.Anchor)
	require.Equal(t, wire.OutPoint{
		Hash:  timeoutTx.TxHash(),
		Index: 1,
	}, req.Anchor.OutPoint())
	require.Equal(t, fn.Some(int32(600)), req.DeadlineHeight)
	require.Equal(t, parentFeeOf(timeoutTx, 1200),
		req.Anchor.UnconfParent().Fee)
	require.Greater(t, req.Budget, AnchorOutputValue)

	// Nothing was handed to SweepInput directly: the sweeper owns the
	// anchor through the pre-signed lifecycle.
	require.Empty(t, sweeper.sweptInputs)

	resolver.Stop()
	resolver.wg.Wait()
}

// parentFeeOf returns the baked-in fee of a tx spending a single input of the
// given value.
func parentFeeOf(tx *wire.MsgTx, inputValue int64) btcutil.Amount {
	var out int64
	for _, txOut := range tx.TxOut {
		out += txOut.Value
	}

	return btcutil.Amount(inputValue - out)
}

// TestHtlcTimeoutResolverLaunchRetry asserts that a synchronous failure to
// hand the pre-signed tx to the sweeper does not leave the resolver
// permanently marked as launched: a later Launch call must retry and
// succeed.
func TestHtlcTimeoutResolverLaunchRetry(t *testing.T) {
	t.Parallel()

	sweeper := newMockSweeper()
	sweeper.preSignedErr = sweep.ErrSweeperShuttingDown
	resolver, timeoutTx := newPreSignedTestResolver(t, sweeper)

	// The first Launch hits the failing hand-off: it must error AND leave
	// the resolver un-launched so it can be retried.
	err := resolver.Launch()
	require.ErrorIs(t, err, sweep.ErrSweeperShuttingDown)
	require.False(t, resolver.isLaunched(),
		"failed launch must not leave the resolver marked launched")
	require.Empty(t, sweeper.preSignedReqs)

	// With the sweeper accepting again, the second Launch hands the tx
	// over.
	sweeper.preSignedErr = nil
	require.NoError(t, resolver.Launch())
	require.True(t, resolver.isLaunched())

	req := <-sweeper.preSignedReqs
	require.Equal(t, timeoutTx.TxHash(), req.Tx.TxHash())

	// A further Launch is a no-op: the lifecycle is owned by the sweeper
	// and must not be submitted twice by the resolver.
	require.NoError(t, resolver.Launch())
	require.Empty(t, sweeper.preSignedReqs)

	resolver.Stop()
	resolver.wg.Wait()
}

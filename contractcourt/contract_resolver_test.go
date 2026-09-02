package contractcourt

import (
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/mock"
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

// TestOfferSecondLevelAnchorToSweeper asserts the CPFP anchor hand-off to the
// sweeper: nothing is offered for anchor-less parent transactions or when the
// sweep descriptor lacks the delay key material, and for a well-formed parent
// the anchor outpoint at index 1 is swept with the caller's deadline, the
// exact parent fee, and a budget derived from the protected HTLC value via
// the anchor CPFP budget configuration.
func TestOfferSecondLevelAnchorToSweeper(t *testing.T) {
	t.Parallel()

	log := btclog.Disabled

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

	newReq := func(sweeper UtxoSweeper, parentTx *wire.MsgTx,
		desc input.SignDescriptor) *secondLevelAnchorSweepReq {

		return &secondLevelAnchorSweepReq{
			sweeper:         sweeper,
			parentTx:        parentTx,
			htlcSweepDesc:   desc,
			parentFee:       parentFee,
			budget:          budgetCfg,
			broadcastHeight: 100,
			deadlineHeight:  fn.Some(int32(200)),
			log:             log,
		}
	}

	// A single-output parent (no anchor appended) must be a no-op: no
	// error and no result channel to supervise.
	sweeper := newMockSweeper()
	oneOutTx := wire.NewMsgTx(2)
	oneOutTx.AddTxOut(&wire.TxOut{Value: htlcOutValue})
	resultChan, err := offerSecondLevelAnchorToSweeper(
		newReq(sweeper, oneOutTx, signDesc),
	)
	require.NoError(t, err)
	require.Nil(t, resultChan)
	require.Empty(t, sweeper.sweptInputs)

	// A parent with an anchor but a descriptor lacking key material must
	// also be a (logged) no-op rather than an error.
	twoOutTx := wire.NewMsgTx(2)
	twoOutTx.AddTxOut(&wire.TxOut{Value: htlcOutValue})
	twoOutTx.AddTxOut(&wire.TxOut{
		Value: int64(lnwallet.AnchorSize),
	})
	resultChan, err = offerSecondLevelAnchorToSweeper(
		newReq(sweeper, twoOutTx, input.SignDescriptor{}),
	)
	require.NoError(t, err)
	require.Nil(t, resultChan)
	require.Empty(t, sweeper.sweptInputs)

	// The well-formed case: the anchor outpoint at index 1 is offered
	// with the caller's deadline, and the sweeper's result channel is
	// handed back for supervision.
	resultChan, err = offerSecondLevelAnchorToSweeper(
		newReq(sweeper, twoOutTx, signDesc),
	)
	require.NoError(t, err)
	require.NotNil(t, resultChan)
	require.Len(t, sweeper.sweptInputs, 1)

	swept := <-sweeper.sweptInputs
	require.Equal(t, wire.OutPoint{
		Hash:  twoOutTx.TxHash(),
		Index: 1,
	}, swept.OutPoint())
	require.Equal(
		t, input.TaprootAnchorSweepSpend, swept.WitnessType(),
	)
	require.Equal(t, []int{200}, sweeper.deadlines)

	// The budget must be derived from the protected HTLC value via the
	// anchor CPFP configuration (50% of 100k, capped at 10k), plus the
	// anchor's own value.
	require.Equal(
		t, []btcutil.Amount{10_000 + AnchorOutputValue},
		sweeper.budgets,
	)

	// The parent info must carry the exact baked-in fee for package
	// fee-rate calculation.
	require.NotNil(t, swept.UnconfParent())
	require.Equal(t, parentFee, swept.UnconfParent().Fee)

	// The sign descriptor must target the anchor output with a key-path
	// (SigHashDefault) spend of the tweaked delay key's anchor tree.
	sd := swept.SignDesc()
	require.Equal(t, twoOutTx.TxOut[1], sd.Output)
	require.Equal(t, txscript.SigHashDefault, sd.HashType)

	delayKey := input.TweakPubKeyWithTweak(
		delayBase.PubKey(), singleTweak,
	)
	anchorTree, err := input.NewAnchorScriptTree(delayKey)
	require.NoError(t, err)
	require.Equal(t, anchorTree.TapscriptRoot, sd.TapTweak)
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

// TestPublishPreSignedHtlcTxAnchorSupervision exercises the publisher
// goroutine's anchor supervision loop: an immediate offer error is retried
// on the next block, a failed sweep result re-offers on the next block, and
// a successful sweep (or a remote spend of the anchor) ends the loop.
func TestPublishPreSignedHtlcTxAnchorSupervision(t *testing.T) {
	t.Parallel()

	newEpoch := func() *chainntnfs.BlockEpoch {
		return &chainntnfs.BlockEpoch{Height: 100}
	}

	// Registration failure must surface synchronously, before any
	// goroutine is spawned.
	t.Run("registration failure", func(t *testing.T) {
		t.Parallel()

		notifier := &flakyEpochNotifier{
			ChainNotifier: &mock.ChainNotifier{},
			failures:      1,
		}

		var wg sync.WaitGroup
		err := publishPreSignedHtlcTx(
			wire.NewMsgTx(2), "test",
			func(*wire.MsgTx, string) error { return nil },
			notifier, make(chan struct{}), &wg, log, nil,
		)
		require.ErrorContains(t, err, "register block epochs")
	})

	// runSupervision starts the publisher with an instrumented
	// sweepAnchor and returns the channel on which each offer attempt
	// delivers its fresh result channel (nil for the initial error
	// attempt), plus the epoch feed and a cleanup-tracked WaitGroup.
	runSupervision := func(t *testing.T) (chan chan sweep.Result,
		chan *chainntnfs.BlockEpoch, chan struct{}, *sync.WaitGroup) {

		epochChan := make(chan *chainntnfs.BlockEpoch, 1)
		notifier := &mock.ChainNotifier{EpochChan: epochChan}

		published := make(chan struct{}, 1)
		publish := func(*wire.MsgTx, string) error {
			published <- struct{}{}
			return nil
		}

		// Every offer attempt reports here: a nil channel means the
		// attempt returned an error, otherwise the value is the
		// result channel handed to the supervision loop.
		offers := make(chan chan sweep.Result, 4)
		attempt := 0
		sweepAnchor := func() (<-chan sweep.Result, error) {
			attempt++
			if attempt == 1 {
				offers <- nil
				return nil, fmt.Errorf("sweeper unavailable")
			}

			rc := make(chan sweep.Result, 1)
			offers <- rc

			return rc, nil
		}

		quit := make(chan struct{})
		var wg sync.WaitGroup
		err := publishPreSignedHtlcTx(
			wire.NewMsgTx(2), "test", publish, notifier, quit,
			&wg, log, sweepAnchor,
		)
		require.NoError(t, err)

		// First epoch: the tx is published and the first offer
		// attempt fails immediately.
		epochChan <- newEpoch()
		<-published
		require.Nil(t, <-offers)

		// Second epoch: the offer is retried and this time the
		// sweeper accepts it.
		epochChan <- newEpoch()

		t.Cleanup(func() {
			close(quit)
			wg.Wait()
		})

		return offers, epochChan, quit, &wg
	}

	// A failed sweep result must re-offer on the next block; a
	// successful result ends supervision.
	t.Run("failed result re-offers", func(t *testing.T) {
		t.Parallel()

		offers, epochChan, _, wg := runSupervision(t)

		rc := <-offers
		require.NotNil(t, rc)

		// Deliver a failed outcome: the loop must re-offer after the
		// next block.
		rc <- sweep.Result{Err: fmt.Errorf("budget exhausted")}
		epochChan <- newEpoch()

		rc = <-offers
		require.NotNil(t, rc)

		// A successful outcome ends the loop.
		rc <- sweep.Result{Tx: wire.NewMsgTx(2)}
		wg.Wait()
	})

	// A remote spend of the anchor is terminal: no re-offer.
	t.Run("remote spend is terminal", func(t *testing.T) {
		t.Parallel()

		offers, _, _, wg := runSupervision(t)

		rc := <-offers
		require.NotNil(t, rc)

		rc <- sweep.Result{Err: sweep.ErrRemoteSpend}
		wg.Wait()

		select {
		case rc := <-offers:
			t.Fatalf("unexpected re-offer after remote "+
				"spend: %v", rc)
		default:
		}
	})
}

// flakyEpochNotifier wraps the mock notifier and fails the first `failures`
// block epoch registrations.
type flakyEpochNotifier struct {
	*mock.ChainNotifier

	failures int
}

// RegisterBlockEpochNtfn fails while failures is positive, then delegates to
// the embedded mock.
func (f *flakyEpochNotifier) RegisterBlockEpochNtfn(
	epoch *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	if f.failures > 0 {
		f.failures--
		return nil, fmt.Errorf("notifier not ready")
	}

	return f.ChainNotifier.RegisterBlockEpochNtfn(epoch)
}

// TestHtlcTimeoutResolverLaunchRetry asserts that a synchronous publish
// setup failure (failing block epoch registration) does not leave the
// resolver permanently marked as launched: a later Launch call must retry
// and actually publish the pre-signed second-level tx.
func TestHtlcTimeoutResolverLaunchRetry(t *testing.T) {
	t.Parallel()

	customChanType := channeldb.SimpleTaprootFeatureBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SingleFunderTweaklessBit |
		channeldb.TapscriptRootBit

	notifier := &flakyEpochNotifier{
		ChainNotifier: &mock.ChainNotifier{
			EpochChan: make(chan *chainntnfs.BlockEpoch, 1),
		},
		failures: 1,
	}

	published := make(chan *wire.MsgTx, 1)
	cfg := ResolverConfig{
		ChannelArbitratorConfig: ChannelArbitratorConfig{
			ChainArbitratorConfig: ChainArbitratorConfig{
				Notifier: notifier,
				PublishTx: func(tx *wire.MsgTx,
					_ string) error {

					published <- tx
					return nil
				},
				Sweeper: newMockSweeper(),
				Budget:  *DefaultBudgetConfig(),
			},
		},
	}

	// A single-output pre-signed timeout tx (no anchor) keeps the
	// supervision phase a no-op, which is irrelevant to this test.
	timeoutTx := wire.NewMsgTx(2)
	timeoutTx.AddTxOut(&wire.TxOut{Value: 630})

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
		},
		chanType: customChanType,
	}
	resolver.initLogger("test")

	// The first Launch hits the failing registration: it must error AND
	// leave the resolver un-launched so it can be retried.
	err := resolver.Launch()
	require.ErrorContains(t, err, "notifier not ready")
	require.False(t, resolver.isLaunched(),
		"failed launch must not leave the resolver marked launched")

	// The second Launch, with the notifier restored, must schedule the
	// publish: feeding a block epoch results in the pre-signed tx being
	// broadcast.
	require.NoError(t, resolver.Launch())
	require.True(t, resolver.isLaunched())

	notifier.EpochChan <- &chainntnfs.BlockEpoch{Height: 100}
	require.Equal(t, timeoutTx.TxHash(), (<-published).TxHash())

	resolver.Stop()
	resolver.wg.Wait()
}

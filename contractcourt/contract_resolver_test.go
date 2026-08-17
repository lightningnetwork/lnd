package contractcourt

import (
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
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

	quit := make(chan struct{})
	var wg sync.WaitGroup
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
			quit:            quit,
			wg:              &wg,
			log:             log,
		}
	}

	// A single-output parent (no anchor appended) must be a no-op.
	sweeper := newMockSweeper()
	oneOutTx := wire.NewMsgTx(2)
	oneOutTx.AddTxOut(&wire.TxOut{Value: htlcOutValue})
	err = offerSecondLevelAnchorToSweeper(
		newReq(sweeper, oneOutTx, signDesc),
	)
	require.NoError(t, err)
	require.Empty(t, sweeper.sweptInputs)

	// A parent with an anchor but a descriptor lacking key material must
	// also be a (logged) no-op rather than an error.
	twoOutTx := wire.NewMsgTx(2)
	twoOutTx.AddTxOut(&wire.TxOut{Value: htlcOutValue})
	twoOutTx.AddTxOut(&wire.TxOut{
		Value: int64(lnwallet.AnchorSize),
	})
	err = offerSecondLevelAnchorToSweeper(
		newReq(sweeper, twoOutTx, input.SignDescriptor{}),
	)
	require.NoError(t, err)
	require.Empty(t, sweeper.sweptInputs)

	// The well-formed case: the anchor outpoint at index 1 is offered
	// with the caller's deadline.
	err = offerSecondLevelAnchorToSweeper(
		newReq(sweeper, twoOutTx, signDesc),
	)
	require.NoError(t, err)
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

package contractcourt

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// Channel types used by the second-level resolution blob tests.
const (
	// blobTestAuxChanType is an aux/custom (taproot asset) channel.
	blobTestAuxChanType = channeldb.SimpleTaprootFeatureBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SingleFunderTweaklessBit |
		channeldb.TapscriptRootBit

	// blobTestTaprootChanType is a regular simple taproot channel.
	blobTestTaprootChanType = channeldb.SimpleTaprootFeatureBit |
		channeldb.AnchorOutputsBit |
		channeldb.ZeroHtlcTxFeeBit |
		channeldb.SingleFunderTweaklessBit
)

// secondLevelBlobTestCase describes a resolver whose second-level HTLC tx
// confirmed and that is about to sweep its output.
type secondLevelBlobTestCase struct {
	name        string
	chanType    channeldb.ChannelType
	sigHashType txscript.SigHashType
	resolveErr  error

	// wantResolved is true if the blob must be resolved anew against the
	// second-level tx.
	wantResolved bool

	// wantBlob is the blob the output must be swept with.
	wantBlob tlv.Blob
}

// Blobs used by the second-level resolution blob tests.
var (
	// forceCloseBlob is the blob resolved at force close time.
	forceCloseBlob = tlv.Blob{0x01}

	// secondLevelBlob is the blob resolved against the second-level tx.
	secondLevelBlob = tlv.Blob{0x02}
)

// secondLevelBlobTestCases covers the gate of the second-level blob
// re-resolution. Only a pre-signed SigHashDefault second-level tx, which
// exists on aux channels alone, confirms exactly as we hold it. A
// SINGLE|ANYONECANPAY one is rebuilt by the sweeper, so on such a channel the
// blob from force close time must be kept, aux channel or not: a blob
// resolved against our copy of the transaction would point at a transaction
// that never confirmed.
var secondLevelBlobTestCases = []secondLevelBlobTestCase{{
	name:         "aux channel, SigHashDefault",
	chanType:     blobTestAuxChanType,
	sigHashType:  txscript.SigHashDefault,
	wantResolved: true,
	wantBlob:     secondLevelBlob,
}, {
	name:     "aux channel, SINGLE|ANYONECANPAY",
	chanType: blobTestAuxChanType,
	sigHashType: txscript.SigHashSingle |
		txscript.SigHashAnyOneCanPay,
	wantBlob: forceCloseBlob,
}, {
	// SigHashDefault is the zero value of the sighash type, so a regular
	// channel that never sets it must not be mistaken for one that
	// negotiated it.
	name:        "regular taproot channel",
	chanType:    blobTestTaprootChanType,
	sigHashType: txscript.SigHashDefault,
	wantBlob:    forceCloseBlob,
}, {
	// If the aux resolver fails, the blob from force close time is all
	// we have.
	name:         "aux channel, SigHashDefault, resolver fails",
	chanType:     blobTestAuxChanType,
	sigHashType:  txscript.SigHashDefault,
	resolveErr:   errors.New("cannot resolve"),
	wantResolved: true,
	wantBlob:     forceCloseBlob,
}}

// newSecondLevelBlobTestConfig returns a resolver config with the given aux
// resolver attached, the way it is attached to every resolver of a node that
// runs with an aux resolver, whatever the channel type.
func newSecondLevelBlobTestConfig(resolver *auxTestResolver) ResolverConfig {
	return ResolverConfig{
		ChannelArbitratorConfig: ChannelArbitratorConfig{
			ChainArbitratorConfig: ChainArbitratorConfig{
				//nolint:ll
				AuxResolver: fn.Some[lnwallet.AuxContractResolver](
					resolver,
				),
			},
		},
	}
}

// requireSecondLevelBlob asserts the outcome of a second-level resolution
// blob test case.
func requireSecondLevelBlob(t *testing.T, tc secondLevelBlobTestCase,
	resolver *auxTestResolver, secondLevelTx *wire.MsgTx,
	witType input.WitnessType, blob fn.Option[tlv.Blob]) {

	t.Helper()

	require.Equal(t, fn.Some(tc.wantBlob), blob)

	if !tc.wantResolved {
		require.Empty(t, resolver.reqs)

		return
	}

	// The request targets the second-level output sweep and carries the
	// second-level tx along with the height it confirmed at.
	require.Len(t, resolver.reqs, 1)
	req := resolver.reqs[0]
	require.Equal(t, witType, req.Type)

	secondLevel, err := req.SecondLevel.UnwrapOrErr(
		errors.New("no second-level info"),
	)
	require.NoError(t, err)
	require.Equal(t, secondLevelTx, secondLevel.Tx)
	require.EqualValues(t, 800, secondLevel.BlockHeight)
}

// TestHtlcSuccessResolverSecondLevelBlob asserts which resolution blob the
// success resolver sweeps the output of its second-level tx with.
func TestHtlcSuccessResolverSecondLevelBlob(t *testing.T) {
	t.Parallel()

	witType := input.WitnessType(
		input.TaprootHtlcAcceptedSuccessSecondLevel,
	)

	for _, tc := range secondLevelBlobTestCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolver := &auxTestResolver{
				blob: secondLevelBlob,
				err:  tc.resolveErr,
			}
			successTx := wire.NewMsgTx(2)
			successTx.AddTxOut(&wire.TxOut{Value: 630})

			cfg := newSecondLevelBlobTestConfig(resolver)
			h := &htlcSuccessResolver{
				contractResolverKit: *newContractResolverKit(
					cfg,
				),
				htlcResolution: lnwallet.IncomingHtlcResolution{
					SignedSuccessTx: successTx,
					SignDetails: &input.SignDetails{
						SigHashType: tc.sigHashType,
					},
					ResolutionBlob: fn.Some(forceCloseBlob),

					ResolveReq: &lnwallet.ResolutionReq{},
				},
				chanType: tc.chanType,
			}
			h.initLogger("test")

			blob := h.secondLevelResolutionBlob(witType, 800)
			requireSecondLevelBlob(
				t, tc, resolver, successTx, witType, blob,
			)
		})
	}
}

// TestHtlcTimeoutResolverSecondLevelBlob asserts which resolution blob the
// timeout resolver sweeps the output of its second-level tx with.
func TestHtlcTimeoutResolverSecondLevelBlob(t *testing.T) {
	t.Parallel()

	witType := input.WitnessType(
		input.TaprootHtlcOfferedTimeoutSecondLevel,
	)

	for _, tc := range secondLevelBlobTestCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolver := &auxTestResolver{
				blob: secondLevelBlob,
				err:  tc.resolveErr,
			}
			timeoutTx := wire.NewMsgTx(2)
			timeoutTx.AddTxOut(&wire.TxOut{Value: 630})

			cfg := newSecondLevelBlobTestConfig(resolver)
			h := &htlcTimeoutResolver{
				contractResolverKit: *newContractResolverKit(
					cfg,
				),
				htlcResolution: lnwallet.OutgoingHtlcResolution{
					SignedTimeoutTx: timeoutTx,
					SignDetails: &input.SignDetails{
						SigHashType: tc.sigHashType,
					},
					ResolutionBlob: fn.Some(forceCloseBlob),

					ResolveReq: &lnwallet.ResolutionReq{},
				},
				chanType: tc.chanType,
			}
			h.initLogger("test")

			blob := h.secondLevelResolutionBlob(witType, 800)
			requireSecondLevelBlob(
				t, tc, resolver, timeoutTx, witType, blob,
			)
		})
	}
}

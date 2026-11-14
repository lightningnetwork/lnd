package chancloser

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/types"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	// auxLocalRecords are the custom records our mock aux closer puts
	// into our shutdown message.
	auxLocalRecords = lnwire.CustomRecords{
		65537: []byte("local"),
	}

	// auxRemoteRecords are the custom records the remote party sends in
	// its shutdown message.
	auxRemoteRecords = lnwire.CustomRecords{
		65537: []byte("remote"),
	}

	// auxExtraScript is the pkScript of the extra output the mock aux
	// closer adds to the close transaction.
	auxExtraScript = append(
		[]byte{0x51, 0x20}, bytes.Repeat([]byte{0x03}, 32)...,
	)

	// auxExtraValue is the value of the extra output the mock aux closer
	// adds to the close transaction.
	auxExtraValue = btcutil.Amount(1000)

	auxCommitBlob  = tlv.Blob("commit")
	auxFundingBlob = tlv.Blob("funding")

	errAuxCloser = fmt.Errorf("aux closer failed")
)

// rbfMockChanInfo is a static ChanInfo for the aux closer tests.
type rbfMockChanInfo struct {
	initiator bool
	commitFee btcutil.Amount
}

func (m *rbfMockChanInfo) IsInitiator() bool {
	return m.initiator
}

func (m *rbfMockChanInfo) CommitFee() btcutil.Amount {
	return m.commitFee
}

func (m *rbfMockChanInfo) LocalCommitmentBlob() fn.Option[tlv.Blob] {
	return fn.Some(auxCommitBlob)
}

func (m *rbfMockChanInfo) FundingBlob() fn.Option[tlv.Blob] {
	return fn.Some(auxFundingBlob)
}

func (m *rbfMockChanInfo) LocalBalanceDust() (bool, btcutil.Amount) {
	return false, 354
}

func (m *rbfMockChanInfo) RemoteBalanceDust() (bool, btcutil.Amount) {
	return false, 354
}

// rbfMockAuxCloser is an aux closer that records the requests it receives,
// so the tests can assert what the state machine handed to it.
type rbfMockAuxCloser struct {
	// shape is the shape the closer declares for fee estimation.
	shape []AuxCloseOutputShape

	// outputs are the concrete outputs the closer returns.
	outputs []lnwallet.CloseOutput

	shutdownErr error
	outputsErr  error

	shutdownReqs []types.AuxShutdownReq
	closeDescs   []types.AuxCloseDesc
}

func newRbfMockAuxCloser() *rbfMockAuxCloser {
	return &rbfMockAuxCloser{
		shape: []AuxCloseOutputShape{{
			IsLocal:      true,
			PkScriptSize: len(auxExtraScript),
		}},
		outputs: []lnwallet.CloseOutput{{
			TxOut: wire.TxOut{
				PkScript: auxExtraScript,
				Value:    int64(auxExtraValue),
			},
			IsLocal: true,
		}},
	}
}

func (m *rbfMockAuxCloser) ShutdownBlob(
	req types.AuxShutdownReq) (fn.Option[lnwire.CustomRecords], error) {

	m.shutdownReqs = append(m.shutdownReqs, req)

	if m.shutdownErr != nil {
		return fn.None[lnwire.CustomRecords](), m.shutdownErr
	}

	return fn.Some(auxLocalRecords), nil
}

func (m *rbfMockAuxCloser) AuxCloseShape(
	desc types.AuxCloseShapeDesc) (fn.Option[AuxCloseShape], error) {

	return fn.Some(AuxCloseShape{Outputs: m.shape}), nil
}

func (m *rbfMockAuxCloser) AuxCloseOutputs(
	desc types.AuxCloseDesc) (fn.Option[AuxCloseOutputs], error) {

	m.closeDescs = append(m.closeDescs, desc)

	if m.outputsErr != nil {
		return fn.None[AuxCloseOutputs](), m.outputsErr
	}

	return fn.Some(AuxCloseOutputs{
		ExtraCloseOutputs: m.outputs,
		CustomSort:        auxCloseSort,
	}), nil
}

// auxCloseSort orders the close outputs independently of their values, as
// the aux outputs of a negotiation are reused for every fee: the outputs of
// the aux closer come first, followed by the settled outputs by script.
func auxCloseSort(tx *wire.MsgTx) error {
	isAux := func(o *wire.TxOut) bool {
		return bytes.Equal(o.PkScript, auxExtraScript)
	}

	slices.SortStableFunc(tx.TxOut, func(a, b *wire.TxOut) int {
		switch {
		case isAux(a) && !isAux(b):
			return -1
		case !isAux(a) && isAux(b):
			return 1
		default:
			return bytes.Compare(a.PkScript, b.PkScript)
		}
	})

	return nil
}

func (m *rbfMockAuxCloser) FinalizeClose(types.AuxCloseDesc,
	*wire.MsgTx) error {

	return nil
}

func (m *rbfMockAuxCloser) SupportsRbfClose(lnwire.ChannelID,
	route.Vertex) bool {

	return true
}

// auxHarnessCfg returns a harness config for an aux channel with the given
// aux closer wired in, along with a chan info and internal key lookup.
func auxHarnessCfg(auxCloser *rbfMockAuxCloser, initiator bool,
	internalKey *btcec.PublicKey) *harnessCfg {

	return &harnessCfg{
		chanType:  channeldb.TapscriptRootBit,
		auxCloser: fn.Some[AuxChanCloser](auxCloser),
		chanInfo: &rbfMockChanInfo{
			initiator: initiator,
			commitFee: 500,
		},
		deliveryAddrInternalKey: func(
			addr lnwire.DeliveryAddress) (
			fn.Option[btcec.PublicKey], error) {

			if !bytes.Equal(addr, localAddr) {
				return fn.None[btcec.PublicKey](), nil
			}

			return fn.Some(*internalKey), nil
		},
	}
}

// auxTestEnv returns an environment with the aux parts of the given harness
// config, which is enough to build the aux close template and the close
// transactions outside of the state machine.
func auxTestEnv(cfg *harnessCfg) *Environment {
	return &Environment{
		ChanType:                cfg.chanType,
		AuxCloser:               cfg.auxCloser,
		ChanInfo:                cfg.chanInfo,
		DeliveryAddrInternalKey: cfg.deliveryAddrInternalKey,
	}
}

// plainAuxCloseTerms returns close terms for a channel that has exchanged
// aux shutdown records, without the aux close template.
func plainAuxCloseTerms(localBalance,
	remoteBalance lnwire.MilliSatoshi) *CloseChannelTerms {

	return &CloseChannelTerms{
		ShutdownBalances: ShutdownBalances{
			LocalBalance:  localBalance,
			RemoteBalance: remoteBalance,
		},
		ShutdownScripts: ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		},
		ShutdownCustomRecords: ShutdownCustomRecords{
			LocalCustomRecords:  auxLocalRecords,
			RemoteCustomRecords: auxRemoteRecords,
		},
	}
}

// auxCloseTerms returns close terms for a channel that has exchanged aux
// shutdown records, with the aux close template the aux closer of the given
// config produces for them. Building the template is the one request the
// aux closer sees.
func auxCloseTerms(t *testing.T, cfg *harnessCfg, localBalance,
	remoteBalance lnwire.MilliSatoshi) *CloseChannelTerms {

	t.Helper()

	terms := plainAuxCloseTerms(localBalance, remoteBalance)

	template, err := auxTestEnv(cfg).auxCloseTemplate(terms)
	require.NoError(t, err)
	require.True(t, template.IsSome())

	terms.AuxTemplate = template

	return terms
}

// auxCloseTx returns the close transaction the channel builds for the given
// fee and payer under the given terms.
func auxCloseTx(t *testing.T, cfg *harnessCfg, terms *CloseChannelTerms,
	fee btcutil.Amount, payer lntypes.ChannelParty) *wire.MsgTx {

	t.Helper()

	closeTx, err := auxTestEnv(cfg).closeTxForFee(
		terms, fee, fn.Some(payer), terms.auxOutputs(),
	)
	require.NoError(t, err)

	return closeTx
}

// withoutAuxOutput returns a copy of the close transaction without the
// output of the mock aux closer.
func withoutAuxOutput(t *testing.T, closeTx *wire.MsgTx) *wire.MsgTx {
	t.Helper()

	stripped := closeTx.Copy()
	stripped.TxOut = fn.Filter(stripped.TxOut, func(o *wire.TxOut) bool {
		return !bytes.Equal(o.PkScript, auxExtraScript)
	})
	require.Len(t, stripped.TxOut, len(closeTx.TxOut)-1)

	return stripped
}

// assertAuxShutdownReq asserts that the aux closer was handed the channel
// info the harness was configured with.
func assertAuxShutdownReq(t *testing.T, h *rbfCloserTestHarness,
	req types.AuxShutdownReq, initiator bool,
	internalKey *btcec.PublicKey) {

	t.Helper()

	require.Equal(t, h.env.ChanPoint, req.ChanPoint)
	require.Equal(t, h.env.Scid, req.ShortChanID)
	require.Equal(t, initiator, req.Initiator)
	require.Equal(t, fn.Some(*internalKey), req.InternalKey)
	require.Equal(t, fn.Some(auxCommitBlob), req.CommitBlob)
	require.Equal(t, fn.Some(auxFundingBlob), req.FundingBlob)
}

// expectFeeEstimateWithAuxShape expects a fee estimate call whose extra
// outputs match the shape declared by the aux closer.
func (r *rbfCloserTestHarness) expectFeeEstimateWithAuxShape(
	absoluteFee btcutil.Amount, shape []AuxCloseOutputShape,
	numTimes int) {

	r.T.Helper()

	extraOutsMatch := func(extraTxOuts []*wire.TxOut) bool {
		if len(extraTxOuts) != len(shape) {
			return false
		}
		for i, out := range extraTxOuts {
			if len(out.PkScript) != shape[i].PkScriptSize {
				return false
			}
		}

		return true
	}

	r.feeEstimator.On(
		"EstimateFee", mock.Anything, mock.Anything, mock.Anything,
		mock.MatchedBy(extraOutsMatch), mock.Anything,
	).Return(absoluteFee, nil).Times(numTimes)
}

// expectAuxCloseProposal expects a close proposal to be signed, and hands
// back the given transaction for it.
func (r *rbfCloserTestHarness) expectAuxCloseProposal(fee,
	closeBalance btcutil.Amount, closeTx *wire.MsgTx) {

	r.T.Helper()

	r.signer.On(
		"CreateCloseProposal", fee, []byte(localAddr),
		[]byte(remoteAddr), mock.Anything,
	).Return(&localSig, closeTx, closeBalance, nil)
}

// expectAuxCloseCompleted expects the close to be completed with the given
// transaction, which is then marked as broadcast and published.
func (r *rbfCloserTestHarness) expectAuxCloseCompleted(fee,
	closeBalance btcutil.Amount, isLocal bool, closeTx *wire.MsgTx) {

	r.T.Helper()

	r.signer.On(
		"CompleteCooperativeClose", &localSig, &remoteSig,
		[]byte(localAddr), []byte(remoteAddr), fee, mock.Anything,
	).Return(closeTx, closeBalance, nil)
	r.chanObserver.On("MarkCoopBroadcasted", closeTx, isLocal).Return(nil)
	r.daemonAdapters.On(
		"BroadcastTransaction", closeTx, mock.Anything,
	).Return(nil)
}

// assertClosePending asserts that the given party's state is ClosePending
// for the given close transaction.
func assertClosePending(t *testing.T, h *rbfCloserTestHarness,
	party lntypes.ChannelParty, closeTx *wire.MsgTx) {

	t.Helper()

	currentState := assertStateT[*ClosingNegotiation](h)
	pendingState, ok := currentState.PeerState.GetForParty(
		party,
	).(*ClosePending)
	require.True(t, ok)
	require.Equal(t, closeTx, pendingState.CloseTx)
}

// TestRbfAuxShutdownRecords asserts that the custom records of the aux
// closer make it into our shutdown message, and that the records of both
// parties are carried through to the close terms.
func TestRbfAuxShutdownRecords(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	internalKey := randPubKey(t)
	feeRate := chainfee.SatPerVByte(1000)

	shutdownWithRecords := singleMsgMatcher(func(m *lnwire.Shutdown) bool {
		return maps.EqualFunc(
			m.CustomRecords, auxLocalRecords, bytes.Equal,
		)
	})
	shutdownWithoutRecords := singleMsgMatcher(
		func(m *lnwire.Shutdown) bool {
			return len(m.CustomRecords) == 0
		},
	)

	// When we initiate the close, our shutdown message carries the aux
	// records, and the records of the remote party's shutdown are stored
	// once it arrives.
	t.Run("local_initiated", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.localUpfrontAddr = fn.Some(localAddr)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectShutdownEvents(shutdownExpect{
			isInitiator: true,
			allowSend:   true,
		})
		closeHarness.expectMsgSent(shutdownWithRecords)

		closeHarness.chanCloser.SendEvent(
			ctx, &SendShutdown{IdealFeeRate: feeRate},
		)
		closeHarness.assertStateTransitions(&ShutdownPending{})

		pendingState := assertStateT[*ShutdownPending](closeHarness)
		require.Equal(
			t, auxLocalRecords, pendingState.LocalCustomRecords,
		)
		require.Empty(t, pendingState.RemoteCustomRecords)

		// The aux closer should have been asked once, with our
		// delivery address' internal key.
		require.Len(t, auxCloser.shutdownReqs, 1)
		assertAuxShutdownReq(
			t, closeHarness, auxCloser.shutdownReqs[0], true,
			internalKey,
		)

		closeHarness.waitForMsgSent()

		// Once the remote party sends its shutdown, its records are
		// stored alongside ours.
		closeHarness.expectIncomingAddsDisabled()
		closeHarness.chanCloser.SendEvent(ctx, &ShutdownReceived{
			ShutdownScript: remoteAddr,
			CustomRecords:  auxRemoteRecords,
		})
		closeHarness.assertStateTransitions(&ChannelFlushing{})

		flushingState := assertStateT[*ChannelFlushing](closeHarness)
		require.Equal(
			t, auxLocalRecords, flushingState.LocalCustomRecords,
		)
		require.Equal(
			t, auxRemoteRecords, flushingState.RemoteCustomRecords,
		)
	})

	// When the remote party initiates the close, we store its records,
	// and reply with a shutdown that carries ours.
	t.Run("remote_initiated", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, false, internalKey)
		cfg.localUpfrontAddr = fn.Some(localAddr)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectShutdownEvents(shutdownExpect{
			isInitiator:  false,
			allowSend:    true,
			recvShutdown: true,
		})
		closeHarness.expectMsgSent(shutdownWithRecords)

		closeHarness.chanCloser.SendEvent(ctx, &ShutdownReceived{
			ShutdownScript: remoteAddr,
			CustomRecords:  auxRemoteRecords,
		})

		// Our shutdown goes out right away, and once it did we move
		// on to the flushing state, which carries the records of
		// both parties.
		closeHarness.assertStateTransitions(
			&ShutdownPending{}, &ChannelFlushing{},
		)
		closeHarness.waitForMsgSent()

		require.Len(t, auxCloser.shutdownReqs, 1)
		assertAuxShutdownReq(
			t, closeHarness, auxCloser.shutdownReqs[0], false,
			internalKey,
		)

		flushingState := assertStateT[*ChannelFlushing](closeHarness)
		require.Equal(
			t, auxLocalRecords, flushingState.LocalCustomRecords,
		)
		require.Equal(
			t, auxRemoteRecords, flushingState.RemoteCustomRecords,
		)
	})

	// A channel without a tapscript root isn't an aux channel, so the
	// aux closer isn't consulted for it even if one is configured.
	t.Run("plain_channel", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.chanType = channeldb.SingleFunderTweaklessBit
		cfg.localUpfrontAddr = fn.Some(localAddr)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectShutdownEvents(shutdownExpect{
			isInitiator: true,
			allowSend:   true,
		})
		closeHarness.expectMsgSent(shutdownWithoutRecords)

		closeHarness.chanCloser.SendEvent(
			ctx, &SendShutdown{IdealFeeRate: feeRate},
		)
		closeHarness.assertStateTransitions(&ShutdownPending{})
		closeHarness.waitForMsgSent()

		pendingState := assertStateT[*ShutdownPending](closeHarness)
		require.Empty(t, pendingState.LocalCustomRecords)
		require.Empty(t, auxCloser.shutdownReqs)
	})

	// If the aux closer fails to produce the shutdown records, the state
	// machine reports the error and doesn't send a shutdown.
	t.Run("aux_closer_error", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		auxCloser.shutdownErr = errAuxCloser

		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.localUpfrontAddr = fn.Some(localAddr)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.sendEventAndExpectFailure(
			ctx, &SendShutdown{IdealFeeRate: feeRate}, errAuxCloser,
		)
		closeHarness.assertNoStateTransitions()
	})

	// An aux closer without the chan info can't be driven, so the state
	// machine fails rather than sending a shutdown without records.
	t.Run("missing_chan_info", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.localUpfrontAddr = fn.Some(localAddr)
		cfg.chanInfo = nil

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.sendEventAndExpectFailure(
			ctx, &SendShutdown{IdealFeeRate: feeRate},
			ErrAuxChanInfoMissing,
		)
		closeHarness.assertNoStateTransitions()
		require.Empty(t, auxCloser.shutdownReqs)
	})
}

// TestRbfAuxFlushedTemplate asserts that once the channel is flushed, the
// aux closer is asked for its outputs exactly once, without a fee, and that
// the outputs make up the aux close template of the negotiation.
func TestRbfAuxFlushedTemplate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	internalKey := randPubKey(t)
	absoluteFee := btcutil.Amount(10_100)
	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)

	flushingState := func() *ChannelFlushing {
		return &ChannelFlushing{
			ShutdownScripts: ShutdownScripts{
				LocalDeliveryScript:  localAddr,
				RemoteDeliveryScript: remoteAddr,
			},
			ShutdownCustomRecords: ShutdownCustomRecords{
				LocalCustomRecords:  auxLocalRecords,
				RemoteCustomRecords: auxRemoteRecords,
			},
		}
	}
	flushedEvent := &ChannelFlushed{
		ShutdownBalances: ShutdownBalances{
			LocalBalance:  localBalance,
			RemoteBalance: remoteBalance,
		},
	}

	t.Run("template_built", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.initialState = fn.Some[ProtocolState](flushingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		// We can pay the fee, so the flushed event is followed by our
		// offer, which carries the aux outputs. The fee is estimated
		// with the aux output shape both times.
		terms := auxCloseTerms(t, cfg, localBalance, remoteBalance)
		offerTx := auxCloseTx(
			t, cfg, terms, absoluteFee, lntypes.Local,
		)
		auxCloser.closeDescs = nil

		closeHarness.expectFeeEstimateWithAuxShape(
			absoluteFee, auxCloser.shape, 2,
		)
		closeHarness.expectAuxCloseProposal(
			absoluteFee, localBalance.ToSatoshis()-absoluteFee,
			offerTx,
		)
		closeHarness.expectMsgSent(
			singleMsgMatcher[*lnwire.ClosingComplete](nil),
		)

		closeHarness.chanCloser.SendEvent(ctx, flushedEvent)
		closeHarness.assertStateTransitions(
			&ClosingNegotiation{}, &ClosingNegotiation{},
		)
		closeHarness.waitForMsgSent()

		// The aux closer was asked for its outputs once, without a
		// fee, and with the close outputs carrying the settled
		// balances and both parties' shutdown records.
		require.Len(t, auxCloser.closeDescs, 1)
		desc := auxCloser.closeDescs[0]
		assertAuxShutdownReq(
			t, closeHarness, desc.AuxShutdownReq, true, internalKey,
		)
		require.True(t, desc.FeeIndependent)
		require.Zero(t, desc.CloseFee)
		require.Equal(t, btcutil.Amount(500), desc.CommitFee)

		localOut := desc.LocalCloseOutput.UnsafeFromSome()
		require.Equal(t, localBalance.ToSatoshis(), localOut.Amt)
		require.EqualValues(t, localAddr, localOut.PkScript)
		require.Equal(t, auxLocalRecords, localOut.ShutdownRecords)

		remoteOut := desc.RemoteCloseOutput.UnsafeFromSome()
		require.Equal(t, remoteBalance.ToSatoshis(), remoteOut.Amt)
		require.EqualValues(t, remoteAddr, remoteOut.PkScript)
		require.Equal(t, auxRemoteRecords, remoteOut.ShutdownRecords)

		// The close terms carry the records and the template: the
		// aux output sorts first by its value, followed by the two
		// settled outputs.
		currentState := assertStateT[*ClosingNegotiation](closeHarness)
		require.Equal(
			t, auxLocalRecords, currentState.LocalCustomRecords,
		)
		require.Equal(
			t, auxRemoteRecords, currentState.RemoteCustomRecords,
		)

		template := currentState.AuxTemplate.UnsafeFromSome()
		require.Equal(
			t, auxCloser.outputs,
			template.Outputs.ExtraCloseOutputs,
		)
		require.Equal(
			t, [][]byte{auxExtraScript, localAddr, remoteAddr},
			template.PkScripts,
		)

		// Our offer was built from the template.
		offerSentState, ok := currentState.PeerState.GetForParty(
			lntypes.Local,
		).(*LocalOfferSent)
		require.True(t, ok)
		require.Equal(t, absoluteFee, offerSentState.ProposedFee)
	})

	// A channel without a tapscript root has no aux outputs, so there's
	// no template and the fee is estimated without extra outputs.
	t.Run("plain_channel", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.chanType = channeldb.SingleFunderTweaklessBit
		cfg.initialState = fn.Some[ProtocolState](flushingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectFeeEstimateWithAuxShape(
			absoluteFee, nil, 2,
		)
		closeHarness.expectNewCloseSig(
			localAddr, remoteAddr, absoluteFee,
			localBalance.ToSatoshis()-absoluteFee,
		)
		closeHarness.expectMsgSent(
			singleMsgMatcher[*lnwire.ClosingComplete](nil),
		)

		closeHarness.chanCloser.SendEvent(ctx, flushedEvent)
		closeHarness.assertStateTransitions(
			&ClosingNegotiation{}, &ClosingNegotiation{},
		)
		closeHarness.waitForMsgSent()

		require.Empty(t, auxCloser.closeDescs)

		currentState := assertStateT[*ClosingNegotiation](closeHarness)
		require.True(t, currentState.AuxTemplate.IsNone())
	})

	// If the aux closer returns outputs that don't match the shape it
	// declared for the fee estimate, the negotiation doesn't start.
	t.Run("shape_mismatch", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		auxCloser.outputs = append(
			auxCloser.outputs, auxCloser.outputs[0],
		)

		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.initialState = fn.Some[ProtocolState](flushingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.sendEventAndExpectFailure(
			ctx, flushedEvent, ErrAuxShapeMismatch,
		)
		closeHarness.assertNoStateTransitions()
	})

	// If the aux closer fails, the negotiation doesn't start either.
	t.Run("aux_closer_error", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		auxCloser.outputsErr = errAuxCloser

		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.initialState = fn.Some[ProtocolState](flushingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.sendEventAndExpectFailure(
			ctx, flushedEvent, errAuxCloser,
		)
		closeHarness.assertNoStateTransitions()
	})
}

// TestRbfAuxLocalOffer asserts that when we act as the closer, the aux
// outputs of the template are part of the fee estimate and the close
// transaction, and that a transaction that doesn't match the template isn't
// offered.
func TestRbfAuxLocalOffer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	internalKey := randPubKey(t)

	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)
	absoluteFee := btcutil.Amount(10_100)
	balanceAfterClose := localBalance.ToSatoshis() - absoluteFee

	newStartingState := func(
		closeTerms *CloseChannelTerms) *ClosingNegotiation {

		return &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: closeTerms,
				},
				Remote: &RemoteCloseStart{
					CloseChannelTerms: closeTerms,
				},
			},
			CloseChannelTerms: closeTerms,
		}
	}

	sendOfferEvent := &SendOfferEvent{
		TargetFeeRate: chainfee.FeePerKwFloor.FeePerVByte(),
	}

	t.Run("aux_outputs_in_offer", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		terms := auxCloseTerms(t, cfg, localBalance, remoteBalance)
		offerTx := auxCloseTx(
			t, cfg, terms, absoluteFee, lntypes.Local,
		)
		cfg.initialState = fn.Some[ProtocolState](
			newStartingState(terms),
		)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		// The fee estimate must account for the aux output shape.
		closeHarness.expectFeeEstimateWithAuxShape(
			absoluteFee, auxCloser.shape, 1,
		)
		closeHarness.expectAuxCloseProposal(
			absoluteFee, balanceAfterClose, offerTx,
		)
		closeHarness.expectMsgSent(
			singleMsgMatcher[*lnwire.ClosingComplete](nil),
		)

		closeHarness.chanCloser.SendEvent(ctx, sendOfferEvent)
		closeHarness.assertStateTransitions(&ClosingNegotiation{})
		closeHarness.waitForMsgSent()

		currentState := assertStateT[*ClosingNegotiation](closeHarness)
		offerSentState, ok := currentState.PeerState.GetForParty(
			lntypes.Local,
		).(*LocalOfferSent)
		require.True(t, ok)
		require.Equal(t, absoluteFee, offerSentState.ProposedFee)

		// Once the remote party signs, the very same aux outputs are
		// used to complete the close.
		closeHarness.expectAuxCloseCompleted(
			absoluteFee, balanceAfterClose, true, offerTx,
		)
		closeHarness.chanCloser.SendEvent(ctx, &LocalSigReceived{
			SigMsg: lnwire.ClosingSig{
				CloserScript: localAddr,
				CloseeScript: remoteAddr,
				ClosingSigs: lnwire.ClosingSigs{
					CloserAndClosee: newSigTlv[tlv.TlvType3]( //nolint:ll
						remoteWireSig,
					),
				},
			},
		})
		closeHarness.assertStateTransitions(&ClosingNegotiation{})
		assertClosePending(t, closeHarness, lntypes.Local, offerTx)

		// The aux closer was only ever asked for the template.
		require.Len(t, auxCloser.closeDescs, 1)
	})

	// A close transaction that doesn't carry the outputs of the template
	// isn't offered, as the aux closer didn't sign for it.
	t.Run("template_violated", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		terms := auxCloseTerms(t, cfg, localBalance, remoteBalance)
		offerTx := withoutAuxOutput(t, auxCloseTx(
			t, cfg, terms, absoluteFee, lntypes.Local,
		))
		cfg.initialState = fn.Some[ProtocolState](
			newStartingState(terms),
		)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectFeeEstimateWithAuxShape(
			absoluteFee, auxCloser.shape, 1,
		)
		closeHarness.expectAuxCloseProposal(
			absoluteFee, balanceAfterClose, offerTx,
		)
		closeHarness.sendEventAndExpectFailure(
			ctx, sendOfferEvent, ErrAuxCloseTemplate,
		)
		closeHarness.assertNoStateTransitions()
	})

	// A fee that would merge our settled output into the aux output
	// changes the output structure, so we don't offer to pay it.
	t.Run("fee_breaks_template", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		terms := auxCloseTerms(t, cfg, localBalance, remoteBalance)
		cfg.initialState = fn.Some[ProtocolState](
			newStartingState(terms),
		)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		// As the initiator we regain the commit fee, which goes into
		// the fee we can pay before our settled output is merged.
		mergingFee := localBalance.ToSatoshis() +
			cfg.chanInfo.CommitFee() - auxExtraValue - 354
		closeHarness.expectFeeEstimateWithAuxShape(
			mergingFee, auxCloser.shape, 1,
		)

		closeHarness.chanCloser.SendEvent(ctx, sendOfferEvent)
		closeHarness.assertStateTransitions(&ClosingNegotiation{})

		currentState := assertStateT[*ClosingNegotiation](closeHarness)
		errState, ok := currentState.PeerState.GetForParty(
			lntypes.Local,
		).(*CloseErr)
		require.True(t, ok)
		require.IsType(t, &ErrStateCantPayForFee{}, errState.ErrState)
	})
}

// TestRbfAuxRemoteOffer asserts that when the remote party is the closer,
// the aux outputs of the template are part of the transaction we counter
// sign, and that we don't sign a transaction that doesn't match the
// template.
func TestRbfAuxRemoteOffer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	internalKey := randPubKey(t)

	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)
	absoluteFee := btcutil.Amount(10_100)

	newStartingState := func(
		closeTerms *CloseChannelTerms) *ClosingNegotiation {

		return &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: closeTerms,
				},
				Remote: &RemoteCloseStart{
					CloseChannelTerms: closeTerms,
				},
			},
			CloseChannelTerms: closeTerms,
		}
	}

	newOffer := func(
		closerScript lnwire.DeliveryAddress) *OfferReceivedEvent {

		return &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				FeeSatoshis:  absoluteFee,
				CloserScript: closerScript,
				CloseeScript: localAddr,
				ClosingSigs: lnwire.ClosingSigs{
					CloserAndClosee: newSigTlv[tlv.TlvType3]( //nolint:ll
						remoteWireSig,
					),
				},
			},
		}
	}

	t.Run("aux_outputs_in_counter_sig", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, false, internalKey)
		terms := auxCloseTerms(t, cfg, localBalance, remoteBalance)
		offerTx := auxCloseTx(
			t, cfg, terms, absoluteFee, lntypes.Remote,
		)
		cfg.initialState = fn.Some[ProtocolState](
			newStartingState(terms),
		)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectAuxCloseProposal(
			absoluteFee, localBalance.ToSatoshis(), offerTx,
		)
		closeHarness.expectAuxCloseCompleted(
			absoluteFee, localBalance.ToSatoshis(), false, offerTx,
		)
		closeHarness.expectMsgSent(
			singleMsgMatcher[*lnwire.ClosingSig](nil),
		)

		closeHarness.chanCloser.SendEvent(ctx, newOffer(remoteAddr))
		closeHarness.assertStateTransitions(&ClosingNegotiation{})
		closeHarness.waitForMsgSent()

		assertClosePending(t, closeHarness, lntypes.Remote, offerTx)

		// The aux closer was only ever asked for the template.
		require.Len(t, auxCloser.closeDescs, 1)
	})

	// The remote party can't switch its delivery script within the
	// negotiation, as its aux records are bound to the script it sent
	// them with.
	t.Run("script_change_rejected", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, false, internalKey)
		terms := auxCloseTerms(t, cfg, localBalance, remoteBalance)
		cfg.initialState = fn.Some[ProtocolState](
			newStartingState(terms),
		)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		newRemoteAddr := lnwire.DeliveryAddress(append(
			[]byte{0x51, 0x20}, bytes.Repeat([]byte{0x04}, 32)...,
		))
		closeHarness.sendEventAndExpectFailure(
			ctx, newOffer(newRemoteAddr), ErrAuxScriptChange,
		)
		closeHarness.assertNoStateTransitions()
	})

	// A close transaction that doesn't carry the outputs of the template
	// isn't counter signed, as the aux closer didn't sign for it.
	t.Run("template_violated", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, false, internalKey)
		terms := auxCloseTerms(t, cfg, localBalance, remoteBalance)
		offerTx := withoutAuxOutput(t, auxCloseTx(
			t, cfg, terms, absoluteFee, lntypes.Remote,
		))
		cfg.initialState = fn.Some[ProtocolState](
			newStartingState(terms),
		)

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectAuxCloseProposal(
			absoluteFee, localBalance.ToSatoshis(), offerTx,
		)
		closeHarness.signer.On(
			"CompleteCooperativeClose", &localSig, &remoteSig,
			[]byte(localAddr), []byte(remoteAddr), absoluteFee,
			mock.Anything,
		).Return(offerTx, localBalance.ToSatoshis(), nil)

		closeHarness.sendEventAndExpectFailure(
			ctx, newOffer(remoteAddr), ErrAuxCloseTemplate,
		)
		closeHarness.assertNoStateTransitions()
	})
}

// TestRbfAuxCloseFin asserts that the terminal state carries the close
// outputs and the aux outputs of the template if one of the negotiated
// transactions confirmed.
func TestRbfAuxCloseFin(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	internalKey := randPubKey(t)

	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)

	localTx := wire.NewMsgTx(2)
	localTx.LockTime = 1
	remoteTx := wire.NewMsgTx(2)
	remoteTx.LockTime = 2
	unknownTx := wire.NewMsgTx(2)
	unknownTx.LockTime = 3

	newStartingState := func(
		closeTerms *CloseChannelTerms) *ClosingNegotiation {

		return &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &ClosePending{
					CloseTx:           localTx,
					CloseChannelTerms: closeTerms,
					Party:             lntypes.Local,
				},
				Remote: &ClosePending{
					CloseTx:           remoteTx,
					CloseChannelTerms: closeTerms,
					Party:             lntypes.Remote,
				},
			},
			CloseChannelTerms: closeTerms,
		}
	}

	tests := []struct {
		name        string
		confirmedTx *wire.MsgTx
		haveOutputs bool
	}{
		{
			name:        "local tx confirmed",
			confirmedTx: localTx,
			haveOutputs: true,
		},
		{
			name:        "remote tx confirmed",
			confirmedTx: remoteTx,
			haveOutputs: true,
		},
		{
			name:        "unknown tx confirmed",
			confirmedTx: unknownTx,
			haveOutputs: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auxCloser := newRbfMockAuxCloser()
			cfg := auxHarnessCfg(auxCloser, true, internalKey)
			terms := auxCloseTerms(
				t, cfg, localBalance, remoteBalance,
			)
			cfg.initialState = fn.Some[ProtocolState](
				newStartingState(terms),
			)

			closeHarness := newCloser(t, cfg)
			defer closeHarness.stopAndAssert()

			closeHarness.chanCloser.SendEvent(
				ctx, &SpendEvent{Tx: test.confirmedTx},
			)
			closeHarness.assertStateTransitions(&CloseFin{})

			closeFin := assertStateT[*CloseFin](closeHarness)
			require.Equal(t, test.confirmedTx, closeFin.ConfirmedTx)

			if !test.haveOutputs {
				require.True(t, closeFin.AuxOutputs.IsNone())
				require.True(
					t, closeFin.LocalCloseOutput.IsNone(),
				)
				require.True(
					t, closeFin.RemoteCloseOutput.IsNone(),
				)

				return
			}

			auxOutputs := closeFin.AuxOutputs.UnsafeFromSome()
			require.Equal(
				t, auxCloser.outputs,
				auxOutputs.ExtraCloseOutputs,
			)

			localOut := closeFin.LocalCloseOutput.UnsafeFromSome()
			require.Equal(
				t, localBalance.ToSatoshis(), localOut.Amt,
			)
			require.EqualValues(t, localAddr, localOut.PkScript)
			require.Equal(
				t, auxLocalRecords, localOut.ShutdownRecords,
			)

			remoteOut := closeFin.RemoteCloseOutput.UnsafeFromSome()
			require.Equal(
				t, remoteBalance.ToSatoshis(), remoteOut.Amt,
			)
			require.EqualValues(t, remoteAddr, remoteOut.PkScript)
			require.Equal(
				t, auxRemoteRecords, remoteOut.ShutdownRecords,
			)
		})
	}
}

// TestRbfAuxCloseOutputsAnchors asserts that the close outputs handed to the
// aux closer credit the anchor value to the initiator of an anchor channel.
func TestRbfAuxCloseOutputsAnchors(t *testing.T) {
	t.Parallel()

	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)
	anchorDelta := 2 * lnwallet.AnchorSize

	anchorChanType := channeldb.AnchorOutputsBit |
		channeldb.SingleFunderTweaklessBit

	tests := []struct {
		name        string
		chanType    channeldb.ChannelType
		initiator   bool
		localDelta  btcutil.Amount
		remoteDelta btcutil.Amount
	}{
		{
			name:       "anchors, initiator",
			chanType:   anchorChanType,
			initiator:  true,
			localDelta: anchorDelta,
		},
		{
			name:        "anchors, responder",
			chanType:    anchorChanType,
			initiator:   false,
			remoteDelta: anchorDelta,
		},
		{
			name:      "no anchors, initiator",
			initiator: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := &Environment{
				ChanType: test.chanType,
				ChanInfo: &rbfMockChanInfo{
					initiator: test.initiator,
				},
			}
			terms := plainAuxCloseTerms(localBalance, remoteBalance)

			localOut, remoteOut := env.closeOutputs(terms)
			require.Equal(
				t, localBalance.ToSatoshis()+test.localDelta,
				localOut.Amt,
			)
			require.Equal(
				t, remoteBalance.ToSatoshis()+test.remoteDelta,
				remoteOut.Amt,
			)
			require.Equal(
				t, btcutil.Amount(354), localOut.DustLimit,
			)
			require.Equal(
				t, btcutil.Amount(354), remoteOut.DustLimit,
			)
		})
	}
}

// TestRbfMsgMapperShutdownRecords asserts that the custom records of a
// shutdown message are mapped into the ShutdownReceived event, so the aux
// closer gets to see the remote party's records.
func TestRbfMsgMapperShutdownRecords(t *testing.T) {
	t.Parallel()

	chanPoint := randOutPoint(t)
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)
	peerPub := randPubKey(t)

	mapper := NewRbfMsgMapper(
		func() uint32 { return 100 }, chanID, *peerPub,
	)

	event := mapper.MapMsg(msgmux.PeerMsg{
		PeerPub: *peerPub,
		Message: &lnwire.Shutdown{
			ChannelID:     chanID,
			Address:       remoteAddr,
			CustomRecords: auxRemoteRecords,
		},
	})
	require.True(t, event.IsSome())

	shutdownEvent, ok := event.UnsafeFromSome().(*ShutdownReceived)
	require.True(t, ok)
	require.Equal(t, remoteAddr, shutdownEvent.ShutdownScript)
	require.Equal(t, auxRemoteRecords, shutdownEvent.CustomRecords)
	require.EqualValues(t, 100, shutdownEvent.BlockHeight)
}

// TestRbfAuxLocalCanPayFees asserts that for an aux channel we only act as
// the closer if the fee keeps the output structure of the aux close
// template, as the aux closer signed its asset state for it.
func TestRbfAuxLocalCanPayFees(t *testing.T) {
	t.Parallel()

	const (
		dustLimit   btcutil.Amount = 354
		absoluteFee btcutil.Amount = 1_000
	)

	internalKey := randPubKey(t)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)

	tests := []struct {
		name         string
		aux          bool
		localBalance btcutil.Amount
		initiator    bool
		commitFee    btcutil.Amount
		canPay       bool
	}{
		{
			name:         "no aux outputs, can afford fee",
			localBalance: absoluteFee,
			canPay:       true,
		},
		{
			name:         "no aux outputs, can't afford fee",
			localBalance: absoluteFee - 1,
			canPay:       false,
		},
		{
			name:         "aux output, can't afford fee",
			aux:          true,
			localBalance: absoluteFee - 1,
			canPay:       false,
		},
		{
			name:         "aux output, settled output dropped",
			aux:          true,
			localBalance: absoluteFee + dustLimit - 1,
			canPay:       false,
		},
		{
			name:         "aux output, settled output merged",
			aux:          true,
			localBalance: absoluteFee + auxExtraValue + dustLimit,
			canPay:       false,
		},
		{
			name: "aux output, settled output kept",
			aux:  true,
			localBalance: absoluteFee + auxExtraValue +
				dustLimit + 1,
			canPay: true,
		},
		{
			name:         "initiator regains commit fee",
			aux:          true,
			localBalance: absoluteFee + auxExtraValue + dustLimit,
			initiator:    true,
			commitFee:    1,
			canPay:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auxCloser := newRbfMockAuxCloser()
			cfg := auxHarnessCfg(
				auxCloser, tc.initiator, internalKey,
			)
			cfg.chanInfo = &rbfMockChanInfo{
				initiator: tc.initiator,
				commitFee: tc.commitFee,
			}

			localBalance := lnwire.NewMSatFromSatoshis(
				tc.localBalance,
			)
			var terms *CloseChannelTerms
			if tc.aux {
				terms = auxCloseTerms(
					t, cfg, localBalance, remoteBalance,
				)
			} else {
				terms = plainAuxCloseTerms(
					localBalance, remoteBalance,
				)
			}

			canPay := auxTestEnv(cfg).localCanPayFees(
				terms, absoluteFee,
			)
			require.Equal(t, tc.canPay, canPay)
		})
	}
}

// TestRbfAuxCloseTemplateCheck asserts that a close transaction matches the
// aux close template only if it carries the template's scripts in order,
// whatever the output values.
func TestRbfAuxCloseTemplateCheck(t *testing.T) {
	t.Parallel()

	scriptA := []byte{0x51, 0x20, 0x01}
	scriptB := []byte{0x51, 0x20, 0x02}
	scriptC := []byte{0x51, 0x20, 0x03}

	newTx := func(values map[int]int64, scripts ...[]byte) *wire.MsgTx {
		tx := wire.NewMsgTx(2)
		for i, script := range scripts {
			tx.AddTxOut(&wire.TxOut{
				PkScript: script, Value: values[i],
			})
		}

		return tx
	}

	// Without a template there's nothing to check.
	var terms CloseChannelTerms
	require.NoError(t, terms.checkAuxCloseTemplate(newTx(nil, scriptA)))

	terms.AuxTemplate = fn.Some(AuxCloseTemplate{
		PkScripts: [][]byte{scriptA, scriptB},
	})

	// The same scripts in the same order match, whatever the values.
	require.NoError(t, terms.checkAuxCloseTemplate(
		newTx(map[int]int64{0: 1, 1: 2}, scriptA, scriptB),
	))
	require.NoError(t, terms.checkAuxCloseTemplate(
		newTx(map[int]int64{0: 1000, 1: 1}, scriptA, scriptB),
	))

	// A missing, extra, swapped or replaced output doesn't.
	require.ErrorIs(t, terms.checkAuxCloseTemplate(
		newTx(nil, scriptA),
	), ErrAuxCloseTemplate)
	require.ErrorIs(t, terms.checkAuxCloseTemplate(
		newTx(nil, scriptA, scriptB, scriptC),
	), ErrAuxCloseTemplate)
	require.ErrorIs(t, terms.checkAuxCloseTemplate(
		newTx(nil, scriptB, scriptA),
	), ErrAuxCloseTemplate)
	require.ErrorIs(t, terms.checkAuxCloseTemplate(
		newTx(nil, scriptA, scriptC),
	), ErrAuxCloseTemplate)
}

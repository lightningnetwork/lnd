package chancloser

import (
	"bytes"
	"fmt"
	"maps"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/mempool"
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
				Value:    1000,
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
	}), nil
}

func (m *rbfMockAuxCloser) FinalizeClose(types.AuxCloseDesc,
	*wire.MsgTx) error {

	return nil
}

func (m *rbfMockAuxCloser) SupportsRbfClose(lnwire.ChannelID,
	route.Vertex) bool {

	return true
}

// auxHarnessCfg returns a harness config with the given aux closer wired in,
// along with a chan info and internal key lookup.
func auxHarnessCfg(auxCloser *rbfMockAuxCloser, initiator bool,
	internalKey *btcec.PublicKey) *harnessCfg {

	return &harnessCfg{
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

// auxCloseTerms returns close terms for a channel that has exchanged aux
// shutdown records.
func auxCloseTerms(localBalance,
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

// TestRbfAuxLocalOffer asserts that when we act as the closer, the aux
// outputs are part of the fee estimate and the close transaction, and that
// the aux closer is told that we pay the fee.
func TestRbfAuxLocalOffer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	internalKey := randPubKey(t)

	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)
	absoluteFee := btcutil.Amount(10_100)
	balanceAfterClose := localBalance.ToSatoshis() - absoluteFee

	newStartingState := func() *ClosingNegotiation {
		closeTerms := auxCloseTerms(localBalance, remoteBalance)

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
		cfg.initialState = fn.Some[ProtocolState](newStartingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		// The fee estimate must account for the aux output shape.
		closeHarness.expectFeeEstimateWithAuxShape(
			absoluteFee, auxCloser.shape, 1,
		)
		closeHarness.expectNewCloseSig(
			localAddr, remoteAddr, absoluteFee, balanceAfterClose,
		)
		closeHarness.expectMsgSent(
			singleMsgMatcher[*lnwire.ClosingComplete](nil),
		)

		closeHarness.chanCloser.SendEvent(ctx, sendOfferEvent)
		closeHarness.assertStateTransitions(&ClosingNegotiation{})

		currentState := assertStateT[*ClosingNegotiation](closeHarness)
		offerSentState, ok := currentState.PeerState.GetForParty(
			lntypes.Local,
		).(*LocalOfferSent)
		require.True(t, ok)

		// The aux outputs we signed for are stashed in the state.
		require.Equal(
			t, auxCloser.outputs,
			offerSentState.AuxOutputs.UnsafeFromSome().ExtraCloseOutputs, //nolint:ll
		)

		// The aux closer was asked for the outputs with the proposed
		// fee, with us as the payer, and with the close outputs
		// carrying both parties' shutdown records.
		require.Len(t, auxCloser.closeDescs, 1)
		desc := auxCloser.closeDescs[0]
		assertAuxShutdownReq(
			t, closeHarness, desc.AuxShutdownReq, true, internalKey,
		)
		require.Equal(t, absoluteFee, desc.CloseFee)
		require.Equal(t, btcutil.Amount(500), desc.CommitFee)
		require.Equal(t, fn.Some(lntypes.Local), desc.FeePayer)

		localOut := desc.LocalCloseOutput.UnsafeFromSome()
		require.Equal(t, localBalance.ToSatoshis(), localOut.Amt)
		require.EqualValues(t, localAddr, localOut.PkScript)
		require.Equal(t, auxLocalRecords, localOut.ShutdownRecords)

		remoteOut := desc.RemoteCloseOutput.UnsafeFromSome()
		require.Equal(t, remoteBalance.ToSatoshis(), remoteOut.Amt)
		require.EqualValues(t, remoteAddr, remoteOut.PkScript)
		require.Equal(t, auxRemoteRecords, remoteOut.ShutdownRecords)

		// Once the remote party signs, the very same aux outputs are
		// used to complete the close, without asking the aux closer
		// again.
		closeHarness.expectCloseFinalized(
			&localSig, &remoteSig, localAddr, remoteAddr,
			absoluteFee, balanceAfterClose, true,
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
		closeHarness.assertLocalClosePending()

		currentState = assertStateT[*ClosingNegotiation](closeHarness)
		pendingState, ok := currentState.PeerState.GetForParty(
			lntypes.Local,
		).(*ClosePending)
		require.True(t, ok)
		require.Equal(
			t, auxCloser.outputs,
			pendingState.AuxOutputs.UnsafeFromSome().ExtraCloseOutputs, //nolint:ll
		)
		require.Len(t, auxCloser.closeDescs, 1)
	})

	// If the aux closer returns outputs that don't match the shape used
	// for the fee estimate, we refuse to sign.
	t.Run("shape_mismatch", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		auxCloser.outputs = append(
			auxCloser.outputs, auxCloser.outputs[0],
		)

		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.initialState = fn.Some[ProtocolState](newStartingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectFeeEstimateWithAuxShape(
			absoluteFee, auxCloser.shape, 1,
		)
		closeHarness.sendEventAndExpectFailure(
			ctx, sendOfferEvent, ErrAuxShapeMismatch,
		)
		closeHarness.assertNoStateTransitions()
	})

	// If the aux closer fails, we don't sign either.
	t.Run("aux_closer_error", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		auxCloser.outputsErr = errAuxCloser

		cfg := auxHarnessCfg(auxCloser, true, internalKey)
		cfg.initialState = fn.Some[ProtocolState](newStartingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.expectFeeEstimateWithAuxShape(
			absoluteFee, auxCloser.shape, 1,
		)
		closeHarness.sendEventAndExpectFailure(
			ctx, sendOfferEvent, errAuxCloser,
		)
		closeHarness.assertNoStateTransitions()
	})
}

// TestRbfAuxFlushedFeeEstimate asserts that the fee estimate made when the
// channel is flushed accounts for the aux output shape.
func TestRbfAuxFlushedFeeEstimate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	internalKey := randPubKey(t)
	absoluteFee := btcutil.Amount(10_100)

	auxCloser := newRbfMockAuxCloser()
	cfg := auxHarnessCfg(auxCloser, true, internalKey)
	cfg.initialState = fn.Some[ProtocolState](&ChannelFlushing{
		ShutdownScripts: ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		},
		ShutdownCustomRecords: ShutdownCustomRecords{
			LocalCustomRecords:  auxLocalRecords,
			RemoteCustomRecords: auxRemoteRecords,
		},
	})

	closeHarness := newCloser(t, cfg)
	defer closeHarness.stopAndAssert()

	// With a local balance that can't cover the fee, the only side effect
	// of the flushed event is the fee estimate itself.
	closeHarness.expectFeeEstimateWithAuxShape(
		absoluteFee, auxCloser.shape, 1,
	)
	closeHarness.chanCloser.SendEvent(ctx, &ChannelFlushed{
		ShutdownBalances: ShutdownBalances{
			LocalBalance:  lnwire.NewMSatFromSatoshis(1_000),
			RemoteBalance: lnwire.NewMSatFromSatoshis(50_000),
		},
	})
	closeHarness.assertStateTransitions(&ClosingNegotiation{})

	// The records made it into the close terms.
	currentState := assertStateT[*ClosingNegotiation](closeHarness)
	require.Equal(t, auxLocalRecords, currentState.LocalCustomRecords)
	require.Equal(t, auxRemoteRecords, currentState.RemoteCustomRecords)
}

// TestRbfAuxRemoteOffer asserts that when the remote party is the closer,
// the aux outputs are part of the transaction we counter sign, with the
// remote party as the fee payer.
func TestRbfAuxRemoteOffer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	internalKey := randPubKey(t)

	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)
	absoluteFee := btcutil.Amount(10_100)
	balanceAfterClose := remoteBalance.ToSatoshis() - absoluteFee

	newStartingState := func() *ClosingNegotiation {
		closeTerms := auxCloseTerms(localBalance, remoteBalance)

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
		cfg.initialState = fn.Some[ProtocolState](newStartingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.assertSingleRemoteRbfIteration(
			newOffer(remoteAddr), balanceAfterClose, absoluteFee,
			uint32(mempool.MaxRBFSequence), false, true,
		)

		currentState := assertStateT[*ClosingNegotiation](closeHarness)
		pendingState, ok := currentState.PeerState.GetForParty(
			lntypes.Remote,
		).(*ClosePending)
		require.True(t, ok)
		require.Equal(
			t, auxCloser.outputs,
			pendingState.AuxOutputs.UnsafeFromSome().ExtraCloseOutputs, //nolint:ll
		)

		// The aux closer was asked once, with the remote party's fee
		// and the remote party as the payer.
		require.Len(t, auxCloser.closeDescs, 1)
		desc := auxCloser.closeDescs[0]
		assertAuxShutdownReq(
			t, closeHarness, desc.AuxShutdownReq, false,
			internalKey,
		)
		require.Equal(t, absoluteFee, desc.CloseFee)
		require.Equal(t, fn.Some(lntypes.Remote), desc.FeePayer)
		require.Equal(
			t, auxRemoteRecords,
			desc.RemoteCloseOutput.UnsafeFromSome().ShutdownRecords,
		)
	})

	// The remote party can't switch its delivery script within the
	// negotiation, as its aux records are bound to the script it sent
	// them with.
	t.Run("script_change_rejected", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		cfg := auxHarnessCfg(auxCloser, false, internalKey)
		cfg.initialState = fn.Some[ProtocolState](newStartingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		newRemoteAddr := lnwire.DeliveryAddress(append(
			[]byte{0x51, 0x20}, bytes.Repeat([]byte{0x04}, 32)...,
		))
		closeHarness.sendEventAndExpectFailure(
			ctx, newOffer(newRemoteAddr), ErrAuxScriptChange,
		)
		closeHarness.assertNoStateTransitions()
		require.Empty(t, auxCloser.closeDescs)
	})

	// If the aux closer fails, we don't counter sign.
	t.Run("aux_closer_error", func(t *testing.T) {
		auxCloser := newRbfMockAuxCloser()
		auxCloser.outputsErr = errAuxCloser

		cfg := auxHarnessCfg(auxCloser, false, internalKey)
		cfg.initialState = fn.Some[ProtocolState](newStartingState())

		closeHarness := newCloser(t, cfg)
		defer closeHarness.stopAndAssert()

		closeHarness.sendEventAndExpectFailure(
			ctx, newOffer(remoteAddr), errAuxCloser,
		)
		closeHarness.assertNoStateTransitions()
	})
}

// TestRbfAuxCloseFin asserts that the terminal state carries the close
// outputs of whichever negotiated transaction confirmed.
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

	localAuxOutputs := fn.Some(AuxCloseOutputs{
		ExtraCloseOutputs: []lnwallet.CloseOutput{{
			TxOut: wire.TxOut{PkScript: auxExtraScript, Value: 1},
		}},
	})
	remoteAuxOutputs := fn.Some(AuxCloseOutputs{
		ExtraCloseOutputs: []lnwallet.CloseOutput{{
			TxOut: wire.TxOut{PkScript: auxExtraScript, Value: 2},
		}},
	})

	newStartingState := func() *ClosingNegotiation {
		closeTerms := auxCloseTerms(localBalance, remoteBalance)

		return &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &ClosePending{
					CloseTx:           localTx,
					CloseChannelTerms: closeTerms,
					Party:             lntypes.Local,
					AuxOutputs:        localAuxOutputs,
				},
				Remote: &ClosePending{
					CloseTx:           remoteTx,
					CloseChannelTerms: closeTerms,
					Party:             lntypes.Remote,
					AuxOutputs:        remoteAuxOutputs,
				},
			},
			CloseChannelTerms: closeTerms,
		}
	}

	tests := []struct {
		name        string
		confirmedTx *wire.MsgTx
		auxOutputs  fn.Option[AuxCloseOutputs]
		haveOutputs bool
	}{
		{
			name:        "local tx confirmed",
			confirmedTx: localTx,
			auxOutputs:  localAuxOutputs,
			haveOutputs: true,
		},
		{
			name:        "remote tx confirmed",
			confirmedTx: remoteTx,
			auxOutputs:  remoteAuxOutputs,
			haveOutputs: true,
		},
		{
			name:        "unknown tx confirmed",
			confirmedTx: unknownTx,
			auxOutputs:  fn.None[AuxCloseOutputs](),
			haveOutputs: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auxCloser := newRbfMockAuxCloser()
			cfg := auxHarnessCfg(auxCloser, true, internalKey)
			cfg.initialState = fn.Some[ProtocolState](
				newStartingState(),
			)

			closeHarness := newCloser(t, cfg)
			defer closeHarness.stopAndAssert()

			closeHarness.chanCloser.SendEvent(
				ctx, &SpendEvent{Tx: test.confirmedTx},
			)
			closeHarness.assertStateTransitions(&CloseFin{})

			closeFin := assertStateT[*CloseFin](closeHarness)
			require.Equal(t, test.confirmedTx, closeFin.ConfirmedTx)
			require.Equal(t, test.auxOutputs, closeFin.AuxOutputs)

			if !test.haveOutputs {
				require.True(
					t, closeFin.LocalCloseOutput.IsNone(),
				)
				require.True(
					t, closeFin.RemoteCloseOutput.IsNone(),
				)

				return
			}

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
			terms := auxCloseTerms(localBalance, remoteBalance)

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
// the closer if our output stays above dust after paying the fee, as our
// aux output hangs off of it.
func TestRbfAuxLocalCanPayFees(t *testing.T) {
	t.Parallel()

	const (
		dustLimit   btcutil.Amount = 354
		absoluteFee btcutil.Amount = 1_000
	)

	localShape := fn.Some(AuxCloseShape{
		Outputs: []AuxCloseOutputShape{{IsLocal: true}},
	})
	remoteShape := fn.Some(AuxCloseShape{
		Outputs: []AuxCloseOutputShape{{IsLocal: false}},
	})

	tests := []struct {
		name         string
		localBalance btcutil.Amount
		initiator    bool
		commitFee    btcutil.Amount
		shape        fn.Option[AuxCloseShape]
		canPay       bool
	}{
		{
			name:         "can't afford fee",
			localBalance: absoluteFee - 1,
			shape:        localShape,
			canPay:       false,
		},
		{
			name:         "no aux outputs, dust after fee",
			localBalance: absoluteFee + dustLimit - 1,
			shape:        fn.None[AuxCloseShape](),
			canPay:       true,
		},
		{
			name:         "only remote aux output, dust after fee",
			localBalance: absoluteFee + dustLimit - 1,
			shape:        remoteShape,
			canPay:       true,
		},
		{
			name:         "local aux output, dust after fee",
			localBalance: absoluteFee + dustLimit - 1,
			shape:        localShape,
			canPay:       false,
		},
		{
			name:         "local aux output, above dust after fee",
			localBalance: absoluteFee + dustLimit,
			shape:        localShape,
			canPay:       true,
		},
		{
			name:         "initiator regains commit fee",
			localBalance: absoluteFee + dustLimit - 1,
			initiator:    true,
			commitFee:    1,
			shape:        localShape,
			canPay:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := &Environment{
				ChanInfo: &rbfMockChanInfo{
					initiator: tc.initiator,
					commitFee: tc.commitFee,
				},
			}
			terms := auxCloseTerms(
				lnwire.NewMSatFromSatoshis(tc.localBalance),
				lnwire.NewMSatFromSatoshis(50_000),
			)

			canPay := env.localCanPayFees(
				terms, absoluteFee, tc.shape,
			)
			require.Equal(t, tc.canPay, canPay)
		})
	}
}

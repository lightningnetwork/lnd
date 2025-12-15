package chancloser

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	localAddr = lnwire.DeliveryAddress(append(
		[]byte{txscript.OP_1, txscript.OP_DATA_32},
		bytes.Repeat([]byte{0x01}, 32)...,
	))

	remoteAddr = lnwire.DeliveryAddress(append(
		[]byte{txscript.OP_1, txscript.OP_DATA_32},
		bytes.Repeat([]byte{0x02}, 32)...,
	))

	localSigBytes = fromHex("3045022100cd496f2ab4fe124f977ffe3caa09f757" +
		"6d8a34156b4e55d326b4dffc0399a094022013500a0510b5094bff220c7" +
		"4656879b8ca0369d3da78004004c970790862fc03")
	localSig     = sigMustParse(localSigBytes)
	localSigWire = mustWireSig(&localSig)

	remoteSigBytes = fromHex("304502210082235e21a2300022738dabb8e1bbd9d1" +
		"9cfb1e7ab8c30a23b0afbb8d178abcf3022024bf68e256c534ddfaf966b" +
		"f908deb944305596f7bdcc38d69acad7f9c868724")
	remoteSig            = sigMustParse(remoteSigBytes)
	remoteWireSig        = mustWireSig(&remoteSig)
	remoteSigRecordType3 = newSigTlv[tlv.TlvType3](remoteWireSig)
	remoteSigRecordType1 = newSigTlv[tlv.TlvType1](remoteWireSig)

	localTx = wire.MsgTx{Version: 2}

	closeTx = wire.NewMsgTx(2)

	defaultTimeout = 500 * time.Millisecond
	longTimeout    = 3 * time.Second
	defaultPoll    = 50 * time.Millisecond
)

func sigMustParse(sigBytes []byte) ecdsa.Signature {
	sig, err := ecdsa.ParseSignature(sigBytes)
	if err != nil {
		panic(err)
	}

	return *sig
}

func mustWireSig(e input.Signature) lnwire.Sig {
	wireSig, err := lnwire.NewSigFromSignature(e)
	if err != nil {
		panic(err)
	}

	return wireSig
}

func fromHex(s string) []byte {
	r, err := hex.DecodeString(s)
	if err != nil {
		panic("invalid hex in source file: " + s)
	}

	return r
}

func randOutPoint(t *testing.T) wire.OutPoint {
	var op wire.OutPoint
	if _, err := rand.Read(op.Hash[:]); err != nil {
		t.Fatalf("unable to generate random outpoint: %v", err)
	}
	op.Index = rand.Uint32()

	return op
}

func randPubKey(t *testing.T) *btcec.PublicKey {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatalf("unable to generate private key: %v", err)
	}

	return priv.PubKey()
}

func assertStateTransitions[Event any, Env protofsm.Environment](
	t *testing.T, stateSub protofsm.StateSubscriber[Event, Env],
	expectedStates []protofsm.State[Event, Env]) {

	t.Helper()

	for _, expectedState := range expectedStates {
		newState, err := fn.RecvOrTimeout(
			stateSub.NewItemCreated.ChanOut(), defaultTimeout,
		)
		require.NoError(t, err, "expected state: %T", expectedState)

		require.IsType(t, expectedState, newState)
	}

	// We should have no more states.
	select {
	case newState := <-stateSub.NewItemCreated.ChanOut():
		t.Fatalf("unexpected state transition: %v", newState)
	case <-time.After(defaultPoll):
	}
}

// unknownEvent is a dummy event that is used to test that the state machine
// transitions properly fail when an unknown event is received.
type unknownEvent struct {
}

func (u *unknownEvent) protocolSealed() {
}

// assertUnknownEventFail asserts that the state machine fails as expected
// given an unknown event.
func assertUnknownEventFail(t *testing.T, startingState ProtocolState) {
	t.Helper()

	// Any other event should be ignored.
	t.Run("unknown_event", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some(startingState),
		})
		defer closeHarness.stopAndAssert()

		closeHarness.sendEventAndExpectFailure(
			t.Context(), &unknownEvent{},
			ErrInvalidStateTransition,
		)
	})
}

// assertSpendEventCloseFin asserts that the state machine transitions to the
// CloseFin state when a spend event is received.
func assertSpendEventCloseFin(t *testing.T, startingState ProtocolState) {
	t.Helper()

	// If a spend event is received, the state machine should transition to
	// the CloseFin state.
	t.Run("spend_event", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some(startingState),
		})
		defer closeHarness.stopAndAssert()

		closeHarness.chanCloser.SendEvent(
			t.Context(), &SpendEvent{},
		)

		closeHarness.assertStateTransitions(&CloseFin{})
	})
}

type harnessCfg struct {
	initialState fn.Option[ProtocolState]

	thawHeight fn.Option[uint32]

	localUpfrontAddr  fn.Option[lnwire.DeliveryAddress]
	remoteUpfrontAddr fn.Option[lnwire.DeliveryAddress]
}

// rbfCloserTestHarness is a test harness for the RBF closer.
type rbfCloserTestHarness struct {
	*testing.T

	cfg *harnessCfg

	chanCloser *RbfChanCloser
	env        Environment

	newAddrErr error

	pkScript []byte
	peerPub  btcec.PublicKey

	startingState RbfState

	feeEstimator   *mockFeeEstimator
	chanObserver   *mockChanObserver
	signer         *mockCloseSigner
	daemonAdapters *dummyAdapters
	errReporter    *mockErrorReporter

	stateSub protofsm.StateSubscriber[ProtocolEvent, *Environment]
}

var errfailAddr = fmt.Errorf("fail")

// failNewAddrFunc causes the newAddrFunc to fail.
func (r *rbfCloserTestHarness) failNewAddrFunc() {
	r.T.Helper()

	r.newAddrErr = errfailAddr
}

func (r *rbfCloserTestHarness) newAddrFunc() (
	lnwire.DeliveryAddress, error) {

	r.T.Helper()

	return lnwire.DeliveryAddress{}, r.newAddrErr
}

func (r *rbfCloserTestHarness) assertExpectations() {
	r.T.Helper()

	r.feeEstimator.AssertExpectations(r.T)
	r.chanObserver.AssertExpectations(r.T)
	r.daemonAdapters.AssertExpectations(r.T)
	r.errReporter.AssertExpectations(r.T)
	r.signer.AssertExpectations(r.T)
}

// sendEventAndExpectFailure is a helper to expect a failure, send an event,
// and wait for the error report.
func (r *rbfCloserTestHarness) sendEventAndExpectFailure(
	ctx context.Context, event ProtocolEvent, expectedErr error) {

	r.T.Helper()

	r.expectFailure(expectedErr)

	r.chanCloser.SendEvent(ctx, event)

	select {
	case reportedErr := <-r.errReporter.errorReported:
		r.T.Logf("Test received error report: %v", reportedErr)

		if !errors.Is(reportedErr, expectedErr) {
			r.T.Fatalf("reported error (%v) is not the "+
				"expected error (%v)", reportedErr, expectedErr)
		}

	case <-time.After(1 * time.Second):
		r.T.Fatalf("timed out waiting for error to be "+
			"reported by mockErrorReporter for event %T", event)
	}
}
func (r *rbfCloserTestHarness) stopAndAssert() {
	r.T.Helper()

	defer r.chanCloser.RemoveStateSub(r.stateSub)
	r.chanCloser.Stop()

	r.assertExpectations()
}

func (r *rbfCloserTestHarness) assertStartupAssertions() {
	r.T.Helper()

	// When the state machine has started up, we recv a starting state
	// transition for the initial state.
	expectedStates := []RbfState{r.startingState}
	assertStateTransitions(r.T, r.stateSub, expectedStates)

	// Registering the spend transaction should have been called.
	r.daemonAdapters.AssertCalled(
		r.T, "RegisterSpendNtfn", &r.env.ChanPoint, r.pkScript,
		r.env.Scid.BlockHeight,
	)
}

func (r *rbfCloserTestHarness) assertNoStateTransitions() {
	r.T.Helper()

	select {
	case newState := <-r.stateSub.NewItemCreated.ChanOut():
		r.T.Fatalf("unexpected state transition: %T", newState)
	case <-time.After(defaultPoll):
	}
}

func (r *rbfCloserTestHarness) assertStateTransitions(states ...RbfState) {
	r.T.Helper()

	assertStateTransitions(r.T, r.stateSub, states)
}

func (r *rbfCloserTestHarness) currentState() RbfState {
	state, err := r.chanCloser.CurrentState()
	require.NoError(r.T, err)

	return state
}

type shutdownExpect struct {
	isInitiator   bool
	allowSend     bool
	recvShutdown  bool
	finalBalances fn.Option[ShutdownBalances]
}

func (r *rbfCloserTestHarness) expectShutdownEvents(expect shutdownExpect) {
	r.T.Helper()

	r.chanObserver.On("FinalBalances").Return(expect.finalBalances)

	if expect.isInitiator {
		r.chanObserver.On("NoDanglingUpdates").Return(expect.allowSend)
	} else {
		r.chanObserver.On("NoDanglingUpdates").Return(
			expect.allowSend,
		).Maybe()
	}

	// When we're receiving a shutdown, we should also disable incoming
	// adds.
	if expect.recvShutdown {
		r.chanObserver.On("DisableIncomingAdds").Return(nil)
	}

	// If a close is in progress, this is an RBF iteration, the link has
	// already been cleaned up, so we don't expect any assertions, other
	// than to check the closing state.
	if expect.finalBalances.IsSome() {
		return
	}

	r.chanObserver.On("DisableOutgoingAdds").Return(nil)

	r.chanObserver.On(
		"MarkShutdownSent", mock.Anything, expect.isInitiator,
	).Return(nil)

	r.chanObserver.On("DisableChannel").Return(nil)
}

func (r *rbfCloserTestHarness) expectFinalBalances(
	b fn.Option[ShutdownBalances]) {

	r.chanObserver.On("FinalBalances").Return(b)
}

func (r *rbfCloserTestHarness) expectIncomingAddsDisabled() {
	r.T.Helper()

	r.chanObserver.On("DisableIncomingAdds").Return(nil)
}

type msgMatcher func([]lnwire.Message) bool

func singleMsgMatcher[M lnwire.Message](f func(M) bool) msgMatcher {
	return func(msgs []lnwire.Message) bool {
		if len(msgs) != 1 {
			return false
		}

		wireMsg := msgs[0]

		msg, ok := wireMsg.(M)
		if !ok {
			return false
		}

		if f == nil {
			return true
		}

		return f(msg)
	}
}

func (r *rbfCloserTestHarness) expectMsgSent(matcher msgMatcher) {
	r.T.Helper()

	if matcher == nil {
		r.daemonAdapters.On(
			"SendMessages", r.peerPub, mock.Anything,
		).Return(nil)
	} else {
		r.daemonAdapters.On(
			"SendMessages", r.peerPub, mock.MatchedBy(matcher),
		).Return(nil)
	}
}

func (r *rbfCloserTestHarness) expectFeeEstimate(absoluteFee btcutil.Amount,
	numTimes int) {

	r.T.Helper()

	// TODO(roasbeef): mo assertions for dust case

	r.feeEstimator.On(
		"EstimateFee", mock.Anything, mock.Anything, mock.Anything,
		mock.Anything,
	).Return(absoluteFee, nil).Times(numTimes)
}

func (r *rbfCloserTestHarness) expectFailure(err error) {
	r.T.Helper()

	errorMatcher := func(e error) bool {
		return errors.Is(e, err)
	}

	r.errReporter.On("ReportError", mock.MatchedBy(errorMatcher)).Return()
}

func (r *rbfCloserTestHarness) expectNewCloseSig(
	localScript, remoteScript []byte, fee btcutil.Amount,
	closeBalance btcutil.Amount) {

	r.T.Helper()

	r.signer.On(
		"CreateCloseProposal", fee, localScript, remoteScript,
		mock.Anything,
	).Return(&localSig, &localTx, closeBalance, nil)
}

func (r *rbfCloserTestHarness) waitForMsgSent() {
	r.T.Helper()

	err := wait.Predicate(func() bool {
		return r.daemonAdapters.msgSent.Load()
	}, longTimeout)
	require.NoError(r.T, err)
}

func (r *rbfCloserTestHarness) expectRemoteCloseFinalized(
	localCoopSig, remoteCoopSig input.Signature, localScript,
	remoteScript []byte, fee btcutil.Amount,
	balanceAfterClose btcutil.Amount, isLocal bool) {

	r.expectNewCloseSig(
		localScript, remoteScript, fee, balanceAfterClose,
	)

	r.expectCloseFinalized(
		localCoopSig, remoteCoopSig, localScript, remoteScript,
		fee, balanceAfterClose, isLocal,
	)

	r.expectMsgSent(singleMsgMatcher[*lnwire.ClosingSig](nil))
}

func (r *rbfCloserTestHarness) expectCloseFinalized(
	localCoopSig, remoteCoopSig input.Signature, localScript,
	remoteScript []byte, fee btcutil.Amount,
	balanceAfterClose btcutil.Amount, isLocal bool) {

	// The caller should obtain the final signature.
	r.signer.On("CompleteCooperativeClose",
		localCoopSig, remoteCoopSig, localScript,
		remoteScript, fee, mock.Anything,
	).Return(closeTx, balanceAfterClose, nil)

	// The caller should also mark the transaction as broadcast on disk.
	r.chanObserver.On("MarkCoopBroadcasted", closeTx, isLocal).Return(nil)

	// Finally, we expect that the daemon executor should broadcast the
	// above transaction.
	r.daemonAdapters.On(
		"BroadcastTransaction", closeTx, mock.Anything,
	).Return(nil)
}

func (r *rbfCloserTestHarness) expectChanPendingClose() {
	var nilTx *wire.MsgTx
	r.chanObserver.On("MarkCoopBroadcasted", nilTx, true).Return(nil)
}

func (r *rbfCloserTestHarness) assertLocalClosePending() {
	// We should then remain in the outer close negotiation state.
	r.assertStateTransitions(&ClosingNegotiation{})

	// If we examine the final resting state, we should see that the we're
	// now in the negotiation state still for our local peer state.
	currentState := assertStateT[*ClosingNegotiation](r)

	// From this, we'll assert the resulting peer state, and that the co-op
	// close txn is known.
	closePendingState, ok := currentState.PeerState.GetForParty(
		lntypes.Local,
	).(*ClosePending)
	require.True(r.T, ok)

	require.Equal(r.T, closeTx, closePendingState.CloseTx)
}

type dustExpectation uint

const (
	noDustExpect dustExpectation = iota

	localDustExpect

	remoteDustExpect
)

func (d dustExpectation) String() string {
	switch d {
	case noDustExpect:
		return "no dust"
	case localDustExpect:
		return "local dust"
	case remoteDustExpect:
		return "remote dust"
	default:
		return "unknown"
	}
}

// expectHalfSignerIteration asserts that we carry out 1/2 of the locally
// initiated signer iteration. This constitutes sending a ClosingComplete
// message to the remote party, and all the other intermediate steps.
func (r *rbfCloserTestHarness) expectHalfSignerIteration(
	initEvent ProtocolEvent, balanceAfterClose, absoluteFee btcutil.Amount,
	dustExpect dustExpectation, iteration bool) {

	ctx := r.T.Context()
	numFeeCalls := 2

	// If we're using the SendOfferEvent as a trigger, we only need to call
	// the few estimation once.
	if _, ok := initEvent.(*SendOfferEvent); ok {
		numFeeCalls = 1
	}

	// We'll now expect that fee estimation is called, and then
	// send in the flushed event. We expect two calls: one to
	// figure out if we can pay the fee, and then another when we
	// actually pay the fee.
	r.expectFeeEstimate(absoluteFee, numFeeCalls)

	// Next, we'll assert that we receive calls to generate a new
	// commitment signature, and then send out the commit
	// ClosingComplete message to the remote peer.
	r.expectNewCloseSig(
		localAddr, remoteAddr, absoluteFee, balanceAfterClose,
	)

	// We expect that only the closer and closee sig is set as both
	// parties have a balance.
	msgExpect := singleMsgMatcher(func(m *lnwire.ClosingComplete) bool {
		r.T.Helper()

		switch {
		case m.CloserNoClosee.IsSome():
			r.T.Logf("closer no closee field set, expected: %v",
				dustExpect)

			return dustExpect == remoteDustExpect
		case m.NoCloserClosee.IsSome():
			r.T.Logf("no close closee field set, expected: %v",
				dustExpect)

			return dustExpect == localDustExpect
		default:
			r.T.Logf("no dust field set, expected: %v", dustExpect)

			return (m.CloserAndClosee.IsSome() &&
				dustExpect == noDustExpect)
		}
	})
	r.expectMsgSent(msgExpect)

	r.chanCloser.SendEvent(ctx, initEvent)

	// Based on the init event, we'll either just go to the closing
	// negotiation state, or go through the channel flushing state first.
	var expectedStates []RbfState
	switch initEvent.(type) {
	case *ShutdownReceived:
		expectedStates = []RbfState{
			&ChannelFlushing{}, &ClosingNegotiation{},
		}

	case *SendOfferEvent:
		expectedStates = []RbfState{&ClosingNegotiation{}}

		// If we're in the middle of an iteration, then we expect a
		// transition from ClosePending -> LocalCloseStart.
		if iteration {
			expectedStates = append(
				expectedStates, &ClosingNegotiation{},
			)
		}

	case *ChannelFlushed:
		// If we're sending a flush event here, then this means that we
		// also have enough balance to cover the fee so we'll have
		// another inner transition to the negotiation state.
		expectedStates = []RbfState{
			&ClosingNegotiation{}, &ClosingNegotiation{},
		}

	default:
		r.T.Fatalf("unknown event type: %T", initEvent)
	}

	// We should transition from the negotiation state back to
	// itself.
	r.assertStateTransitions(expectedStates...)

	// If we examine the final resting state, we should see that
	// the we're now in the LocalOffersSent for our local peer
	// state.
	currentState := assertStateT[*ClosingNegotiation](r)

	// From this, we'll assert the resulting peer state.
	offerSentState, ok := currentState.PeerState.GetForParty(
		lntypes.Local,
	).(*LocalOfferSent)
	require.True(r.T, ok)

	// The proposed fee, as well as our local signature should be
	// properly stashed in the state.
	require.Equal(r.T, absoluteFee, offerSentState.ProposedFee)
	require.Equal(r.T, localSigWire, offerSentState.LocalSig)
}

func (r *rbfCloserTestHarness) assertSingleRbfIteration(
	initEvent ProtocolEvent, balanceAfterClose, absoluteFee btcutil.Amount,
	dustExpect dustExpectation, iteration bool) {

	ctx := r.T.Context()

	// We'll now send in the send offer event, which should trigger 1/2 of
	// the RBF loop, ending us in the LocalOfferSent state.
	r.expectHalfSignerIteration(
		initEvent, balanceAfterClose, absoluteFee, dustExpect,
		iteration,
	)

	// Now that we're in the local offer sent state, we'll send the
	// response of the remote party, which completes one iteration
	localSigEvent := &LocalSigReceived{
		SigMsg: lnwire.ClosingSig{
			CloserScript: localAddr,
			CloseeScript: remoteAddr,
			ClosingSigs: lnwire.ClosingSigs{
				CloserAndClosee: newSigTlv[tlv.TlvType3](
					remoteWireSig,
				),
			},
		},
	}

	// Before we send the event, we expect the close the final signature to
	// be combined/obtained, and for the close to finalized on disk.
	r.expectCloseFinalized(
		&localSig, &remoteSig, localAddr, remoteAddr, absoluteFee,
		balanceAfterClose, true,
	)

	r.chanCloser.SendEvent(ctx, localSigEvent)

	// We should transition to the pending closing state now.
	r.assertLocalClosePending()
}

// assertSingleRemoteRbfIteration asserts that a single RBF iteration initiated
// by the remote party completes successfully. The sendEvent callback controls
// when the event that kicks off the process is sent, which is useful for tests
// that need to set up mocks before the event is processed. The callback is
// provided with the context and the initial offer event so most callers can
// pass chanCloser.SendEvent directly.
func (r *rbfCloserTestHarness) assertSingleRemoteRbfIteration(
	initEvent *OfferReceivedEvent, balanceAfterClose,
	absoluteFee btcutil.Amount, sequence uint32, iteration bool,
	sendEvent func(context.Context, ProtocolEvent)) {

	ctx := r.T.Context()

	// When we receive the signature below, our local state machine should
	// move to finalize the close.
	r.expectRemoteCloseFinalized(
		&localSig, &remoteSig, initEvent.SigMsg.CloseeScript,
		initEvent.SigMsg.CloserScript,
		absoluteFee, balanceAfterClose, false,
	)

	sendEvent(ctx, initEvent)

	// Our outer state should transition to ClosingNegotiation state.
	transitions := []RbfState{
		&ClosingNegotiation{},
	}

	// If this is an iteration, then we'll go from ClosePending ->
	// RemoteCloseStart -> ClosePending. So we'll assert an extra transition
	// here.
	if iteration {
		transitions = append(transitions, &ClosingNegotiation{})
	}

	// Now that we know how many state transitions to expect, we'll wait
	// for them.
	r.assertStateTransitions(transitions...)

	// If we examine the final resting state, we should see that the we're
	// now in the ClosePending state for the remote peer.
	currentState := assertStateT[*ClosingNegotiation](r)

	// From this, we'll assert the resulting peer state.
	pendingState, ok := currentState.PeerState.GetForParty(
		lntypes.Remote,
	).(*ClosePending)
	require.True(r.T, ok)

	// The proposed fee, as well as our local signature should be properly
	// stashed in the state.
	require.Equal(r.T, closeTx, pendingState.CloseTx)
}

func assertStateT[T ProtocolState](h *rbfCloserTestHarness) T {
	h.T.Helper()

	currentState, ok := h.currentState().(T)
	require.True(h.T, ok)

	return currentState
}

// newRbfCloserTestHarness creates a new test harness for the RBF closer.
func newRbfCloserTestHarness(t *testing.T,
	cfg *harnessCfg) *rbfCloserTestHarness {

	ctx := t.Context()

	startingHeight := 200

	chanPoint := randOutPoint(t)
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	scid := lnwire.NewShortChanIDFromInt(rand.Uint64())

	peerPub := randPubKey(t)

	msgMapper := NewRbfMsgMapper(uint32(startingHeight), chanID, *peerPub)

	initialState := cfg.initialState.UnwrapOr(&ChannelActive{})

	defaultFeeRate := chainfee.FeePerKwFloor

	feeEstimator := &mockFeeEstimator{}
	mockObserver := &mockChanObserver{}
	errReporter := newMockErrorReporter(t)
	mockSigner := &mockCloseSigner{}

	harness := &rbfCloserTestHarness{
		T:             t,
		cfg:           cfg,
		startingState: initialState,
		feeEstimator:  feeEstimator,
		signer:        mockSigner,
		chanObserver:  mockObserver,
		errReporter:   errReporter,
		peerPub:       *peerPub,
	}

	env := Environment{
		ChainParams:           chaincfg.RegressionNetParams,
		ChanPeer:              *peerPub,
		ChanPoint:             chanPoint,
		ChanID:                chanID,
		Scid:                  scid,
		DefaultFeeRate:        defaultFeeRate.FeePerVByte(),
		ThawHeight:            cfg.thawHeight,
		RemoteUpfrontShutdown: cfg.remoteUpfrontAddr,
		LocalUpfrontShutdown:  cfg.localUpfrontAddr,
		NewDeliveryScript:     harness.newAddrFunc,
		FeeEstimator:          feeEstimator,
		ChanObserver:          mockObserver,
		CloseSigner:           mockSigner,
	}
	harness.env = env

	var pkScript []byte
	harness.pkScript = pkScript

	spendEvent := protofsm.RegisterSpend[ProtocolEvent]{
		OutPoint:       chanPoint,
		HeightHint:     scid.BlockHeight,
		PkScript:       pkScript,
		PostSpendEvent: fn.Some[RbfSpendMapper](SpendMapper),
	}

	daemonAdapters := newDaemonAdapters()
	harness.daemonAdapters = daemonAdapters

	protoCfg := RbfChanCloserCfg{
		ErrorReporter: errReporter,
		Daemon:        daemonAdapters,
		InitialState:  initialState,
		Env:           &env,
		InitEvent:     fn.Some[protofsm.DaemonEvent](&spendEvent),
		MsgMapper: fn.Some[protofsm.MsgMapper[ProtocolEvent]](
			msgMapper,
		),
		CustomPollInterval: fn.Some(defaultPoll),
	}

	// Before we start we always expect an initial spend event.
	daemonAdapters.On(
		"RegisterSpendNtfn", &chanPoint, pkScript, scid.BlockHeight,
	).Return(nil)

	chanCloser := protofsm.NewStateMachine(protoCfg)

	// We register our subscriber before starting the state machine, to make
	// sure we don't miss any events.
	harness.stateSub = chanCloser.RegisterStateEvents()

	chanCloser.Start(ctx)

	harness.chanCloser = &chanCloser

	return harness
}

func newCloser(t *testing.T, cfg *harnessCfg) *rbfCloserTestHarness {
	chanCloser := newRbfCloserTestHarness(t, cfg)

	// We should start in the active state, and have our spend req
	// daemon event handled.
	chanCloser.assertStartupAssertions()

	return chanCloser
}

// TestRbfChannelActiveTransitions tests the transitions of from the
// ChannelActive state.
func TestRbfChannelActiveTransitions(t *testing.T) {
	ctx := t.Context()
	localAddr := lnwire.DeliveryAddress(bytes.Repeat([]byte{0x01}, 20))
	remoteAddr := lnwire.DeliveryAddress(bytes.Repeat([]byte{0x02}, 20))

	feeRate := chainfee.SatPerVByte(1000)

	// Test that if a spend event is received, the FSM transitions to the
	// CloseFin terminal state.
	t.Run("spend_event", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		closeHarness.chanCloser.SendEvent(ctx, &SpendEvent{})

		closeHarness.assertStateTransitions(&CloseFin{})
	})

	// If we send in a local shutdown event, but fail to get an addr, the
	// state machine should terminate.
	t.Run("local_initiated_close_addr_fail", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{})
		defer closeHarness.stopAndAssert()

		closeHarness.failNewAddrFunc()

		// We don't specify an upfront shutdown addr, and don't specify
		// on here in the vent, so we should call new addr, but then
		// fail.
		closeHarness.sendEventAndExpectFailure(
			ctx, &SendShutdown{}, errfailAddr,
		)
		closeHarness.assertNoStateTransitions()
	})

	// Initiating the shutdown should have us transition to the shutdown
	// pending state. We should also emit events to disable the channel,
	// and also send a message to our target peer.
	t.Run("local_initiated_close_ok", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		// Once we send the event below, we should get calls to the
		// chan observer, the msg sender, and the link control.
		closeHarness.expectShutdownEvents(shutdownExpect{
			isInitiator: true,
			allowSend:   true,
		})
		closeHarness.expectMsgSent(nil)

		// If we send the shutdown event, we should transition to the
		// shutdown pending state.
		closeHarness.chanCloser.SendEvent(
			ctx, &SendShutdown{IdealFeeRate: feeRate},
		)
		closeHarness.assertStateTransitions(&ShutdownPending{})

		// If we examine the internal state, it should be consistent
		// with the fee+addr we sent in.
		currentState := assertStateT[*ShutdownPending](closeHarness)

		require.Equal(
			t, feeRate, currentState.IdealFeeRate.UnsafeFromSome(),
		)
		require.Equal(
			t, localAddr,
			currentState.ShutdownScripts.LocalDeliveryScript,
		)

		// Wait till the msg has been sent to assert our expectations.
		//
		// TODO(roasbeef): can use call.WaitFor here?
		closeHarness.waitForMsgSent()
	})

	// If the remote party attempts to close, and a thaw height is active,
	// but not yet met, then we should fail.
	t.Run("remote_initiated_thaw_height_close_fail", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			localUpfrontAddr: fn.Some(localAddr),
			thawHeight:       fn.Some(uint32(100000)),
		})
		defer closeHarness.stopAndAssert()

		// Next, we'll emit the recv event, with the addr of the remote
		// party.
		event := &ShutdownReceived{
			ShutdownScript: remoteAddr,
			BlockHeight:    1,
		}
		closeHarness.sendEventAndExpectFailure(
			ctx, event, ErrThawHeightNotReached,
		)
	})

	// When we receive a shutdown, we should transition to the shutdown
	// pending state, with the local+remote shutdown addrs known.
	t.Run("remote_initiated_close_ok", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		// We assert our shutdown events, and also that we eventually
		// send a shutdown to the remote party. We'll hold back the
		// send in this case though, as we should only send once the no
		// updates are dangling.
		closeHarness.expectShutdownEvents(shutdownExpect{
			isInitiator:  false,
			allowSend:    false,
			recvShutdown: true,
		})

		// Next, we'll emit the recv event, with the addr of the remote
		// party.
		closeHarness.chanCloser.SendEvent(
			ctx, &ShutdownReceived{ShutdownScript: remoteAddr},
		)

		// We should transition to the shutdown pending state.
		closeHarness.assertStateTransitions(&ShutdownPending{})

		currentState := assertStateT[*ShutdownPending](closeHarness)

		// Both the local and remote shutdown scripts should be set.
		require.Equal(
			t, localAddr,
			currentState.ShutdownScripts.LocalDeliveryScript,
		)
		require.Equal(
			t, remoteAddr,
			currentState.ShutdownScripts.RemoteDeliveryScript,
		)
	})

	// Any other event should be ignored.
	assertUnknownEventFail(t, &ChannelActive{})

	// Sending a Spend event should transition to CloseFin.
	assertSpendEventCloseFin(t, &ChannelActive{})
}

// TestRbfShutdownPendingTransitions tests the transitions of the RBF closer
// once we get to the shutdown pending state. In this state, we wait for either
// a shutdown to be received, or a notification that we're able to send a
// shutdown ourselves.
func TestRbfShutdownPendingTransitions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	startingState := &ShutdownPending{}

	// Test that if a spend event is received, the FSM transitions to the
	// CloseFin terminal state.
	t.Run("spend_event", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](
				startingState,
			),
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		closeHarness.chanCloser.SendEvent(ctx, &SpendEvent{})

		closeHarness.assertStateTransitions(&CloseFin{})
	})

	// If the remote party sends us a diff shutdown addr than we expected,
	// then we'll fail.
	t.Run("initiator_shutdown_recv_validate_fail", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](
				startingState,
			),
			remoteUpfrontAddr: fn.Some(remoteAddr),
		})
		defer closeHarness.stopAndAssert()

		// We'll now send in a ShutdownReceived event, but with a
		// different address provided in the shutdown message. This
		// should result in an error.
		event := &ShutdownReceived{
			ShutdownScript: localAddr,
		}

		closeHarness.sendEventAndExpectFailure(
			ctx, event, ErrUpfrontShutdownScriptMismatch,
		)
		closeHarness.assertNoStateTransitions()
	})

	// Otherwise, if the shutdown is well composed, then we should
	// transition to the ChannelFlushing state.
	t.Run("initiator_shutdown_recv_ok", func(t *testing.T) {
		firstState := *startingState
		firstState.IdealFeeRate = fn.Some(
			chainfee.FeePerKwFloor.FeePerVByte(),
		)
		firstState.ShutdownScripts = ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](
				&firstState,
			),
			localUpfrontAddr:  fn.Some(localAddr),
			remoteUpfrontAddr: fn.Some(remoteAddr),
		})
		defer closeHarness.stopAndAssert()

		// We should disable the outgoing adds for the channel at this
		// point as well.
		closeHarness.expectFinalBalances(fn.None[ShutdownBalances]())
		closeHarness.expectIncomingAddsDisabled()

		// We'll send in a shutdown received event, with the expected
		// co-op close addr.
		closeHarness.chanCloser.SendEvent(
			ctx, &ShutdownReceived{ShutdownScript: remoteAddr},
		)

		// We should transition to the channel flushing state.
		closeHarness.assertStateTransitions(&ChannelFlushing{})

		// Now we'll ensure that the flushing state has the proper
		// co-op close state.
		currentState := assertStateT[*ChannelFlushing](closeHarness)

		require.Equal(t, localAddr, currentState.LocalDeliveryScript)
		require.Equal(t, remoteAddr, currentState.RemoteDeliveryScript)
		require.Equal(
			t, firstState.IdealFeeRate, currentState.IdealFeeRate,
		)
	})

	// If we received the shutdown event, then we'll rely on the external
	// signal that we were able to send the message once there were no
	// updates dangling.
	t.Run("responder_complete", func(t *testing.T) {
		firstState := *startingState
		firstState.IdealFeeRate = fn.Some(
			chainfee.FeePerKwFloor.FeePerVByte(),
		)
		firstState.ShutdownScripts = ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](
				&firstState,
			),
		})
		defer closeHarness.stopAndAssert()

		// In this case we're doing the shutdown dance for the first
		// time, so we'll mark the channel as not being flushed.
		closeHarness.expectFinalBalances(fn.None[ShutdownBalances]())

		// We'll send in a shutdown received event.
		closeHarness.chanCloser.SendEvent(ctx, &ShutdownComplete{})

		// We should transition to the channel flushing state.
		closeHarness.assertStateTransitions(&ChannelFlushing{})
	})

	// If we an early offer from the remote party, then we should stash
	// that, transition to the channel flushing state. Once there, another
	// self transition should emit the stashed offer.
	t.Run("early_remote_offer_shutdown_complete", func(t *testing.T) {
		firstState := *startingState
		firstState.IdealFeeRate = fn.Some(
			chainfee.FeePerKwFloor.FeePerVByte(),
		)
		firstState.ShutdownScripts = ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](
				&firstState,
			),
		})
		defer closeHarness.stopAndAssert()

		// In this case we're doing the shutdown dance for the first
		// time, so we'll mark the channel as not being flushed.
		closeHarness.expectFinalBalances(fn.None[ShutdownBalances]())

		// Before we send the shutdown complete event, we'll send in an
		// early offer from the remote party.
		closeHarness.chanCloser.SendEvent(ctx, &OfferReceivedEvent{})

		// This will cause a self transition back to ShutdownPending.
		closeHarness.assertStateTransitions(&ShutdownPending{})

		// Next, we'll send in a shutdown complete event.
		closeHarness.chanCloser.SendEvent(ctx, &ShutdownComplete{})

		// We should transition to the channel flushing state, then the
		// self event to have this state cache he early offer should
		// follow.
		closeHarness.assertStateTransitions(
			&ChannelFlushing{}, &ChannelFlushing{},
		)

		// If we get the current state, we should see that the offer is
		// cached.
		currentState := assertStateT[*ChannelFlushing](closeHarness)
		require.NotNil(t, currentState.EarlyRemoteOffer)
	})

	// If we an early offer from the remote party, then we should stash
	// that, transition to the channel flushing state. Once there, another
	// self transition should emit the stashed offer.
	t.Run("early_remote_offer_shutdown_received", func(t *testing.T) {
		firstState := *startingState
		firstState.IdealFeeRate = fn.Some(
			chainfee.FeePerKwFloor.FeePerVByte(),
		)
		firstState.ShutdownScripts = ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](
				&firstState,
			),
		})
		defer closeHarness.stopAndAssert()

		// In this case we're doing the shutdown dance for the first
		// time, so we'll mark the channel as not being flushed.
		closeHarness.expectFinalBalances(fn.None[ShutdownBalances]())
		closeHarness.expectIncomingAddsDisabled()

		// Before we send the shutdown complete event, we'll send in an
		// early offer from the remote party.
		closeHarness.chanCloser.SendEvent(ctx, &OfferReceivedEvent{})

		// This will cause a self transition back to ShutdownPending.
		closeHarness.assertStateTransitions(&ShutdownPending{})

		// Next, we'll send in a shutdown complete event.
		closeHarness.chanCloser.SendEvent(ctx, &ShutdownReceived{})

		// We should transition to the channel flushing state, then the
		// self event to have this state cache he early offer should
		// follow.
		closeHarness.assertStateTransitions(
			&ChannelFlushing{}, &ChannelFlushing{},
		)

		// If we get the current state, we should see that the offer is
		// cached.
		currentState := assertStateT[*ChannelFlushing](closeHarness)
		require.NotNil(t, currentState.EarlyRemoteOffer)
	})

	// Any other event should be ignored.
	assertUnknownEventFail(t, startingState)

	// Sending a Spend event should transition to CloseFin.
	assertSpendEventCloseFin(t, startingState)
}

// TestRbfChannelFlushingTransitions tests the transitions of the RBF closer
// for the channel flushing state. Once the coast is clear, we should
// transition to the negotiation state.
func TestRbfChannelFlushingTransitions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	localBalance := lnwire.NewMSatFromSatoshis(10_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)

	absoluteFee := btcutil.Amount(10_100)

	startingState := &ChannelFlushing{
		ShutdownScripts: ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		},
	}

	flushTemplate := &ChannelFlushed{
		ShutdownBalances: ShutdownBalances{
			LocalBalance:  localBalance,
			RemoteBalance: remoteBalance,
		},
	}

	// If send in the channel flushed event, but the local party can't pay
	// for fees, then we should just head to the negotiation state.
	for _, isFreshFlush := range []bool{true, false} {
		chanFlushedEvent := *flushTemplate
		chanFlushedEvent.FreshFlush = isFreshFlush

		testName := fmt.Sprintf("local_cannot_pay_for_fee/"+
			"fresh_flush=%v", isFreshFlush)

		t.Run(testName, func(t *testing.T) {
			firstState := *startingState

			closeHarness := newCloser(t, &harnessCfg{
				initialState: fn.Some[ProtocolState](
					&firstState,
				),
			})
			defer closeHarness.stopAndAssert()

			// As part of the set up for this state, we'll have the
			// final absolute fee required be greater than the
			// balance of the local party.
			closeHarness.expectFeeEstimate(absoluteFee, 1)

			// If this is a fresh flush, then we expect the state
			// to be marked on disk.
			if isFreshFlush {
				closeHarness.expectChanPendingClose()
			}

			// We'll now send in the event which should trigger
			// this code path.
			closeHarness.chanCloser.SendEvent(
				ctx, &chanFlushedEvent,
			)

			// With the event sent, we should now transition
			// straight to the ClosingNegotiation state, with no
			// further state transitions.
			closeHarness.assertStateTransitions(
				&ClosingNegotiation{},
			)
		})
	}

	for _, isFreshFlush := range []bool{true, false} {
		flushEvent := *flushTemplate
		flushEvent.FreshFlush = isFreshFlush

		// We'll modify the starting balance to be 3x the required fee
		// to ensure that we can pay for the fee.
		localBalanceMSat := lnwire.NewMSatFromSatoshis(absoluteFee * 3)
		flushEvent.ShutdownBalances.LocalBalance = localBalanceMSat

		testName := fmt.Sprintf("local_can_pay_for_fee/"+
			"fresh_flush=%v", isFreshFlush)

		// This scenario, we'll have the local party be able to pay for
		// the fees, which will trigger additional state transitions.
		t.Run(testName, func(t *testing.T) {
			firstState := *startingState

			closeHarness := newCloser(t, &harnessCfg{
				initialState: fn.Some[ProtocolState](
					&firstState,
				),
			})
			defer closeHarness.stopAndAssert()

			localBalance := flushEvent.ShutdownBalances.LocalBalance
			balanceAfterClose := localBalance.ToSatoshis() -
				absoluteFee

			// If this is a fresh flush, then we expect the state
			// to be marked on disk.
			if isFreshFlush {
				closeHarness.expectChanPendingClose()
			}

			// From here, we expect the state transition to go
			// back to closing negotiated, for a ClosingComplete
			// message to be sent and then for us to terminate at
			// that state. This is 1/2 of the normal RBF signer
			// flow.
			closeHarness.expectHalfSignerIteration(
				&flushEvent, balanceAfterClose, absoluteFee,
				noDustExpect, false,
			)
		})
	}

	// This tests that if we receive an `OfferReceivedEvent` while in the
	// flushing state, then we'll cache that, and once we receive
	// ChannelFlushed, we'll emit an internal `OfferReceivedEvent` in the
	// negotiation state.
	t.Run("early_offer", func(t *testing.T) {
		firstState := *startingState

		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](
				&firstState,
			),
		})
		defer closeHarness.stopAndAssert()

		flushEvent := *flushTemplate

		// Set up the fee estimate s.t the local party doesn't have
		// balance to close.
		closeHarness.expectFeeEstimate(absoluteFee, 1)

		// First, we'll emit an `OfferReceivedEvent` to simulate an
		// early offer (async network, they determine the channel is
		// "flushed" before we do, and send their offer over).
		remoteOffer := &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				FeeSatoshis:  absoluteFee,
				CloserScript: remoteAddr,
				CloseeScript: localAddr,
				ClosingSigs: lnwire.ClosingSigs{
					CloserAndClosee: remoteSigRecordType3,
				},
			},
		}

		closeHarness.chanCloser.SendEvent(ctx, remoteOffer)

		// We should do a self transition, and still be in the
		// ChannelFlushing state.
		closeHarness.assertStateTransitions(&ChannelFlushing{})

		sequence := uint32(mempool.MaxRBFSequence)

		// Now we'll send in the channel flushed event, and assert that
		// this triggers a remote RBF iteration (we process their early
		// offer and send our sig).
		closeHarness.assertSingleRemoteRbfIteration(
			remoteOffer, absoluteFee, absoluteFee, sequence, true,
			func(ctx context.Context, _ ProtocolEvent) {
				closeHarness.chanCloser.SendEvent(
					ctx, &flushEvent,
				)
			},
		)
	})

	// Any other event should be ignored.
	assertUnknownEventFail(t, startingState)

	// Sending a Spend event should transition to CloseFin.
	assertSpendEventCloseFin(t, startingState)
}

// TestRbfCloseClosingNegotiationLocal tests the local portion of the primary
// RBF close loop. We should be able to transition to a close state, get a sig,
// then restart all over again to re-request a signature of at new higher fee
// rate.
func TestRbfCloseClosingNegotiationLocal(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)

	absoluteFee := btcutil.Amount(10_100)

	closeTerms := &CloseChannelTerms{
		ShutdownBalances: ShutdownBalances{
			LocalBalance:  localBalance,
			RemoteBalance: remoteBalance,
		},
		ShutdownScripts: ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		},
	}
	startingState := &ClosingNegotiation{
		PeerState: lntypes.Dual[AsymmetricPeerState]{
			Local: &LocalCloseStart{
				CloseChannelTerms: closeTerms,
			},
		},
		CloseChannelTerms: closeTerms,
	}

	sendOfferEvent := &SendOfferEvent{
		TargetFeeRate: chainfee.FeePerKwFloor.FeePerVByte(),
	}

	balanceAfterClose := localBalance.ToSatoshis() - absoluteFee

	// TODO(roasbeef): add test case for error state validation, then resume

	// In this state, we'll simulate deciding that we need to send a new
	// offer to the remote party.
	t.Run("send_offer_iteration_no_dust", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](startingState),
		})
		defer closeHarness.stopAndAssert()

		// We'll now send in the initial sender offer event, which
		// should then trigger a single RBF iteration, ending at the
		// pending state.
		closeHarness.assertSingleRbfIteration(
			sendOfferEvent, balanceAfterClose, absoluteFee,
			noDustExpect, false,
		)
	})

	// We'll run a similar test as above, but verify that if more than one
	// sig field is set, we error out.
	t.Run("send_offer_too_many_sigs_received", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](startingState),
		})
		defer closeHarness.stopAndAssert()

		// We'll kick off the test as normal, triggering a send offer
		// event to advance the state machine.
		closeHarness.expectHalfSignerIteration(
			sendOfferEvent, balanceAfterClose, absoluteFee,
			noDustExpect, false,
		)

		// We'll now craft the local sig received event, but this time
		// we'll specify 2 signature fields.
		localSigEvent := &LocalSigReceived{
			SigMsg: lnwire.ClosingSig{
				CloserScript: localAddr,
				CloseeScript: remoteAddr,
				ClosingSigs: lnwire.ClosingSigs{
					CloserNoClosee: newSigTlv[tlv.TlvType1](
						remoteWireSig,
					),
					CloserAndClosee: remoteSigRecordType3,
				},
			},
		}

		closeHarness.sendEventAndExpectFailure(
			ctx, localSigEvent, ErrTooManySigs,
		)
	})

	// Next, we'll verify that if the balance of the remote party is dust,
	// then the proper sig field is set.
	t.Run("send_offer_iteration_remote_dust", func(t *testing.T) {
		// We'll modify the starting state to reduce the balance of the
		// remote party to something that'll be dust.
		newCloseTerms := *closeTerms
		newCloseTerms.ShutdownBalances.RemoteBalance = 100
		firstState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: &newCloseTerms,
				},
			},
			CloseChannelTerms: &newCloseTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](firstState),
		})
		defer closeHarness.stopAndAssert()

		// We'll kick off the half iteration as normal, but this time
		// we expect that the remote party's output is dust, so the
		// proper field is set.
		closeHarness.expectHalfSignerIteration(
			sendOfferEvent, balanceAfterClose, absoluteFee,
			remoteDustExpect, false,
		)
	})

	// Similarly, we'll verify that if our final closing balance is dust,
	// then we send the sig that omits our output.
	t.Run("send_offer_iteration_local_dust", func(t *testing.T) {
		firstState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: closeTerms,
				},
			},
			CloseChannelTerms: closeTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](firstState),
		})
		defer closeHarness.stopAndAssert()

		// We'll kick off the half iteration as normal, but this time
		// we'll have our balance after close be dust, so we expect
		// that the local output is dust in the sig we send.
		dustBalance := btcutil.Amount(100)
		closeHarness.expectHalfSignerIteration(
			sendOfferEvent, dustBalance, absoluteFee,
			localDustExpect, false,
		)
	})

	// In this test, we'll assert that we're able to restart the RBF loop
	// to trigger additional signature iterations.
	t.Run("send_offer_rbf_wrong_local_script", func(t *testing.T) {
		firstState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: closeTerms,
				},
			},
			CloseChannelTerms: closeTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState:     fn.Some[ProtocolState](firstState),
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		// The remote party will send a ClosingSig message, but with the
		// wrong local script. We should expect an error.
		// We'll send this message in directly, as we shouldn't get any
		// further in the process.
		// assuming we start in this negotiation state.
		localSigEvent := &LocalSigReceived{
			SigMsg: lnwire.ClosingSig{
				CloserScript: remoteAddr,
				CloseeScript: remoteAddr,
				ClosingSigs: lnwire.ClosingSigs{
					CloserAndClosee: remoteSigRecordType3,
				},
			},
		}
		closeHarness.sendEventAndExpectFailure(
			ctx, localSigEvent, ErrWrongLocalScript,
		)
	})

	// In this test, we'll assert that we're able to restart the RBF loop
	// to trigger additional signature iterations.
	t.Run("send_offer_rbf_iteration_loop", func(t *testing.T) {
		firstState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: closeTerms,
				},
			},
			CloseChannelTerms: closeTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState:     fn.Some[ProtocolState](firstState),
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		// We'll start out by first triggering a routine iteration,
		// assuming we start in this negotiation state.
		closeHarness.assertSingleRbfIteration(
			sendOfferEvent, balanceAfterClose, absoluteFee,
			noDustExpect, false,
		)

		// Next, we'll send in a new SendOfferEvent event which
		// simulates the user requesting a RBF fee bump. We'll use 10x
		// the fee we used in the last iteration.
		rbfFeeBump := chainfee.FeePerKwFloor.FeePerVByte() * 10
		localOffer := &SendOfferEvent{
			TargetFeeRate: rbfFeeBump,
		}

		// Now we expect that another full RBF iteration takes place (we
		// initiate a new local sig).
		closeHarness.assertSingleRbfIteration(
			localOffer, balanceAfterClose, absoluteFee,
			noDustExpect, true,
		)
	})

	// Make sure that we'll go to the error state if we try to try a close
	// that we can't pay for.
	t.Run("send_offer_cannot_pay_for_fees", func(t *testing.T) {
		firstState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: closeTerms,
				},
			},
			CloseChannelTerms: closeTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState:     fn.Some[ProtocolState](firstState),
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		// We'll prep to return an absolute fee that's much higher than
		// the amount we have in the channel.
		closeHarness.expectFeeEstimate(btcutil.SatoshiPerBitcoin, 1)

		rbfFeeBump := chainfee.FeePerKwFloor.FeePerVByte()
		localOffer := &SendOfferEvent{
			TargetFeeRate: rbfFeeBump,
		}

		// Next, we'll send in this event, which should fail as we can't
		// actually pay for fees.
		closeHarness.chanCloser.SendEvent(ctx, localOffer)

		// We should transition to the CloseErr (within
		// ClosingNegotiation) state.
		closeHarness.assertStateTransitions(&ClosingNegotiation{})

		// If we get the state, we should see the expected ErrState.
		currentState := assertStateT[*ClosingNegotiation](closeHarness)

		closeErrState, ok := currentState.PeerState.GetForParty(
			lntypes.Local,
		).(*CloseErr)
		require.True(t, ok)
		require.IsType(
			t, &ErrStateCantPayForFee{}, closeErrState.ErrState,
		)
	})

	// Any other event should be ignored.
	assertUnknownEventFail(t, startingState)

	// Sending a Spend event should transition to CloseFin.
	assertSpendEventCloseFin(t, startingState)
}

// TestRbfCloseClosingNegotiationRemote tests that state machine is able to
// handle RBF iterations to sign for the closing transaction of the remote
// party.
func TestRbfCloseClosingNegotiationRemote(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)

	absoluteFee := btcutil.Amount(10_100)

	closeTerms := &CloseChannelTerms{
		ShutdownBalances: ShutdownBalances{
			LocalBalance:  localBalance,
			RemoteBalance: remoteBalance,
		},
		ShutdownScripts: ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		},
	}
	startingState := &ClosingNegotiation{
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

	balanceAfterClose := remoteBalance.ToSatoshis() - absoluteFee
	sequence := uint32(mempool.MaxRBFSequence)

	// This case tests that if we receive a signature from the remote
	// party, where they can't pay for the fees, we exit.
	t.Run("recv_offer_cannot_pay_for_fees", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](startingState),
		})
		defer closeHarness.stopAndAssert()

		// We'll send in a new fee proposal, but the proposed fee will
		// be higher than the remote party's balance.
		event := &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				CloserScript: remoteAddr,
				CloseeScript: localAddr,
				FeeSatoshis:  absoluteFee * 10,
			},
		}

		closeHarness.sendEventAndExpectFailure(
			ctx, event, ErrRemoteCannotPay,
		)
		closeHarness.assertNoStateTransitions()
	})

	// If our balance is dust, then the remote party should send a
	// signature that doesn't include our output.
	t.Run("recv_offer_err_closer_no_closee", func(t *testing.T) {
		// We'll modify our local balance to be dust.
		closingTerms := *closeTerms
		closingTerms.ShutdownBalances.LocalBalance = 100

		firstState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: &closingTerms,
				},
				Remote: &RemoteCloseStart{
					CloseChannelTerms: &closingTerms,
				},
			},
			CloseChannelTerms: &closingTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](firstState),
		})
		defer closeHarness.stopAndAssert()

		// Our balance is dust, so we should reject this signature that
		// includes our output.
		event := &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				FeeSatoshis:  absoluteFee,
				CloserScript: remoteAddr,
				CloseeScript: localAddr,
				ClosingSigs: lnwire.ClosingSigs{
					CloserAndClosee: remoteSigRecordType3,
				},
			},
		}
		closeHarness.sendEventAndExpectFailure(
			ctx, event, ErrCloserNoClosee,
		)
		closeHarness.assertNoStateTransitions()
	})

	// If no balances are dust, then they should send a sig covering both
	// outputs.
	t.Run("recv_offer_err_closer_and_closee", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](startingState),
		})
		defer closeHarness.stopAndAssert()

		// Both balances are above dust, we should reject this
		// signature as it excludes an output.
		event := &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				FeeSatoshis:  absoluteFee,
				CloserScript: remoteAddr,
				CloseeScript: localAddr,
				ClosingSigs: lnwire.ClosingSigs{
					CloserNoClosee: remoteSigRecordType1,
				},
			},
		}
		closeHarness.sendEventAndExpectFailure(
			ctx, event, ErrCloserAndClosee,
		)
		closeHarness.assertNoStateTransitions()
	})

	// If everything lines up, then we should be able to do multiple RBF
	// loops to enable the remote party to sign.new versions of the co-op
	// close transaction.
	t.Run("recv_offer_rbf_loop_iterations", func(t *testing.T) {
		// We'll modify our balance s.t we're unable to pay for fees,
		// but aren't yet dust.
		closingTerms := *closeTerms
		closingTerms.ShutdownBalances.LocalBalance = lnwire.NewMSatFromSatoshis( //nolint:ll
			9000,
		)

		firstState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: &closingTerms,
				},
				Remote: &RemoteCloseStart{
					CloseChannelTerms: &closingTerms,
				},
			},
			CloseChannelTerms: &closingTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState:     fn.Some[ProtocolState](firstState),
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		feeOffer := &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				CloserScript: remoteAddr,
				CloseeScript: localAddr,
				FeeSatoshis:  absoluteFee,
				LockTime:     1,
				ClosingSigs: lnwire.ClosingSigs{
					CloserAndClosee: remoteSigRecordType3,
				},
			},
		}

		// As we're already in the negotiation phase, we'll now trigger
		// a new iteration by having the remote party send a new offer
		// sig.
		closeHarness.assertSingleRemoteRbfIteration(
			feeOffer, balanceAfterClose, absoluteFee, sequence,
			false, closeHarness.chanCloser.SendEvent,
		)

		// Next, we'll receive an offer from the remote party, and drive
		// another RBF iteration. This time, we'll increase the absolute
		// fee by 1k sats.
		feeOffer.SigMsg.FeeSatoshis += 1000
		absoluteFee = feeOffer.SigMsg.FeeSatoshis
		closeHarness.assertSingleRemoteRbfIteration(
			feeOffer, balanceAfterClose, absoluteFee, sequence,
			true, closeHarness.chanCloser.SendEvent,
		)

		closeHarness.assertNoStateTransitions()
	})

	// This tests that if we get an offer that has the wrong local script,
	// then we'll emit a hard error.
	t.Run("recv_offer_wrong_local_script", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](startingState),
		})
		defer closeHarness.stopAndAssert()

		// The remote party will send a ClosingComplete message, but
		// with the wrong local script. We should expect an error.
		// We'll send our remote addr as the Closee script, which should
		// trigger an error.
		event := &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				FeeSatoshis:  absoluteFee,
				CloserScript: remoteAddr,
				CloseeScript: remoteAddr,
				ClosingSigs: lnwire.ClosingSigs{
					CloserNoClosee: remoteSigRecordType1,
				},
			},
		}
		closeHarness.sendEventAndExpectFailure(
			ctx, event, ErrWrongLocalScript,
		)
		closeHarness.assertNoStateTransitions()
	})

	// If we receive an offer from the remote party with a different remote
	// script, then this ensures that we'll process that and use that create
	// the next offer.
	t.Run("recv_offer_remote_addr_change", func(t *testing.T) {
		closingTerms := *closeTerms

		firstState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Local: &LocalCloseStart{
					CloseChannelTerms: &closingTerms,
				},
				Remote: &RemoteCloseStart{
					CloseChannelTerms: &closingTerms,
				},
			},
			CloseChannelTerms: &closingTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState:     fn.Some[ProtocolState](firstState),
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		// This time, the close request sent by the remote party will
		// modify their normal remote address. This should cause us to
		// recognize this, and counter sign the proper co-op close
		// transaction.
		newRemoteAddr := lnwire.DeliveryAddress(append(
			[]byte{txscript.OP_1, txscript.OP_DATA_32},
			bytes.Repeat([]byte{0x03}, 32)...,
		))
		feeOffer := &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				CloserScript: newRemoteAddr,
				CloseeScript: localAddr,
				FeeSatoshis:  absoluteFee,
				LockTime:     1,
				ClosingSigs: lnwire.ClosingSigs{
					CloserAndClosee: remoteSigRecordType3,
				},
			},
		}

		// As we're already in the negotiation phase, we'll now trigger
		// a new iteration by having the remote party send a new offer
		// sig.
		closeHarness.assertSingleRemoteRbfIteration(
			feeOffer, balanceAfterClose, absoluteFee, sequence,
			false, closeHarness.chanCloser.SendEvent,
		)
	})

	// Any other event should be ignored.
	assertUnknownEventFail(t, startingState)

	// Sending a Spend event should transition to CloseFin.
	assertSpendEventCloseFin(t, startingState)
}

// TestRbfCloseErr tests that the state machine is able to properly restart
// the state machine if we encounter an error.
func TestRbfCloseErr(t *testing.T) {
	localBalance := lnwire.NewMSatFromSatoshis(40_000)
	remoteBalance := lnwire.NewMSatFromSatoshis(50_000)

	closeTerms := &CloseChannelTerms{
		ShutdownBalances: ShutdownBalances{
			LocalBalance:  localBalance,
			RemoteBalance: remoteBalance,
		},
		ShutdownScripts: ShutdownScripts{
			LocalDeliveryScript:  localAddr,
			RemoteDeliveryScript: remoteAddr,
		},
	}
	startingState := &ClosingNegotiation{
		PeerState: lntypes.Dual[AsymmetricPeerState]{
			Local: &CloseErr{
				CloseChannelTerms: closeTerms,
			},
		},
		CloseChannelTerms: closeTerms,
	}

	absoluteFee := btcutil.Amount(10_100)
	balanceAfterClose := localBalance.ToSatoshis() - absoluteFee

	// From the error state, we should be able to kick off a new iteration
	// for a local fee bump.
	t.Run("send_offer_restart", func(t *testing.T) {
		closeHarness := newCloser(t, &harnessCfg{
			initialState: fn.Some[ProtocolState](startingState),
		})
		defer closeHarness.stopAndAssert()

		rbfFeeBump := chainfee.FeePerKwFloor.FeePerVByte()
		localOffer := &SendOfferEvent{
			TargetFeeRate: rbfFeeBump,
		}

		// Now we expect that another full RBF iteration takes place (we
		// initiate a new local sig).
		closeHarness.assertSingleRbfIteration(
			localOffer, balanceAfterClose, absoluteFee,
			noDustExpect, true,
		)
	})

	// From the error state, we should be able to handle the remote party
	// kicking off a new iteration for a fee bump.
	t.Run("recv_offer_restart", func(t *testing.T) {
		startingState := &ClosingNegotiation{
			PeerState: lntypes.Dual[AsymmetricPeerState]{
				Remote: &CloseErr{
					CloseChannelTerms: closeTerms,
					Party:             lntypes.Remote,
				},
			},
			CloseChannelTerms: closeTerms,
		}

		closeHarness := newCloser(t, &harnessCfg{
			initialState:     fn.Some[ProtocolState](startingState),
			localUpfrontAddr: fn.Some(localAddr),
		})
		defer closeHarness.stopAndAssert()

		feeOffer := &OfferReceivedEvent{
			SigMsg: lnwire.ClosingComplete{
				CloserScript: remoteAddr,
				CloseeScript: localAddr,
				FeeSatoshis:  absoluteFee,
				LockTime:     1,
				ClosingSigs: lnwire.ClosingSigs{
					CloserAndClosee: remoteSigRecordType3,
				},
			},
		}

		sequence := uint32(mempool.MaxRBFSequence)

		// As we're already in the negotiation phase, we'll now trigger
		// a new iteration by having the remote party send a new offer
		// sig.
		closeHarness.assertSingleRemoteRbfIteration(
			feeOffer, balanceAfterClose, absoluteFee, sequence,
			true, closeHarness.chanCloser.SendEvent,
		)
	})

	// Sending a Spend event should transition to CloseFin.
	assertSpendEventCloseFin(t, startingState)
}

package chancloser

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
)

var (
	// ErrInvalidStateTransition is returned when we receive an unexpected
	// event for a given state.
	ErrInvalidStateTransition = fmt.Errorf("invalid state transition")

	// ErrTooManySigs is returned when we receive too many sigs from the
	// remote party in the ClosingSigs message.
	ErrTooManySigs = fmt.Errorf("too many sigs received")

	// ErrNoSig is returned when we receive no sig from the remote party.
	ErrNoSig = fmt.Errorf("no sig received")

	// ErrUnknownFinalBalance is returned if we're unable to determine the
	// final channel balance after a flush.
	ErrUnknownFinalBalance = fmt.Errorf("unknown final balance")

	// ErrRemoteCannotPay is returned if the remote party cannot pay the
	// pay for the fees when it sends a signature.
	ErrRemoteCannotPay = fmt.Errorf("remote cannot pay fees")

	// ErrNonFinalSequence is returned if we receive a non-final sequence
	// from the remote party for their signature.
	ErrNonFinalSequence = fmt.Errorf("received non-final sequence")

	// ErrCloserNoClosee is returned if our balance is dust, but the remote
	// party includes our output.
	ErrCloserNoClosee = fmt.Errorf("expected CloserNoClosee sig")

	// ErrCloserAndClosee is returned when we expect a sig covering both
	// outputs, it isn't present.
	ErrCloserAndClosee = fmt.Errorf("expected CloserAndClosee sig")

	// ErrWrongLocalScript is returned when the remote party sends a
	// ClosingComplete message that doesn't carry our last local script
	// sent.
	ErrWrongLocalScript = fmt.Errorf("wrong local script")
)

// ProtocolEvent is a special interface used to create the equivalent of a
// sum-type, but using a "sealed" interface. Protocol events can be used as
// input to trigger a state transition, and also as output to trigger a new set
// of events into the very same state machine.
type ProtocolEvent interface {
	protocolSealed()
}

// ProtocolEvents is a special type constraint that enumerates all the possible
// protocol events. This is used mainly as type-level documentation, and may
// also be useful to constraint certain state transition functions.
type ProtocolEvents interface {
	SendShutdown | ShutdownReceived | ShutdownComplete | ChannelFlushed |
		SendOfferEvent | OfferReceivedEvent | LocalSigReceived |
		SpendEvent
}

// SpendEvent indicates that a transaction spending the funding outpoint has
// been confirmed in the main chain.
type SpendEvent struct {
	// Tx is the spending transaction that has been confirmed.
	Tx *wire.MsgTx

	// BlockHeight is the height of the block that confirmed the
	// transaction.
	BlockHeight uint32
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (s *SpendEvent) protocolSealed() {}

// SendShutdown indicates that the user wishes to co-op close the channel, so we
// should send a new shutdown message to the remote party.
//
// transition:
//   - fromState: ChannelActive
//   - toState: ChannelFlushing
type SendShutdown struct {
	// DeliveryAddr is the address we'd like to receive the funds to. If
	// None, then a new addr will be generated.
	DeliveryAddr fn.Option[lnwire.DeliveryAddress]

	// IdealFeeRate is the ideal fee rate we'd like to use for the closing
	// attempt.
	IdealFeeRate chainfee.SatPerVByte
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (s *SendShutdown) protocolSealed() {}

// ShutdownReceived indicates that we received a shutdown event so we need to
// enter the flushing state.
//
// transition:
//   - fromState: ChannelActive
//   - toState: ChannelFlushing
type ShutdownReceived struct {
	// ShutdownScript is the script the remote party wants to use to
	// shutdown.
	ShutdownScript lnwire.DeliveryAddress

	// BlockHeight is the height at which the shutdown message was
	// received. This is used for channel leases to determine if a co-op
	// close can occur.
	BlockHeight uint32
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (s *ShutdownReceived) protocolSealed() {}

// ShutdownComplete is an event that indicates the channel has been fully
// shutdown. At this point, we'll go to the ChannelFlushing state so we can
// wait for all pending updates to be gone from the channel.
//
// transition:
//   - fromState: ShutdownPending
//   - toState: ChannelFlushing
type ShutdownComplete struct {
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (s *ShutdownComplete) protocolSealed() {}

// ShutdownBalances holds the local+remote balance once the channel has been
// fully flushed.
type ShutdownBalances struct {
	// LocalBalance is the local balance of the channel.
	LocalBalance lnwire.MilliSatoshi

	// RemoteBalance is the remote balance of the channel.
	RemoteBalance lnwire.MilliSatoshi
}

// unknownBalance is a special variable used to denote an unknown channel
// balance (channel not fully flushed yet).
var unknownBalance = ShutdownBalances{}

// ChannelFlushed is an event that indicates the channel has been fully flushed
// can we can now start closing negotiation.
//
// transition:
//   - fromState: ChannelFlushing
//   - toState: ClosingNegotiation
type ChannelFlushed struct {
	// FreshFlush indicates if this is the first time the channel has been
	// flushed, or if this is a flush as part of an RBF iteration.
	FreshFlush bool

	// ShutdownBalances is the balances of the channel once it has been
	// flushed. We tie this to the ChannelFlushed state as this may not be
	// the same as the starting value.
	ShutdownBalances
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (c *ChannelFlushed) protocolSealed() {}

// SendOfferEvent is a self-triggered event that transitions us from the
// LocalCloseStart state to the LocalOfferSent state. This kicks off the new
// signing process for the co-op close process.
//
// transition:
//   - fromState: LocalCloseStart
//   - toState: LocalOfferSent
type SendOfferEvent struct {
	// TargetFeeRate is the fee rate we'll use for the closing transaction.
	TargetFeeRate chainfee.SatPerVByte
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (s *SendOfferEvent) protocolSealed() {}

// LocalSigReceived is an event that indicates we've received a signature from
// the remote party, which signs our the co-op close transaction at our
// specified fee rate.
//
// transition:
//   - fromState: LocalOfferSent
//   - toState: ClosePending
type LocalSigReceived struct {
	// SigMsg is the sig message we received from the remote party.
	SigMsg lnwire.ClosingSig
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (s *LocalSigReceived) protocolSealed() {}

// OfferReceivedEvent is an event that indicates we've received an offer from
// the remote party. This applies to the RemoteCloseStart state.
//
// transition:
//   - fromState: RemoteCloseStart
//   - toState: ClosePending
type OfferReceivedEvent struct {
	// SigMsg is the signature message we received from the remote party.
	SigMsg lnwire.ClosingComplete
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (s *OfferReceivedEvent) protocolSealed() {}

// CloseSigner is an interface that abstracts away the details of the signing
// new coop close transactions.
type CloseSigner interface {
	// CreateCloseProposal creates a new co-op close proposal in the form
	// of a valid signature, the chainhash of the final txid, and our final
	// balance in the created state.
	CreateCloseProposal(proposedFee btcutil.Amount,
		localDeliveryScript []byte, remoteDeliveryScript []byte,
		closeOpt ...lnwallet.ChanCloseOpt) (input.Signature,
		*wire.MsgTx, btcutil.Amount, error)

	// CompleteCooperativeClose persistently "completes" the cooperative
	// close by producing a fully signed co-op close transaction.
	CompleteCooperativeClose(localSig, remoteSig input.Signature,
		localDeliveryScript, remoteDeliveryScript []byte,
		proposedFee btcutil.Amount,
		closeOpt ...lnwallet.ChanCloseOpt) (*wire.MsgTx, btcutil.Amount,
		error)
}

// ChanStateObserver is an interface used to observe state changes that occur
// in a channel. This can be used to figure out if we're able to send a
// shutdown message or not.
type ChanStateObserver interface {
	// NoDanglingUpdates returns true if there are no dangling updates in
	// the channel. In other words, there are no active update messages
	// that haven't already been covered by a commit sig.
	NoDanglingUpdates() bool

	// DisableIncomingAdds instructs the channel link to disable process new
	// incoming add messages.
	DisableIncomingAdds() error

	// DisableOutgoingAdds instructs the channel link to disable process
	// new outgoing add messages.
	DisableOutgoingAdds() error

	// DisableChannel attempts to disable a channel (marking it ineligible
	// to forward), and also sends out a network update to disable the
	// channel.
	DisableChannel() error

	// MarkCoopBroadcasted persistently marks that the channel close
	// transaction has been broadcast.
	MarkCoopBroadcasted(*wire.MsgTx, bool) error

	// MarkShutdownSent persists the given ShutdownInfo. The existence of
	// the ShutdownInfo represents the fact that the Shutdown message has
	// been sent by us and so should be re-sent on re-establish.
	MarkShutdownSent(deliveryAddr []byte, isInitiator bool) error

	// FinalBalances is the balances of the channel once it has been
	// flushed. If Some, then this indicates that the channel is now in a
	// state where it's always flushed, so we can accelerate the state
	// transitions.
	FinalBalances() fn.Option[ShutdownBalances]
}

// Environment is a set of dependencies that a state machine may need to carry
// out the logic for a given state transition. All fields are to be considered
// immutable, and will be fixed for the lifetime of the state machine.
type Environment struct {
	// ChainParams is the chain parameters for the channel.
	ChainParams chaincfg.Params

	// ChanPeer is the peer we're attempting to close the channel with.
	ChanPeer btcec.PublicKey

	// ChanPoint is the channel point of the active channel.
	ChanPoint wire.OutPoint

	// ChanID is the channel ID of the channel we're attempting to close.
	ChanID lnwire.ChannelID

	// ShortChanID is the short channel ID of the channel we're attempting
	// to close.
	Scid lnwire.ShortChannelID

	// ChanType is the type of channel we're attempting to close.
	ChanType channeldb.ChannelType

	// BlockHeight is the current block height.
	BlockHeight uint32

	// DefaultFeeRate is the fee we'll use for the closing transaction if
	// the user didn't specify an ideal fee rate. This may happen if the
	// remote party is the one that initiates the co-op close.
	DefaultFeeRate chainfee.SatPerVByte

	// ThawHeight is the height at which the channel will be thawed. If
	// this is None, then co-op close can occur at any moment.
	ThawHeight fn.Option[uint32]

	// RemoteUprontShutdown is the upfront shutdown addr of the remote
	// party. We'll use this to validate if the remote peer is authorized to
	// close the channel with the sent addr or not.
	RemoteUpfrontShutdown fn.Option[lnwire.DeliveryAddress]

	// LocalUprontShutdown is our upfront shutdown address. If Some, then
	// we'll default to using this.
	LocalUpfrontShutdown fn.Option[lnwire.DeliveryAddress]

	// NewDeliveryScript is a function that returns a new delivery script.
	// This is used if we don't have an upfront shutdown addr, and no addr
	// was specified at closing time.
	NewDeliveryScript func() (lnwire.DeliveryAddress, error)

	// FeeEstimator is the fee estimator we'll use to determine the fee in
	// satoshis we'll pay given a local and/or remote output.
	FeeEstimator CoopFeeEstimator

	// ChanObserver is an interface used to observe state changes to the
	// channel. We'll use this to figure out when/if we can send certain
	// messages.
	ChanObserver ChanStateObserver

	// CloseSigner is the signer we'll use to sign the close transaction.
	// This is a part of the ChannelFlushed state, as the channel state
	// we'll be signing can only be determined once the channel has been
	// flushed.
	CloseSigner CloseSigner
}

// Name returns the name of the environment. This is used to uniquely identify
// the environment of related state machines. For this state machine, the name
// is based on the channel ID.
func (e *Environment) Name() string {
	return fmt.Sprintf("rbf_chan_closer(%v)", e.ChanPoint)
}

// CloseStateTransition is the StateTransition type specific to the coop close
// state machine.
//
//nolint:ll
type CloseStateTransition = protofsm.StateTransition[ProtocolEvent, *Environment]

// ProtocolState is our sum-type ish interface that represents the current
// protocol state.
type ProtocolState interface {
	// protocolStateSealed is a special method that is used to seal the
	// interface (only types in this package can implement it).
	protocolStateSealed()

	// IsTerminal returns true if the target state is a terminal state.
	IsTerminal() bool

	// ProcessEvent takes a protocol event, and implements a state
	// transition for the state.
	ProcessEvent(ProtocolEvent, *Environment) (*CloseStateTransition, error)

	// String returns the name of the state.
	String() string
}

// AsymmetricPeerState is an extension of the normal ProtocolState interface
// that gives a caller a hit on if the target state should process an incoming
// event or not.
type AsymmetricPeerState interface {
	ProtocolState

	// ShouldRouteTo returns true if the target state should process the
	// target event.
	ShouldRouteTo(ProtocolEvent) bool
}

// ProtocolStates is a special type constraint that enumerates all the possible
// protocol states.
type ProtocolStates interface {
	ChannelActive | ShutdownPending | ChannelFlushing | ClosingNegotiation |
		LocalCloseStart | LocalOfferSent | RemoteCloseStart |
		ClosePending | CloseFin | CloseErr
}

// ChannelActive is the base state for the channel closer state machine. In
// this state, we haven't begun the shutdown process yet, so the channel is
// still active. Receiving the ShutdownSent or ShutdownReceived events will
// transition us to the ChannelFushing state.
//
// When we transition to this state, we emit a DaemonEvent to send the shutdown
// message if we received one ourselves. Alternatively, we may send out a new
// shutdown if we're initiating it for the very first time.
//
// transition:
//   - fromState: None
//   - toState: ChannelFlushing
//
// input events:
//   - SendShutdown
//   - ShutdownReceived
type ChannelActive struct {
}

// String returns the name of the state for ChannelActive.
func (c *ChannelActive) String() string {
	return "ChannelActive"
}

// IsTerminal returns true if the target state is a terminal state.
func (c *ChannelActive) IsTerminal() bool {
	return false
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (c *ChannelActive) protocolStateSealed() {}

// ShutdownScripts is a set of scripts that we'll use to co-op close the
// channel.
type ShutdownScripts struct {
	// LocalDeliveryScript is the script that we'll send our settled
	// channel funds to.
	LocalDeliveryScript lnwire.DeliveryAddress

	// RemoteDeliveryScript is the script that we'll send the remote
	// party's settled channel funds to.
	RemoteDeliveryScript lnwire.DeliveryAddress
}

// ShutdownPending is the state we enter into after we've sent or received the
// shutdown message. If we sent the shutdown, then we'll wait for the remote
// party to send a shutdown. Otherwise, if we received it, then we'll send our
// shutdown then go to the next state.
//
// transition:
//   - fromState: ChannelActive
//   - toState: ChannelFlushing
//
// input events:
//   - SendShutdown
//   - ShutdownReceived
type ShutdownPending struct {
	// ShutdownScripts store the set of scripts we'll use to initiate a coop
	// close.
	ShutdownScripts

	// IdealFeeRate is the ideal fee rate we'd like to use for the closing
	// attempt.
	IdealFeeRate fn.Option[chainfee.SatPerVByte]

	// EarlyRemoteOffer is the offer we received from the remote party
	// before we received their shutdown message. We'll stash it to process
	// later.
	EarlyRemoteOffer fn.Option[OfferReceivedEvent]
}

// String returns the name of the state for ShutdownPending.
func (s *ShutdownPending) String() string {
	return "ShutdownPending"
}

// IsTerminal returns true if the target state is a terminal state.
func (s *ShutdownPending) IsTerminal() bool {
	return false
}

// protocolStateSealed indicates that this struct is a ProtocolEvent instance.
func (s *ShutdownPending) protocolStateSealed() {}

// ChannelFlushing is the state we enter into after we've received or sent a
// shutdown message. In this state, we wait the ChannelFlushed event, after
// which we'll transition to the CloseReady state.
//
// transition:
//   - fromState: ShutdownPending
//   - toState: ClosingNegotiation
//
// input events:
//   - ShutdownComplete
//   - ShutdownReceived
type ChannelFlushing struct {
	// EarlyRemoteOffer is the offer we received from the remote party
	// before we obtained the local channel flushed event. We'll stash this
	// to process later.
	EarlyRemoteOffer fn.Option[OfferReceivedEvent]

	// ShutdownScripts store the set of scripts we'll use to initiate a coop
	// close.
	ShutdownScripts

	// IdealFeeRate is the ideal fee rate we'd like to use for the closing
	// transaction. Once the channel has been flushed, we'll use this as
	// our target fee rate.
	IdealFeeRate fn.Option[chainfee.SatPerVByte]
}

// String returns the name of the state for ChannelFlushing.
func (c *ChannelFlushing) String() string {
	return "ChannelFlushing"
}

// protocolStateSealed indicates that this struct is a ProtocolEvent instance.
func (c *ChannelFlushing) protocolStateSealed() {}

// IsTerminal returns true if the target state is a terminal state.
func (c *ChannelFlushing) IsTerminal() bool {
	return false
}

// ClosingNegotiation is the state we transition to once the channel has been
// flushed. This is actually a composite state that contains one for each side
// of the channel, as the closing process is asymmetric. Once either of the
// peer states reaches the CloseFin state, then the channel is fully closed,
// and we'll transition to that terminal state.
//
// transition:
//   - fromState: ChannelFlushing
//   - toState: CloseFin
//
// input events:
//   - ChannelFlushed
type ClosingNegotiation struct {
	// PeerStates is a composite state that contains the state for both the
	// local and remote parties. Our usage of Dual makes this a special
	// state that allows us to treat two states as a single state. We'll use
	// the ShouldRouteTo method to determine which state route incoming
	// events to.
	PeerState lntypes.Dual[AsymmetricPeerState]

	// CloseChannelTerms is the terms we'll use to close the channel. We
	// hold a value here which is pointed to by the various
	// AsymmetricPeerState instances. This allows us to update this value if
	// the remote peer sends a new address, with each of the state noting
	// the new value via a pointer.
	*CloseChannelTerms
}

// String returns the name of the state for ClosingNegotiation.
func (c *ClosingNegotiation) String() string {
	localState := c.PeerState.GetForParty(lntypes.Local)
	remoteState := c.PeerState.GetForParty(lntypes.Remote)

	return fmt.Sprintf("ClosingNegotiation(local=%v, remote=%v)",
		localState, remoteState)
}

// IsTerminal returns true if the target state is a terminal state.
func (c *ClosingNegotiation) IsTerminal() bool {
	return false
}

// protocolSealed indicates that this struct is a ProtocolEvent instance.
func (c *ClosingNegotiation) protocolStateSealed() {}

// ErrState can be used to introspect into a benign error related to a state
// transition.
type ErrState interface {
	sealed()

	error

	// Err returns an error for the ErrState.
	Err() error
}

// ErrStateCantPayForFee is sent when the local party attempts a fee update
// that they can't actually party for.
type ErrStateCantPayForFee struct {
	localBalance btcutil.Amount

	attemptedFee btcutil.Amount
}

// NewErrStateCantPayForFee returns a new NewErrStateCantPayForFee error.
func NewErrStateCantPayForFee(localBalance,
	attemptedFee btcutil.Amount) *ErrStateCantPayForFee {

	return &ErrStateCantPayForFee{
		localBalance: localBalance,
		attemptedFee: attemptedFee,
	}
}

// sealed makes this a sealed interface.
func (e *ErrStateCantPayForFee) sealed() {
}

// Err returns an error for the ErrState.
func (e *ErrStateCantPayForFee) Err() error {
	return fmt.Errorf("cannot pay for fee of %v, only have %v local "+
		"balance", e.attemptedFee, e.localBalance)
}

// Error returns the error string for the ErrState.
func (e *ErrStateCantPayForFee) Error() string {
	return e.Err().Error()
}

// String returns the string for the ErrStateCantPayForFee.
func (e *ErrStateCantPayForFee) String() string {
	return fmt.Sprintf("ErrStateCantPayForFee(local_balance=%v, "+
		"attempted_fee=%v)", e.localBalance, e.attemptedFee)
}

// CloseChannelTerms is a set of terms that we'll use to close the channel. This
// includes the balances of the channel, and the scripts we'll use to send each
// party's funds to.
type CloseChannelTerms struct {
	ShutdownScripts

	ShutdownBalances
}

// DeriveCloseTxOuts takes the close terms, and returns the local and remote tx
// out for the close transaction. If an output is dust, then it'll be nil.
func (c *CloseChannelTerms) DeriveCloseTxOuts() (*wire.TxOut, *wire.TxOut) {
	deriveTxOut := func(balance btcutil.Amount,
		pkScript []byte) *wire.TxOut {

		// We'll base the existence of the output on our normal dust
		// check.
		dustLimit := lnwallet.DustLimitForSize(len(pkScript))
		if balance >= dustLimit {
			return &wire.TxOut{
				PkScript: pkScript,
				Value:    int64(balance),
			}
		}

		return nil
	}

	localTxOut := deriveTxOut(
		c.LocalBalance.ToSatoshis(),
		c.LocalDeliveryScript,
	)
	remoteTxOut := deriveTxOut(
		c.RemoteBalance.ToSatoshis(),
		c.RemoteDeliveryScript,
	)

	return localTxOut, remoteTxOut
}

// RemoteAmtIsDust returns true if the remote output is dust.
func (c *CloseChannelTerms) RemoteAmtIsDust() bool {
	return c.RemoteBalance.ToSatoshis() < lnwallet.DustLimitForSize(
		len(c.RemoteDeliveryScript),
	)
}

// LocalAmtIsDust returns true if the local output is dust.
func (c *CloseChannelTerms) LocalAmtIsDust() bool {
	return c.LocalBalance.ToSatoshis() < lnwallet.DustLimitForSize(
		len(c.LocalDeliveryScript),
	)
}

// LocalCanPayFees returns true if the local party can pay the absolute fee
// from their local settled balance.
func (c *CloseChannelTerms) LocalCanPayFees(absoluteFee btcutil.Amount) bool {
	return c.LocalBalance.ToSatoshis() >= absoluteFee
}

// RemoteCanPayFees returns true if the remote party can pay the absolute fee
// from their remote settled balance.
func (c *CloseChannelTerms) RemoteCanPayFees(absoluteFee btcutil.Amount) bool {
	return c.RemoteBalance.ToSatoshis() >= absoluteFee
}

// LocalCloseStart is the state we enter into after we've received or sent
// shutdown, and the channel has been flushed. In this state, we'll emit a new
// event to send our offer to drive the rest of the process.
//
// transition:
//   - fromState: ChannelFlushing
//   - toState: LocalOfferSent
//
// input events:
//   - SendOfferEvent
type LocalCloseStart struct {
	*CloseChannelTerms
}

// String returns the name of the state for LocalCloseStart, including proposed
// fee details.
func (l *LocalCloseStart) String() string {
	return "LocalCloseStart"
}

// ShouldRouteTo returns true if the target state should process the target
// event.
func (l *LocalCloseStart) ShouldRouteTo(event ProtocolEvent) bool {
	switch event.(type) {
	case *SendOfferEvent:
		return true
	default:
		return false
	}
}

// IsTerminal returns true if the target state is a terminal state.
func (l *LocalCloseStart) IsTerminal() bool {
	return false
}

// protocolStateSealed indicates that this struct is a ProtocolEvent instance.
func (l *LocalCloseStart) protocolStateSealed() {}

// LocalOfferSent is the state we transition to after we receiver the
// SendOfferEvent in the LocalCloseStart state. With this state we send our
// offer to the remote party, then await a sig from them which concludes the
// local cooperative close process.
//
// transition:
//   - fromState: LocalCloseStart
//   - toState: ClosePending
//
// input events:
//   - LocalSigReceived
type LocalOfferSent struct {
	*CloseChannelTerms

	// ProposedFee is the fee we proposed to the remote party.
	ProposedFee btcutil.Amount

	// ProposedFeeRate is the fee rate we proposed to the remote party.
	ProposedFeeRate chainfee.SatPerVByte

	// LocalSig is the signature we sent to the remote party.
	LocalSig lnwire.Sig
}

// String returns the name of the state for LocalOfferSent, including proposed.
func (l *LocalOfferSent) String() string {
	return fmt.Sprintf("LocalOfferSent(proposed_fee=%v)", l.ProposedFee)
}

// ShouldRouteTo returns true if the target state should process the target
// event.
func (l *LocalOfferSent) ShouldRouteTo(event ProtocolEvent) bool {
	switch event.(type) {
	case *LocalSigReceived:
		return true
	default:
		return false
	}
}

// protocolStateaSealed indicates that this struct is a ProtocolEvent instance.
func (l *LocalOfferSent) protocolStateSealed() {}

// IsTerminal returns true if the target state is a terminal state.
func (l *LocalOfferSent) IsTerminal() bool {
	return false
}

// ClosePending is the state we enter after concluding the negotiation for the
// remote or local state. At this point, given a confirmation notification we
// can terminate the process. Otherwise, we can receive a fresh CoopCloseReq to
// go back to the very start.
//
// transition:
//   - fromState: LocalOfferSent || RemoteCloseStart
//   - toState: CloseFin
//
// input events:
//   - LocalSigReceived
//   - OfferReceivedEvent
type ClosePending struct {
	// CloseTx is the pending close transaction.
	CloseTx *wire.MsgTx

	*CloseChannelTerms

	// FeeRate is the fee rate of the closing transaction.
	FeeRate chainfee.SatPerVByte

	// Party indicates which party is at this state. This is used to
	// implement the state transition properly, based on ShouldRouteTo.
	Party lntypes.ChannelParty
}

// String returns the name of the state for ClosePending.
func (c *ClosePending) String() string {
	return fmt.Sprintf("ClosePending(txid=%v, party=%v, fee_rate=%v)",
		c.CloseTx.TxHash(), c.Party, c.FeeRate)
}

// isType returns true if the value is of type T.
func isType[T any](value any) bool {
	_, ok := value.(T)
	return ok
}

// ShouldRouteTo returns true if the target state should process the target
// event.
func (c *ClosePending) ShouldRouteTo(event ProtocolEvent) bool {
	switch event.(type) {
	case *SpendEvent:
		return true
	default:
		switch {
		case c.Party == lntypes.Local && isType[*SendOfferEvent](event):
			return true

		case c.Party == lntypes.Remote && isType[*OfferReceivedEvent](
			event,
		):

			return true
		}

		return false
	}
}

// protocolStateSealed indicates that this struct is a ProtocolEvent instance.
func (c *ClosePending) protocolStateSealed() {}

// IsTerminal returns true if the target state is a terminal state.
func (c *ClosePending) IsTerminal() bool {
	return true
}

// CloseFin is the terminal state for the channel closer state machine. At this
// point, the close tx has been confirmed on chain.
type CloseFin struct {
	// ConfirmedTx is the transaction that confirmed the channel close.
	ConfirmedTx *wire.MsgTx
}

// String returns the name of the state for CloseFin.
func (c *CloseFin) String() string {
	return "CloseFin"
}

// protocolStateSealed indicates that this struct is a ProtocolEvent instance.
func (c *CloseFin) protocolStateSealed() {}

// IsTerminal returns true if the target state is a terminal state.
func (c *CloseFin) IsTerminal() bool {
	return true
}

// RemoteCloseStart is similar to the LocalCloseStart, but is used to drive the
// process of signing an offer for the remote party
//
// transition:
//   - fromState: ChannelFlushing
//   - toState: ClosePending
type RemoteCloseStart struct {
	*CloseChannelTerms
}

// String returns the name of the state for RemoteCloseStart.
func (r *RemoteCloseStart) String() string {
	return "RemoteCloseStart"
}

// ShouldRouteTo returns true if the target state should process the target
// event.
func (l *RemoteCloseStart) ShouldRouteTo(event ProtocolEvent) bool {
	switch event.(type) {
	case *OfferReceivedEvent:
		return true
	default:
		return false
	}
}

// protocolStateSealed indicates that this struct is a ProtocolEvent instance.
func (l *RemoteCloseStart) protocolStateSealed() {}

// IsTerminal returns true if the target state is a terminal state.
func (l *RemoteCloseStart) IsTerminal() bool {
	return false
}

// CloseErr is an error state in the protocol. We enter this state when a
// protocol constraint is violated, or an upfront sanity check fails.
type CloseErr struct {
	ErrState

	*CloseChannelTerms

	// Party indicates which party is at this state. This is used to
	// implement the state transition properly, based on ShouldRouteTo.
	Party lntypes.ChannelParty
}

// String returns the name of the state for CloseErr, including error and party
// details.
func (c *CloseErr) String() string {
	return fmt.Sprintf("CloseErr(party=%v, err=%v)", c.Party, c.ErrState)
}

// ShouldRouteTo returns true if the target state should process the target
// event.
func (c *CloseErr) ShouldRouteTo(event ProtocolEvent) bool {
	switch event.(type) {
	case *SpendEvent:
		return true
	default:
		switch {
		case c.Party == lntypes.Local && isType[*SendOfferEvent](event):
			return true

		case c.Party == lntypes.Remote && isType[*OfferReceivedEvent](
			event,
		):

			return true
		}

		return false
	}
}

// protocolStateSealed indicates that this struct is a ProtocolEvent instance.
func (c *CloseErr) protocolStateSealed() {}

// IsTerminal returns true if the target state is a terminal state.
func (c *CloseErr) IsTerminal() bool {
	return true
}

// RbfChanCloser is a state machine that handles the RBF-enabled cooperative
// channel close protocol.
type RbfChanCloser = protofsm.StateMachine[ProtocolEvent, *Environment]

// RbfChanCloserCfg is a configuration struct that is used to initialize a new
// RBF chan closer state machine.
type RbfChanCloserCfg = protofsm.StateMachineCfg[ProtocolEvent, *Environment]

// RbfSpendMapper is a type used to map the generic spend event to one specific
// to this package.
type RbfSpendMapper = protofsm.SpendMapper[ProtocolEvent]

func SpendMapper(spendEvent *chainntnfs.SpendDetail) ProtocolEvent {
	return &SpendEvent{
		Tx:          spendEvent.SpendingTx,
		BlockHeight: uint32(spendEvent.SpendingHeight),
	}
}

// RbfMsgMapperT is a type used to map incoming wire messages to protocol
// events.
type RbfMsgMapperT = protofsm.MsgMapper[ProtocolEvent]

// RbfState is a type alias for the state of the RBF channel closer.
type RbfState = protofsm.State[ProtocolEvent, *Environment]

// RbfEvent is a type alias for the event type of the RBF channel closer.
type RbfEvent = protofsm.EmittedEvent[ProtocolEvent]

// RbfStateSub is a type alias for the state subscription type of the RBF chan
// closer.
type RbfStateSub = protofsm.StateSubscriber[ProtocolEvent, *Environment]

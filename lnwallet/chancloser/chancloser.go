package chancloser

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrChanAlreadyClosing is returned when a channel shutdown is attempted
	// more than once.
	ErrChanAlreadyClosing = fmt.Errorf("channel shutdown already initiated")

	// ErrChanCloseNotFinished is returned when a caller attempts to access
	// a field or function that is contingent on the channel closure negotiation
	// already being completed.
	ErrChanCloseNotFinished = fmt.Errorf("close negotiation not finished")

	// ErrInvalidState is returned when the closing state machine receives a
	// message while it is in an unknown state.
	ErrInvalidState = fmt.Errorf("invalid state")

	// ErrUpfrontShutdownScriptMismatch is returned when a peer or end user
	// provides a cooperative close script which does not match the upfront
	// shutdown script previously set for that party.
	ErrUpfrontShutdownScriptMismatch = fmt.Errorf("shutdown script does not " +
		"match upfront shutdown script")

	// ErrProposalExeceedsMaxFee is returned when as the initiator, the
	// latest fee proposal sent by the responder exceed our max fee.
	// responder.
	ErrProposalExeceedsMaxFee = fmt.Errorf("latest fee proposal exceeds " +
		"max fee")

	// ErrInvalidShutdownScript is returned when we receive an address from
	// a peer that isn't either a p2wsh or p2tr address.
	ErrInvalidShutdownScript = fmt.Errorf("invalid shutdown script")
)

// closeState represents all the possible states the channel closer state
// machine can be in. Each message will either advance to the next state, or
// remain at the current state. Once the state machine reaches a state of
// closeFinished, then negotiation is over.
type closeState uint8

const (
	// closeIdle is the initial starting state. In this state, the state
	// machine has been instantiated, but no state transitions have been
	// attempted. If a state machine receives a message while in this state,
	// then it is the responder to an initiated cooperative channel closure.
	closeIdle closeState = iota

	// closeShutdownInitiated is the state that's transitioned to once the
	// initiator of a closing workflow sends the shutdown message. At this
	// point, they're waiting for the remote party to respond with their own
	// shutdown message. After which, they'll both enter the fee negotiation
	// phase.
	closeShutdownInitiated

	// closeFeeNegotiation is the third, and most persistent state. Both
	// parties enter this state after they've sent and received a shutdown
	// message. During this phase, both sides will send monotonically
	// increasing fee requests until one side accepts the last fee rate offered
	// by the other party. In this case, the party will broadcast the closing
	// transaction, and send the accepted fee to the remote party. This then
	// causes a shift into the closeFinished state.
	closeFeeNegotiation

	// closeFinished is the final state of the state machine. In this state, a
	// side has accepted a fee offer and has broadcast the valid closing
	// transaction to the network. During this phase, the closing transaction
	// becomes available for examination.
	closeFinished
)

const (
	// defaultMaxFeeMultiplier is a multiplier we'll apply to the ideal fee
	// of the initiator, to decide when the negotiated fee is too high. By
	// default, we want to bail out if we attempt to negotiate a fee that's
	// 3x higher than our max fee.
	defaultMaxFeeMultiplier = 3
)

// Channel abstracts away from the core channel state machine by exposing an
// interface that requires only the methods we need to carry out the channel
// closing process.
type Channel interface {
	// ChannelPoint returns the channel point of the target channel.
	ChannelPoint() *wire.OutPoint

	// MarkCoopBroadcasted persistently marks that the channel close
	// transaction has been broadcast.
	MarkCoopBroadcasted(*wire.MsgTx, bool) error

	// IsInitiator returns true we are the initiator of the channel.
	IsInitiator() bool

	// ShortChanID returns the scid of the channel.
	ShortChanID() lnwire.ShortChannelID

	// AbsoluteThawHeight returns the absolute thaw height of the channel.
	// If the channel is pending, or an unconfirmed zero conf channel, then
	// an error should be returned.
	AbsoluteThawHeight() (uint32, error)

	// LocalBalanceDust returns true if when creating a co-op close
	// transaction, the balance of the local party will be dust after
	// accounting for any anchor outputs.
	LocalBalanceDust() bool

	// RemoteBalanceDust returns true if when creating a co-op close
	// transaction, the balance of the remote party will be dust after
	// accounting for any anchor outputs.
	RemoteBalanceDust() bool

	// RemoteUpfrontShutdownScript returns the upfront shutdown script of
	// the remote party. If the remote party didn't specify such a script,
	// an empty delivery address should be returned.
	RemoteUpfrontShutdownScript() lnwire.DeliveryAddress

	// CreateCloseProposal creates a new co-op close proposal in the form
	// of a valid signature, the chainhash of the final txid, and our final
	// balance in the created state.
	CreateCloseProposal(proposedFee btcutil.Amount, localDeliveryScript []byte,
		remoteDeliveryScript []byte) (input.Signature, *chainhash.Hash,
		btcutil.Amount, error)

	// CompleteCooperativeClose persistently "completes" the cooperative
	// close by producing a fully signed co-op close transaction.
	CompleteCooperativeClose(localSig, remoteSig input.Signature,
		localDeliveryScript, remoteDeliveryScript []byte,
		proposedFee btcutil.Amount) (*wire.MsgTx, btcutil.Amount, error)
}

// CoopFeeEstimator is used to estimate the fee of a co-op close transaction.
type CoopFeeEstimator interface {
	// EstimateFee estimates an _absolute_ fee for a co-op close transaction
	// given the local+remote tx outs (for the co-op close transaction),
	// channel type, and ideal fee rate. If a passed TxOut is nil, then
	// that indicates that an output is dust on the co-op close transaction
	// _before_ fees are accounted for.
	EstimateFee(chanType channeldb.ChannelType,
		localTxOut, remoteTxOut *wire.TxOut,
		idealFeeRate chainfee.SatPerKWeight) btcutil.Amount
}

// ChanCloseCfg holds all the items that a ChanCloser requires to carry out its
// duties.
type ChanCloseCfg struct {
	// Channel is the channel that should be closed.
	Channel Channel

	// BroadcastTx broadcasts the passed transaction to the network.
	BroadcastTx func(*wire.MsgTx, string) error

	// DisableChannel disables a channel, resulting in it not being able to
	// forward payments.
	DisableChannel func(wire.OutPoint) error

	// Disconnect will disconnect from the remote peer in this close.
	Disconnect func() error

	// MaxFee, is non-zero represents the highest fee that the initiator is
	// willing to pay to close the channel.
	MaxFee chainfee.SatPerKWeight

	// ChainParams holds the parameters of the chain that we're active on.
	ChainParams *chaincfg.Params

	// Quit is a channel that should be sent upon in the occasion the state
	// machine should cease all progress and shutdown.
	Quit chan struct{}

	// FeeEstimator is used to estimate the absolute starting co-op close
	// fee.
	FeeEstimator CoopFeeEstimator
}

// ChanCloser is a state machine that handles the cooperative channel closure
// procedure. This includes shutting down a channel, marking it ineligible for
// routing HTLC's, negotiating fees with the remote party, and finally
// broadcasting the fully signed closure transaction to the network.
type ChanCloser struct {
	// state is the current state of the state machine.
	state closeState

	// cfg holds the configuration for this ChanCloser instance.
	cfg ChanCloseCfg

	// chanPoint is the full channel point of the target channel.
	chanPoint wire.OutPoint

	// cid is the full channel ID of the target channel.
	cid lnwire.ChannelID

	// negotiationHeight is the height that the fee negotiation begun at.
	negotiationHeight uint32

	// closingTx is the final, fully signed closing transaction. This will only
	// be populated once the state machine shifts to the closeFinished state.
	closingTx *wire.MsgTx

	// idealFeeSat is the ideal fee that the state machine should initially
	// offer when starting negotiation. This will be used as a baseline.
	idealFeeSat btcutil.Amount

	// maxFee is the highest fee the initiator is willing to pay to close
	// out the channel. This is either a use specified value, or a default
	// multiplier based of the initial starting ideal fee.
	maxFee btcutil.Amount

	// idealFeeRate is our ideal fee rate.
	idealFeeRate chainfee.SatPerKWeight

	// lastFeeProposal is the last fee that we proposed to the remote party.
	// We'll use this as a pivot point to ratchet our next offer up, down, or
	// simply accept the remote party's prior offer.
	lastFeeProposal btcutil.Amount

	// priorFeeOffers is a map that keeps track of all the proposed fees that
	// we've offered during the fee negotiation. We use this map to cut the
	// negotiation early if the remote party ever sends an offer that we've
	// sent in the past. Once negotiation terminates, we can extract the prior
	// signature of our accepted offer from this map.
	//
	// TODO(roasbeef): need to ensure if they broadcast w/ any of our prior
	// sigs, we are aware of
	priorFeeOffers map[btcutil.Amount]*lnwire.ClosingSigned

	// closeReq is the initial closing request. This will only be populated if
	// we're the initiator of this closing negotiation.
	//
	// TODO(roasbeef): abstract away
	closeReq *htlcswitch.ChanClose

	// localDeliveryScript is the script that we'll send our settled channel
	// funds to.
	localDeliveryScript []byte

	// remoteDeliveryScript is the script that we'll send the remote party's
	// settled channel funds to.
	remoteDeliveryScript []byte

	// locallyInitiated is true if we initiated the channel close.
	locallyInitiated bool
}

// calcCoopCloseFee computes an "ideal" absolute co-op close fee given the
// delivery scripts of both parties and our ideal fee rate.
func calcCoopCloseFee(localOutput, remoteOutput *wire.TxOut,
	idealFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	var weightEstimator input.TxWeightEstimator

	weightEstimator.AddWitnessInput(input.MultiSigWitnessSize)

	// One of these outputs might be dust, so we'll skip adding it to our
	// mock transaction, so the fees are more accurate.
	if localOutput != nil {
		weightEstimator.AddTxOutput(localOutput)
	}
	if remoteOutput != nil {
		weightEstimator.AddTxOutput(remoteOutput)
	}

	totalWeight := int64(weightEstimator.Weight())

	return idealFeeRate.FeeForWeight(totalWeight)
}

// SimpleCoopFeeEstimator is the default co-op close fee estimator. It assumes
// a normal segwit v0 channel, and that no outputs on the closing transaction
// are dust.
type SimpleCoopFeeEstimator struct {
}

// EstimateFee estimates an _absolute_ fee for a co-op close transaction given
// the local+remote tx outs (for the co-op close transaction), channel type,
// and ideal fee rate.
func (d *SimpleCoopFeeEstimator) EstimateFee(chanType channeldb.ChannelType,
	localTxOut, remoteTxOut *wire.TxOut,
	idealFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	return calcCoopCloseFee(localTxOut, remoteTxOut, idealFeeRate)
}

// NewChanCloser creates a new instance of the channel closure given the passed
// configuration, and delivery+fee preference. The final argument should only
// be populated iff, we're the initiator of this closing request.
func NewChanCloser(cfg ChanCloseCfg, deliveryScript []byte,
	idealFeePerKw chainfee.SatPerKWeight, negotiationHeight uint32,
	closeReq *htlcswitch.ChanClose, locallyInitiated bool) *ChanCloser {

	cid := lnwire.NewChanIDFromOutPoint(cfg.Channel.ChannelPoint())
	return &ChanCloser{
		closeReq:            closeReq,
		state:               closeIdle,
		chanPoint:           *cfg.Channel.ChannelPoint(),
		cid:                 cid,
		cfg:                 cfg,
		negotiationHeight:   negotiationHeight,
		idealFeeRate:        idealFeePerKw,
		localDeliveryScript: deliveryScript,
		priorFeeOffers:      make(map[btcutil.Amount]*lnwire.ClosingSigned),
		locallyInitiated:    locallyInitiated,
	}
}

// initFeeBaseline computes our ideal fee rate, and also the largest fee we'll
// accept given information about the delivery script of the remote party.
func (c *ChanCloser) initFeeBaseline() {
	// Depending on if a balance ends up being dust or not, we'll pass a
	// nil TxOut into the EstimateFee call which can handle it.
	var localTxOut, remoteTxOut *wire.TxOut
	if !c.cfg.Channel.LocalBalanceDust() {
		localTxOut = &wire.TxOut{
			PkScript: c.localDeliveryScript,
			Value:    0,
		}
	}
	if !c.cfg.Channel.RemoteBalanceDust() {
		remoteTxOut = &wire.TxOut{
			PkScript: c.remoteDeliveryScript,
			Value:    0,
		}
	}

	// Given the target fee-per-kw, we'll compute what our ideal _total_
	// fee will be starting at for this fee negotiation.
	c.idealFeeSat = c.cfg.FeeEstimator.EstimateFee(
		0, localTxOut, remoteTxOut, c.idealFeeRate,
	)

	// When we're the initiator, we'll want to also factor in the highest
	// fee we want to pay. This'll either be 3x the ideal fee, or the
	// specified explicit max fee.
	c.maxFee = c.idealFeeSat * defaultMaxFeeMultiplier
	if c.cfg.MaxFee > 0 {
		c.maxFee = c.cfg.FeeEstimator.EstimateFee(
			0, localTxOut, remoteTxOut, c.cfg.MaxFee,
		)
	}

	chancloserLog.Infof("Ideal fee for closure of ChannelPoint(%v) "+
		"is: %v sat (max_fee=%v sat)", c.cfg.Channel.ChannelPoint(),
		int64(c.idealFeeSat), int64(c.maxFee))
}

// initChanShutdown begins the shutdown process by un-registering the channel,
// and creating a valid shutdown message to our target delivery address.
func (c *ChanCloser) initChanShutdown() (*lnwire.Shutdown, error) {
	// With both items constructed we'll now send the shutdown message for this
	// particular channel, advertising a shutdown request to our desired
	// closing script.
	shutdown := lnwire.NewShutdown(c.cid, c.localDeliveryScript)

	// Before closing, we'll attempt to send a disable update for the channel.
	// We do so before closing the channel as otherwise the current edge policy
	// won't be retrievable from the graph.
	if err := c.cfg.DisableChannel(c.chanPoint); err != nil {
		chancloserLog.Warnf("Unable to disable channel %v on close: %v",
			c.chanPoint, err)
	}

	// Before continuing, mark the channel as cooperatively closed with a nil
	// txn. Even though we haven't negotiated the final txn, this guarantees
	// that our listchannels rpc will be externally consistent, and reflect
	// that the channel is being shutdown by the time the closing request
	// returns.
	err := c.cfg.Channel.MarkCoopBroadcasted(nil, c.locallyInitiated)
	if err != nil {
		return nil, err
	}

	chancloserLog.Infof("ChannelPoint(%v): sending shutdown message",
		c.chanPoint)

	return shutdown, nil
}

// ShutdownChan is the first method that's to be called by the initiator of the
// cooperative channel closure. This message returns the shutdown message to
// send to the remote party. Upon completion, we enter the
// closeShutdownInitiated phase as we await a response.
func (c *ChanCloser) ShutdownChan() (*lnwire.Shutdown, error) {
	// If we attempt to shutdown the channel for the first time, and we're not
	// in the closeIdle state, then the caller made an error.
	if c.state != closeIdle {
		return nil, ErrChanAlreadyClosing
	}

	chancloserLog.Infof("ChannelPoint(%v): initiating shutdown", c.chanPoint)

	shutdownMsg, err := c.initChanShutdown()
	if err != nil {
		return nil, err
	}

	// With the opening steps complete, we'll transition into the
	// closeShutdownInitiated state. In this state, we'll wait until the other
	// party sends their version of the shutdown message.
	c.state = closeShutdownInitiated

	// Finally, we'll return the shutdown message to the caller so it can send
	// it to the remote peer.
	return shutdownMsg, nil
}

// ClosingTx returns the fully signed, final closing transaction.
//
// NOTE: This transaction is only available if the state machine is in the
// closeFinished state.
func (c *ChanCloser) ClosingTx() (*wire.MsgTx, error) {
	// If the state machine hasn't finished closing the channel, then we'll
	// return an error as we haven't yet computed the closing tx.
	if c.state != closeFinished {
		return nil, ErrChanCloseNotFinished
	}

	return c.closingTx, nil
}

// CloseRequest returns the original close request that prompted the creation
// of the state machine.
//
// NOTE: This will only return a non-nil pointer if we were the initiator of
// the cooperative closure workflow.
func (c *ChanCloser) CloseRequest() *htlcswitch.ChanClose {
	return c.closeReq
}

// Channel returns the channel stored in the config as a
// *lnwallet.LightningChannel.
//
// NOTE: This method will PANIC if the underlying channel implementation isn't
// the desired type.
func (c *ChanCloser) Channel() *lnwallet.LightningChannel {
	return c.cfg.Channel.(*lnwallet.LightningChannel)
}

// NegotiationHeight returns the negotiation height.
func (c *ChanCloser) NegotiationHeight() uint32 {
	return c.negotiationHeight
}

// validateShutdownScript attempts to match and validate the script provided in
// our peer's shutdown message with the upfront shutdown script we have on
// record. For any script specified, we also make sure it matches our
// requirements. If no upfront shutdown script was set, we do not need to
// enforce option upfront shutdown, so the function returns early. If an
// upfront script is set, we check whether it matches the script provided by
// our peer. If they do not match, we use the disconnect function provided to
// disconnect from the peer.
func validateShutdownScript(disconnect func() error, upfrontScript,
	peerScript lnwire.DeliveryAddress, netParams *chaincfg.Params) error {

	// Either way, we'll make sure that the script passed meets our
	// standards. The upfrontScript should have already been checked at an
	// earlier stage, but we'll repeat the check here for defense in depth.
	if len(upfrontScript) != 0 {
		if !lnwallet.ValidateUpfrontShutdown(upfrontScript, netParams) {
			return ErrInvalidShutdownScript
		}
	}
	if len(peerScript) != 0 {
		if !lnwallet.ValidateUpfrontShutdown(peerScript, netParams) {
			return ErrInvalidShutdownScript
		}
	}

	// If no upfront shutdown script was set, return early because we do
	// not need to enforce closure to a specific script.
	if len(upfrontScript) == 0 {
		return nil
	}

	// If an upfront shutdown script was provided, disconnect from the peer, as
	// per BOLT 2, and return an error.
	if !bytes.Equal(upfrontScript, peerScript) {
		chancloserLog.Warnf("peer's script: %x does not match upfront "+
			"shutdown script: %x", peerScript, upfrontScript)

		// Disconnect from the peer because they have violated option upfront
		// shutdown.
		if err := disconnect(); err != nil {
			return err
		}

		return ErrUpfrontShutdownScriptMismatch
	}

	return nil
}

// ProcessCloseMsg attempts to process the next message in the closing series.
// This method will update the state accordingly and return two primary values:
// the next set of messages to be sent, and a bool indicating if the fee
// negotiation process has completed. If the second value is true, then this
// means the ChanCloser can be garbage collected.
func (c *ChanCloser) ProcessCloseMsg(msg lnwire.Message) ([]lnwire.Message,
	bool, error) {

	switch c.state {
	// If we're in the close idle state, and we're receiving a channel closure
	// related message, then this indicates that we're on the receiving side of
	// an initiated channel closure.
	case closeIdle:
		// First, we'll assert that we have a channel shutdown message,
		// as otherwise, this is an attempted invalid state transition.
		shutdownMsg, ok := msg.(*lnwire.Shutdown)
		if !ok {
			return nil, false, fmt.Errorf("expected lnwire.Shutdown, instead "+
				"have %v", spew.Sdump(msg))
		}

		// As we're the responder to this shutdown (the other party
		// wants to close), we'll check if this is a frozen channel or
		// not. If the channel is frozen and we were not also the
		// initiator of the channel opening, then we'll deny their close
		// attempt.
		chanInitiator := c.cfg.Channel.IsInitiator()
		if !chanInitiator {
			absoluteThawHeight, err := c.cfg.Channel.AbsoluteThawHeight()
			if err != nil {
				return nil, false, err
			}
			if c.negotiationHeight < absoluteThawHeight {
				return nil, false, fmt.Errorf("initiator "+
					"attempting to co-op close frozen "+
					"ChannelPoint(%v) (current_height=%v, "+
					"thaw_height=%v)", c.chanPoint,
					c.negotiationHeight, absoluteThawHeight)
			}
		}

		// If the remote node opened the channel with option upfront shutdown
		// script, check that the script they provided matches.
		if err := validateShutdownScript(
			c.cfg.Disconnect, c.cfg.Channel.RemoteUpfrontShutdownScript(),
			shutdownMsg.Address, c.cfg.ChainParams,
		); err != nil {
			return nil, false, err
		}

		// Once we have checked that the other party has not violated option
		// upfront shutdown we set their preference for delivery address. We'll
		// use this when we craft the closure transaction.
		c.remoteDeliveryScript = shutdownMsg.Address

		// Now that we know their desried delivery script, we can
		// compute what our max/ideal fee will be.
		c.initFeeBaseline()

		// We'll generate a shutdown message of our own to send across the
		// wire.
		localShutdown, err := c.initChanShutdown()
		if err != nil {
			return nil, false, err
		}

		chancloserLog.Infof("ChannelPoint(%v): responding to shutdown",
			c.chanPoint)

		msgsToSend := make([]lnwire.Message, 0, 2)
		msgsToSend = append(msgsToSend, localShutdown)

		// After the other party receives this message, we'll actually start
		// the final stage of the closure process: fee negotiation. So we'll
		// update our internal state to reflect this, so we can handle the next
		// message sent.
		c.state = closeFeeNegotiation

		// We'll also craft our initial close proposal in order to keep the
		// negotiation moving, but only if we're the negotiator.
		if chanInitiator {
			closeSigned, err := c.proposeCloseSigned(c.idealFeeSat)
			if err != nil {
				return nil, false, err
			}
			msgsToSend = append(msgsToSend, closeSigned)
		}

		// We'll return both sets of messages to send to the remote party to
		// kick off the fee negotiation process.
		return msgsToSend, false, nil

	// If we just initiated a channel shutdown, and we receive a new message,
	// then this indicates the other party is ready to shutdown as well. In
	// this state we'll send our first signature.
	case closeShutdownInitiated:
		// First, we'll assert that we have a channel shutdown message.
		// Otherwise, this is an attempted invalid state transition.
		shutdownMsg, ok := msg.(*lnwire.Shutdown)
		if !ok {
			return nil, false, fmt.Errorf("expected lnwire.Shutdown, instead "+
				"have %v", spew.Sdump(msg))
		}

		// If the remote node opened the channel with option upfront shutdown
		// script, check that the script they provided matches.
		if err := validateShutdownScript(
			c.cfg.Disconnect,
			c.cfg.Channel.RemoteUpfrontShutdownScript(), shutdownMsg.Address,
			c.cfg.ChainParams,
		); err != nil {
			return nil, false, err
		}

		// Now that we know this is a valid shutdown message and address, we'll
		// record their preferred delivery closing script.
		c.remoteDeliveryScript = shutdownMsg.Address

		// At this point, we can now start the fee negotiation state, by
		// constructing and sending our initial signature for what we think the
		// closing transaction should look like.
		c.state = closeFeeNegotiation

		// Now that we know their desried delivery script, we can
		// compute what our max/ideal fee will be.
		c.initFeeBaseline()

		chancloserLog.Infof("ChannelPoint(%v): shutdown response received, "+
			"entering fee negotiation", c.chanPoint)

		// Starting with our ideal fee rate, we'll create an initial closing
		// proposal, but only if we're the initiator, as otherwise, the other
		// party will send their initial proposal first.
		if c.cfg.Channel.IsInitiator() {
			closeSigned, err := c.proposeCloseSigned(c.idealFeeSat)
			if err != nil {
				return nil, false, err
			}

			return []lnwire.Message{closeSigned}, false, nil
		}

		return nil, false, nil

	// If we're receiving a message while we're in the fee negotiation phase,
	// then this indicates the remote party is responding to a close signed
	// message we sent, or kicking off the process with their own.
	case closeFeeNegotiation:
		// First, we'll assert that we're actually getting a ClosingSigned
		// message, otherwise an invalid state transition was attempted.
		closeSignedMsg, ok := msg.(*lnwire.ClosingSigned)
		if !ok {
			return nil, false, fmt.Errorf("expected lnwire.ClosingSigned, "+
				"instead have %v", spew.Sdump(msg))
		}

		// We'll compare the proposed total fee, to what we've proposed during
		// the negotiations. If it doesn't match any of our prior offers, then
		// we'll attempt to ratchet the fee closer to
		remoteProposedFee := closeSignedMsg.FeeSatoshis
		if _, ok := c.priorFeeOffers[remoteProposedFee]; !ok {
			// We'll now attempt to ratchet towards a fee deemed acceptable by
			// both parties, factoring in our ideal fee rate, and the last
			// proposed fee by both sides.
			feeProposal := calcCompromiseFee(c.chanPoint, c.idealFeeSat,
				c.lastFeeProposal, remoteProposedFee,
			)
			if c.cfg.Channel.IsInitiator() && feeProposal > c.maxFee {
				return nil, false, fmt.Errorf("%w: %v > %v",
					ErrProposalExeceedsMaxFee, feeProposal,
					c.maxFee)
			}

			// With our new fee proposal calculated, we'll craft a new close
			// signed signature to send to the other party so we can continue
			// the fee negotiation process.
			closeSigned, err := c.proposeCloseSigned(feeProposal)
			if err != nil {
				return nil, false, err
			}

			// If the compromise fee doesn't match what the peer proposed, then
			// we'll return this latest close signed message so we can continue
			// negotiation.
			if feeProposal != remoteProposedFee {
				chancloserLog.Debugf("ChannelPoint(%v): close tx fee "+
					"disagreement, continuing negotiation", c.chanPoint)
				return []lnwire.Message{closeSigned}, false, nil
			}
		}

		chancloserLog.Infof("ChannelPoint(%v) fee of %v accepted, ending "+
			"negotiation", c.chanPoint, remoteProposedFee)

		// Otherwise, we've agreed on a fee for the closing transaction! We'll
		// craft the final closing transaction so we can broadcast it to the
		// network.
		matchingSig := c.priorFeeOffers[remoteProposedFee].Signature
		localSig, err := matchingSig.ToSignature()
		if err != nil {
			return nil, false, err
		}

		remoteSig, err := closeSignedMsg.Signature.ToSignature()
		if err != nil {
			return nil, false, err
		}

		closeTx, _, err := c.cfg.Channel.CompleteCooperativeClose(
			localSig, remoteSig, c.localDeliveryScript, c.remoteDeliveryScript,
			remoteProposedFee,
		)
		if err != nil {
			return nil, false, err
		}
		c.closingTx = closeTx

		// Before publishing the closing tx, we persist it to the database,
		// such that it can be republished if something goes wrong.
		err = c.cfg.Channel.MarkCoopBroadcasted(closeTx, c.locallyInitiated)
		if err != nil {
			return nil, false, err
		}

		// With the closing transaction crafted, we'll now broadcast it to the
		// network.
		chancloserLog.Infof("Broadcasting cooperative close tx: %v",
			newLogClosure(func() string {
				return spew.Sdump(closeTx)
			}),
		)

		// Create a close channel label.
		chanID := c.cfg.Channel.ShortChanID()
		closeLabel := labels.MakeLabel(
			labels.LabelTypeChannelClose, &chanID,
		)

		if err := c.cfg.BroadcastTx(closeTx, closeLabel); err != nil {
			return nil, false, err
		}

		// Finally, we'll transition to the closeFinished state, and also
		// return the final close signed message we sent. Additionally, we
		// return true for the second argument to indicate we're finished with
		// the channel closing negotiation.
		c.state = closeFinished
		matchingOffer := c.priorFeeOffers[remoteProposedFee]
		return []lnwire.Message{matchingOffer}, true, nil

	// If we received a message while in the closeFinished state, then this
	// should only be the remote party echoing the last ClosingSigned message
	// that we agreed on.
	case closeFinished:
		if _, ok := msg.(*lnwire.ClosingSigned); !ok {
			return nil, false, fmt.Errorf("expected lnwire.ClosingSigned, "+
				"instead have %v", spew.Sdump(msg))
		}

		// There's no more to do as both sides should have already broadcast
		// the closing transaction at this state.
		return nil, true, nil

	// Otherwise, we're in an unknown state, and can't proceed.
	default:
		return nil, false, ErrInvalidState
	}
}

// proposeCloseSigned attempts to propose a new signature for the closing
// transaction for a channel based on the prior fee negotiations and our current
// compromise fee.
func (c *ChanCloser) proposeCloseSigned(fee btcutil.Amount) (*lnwire.ClosingSigned, error) {
	rawSig, _, _, err := c.cfg.Channel.CreateCloseProposal(
		fee, c.localDeliveryScript, c.remoteDeliveryScript,
	)
	if err != nil {
		return nil, err
	}

	// We'll note our last signature and proposed fee so when the remote party
	// responds we'll be able to decide if we've agreed on fees or not.
	c.lastFeeProposal = fee
	parsedSig, err := lnwire.NewSigFromSignature(rawSig)
	if err != nil {
		return nil, err
	}

	chancloserLog.Infof("ChannelPoint(%v): proposing fee of %v sat to close "+
		"chan", c.chanPoint, int64(fee))

	// We'll assemble a ClosingSigned message using this information and return
	// it to the caller so we can kick off the final stage of the channel
	// closure process.
	closeSignedMsg := lnwire.NewClosingSigned(c.cid, fee, parsedSig)

	// We'll also save this close signed, in the case that the remote party
	// accepts our offer. This way, we don't have to re-sign.
	c.priorFeeOffers[fee] = closeSignedMsg

	return closeSignedMsg, nil
}

// feeInAcceptableRange returns true if the passed remote fee is deemed to be
// in an "acceptable" range to our local fee. This is an attempt at a
// compromise and to ensure that the fee negotiation has a stopping point. We
// consider their fee acceptable if it's within 30% of our fee.
func feeInAcceptableRange(localFee, remoteFee btcutil.Amount) bool {
	// If our offer is lower than theirs, then we'll accept their offer if it's
	// no more than 30% *greater* than our current offer.
	if localFee < remoteFee {
		acceptableRange := localFee + ((localFee * 3) / 10)
		return remoteFee <= acceptableRange
	}

	// If our offer is greater than theirs, then we'll accept their offer if
	// it's no more than 30% *less* than our current offer.
	acceptableRange := localFee - ((localFee * 3) / 10)
	return remoteFee >= acceptableRange
}

// ratchetFee is our step function used to inch our fee closer to something
// that both sides can agree on. If up is true, then we'll attempt to increase
// our offered fee. Otherwise, if up is false, then we'll attempt to decrease
// our offered fee.
func ratchetFee(fee btcutil.Amount, up bool) btcutil.Amount {
	// If we need to ratchet up, then we'll increase our fee by 10%.
	if up {
		return fee + ((fee * 1) / 10)
	}

	// Otherwise, we'll *decrease* our fee by 10%.
	return fee - ((fee * 1) / 10)
}

// calcCompromiseFee performs the current fee negotiation algorithm, taking
// into consideration our ideal fee based on current fee environment, the fee
// we last proposed (if any), and the fee proposed by the peer.
func calcCompromiseFee(chanPoint wire.OutPoint, ourIdealFee, lastSentFee,
	remoteFee btcutil.Amount) btcutil.Amount {

	// TODO(roasbeef): take in number of rounds as well?

	chancloserLog.Infof("ChannelPoint(%v): computing fee compromise, ideal="+
		"%v, last_sent=%v, remote_offer=%v", chanPoint, int64(ourIdealFee),
		int64(lastSentFee), int64(remoteFee))

	// Otherwise, we'll need to attempt to make a fee compromise if this is the
	// second round, and neither side has agreed on fees.
	switch {
	// If their proposed fee is identical to our ideal fee, then we'll go with
	// that as we can short circuit the fee negotiation. Similarly, if we
	// haven't sent an offer yet, we'll default to our ideal fee.
	case ourIdealFee == remoteFee || lastSentFee == 0:
		return ourIdealFee

	// If the last fee we sent, is equal to the fee the remote party is
	// offering, then we can simply return this fee as the negotiation is over.
	case remoteFee == lastSentFee:
		return lastSentFee

	// If the fee the remote party is offering is less than the last one we
	// sent, then we'll need to ratchet down in order to move our offer closer
	// to theirs.
	case remoteFee < lastSentFee:
		// If the fee is lower, but still acceptable, then we'll just return
		// this fee and end the negotiation.
		if feeInAcceptableRange(lastSentFee, remoteFee) {
			chancloserLog.Infof("ChannelPoint(%v): proposed remote fee is "+
				"close enough, capitulating", chanPoint)
			return remoteFee
		}

		// Otherwise, we'll ratchet the fee *down* using our current algorithm.
		return ratchetFee(lastSentFee, false)

	// If the fee the remote party is offering is greater than the last one we
	// sent, then we'll ratchet up in order to ensure we terminate eventually.
	case remoteFee > lastSentFee:
		// If the fee is greater, but still acceptable, then we'll just return
		// this fee in order to put an end to the negotiation.
		if feeInAcceptableRange(lastSentFee, remoteFee) {
			chancloserLog.Infof("ChannelPoint(%v): proposed remote fee is "+
				"close enough, capitulating", chanPoint)
			return remoteFee
		}

		// Otherwise, we'll ratchet the fee up using our current algorithm.
		return ratchetFee(lastSentFee, true)

	default:
		// TODO(roasbeef): fail if their fee isn't in expected range
		return remoteFee
	}
}

// ParseUpfrontShutdownAddress attempts to parse an upfront shutdown address.
// If the address is empty, it returns nil. If it successfully decoded the
// address, it returns a script that pays out to the address.
func ParseUpfrontShutdownAddress(address string,
	params *chaincfg.Params) (lnwire.DeliveryAddress, error) {

	if len(address) == 0 {
		return nil, nil
	}

	addr, err := btcutil.DecodeAddress(
		address, params,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %v", err)
	}

	if !addr.IsForNet(params) {
		return nil, fmt.Errorf("invalid address: %v is not a %s "+
			"address", addr, params.Name)
	}

	return txscript.PayToAddrScript(addr)
}

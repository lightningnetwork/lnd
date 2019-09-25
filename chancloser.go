package lnd

import (
	"fmt"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrChanAlreadyClosing is returned when a channel shutdown is
	// attempted more than once.
	ErrChanAlreadyClosing = fmt.Errorf("channel shutdown already initiated")

	// ErrChanCloseNotFinished is returned when a caller attempts to access
	// a field or function that is contingent on the channel closure
	// negotiation already being completed.
	ErrChanCloseNotFinished = fmt.Errorf("close negotiation not finished")

	// ErrInvalidState is returned when the closing state machine receives
	// a message while it is in an unknown state.
	ErrInvalidState = fmt.Errorf("invalid state")
)

// closeState represents all the possible states the channel closer state
// machine can be in. Each message will either advance to the next state, or
// remain at the current state. Once the state machine reaches a state of
// closeFinished, then negotiation is over.
type closeState uint8

const (
	// closeIdle is the initial starting state. In this state, the state
	// machine has been instantiated, but no state transitions have been
	// attempted. If a state machine receives a message while in this
	// state, then it is the responder to an initiated cooperative channel
	// closure.
	closeIdle closeState = iota

	// closeShutdownInitiated is the state that's transitioned to once the
	// initiator of a closing workflow sends the shutdown message. At this
	// point, they're waiting for the remote party to respond with their
	// own shutdown message. After which, they'll both enter the fee
	// negotiation phase.
	closeShutdownInitiated

	// closeFeeNegotiation is the third, and most persistent state. Both
	// parties enter this state after they've sent and received a shutdown
	// message. During this phase, both sides will send monotonically
	// increasing fee requests until one side accepts the last fee rate
	// offered by the other party. In this case, the party will broadcast
	// the closing transaction, and send the accepted fee to the remote
	// party. This then causes a shift into the closeFinished state.
	closeFeeNegotiation

	// closeFinished is the final state of the state machine. In this,
	// state a side has accepted a fee offer and has broadcast the valid
	// closing transaction to the network. During this phase, the closing
	// transaction becomes available for examination.
	closeFinished
)

// chanCloseCfg holds all the items that a channelCloser requires to carry out
// its duties.
type chanCloseCfg struct {
	// channel is the channel that should be closed.
	channel *lnwallet.LightningChannel

	// unregisterChannel is a function closure that allows the
	// channelCloser to re-register a channel. Once this has been done, no
	// further HTLC's should be routed through the channel.
	unregisterChannel func(lnwire.ChannelID)

	// broadcastTx broadcasts the passed transaction to the network.
	broadcastTx func(*wire.MsgTx) error

	// disableChannel disables a channel, resulting in it not being able to
	// forward payments.
	disableChannel func(wire.OutPoint) error

	// quit is a channel that should be sent upon in the occasion the state
	// machine should cease all progress and shutdown.
	quit chan struct{}
}

// channelCloser is a state machine that handles the cooperative channel
// closure procedure. This includes shutting down a channel, marking it
// ineligible for routing HTLC's, negotiating fees with the remote party, and
// finally broadcasting the fully signed closure transaction to the network.
type channelCloser struct {
	// state is the current state of the state machine.
	state closeState

	// cfg holds the configuration for this channelCloser instance.
	cfg chanCloseCfg

	// chanPoint is the full channel point of the target channel.
	chanPoint wire.OutPoint

	// cid is the full channel ID of the target channel.
	cid lnwire.ChannelID

	// negotiationHeight is the height that the fee negotiation begun at.
	negotiationHeight uint32

	// closingTx is the final, fully signed closing transaction. This will
	// only be populated once the state machine shifts to the closeFinished
	// state.
	closingTx *wire.MsgTx

	// idealFeeSat is the ideal fee that the state machine should initially
	// offer when starting negotiation. This will be used as a baseline.
	idealFeeSat btcutil.Amount

	// lastFeeProposal is the last fee that we proposed to the remote
	// party. We'll use this as a pivot point to rachet our next offer up,
	// or down, or simply accept the remote party's prior offer.
	lastFeeProposal btcutil.Amount

	// priorFeeOffers is a map that keeps track of all the proposed fees
	// that we've offered during the fee negotiation. We use this map to
	// cut the negotiation early if the remote party ever sends an offer
	// that we've sent in the past. Once negotiation terminates, we can
	// extract the prior signature of our accepted offer from this map.
	//
	// TODO(roasbeef): need to ensure if they broadcast w/ any of our prior
	// sigs, we are aware of
	priorFeeOffers map[btcutil.Amount]*lnwire.ClosingSigned

	// closeReq is the initial closing request. This will only be populated
	// if we're the initiator of this closing negotiation.
	//
	// TODO(roasbeef): abstract away
	closeReq *htlcswitch.ChanClose

	// localDeliveryScript is the script that we'll send our settled
	// channel funds to.
	localDeliveryScript []byte

	// remoteDeliveryScript is the script that we'll send the remote
	// party's settled channel funds to.
	remoteDeliveryScript []byte
}

// newChannelCloser creates a new instance of the channel closure given the
// passed configuration, and delivery+fee preference. The final argument should
// only be populated iff, we're the initiator of this closing request.
func newChannelCloser(cfg chanCloseCfg, deliveryScript []byte,
	idealFeePerKw lnwallet.SatPerKWeight, negotiationHeight uint32,
	closeReq *htlcswitch.ChanClose) *channelCloser {

	// Given the target fee-per-kw, we'll compute what our ideal _total_
	// fee will be starting at for this fee negotiation.
	//
	// TODO(roasbeef): should factor in minimal commit
	idealFeeSat := cfg.channel.CalcFee(idealFeePerKw)

	// If this fee is greater than the fee currently present within the
	// commitment transaction, then we'll clamp it down to be within the
	// proper range.
	//
	// TODO(roasbeef): clamp fee func?
	channelCommitFee := cfg.channel.StateSnapshot().CommitFee
	if idealFeeSat > channelCommitFee {
		peerLog.Infof("Ideal starting fee of %v is greater than "+
			"commit fee of %v, clamping", int64(idealFeeSat),
			int64(channelCommitFee))

		idealFeeSat = channelCommitFee
	}

	peerLog.Infof("Ideal fee for closure of ChannelPoint(%v) is: %v sat",
		cfg.channel.ChannelPoint(), int64(idealFeeSat))

	cid := lnwire.NewChanIDFromOutPoint(cfg.channel.ChannelPoint())
	return &channelCloser{
		closeReq:            closeReq,
		state:               closeIdle,
		chanPoint:           *cfg.channel.ChannelPoint(),
		cid:                 cid,
		cfg:                 cfg,
		negotiationHeight:   negotiationHeight,
		idealFeeSat:         idealFeeSat,
		localDeliveryScript: deliveryScript,
		priorFeeOffers:      make(map[btcutil.Amount]*lnwire.ClosingSigned),
	}
}

// initChanShutdown begins the shutdown process by un-registering the channel,
// and creating a valid shutdown message to our target delivery address.
func (c *channelCloser) initChanShutdown() (*lnwire.Shutdown, error) {
	// With both items constructed we'll now send the shutdown message for
	// this particular channel, advertising a shutdown request to our
	// desired closing script.
	shutdown := lnwire.NewShutdown(c.cid, c.localDeliveryScript)

	// TODO(roasbeef): err if channel has htlc's?

	// Before returning the shutdown message, we'll unregister the channel
	// to ensure that it isn't seen as usable within the system.
	//
	// TODO(roasbeef): fail if err?
	c.cfg.unregisterChannel(c.cid)

	peerLog.Infof("ChannelPoint(%v): sending shutdown message", c.chanPoint)

	return shutdown, nil
}

// ShutdownChan is the first method that's to be called by the initiator of the
// cooperative channel closure. This message returns the shutdown message to
// send to the remote party. Upon completion, we enter the
// closeShutdownInitiated phase as we await a response.
func (c *channelCloser) ShutdownChan() (*lnwire.Shutdown, error) {
	// If we attempt to shutdown the channel for the first time, and we're
	// not in the closeIdle state, then the caller made an error.
	if c.state != closeIdle {
		return nil, ErrChanAlreadyClosing
	}

	peerLog.Infof("ChannelPoint(%v): initiating shutdown of", c.chanPoint)

	shutdownMsg, err := c.initChanShutdown()
	if err != nil {
		return nil, err
	}

	// With the opening steps complete, we'll transition into the
	// closeShutdownInitiated state. In this state, we'll wait until the
	// other party sends their version of the shutdown message.
	c.state = closeShutdownInitiated

	// Finally, we'll return the shutdown message to the caller so it can
	// send it to the remote peer.
	return shutdownMsg, nil
}

// ClosingTx returns the fully signed, final closing transaction.
//
// NOTE: This transaction is only available if the state machine is in the
// closeFinished state.
func (c *channelCloser) ClosingTx() (*wire.MsgTx, error) {
	// If the state machine hasn't finished closing the channel then we'll
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
func (c *channelCloser) CloseRequest() *htlcswitch.ChanClose {
	return c.closeReq
}

// ProcessCloseMsg attempts to process the next message in the closing series.
// This method will update the state accordingly and return two primary values:
// the next set of messages to be sent, and a bool indicating if the fee
// negotiation process has completed. If the second value is true, then this
// means the channelCloser can be garbage collected.
func (c *channelCloser) ProcessCloseMsg(msg lnwire.Message) ([]lnwire.Message, bool, error) {
	switch c.state {

	// If we're in the close idle state, and we're receiving a channel
	// closure related message, then this indicates that we're on the
	// receiving side of an initiated channel closure.
	case closeIdle:
		// First, we'll assert that we have a channel shutdown message,
		// otherwise, this is an attempted invalid state transition.
		shutDownMsg, ok := msg.(*lnwire.Shutdown)
		if !ok {
			return nil, false, fmt.Errorf("expected lnwire.Shutdown, "+
				"instead have %v", spew.Sdump(msg))
		}

		// Next, we'll note the other party's preference for their
		// delivery address. We'll use this when we craft the closure
		// transaction.
		c.remoteDeliveryScript = shutDownMsg.Address

		// We'll generate a shutdown message of our own to send across
		// the wire.
		localShutdown, err := c.initChanShutdown()
		if err != nil {
			return nil, false, err
		}

		peerLog.Infof("ChannelPoint(%v): Responding to shutdown",
			c.chanPoint)

		msgsToSend := make([]lnwire.Message, 0, 2)
		msgsToSend = append(msgsToSend, localShutdown)

		// After the other party receives this message, we'll actually
		// start the final stage of the closure process: fee
		// negotiation.  So we'll update our internal state to reflect
		// this, so we can handle the next message sent.
		c.state = closeFeeNegotiation

		// We'll also craft our initial close proposal in order to keep
		// the negotiation moving, but only if we're the negotiator.
		if c.cfg.channel.IsInitiator() {
			closeSigned, err := c.proposeCloseSigned(c.idealFeeSat)
			if err != nil {
				return nil, false, err
			}
			msgsToSend = append(msgsToSend, closeSigned)
		}

		// We'll return both sets of messages to send to the remote
		// party to kick off the fee negotiation process.
		return msgsToSend, false, nil

	// If we just initiated a channel shutdown, and we receive a new
	// message, then this indicates the other party is ready to shutdown as
	// well. In this state we'll send our first signature.
	case closeShutdownInitiated:
		// First, we'll assert that we have a channel shutdown message,
		// otherwise, this is an attempted invalid state transition.
		shutDownMsg, ok := msg.(*lnwire.Shutdown)
		if !ok {
			return nil, false, fmt.Errorf("expected lnwire.Shutdown, "+
				"instead have %v", spew.Sdump(msg))
		}

		// Now that we know this is a valid shutdown message, we'll
		// record their preferred delivery closing script.
		c.remoteDeliveryScript = shutDownMsg.Address

		// At this point, we can now start the fee negotiation state,
		// by constructing and sending our initial signature for what
		// we think the closing transaction should look like.
		c.state = closeFeeNegotiation

		peerLog.Infof("ChannelPoint(%v): shutdown response received, "+
			"entering fee negotiation", c.chanPoint)

		// Starting with our ideal fee rate, we'll create an initial
		// closing proposal, but only if we're the initiator, as
		// otherwise, the other party will send their first proposal
		// first.
		if c.cfg.channel.IsInitiator() {
			closeSigned, err := c.proposeCloseSigned(c.idealFeeSat)
			if err != nil {
				return nil, false, err
			}

			return []lnwire.Message{closeSigned}, false, nil
		}

		return nil, false, nil

	// If we're receiving a message while we're in the fee negotiation
	// phase, then this indicates the remote party is responding to a closed
	// signed message we sent, or kicking off the process with their own.
	case closeFeeNegotiation:
		// First, we'll assert that we're actually getting a
		// CloseSigned message, otherwise an invalid state transition
		// was attempted.
		closeSignedMsg, ok := msg.(*lnwire.ClosingSigned)
		if !ok {
			return nil, false, fmt.Errorf("expected lnwire.ClosingSigned, "+
				"instead have %v", spew.Sdump(msg))
		}

		// We'll compare the proposed total fee, to what we've proposed
		// during the negotiations, if it doesn't match any of our
		// prior offers, then we'll attempt to rachet the fee closer to
		remoteProposedFee := closeSignedMsg.FeeSatoshis
		if _, ok := c.priorFeeOffers[remoteProposedFee]; !ok {
			// We'll now attempt to rachet towards a fee deemed
			// acceptable by both parties, factoring in our ideal
			// fee rate, and the last proposed fee by both sides.
			feeProposal := calcCompromiseFee(c.chanPoint,
				c.idealFeeSat, c.lastFeeProposal,
				remoteProposedFee,
			)

			// With our new fee proposal calculated, we'll craft a
			// new close signed signature to send to the other
			// party so we can continue the fee negotiation
			// process.
			closeSigned, err := c.proposeCloseSigned(feeProposal)
			if err != nil {
				return nil, false, err
			}

			// If the compromise fee doesn't match what the peer
			// proposed, then we'll return this latest close signed
			// message so we continue negotiation.
			if feeProposal != remoteProposedFee {
				peerLog.Debugf("ChannelPoint(%v): close tx "+
					"fee disagreement, continuing negotiation",
					c.chanPoint)
				return []lnwire.Message{closeSigned}, false, nil
			}
		}

		peerLog.Infof("ChannelPoint(%v) fee of %v accepted, ending "+
			"negotiation", c.chanPoint, remoteProposedFee)

		// Otherwise, we've agreed on a fee for the closing
		// transaction!  We'll craft the final closing transaction so
		// we can broadcast it to the network.
		matchingSig := c.priorFeeOffers[remoteProposedFee].Signature
		localSigBytes := matchingSig.ToSignatureBytes()
		localSig := append(localSigBytes, byte(txscript.SigHashAll))

		remoteSigBytes := closeSignedMsg.Signature.ToSignatureBytes()
		remoteSig := append(remoteSigBytes, byte(txscript.SigHashAll))

		closeTx, _, err := c.cfg.channel.CompleteCooperativeClose(
			localSig, remoteSig, c.localDeliveryScript,
			c.remoteDeliveryScript, remoteProposedFee,
		)
		if err != nil {
			return nil, false, err
		}
		c.closingTx = closeTx

		// Before closing, we'll attempt to send a disable update for
		// the channel. We do so before closing the channel as otherwise
		// the current edge policy won't be retrievable from the graph.
		if err := c.cfg.disableChannel(c.chanPoint); err != nil {
			peerLog.Warnf("Unable to disable channel %v on "+
				"close: %v", c.chanPoint, err)
		}

		// Before publishing the closing tx, we persist it to the
		// database, such that it can be republished if something goes
		// wrong.
		err = c.cfg.channel.MarkCommitmentBroadcasted(closeTx)
		if err != nil {
			return nil, false, err
		}

		// With the closing transaction crafted, we'll now broadcast it
		// to the network.
		peerLog.Infof("Broadcasting cooperative close tx: %v",
			newLogClosure(func() string {
				return spew.Sdump(closeTx)
			}))
		if err := c.cfg.broadcastTx(closeTx); err != nil {
			return nil, false, err
		}

		// Finally, we'll transition to the closeFinished state, and
		// also return the final close signed message we sent.
		// Additionally, we return true for the second argument to
		// indicate we're finished with the channel closing
		// negotiation.
		c.state = closeFinished
		matchingOffer := c.priorFeeOffers[remoteProposedFee]
		return []lnwire.Message{matchingOffer}, true, nil

	// If we receive a message while in the closeFinished state, then this
	// should only be the remote party echoing the last ClosingSigned
	// message that we agreed on.
	case closeFinished:
		if _, ok := msg.(*lnwire.ClosingSigned); !ok {
			return nil, false, fmt.Errorf("expected "+
				"lnwire.ClosingSigned, instead have %v",
				spew.Sdump(msg))
		}

		// There's no more to do as both sides should have already
		// broadcast the closing transaction at this state.
		return nil, true, nil

	// Otherwise, we're in an unknown state, and can't proceed.
	default:
		return nil, false, ErrInvalidState
	}
}

// proposeCloseSigned attempts to propose a new signature for the closing
// transaction for a channel based on the prior fee negotiations and our
// current compromise fee.
func (c *channelCloser) proposeCloseSigned(fee btcutil.Amount) (*lnwire.ClosingSigned, error) {

	rawSig, _, _, err := c.cfg.channel.CreateCloseProposal(
		fee, c.localDeliveryScript, c.remoteDeliveryScript,
	)
	if err != nil {
		return nil, err
	}

	// We'll note our last signature and proposed fee so when the remote
	// party responds we'll be able to decide if we've agreed on fees or
	// not.
	c.lastFeeProposal = fee
	parsedSig, err := lnwire.NewSigFromRawSignature(rawSig)
	if err != nil {
		return nil, err
	}

	peerLog.Infof("ChannelPoint(%v): proposing fee of %v sat to close "+
		"chan", c.chanPoint, int64(fee))

	// We'll assemble a ClosingSigned message using this information and
	// return it to the caller so we can kick off the final stage of the
	// channel closure project.
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
	// If our offer is lower than theirs, then we'll accept their
	// offer if it's no more than 30% *greater* than our current
	// offer.
	if localFee < remoteFee {
		acceptableRange := localFee + ((localFee * 3) / 10)
		return remoteFee <= acceptableRange
	}

	// If our offer is greater than theirs, then we'll accept their offer
	// if it's no more than 30% *less* than our current offer.
	acceptableRange := localFee - ((localFee * 3) / 10)
	return remoteFee >= acceptableRange
}

// rachetFee is our step function used to inch our fee closer to something that
// both sides can agree on. If up is true, then we'll attempt to increase our
// offered fee. Otherwise, if up is false, then we'll attempt to decrease our
// offered fee.
func rachetFee(fee btcutil.Amount, up bool) btcutil.Amount {
	// If we need to rachet up, then we'll increase our fee by 10%.
	if up {
		return fee + ((fee * 1) / 10)
	}

	// Otherwise, we'll *decrease* our fee by 10%.
	return fee - ((fee * 1) / 10)
}

// calcCompromiseFee performs the current fee negotiation algorithm, taking
// into consideration our ideal fee based on current fee environment, the fee
// we last proposed (if any), and the fee proposed by the peer.
func calcCompromiseFee(chanPoint wire.OutPoint,
	ourIdealFee, lastSentFee, remoteFee btcutil.Amount) btcutil.Amount {

	// TODO(roasbeef): take in number of rounds as well?

	peerLog.Infof("ChannelPoint(%v): computing fee compromise, ideal=%v, "+
		"last_sent=%v, remote_offer=%v", chanPoint, int64(ourIdealFee),
		int64(lastSentFee), int64(remoteFee))

	// Otherwise, we'll need to attempt to make a fee compromise if this is
	// the second round, and neither side has agreed on fees.
	switch {

	// If their proposed fee is identical to our ideal fee, then we'll go
	// with that as we can short circuit the fee negotiation. Similarly, if
	// we haven't sent an offer yet, we'll default to our ideal fee.
	case ourIdealFee == remoteFee || lastSentFee == 0:
		return ourIdealFee

	// If the last fee we sent, is equal to the fee the remote party is
	// offering, then we can simply return this fee as the negotiation is
	// over.
	case remoteFee == lastSentFee:
		return lastSentFee

	// If the fee the remote party is offering is less than the last one we
	// sent, then we'll need to rachet down in order to move our offer
	// closer to theirs.
	case remoteFee < lastSentFee:
		// If the fee is lower, but still acceptable, then we'll just
		// return this fee and end the negotiation.
		if feeInAcceptableRange(lastSentFee, remoteFee) {
			peerLog.Infof("ChannelPoint(%v): proposed remote fee "+
				"is close enough, capitulating", chanPoint)
			return remoteFee
		}

		// Otherwise, we'll rachet the fee *down* using our current
		// algorithm.
		return rachetFee(lastSentFee, false)

	// If the fee the remote party is offering is greater than the last one
	// we sent, then we'll rachet up in order to ensure we terminate
	// eventually.
	case remoteFee > lastSentFee:
		// If the fee is greater, but still acceptable, then we'll just
		// return this fee in order to put an end to the negotiation.
		if feeInAcceptableRange(lastSentFee, remoteFee) {
			peerLog.Infof("ChannelPoint(%v): proposed remote fee "+
				"is close enough, capitulating", chanPoint)
			return remoteFee
		}

		// Otherwise, we'll rachet the fee up using our current
		// algorithm.
		return rachetFee(lastSentFee, true)

	default:
		// TODO(roasbeef): fail if their fee isn't in expected range
		return remoteFee
	}
}

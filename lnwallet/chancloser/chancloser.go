package chancloser

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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

	// ErrInvalidState is returned when the closing state machine receives a
	// message while it is in an unknown state.
	ErrInvalidState = fmt.Errorf("invalid state")

	// ErrUpfrontShutdownScriptMismatch is returned when a peer or end user
	// provides a cooperative close script which does not match the upfront
	// shutdown script previously set for that party.
	ErrUpfrontShutdownScriptMismatch = fmt.Errorf("shutdown script does not " +
		"match upfront shutdown script")

	// ErrProposalExceedsMaxFee is returned when as the initiator, the
	// latest fee proposal sent by the responder exceed our max fee.
	// responder.
	ErrProposalExceedsMaxFee = fmt.Errorf("latest fee proposal exceeds " +
		"max fee")

	// ErrInvalidShutdownScript is returned when we receive an address from
	// a peer that isn't either a p2wsh or p2tr address.
	ErrInvalidShutdownScript = fmt.Errorf("invalid shutdown script")

	// errNoShutdownNonce is returned when a shutdown message is received
	// w/o a nonce for a taproot channel.
	errNoShutdownNonce = fmt.Errorf("shutdown nonce not populated")
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

	// closeAwaitingFlush is the state that's transitioned to once both
	// Shutdown messages have been exchanged but we are waiting for the
	// HTLCs to clear out of the channel.
	closeAwaitingFlush

	// closeFeeNegotiation is the third, and most persistent state. Both
	// parties enter this state after they've sent and received a shutdown
	// message. During this phase, both sides will send monotonically
	// increasing fee requests until one side accepts the last fee rate
	// offered by the other party. In this case, the party will broadcast
	// the closing transaction, and send the accepted fee to the remote
	// party. This then causes a shift into the closeFinished state.
	closeFeeNegotiation

	// closeFinished is the final state of the state machine. In this state,
	// a side has accepted a fee offer and has broadcast the valid closing
	// transaction to the network. During this phase, the closing
	// transaction becomes available for examination.
	closeFinished
)

const (
	// defaultMaxFeeMultiplier is a multiplier we'll apply to the ideal fee
	// of the initiator, to decide when the negotiated fee is too high. By
	// default, we want to bail out if we attempt to negotiate a fee that's
	// 3x higher than our max fee.
	defaultMaxFeeMultiplier = 3
)

// DeliveryAddrWithKey wraps a normal delivery addr, but also includes the
// internal key for the delivery addr if known.
type DeliveryAddrWithKey struct {
	// DeliveryAddress is the raw, serialized pkScript of the delivery
	// address.
	lnwire.DeliveryAddress

	// InternalKey is the Taproot internal key of the delivery address, if
	// the address is a P2TR output.
	InternalKey fn.Option[btcec.PublicKey]
}

// ChanCloseCfg holds all the items that a ChanCloser requires to carry out its
// duties.
type ChanCloseCfg struct {
	// Channel is the channel that should be closed.
	Channel Channel

	// MusigSession is used to handle generating musig2 nonces, and also
	// creating the proper set of closing options for taproot channels.
	MusigSession MusigSession

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

	// AuxCloser is an optional interface that can be used to modify the
	// way the co-op close process proceeds.
	AuxCloser fn.Option[AuxChanCloser]
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

	// closingTx is the final, fully signed closing transaction. This will
	// only be populated once the state machine shifts to the closeFinished
	// state.
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
	// We'll use this as a pivot point to ratchet our next offer up, down,
	// or simply accept the remote party's prior offer.
	lastFeeProposal btcutil.Amount

	// priorFeeOffers is a map that keeps track of all the proposed fees
	// that we've offered during the fee negotiation. We use this map to cut
	// the negotiation early if the remote party ever sends an offer that
	// we've sent in the past. Once negotiation terminates, we can extract
	// the prior signature of our accepted offer from this map.
	//
	// TODO(roasbeef): need to ensure if they broadcast w/ any of our prior
	// sigs, we are aware of
	priorFeeOffers map[btcutil.Amount]*lnwire.ClosingSigned

	// closeReq is the initial closing request. This will only be populated
	// if we're the initiator of this closing negotiation.
	//
	// TODO(roasbeef): abstract away
	closeReq *htlcswitch.ChanClose

	// localDeliveryScript is the script that we'll send our settled channel
	// funds to.
	localDeliveryScript []byte

	// localInternalKey is the local delivery address Taproot internal key,
	// if the local delivery script is a P2TR output.
	localInternalKey fn.Option[btcec.PublicKey]

	// remoteDeliveryScript is the script that we'll send the remote party's
	// settled channel funds to.
	remoteDeliveryScript []byte

	// closer is ChannelParty who initiated the coop close
	closer lntypes.ChannelParty

	// cachedClosingSigned is a cached copy of a received ClosingSigned that
	// we use to handle a specific race condition caused by the independent
	// message processing queues.
	cachedClosingSigned fn.Option[lnwire.ClosingSigned]

	// localCloseOutput is the local output on the closing transaction that
	// the local party should be paid to. This will only be populated if the
	// local balance isn't dust.
	localCloseOutput fn.Option[CloseOutput]

	// remoteCloseOutput is the remote output on the closing transaction
	// that the remote party should be paid to. This will only be populated
	// if the remote balance isn't dust.
	remoteCloseOutput fn.Option[CloseOutput]

	// auxOutputs are the optional additional outputs that might be added to
	// the closing transaction.
	auxOutputs fn.Option[AuxCloseOutputs]
}

// calcCoopCloseFee computes an "ideal" absolute co-op close fee given the
// delivery scripts of both parties and our ideal fee rate.
func calcCoopCloseFee(chanType channeldb.ChannelType,
	localOutput, remoteOutput *wire.TxOut,
	idealFeeRate chainfee.SatPerKWeight) btcutil.Amount {

	var weightEstimator input.TxWeightEstimator

	if chanType.IsTaproot() {
		weightEstimator.AddWitnessInput(
			input.TaprootSignatureWitnessSize,
		)
	} else {
		weightEstimator.AddWitnessInput(input.MultiSigWitnessSize)
	}

	// One of these outputs might be dust, so we'll skip adding it to our
	// mock transaction, so the fees are more accurate.
	if localOutput != nil {
		weightEstimator.AddTxOutput(localOutput)
	}
	if remoteOutput != nil {
		weightEstimator.AddTxOutput(remoteOutput)
	}

	totalWeight := weightEstimator.Weight()

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

	return calcCoopCloseFee(chanType, localTxOut, remoteTxOut, idealFeeRate)
}

// NewChanCloser creates a new instance of the channel closure given the passed
// configuration, and delivery+fee preference. The final argument should only
// be populated iff, we're the initiator of this closing request.
func NewChanCloser(cfg ChanCloseCfg, deliveryScript DeliveryAddrWithKey,
	idealFeePerKw chainfee.SatPerKWeight, negotiationHeight uint32,
	closeReq *htlcswitch.ChanClose,
	closer lntypes.ChannelParty) *ChanCloser {

	chanPoint := cfg.Channel.ChannelPoint()
	cid := lnwire.NewChanIDFromOutPoint(chanPoint)
	return &ChanCloser{
		closeReq:            closeReq,
		state:               closeIdle,
		chanPoint:           chanPoint,
		cid:                 cid,
		cfg:                 cfg,
		negotiationHeight:   negotiationHeight,
		idealFeeRate:        idealFeePerKw,
		localInternalKey:    deliveryScript.InternalKey,
		localDeliveryScript: deliveryScript.DeliveryAddress,
		priorFeeOffers: make(
			map[btcutil.Amount]*lnwire.ClosingSigned,
		),
		closer: closer,
	}
}

// initFeeBaseline computes our ideal fee rate, and also the largest fee we'll
// accept given information about the delivery script of the remote party.
func (c *ChanCloser) initFeeBaseline() {
	// Depending on if a balance ends up being dust or not, we'll pass a
	// nil TxOut into the EstimateFee call which can handle it.
	var localTxOut, remoteTxOut *wire.TxOut
	if isDust, _ := c.cfg.Channel.LocalBalanceDust(); !isDust {
		localTxOut = &wire.TxOut{
			PkScript: c.localDeliveryScript,
			Value:    0,
		}
	}
	if isDust, _ := c.cfg.Channel.RemoteBalanceDust(); !isDust {
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

	// TODO(ziggie): Make sure the ideal fee is not higher than the max fee.
	// Either error out or cap the ideal fee at the max fee.

	chancloserLog.Infof("Ideal fee for closure of ChannelPoint(%v) "+
		"is: %v sat (max_fee=%v sat)", c.cfg.Channel.ChannelPoint(),
		int64(c.idealFeeSat), int64(c.maxFee))
}

// initChanShutdown begins the shutdown process by un-registering the channel,
// and creating a valid shutdown message to our target delivery address.
func (c *ChanCloser) initChanShutdown() (*lnwire.Shutdown, error) {
	// With both items constructed we'll now send the shutdown message for
	// this particular channel, advertising a shutdown request to our
	// desired closing script.
	shutdown := lnwire.NewShutdown(c.cid, c.localDeliveryScript)

	// At this point, we'll check to see if we have any custom records to
	// add to the shutdown message.
	err := fn.MapOptionZ(c.cfg.AuxCloser, func(a AuxChanCloser) error {
		shutdownCustomRecords, err := a.ShutdownBlob(AuxShutdownReq{
			ChanPoint:   c.chanPoint,
			ShortChanID: c.cfg.Channel.ShortChanID(),
			Initiator:   c.cfg.Channel.IsInitiator(),
			InternalKey: c.localInternalKey,
			CommitBlob:  c.cfg.Channel.LocalCommitmentBlob(),
			FundingBlob: c.cfg.Channel.FundingBlob(),
		})
		if err != nil {
			return err
		}

		shutdownCustomRecords.WhenSome(func(cr lnwire.CustomRecords) {
			shutdown.CustomRecords = cr
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	// If this is a taproot channel, then we'll need to also generate a
	// nonce that'll be used sign the co-op close transaction offer.
	if c.cfg.Channel.ChanType().IsTaproot() {
		firstClosingNonce, err := c.cfg.MusigSession.ClosingNonce()
		if err != nil {
			return nil, err
		}

		shutdown.ShutdownNonce = lnwire.SomeShutdownNonce(
			firstClosingNonce.PubNonce,
		)

		chancloserLog.Infof("Initiating shutdown w/ nonce: %v",
			lnutils.SpewLogClosure(firstClosingNonce.PubNonce))
	}

	// Before closing, we'll attempt to send a disable update for the
	// channel.  We do so before closing the channel as otherwise the
	// current edge policy won't be retrievable from the graph.
	if err := c.cfg.DisableChannel(c.chanPoint); err != nil {
		chancloserLog.Warnf("Unable to disable channel %v on close: %v",
			c.chanPoint, err)
	}

	chancloserLog.Infof("ChannelPoint(%v): sending shutdown message",
		c.chanPoint)

	// At this point, we persist any relevant info regarding the Shutdown
	// message we are about to send in order to ensure that if a
	// re-establish occurs then we will re-send the same Shutdown message.
	shutdownInfo := channeldb.NewShutdownInfo(
		c.localDeliveryScript, c.closer.IsLocal(),
	)
	err = c.cfg.Channel.MarkShutdownSent(shutdownInfo)
	if err != nil {
		return nil, err
	}

	// We'll track our local close output, even if it's dust in BTC terms,
	// it might still carry value in custom channel terms.
	_, dustAmt := c.cfg.Channel.LocalBalanceDust()
	localBalance, _ := c.cfg.Channel.CommitBalances()
	c.localCloseOutput = fn.Some(CloseOutput{
		Amt:             localBalance,
		DustLimit:       dustAmt,
		PkScript:        c.localDeliveryScript,
		ShutdownRecords: shutdown.CustomRecords,
	})

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
	// TODO(roasbeef): remove this
	return c.cfg.Channel.(*lnwallet.LightningChannel)
}

// NegotiationHeight returns the negotiation height.
func (c *ChanCloser) NegotiationHeight() uint32 {
	return c.negotiationHeight
}

// LocalCloseOutput returns the local close output.
func (c *ChanCloser) LocalCloseOutput() fn.Option[CloseOutput] {
	return c.localCloseOutput
}

// RemoteCloseOutput returns the remote close output.
func (c *ChanCloser) RemoteCloseOutput() fn.Option[CloseOutput] {
	return c.remoteCloseOutput
}

// AuxOutputs returns optional extra outputs.
func (c *ChanCloser) AuxOutputs() fn.Option[AuxCloseOutputs] {
	return c.auxOutputs
}

// validateShutdownScript attempts to match and validate the script provided in
// our peer's shutdown message with the upfront shutdown script we have on
// record. For any script specified, we also make sure it matches our
// requirements. If no upfront shutdown script was set, we do not need to
// enforce option upfront shutdown, so the function returns early. If an
// upfront script is set, we check whether it matches the script provided by
// our peer. If they do not match, we use the disconnect function provided to
// disconnect from the peer.
func validateShutdownScript(upfrontScript, peerScript lnwire.DeliveryAddress,
	netParams *chaincfg.Params) error {

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

		return ErrUpfrontShutdownScriptMismatch
	}

	return nil
}

// ReceiveShutdown takes a raw Shutdown message and uses it to try and advance
// the ChanCloser state machine, failing if it is coming in at an invalid time.
// If appropriate, it will also generate a Shutdown message of its own to send
// out to the peer. It is possible for this method to return None when no error
// occurred.
func (c *ChanCloser) ReceiveShutdown(msg lnwire.Shutdown) (
	fn.Option[lnwire.Shutdown], error) {

	noShutdown := fn.None[lnwire.Shutdown]()

	// We'll track their remote close output, even if it's dust in BTC
	// terms, it might still carry value in custom channel terms.
	_, dustAmt := c.cfg.Channel.RemoteBalanceDust()
	_, remoteBalance := c.cfg.Channel.CommitBalances()
	c.remoteCloseOutput = fn.Some(CloseOutput{
		Amt:             remoteBalance,
		DustLimit:       dustAmt,
		PkScript:        msg.Address,
		ShutdownRecords: msg.CustomRecords,
	})

	switch c.state {
	// If we're in the close idle state, and we're receiving a channel
	// closure related message, then this indicates that we're on the
	// receiving side of an initiated channel closure.
	case closeIdle:
		// As we're the responder to this shutdown (the other party
		// wants to close), we'll check if this is a frozen channel or
		// not. If the channel is frozen and we were not also the
		// initiator of the channel opening, then we'll deny their close
		// attempt.
		chanInitiator := c.cfg.Channel.IsInitiator()
		if !chanInitiator {
			absoluteThawHeight, err :=
				c.cfg.Channel.AbsoluteThawHeight()
			if err != nil {
				return noShutdown, err
			}
			if c.negotiationHeight < absoluteThawHeight {
				return noShutdown, fmt.Errorf("initiator "+
					"attempting to co-op close frozen "+
					"ChannelPoint(%v) (current_height=%v, "+
					"thaw_height=%v)", c.chanPoint,
					c.negotiationHeight, absoluteThawHeight)
			}
		}

		// If the remote node opened the channel with option upfront
		// shutdown script, check that the script they provided matches.
		if err := validateShutdownScript(
			c.cfg.Channel.RemoteUpfrontShutdownScript(),
			msg.Address, c.cfg.ChainParams,
		); err != nil {
			return noShutdown, err
		}

		// Once we have checked that the other party has not violated
		// option upfront shutdown we set their preference for delivery
		// address. We'll use this when we craft the closure
		// transaction.
		c.remoteDeliveryScript = msg.Address

		// We'll generate a shutdown message of our own to send across
		// the wire.
		localShutdown, err := c.initChanShutdown()
		if err != nil {
			return noShutdown, err
		}

		// If this is a taproot channel, then we'll want to stash the
		// remote nonces so we can properly create a new musig
		// session for signing.
		if c.cfg.Channel.ChanType().IsTaproot() {
			shutdownNonce, err := msg.ShutdownNonce.UnwrapOrErrV(
				errNoShutdownNonce,
			)
			if err != nil {
				return noShutdown, err
			}

			c.cfg.MusigSession.InitRemoteNonce(&musig2.Nonces{
				PubNonce: shutdownNonce,
			})
		}

		chancloserLog.Infof("ChannelPoint(%v): responding to shutdown",
			c.chanPoint)

		// After the other party receives this message, we'll actually
		// start the final stage of the closure process: fee
		// negotiation. So we'll update our internal state to reflect
		// this, so we can handle the next message sent.
		c.state = closeAwaitingFlush

		return fn.Some(*localShutdown), err

	case closeShutdownInitiated:
		// If the remote node opened the channel with option upfront
		// shutdown script, check that the script they provided matches.
		if err := validateShutdownScript(
			c.cfg.Channel.RemoteUpfrontShutdownScript(),
			msg.Address, c.cfg.ChainParams,
		); err != nil {
			return noShutdown, err
		}

		// Now that we know this is a valid shutdown message and
		// address, we'll record their preferred delivery closing
		// script.
		c.remoteDeliveryScript = msg.Address

		// At this point, we can now start the fee negotiation state, by
		// constructing and sending our initial signature for what we
		// think the closing transaction should look like.
		c.state = closeAwaitingFlush

		// If this is a taproot channel, then we'll want to stash the
		// local+remote nonces so we can properly create a new musig
		// session for signing.
		if c.cfg.Channel.ChanType().IsTaproot() {
			shutdownNonce, err := msg.ShutdownNonce.UnwrapOrErrV(
				errNoShutdownNonce,
			)
			if err != nil {
				return noShutdown, err
			}

			c.cfg.MusigSession.InitRemoteNonce(&musig2.Nonces{
				PubNonce: shutdownNonce,
			})
		}

		chancloserLog.Infof("ChannelPoint(%v): shutdown response "+
			"received, entering fee negotiation", c.chanPoint)

		return noShutdown, nil

	default:
		// Otherwise we are not in a state where we can accept this
		// message.
		return noShutdown, ErrInvalidState
	}
}

// BeginNegotiation should be called when we have definitively reached a clean
// channel state and are ready to cooperatively arrive at a closing transaction.
// If it is our responsibility to kick off the negotiation, this method will
// generate a ClosingSigned message. If it is the remote's responsibility, then
// it will not. In either case it will transition the ChanCloser state machine
// to the negotiation phase wherein ClosingSigned messages are exchanged until
// a mutually agreeable result is achieved.
func (c *ChanCloser) BeginNegotiation() (fn.Option[lnwire.ClosingSigned],
	error) {

	noClosingSigned := fn.None[lnwire.ClosingSigned]()

	switch c.state {
	case closeAwaitingFlush:
		// Now that we know their desired delivery script, we can
		// compute what our max/ideal fee will be.
		c.initFeeBaseline()

		// Before continuing, mark the channel as cooperatively closed
		// with a nil txn. Even though we haven't negotiated the final
		// txn, this guarantees that our listchannels rpc will be
		// externally consistent, and reflect that the channel is being
		// shutdown by the time the closing request returns.
		err := c.cfg.Channel.MarkCoopBroadcasted(
			nil, c.closer,
		)
		if err != nil {
			return noClosingSigned, err
		}

		// At this point, we can now start the fee negotiation state, by
		// constructing and sending our initial signature for what we
		// think the closing transaction should look like.
		c.state = closeFeeNegotiation

		if !c.cfg.Channel.IsInitiator() {
			// By default this means we do nothing, but we do want
			// to check if we have a cached remote offer to process.
			// If we do, we'll process it here.
			res := noClosingSigned
			err = nil
			c.cachedClosingSigned.WhenSome(
				func(cs lnwire.ClosingSigned) {
					res, err = c.ReceiveClosingSigned(cs)
				},
			)

			return res, err
		}

		// We'll craft our initial close proposal in order to keep the
		// negotiation moving, but only if we're the initiator.
		closingSigned, err := c.proposeCloseSigned(c.idealFeeSat)
		if err != nil {
			return noClosingSigned,
				fmt.Errorf("unable to sign new co op "+
					"close offer: %w", err)
		}

		return fn.Some(*closingSigned), nil

	default:
		return noClosingSigned, ErrInvalidState
	}
}

// ReceiveClosingSigned is a method that should be called whenever we receive a
// ClosingSigned message from the wire. It may or may not return a
// ClosingSigned of our own to send back to the remote.
func (c *ChanCloser) ReceiveClosingSigned( //nolint:funlen
	msg lnwire.ClosingSigned) (fn.Option[lnwire.ClosingSigned], error) {

	noClosing := fn.None[lnwire.ClosingSigned]()

	switch c.state {
	case closeAwaitingFlush:
		// If we hit this case it either means there's a protocol
		// violation or that our chanCloser received the remote offer
		// before the link finished processing the channel flush.
		c.cachedClosingSigned = fn.Some(msg)
		return fn.None[lnwire.ClosingSigned](), nil

	case closeFeeNegotiation:
		// If this is a taproot channel, then it MUST have a partial
		// signature set at this point.
		isTaproot := c.cfg.Channel.ChanType().IsTaproot()
		if isTaproot && msg.PartialSig.IsNone() {
			return noClosing,
				fmt.Errorf("partial sig not set " +
					"for taproot chan")
		}

		isInitiator := c.cfg.Channel.IsInitiator()

		// We'll compare the proposed total fee, to what we've proposed
		// during the negotiations. If it doesn't match any of our
		// prior offers, then we'll attempt to ratchet the fee closer
		// to our ideal fee.
		remoteProposedFee := msg.FeeSatoshis

		_, feeMatchesOffer := c.priorFeeOffers[remoteProposedFee]
		switch {
		// For taproot channels, since nonces are involved, we can't do
		// the existing co-op close negotiation process without going
		// to a fully round based model. Rather than do this, we'll
		// just accept the very first offer by the initiator.
		case isTaproot && !isInitiator:
			chancloserLog.Infof("ChannelPoint(%v) accepting "+
				"initiator fee of %v", c.chanPoint,
				remoteProposedFee)

			// To auto-accept the initiators proposal, we'll just
			// send back a signature w/ the same offer. We don't
			// send the message here, as we can drop down and
			// finalize the closure and broadcast, then echo back
			// to Alice the final signature.
			_, err := c.proposeCloseSigned(remoteProposedFee)
			if err != nil {
				return noClosing, fmt.Errorf("unable to sign "+
					"new co op close offer: %w", err)
			}

		// Otherwise, if we are the initiator, and we just sent a
		// signature for a taproot channel, then we'll ensure that the
		// fee rate matches up exactly.
		case isTaproot && isInitiator && !feeMatchesOffer:
			return noClosing,
				fmt.Errorf("fee rate for "+
					"taproot channels was not accepted: "+
					"sent %v, got %v",
					c.idealFeeSat, remoteProposedFee)

		// If we're the initiator of the taproot channel, and we had
		// our fee echo'd back, then it's all good, and we can proceed
		// with final broadcast.
		case isTaproot && isInitiator && feeMatchesOffer:
			break

		// Otherwise, if this is a normal segwit v0 channel, and the
		// fee doesn't match our offer, then we'll try to "negotiate" a
		// new fee.
		case !feeMatchesOffer:
			// We'll now attempt to ratchet towards a fee deemed
			// acceptable by both parties, factoring in our ideal
			// fee rate, and the last proposed fee by both sides.
			proposal := calcCompromiseFee(
				c.chanPoint, c.idealFeeSat, c.lastFeeProposal,
				remoteProposedFee,
			)
			if c.cfg.Channel.IsInitiator() && proposal > c.maxFee {
				return noClosing, fmt.Errorf(
					"%w: %v > %v",
					ErrProposalExceedsMaxFee,
					proposal, c.maxFee)
			}

			// With our new fee proposal calculated, we'll craft a
			// new close signed signature to send to the other
			// party so we can continue the fee negotiation
			// process.
			closeSigned, err := c.proposeCloseSigned(proposal)
			if err != nil {
				return noClosing, fmt.Errorf("unable to sign "+
					"new co op close offer: %w", err)
			}

			// If the compromise fee doesn't match what the peer
			// proposed, then we'll return this latest close signed
			// message so we can continue negotiation.
			if proposal != remoteProposedFee {
				chancloserLog.Debugf("ChannelPoint(%v): close "+
					"tx fee disagreement, continuing "+
					"negotiation", c.chanPoint)

				return fn.Some(*closeSigned), nil
			}
		}

		chancloserLog.Infof("ChannelPoint(%v) fee of %v accepted, "+
			"ending negotiation", c.chanPoint, remoteProposedFee)

		// Otherwise, we've agreed on a fee for the closing
		// transaction! We'll craft the final closing transaction so we
		// can broadcast it to the network.
		var (
			localSig, remoteSig input.Signature
			closeOpts           []lnwallet.ChanCloseOpt
			err                 error
		)
		matchingSig := c.priorFeeOffers[remoteProposedFee]
		if c.cfg.Channel.ChanType().IsTaproot() {
			localWireSig, err := matchingSig.PartialSig.UnwrapOrErrV( //nolint:ll
				fmt.Errorf("none local sig"),
			)
			if err != nil {
				return noClosing, err
			}
			remoteWireSig, err := msg.PartialSig.UnwrapOrErrV(
				fmt.Errorf("none remote sig"),
			)
			if err != nil {
				return noClosing, err
			}

			muSession := c.cfg.MusigSession
			localSig, remoteSig, closeOpts, err = muSession.CombineClosingOpts( //nolint:ll
				localWireSig, remoteWireSig,
			)
			if err != nil {
				return noClosing, err
			}
		} else {
			localSig, err = matchingSig.Signature.ToSignature()
			if err != nil {
				return noClosing, err
			}
			remoteSig, err = msg.Signature.ToSignature()
			if err != nil {
				return noClosing, err
			}
		}

		// Before we complete the cooperative close, we'll see if we
		// have any extra aux options.
		c.auxOutputs, err = c.auxCloseOutputs(remoteProposedFee)
		if err != nil {
			return noClosing, err
		}
		c.auxOutputs.WhenSome(func(outs AuxCloseOutputs) {
			closeOpts = append(
				closeOpts, lnwallet.WithExtraCloseOutputs(
					outs.ExtraCloseOutputs,
				),
			)
			closeOpts = append(
				closeOpts, lnwallet.WithCustomCoopSort(
					outs.CustomSort,
				),
			)
		})

		closeTx, _, err := c.cfg.Channel.CompleteCooperativeClose(
			localSig, remoteSig, c.localDeliveryScript,
			c.remoteDeliveryScript, remoteProposedFee, closeOpts...,
		)
		if err != nil {
			return noClosing, err
		}
		c.closingTx = closeTx

		// If there's an aux chan closer, then we'll finalize with it
		// before we write to disk.
		err = fn.MapOptionZ(
			c.cfg.AuxCloser, func(aux AuxChanCloser) error {
				channel := c.cfg.Channel
				//nolint:ll
				req := AuxShutdownReq{
					ChanPoint:   c.chanPoint,
					ShortChanID: c.cfg.Channel.ShortChanID(),
					InternalKey: c.localInternalKey,
					Initiator:   channel.IsInitiator(),
					CommitBlob:  channel.LocalCommitmentBlob(),
					FundingBlob: channel.FundingBlob(),
				}
				desc := AuxCloseDesc{
					AuxShutdownReq:    req,
					LocalCloseOutput:  c.localCloseOutput,
					RemoteCloseOutput: c.remoteCloseOutput,
				}

				return aux.FinalizeClose(desc, closeTx)
			},
		)
		if err != nil {
			return noClosing, err
		}

		// Before publishing the closing tx, we persist it to the
		// database, such that it can be republished if something goes
		// wrong.
		err = c.cfg.Channel.MarkCoopBroadcasted(
			closeTx, c.closer,
		)
		if err != nil {
			return noClosing, err
		}

		// With the closing transaction crafted, we'll now broadcast it
		// to the network.
		chancloserLog.Infof("Broadcasting cooperative close tx: %v",
			lnutils.SpewLogClosure(closeTx))

		// Create a close channel label.
		chanID := c.cfg.Channel.ShortChanID()
		closeLabel := labels.MakeLabel(
			labels.LabelTypeChannelClose, &chanID,
		)

		if err := c.cfg.BroadcastTx(closeTx, closeLabel); err != nil {
			return noClosing, err
		}

		// Finally, we'll transition to the closeFinished state, and
		// also return the final close signed message we sent.
		// Additionally, we return true for the second argument to
		// indicate we're finished with the channel closing
		// negotiation.
		c.state = closeFinished
		matchingOffer := c.priorFeeOffers[remoteProposedFee]

		return fn.Some(*matchingOffer), nil

	// If we received a message while in the closeFinished state, then this
	// should only be the remote party echoing the last ClosingSigned
	// message that we agreed on.
	case closeFinished:

		// There's no more to do as both sides should have already
		// broadcast the closing transaction at this state.
		return noClosing, nil

	default:
		return noClosing, ErrInvalidState
	}
}

// auxCloseOutputs returns any additional outputs that should be used when
// closing the channel.
func (c *ChanCloser) auxCloseOutputs(
	closeFee btcutil.Amount) (fn.Option[AuxCloseOutputs], error) {

	var closeOuts fn.Option[AuxCloseOutputs]
	err := fn.MapOptionZ(c.cfg.AuxCloser, func(aux AuxChanCloser) error {
		req := AuxShutdownReq{
			ChanPoint:   c.chanPoint,
			ShortChanID: c.cfg.Channel.ShortChanID(),
			InternalKey: c.localInternalKey,
			Initiator:   c.cfg.Channel.IsInitiator(),
			CommitBlob:  c.cfg.Channel.LocalCommitmentBlob(),
			FundingBlob: c.cfg.Channel.FundingBlob(),
		}
		outs, err := aux.AuxCloseOutputs(AuxCloseDesc{
			AuxShutdownReq:    req,
			CloseFee:          closeFee,
			CommitFee:         c.cfg.Channel.CommitFee(),
			LocalCloseOutput:  c.localCloseOutput,
			RemoteCloseOutput: c.remoteCloseOutput,
		})
		if err != nil {
			return err
		}

		closeOuts = outs

		return nil
	})
	if err != nil {
		return closeOuts, err
	}

	return closeOuts, nil
}

// proposeCloseSigned attempts to propose a new signature for the closing
// transaction for a channel based on the prior fee negotiations and our
// current compromise fee.
func (c *ChanCloser) proposeCloseSigned(fee btcutil.Amount) (
	*lnwire.ClosingSigned, error) {

	var (
		closeOpts []lnwallet.ChanCloseOpt
		err       error
	)

	// If this is a taproot channel, then we'll include the musig session
	// generated for the next co-op close negotiation round.
	if c.cfg.Channel.ChanType().IsTaproot() {
		closeOpts, err = c.cfg.MusigSession.ProposalClosingOpts()
		if err != nil {
			return nil, err
		}
	}

	// We'll also now see if the aux chan closer has any additional options
	// for the closing purpose.
	c.auxOutputs, err = c.auxCloseOutputs(fee)
	if err != nil {
		return nil, err
	}
	c.auxOutputs.WhenSome(func(outs AuxCloseOutputs) {
		closeOpts = append(
			closeOpts, lnwallet.WithExtraCloseOutputs(
				outs.ExtraCloseOutputs,
			),
		)
		closeOpts = append(
			closeOpts, lnwallet.WithCustomCoopSort(
				outs.CustomSort,
			),
		)
	})

	// With all our options added, we'll attempt to co-op close now.
	rawSig, _, _, err := c.cfg.Channel.CreateCloseProposal(
		fee, c.localDeliveryScript, c.remoteDeliveryScript,
		closeOpts...,
	)
	if err != nil {
		return nil, err
	}

	// We'll note our last signature and proposed fee so when the remote
	// party responds we'll be able to decide if we've agreed on fees or
	// not.
	var (
		parsedSig  lnwire.Sig
		partialSig *lnwire.PartialSigWithNonce
	)
	if c.cfg.Channel.ChanType().IsTaproot() {
		musig, ok := rawSig.(*lnwallet.MusigPartialSig)
		if !ok {
			return nil, fmt.Errorf("expected MusigPartialSig, "+
				"got %T", rawSig)
		}

		partialSig = musig.ToWireSig()
	} else {
		parsedSig, err = lnwire.NewSigFromSignature(rawSig)
		if err != nil {
			return nil, err
		}
	}

	c.lastFeeProposal = fee

	chancloserLog.Infof("ChannelPoint(%v): proposing fee of %v sat to "+
		"close chan", c.chanPoint, int64(fee))

	// We'll assemble a ClosingSigned message using this information and
	// return it to the caller so we can kick off the final stage of the
	// channel closure process.
	closeSignedMsg := lnwire.NewClosingSigned(c.cid, fee, parsedSig)

	// For musig2 channels, the main sig is blank, and instead we'll send
	// over a partial signature which'll be combined once our offer is
	// accepted.
	if partialSig != nil {
		closeSignedMsg.PartialSig = lnwire.SomePartialSig(
			partialSig.PartialSig,
		)
	}

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
	// If our offer is lower than theirs, then we'll accept their offer if
	// it's no more than 30% *greater* than our current offer.
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

	chancloserLog.Infof("ChannelPoint(%v): computing fee compromise, "+
		"ideal=%v, last_sent=%v, remote_offer=%v", chanPoint,
		int64(ourIdealFee), int64(lastSentFee), int64(remoteFee))

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
	// sent, then we'll need to ratchet down in order to move our offer
	// closer to theirs.
	case remoteFee < lastSentFee:
		// If the fee is lower, but still acceptable, then we'll just
		// return this fee and end the negotiation.
		if feeInAcceptableRange(lastSentFee, remoteFee) {
			chancloserLog.Infof("ChannelPoint(%v): proposed "+
				"remote fee is close enough, capitulating",
				chanPoint)

			return remoteFee
		}

		// Otherwise, we'll ratchet the fee *down* using our current
		// algorithm.
		return ratchetFee(lastSentFee, false)

	// If the fee the remote party is offering is greater than the last one
	// we sent, then we'll ratchet up in order to ensure we terminate
	// eventually.
	case remoteFee > lastSentFee:
		// If the fee is greater, but still acceptable, then we'll just
		// return this fee in order to put an end to the negotiation.
		if feeInAcceptableRange(lastSentFee, remoteFee) {
			chancloserLog.Infof("ChannelPoint(%v): proposed "+
				"remote fee is close enough, capitulating",
				chanPoint)

			return remoteFee
		}

		// Otherwise, we'll ratchet the fee up using our current
		// algorithm.
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
		return nil, fmt.Errorf("invalid address: %w", err)
	}

	if !addr.IsForNet(params) {
		return nil, fmt.Errorf("invalid address: %v is not a %s "+
			"address", addr, params.Name)
	}

	return txscript.PayToAddrScript(addr)
}

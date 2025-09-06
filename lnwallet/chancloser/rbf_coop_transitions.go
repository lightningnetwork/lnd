package chancloser

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrThawHeightNotReached is returned if the remote party tries to
	// close, but the thaw height hasn't been matched yet.
	ErrThawHeightNotReached = fmt.Errorf("thaw height not reached")
)

// sendShutdownEvents is a helper function that returns a set of daemon events
// we need to emit when we decide that we should send a shutdown message. We'll
// also mark the channel as borked as well, as at this point, we no longer want
// to continue with normal operation.
func sendShutdownEvents(chanID lnwire.ChannelID, chanPoint wire.OutPoint,
	deliveryAddr lnwire.DeliveryAddress, peerPub btcec.PublicKey,
	postSendEvent fn.Option[ProtocolEvent],
	chanState ChanStateObserver) (protofsm.DaemonEventSet, error) {

	// We'll emit a daemon event that instructs the daemon to send out a
	// new shutdown message to the remote peer.
	msgsToSend := &protofsm.SendMsgEvent[ProtocolEvent]{
		TargetPeer: peerPub,
		Msgs: []lnwire.Message{&lnwire.Shutdown{
			ChannelID: chanID,
			Address:   deliveryAddr,
		}},
		SendWhen: fn.Some(func() bool {
			ok := chanState.NoDanglingUpdates()
			if ok {
				chancloserLog.Infof("ChannelPoint(%v): no "+
					"dangling updates sending shutdown "+
					"message", chanPoint)
			}

			return ok
		}),
		PostSendEvent: postSendEvent,
	}

	// If a close is already in process (we're in the RBF loop), then we
	// can skip everything below, and just send out the shutdown message.
	if chanState.FinalBalances().IsSome() {
		return protofsm.DaemonEventSet{msgsToSend}, nil
	}

	// Before closing, we'll attempt to send a disable update for the
	// channel.  We do so before closing the channel as otherwise the
	// current edge policy won't be retrievable from the graph.
	if err := chanState.DisableChannel(); err != nil {
		return nil, fmt.Errorf("unable to disable channel: %w", err)
	}

	// If we have a post-send event, then this means that we're the
	// responder. We'll use this fact below to update state in the DB.
	isInitiator := postSendEvent.IsNone()

	chancloserLog.Infof("ChannelPoint(%v): disabling outgoing adds",
		chanPoint)

	// As we're about to send a shutdown, we'll disable adds in the
	// outgoing direction.
	if err := chanState.DisableOutgoingAdds(); err != nil {
		return nil, fmt.Errorf("unable to disable outgoing "+
			"adds: %w", err)
	}

	// To be able to survive a restart, we'll also write to disk
	// information about the shutdown we're about to send out.
	err := chanState.MarkShutdownSent(deliveryAddr, isInitiator)
	if err != nil {
		return nil, fmt.Errorf("unable to mark shutdown sent: %w", err)
	}

	chancloserLog.Debugf("ChannelPoint(%v): marking channel as borked",
		chanPoint)

	return protofsm.DaemonEventSet{msgsToSend}, nil
}

// validateShutdown is a helper function that validates that the shutdown has a
// proper delivery script, and can be sent based on the current thaw height of
// the channel.
func validateShutdown(chanThawHeight fn.Option[uint32],
	upfrontAddr fn.Option[lnwire.DeliveryAddress],
	msg *ShutdownReceived, chanPoint wire.OutPoint,
	chainParams chaincfg.Params) error {

	// If we've received a shutdown message, and we have a thaw height,
	// then we need to make sure that the channel can now be co-op closed.
	err := fn.MapOptionZ(chanThawHeight, func(thawHeight uint32) error {
		// If the current height is below the thaw height, then we'll
		// reject the shutdown message as we can't yet co-op close the
		// channel.
		if msg.BlockHeight < thawHeight {
			return fmt.Errorf("%w: initiator attempting to "+
				"co-op close frozen ChannelPoint(%v) "+
				"(current_height=%v, thaw_height=%v)",
				ErrThawHeightNotReached, chanPoint,
				msg.BlockHeight, thawHeight)
		}

		return nil
	})
	if err != nil {
		return err
	}

	// Next, we'll verify that the remote party is sending the expected
	// shutdown script.
	return fn.MapOption(func(addr lnwire.DeliveryAddress) error {
		return validateShutdownScript(
			addr, msg.ShutdownScript, &chainParams,
		)
	})(upfrontAddr).UnwrapOr(nil)
}

// ProcessEvent takes a protocol event, and implements a state transition for
// the state. From this state, we can receive two possible incoming events:
// SendShutdown and ShutdownReceived. Both of these will transition us to the
// ChannelFlushing state.
func (c *ChannelActive) ProcessEvent(event ProtocolEvent,
	env *Environment) (*CloseStateTransition, error) {

	switch msg := event.(type) {
	// If we get a confirmation, then a prior transaction we broadcasted
	// has confirmed, so we can move to our terminal state early.
	case *SpendEvent:
		return &CloseStateTransition{
			NextState: &CloseFin{
				ConfirmedTx: msg.Tx,
			},
		}, nil

	// If we receive the SendShutdown event, then we'll send our shutdown
	// with a special SendPredicate, then go to the ShutdownPending where
	// we'll wait for the remote to send their shutdown.
	case *SendShutdown:
		// If we have an upfront shutdown addr or a delivery addr then
		// we'll use that. Otherwise, we'll generate a new delivery
		// addr.
		shutdownScript, err := env.LocalUpfrontShutdown.Alt(
			msg.DeliveryAddr,
		).UnwrapOrFuncErr(env.NewDeliveryScript)
		if err != nil {
			return nil, err
		}

		// We'll emit some daemon events to send the shutdown message
		// and disable the channel on the network level. In this case,
		// we don't need a post send event as receive their shutdown is
		// what'll move us beyond the ShutdownPending state.
		daemonEvents, err := sendShutdownEvents(
			env.ChanID, env.ChanPoint, shutdownScript,
			env.ChanPeer, fn.None[ProtocolEvent](),
			env.ChanObserver,
		)
		if err != nil {
			return nil, err
		}

		chancloserLog.Infof("ChannelPoint(%v): sending shutdown msg, "+
			"delivery_script=%x", env.ChanPoint, shutdownScript)

		// From here, we'll transition to the shutdown pending state. In
		// this state we await their shutdown message (self loop), then
		// also the flushing event.
		return &CloseStateTransition{
			NextState: &ShutdownPending{
				IdealFeeRate: fn.Some(msg.IdealFeeRate),
				ShutdownScripts: ShutdownScripts{
					LocalDeliveryScript: shutdownScript,
				},
			},
			NewEvents: fn.Some(RbfEvent{
				ExternalEvents: daemonEvents,
			}),
		}, nil

	// When we receive a shutdown from the remote party, we'll validate the
	// shutdown message, then transition to the ShutdownPending state. We'll
	// also emit similar events like the above to send out shutdown, and
	// also disable the channel.
	case *ShutdownReceived:
		chancloserLog.Infof("ChannelPoint(%v): received shutdown msg",
			env.ChanPoint)

		// Validate that they can send the message now, and also that
		// they haven't violated their commitment to a prior upfront
		// shutdown addr.
		err := validateShutdown(
			env.ThawHeight, env.RemoteUpfrontShutdown, msg,
			env.ChanPoint, env.ChainParams,
		)
		if err != nil {
			chancloserLog.Errorf("ChannelPoint(%v): rejecting "+
				"shutdown attempt: %v", env.ChanPoint, err)

			return nil, err
		}

		// If we have an upfront shutdown addr we'll use that,
		// otherwise, we'll generate a new delivery script.
		shutdownAddr, err := env.LocalUpfrontShutdown.UnwrapOrFuncErr(
			env.NewDeliveryScript,
		)
		if err != nil {
			return nil, err
		}

		chancloserLog.Infof("ChannelPoint(%v): sending shutdown msg "+
			"at next clean commit state", env.ChanPoint)

		// Now that we know the shutdown message is valid, we'll obtain
		// the set of daemon events we need to emit. We'll also specify
		// that once the message has actually been sent, that we
		// generate receive an input event of a ShutdownComplete.
		daemonEvents, err := sendShutdownEvents(
			env.ChanID, env.ChanPoint, shutdownAddr,
			env.ChanPeer,
			fn.Some[ProtocolEvent](&ShutdownComplete{}),
			env.ChanObserver,
		)
		if err != nil {
			return nil, err
		}

		chancloserLog.Infof("ChannelPoint(%v): disabling incoming adds",
			env.ChanPoint)

		// We just received a shutdown, so we'll disable the adds in
		// the outgoing direction.
		if err := env.ChanObserver.DisableIncomingAdds(); err != nil {
			return nil, fmt.Errorf("unable to disable incoming "+
				"adds: %w", err)
		}

		remoteAddr := msg.ShutdownScript

		return &CloseStateTransition{
			NextState: &ShutdownPending{
				ShutdownScripts: ShutdownScripts{
					LocalDeliveryScript:  shutdownAddr,
					RemoteDeliveryScript: remoteAddr,
				},
			},
			NewEvents: fn.Some(protofsm.EmittedEvent[ProtocolEvent]{
				ExternalEvents: daemonEvents,
			}),
		}, nil

	// Any other messages in this state will result in an error, as this is
	// an undefined state transition.
	default:
		return nil, fmt.Errorf("%w: received %T while in ChannelActive",
			ErrInvalidStateTransition, msg)
	}
}

// ProcessEvent takes a protocol event, and implements a state transition for
// the state. Our path to this state will determine the set of valid events. If
// we were the one that sent the shutdown, then we'll just wait on the
// ShutdownReceived event. Otherwise, we received the shutdown, and can move
// forward once we receive the ShutdownComplete event. Receiving
// ShutdownComplete means that we've sent our shutdown, as this was specified
// as a post send event.
func (s *ShutdownPending) ProcessEvent(event ProtocolEvent,
	env *Environment) (*CloseStateTransition, error) {

	switch msg := event.(type) {
	// If we get a confirmation, then a prior transaction we broadcasted
	// has confirmed, so we can move to our terminal state early.
	case *SpendEvent:
		return &CloseStateTransition{
			NextState: &CloseFin{
				ConfirmedTx: msg.Tx,
			},
		}, nil

	// The remote party sent an offer early. We'll go to the ChannelFlushing
	// case, and then emit the offer as a internal event, which'll be
	// handled as an early offer.
	case *OfferReceivedEvent:
		chancloserLog.Infof("ChannelPoint(%v): got an early offer "+
			"in ShutdownPending, emitting as external event",
			env.ChanPoint)

		s.EarlyRemoteOffer = fn.Some(*msg)

		// We'll perform a noop update so we can wait for the actual
		// channel flushed event.
		return &CloseStateTransition{
			NextState: s,
		}, nil

	// When we receive a shutdown from the remote party, we'll validate the
	// shutdown message, then transition to the ChannelFlushing state.
	case *ShutdownReceived:
		chancloserLog.Infof("ChannelPoint(%v): received shutdown msg",
			env.ChanPoint)

		// Validate that they can send the message now, and also that
		// they haven't violated their commitment to a prior upfront
		// shutdown addr.
		err := validateShutdown(
			env.ThawHeight, env.RemoteUpfrontShutdown, msg,
			env.ChanPoint, env.ChainParams,
		)
		if err != nil {
			chancloserLog.Errorf("ChannelPoint(%v): rejecting "+
				"shutdown attempt: %v", env.ChanPoint, err)

			return nil, err
		}

		// If the channel is *already* flushed, and the close is
		// go straight into negotiation, as this is the RBF loop.
		// already in progress, then we can skip the flushing state and
		var eventsToEmit []ProtocolEvent
		finalBalances := env.ChanObserver.FinalBalances().UnwrapOr(
			unknownBalance,
		)
		if finalBalances != unknownBalance {
			channelFlushed := ProtocolEvent(&ChannelFlushed{
				ShutdownBalances: finalBalances,
			})
			eventsToEmit = append(eventsToEmit, channelFlushed)
		}

		chancloserLog.Infof("ChannelPoint(%v): disabling incoming adds",
			env.ChanPoint)

		// We just received a shutdown, so we'll disable the adds in
		// the outgoing direction.
		if err := env.ChanObserver.DisableIncomingAdds(); err != nil {
			return nil, fmt.Errorf("unable to disable incoming "+
				"adds: %w", err)
		}

		chancloserLog.Infof("ChannelPoint(%v): waiting for channel to "+
			"be flushed...", env.ChanPoint)

		// If we received a remote offer early from the remote party,
		// then we'll add that to the set of internal events to emit.
		s.EarlyRemoteOffer.WhenSome(func(offer OfferReceivedEvent) {
			eventsToEmit = append(eventsToEmit, &offer)
		})

		var newEvents fn.Option[RbfEvent]
		if len(eventsToEmit) > 0 {
			newEvents = fn.Some(RbfEvent{
				InternalEvent: eventsToEmit,
			})
		}

		// We transition to the ChannelFlushing state, where we await
		// the ChannelFlushed event.
		return &CloseStateTransition{
			NextState: &ChannelFlushing{
				IdealFeeRate: s.IdealFeeRate,
				ShutdownScripts: ShutdownScripts{
					LocalDeliveryScript:  s.LocalDeliveryScript, //nolint:ll
					RemoteDeliveryScript: msg.ShutdownScript,    //nolint:ll
				},
			},
			NewEvents: newEvents,
		}, nil

	// If we get this message, then this means that we were finally able to
	// send out shutdown after receiving it from the remote party. We'll
	// now transition directly to the ChannelFlushing state.
	case *ShutdownComplete:
		chancloserLog.Infof("ChannelPoint(%v): waiting for channel to "+
			"be flushed...", env.ChanPoint)

		// If the channel is *already* flushed, and the close is
		// already in progress, then we can skip the flushing state and
		// go straight into negotiation, as this is the RBF loop.
		var eventsToEmit []ProtocolEvent
		finalBalances := env.ChanObserver.FinalBalances().UnwrapOr(
			unknownBalance,
		)
		if finalBalances != unknownBalance {
			channelFlushed := ProtocolEvent(&ChannelFlushed{
				ShutdownBalances: finalBalances,
			})
			eventsToEmit = append(eventsToEmit, channelFlushed)
		}

		// If we received a remote offer early from the remote party,
		// then we'll add that to the set of internal events to emit.
		s.EarlyRemoteOffer.WhenSome(func(offer OfferReceivedEvent) {
			eventsToEmit = append(eventsToEmit, &offer)
		})

		var newEvents fn.Option[RbfEvent]
		if len(eventsToEmit) > 0 {
			newEvents = fn.Some(RbfEvent{
				InternalEvent: eventsToEmit,
			})
		}

		// From here, we'll transition to the channel flushing state.
		// We'll stay here until we receive the ChannelFlushed event.
		return &CloseStateTransition{
			NextState: &ChannelFlushing{
				IdealFeeRate:    s.IdealFeeRate,
				ShutdownScripts: s.ShutdownScripts,
			},
			NewEvents: newEvents,
		}, nil

	// Any other messages in this state will result in an error, as this is
	// an undefined state transition.
	default:
		return nil, fmt.Errorf("%w: received %T while in "+
			"ShutdownPending", ErrInvalidStateTransition, msg)
	}
}

// ProcessEvent takes a new protocol event, and figures out if we can
// transition to the next state, or just loop back upon ourself. If we receive
// a ShutdownReceived event, then we'll stay in the ChannelFlushing state, as
// we haven't yet fully cleared the channel. Otherwise, we can move to the
// CloseReady state which'll being the channel closing process.
func (c *ChannelFlushing) ProcessEvent(event ProtocolEvent,
	env *Environment) (*CloseStateTransition, error) {

	switch msg := event.(type) {
	// If we get a confirmation, then a prior transaction we broadcasted
	// has confirmed, so we can move to our terminal state early.
	case *SpendEvent:
		return &CloseStateTransition{
			NextState: &CloseFin{
				ConfirmedTx: msg.Tx,
			},
		}, nil

	// If we get an OfferReceived event, then the channel is flushed from
	// the PoV of the remote party. However, due to propagation delay or
	// concurrency, we may not have received the ChannelFlushed event yet.
	// In this case, we'll stash the event and wait for the ChannelFlushed
	// event.
	case *OfferReceivedEvent:
		chancloserLog.Infof("ChannelPoint(%v): received remote offer "+
			"early, stashing...", env.ChanPoint)

		c.EarlyRemoteOffer = fn.Some(*msg)

		// We'll perform a noop update so we can wait for the actual
		// channel flushed event.
		return &CloseStateTransition{
			NextState: c,
		}, nil

	// If we receive the ChannelFlushed event, then the coast is clear so
	// we'll now morph into the dual peer state so we can handle any
	// messages needed to drive forward the close process.
	case *ChannelFlushed:
		// Both the local and remote losing negotiation needs the terms
		// we'll be using to close the channel, so we'll create them
		// here.
		closeTerms := CloseChannelTerms{
			ShutdownScripts:  c.ShutdownScripts,
			ShutdownBalances: msg.ShutdownBalances,
		}

		chancloserLog.Infof("ChannelPoint(%v): channel flushed! "+
			"proceeding with co-op close", env.ChanPoint)

		// Now that the channel has been flushed, we'll mark on disk
		// that we're approaching the point of no return where we'll
		// send a new signature to the remote party.
		//
		// TODO(roasbeef): doesn't actually matter if initiator here?
		if msg.FreshFlush {
			err := env.ChanObserver.MarkCoopBroadcasted(nil, true)
			if err != nil {
				return nil, err
			}
		}

		// If an ideal fee rate was specified, then we'll use that,
		// otherwise we'll fall back to the default value given in the
		// env.
		idealFeeRate := c.IdealFeeRate.UnwrapOr(env.DefaultFeeRate)

		// We'll then use that fee rate to determine the absolute fee
		// we'd propose.
		localTxOut, remoteTxOut := closeTerms.DeriveCloseTxOuts()
		absoluteFee := env.FeeEstimator.EstimateFee(
			env.ChanType, localTxOut, remoteTxOut,
			idealFeeRate.FeePerKWeight(),
		)

		chancloserLog.Infof("ChannelPoint(%v): using ideal_fee=%v, "+
			"absolute_fee=%v", env.ChanPoint, idealFeeRate,
			absoluteFee)

		var (
			internalEvents []ProtocolEvent
			newEvents      fn.Option[RbfEvent]
		)

		// If we received a remote offer early from the remote party,
		// then we'll add that to the set of internal events to emit.
		c.EarlyRemoteOffer.WhenSome(func(offer OfferReceivedEvent) {
			internalEvents = append(internalEvents, &offer)
		})

		// Only if we have enough funds to pay for the fees do we need
		// to emit a localOfferSign event.
		//
		// TODO(roasbeef): also only proceed if was higher than fee in
		// last round?
		if closeTerms.LocalCanPayFees(absoluteFee) {
			// Each time we go into this negotiation flow, we'll
			// kick off our local state with a new close attempt.
			// So we'll emit a internal event to drive forward that
			// part of the state.
			localOfferSign := ProtocolEvent(&SendOfferEvent{
				TargetFeeRate: idealFeeRate,
			})
			internalEvents = append(internalEvents, localOfferSign)
		} else {
			chancloserLog.Infof("ChannelPoint(%v): unable to pay "+
				"fees with local balance, skipping "+
				"closing_complete", env.ChanPoint)
		}

		if len(internalEvents) > 0 {
			newEvents = fn.Some(RbfEvent{
				InternalEvent: internalEvents,
			})
		}

		return &CloseStateTransition{
			NextState: &ClosingNegotiation{
				PeerState: lntypes.Dual[AsymmetricPeerState]{
					Local: &LocalCloseStart{
						CloseChannelTerms: &closeTerms,
					},
					Remote: &RemoteCloseStart{
						CloseChannelTerms: &closeTerms,
					},
				},
				CloseChannelTerms: &closeTerms,
			},
			NewEvents: newEvents,
		}, nil

	default:
		return nil, fmt.Errorf("%w: received %T while in "+
			"ChannelFlushing", ErrInvalidStateTransition, msg)
	}
}

// processNegotiateEvent is a helper function that processes a new event to
// local channel state once we're in the ClosingNegotiation state.
func processNegotiateEvent(c *ClosingNegotiation, event ProtocolEvent,
	env *Environment,
	chanPeer lntypes.ChannelParty) (*CloseStateTransition, error) {

	targetPeerState := c.PeerState.GetForParty(chanPeer)

	// Drive forward the remote state based on the next event.
	transition, err := targetPeerState.ProcessEvent(
		event, env,
	)
	if err != nil {
		return nil, err
	}

	nextPeerState, ok := transition.NextState.(AsymmetricPeerState) //nolint:ll
	if !ok {
		return nil, fmt.Errorf("expected %T to be "+
			"AsymmetricPeerState", transition.NextState)
	}

	// Make a copy of the input state, then update the peer state of the
	// proper party.
	newPeerState := *c
	newPeerState.PeerState.SetForParty(chanPeer, nextPeerState)

	return &CloseStateTransition{
		NextState: &newPeerState,
		NewEvents: transition.NewEvents,
	}, nil
}

// updateAndValidateCloseTerms is a helper function that validates examines the
// incoming event, and decide if we need to update the remote party's address,
// or reject it if it doesn't include our latest address.
func (c *ClosingNegotiation) updateAndValidateCloseTerms(
	event ProtocolEvent) error {

	assertLocalScriptMatches := func(localScriptInMsg []byte) error {
		if !bytes.Equal(
			c.LocalDeliveryScript, localScriptInMsg,
		) {

			return fmt.Errorf("%w: remote party sent wrong "+
				"script, expected %x, got %x",
				ErrWrongLocalScript, c.LocalDeliveryScript,
				localScriptInMsg,
			)
		}

		return nil
	}

	switch msg := event.(type) {
	// The remote party is sending us a new request to counter sign their
	// version of the commitment transaction.
	case *OfferReceivedEvent:
		// Make sure that they're sending our local script, and not
		// something else.
		err := assertLocalScriptMatches(msg.SigMsg.CloseeScript)
		if err != nil {
			return err
		}

		oldRemoteAddr := c.RemoteDeliveryScript
		newRemoteAddr := msg.SigMsg.CloserScript

		// If they're sending a new script, then we'll update to the new
		// one.
		if !bytes.Equal(oldRemoteAddr, newRemoteAddr) {
			c.RemoteDeliveryScript = newRemoteAddr
		}

	// The remote party responded to our sig request with a signature for
	// our version of the commitment transaction.
	case *LocalSigReceived:
		// Make sure that they're sending our local script, and not
		// something else.
		err := assertLocalScriptMatches(msg.SigMsg.CloserScript)
		if err != nil {
			return err
		}

		return nil
	}

	return nil
}

// ProcessEvent drives forward the composite states for the local and remote
// party in response to new events. From this state, we'll continue to drive
// forward the local and remote states until we arrive at the StateFin stage,
// or we loop back up to the ShutdownPending state.
func (c *ClosingNegotiation) ProcessEvent(event ProtocolEvent,
	env *Environment) (*CloseStateTransition, error) {

	// There're two classes of events that can break us out of this state:
	// we receive a confirmation event, or we receive a signal to restart
	// the co-op close process.
	switch msg := event.(type) {
	// Ignore any potential duplicate channel flushed events.
	case *ChannelFlushed:
		return &CloseStateTransition{
			NextState: c,
		}, nil

	// If we get a confirmation, then the spend request we issued when we
	// were leaving the ChannelFlushing state has been confirmed.  We'll
	// now transition to the StateFin state.
	case *SpendEvent:
		return &CloseStateTransition{
			NextState: &CloseFin{
				ConfirmedTx: msg.Tx,
			},
		}, nil
	}

	// At this point, we know its a new signature message. We'll validate,
	// and maybe update the set of close terms based on what we receive. We
	// might update the remote party's address for example.
	if err := c.updateAndValidateCloseTerms(event); err != nil {
		return nil, fmt.Errorf("event violates close terms: %w", err)
	}

	shouldRouteTo := func(party lntypes.ChannelParty) bool {
		state := c.PeerState.GetForParty(party)
		if state == nil {
			return false
		}

		return state.ShouldRouteTo(event)
	}

	// If we get to this point, then we have an event that'll drive forward
	// the negotiation process.  Based on the event, we'll figure out which
	// state we'll be modifying.
	switch {
	case shouldRouteTo(lntypes.Local):
		chancloserLog.Infof("ChannelPoint(%v): routing %T to local "+
			"chan state", env.ChanPoint, event)

		// Drive forward the local state based on the next event.
		return processNegotiateEvent(c, event, env, lntypes.Local)

	case shouldRouteTo(lntypes.Remote):
		chancloserLog.Infof("ChannelPoint(%v): routing %T to remote "+
			"chan state", env.ChanPoint, event)

		// Drive forward the remote state based on the next event.
		return processNegotiateEvent(c, event, env, lntypes.Remote)
	}

	return nil, fmt.Errorf("%w: received %T while in %v",
		ErrInvalidStateTransition, event, c)
}

// newSigTlv is a helper function that returns a new optional TLV sig field for
// the parametrized tlv.TlvType value.
func newSigTlv[T tlv.TlvType](s lnwire.Sig) tlv.OptionalRecordT[T, lnwire.Sig] {
	return tlv.SomeRecordT(tlv.NewRecordT[T](s))
}

// ProcessEvent implements the event processing to kick off the process of
// obtaining a new (possibly RBF'd) signature for our commitment transaction.
func (l *LocalCloseStart) ProcessEvent(event ProtocolEvent,
	env *Environment) (*CloseStateTransition, error) {

	switch msg := event.(type) { //nolint:gocritic
	// If we receive a SendOfferEvent, then we'll use the specified fee
	// rate to generate for the closing transaction with our ideal fee
	// rate.
	case *SendOfferEvent:
		// given the state of the local/remote outputs.
		// First, we'll figure out the absolute fee rate we should pay
		localTxOut, remoteTxOut := l.DeriveCloseTxOuts()
		absoluteFee := env.FeeEstimator.EstimateFee(
			env.ChanType, localTxOut, remoteTxOut,
			msg.TargetFeeRate.FeePerKWeight(),
		)

		// If we can't actually pay for fees here, then we'll just do a
		// noop back to the same state to await a new fee rate.
		if !l.LocalCanPayFees(absoluteFee) {
			chancloserLog.Infof("ChannelPoint(%v): unable to pay "+
				"fee=%v with local balance %v, skipping "+
				"closing_complete", env.ChanPoint, absoluteFee,
				l.LocalBalance)

			return &CloseStateTransition{
				NextState: &CloseErr{
					CloseChannelTerms: l.CloseChannelTerms,
					Party:             lntypes.Local,
					ErrState: NewErrStateCantPayForFee(
						l.LocalBalance.ToSatoshis(),
						absoluteFee,
					),
				},
			}, nil
		}

		// Now that we know what fee we want to pay, we'll create a new
		// signature over our co-op close transaction. For our
		// proposals, we'll just always use the known RBF sequence
		// value.
		localScript := l.LocalDeliveryScript
		rawSig, closeTx, closeBalance, err := env.CloseSigner.CreateCloseProposal( //nolint:ll
			absoluteFee, localScript, l.RemoteDeliveryScript,
			lnwallet.WithCustomSequence(mempool.MaxRBFSequence),
			lnwallet.WithCustomPayer(lntypes.Local),
		)
		if err != nil {
			return nil, err
		}
		wireSig, err := lnwire.NewSigFromSignature(rawSig)
		if err != nil {
			return nil, err
		}

		chancloserLog.Infof("closing w/ local_addr=%x, "+
			"remote_addr=%x, fee=%v", localScript[:],
			l.RemoteDeliveryScript[:], absoluteFee)

		chancloserLog.Infof("proposing closing_tx=%v",
			lnutils.SpewLogClosure(closeTx))

		// Now that we have our signature, we'll set the proper
		// closingSigs field based on if the remote party's output is
		// dust or not.
		var closingSigs lnwire.ClosingSigs
		switch {
		// If the remote party's output is dust, then we'll set the
		// CloserNoClosee field.
		case remoteTxOut == nil:
			closingSigs.CloserNoClosee = newSigTlv[tlv.TlvType1](
				wireSig,
			)

		// If after paying for fees, our balance is below dust, then
		// we'll set the NoCloserClosee field.
		case closeBalance < lnwallet.DustLimitForSize(len(localScript)):
			closingSigs.NoCloserClosee = newSigTlv[tlv.TlvType2](
				wireSig,
			)

		// Otherwise, we'll set the CloserAndClosee field.
		//
		// TODO(roasbeef): should actually set both??
		default:
			closingSigs.CloserAndClosee = newSigTlv[tlv.TlvType3](
				wireSig,
			)
		}

		// Now that we have our sig, we'll emit a daemon event to send
		// it to the remote party, then transition to the
		// LocalOfferSent state.
		//
		// TODO(roasbeef): type alias for protocol event
		sendEvent := protofsm.DaemonEventSet{&protofsm.SendMsgEvent[ProtocolEvent]{ //nolint:ll
			TargetPeer: env.ChanPeer,
			Msgs: []lnwire.Message{&lnwire.ClosingComplete{
				ChannelID:    env.ChanID,
				CloserScript: l.LocalDeliveryScript,
				CloseeScript: l.RemoteDeliveryScript,
				FeeSatoshis:  absoluteFee,
				LockTime:     env.BlockHeight,
				ClosingSigs:  closingSigs,
			}},
		}}

		chancloserLog.Infof("ChannelPoint(%v): sending closing sig "+
			"to remote party, fee_sats=%v", env.ChanPoint,
			absoluteFee)

		return &CloseStateTransition{
			NextState: &LocalOfferSent{
				ProposedFee:       absoluteFee,
				ProposedFeeRate:   msg.TargetFeeRate,
				LocalSig:          wireSig,
				CloseChannelTerms: l.CloseChannelTerms,
			},
			NewEvents: fn.Some(RbfEvent{
				ExternalEvents: sendEvent,
			}),
		}, nil
	}

	return nil, fmt.Errorf("%w: received %T while in LocalCloseStart",
		ErrInvalidStateTransition, event)
}

// extractSig extracts the expected signature from the closing sig message.
// Only one of them should actually be populated as the closing sig message is
// sent in response to a ClosingComplete message, it should only sign the same
// version of the co-op close tx as the sender did.
func extractSig(msg lnwire.ClosingSig) fn.Result[lnwire.Sig] {
	// First, we'll validate that only one signature is included in their
	// response to our initial offer. If not, then we'll exit here, and
	// trigger a recycle of the connection.
	sigInts := []bool{
		msg.CloserNoClosee.IsSome(), msg.NoCloserClosee.IsSome(),
		msg.CloserAndClosee.IsSome(),
	}
	numSigs := fn.Foldl(0, sigInts, func(acc int, sigInt bool) int {
		if sigInt {
			return acc + 1
		}

		return acc
	})
	if numSigs != 1 {
		return fn.Errf[lnwire.Sig]("%w: only one sig should be set, "+
			"got %v", ErrTooManySigs, numSigs)
	}

	// The final sig is the one that's actually set.
	sig := msg.CloserAndClosee.ValOpt().Alt(
		msg.NoCloserClosee.ValOpt(),
	).Alt(
		msg.CloserNoClosee.ValOpt(),
	)

	return fn.NewResult(sig.UnwrapOrErr(ErrNoSig))
}

// ProcessEvent implements the state transition function for the
// LocalOfferSent state. In this state, we'll wait for the remote party to
// send a close_signed message which gives us the ability to broadcast a new
// co-op close transaction.
func (l *LocalOfferSent) ProcessEvent(event ProtocolEvent,
	env *Environment) (*CloseStateTransition, error) {

	switch msg := event.(type) { //nolint:gocritic
	// If we receive a LocalSigReceived event, then we'll attempt to
	// validate the signature from the remote party. If valid, then we can
	// broadcast the transaction, and transition to the ClosePending state.
	case *LocalSigReceived:
		// Extract and validate that only one sig field is set.
		sig, err := extractSig(msg.SigMsg).Unpack()
		if err != nil {
			return nil, err
		}

		remoteSig, err := sig.ToSignature()
		if err != nil {
			return nil, err
		}
		localSig, err := l.LocalSig.ToSignature()
		if err != nil {
			return nil, err
		}

		// Now that we have their signature, we'll attempt to validate
		// it, then extract a valid closing signature from it.
		closeTx, _, err := env.CloseSigner.CompleteCooperativeClose(
			localSig, remoteSig, l.LocalDeliveryScript,
			l.RemoteDeliveryScript, l.ProposedFee,
			lnwallet.WithCustomSequence(mempool.MaxRBFSequence),
			lnwallet.WithCustomPayer(lntypes.Local),
		)
		if err != nil {
			return nil, err
		}

		// As we're about to broadcast a new version of the co-op close
		// transaction, we'll mark again as broadcast, but with this
		// variant of the co-op close tx.
		err = env.ChanObserver.MarkCoopBroadcasted(closeTx, true)
		if err != nil {
			return nil, err
		}

		broadcastEvent := protofsm.DaemonEventSet{&protofsm.BroadcastTxn{ //nolint:ll
			Tx: closeTx,
			Label: labels.MakeLabel(
				labels.LabelTypeChannelClose, &env.Scid,
			),
		}}

		chancloserLog.Infof("ChannelPoint(%v): received sig from "+
			"remote party, broadcasting: tx=%v", env.ChanPoint,
			lnutils.SpewLogClosure(closeTx),
		)

		return &CloseStateTransition{
			NextState: &ClosePending{
				CloseTx:           closeTx,
				FeeRate:           l.ProposedFeeRate,
				CloseChannelTerms: l.CloseChannelTerms,
				Party:             lntypes.Local,
			},
			NewEvents: fn.Some(protofsm.EmittedEvent[ProtocolEvent]{
				ExternalEvents: broadcastEvent,
			}),
		}, nil
	}

	return nil, fmt.Errorf("%w: received %T while in LocalOfferSent",
		ErrInvalidStateTransition, event)
}

// ProcessEvent implements the state transition function for the
// RemoteCloseStart. In this state, we'll wait for the remote party to send a
// closing_complete message. Assuming they can pay for the fees, we'll sign it
// ourselves, then transition to the next state of ClosePending.
func (l *RemoteCloseStart) ProcessEvent(event ProtocolEvent,
	env *Environment) (*CloseStateTransition, error) {

	switch msg := event.(type) { //nolint:gocritic
	// If we receive a OfferReceived event, we'll make sure they can
	// actually pay for the fee. If so, then we'll counter sign and
	// transition to a terminal state.
	case *OfferReceivedEvent:
		// To start, we'll perform some basic validation of the sig
		// message they've sent. We'll validate that the remote party
		// actually has enough fees to pay the closing fees.
		if !l.RemoteCanPayFees(msg.SigMsg.FeeSatoshis) {
			return nil, fmt.Errorf("%w: %v vs %v",
				ErrRemoteCannotPay,
				msg.SigMsg.FeeSatoshis,
				l.RemoteBalance.ToSatoshis())
		}

		// With the basic sanity checks out of the way, we'll now
		// figure out which signature that we'll attempt to sign
		// against.
		var (
			remoteSig input.Signature
			noClosee  bool
		)
		switch {
		// If our balance is dust, then we expect the CloserNoClosee
		// sig to be set.
		case l.LocalAmtIsDust():
			if msg.SigMsg.CloserNoClosee.IsNone() {
				return nil, ErrCloserNoClosee
			}
			msg.SigMsg.CloserNoClosee.WhenSomeV(func(s lnwire.Sig) {
				remoteSig, _ = s.ToSignature()
				noClosee = true
			})

		// Otherwise, we'll assume that CloseAndClosee is set.
		//
		// TODO(roasbeef): NoCloserClosee, but makes no sense?
		default:
			if msg.SigMsg.CloserAndClosee.IsNone() {
				return nil, ErrCloserAndClosee
			}
			msg.SigMsg.CloserAndClosee.WhenSomeV(func(s lnwire.Sig) { //nolint:ll
				remoteSig, _ = s.ToSignature()
			})
		}

		chanOpts := []lnwallet.ChanCloseOpt{
			lnwallet.WithCustomSequence(mempool.MaxRBFSequence),
			lnwallet.WithCustomLockTime(msg.SigMsg.LockTime),
			lnwallet.WithCustomPayer(lntypes.Remote),
		}

		chancloserLog.Infof("responding to close w/ local_addr=%x, "+
			"remote_addr=%x, fee=%v",
			l.LocalDeliveryScript[:], l.RemoteDeliveryScript[:],
			msg.SigMsg.FeeSatoshis)

		// Now that we have the remote sig, we'll sign the version they
		// signed, then attempt to complete the cooperative close
		// process.
		//
		// TODO(roasbeef): need to be able to omit an output when
		// signing based on the above, as closing opt
		rawSig, _, _, err := env.CloseSigner.CreateCloseProposal(
			msg.SigMsg.FeeSatoshis, l.LocalDeliveryScript,
			l.RemoteDeliveryScript, chanOpts...,
		)
		if err != nil {
			return nil, err
		}
		wireSig, err := lnwire.NewSigFromSignature(rawSig)
		if err != nil {
			return nil, err
		}

		localSig, err := wireSig.ToSignature()
		if err != nil {
			return nil, err
		}

		// With our signature created, we'll now attempt to finalize the
		// close process.
		closeTx, _, err := env.CloseSigner.CompleteCooperativeClose(
			localSig, remoteSig, l.LocalDeliveryScript,
			l.RemoteDeliveryScript, msg.SigMsg.FeeSatoshis,
			chanOpts...,
		)
		if err != nil {
			return nil, err
		}

		chancloserLog.Infof("ChannelPoint(%v): received sig (fee=%v "+
			"sats) from remote party, signing new tx=%v",
			env.ChanPoint, msg.SigMsg.FeeSatoshis,
			lnutils.SpewLogClosure(closeTx),
		)

		var closingSigs lnwire.ClosingSigs
		if noClosee {
			closingSigs.CloserNoClosee = newSigTlv[tlv.TlvType1](
				wireSig,
			)
		} else {
			closingSigs.CloserAndClosee = newSigTlv[tlv.TlvType3](
				wireSig,
			)
		}

		// As we're about to broadcast a new version of the co-op close
		// transaction, we'll mark again as broadcast, but with this
		// variant of the co-op close tx.
		//
		// TODO(roasbeef): db will only store one instance, store both?
		err = env.ChanObserver.MarkCoopBroadcasted(closeTx, false)
		if err != nil {
			return nil, err
		}

		// As we transition, we'll omit two events: one to broadcast
		// the transaction, and the other to send our ClosingSig
		// message to the remote party.
		sendEvent := &protofsm.SendMsgEvent[ProtocolEvent]{
			TargetPeer: env.ChanPeer,
			Msgs: []lnwire.Message{&lnwire.ClosingSig{
				ChannelID:    env.ChanID,
				CloserScript: l.RemoteDeliveryScript,
				CloseeScript: l.LocalDeliveryScript,
				FeeSatoshis:  msg.SigMsg.FeeSatoshis,
				LockTime:     msg.SigMsg.LockTime,
				ClosingSigs:  closingSigs,
			}},
		}
		broadcastEvent := &protofsm.BroadcastTxn{
			Tx: closeTx,
			Label: labels.MakeLabel(
				labels.LabelTypeChannelClose, &env.Scid,
			),
		}
		daemonEvents := protofsm.DaemonEventSet{
			sendEvent, broadcastEvent,
		}

		// We'll also compute the final fee rate that the remote party
		// paid based off the absolute fee and the size of the closing
		// transaction.
		vSize := mempool.GetTxVirtualSize(btcutil.NewTx(closeTx))
		feeRate := chainfee.SatPerVByte(
			int64(msg.SigMsg.FeeSatoshis) / vSize,
		)

		// Now that we've extracted the signature, we'll transition to
		// the next state where we'll sign+broadcast the sig.
		return &CloseStateTransition{
			NextState: &ClosePending{
				CloseTx:           closeTx,
				FeeRate:           feeRate,
				CloseChannelTerms: l.CloseChannelTerms,
				Party:             lntypes.Remote,
			},
			NewEvents: fn.Some(protofsm.EmittedEvent[ProtocolEvent]{
				ExternalEvents: daemonEvents,
			}),
		}, nil
	}

	return nil, fmt.Errorf("%w: received %T while in RemoteCloseStart",
		ErrInvalidStateTransition, event)
}

// ProcessEvent is a semi-terminal state in the rbf-coop close state machine.
// In this state, we're waiting for either a confirmation, or for either side
// to attempt to create a new RBF'd co-op close transaction.
func (c *ClosePending) ProcessEvent(event ProtocolEvent,
	_ *Environment) (*CloseStateTransition, error) {

	switch msg := event.(type) {
	// If we can a spend while waiting for the close, then we'll go to our
	// terminal state.
	case *SpendEvent:
		return &CloseStateTransition{
			NextState: &CloseFin{
				ConfirmedTx: msg.Tx,
			},
		}, nil

	// If we get a send offer event in this state, then we're doing a state
	// transition to the LocalCloseStart state, so we can sign a new closing
	// tx.
	case *SendOfferEvent:
		return &CloseStateTransition{
			NextState: &LocalCloseStart{
				CloseChannelTerms: c.CloseChannelTerms,
			},
			NewEvents: fn.Some(protofsm.EmittedEvent[ProtocolEvent]{
				InternalEvent: []ProtocolEvent{msg},
			}),
		}, nil

	// If we get an offer received event, then we're doing a state
	// transition to the RemoteCloseStart, as the remote peer wants to sign
	// a new closing tx.
	case *OfferReceivedEvent:
		return &CloseStateTransition{
			NextState: &RemoteCloseStart{
				CloseChannelTerms: c.CloseChannelTerms,
			},
			NewEvents: fn.Some(protofsm.EmittedEvent[ProtocolEvent]{
				InternalEvent: []ProtocolEvent{msg},
			}),
		}, nil

	default:

		return &CloseStateTransition{
			NextState: c,
		}, nil
	}
}

// ProcessEvent is the event processing for out terminal state. In this state,
// we just keep looping back on ourselves.
func (c *CloseFin) ProcessEvent(_ ProtocolEvent,
	_ *Environment) (*CloseStateTransition, error) {

	return &CloseStateTransition{
		NextState: c,
	}, nil
}

// ProcessEvent is a semi-terminal state in the rbf-coop close state machine.
// In this state, we hit a validation error in an earlier state, so we'll remain
// in this state for the user to examine. We may also process new requests to
// continue the state machine.
func (c *CloseErr) ProcessEvent(event ProtocolEvent,
	_ *Environment) (*CloseStateTransition, error) {

	switch msg := event.(type) {
	// If we get a send offer event in this state, then we're doing a state
	// transition to the LocalCloseStart state, so we can sign a new closing
	// tx.
	case *SendOfferEvent:
		return &CloseStateTransition{
			NextState: &LocalCloseStart{
				CloseChannelTerms: c.CloseChannelTerms,
			},
			NewEvents: fn.Some(protofsm.EmittedEvent[ProtocolEvent]{
				InternalEvent: []ProtocolEvent{msg},
			}),
		}, nil

	// If we get an offer received event, then we're doing a state
	// transition to the RemoteCloseStart, as the remote peer wants to sign
	// a new closing tx.
	case *OfferReceivedEvent:
		return &CloseStateTransition{
			NextState: &RemoteCloseStart{
				CloseChannelTerms: c.CloseChannelTerms,
			},
			NewEvents: fn.Some(protofsm.EmittedEvent[ProtocolEvent]{
				InternalEvent: []ProtocolEvent{msg},
			}),
		}, nil

	default:
		return &CloseStateTransition{
			NextState: c,
		}, nil
	}
}

package chancloser

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/mempool"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
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
	// ErrInvalidStateTransition is returned if the remote party tries to
	// close, but the thaw height hasn't been matched yet.
	ErrThawHeightNotReached = fmt.Errorf("thaw height not reached")
)

// sendShutdownEvents is a helper function that returns a set of daemon events
// we need to emit when we decide that we should send a shutdown message. We'll
// also mark the channel as borked as well, as at this point, we no longer want
// to continue with normal operation. This function also returns the actual closee
// nonce used (either provided or auto-generated) for taproot channels.
func sendShutdownEvents(chanID lnwire.ChannelID, chanPoint wire.OutPoint,
	deliveryAddr lnwire.DeliveryAddress, peerPub btcec.PublicKey,
	postSendEvent fn.Option[ProtocolEvent], chanState ChanStateObserver,
	env *Environment, localCloseeNonce fn.Option[lnwire.Musig2Nonce],
) (protofsm.DaemonEventSet, fn.Option[lnwire.Musig2Nonce], error) {

	// Create the shutdown message.
	shutdownMsg := &lnwire.Shutdown{
		ChannelID: chanID,
		Address:   deliveryAddr,
	}

	none := fn.None[lnwire.Musig2Nonce]()

	// For taproot channels using modern RBF flow, auto-generate closee
	// nonce if not provided. The shutdown message only contains our closee
	// nonce - the nonce the remote party will use when they act as closer.
	if env.IsTaproot() {
		// If closee nonce not provided, generate one now. Note how we
		// generate it using the RemoteMusigSession, as that'll set our
		// localNonce, we'll receive their remoteNonce for this session
		// once we get their ClosingComplete message.
		if localCloseeNonce.IsNone() {
			remoteMusig := env.RemoteMusigSession
			if remoteMusig != nil {
				closeeNonces, err := remoteMusig.ClosingNonce()
				if err != nil {
					return nil, none, fmt.Errorf("unable "+
						"to generate closee "+
						"nonce: %w", err)
				}
				localCloseeNonce = fn.Some(
					lnwire.Musig2Nonce(
						closeeNonces.PubNonce,
					),
				)
			}
		}
	}

	// If we have a closee nonce, then make sure to include it in the
	// shutdown message.
	localCloseeNonce.WhenSome(func(nonce lnwire.Musig2Nonce) {
		shutdownMsg.ShutdownNonce = lnwire.SomeShutdownNonce(nonce)
	})

	// We'll emit a daemon event that instructs the daemon to send out a new
	// shutdown message to the remote peer.
	msgsToSend := &protofsm.SendMsgEvent[ProtocolEvent]{
		TargetPeer: peerPub,
		Msgs:       []lnwire.Message{shutdownMsg},
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
		return protofsm.DaemonEventSet{msgsToSend}, localCloseeNonce, nil
	}

	// Before closing, we'll attempt to send a disable update for the
	// channel.  We do so before closing the channel as otherwise the
	// current edge policy won't be retrievable from the graph.
	if err := chanState.DisableChannel(); err != nil {
		return nil, none, fmt.Errorf("unable to disable "+
			"channel: %w", err)
	}

	// If we have a post-send event, then this means that we're the
	// responder. We'll use this fact below to update state in the DB.
	isInitiator := postSendEvent.IsNone()

	chancloserLog.Infof("ChannelPoint(%v): disabling outgoing adds",
		chanPoint)

	// As we're about to send a shutdown, we'll disable adds in the
	// outgoing direction.
	if err := chanState.DisableOutgoingAdds(); err != nil {
		return nil, none, fmt.Errorf("unable to disable "+
			"outgoing adds: %w", err)
	}

	// To be able to survive a restart, we'll also write to disk
	// information about the shutdown we're about to send out.
	err := chanState.MarkShutdownSent(deliveryAddr, isInitiator)
	if err != nil {
		return nil, none, fmt.Errorf("unable to mark "+
			"shutdown sent: %w", err)
	}

	chancloserLog.Debugf("ChannelPoint(%v): marking channel as borked",
		chanPoint)

	return protofsm.DaemonEventSet{msgsToSend}, localCloseeNonce, nil
}

// initLocalMusigCloseeNonce initializes the LocalMusigSession with the remote's
// closee nonce. This is used when we act as the closer to create a closing
// transaction.
func initLocalMusigCloseeNonce(env *Environment,
	remoteCloseeNonce fn.Option[lnwire.Musig2Nonce]) {

	if env.LocalMusigSession != nil {
		remoteCloseeNonce.WhenSome(func(nonce lnwire.Musig2Nonce) {
			remoteMusigNonce := musig2.Nonces{PubNonce: nonce}
			env.LocalMusigSession.InitRemoteNonce(&remoteMusigNonce)
		})
	}
}

// initRemoteMusigCloserNonce initializes the RemoteMusigSession with the
// remote party's closer nonce. This is called when we receive ClosingComplete
// and we're acting as closee. The nonce passed in is the remote's JIT closer
// nonce from their ClosingComplete message.
func initRemoteMusigCloserNonce(env *Environment,
	remoteCloserNonce fn.Option[lnwire.Musig2Nonce]) {

	if env.RemoteMusigSession != nil {
		remoteCloserNonce.WhenSome(func(nonce lnwire.Musig2Nonce) {
			remoteMusigNonce := musig2.Nonces{PubNonce: nonce}
			env.RemoteMusigSession.InitRemoteNonce(&remoteMusigNonce)
		})
	}
}

// validateShutdown is a helper function that validates that the shutdown has a
// proper delivery script, and can be sent based on the current thaw height of
// the channel.
func validateShutdown(chanThawHeight fn.Option[uint32],
	upfrontAddr fn.Option[lnwire.DeliveryAddress],
	msg *ShutdownReceived, chanPoint wire.OutPoint,
	chainParams chaincfg.Params, isTaproot bool) error {

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

	// For taproot channels, validate that the shutdown message includes
	// the required nonce for the RBF cooperative close flow.
	if isTaproot && !msg.RemoteShutdownNonce.IsSome() {
		return ErrTaprootShutdownNonceMissing
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
func (c *ChannelActive) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

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
		daemonEvents, closeeNonce, err := sendShutdownEvents(
			env.ChanID, env.ChanPoint, shutdownScript,
			env.ChanPeer, fn.None[ProtocolEvent](),
			env.ChanObserver, env, msg.CloseeNonce,
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
				NonceState: NonceState{
					LocalCloseeNonce: closeeNonce,
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
			env.ChanPoint, env.ChainParams, env.IsTaproot(),
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
		daemonEvents, closeeNonce, err := sendShutdownEvents(
			env.ChanID, env.ChanPoint, shutdownAddr,
			env.ChanPeer,
			fn.Some[ProtocolEvent](&ShutdownComplete{}),
			env.ChanObserver, env, fn.None[lnwire.Musig2Nonce](),
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

		// Initialize our LocalMusigSession with their closee nonce.
		// This prepares the session for when we act as closer.
		initLocalMusigCloseeNonce(env, msg.RemoteShutdownNonce)

		return &CloseStateTransition{
			NextState: &ShutdownPending{
				ShutdownScripts: ShutdownScripts{
					LocalDeliveryScript:  shutdownAddr,
					RemoteDeliveryScript: remoteAddr,
				},
				NonceState: NonceState{
					RemoteCloseeNonce: msg.RemoteShutdownNonce,
					LocalCloseeNonce:  closeeNonce,
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
func (s *ShutdownPending) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

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
			env.ChanPoint, env.ChainParams, env.IsTaproot(),
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

		// Initialize our LocalMusigSession with their closee nonce.
		// This prepares the session for when we act as closer.
		initLocalMusigCloseeNonce(env, msg.RemoteShutdownNonce)

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

		// Make sure that we stash their closee nonce, so we can make a
		// sig if needed in the next state transition.
		updatedNonceState := s.NonceState
		updatedNonceState.RemoteCloseeNonce = msg.RemoteShutdownNonce

		// We transition to the ChannelFlushing state, where we await
		// the ChannelFlushed event.
		return &CloseStateTransition{
			NextState: &ChannelFlushing{
				IdealFeeRate: s.IdealFeeRate,
				ShutdownScripts: ShutdownScripts{
					LocalDeliveryScript:  s.LocalDeliveryScript, //nolint:ll
					RemoteDeliveryScript: msg.ShutdownScript,    //nolint:ll
				},
				NonceState: updatedNonceState,
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
				NonceState:      s.NonceState,
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
func (c *ChannelFlushing) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

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
			NonceState:       c.NonceState,
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
	env *Environment, chanPeer lntypes.ChannelParty,
) (*CloseStateTransition, error) {

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

// partialSigToWireSig converts a PartialSig to a wire Sig format for taproot.
func partialSigToWireSig(partialSig lnwire.PartialSig) lnwire.Sig {
	var wireSig lnwire.Sig
	sigBytes := partialSig.Sig.Bytes()
	copy(wireSig.RawBytes()[:32], sigBytes[:])
	wireSig.ForceSchnorr()
	return wireSig
}

// extractTaprootSigAndNonce extracts the partial signature and closee nonce
// from a taproot ClosingSig message.
func extractTaprootSigAndNonce(msg lnwire.ClosingSig) (sig fn.Result[lnwire.Sig],
	nonce fn.Option[lnwire.Musig2Nonce]) {

	// Count how many taproot sig fields are populated.
	taprootSigInts := []bool{
		msg.TaprootPartialSigs.CloserNoClosee.IsSome(),
		msg.TaprootPartialSigs.NoCloserClosee.IsSome(),
		msg.TaprootPartialSigs.CloserAndClosee.IsSome(),
	}
	numTaprootSigs := fn.Foldl(0, taprootSigInts, func(acc int, sigInt bool) int { //nolint:ll
		if sigInt {
			return acc + 1
		}
		return acc
	})

	// Validate exactly one sig is set.
	if numTaprootSigs != 1 {
		return fn.Errf[lnwire.Sig]("%w: only one sig should be set, got %v",
			ErrTooManySigs, numTaprootSigs), fn.None[lnwire.Musig2Nonce]()
	}

	tapSigs := msg.TaprootPartialSigs

	// Extract the partial signature from whichever field has it.
	var extractedSig lnwire.Sig
	switch {
	case msg.TaprootPartialSigs.CloserNoClosee.IsSome():
		tapSigs.CloserNoClosee.WhenSomeV(func(ps lnwire.PartialSig) {
			extractedSig = partialSigToWireSig(ps)
		})

	case msg.TaprootPartialSigs.NoCloserClosee.IsSome():
		tapSigs.NoCloserClosee.WhenSomeV(func(ps lnwire.PartialSig) {
			extractedSig = partialSigToWireSig(ps)
		})

	case msg.TaprootPartialSigs.CloserAndClosee.IsSome():
		tapSigs.CloserAndClosee.WhenSomeV(func(ps lnwire.PartialSig) {
			extractedSig = partialSigToWireSig(ps)
		})
	}

	// Extract the closee nonce, for taproot channels, we expect this to
	// always be present.
	var nextCloseeNonce fn.Option[lnwire.Musig2Nonce]
	msg.NextCloseeNonce.WhenSomeV(func(nonce lnwire.Musig2Nonce) {
		nextCloseeNonce = fn.Some(nonce)
	})

	// Validate that NextCloseeNonce is always set for taproot channels.
	if nextCloseeNonce.IsNone() {
		return fn.Errf[lnwire.Sig]("NextCloseeNonce must be set for " +
			"taproot channels"), fn.None[lnwire.Musig2Nonce]()
	}

	return fn.Ok(extractedSig), nextCloseeNonce
}

// extractRegularSig extracts the signature from a non-taproot ClosingSig
// message.
func extractRegularSig(msg lnwire.ClosingSig) fn.Result[lnwire.Sig] {
	// Count how many regular sig fields are populated
	regularSigInts := []bool{
		msg.ClosingSigs.CloserNoClosee.IsSome(),
		msg.ClosingSigs.NoCloserClosee.IsSome(),
		msg.ClosingSigs.CloserAndClosee.IsSome(),
	}
	numRegularSigs := fn.Foldl(0, regularSigInts, func(acc int,
		sigInt bool) int {

		if sigInt {
			return acc + 1
		}
		return acc
	})

	// Validate exactly one sig is set
	if numRegularSigs != 1 {
		return fn.Errf[lnwire.Sig]("%w: only one sig should be "+
			"set, got %v", ErrTooManySigs, numRegularSigs)
	}

	// Extract the signature from the appropriate field
	switch {
	case msg.ClosingSigs.CloserNoClosee.IsSome():
		var sig lnwire.Sig
		msg.ClosingSigs.CloserNoClosee.WhenSomeV(func(s lnwire.Sig) {
			sig = s
		})
		return fn.Ok(sig)

	case msg.ClosingSigs.NoCloserClosee.IsSome():
		var sig lnwire.Sig
		msg.ClosingSigs.NoCloserClosee.WhenSomeV(func(s lnwire.Sig) {
			sig = s
		})
		return fn.Ok(sig)

	case msg.ClosingSigs.CloserAndClosee.IsSome():
		var sig lnwire.Sig
		msg.ClosingSigs.CloserAndClosee.WhenSomeV(func(s lnwire.Sig) {
			sig = s
		})
		return fn.Ok(sig)

	default:
		return fn.Errf[lnwire.Sig]("no signature found")
	}
}

// extractSigAndNonce extracts the signature and optional nonce from a
// ClosingSig message. For taproot channels, it extracts both the partial
// signature and the JIT nonce. For non-taproot channels, it extracts just the
// signature.
func extractSigAndNonce(msg lnwire.ClosingSig,
) (sig fn.Result[lnwire.Sig], nonce fn.Option[lnwire.Musig2Nonce]) {

	// Check if this is a taproot or regular signature.
	hasTaprootSigs := msg.TaprootPartialSigs.CloserNoClosee.IsSome() ||
		msg.TaprootPartialSigs.NoCloserClosee.IsSome() ||
		msg.TaprootPartialSigs.CloserAndClosee.IsSome()

	hasRegularSigs := msg.ClosingSigs.CloserNoClosee.IsSome() ||
		msg.ClosingSigs.NoCloserClosee.IsSome() ||
		msg.ClosingSigs.CloserAndClosee.IsSome()

	// Make sure that only a single set of signatures is present.
	if hasTaprootSigs && hasRegularSigs {
		return fn.Errf[lnwire.Sig]("both taproot and regular " +
			"sigs present"), fn.None[lnwire.Musig2Nonce]()
	}

	// If it's a taprotot sig, then we may need to also extract the nonce.
	if hasTaprootSigs {
		return extractTaprootSigAndNonce(msg)
	}

	return extractRegularSig(msg), fn.None[lnwire.Musig2Nonce]()
}

// validateAndExtractSigAndNonce validates that the signature type matches the
// channel type and then extracts the signature and nonce.
func validateAndExtractSigAndNonce(msg lnwire.ClosingSig,
	isTaproot bool) (sig fn.Result[lnwire.Sig], nonce fn.Option[lnwire.Musig2Nonce]) {

	// Check if this is a taproot or regular signature.
	hasTaprootSigs := msg.TaprootPartialSigs.CloserNoClosee.IsSome() ||
		msg.TaprootPartialSigs.NoCloserClosee.IsSome() ||
		msg.TaprootPartialSigs.CloserAndClosee.IsSome()

	hasRegularSigs := msg.ClosingSigs.CloserNoClosee.IsSome() ||
		msg.ClosingSigs.NoCloserClosee.IsSome() ||
		msg.ClosingSigs.CloserAndClosee.IsSome()

	// Assert that the signature type matches the channel type.
	switch {
	case isTaproot && !hasTaprootSigs && hasRegularSigs:
		return fn.Errf[lnwire.Sig]("taproot channel requires " +
				"taproot signatures, got regular signatures"),
			fn.None[lnwire.Musig2Nonce]()

	case !isTaproot && hasTaprootSigs && !hasRegularSigs:
		return fn.Errf[lnwire.Sig]("non-taproot channel requires " +
				"regular signatures, got taproot signatures"),
			fn.None[lnwire.Musig2Nonce]()
	}

	// If everything is clear, then we'll go ahead and extract the
	// signatures.
	return extractSigAndNonce(msg)
}

// updateAndValidateCloseTerms is a helper function that validates examines the
// incoming event, and decide if we need to update the remote party's address,
// or reject it if it doesn't include our latest address.
func (c *ClosingNegotiation) updateAndValidateCloseTerms(event ProtocolEvent,
	isTaproot bool) error {

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
func (c *ClosingNegotiation) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

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
	err := c.updateAndValidateCloseTerms(event, env.IsTaproot())
	if err != nil {
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

// encodeClosingSignatures is a helper function that creates the appropriate
// signature structures for the closing_complete message based on the channel
// type and dust status.
func encodeClosingSignatures(env *Environment, wireSig lnwire.Sig,
	musigPartialSig *lnwallet.MusigPartialSig, noCloser, noClosee bool,
) (lnwire.ClosingSigs, lnwire.TaprootClosingSigs, error) {

	var (
		closingSigs        lnwire.ClosingSigs
		taprootClosingSigs lnwire.TaprootClosingSigs
	)

	// If this is a taproot channel, then we'll return the taproot specific
	// closing sigs variant.
	if env.IsTaproot() {
		if musigPartialSig == nil {
			return closingSigs, taprootClosingSigs,
				fmt.Errorf("missing partial signature for " +
					"taproot channel")
		}

		// Convert the musig partial sig to wire format.
		// This already includes our JIT closer nonce that we used to sign.
		partialSigWithNonce := musigPartialSig.ToWireSig()

		switch {
		case noCloser:
			taprootClosingSigs.NoCloserClosee = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType6](*partialSigWithNonce),
			)
		case noClosee:
			taprootClosingSigs.CloserNoClosee = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType5](*partialSigWithNonce),
			)
		default:
			taprootClosingSigs.CloserAndClosee = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType7](*partialSigWithNonce),
			)
		}

		return closingSigs, taprootClosingSigs, nil
	}

	// For non-taproot channels, we'll populate the normal ECDSA sigantures.
	switch {
	case noClosee:
		closingSigs.CloserNoClosee = newSigTlv[tlv.TlvType1](wireSig)
	case noCloser:
		closingSigs.NoCloserClosee = newSigTlv[tlv.TlvType2](wireSig)
	default:
		closingSigs.CloserAndClosee = newSigTlv[tlv.TlvType3](wireSig)
	}

	return closingSigs, taprootClosingSigs, nil
}

// ProcessEvent implements the event processing to kick off the process of
// obtaining a new (possibly RBF'd) signature for our commitment transaction.
func (l *LocalCloseStart) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

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

		var closeOpts []lnwallet.ChanCloseOpt
		closeOpts = append(closeOpts,
			lnwallet.WithCustomSequence(mempool.MaxRBFSequence),
			lnwallet.WithCustomPayer(lntypes.Local),
		)

		// For taproot channels, we need to use the LocalMusigSession
		// for signing when we're the closer (sending closing_complete).
		if env.IsTaproot() {
			// Initialize with the remote's closee nonce for
			// signing. This may be using the very first nonce they
			// send in shutdown, or the nonce they sent in
			// ClosingSig after responding to our prior offer.
			initLocalMusigCloseeNonce(
				env, l.NonceState.RemoteCloseeNonce,
			)

			// Generate our JIT closer nonce. This sets the internal
			// localNonce field in LocalMusigSession.
			_, err := env.LocalMusigSession.ClosingNonce()
			if err != nil {
				return nil, fmt.Errorf("failed to generate "+
					"JIT closer nonce: %w", err)
			}

			//nolint:ll
			musigOpts, err := env.LocalMusigSession.ProposalClosingOpts()
			if err != nil {
				return nil, fmt.Errorf("failed to get musig "+
					"closing opts: %w", err)
			}
			closeOpts = append(closeOpts, musigOpts...)
		}

		rawSig, closeTx, closeBalance, err := env.CloseSigner.CreateCloseProposal( //nolint:ll
			absoluteFee, localScript, l.RemoteDeliveryScript,
			closeOpts...,
		)
		if err != nil {
			return nil, err
		}

		// Depending on the channel type, we'll be encoding a normal
		// sig, or a musig2 partial sig.
		var (
			wireSig         lnwire.Sig
			musigPartialSig *lnwallet.MusigPartialSig
		)

		// Depending on the channel type, we'll either have a partial
		// signature, or a regular signature.
		switch {
		case env.IsTaproot():
			var ok bool
			musigPartialSig, ok = rawSig.(*lnwallet.MusigPartialSig)
			if !ok {
				return nil, fmt.Errorf("expected "+
					"MusigPartialSig for taproot "+
					"channel, got %T", rawSig)
			}

			// Convert to schnorr shell format for wire sig.
			schnorrSig := musigPartialSig.ToSchnorrShell()
			wireSig, err = lnwire.NewSigFromSignature(schnorrSig)
			if err != nil {
				return nil, err
			}
		default:
			// For non-taproot channels, use regular signature
			// conversion.
			wireSig, err = lnwire.NewSigFromSignature(rawSig)
			if err != nil {
				return nil, err
			}
		}

		chancloserLog.Infof("closing w/ local_addr=%x, "+
			"remote_addr=%x, fee=%v", localScript[:],
			l.RemoteDeliveryScript[:], absoluteFee)

		chancloserLog.Infof("proposing closing_tx=%v",
			spew.Sdump(closeTx))

		var noClosee, noCloser bool
		switch {
		case remoteTxOut == nil:
			noClosee = true
		case closeBalance < lnwallet.DustLimitForSize(len(localScript)):
			noCloser = true
		}

		// Create the appropriate signature structures based on channel
		// type.
		closingSigs, taprootClosingSigs, err := encodeClosingSignatures(
			env, wireSig, musigPartialSig, noCloser, noClosee,
		)
		if err != nil {
			return nil, err
		}

		closingCompleteMsg := &lnwire.ClosingComplete{
			ChannelID:          env.ChanID,
			CloserScript:       l.LocalDeliveryScript,
			CloseeScript:       l.RemoteDeliveryScript,
			FeeSatoshis:        absoluteFee,
			LockTime:           env.BlockHeight,
			ClosingSigs:        closingSigs,
			TaprootClosingSigs: taprootClosingSigs,
		}

		// TODO(roasbeef): type alias for protocol event
		sendEvent := protofsm.DaemonEventSet{&protofsm.SendMsgEvent[ProtocolEvent]{ //nolint:ll
			TargetPeer: env.ChanPeer,
			Msgs:       []lnwire.Message{closingCompleteMsg},
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

// extractTaprootPartialSigWithNonce extracts the PartialSigWithNonce from
// TaprootClosingSigs. It returns the partial sig, which field it was found in,
// and whether it's a NoClosee case.
func extractTaprootPartialSigWithNonce(sigs lnwire.TaprootClosingSigs) (
	partialSig fn.Option[lnwire.PartialSigWithNonce], isNoClosee bool) {

	if sigs.CloserNoClosee.IsSome() {
		var ps lnwire.PartialSigWithNonce
		sigs.CloserNoClosee.WhenSomeV(func(p lnwire.PartialSigWithNonce) {
			ps = p
		})
		return fn.Some(ps), true
	}

	if sigs.NoCloserClosee.IsSome() {
		var ps lnwire.PartialSigWithNonce
		sigs.NoCloserClosee.WhenSomeV(func(p lnwire.PartialSigWithNonce) {
			ps = p
		})
		return fn.Some(ps), false
	}

	if sigs.CloserAndClosee.IsSome() {
		var ps lnwire.PartialSigWithNonce
		sigs.CloserAndClosee.WhenSomeV(func(p lnwire.PartialSigWithNonce) {
			ps = p
		})
		return fn.Some(ps), false
	}

	return fn.None[lnwire.PartialSigWithNonce](), false
}

// createClosingSigMessage creates the ClosingSig message response for the
// closee role.
func createClosingSigMessage(env *Environment, wireSig lnwire.Sig,
	localSig input.Signature,
	localScript, remoteScript lnwire.DeliveryAddress, fee btcutil.Amount,
	lockTime uint32, noClosee bool) (*lnwire.ClosingSig, error) {

	var (
		closingSigs        lnwire.ClosingSigs
		taprootPartialSigs lnwire.TaprootPartialSigs
		nextCloseeNonce    tlv.OptionalRecordT[
			tlv.TlvType22, lnwire.Musig2Nonce,
		]
	)

	// For taproot channels, use PartialSig (no nonce) since receiver knows
	// our nonce
	if env.IsTaproot() {
		// We already have the MusigPartialSig from earlier.
		musigSig := localSig.(*lnwallet.MusigPartialSig)
		wireSigWithNonce := musigSig.ToWireSig()
		partialSig := wireSigWithNonce.PartialSig

		if noClosee {
			taprootPartialSigs.CloserNoClosee = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType5](partialSig),
			)
		} else {
			taprootPartialSigs.CloserAndClosee = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType7](partialSig),
			)
		}

		// Generate our next closee nonce for the next RBF iteration.
		// This is the nonce the closer should use for our closee
		// signature in the next RBF round. We always include this since
		// RBF could occur.
		nextNonces, err := env.RemoteMusigSession.ClosingNonce()
		if err != nil {
			return nil, fmt.Errorf("failed to generate next "+
				"closee nonce: %w", err)
		}
		nextCloseeNonce = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType22](
				lnwire.Musig2Nonce(nextNonces.PubNonce),
			),
		)
	} else {
		// Non-taproot: use regular signatures.
		if noClosee {
			closingSigs.CloserNoClosee = newSigTlv[tlv.TlvType1](
				wireSig,
			)
		} else {
			closingSigs.CloserAndClosee = newSigTlv[tlv.TlvType3](
				wireSig,
			)
		}
	}

	return &lnwire.ClosingSig{
		ChannelID:          env.ChanID,
		CloserScript:       remoteScript,
		CloseeScript:       localScript,
		FeeSatoshis:        fee,
		LockTime:           lockTime,
		ClosingSigs:        closingSigs,
		TaprootPartialSigs: taprootPartialSigs,
		NextCloseeNonce:    nextCloseeNonce,
	}, nil
}

// extractTaprootPartialSig extracts just the PartialSig from TaprootPartialSigs.
// This is useful when we need the actual partial sig for combining.
func extractTaprootPartialSig(sigs lnwire.TaprootPartialSigs) (
	partialSig fn.Option[lnwire.PartialSig]) {

	if sigs.CloserNoClosee.IsSome() {
		var ps lnwire.PartialSig
		sigs.CloserNoClosee.WhenSomeV(func(p lnwire.PartialSig) {
			ps = p
		})
		return fn.Some(ps)
	}

	if sigs.NoCloserClosee.IsSome() {
		var ps lnwire.PartialSig
		sigs.NoCloserClosee.WhenSomeV(func(p lnwire.PartialSig) {
			ps = p
		})
		return fn.Some(ps)
	}

	if sigs.CloserAndClosee.IsSome() {
		var ps lnwire.PartialSig
		sigs.CloserAndClosee.WhenSomeV(func(p lnwire.PartialSig) {
			ps = p
		})
		return fn.Some(ps)
	}

	return fn.None[lnwire.PartialSig]()
}

// prepareClosingSignatures prepares the local and remote signatures for the
// closing transaction. For taproot channels, it handles musig signature
// combination. For non-taproot channels, it converts wire signatures to regular
// signatures.
func prepareClosingSignatures(env *Environment, l *LocalOfferSent,
	msg *LocalSigReceived, sig lnwire.Sig,
	closeOpts []lnwallet.ChanCloseOpt,
) (localSig, remoteSig input.Signature, err error) {

	if env.IsTaproot() {
		// For taproot channels, we need to reconstruct the
		// MusigPartialSig from the wire signature. We'll need to create
		// a new CreateCloseProposal to get the proper MusigPartialSig
		// that CompleteCooperativeClose expects.
		rawLocalSig, _, _, err := env.CloseSigner.CreateCloseProposal(
			l.ProposedFee, l.LocalDeliveryScript,
			l.RemoteDeliveryScript, closeOpts...,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to recreate "+
				"local sig: %w", err)
		}
		localSig = rawLocalSig

		// Extract the partial sig from the message using our helper
		// function.
		remotePartialSigOpt := extractTaprootPartialSig(
			msg.SigMsg.TaprootPartialSigs,
		)
		if remotePartialSigOpt.IsNone() {
			return nil, nil, fmt.Errorf("no taproot partial " +
				"sig found in message")
		}

		remotePartialSig := remotePartialSigOpt.UnwrapOr(
			lnwire.PartialSig{},
		)

		// We also need our local partial sig in wire format.
		localMusigSig, ok := rawLocalSig.(*lnwallet.MusigPartialSig)
		if !ok {
			return nil, nil, fmt.Errorf("expected local sig to "+
				"be MusigPartialSig, got %T", rawLocalSig)
		}
		localPartialSig := localMusigSig.ToWireSig().PartialSig

		// Use CombineClosingOpts to get the proper signatures.
		// notlint:ll
		localCombined, remoteCombined, _, err := env.LocalMusigSession.CombineClosingOpts(
			localPartialSig, remotePartialSig,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to combine "+
				"closing opts: %w", err)
		}

		return localCombined, remoteCombined, nil
	}

	// For non-taproot channels, convert wire signatures to regular
	// signatures.
	remoteSig, err = sig.ToSignature()
	if err != nil {
		return nil, nil, err
	}
	localSig, err = l.LocalSig.ToSignature()
	if err != nil {
		return nil, nil, err
	}

	return localSig, remoteSig, nil
}

// ProcessEvent implements the state transition function for the
// LocalOfferSent state. In this state, we'll wait for the remote party to
// send a close_signed message which gives us the ability to broadcast a new
// co-op close transaction.
func (l *LocalOfferSent) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

	switch msg := event.(type) { //nolint:gocritic
	// If we receive a LocalSigReceived event, then we'll attempt to
	// validate the signature from the remote party. If valid, then we can
	// broadcast the transaction, and transition to the ClosePending state.
	case *LocalSigReceived:
		// Extract and validate that only one sig field is set. For
		// taproot channels, we also extract the NextCloseeNonce.
		sigResult, nextCloseeNonce := validateAndExtractSigAndNonce(
			msg.SigMsg, env.IsTaproot(),
		)
		sig, err := sigResult.Unpack()
		if err != nil {
			return nil, err
		}

		var closeOpts []lnwallet.ChanCloseOpt
		closeOpts = append(closeOpts,
			lnwallet.WithCustomSequence(mempool.MaxRBFSequence),
			lnwallet.WithCustomPayer(lntypes.Local),
		)

		// For taproot channels, we need to initialize the remote's
		// closee nonce BEFORE calling ProposalClosingOpts. We use the
		// nonce from NonceState (set during shutdown or from prior
		// ClosingSig's NextCloseeNonce).
		if env.IsTaproot() {
			// Initialize the remote nonce from our stored state.
			// This is the nonce the remote party committed to in
			// their shutdown message (or their previous ClosingSig).
			initLocalMusigCloseeNonce(env, l.NonceState.RemoteCloseeNonce)

			// Now that the nonce is initialized, we can safely get
			// the musig closing options.
			musigOpts, err := env.LocalMusigSession.ProposalClosingOpts()
			if err != nil {
				return nil, fmt.Errorf("failed to get musig "+
					"closing opts: %w", err)
			}
			closeOpts = append(closeOpts, musigOpts...)

			// Update NonceState with the new nonce from ClosingSig
			// for potential future RBF iterations.
			l.NonceState.RemoteCloseeNonce = nextCloseeNonce
		}

		localSig, remoteSig, err := prepareClosingSignatures(
			env, l, msg, sig, closeOpts,
		)
		if err != nil {
			return nil, err
		}

		// Now that we have their signature, we'll attempt to validate
		// it, then extract a valid closing signature from it.
		closeTx, _, err := env.CloseSigner.CompleteCooperativeClose(
			localSig, remoteSig, l.LocalDeliveryScript,
			l.RemoteDeliveryScript, l.ProposedFee, closeOpts...,
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

// processRemoteTaprootSig handles the extraction and processing of a remote
// taproot signature for the closee role. It extracts the partial sig with
// nonce, initializes the musig session, and returns the remote signature.
func processRemoteTaprootSig(env *Environment, msg lnwire.ClosingComplete,
	jitNonce fn.Option[lnwire.Musig2Nonce]) (input.Signature, error) {

	// Initialize the RemoteMusigSession with their JIT closer nonce. We
	// already added our local nonce either during shutdown, or with our
	// last ClosingSig message.
	initRemoteMusigCloseeNonce(env, jitNonce)

	partialSigOpt, _ := extractTaprootPartialSigWithNonce(msg.TaprootClosingSigs)
	if partialSigOpt.IsNone() {
		return nil, fmt.Errorf("no taproot partial sig found in message")
	}

	var remotePartialSig lnwire.PartialSigWithNonce
	partialSigOpt.WhenSome(func(ps lnwire.PartialSigWithNonce) {
		remotePartialSig = ps
	})

	// Create a MusigPartialSig from the wire format The Nonce in
	// PartialSigWithNonce is their next closee nonce for future RBF. We
	// store it but don't use it for verification of this signature.
	remoteSig := lnwallet.NewMusigPartialSig(
		&musig2.PartialSignature{
			S: &remotePartialSig.PartialSig.Sig,
		},
		remotePartialSig.Nonce, lnwire.Musig2Nonce{}, nil,
		fn.None[chainhash.Hash](),
	)

	return remoteSig, nil
}

// createLocalCloseeSignature creates our local signature for the closee role.
// It returns both the wire format signature and the input.Signature.
func createLocalCloseeSignature(env *Environment, fee btcutil.Amount,
	localScript, remoteScript lnwire.DeliveryAddress,
	chanOpts []lnwallet.ChanCloseOpt) (lnwire.Sig, input.Signature, error) {

	rawSig, _, _, err := env.CloseSigner.CreateCloseProposal(
		fee, localScript, remoteScript, chanOpts...,
	)
	if err != nil {
		return lnwire.Sig{}, nil, fmt.Errorf("failed to "+
			"create close proposal: %w", err)
	}

	var (
		wireSig  lnwire.Sig
		localSig input.Signature
	)

	if env.IsTaproot() {
		musigSig, ok := rawSig.(*lnwallet.MusigPartialSig)
		if !ok {
			return lnwire.Sig{}, nil, fmt.Errorf("expected "+
				"MusigPartialSig for taproot channel, got %T",
				rawSig)
		}

		// Convert to schnorr shell format for wire sig encoding.
		schnorrSig := musigSig.ToSchnorrShell()
		wireSig, err = lnwire.NewSigFromSignature(schnorrSig)
		if err != nil {
			return lnwire.Sig{}, nil, err
		}

		localSig = musigSig
	} else {
		wireSig, err = lnwire.NewSigFromSignature(rawSig)
		if err != nil {
			return lnwire.Sig{}, nil, err
		}

		localSig, err = wireSig.ToSignature()
		if err != nil {
			return lnwire.Sig{}, nil, err
		}
	}

	return wireSig, localSig, nil
}

// SigType represents either a regular or taproot signature.
// Left = regular signature, Right = taproot signature with nonce.
type SigType = fn.Either[lnwire.Sig, lnwire.PartialSigWithNonce]

// NewRegularSigType creates a SigType for a regular (non-taproot) signature.
func NewRegularSigType(sig lnwire.Sig) SigType {
	return fn.NewLeft[lnwire.Sig, lnwire.PartialSigWithNonce](sig)
}

// NewTaprootSigType creates a SigType for a taproot signature with nonce.
func NewTaprootSigType(ps lnwire.PartialSigWithNonce) SigType {
	return fn.NewRight[lnwire.Sig, lnwire.PartialSigWithNonce](ps)
}

// SigFieldSet represents which signature fields are present in a
// ClosingComplete message.
type SigFieldSet struct {
	// CloserNoClosee contains the signature for a transaction with only
	// the closer's output (closee's output is dust/excluded).
	CloserNoClosee fn.Option[SigType]

	// NoCloserClosee contains the signature for a transaction with only
	// the closee's output (closer's output is dust/excluded).
	NoCloserClosee fn.Option[SigType]

	// CloserAndClosee contains the signature for a transaction with both
	// outputs present.
	CloserAndClosee fn.Option[SigType]
}

// IsTaproot returns true if any taproot signatures are present in the field
// set.
func (s SigFieldSet) IsTaproot() bool {
	checkTaproot := func(opt fn.Option[SigType]) bool {
		return fn.MapOptionZ(opt, func(sig SigType) bool {
			return sig.IsRight()
		})
	}

	return checkTaproot(s.CloserNoClosee) ||
		checkTaproot(s.NoCloserClosee) ||
		checkTaproot(s.CloserAndClosee)
}

// HasAnySig returns true if at least one signature field is present.
func (s SigFieldSet) HasAnySig() bool {
	return s.CloserNoClosee.IsSome() ||
		s.NoCloserClosee.IsSome() ||
		s.CloserAndClosee.IsSome()
}

// parseSigFields extracts signature fields from a ClosingComplete message and
// returns a structured representation of which fields are present.
func parseSigFields(msg lnwire.ClosingComplete) SigFieldSet {
	var fields SigFieldSet

	// createSigType is a helper function that creates a SigType based on
	// field otpions.
	createSigType := func(
		taprootOpt fn.Option[lnwire.PartialSigWithNonce],
		regularOpt fn.Option[lnwire.Sig],
	) fn.Option[SigType] {

		// The taproot takes precedence if present.
		if taprootOpt.IsSome() {
			var ps lnwire.PartialSigWithNonce
			taprootOpt.WhenSome(func(p lnwire.PartialSigWithNonce) {
				ps = p
			})

			return fn.Some(NewTaprootSigType(ps))
		}

		// Otherwise, check for a regular signature.
		if regularOpt.IsSome() {
			var sig lnwire.Sig
			regularOpt.WhenSome(func(s lnwire.Sig) {
				sig = s
			})

			return fn.Some(NewRegularSigType(sig))
		}

		return fn.None[SigType]()
	}

	fields.CloserNoClosee = createSigType(
		msg.TaprootClosingSigs.CloserNoClosee.ValOpt(),
		msg.ClosingSigs.CloserNoClosee.ValOpt(),
	)

	fields.NoCloserClosee = createSigType(
		msg.TaprootClosingSigs.NoCloserClosee.ValOpt(),
		msg.ClosingSigs.NoCloserClosee.ValOpt(),
	)

	fields.CloserAndClosee = createSigType(
		msg.TaprootClosingSigs.CloserAndClosee.ValOpt(),
		msg.ClosingSigs.CloserAndClosee.ValOpt(),
	)

	return fields
}

// validateSigFields validates that the signature field set conforms to BOLT
// spec requirements based on the receiver's (closee's) output dust status.
func validateSigFields(sigFields SigFieldSet, localIsDust bool) error {
	// Check if any signature is present at all, if not then this is a
	// terminal error.
	if !sigFields.HasAnySig() {
		return ErrNoSig
	}

	// Per BOLT spec for the receiver (closee) of closing_complete:
	//
	// "Select a signature for validation:
	//   1. If the local output amount is dust: MUST use closer_output_only
	//      (CloserNoClosee).
	//   3. Otherwise, if closer_and_closee_outputs is present: MUST use
	//      closer_and_closee_outputs (CloserAndClosee).
	//   4. Otherwise: MUST use closee_output_only (NoCloserClosee)."
	//
	// We validate that the required signature field is present.
	if localIsDust {
		// Local output is dust, we need CloserNoClosee.
		if sigFields.CloserNoClosee.IsNone() {
			return ErrCloserNoClosee
		}
	} else {
		// Local output is not dust, we prefer CloserAndClosee, but can
		// fall back to NoCloserClosee per spec step 4.
		if sigFields.CloserAndClosee.IsNone() &&
			sigFields.NoCloserClosee.IsNone() {

			return ErrCloserAndClosee
		}
	}

	return nil
}

// selectAndExtractSig selects the appropriate signature field based on BOLT
// spec priority and extracts the signature and nonce.
func selectAndExtractSig(fields SigFieldSet, localIsDust bool) (
	sig lnwire.Sig, nonce fn.Option[lnwire.Musig2Nonce], isNoClosee bool,
	err error) {

	// Select which field to use based on BOLT spec priority.
	var selectedField fn.Option[SigType]
	if localIsDust {
		// Spec step 1: Local output is dust, use CloserNoClosee.
		selectedField = fields.CloserNoClosee
		isNoClosee = true
	} else {
		// Spec step 3: Prefer CloserAndClosee if present.
		if fields.CloserAndClosee.IsSome() {
			selectedField = fields.CloserAndClosee
			isNoClosee = false
		} else {
			// Spec step 4: Fallback to NoCloserClosee.
			selectedField = fields.NoCloserClosee
			isNoClosee = false
		}
	}

	// If the selected field is none, this is an error.
	sigType, err := selectedField.UnwrapOrErr(ErrNoSig)
	if err != nil {
		return lnwire.Sig{}, fn.None[lnwire.Musig2Nonce](), false, err
	}

	// Check if this is a taproot signature (Right side of Either) or
	// regular (Left side).
	nonce = fn.None[lnwire.Musig2Nonce]()

	// If this is a regular signature, extract it directly.
	sigType.WhenLeft(func(regularSig lnwire.Sig) {
		sig = regularSig
	})

	// Otherwise, for taproot, extract the partial sig and nonce.
	sigType.WhenRight(func(partialSig lnwire.PartialSigWithNonce) {
		nonce = fn.Some(partialSig.Nonce)

		sigBytes := partialSig.Sig.Bytes()
		copy(sig.RawBytes()[:32], sigBytes[:])

		sig.ForceSchnorr()
	})

	return sig, nonce, isNoClosee, nil
}

// extractSigAndNonceFromComplete extracts signature and optional nonce from
// ClosingComplete using a three-phase approach: parse, validate, and select.
//
// This function implements the BOLT spec requirements for the receiver (closee)
// of a closing_complete message.
func extractSigAndNonceFromComplete(msg lnwire.ClosingComplete,
	localIsDust bool) (sig lnwire.Sig, nonce fn.Option[lnwire.Musig2Nonce],
	isNoClosee bool, err error) {

	// First, parse the message to extract which signature fields are
	// present.
	fields := parseSigFields(msg)

	// Next, validate that the parsed fields conform to BOLT spec
	// requirements based on our (closee's) output dust status.
	if err := validateSigFields(fields, localIsDust); err != nil {
		return lnwire.Sig{}, fn.None[lnwire.Musig2Nonce](), false, err
	}

	// Finally, select and extract the appropriate signature based on BOLT
	// spec priority.
	sig, nonce, isNoClosee, err = selectAndExtractSig(fields, localIsDust)
	if err != nil {
		return lnwire.Sig{}, fn.None[lnwire.Musig2Nonce](), false, err
	}

	return sig, nonce, isNoClosee, nil
}

// ProcessEvent implements the state transition function for the
// RemoteCloseStart. In this state, we'll wait for the remote party to send a
// closing_complete message. Assuming they can pay for the fees, we'll sign it
// ourselves, then transition to the next state of ClosePending.
func (l *RemoteCloseStart) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

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

		// Extract the signature and JIT nonce from the ClosingComplete
		// message. This function parses, validates, and selects the
		// appropriate signature per BOLT spec.
		sig, jitNonce, noClosee, err := extractSigAndNonceFromComplete(
			msg.SigMsg, l.LocalAmtIsDust(),
		)
		if err != nil {
			return nil, err
		}

		chanOpts := []lnwallet.ChanCloseOpt{
			lnwallet.WithCustomSequence(mempool.MaxRBFSequence),
			lnwallet.WithCustomLockTime(msg.SigMsg.LockTime),
			lnwallet.WithCustomPayer(lntypes.Remote),
		}

		var remoteSig input.Signature

		// For taproot channels, add MusigSession options if available.
		// When we're the closee (sending closing_sig), we use
		// RemoteMusigSession.
		switch {
		case env.RemoteMusigSession != nil:
			// First, process the remote taproot signature which
			// initializes the remote nonce via InitRemoteNonce().
			// This must happen before ProposalClosingOpts() which
			// requires the nonce to be set.
			remoteSig, err = processRemoteTaprootSig(
				env, msg.SigMsg, jitNonce,
			)
			if err != nil {
				return nil, err
			}

			// Now that the nonce is initialized, get the musig
			// closing options.
			musigOpts, err := env.RemoteMusigSession.ProposalClosingOpts()
			if err != nil {
				return nil, fmt.Errorf("failed to get musig "+
					"closing opts: %w", err)
			}
			chanOpts = append(chanOpts, musigOpts...)
		default:
			remoteSig, err = sig.ToSignature()
			if err != nil {
				return nil, err
			}
		}

		chancloserLog.Infof("RemoteCloseStart: responding to close w/ "+
			"local_addr=%x, remote_addr=%x, fee=%v, locktime=%v",
			l.LocalDeliveryScript[:], l.RemoteDeliveryScript[:],
			msg.SigMsg.FeeSatoshis, msg.SigMsg.LockTime)

		// Now that we have the remote sig, we'll sign the version they
		// signed, then attempt to complete the cooperative close
		// process.
		//
		// TODO(roasbeef): need to be able to omit an output when
		// signing based on the above, as closing opt
		wireSig, localSig, err := createLocalCloseeSignature(
			env, msg.SigMsg.FeeSatoshis, l.LocalDeliveryScript,
			l.RemoteDeliveryScript, chanOpts,
		)
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

		closingSigMsg, err := createClosingSigMessage(
			env, wireSig, localSig, l.LocalDeliveryScript,
			l.RemoteDeliveryScript, msg.SigMsg.FeeSatoshis,
			msg.SigMsg.LockTime, noClosee,
		)
		if err != nil {
			return nil, err
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

		sendEvent := &protofsm.SendMsgEvent[ProtocolEvent]{
			TargetPeer: env.ChanPeer,
			Msgs:       []lnwire.Message{closingSigMsg},
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
func (c *ClosePending) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

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
func (c *CloseFin) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

	return &CloseStateTransition{
		NextState: c,
	}, nil
}

// ProcessEvent is a semi-terminal state in the rbf-coop close state machine.
// In this state, we hit a validation error in an earlier state, so we'll remain
// in this state for the user to examine. We may also process new requests to
// continue the state machine.
func (c *CloseErr) ProcessEvent(event ProtocolEvent, env *Environment,
) (*CloseStateTransition, error) {

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

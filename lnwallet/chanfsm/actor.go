package chanfsm

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
)

// DefaultMailboxSize is the default mailbox capacity of a channel actor.
const DefaultMailboxSize = 64

// Msg is a message to a channel actor.
type Msg interface {
	actor.Message

	chanMsg()
}

// CommandMsg carries a local command. Its reply is the command's result.
type CommandMsg struct {
	actor.BaseMessage

	// Cmd is the command: AddHTLC, SettleHTLC, FailHTLC,
	// MalformedFailHTLC, UpdateFee or SignCommitment.
	Cmd Event
}

// PeerMsg carries a message from the peer.
type PeerMsg struct {
	actor.BaseMessage

	// Msg is the message.
	Msg lnwire.Message
}

// connectMsg tells the actor the connection to the peer is up.
type connectMsg struct {
	actor.BaseMessage
}

// stateMsg asks for the current state.
type stateMsg struct {
	actor.BaseMessage
}

// MessageType returns the message's type name.
func (*CommandMsg) MessageType() string { return "chanfsm.Command" }

// MessageType returns the message's type name.
func (*PeerMsg) MessageType() string { return "chanfsm.Peer" }

// MessageType returns the message's type name.
func (*stateMsg) MessageType() string { return "chanfsm.State" }

// MessageType returns the message's type name.
func (*connectMsg) MessageType() string { return "chanfsm.Connect" }

func (*CommandMsg) chanMsg() {}
func (*PeerMsg) chanMsg()    {}
func (*stateMsg) chanMsg()   {}
func (*connectMsg) chanMsg() {}

// Response is a channel actor's reply.
type Response struct {
	// Value is a command's result: the HTLC ID for AddHTLC, and whether a
	// commitment was signed for SignCommitment.
	Value any

	// State is the current state, for a state query.
	State ChanState
}

// PeerSender sends messages to the channel's peer.
type PeerSender interface {
	// SendMessage sends messages to the peer, in order.
	SendMessage(sync bool, msgs ...lnwire.Message) error
}

// Config configures a channel actor.
type Config struct {
	// Channel is the channel the actor drives. The actor must be its only
	// user for as long as the actor runs.
	Channel Channel

	// Peer sends messages to the peer.
	Peer PeerSender

	// OnForward receives each forwarding package a revocation produces.
	OnForward func(*channeldb.FwdPkg)

	// OnContractUpdate receives each change to a commitment's HTLC set.
	OnContractUpdate func(*ContractUpdate)

	// OnFinalHtlcs receives the incoming HTLCs whose resolution a new
	// local commitment locked in.
	OnFinalHtlcs func(map[uint64]bool)

	// OnFailure is called once, when the channel fails.
	OnFailure func(error)

	// CheckLedger makes the state machine check its ledger against the
	// channel after every operation, and fail the channel on any
	// difference.
	CheckLedger bool

	// Observe, if set, is called for every transition.
	Observe protofsm.TransitionObserver[Event, Outbox, *Env]

	// MailboxSize is the mailbox capacity. Zero means
	// DefaultMailboxSize.
	MailboxSize int
}

// ChannelActor runs a channel's commitment protocol. It owns the channel's
// state machine, and is the only goroutine that touches the channel.
type ChannelActor struct {
	actor *actor.Actor[Msg, Response]
}

// NewChannelActor returns a channel actor for a channel in its current
// state. Call Start to run it.
func NewChannelActor(cfg Config) (*ChannelActor, error) {
	b, err := newBehavior(cfg)
	if err != nil {
		return nil, err
	}

	size := cfg.MailboxSize
	if size == 0 {
		size = DefaultMailboxSize
	}

	a, err := actor.NewActor(actor.ActorConfig[Msg, Response]{
		ID:          b.env.Name(),
		Behavior:    b,
		MailboxSize: size,
	})
	if err != nil {
		return nil, err
	}

	return &ChannelActor{actor: a}, nil
}

// Start starts the actor, and sends the peer our channel_reestablish before
// the actor handles anything else: every connection starts with one, and the
// channel was just loaded from disk.
//
// Nothing may send the actor a message before Start returns: a peer message
// or command that reached it first would find the channel still Connecting,
// which fails it.
func (c *ChannelActor) Start(ctx context.Context) error {
	c.actor.Start()

	_, err := c.actor.Ref().Ask(ctx, &connectMsg{}).Await(ctx).Unpack()

	return err
}

// Stop stops the actor.
func (c *ChannelActor) Stop() {
	c.actor.Stop()
}

// Ref returns the actor's reference.
func (c *ChannelActor) Ref() actor.ActorRef[Msg, Response] {
	return c.actor.Ref()
}

// command runs a command and waits for its result.
func (c *ChannelActor) command(ctx context.Context, cmd Event) (any, error) {
	resp, err := c.actor.Ref().Ask(ctx, &CommandMsg{Cmd: cmd}).
		Await(ctx).Unpack()
	if err != nil {
		return nil, err
	}

	return resp.Value, nil
}

// AddHTLC offers an HTLC to the peer and returns its ID.
func (c *ChannelActor) AddHTLC(ctx context.Context, cmd *AddHTLC) (uint64,
	error) {

	v, err := c.command(ctx, cmd)
	if err != nil {
		return 0, err
	}
	id, ok := v.(uint64)
	if !ok {
		return 0, fmt.Errorf("unexpected AddHTLC result %T", v)
	}

	return id, nil
}

// SettleHTLC settles one of the peer's HTLCs.
func (c *ChannelActor) SettleHTLC(ctx context.Context, cmd *SettleHTLC) error {
	_, err := c.command(ctx, cmd)
	return err
}

// FailHTLC fails one of the peer's HTLCs.
func (c *ChannelActor) FailHTLC(ctx context.Context, cmd *FailHTLC) error {
	_, err := c.command(ctx, cmd)
	return err
}

// MalformedFailHTLC fails one of the peer's HTLCs as malformed.
func (c *ChannelActor) MalformedFailHTLC(ctx context.Context,
	cmd *MalformedFailHTLC) error {

	_, err := c.command(ctx, cmd)
	return err
}

// UpdateFee proposes a new commitment fee rate.
func (c *ChannelActor) UpdateFee(ctx context.Context, cmd *UpdateFee) error {
	_, err := c.command(ctx, cmd)
	return err
}

// SignCommitment signs a new commitment for the peer if we owe one and may
// sign. It reports whether it signed.
func (c *ChannelActor) SignCommitment(ctx context.Context) (bool, error) {
	v, err := c.command(ctx, &SignCommitment{})
	if err != nil {
		return false, err
	}
	signed, _ := v.(bool)

	return signed, nil
}

// ReceiveMessage hands the actor a message from the peer and waits until it
// has been handled. A message that violates the protocol fails the channel,
// which is reported through OnFailure, not here.
func (c *ChannelActor) ReceiveMessage(ctx context.Context,
	msg lnwire.Message) error {

	_, err := c.actor.Ref().Ask(ctx, &PeerMsg{Msg: msg}).Await(ctx).
		Unpack()

	return err
}

// CurrentState returns the state machine's current state.
func (c *ChannelActor) CurrentState(ctx context.Context) (ChanState, error) {
	resp, err := c.actor.Ref().Ask(ctx, &stateMsg{}).Await(ctx).Unpack()
	if err != nil {
		return nil, err
	}

	return resp.State, nil
}

// behavior is the channel actor's message handler.
type behavior struct {
	cfg   Config
	env   *Env
	state ChanState
}

// newBehavior returns the behavior for a channel in its current state.
func newBehavior(cfg Config) (*behavior, error) {
	state, err := NewState(cfg.Channel)
	if err != nil {
		return nil, err
	}

	return &behavior{
		cfg:   cfg,
		env:   &Env{ChanID: cfg.Channel.ChannelID()},
		state: state,
	}, nil
}

// peerEvent converts a peer message into its event.
func peerEvent(msg lnwire.Message) (Event, error) {
	switch m := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		return &PeerAdd{Msg: m}, nil
	case *lnwire.UpdateFulfillHTLC:
		return &PeerFulfill{Msg: m}, nil
	case *lnwire.UpdateFailHTLC:
		return &PeerFail{Msg: m}, nil
	case *lnwire.UpdateFailMalformedHTLC:
		return &PeerFailMalformed{Msg: m}, nil
	case *lnwire.UpdateFee:
		return &PeerUpdateFee{Msg: m}, nil
	case *lnwire.CommitSig:
		return &PeerCommitSig{Msg: m}, nil
	case *lnwire.RevokeAndAck:
		return &PeerRevokeAndAck{Msg: m}, nil
	case *lnwire.ChannelReestablish:
		return &PeerReestablish{Msg: m}, nil
	default:
		return nil, fmt.Errorf("unexpected peer message %T", msg)
	}
}

// Receive handles one message.
//
// NOTE: This implements the actor.ActorBehavior interface.
func (b *behavior) Receive(ctx context.Context, msg Msg) fn.Result[Response] {
	var event Event
	switch m := msg.(type) {
	case *stateMsg:
		return fn.Ok(Response{State: b.state})

	case *connectMsg:
		event = &Connect{}

	case *CommandMsg:
		if !isCommand(m.Cmd) {
			return fn.Err[Response](fmt.Errorf("%T is not a "+
				"command", m.Cmd))
		}
		event = m.Cmd

	case *PeerMsg:
		ev, err := peerEvent(m.Msg)
		if err != nil {
			return fn.Err[Response](err)
		}
		event = ev

	default:
		return fn.Err[Response](fmt.Errorf("unknown message %T", msg))
	}

	effects := b.handle(ctx, event)

	var reply *Reply
	for _, out := range effects {
		if r, ok := out.(*Reply); ok {
			reply = r
			continue
		}
		b.dispatch(out)
	}

	if !isCommand(event) {
		return fn.Ok(Response{})
	}
	if reply == nil {
		return fn.Err[Response](fmt.Errorf("no reply to %T", event))
	}
	if reply.Err != nil {
		return fn.Err[Response](reply.Err)
	}

	return fn.Ok(Response{Value: reply.Value})
}

// handle applies an event and every operation it authorizes, and returns the
// side effects to carry out, in order. It runs each operation as soon as a
// transition authorizes it, and feeds its outcome straight back, so the
// state machine never rests in Applying and no other message can reach the
// channel between an authorization and its outcome.
func (b *behavior) handle(ctx context.Context, event Event) []Outbox {
	var effects []Outbox
	for event != nil {
		next, outbox, err := protofsm.ApplyEventsObserved(
			ctx, b.state, event, b.env, b.cfg.Observe,
		)
		if err != nil {
			// A transition only errors on an event its state
			// doesn't expect, which is a bug. Stop using the
			// channel.
			log.Errorf("ChannelActor(%v): %v", b.env.ChanID, err)
			next = &Failed{Err: err}
			outbox = []Outbox{&FailChannel{Err: err}}
			if isCommand(event) {
				outbox = append(outbox, &Reply{Err: err})
			}
		}

		log.Tracef("ChannelActor(%v): %v --%T--> %v", b.env.ChanID,
			b.state, event, next)
		b.state = next

		event = nil
		for _, out := range outbox {
			apply, ok := out.(*ApplyOp)
			if !ok {
				effects = append(effects, out)
				continue
			}

			event = b.run(ctx, apply.Op)
		}
	}

	if _, ok := b.state.(*Applying); ok {
		err := errors.New("state machine rests in Applying")
		log.Errorf("ChannelActor(%v): %v", b.env.ChanID, err)
		b.state = &Failed{Err: err}
		effects = append(effects, &FailChannel{Err: err})
	}

	return effects
}

// run applies an operation to the channel and returns its outcome.
func (b *behavior) run(ctx context.Context, op Op) *OpDone {
	value, err := op.run(ctx, b.cfg.Channel)
	done := &OpDone{Op: op, Value: value, Err: err}
	if err == nil && b.cfg.CheckLedger {
		done.Snapshot = fn.Some(b.cfg.Channel.ProtocolSnapshot())
	}

	return done
}

// dispatch carries out one side effect.
func (b *behavior) dispatch(out Outbox) {
	switch o := out.(type) {
	case *SendToPeer:
		// A failed send means the connection is going away, which
		// tears this actor down, so there's nothing more to do.
		if err := b.cfg.Peer.SendMessage(false, o.Msgs...); err != nil {
			log.Debugf("ChannelActor(%v): unable to send: %v",
				b.env.ChanID, err)
		}

	case *ForwardPackage:
		if b.cfg.OnForward != nil {
			b.cfg.OnForward(o.Pkg)
		}

	case *ContractUpdate:
		if b.cfg.OnContractUpdate != nil {
			b.cfg.OnContractUpdate(o)
		}

	case *FinalHtlcs:
		if b.cfg.OnFinalHtlcs != nil {
			b.cfg.OnFinalHtlcs(o.Resolved)
		}

	case *FailChannel:
		log.Warnf("ChannelActor(%v): channel failed: %v",
			b.env.ChanID, o.Err)
		if b.cfg.OnFailure != nil {
			b.cfg.OnFailure(o.Err)
		}

	default:
		log.Errorf("ChannelActor(%v): unknown outbox event %T",
			b.env.ChanID, out)
	}
}

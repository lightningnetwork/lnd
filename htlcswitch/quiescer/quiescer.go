package quiescer

import (
	"errors"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
)

type Env struct {
	key     secp256k1.PublicKey
	cid     lnwire.ChannelID
	canSend func() bool
}

func (*Env) CleanUp() error { return nil }

type Live struct{}

func (*Live) IsTerminal() bool { return false }
func (*Live) ProcessEvent(event Events, env *Env) (
	*protofsm.StateTransition[Events, *Env], error,
) {

	switch event.(type) {
	case *IncomingStfu:
		stfu := lnwire.Stfu{
			ChanID:    env.cid,
			Initiator: false,
		}
		send := protofsm.SendMsgEvent[Events]{
			Msgs:       []lnwire.Message{&stfu},
			TargetPeer: env.key,
			SendWhen:   fn.Some(env.canSend),
			PostSendEvent: fn.Some(
				Events(&gotoQuiescent{}), // gross
			),
		}

		return &protofsm.StateTransition[Events, *Env]{
			NextState: &Live{},
			NewEvents: fn.Some(
				protofsm.EmittedEvent[Events]{
					ExternalEvents: fn.Some(
						protofsm.DaemonEventSet{&send},
					),
				},
			),
		}, nil
	case *Initiate:
		stfu := lnwire.Stfu{
			ChanID:    env.cid,
			Initiator: true,
		}
		send := protofsm.SendMsgEvent[Events]{
			Msgs:       []lnwire.Message{&stfu},
			TargetPeer: env.key,
			SendWhen:   fn.Some(env.canSend),
			PostSendEvent: fn.Some(
				Events(&gotoAwaitingStfu{}), // gross
			),
		}

		return &protofsm.StateTransition[Events, *Env]{
			NextState: &Live{},
			NewEvents: fn.Some(
				protofsm.EmittedEvent[Events]{
					ExternalEvents: fn.Some(
						protofsm.DaemonEventSet{&send},
					),
				},
			),
		}, nil
	case *gotoAwaitingStfu:
		return &protofsm.StateTransition[Events, *Env]{
			NextState: &AwaitingStfu{},
			NewEvents: fn.None[protofsm.EmittedEvent[Events]](),
		}, nil
	case *gotoQuiescent:
		return &protofsm.StateTransition[Events, *Env]{
			NextState: &Quiescent{},
			NewEvents: fn.None[protofsm.EmittedEvent[Events]](),
		}, nil
	default:
		panic("impossible: invalid QuiescerEvent")
	}
}

type AwaitingStfu struct{}

func (*AwaitingStfu) IsTerminal() bool { return false }
func (*AwaitingStfu) ProcessEvent(event Events, _ *Env) (
	*protofsm.StateTransition[Events, *Env], error,
) {

	switch event.(type) {
	case *IncomingStfu:
		return &protofsm.StateTransition[Events, *Env]{
			NextState: &Quiescent{},
		}, nil
	case *Initiate:
		return nil, errors.New("already negotiating quiescence")
	case *gotoAwaitingStfu:
		panic("impossible: invalid Quiescer StateTransition")
	case *gotoQuiescent:
		panic("impossible: invalid Quiescer StateTransition")
	default:
		panic("impossible: invalid QuiescerEvent")
	}
}

type Quiescent struct{}

func (*Quiescent) IsTerminal() bool { return true }
func (*Quiescent) ProcessEvent(_ Events, _ *Env) (
	*protofsm.StateTransition[Events, *Env], error,
) {

	return &protofsm.StateTransition[Events, *Env]{
		NextState: &Quiescent{},
	}, nil
}

type Events interface {
	sealed()
}

type IncomingStfu struct{}

func (*IncomingStfu) sealed() {}

type Initiate struct{}

func (*Initiate) sealed() {}

type gotoAwaitingStfu struct{}

func (*gotoAwaitingStfu) sealed() {}

type gotoQuiescent struct{}

func (*gotoQuiescent) sealed() {}

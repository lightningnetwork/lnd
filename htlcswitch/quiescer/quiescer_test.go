package quiescer

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type QuiescenceAdapters struct {
	mock.Mock
}

func (a *QuiescenceAdapters) SendMessages(
	pub btcec.PublicKey, msgs []lnwire.Message,
) error {

	args := a.Called(pub, msgs)

	return args.Error(0)
}

func (a *QuiescenceAdapters) BroadcastTransaction(
	tx *wire.MsgTx, label string,
) error {

	args := a.Called(tx, label)

	return args.Error(0)
}

func (a *QuiescenceAdapters) DisableChannel(op wire.OutPoint) error {
	args := a.Called(op)

	return args.Error(0)
}

var (
	hexDecode = func(keyStr string) []byte {
		keyBytes, _ := hex.DecodeString(keyStr)
		return keyBytes
	}
	peer, _ = btcec.ParsePubKey(hexDecode(
		"02ec95e4e8ad994861b95fc5986eedaac24739e5ea3d0634db4c8ccd44cd" +
			"a126ea",
	))
)

func TestQuiescerDaemonEvents(t *testing.T) {
	t.Parallel()

	env := &Env{
		key:     *peer,
		canSend: func() bool { return true },
	}

	adapters := &QuiescenceAdapters{}

	stateMachine := protofsm.NewStateMachine[Events, *Env](
		adapters, &Live{}, env,
	)
	stateMachine.Start()
	defer stateMachine.Stop()

	stateSub := stateMachine.RegisterStateEvents()
	defer stateMachine.RemoveStateSub(stateSub)

	adapters.On("SendMessages", *peer, mock.Anything).Return(nil)

	stateMachine.SendEvent(&Initiate{})

	phase1 := []protofsm.State[Events, *Env]{
		&Live{}, &Live{}, &AwaitingStfu{},
	}
	assertStateTransitions(t, stateSub, phase1)

	assertState(
		t, &stateMachine,
		protofsm.State[Events, *Env](&AwaitingStfu{}),
	)

	adapters.AssertExpectations(t)

	stateMachine.SendEvent(&IncomingStfu{})

	// time.Sleep(time.Second)
	assertState(
		t, &stateMachine,
		protofsm.State[Events, *Env](&Quiescent{}),
	)

	phase2 := []protofsm.State[Events, *Env]{
		&Quiescent{},
	}
	assertStateTransitions(t, stateSub, phase2)
}

func assertStateTransitions[Event any, Env protofsm.Environment](
	t *testing.T, stateSub protofsm.StateSubscriber[Event, Env],
	expectedStates []protofsm.State[Event, Env],
) {

	for _, expectedState := range expectedStates {
		newState := <-stateSub.NewItemCreated.ChanOut()

		require.IsType(t, expectedState, newState)
	}
}

func assertState[Event any, Env protofsm.Environment](
	t *testing.T, m *protofsm.StateMachine[Event, Env],
	expectedState protofsm.State[Event, Env],
) {

	state, err := m.CurrentState()
	require.NoError(t, err)
	require.IsType(t, expectedState, state)
}

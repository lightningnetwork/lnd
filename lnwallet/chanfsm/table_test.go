package chanfsm

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// tableObserver returns a transition observer that fails the test on any
// transition ChannelTransitions doesn't list. It may run on the actor's
// goroutine, so it reports with Errorf.
func tableObserver(t *testing.T) func(Event, ChanState, ChanState,
	[]Outbox) {

	return func(ev Event, from, to ChanState, out []Outbox) {
		err := ChannelTransitions.CheckTransition(from, ev, to, out)
		if err != nil {
			t.Errorf("%v", err)
		}
	}
}

// TestTransitionTableRenders checks the table renders, which also exercises
// every entry's types.
func TestTransitionTableRenders(t *testing.T) {
	t.Parallel()

	md := ChannelTransitions.RenderMarkdown()
	if len(md) == 0 {
		t.Fatal("empty table")
	}
}

// TestReestablishGate checks the events around channel_reestablish that the
// randomized tests never produce: a command before the peer's
// channel_reestablish is refused and changes nothing, a peer message before
// it fails the channel, and so does a second channel_reestablish once the
// channel runs.
func TestReestablishGate(t *testing.T) {
	t.Parallel()

	alice, _ := newPair()
	reest := &Reestablishing{Restored: alice.Restore()}
	peerReest := &PeerReestablish{Msg: &lnwire.ChannelReestablish{
		NextLocalCommitHeight: 1,
	}}

	tests := []struct {
		name  string
		from  ChanState
		event Event
		err   error
		fails bool
	}{{
		name:  "command before reestablish",
		from:  reest,
		event: &SignCommitment{},
		err:   ErrNotReestablished,
	}, {
		name:  "update before reestablish",
		from:  reest,
		event: &PeerAdd{Msg: &lnwire.UpdateAddHTLC{}},
		err:   ErrReestablishFirst,
		fails: true,
	}, {
		name:  "reestablish while synced",
		from:  &Synced{Ledger: alice},
		event: peerReest,
		err:   ErrUnexpectedReestablish,
		fails: true,
	}, {
		name: "reestablish while awaiting revocation",
		from: &AwaitingRevocation{Ledger: func() Ledger {
			l, err := alice.SignCommitment()
			require.NoError(t, err)

			return l
		}()},
		event: peerReest,
		err:   ErrUnexpectedReestablish,
		fails: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := tc.from.ProcessEvent(
				t.Context(), tc.event, nil,
			)
			require.NoError(t, err)

			out := tr.NewEvents.UnwrapOr(chanEmitted{}).Outbox
			require.NoError(t, ChannelTransitions.CheckTransition(
				tc.from, tc.event, tr.NextState, out,
			))
			require.Len(t, out, 1)

			if !tc.fails {
				require.Same(t, tc.from, tr.NextState)
				reply, ok := out[0].(*Reply)
				require.True(t, ok)
				require.ErrorIs(t, reply.Err, tc.err)

				return
			}

			require.IsType(t, &Failed{}, tr.NextState)
			failure, ok := out[0].(*FailChannel)
			require.True(t, ok)
			require.ErrorIs(t, failure.Err, tc.err)
		})
	}
}

// ledgerOf returns the ledger a resting or applying state holds.
func ledgerOf(s ChanState) (Ledger, bool) {
	switch s := s.(type) {
	case *Synced:
		return s.Ledger, true
	case *AwaitingRevocation:
		return s.Ledger, true
	case *Applying:
		return s.Ledger, true
	}

	return Ledger{}, false
}

// TestFailedRefusesEverything checks that a failed channel answers every
// command with ErrChannelFailed, wrapping the failure, and drops every peer
// message without leaving the state or touching the channel.
func TestFailedRefusesEverything(t *testing.T) {
	t.Parallel()

	cause := errors.New("peer sent a premature revocation")
	failed := &Failed{Err: cause}

	commands := []Event{
		&AddHTLC{Htlc: &lnwire.UpdateAddHTLC{}},
		&SettleHTLC{Msg: &lnwire.UpdateFulfillHTLC{}},
		&UpdateFee{},
		&SignCommitment{},
	}
	for _, cmd := range commands {
		tr, err := failed.ProcessEvent(t.Context(), cmd, nil)
		require.NoError(t, err)
		require.Same(t, failed, tr.NextState)

		out := tr.NewEvents.UnwrapOr(chanEmitted{}).Outbox
		require.Len(t, out, 1)
		reply, ok := out[0].(*Reply)
		require.True(t, ok, "%T answered with %T", cmd, out[0])
		require.ErrorIs(t, reply.Err, ErrChannelFailed)
		require.ErrorIs(t, reply.Err, cause)
	}

	messages := []Event{
		&PeerAdd{Msg: &lnwire.UpdateAddHTLC{}},
		&PeerRevokeAndAck{Msg: &lnwire.RevokeAndAck{}},
		&PeerReestablish{Msg: &lnwire.ChannelReestablish{}},
	}
	for _, msg := range messages {
		tr, err := failed.ProcessEvent(t.Context(), msg, nil)
		require.NoError(t, err)
		require.Same(t, failed, tr.NextState)
		require.True(t, tr.NewEvents.IsNone(), "%T emitted events",
			msg)
	}

	// Events only the actor makes never reach a failed channel.
	for _, ev := range []Event{&OpDone{}, &Connect{}} {
		_, err := failed.ProcessEvent(t.Context(), ev, nil)
		require.Error(t, err)
	}
}

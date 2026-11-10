package htlcswitch

import (
	"crypto/sha256"
	"sync"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// commitThenQuitCircuitMap closes the incoming link immediately after a
// circuit has been committed, before ForwardPackets can hand the add to the
// switch event loop.
type commitThenQuitCircuitMap struct {
	CircuitMap

	quit chan struct{}
	once sync.Once
}

func (c *commitThenQuitCircuitMap) CommitCircuits(
	circuits ...*PaymentCircuit) (*CircuitFwdActions, error) {

	actions, err := c.CircuitMap.CommitCircuits(circuits...)
	if err == nil {
		c.once.Do(func() {
			close(c.quit)
		})
	}

	return actions, err
}

// TestForwardPacketsLinkQuitAfterCircuitCommit asserts that a link shutdown
// after circuit persistence but before switch handoff does not leave a
// half-open circuit that suppresses the add on reconnect.
func TestForwardPacketsLinkQuitAfterCircuitCommit(t *testing.T) {
	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = s.Stop()
	})

	linkQuit := make(chan struct{})
	s.circuits = &commitThenQuitCircuitMap{
		CircuitMap: s.circuits,
		quit:       linkQuit,
	}

	preimage := [sha256.Size]byte{1}
	packet := &htlcPacket{
		incomingChanID: lnwire.NewShortChanIDFromInt(11),
		incomingHTLCID: 7,
		outgoingChanID: lnwire.NewShortChanIDFromInt(22),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: sha256.Sum256(preimage[:]),
			Amount:      1000,
		},
	}

	err = s.ForwardPackets(linkQuit, packet)
	require.ErrorIs(t, err, ErrLinkShuttingDown)
	require.Equal(t, 0, s.circuits.NumPending())
	require.Equal(t, 0, s.circuits.NumOpen())

	// Simulate the incoming forwarding package replaying the add. With the
	// abandoned half circuit removed, it is accepted as a fresh circuit
	// instead of being silently dropped as an in-memory duplicate.
	replay := *packet
	addMsg, ok := packet.htlc.(*lnwire.UpdateAddHTLC)
	require.True(t, ok)
	add := *addMsg
	replay.htlc = &add
	replayCircuit := newPaymentCircuit(&add.PaymentHash, &replay)

	actions, err := s.circuits.CommitCircuits(replayCircuit)
	require.NoError(t, err)
	require.Len(t, actions.Adds, 1)
	require.Empty(t, actions.Drops)
	require.Empty(t, actions.Fails)
}

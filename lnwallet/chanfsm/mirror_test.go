package chanfsm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// This file checks that the ledger is an exact abstraction of lnd's
// commitment protocol state: two legacy nodes run random honest protocol
// executions, each with a shadow ledger that applies the same operations,
// and after every step each shadow must equal its channel's
// ProtocolSnapshot, and every forwarding package must hold exactly the
// updates the shadow says became locked in.

// shadowNode is a legacy node paired with the ledger that should describe
// it.
type shadowNode struct {
	*legacyNode

	ledger Ledger
}

// newShadowPair returns two connected legacy nodes with their ledgers.
// Alice is the channel initiator.
func newShadowPair(t *testing.T,
	chanType channeldb.ChannelType) (*shadowNode, *shadowNode) {

	alice, bob, err := lnwallet.CreateTestChannels(t, chanType)
	require.NoError(t, err)

	mk := func(name, peer string, lc *lnwallet.LightningChannel,
		initiator lntypes.ChannelParty) *shadowNode {

		ledger, err := LedgerFromSnapshot(
			lc.ProtocolSnapshot(), initiator,
		)
		require.NoError(t, err)

		return &shadowNode{
			legacyNode: &legacyNode{name: name, peer: peer, lc: lc},
			ledger:     ledger,
		}
	}

	return mk("alice", "bob", alice, lntypes.Local),
		mk("bob", "alice", bob, lntypes.Remote)
}

// check asserts the shadow ledger equals the channel's state.
func (s *shadowNode) check(t require.TestingT) {
	require.NoError(t, s.ledger.MatchesSnapshot(s.lc.ProtocolSnapshot()),
		"%s", s.name)
	require.Equal(t, s.ledger.OweCommitment(lntypes.Local),
		s.lc.OweCommitment(), "%s owe commitment", s.name)
}

// sign applies the legacy node's signing policy to the shadow.
func (s *shadowNode) sign(t require.TestingT) {
	if !s.ledger.OweCommitment(lntypes.Local) {
		return
	}

	next, err := s.ledger.SignCommitment()
	if errors.Is(err, ErrNoRevocationWindow) {
		return
	}
	require.NoError(t, err)
	s.ledger = next
}

// receive delivers a message to the node and its shadow.
func (s *shadowNode) receive(t require.TestingT, msg lnwire.Message) {
	var (
		next = s.ledger
		err  error
		fwds []ForwardRef
	)

	switch m := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		next, err = next.AddHtlc(lntypes.Remote, m.ID)

	case *lnwire.UpdateFulfillHTLC:
		next, err = next.RemoveHtlc(lntypes.Remote, KindSettle, m.ID)

	case *lnwire.UpdateFailHTLC:
		next, err = next.RemoveHtlc(lntypes.Remote, KindFail, m.ID)

	case *lnwire.UpdateFailMalformedHTLC:
		next, err = next.RemoveHtlc(lntypes.Remote, KindFail, m.ID)

	case *lnwire.UpdateFee:
		next, err = next.UpdateFee(lntypes.Remote)

	case *lnwire.CommitSig:
		next, err = next.ReceiveCommitment()
		require.NoError(t, err)
		next, err = next.RevokeCommitment()

	case *lnwire.RevokeAndAck:
		next, fwds, err = next.ReceiveRevocation()
	}
	require.NoError(t, err, "%s: shadow rejected %T", s.name, msg)

	nFwds := len(s.fwds)
	require.NoError(t, s.legacyNode.receive(msg), "%s: channel "+
		"rejected %T", s.name, msg)

	s.ledger = next
	switch msg.(type) {
	case *lnwire.CommitSig:
		s.sign(t)

	case *lnwire.RevokeAndAck:
		require.Len(t, s.fwds, nFwds+1)
		require.True(t, sameForwards(fwds, forwardsOf(s.fwds[nFwds])),
			"%s: forwarding package", s.name)
		s.sign(t)
	}
}

// removable returns the IDs of the peer's HTLCs that we may remove: locked
// in and not yet removed.
func removable(l Ledger) []uint64 {
	var ids []uint64
	for _, e := range l.Logs.Remote.Entries {
		if e.Kind != KindAdd || l.Logs.Remote.modified(e.HtlcIndex) {
			continue
		}
		if l.LockedIn(e) {
			ids = append(ids, e.HtlcIndex)
		}
	}

	return ids
}

// localCommand runs one of our own commands on the node and its shadow. It
// returns false if the channel refused it for a reason the ledger doesn't
// model, such as the balance, in which case neither changes.
func (s *shadowNode) localCommand(t *rapid.T, cmd string) bool {
	var (
		next Ledger
		err  error
	)

	switch cmd {
	case "add":
		id := s.nextHtlcID()
		if _, err := s.addHTLC(); err != nil {
			return false
		}
		next, err = s.ledger.AddHtlc(lntypes.Local, id)

	case "settle", "fail", "malform":
		ids := removable(s.ledger)
		if len(ids) == 0 {
			return false
		}
		id := rapid.SampledFrom(ids).Draw(t, "id")

		kind := map[string]Kind{
			"settle": KindSettle, "fail": KindFail,
			"malform": KindMalformed,
		}[cmd]
		run := map[string]func(uint64) error{
			"settle": s.settleHTLC, "fail": s.failHTLC,
			"malform": s.malformHTLC,
		}[cmd]
		require.NoError(t, run(id))
		next, err = s.ledger.RemoveHtlc(lntypes.Local, kind, id)

	case "fee":
		rate := s.lc.CommitFeeRate() + chainfee.SatPerKWeight(
			rapid.IntRange(-50, 50).Draw(t, "delta"),
		)
		if err := s.updateFee(rate); err != nil {
			return false
		}
		next, err = s.ledger.UpdateFee(lntypes.Local)

	case "sign":
		require.NoError(t, s.signCommitment())
		s.sign(t)

		return true

	default:
		panic(cmd)
	}
	require.NoError(t, err, "%s: shadow rejected %s", s.name, cmd)
	s.ledger = next

	return true
}

// reconnect disconnects the two nodes, losing every message in flight,
// reloads both channels from disk, and runs channel_reestablish, checking
// the restored ledgers and the retransmissions against the ledger.
func reconnect(t *rapid.T, nodes []*shadowNode) {
	restored := make([]RestoredLedger, len(nodes))
	for _, n := range nodes {
		for _, e := range n.ledger.Logs.Remote.Entries {
			if e.Kind == KindAdd && n.ledger.LockedIn(e) {
				planStats.hit("locked-in at restart")
			}
		}
	}
	for i, n := range nodes {
		require.NoError(t, n.restart())
		n.sent = nil
		restored[i] = n.ledger.Restore()
		require.NoError(t,
			restored[i].MatchesSnapshot(n.lc.ProtocolSnapshot()),
			"%s: restored ledger from %+v", n.name, n.ledger)
	}

	syncs := make([]*lnwire.ChannelReestablish, len(nodes))
	for i, n := range nodes {
		msg, err := n.chanSync()
		require.NoError(t, err)
		syncs[i] = msg
	}

	for i, n := range nodes {
		peer := syncs[1-i]
		next, plan, err := restored[i].Reestablish(Reestablish{
			NextLocalHeight:  peer.NextLocalCommitHeight,
			RemoteTailHeight: peer.RemoteCommitTailHeight,
		})
		require.NoError(t, err, "%s: ledger refused reestablish",
			n.name)

		require.NoError(t, n.reestablish(peer), "%s: channel "+
			"refused reestablish", n.name)
		require.Equal(t, planMessages(next, plan),
			syncMessages(n.sent), "%s: retransmission", n.name)

		n.ledger = next
		n.check(t)
		planStats.hit(fmt.Sprintf("%T", plan))
	}
}

// planStats counts the plans reconnections produced, so the test can insist
// that every kind was exercised.
var planStats diffStats

// TestLedgerMirrorsChannel runs random honest executions between two legacy
// nodes and checks each node's shadow ledger against its channel after every
// step.
func TestLedgerMirrorsChannel(t *testing.T) {
	t.Parallel()

	chanTypes := map[string]channeldb.ChannelType{
		"tweakless": channeldb.SingleFunderTweaklessBit,
		"anchors": channeldb.SingleFunderTweaklessBit |
			channeldb.AnchorOutputsBit,
		"taproot": channeldb.SingleFunderTweaklessBit |
			channeldb.AnchorOutputsBit |
			channeldb.SimpleTaprootFeatureBit,
	}

	t.Cleanup(func() {
		t.Logf("plans: %v", planStats.counts)
		if testing.Short() {
			return
		}
		// ResendRevocation needs our revoke_and_ack lost with no
		// commitment of ours outstanding, which the link's habit of
		// signing right after it revokes makes rare: the scripted
		// TestReestablishResendRevocation covers it.
		for _, plan := range []SyncPlan{
			InSync{}, ResendCommitment{}, ResendBoth{},
		} {
			require.Positive(t,
				planStats.counts[fmt.Sprintf("%T", plan)],
				"never exercised %T", plan)
		}
		require.Positive(t, planStats.counts["locked-in at restart"],
			"no restart found an HTLC locked in")
	})
	for name, chanType := range chanTypes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rapid.Check(t, func(rt *rapid.T) {
				runMirror(t, rt, chanType)
			})
		})
	}
}

// runMirror runs one random execution.
func runMirror(t *testing.T, rt *rapid.T, chanType channeldb.ChannelType) {
	alice, bob := newShadowPair(t, chanType)
	nodes := []*shadowNode{alice, bob}
	inbox := [2][]lnwire.Message{}
	reconnects := 0

	collect := func(i int) {
		inbox[1-i] = append(inbox[1-i], nodes[i].sent...)
		nodes[i].sent = nil
	}

	steps := rapid.IntRange(1, 60).Draw(rt, "steps")
	for step := 0; step < steps; step++ {
		i := rapid.IntRange(0, 1).Draw(rt, "node")
		n := nodes[i]

		cmds := []string{"add", "settle", "fail", "malform", "sign",
			"deliver", "deliver", "deliver", "reconnect", "flush",
			"flush"}
		if n.ledger.Initiator.IsLocal() {
			cmds = append(cmds, "fee")
		}
		cmd := rapid.SampledFrom(cmds).Draw(rt, "cmd")

		// A flush lets the protocol run until quiet, so that HTLCs
		// lock in often enough for reconnections to find them.
		if cmd == "flush" {
			for round := 0; round < 3; round++ {
				for j, m := range nodes {
					m.localCommand(rt, "sign")
					collect(j)
				}
				for len(inbox[0])+len(inbox[1]) > 0 {
					for j, m := range nodes {
						if len(inbox[j]) == 0 {
							continue
						}
						msg := inbox[j][0]
						inbox[j] = inbox[j][1:]
						m.receive(rt, msg)
						collect(j)
					}
				}
			}
			for _, m := range nodes {
				m.check(rt)
			}

			continue
		}

		if cmd == "reconnect" {
			if reconnects >= 3 {
				continue
			}
			reconnects++
			inbox = [2][]lnwire.Message{}
			reconnect(rt, nodes)
			collect(0)
			collect(1)

			continue
		}

		if cmd == "deliver" {
			if len(inbox[i]) == 0 {
				continue
			}
			msg := inbox[i][0]
			inbox[i] = inbox[i][1:]
			n.receive(rt, msg)
		} else {
			n.localCommand(rt, cmd)
		}
		collect(i)

		for _, n := range nodes {
			n.check(rt)
		}
	}

	// Drain: keep signing and delivering until both sides are quiet, then
	// both commitments must include every update.
	for round := 0; round < 20; round++ {
		for i, n := range nodes {
			n.localCommand(rt, "sign")
			collect(i)
		}
		for i, n := range nodes {
			for len(inbox[i]) > 0 {
				msg := inbox[i][0]
				inbox[i] = inbox[i][1:]
				n.receive(rt, msg)
				collect(i)
			}
		}
		for _, n := range nodes {
			n.check(rt)
		}
	}

	for _, n := range nodes {
		require.False(rt, n.ledger.OweCommitment(lntypes.Local),
			fmt.Sprintf("%s still owes a commitment", n.name))
	}
}

// TestReestablishResendRevocation scripts the two ways a reconnection finds
// our last revoke_and_ack lost with no commitment of ours outstanding, which
// random runs rarely reach, and checks the ledger's plan against the
// channel's retransmission for each.
func TestReestablishResendRevocation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	cases := []struct {
		name string
		run  func(t *testing.T, alice, bob *shadowNode)
		want SyncPlan
	}{{
		// Bob adds an HTLC and signs. Alice revokes, but hasn't signed
		// yet, so she owes Bob a commitment covering his add, and
		// signs one after resending the revocation.
		name: "sign new",
		run: func(t *testing.T, alice, bob *shadowNode) {
			_, err := bob.addHTLC()
			require.NoError(t, err)
			alice.receiveNoSign(t, bob.sent[0])
			newCommit, err := bob.lc.SignNextCommitment(ctx)
			require.NoError(t, err)
			alice.receiveNoSign(t, commitMsg(bob, newCommit))
		},
		want: ResendRevocation{SignNew: true},
	}, {
		// Alice adds an HTLC and signs, Bob revokes and signs back,
		// and Alice revokes: she owes nothing.
		name: "nothing owed",
		run: func(t *testing.T, alice, bob *shadowNode) {
			_, err := alice.addHTLC()
			require.NoError(t, err)
			require.NoError(t, alice.signCommitment())
			for _, m := range alice.sent {
				require.NoError(t, bob.legacyNode.receive(m))
			}
			for _, m := range bob.sent {
				alice.receiveNoSign(t, m)
			}
		},
		want: ResendRevocation{},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := newShadowPair(
				t, channeldb.SingleFunderTweaklessBit,
			)
			tc.run(t, alice, bob)

			for _, n := range []*shadowNode{alice, bob} {
				require.NoError(t, n.restart())
				n.sent = nil
			}
			restored, err := LedgerFromSnapshot(
				alice.lc.ProtocolSnapshot(), lntypes.Local,
			)
			require.NoError(t, err)
			bobSync, err := bob.chanSync()
			require.NoError(t, err)

			next, plan, err := restored.Restore().Reestablish(
				Reestablish{
					NextLocalHeight: bobSync.
						NextLocalCommitHeight,
					RemoteTailHeight: bobSync.
						RemoteCommitTailHeight,
				},
			)
			require.NoError(t, err)
			require.Equal(t, tc.want, plan)

			require.NoError(t, alice.reestablish(bobSync))
			require.Equal(t, planMessages(next, plan),
				syncMessages(alice.sent))
		})
	}
}

// receiveNoSign has the node handle a message, revoking after a
// commitment_signed but, unlike the link, not signing back.
func (s *shadowNode) receiveNoSign(t *testing.T, msg lnwire.Message) {
	t.Helper()

	switch m := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		_, err := s.lc.ReceiveHTLC(m)
		require.NoError(t, err)

	case *lnwire.CommitSig:
		err := s.lc.ReceiveNewCommitment(&lnwallet.CommitSigs{
			CommitSig: m.CommitSig,
			HtlcSigs:  m.HtlcSigs,
		})
		require.NoError(t, err)
		_, _, _, err = s.lc.RevokeCurrentCommitment()
		require.NoError(t, err)

	case *lnwire.RevokeAndAck:
		_, _, err := s.lc.ReceiveRevocation(m)
		require.NoError(t, err)

	default:
		t.Fatalf("unexpected %T", msg)
	}
}

// commitMsg builds the commitment_signed for a node's new commitment.
func commitMsg(n *shadowNode,
	c *lnwallet.NewCommitState) *lnwire.CommitSig {

	return &lnwire.CommitSig{
		ChanID:    n.lc.ChannelID(),
		CommitSig: c.CommitSig,
		HtlcSigs:  c.HtlcSigs,
	}
}

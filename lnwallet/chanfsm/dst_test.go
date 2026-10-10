package chanfsm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	mrand "math/rand"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// This file is the deterministic simulation test of the channel actor. Both
// ends of a channel are channel actors over real LightningChannels and real
// databases, run inside a synctest bubble. A workload drawn by rapid drives
// them one step at a time: local commands, the delivery of the next message
// in either direction, disconnections, and crashes. The harness waits for
// the bubble to go idle after every step, so a scenario always runs the
// same way, and rapid can shrink a failing one.
//
// A crash happens at the Nth operation that writes to disk after it is
// armed: the write lands, but its result is lost, as if the node died right
// after committing. Both nodes then reload their channels from disk, as lnd
// does when it reconnects, and run channel_reestablish. A disconnection does
// the same without the crash. Either loses every message in flight, which is
// the only way a BOLT 8 connection loses messages.
//
// After the workload, the harness heals the channel and lets it settle, then
// checks the oracles: neither side failed the channel, both sides agree on
// both commitments, no commitment is owed, and, from the forwarding packages
// on disk, every HTLC was forwarded exactly once and every removal of an
// HTLC was forwarded back exactly once, across every crash.

// errSimulatedCrash is what an operation returns when the node crashed
// right after it wrote to disk.
var errSimulatedCrash = errors.New("simulated crash")

// crashChannel is a Channel that crashes after a number of writes.
type crashChannel struct {
	*lnwallet.LightningChannel

	mu        sync.Mutex
	countdown int
	crashed   bool

	// then is the countdown the channel restarted after this one's crash
	// starts with.
	then int

	// site names the operation the node crashed in.
	site string
}

// arm makes the channel crash at the nth write from now, and the channel
// it restarts to at the given write, if not zero.
func (c *crashChannel) arm(n, then int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.countdown = n
	c.then = then
}

// wrote counts a write the named operation just landed, and reports
// whether the node crashed with it.
func (c *crashChannel) wrote(site string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.crashed {
		return true
	}
	if c.countdown == 0 {
		return false
	}
	c.countdown--
	c.crashed = c.countdown == 0
	if c.crashed {
		c.site = site
	}

	return c.crashed
}

// isCrashed reports whether the node crashed.
func (c *crashChannel) isCrashed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.crashed
}

// crashSite names the operation the node crashed in.
func (c *crashChannel) crashSite() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.site
}

// SignNextCommitment writes the new remote commitment.
func (c *crashChannel) SignNextCommitment(
	ctx context.Context) (*lnwallet.NewCommitState, error) {

	res, err := c.LightningChannel.SignNextCommitment(ctx)
	if err == nil && c.wrote("sign") {
		return nil, errSimulatedCrash
	}

	return res, err
}

// RevokeCurrentCommitment writes our new commitment.
func (c *crashChannel) RevokeCurrentCommitment() (*lnwire.RevokeAndAck,
	[]channeldb.HTLC, map[uint64]bool, error) {

	msg, htlcs, final, err := c.LightningChannel.RevokeCurrentCommitment()
	if err == nil && c.wrote("revoke") {
		return nil, nil, nil, errSimulatedCrash
	}

	return msg, htlcs, final, err
}

// ReceiveRevocation writes the peer's new commitment and the forwarding
// package.
func (c *crashChannel) ReceiveRevocation(msg *lnwire.RevokeAndAck) (
	*channeldb.FwdPkg, []channeldb.HTLC, error) {

	pkg, htlcs, err := c.LightningChannel.ReceiveRevocation(msg)
	if err == nil && c.wrote("revocation") {
		return nil, nil, errSimulatedCrash
	}

	return pkg, htlcs, err
}

// ProcessChanSyncMsg writes a new remote commitment when it resends a
// revocation and then signs, the only case in which it writes at all: it
// sends a commitment_signed with no commitment of the peer's pending before.
func (c *crashChannel) ProcessChanSyncMsg(ctx context.Context,
	msg *lnwire.ChannelReestablish) ([]lnwire.Message, []models.CircuitKey,
	[]models.CircuitKey, error) {

	before, err := LedgerFromSnapshot(
		c.LightningChannel.ProtocolSnapshot(), lntypes.Local,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	msgs, opened, closed, err := c.LightningChannel.ProcessChanSyncMsg(
		ctx, msg,
	)
	signed := slices.ContainsFunc(msgs, func(m lnwire.Message) bool {
		_, ok := m.(*lnwire.CommitSig)
		return ok
	})
	if err == nil && signed && before.Chains.Remote.Pending.IsNone() &&
		c.wrote("reestablish") {

		return nil, nil, nil, errSimulatedCrash
	}

	return msgs, opened, closed, err
}

// dstOpKind is a workload step.
type dstOpKind uint8

const (
	dstAdd dstOpKind = iota
	dstSettle
	dstFail
	dstFee
	dstSign
	dstDeliver
	dstDisconnect
	dstCrash
	dstFlush
)

// dstOp is one workload step.
type dstOp struct {
	kind dstOpKind

	// node is the node the step acts on, 0 for Alice and 1 for Bob. For
	// a delivery it is the receiver.
	node int

	// pick chooses among the HTLCs a removal may name.
	pick int

	// writes is the number of writes before a crash.
	writes int

	// then, if not zero, arms the channel the crash restarts to crash
	// again at that write, so crashes can land back to back, as in the
	// writes channel_reestablish makes right after a crash.
	then int
}

// String describes the step, for failure reports.
func (o dstOp) String() string {
	names := []string{
		"add", "settle", "fail", "fee", "sign", "deliver",
		"disconnect", "crash", "flush",
	}

	return fmt.Sprintf("%s(%d,%d,%d,%d)", names[o.kind], o.node, o.pick,
		o.writes, o.then)
}

// dstScenario is a workload.
type dstScenario struct {
	seed     int64
	chanType channeldb.ChannelType
	ops      []dstOp
}

// drawDSTScenario draws a workload for a channel type.
func drawDSTScenario(t *rapid.T, chanType channeldb.ChannelType) dstScenario {
	sc := dstScenario{
		seed:     rapid.Int64().Draw(t, "seed"),
		chanType: chanType,
	}

	weights := []dstOpKind{
		dstAdd, dstAdd, dstSettle, dstSettle, dstFail, dstFee,
		dstSign, dstSign, dstDeliver, dstDeliver, dstDeliver,
		dstDeliver, dstDisconnect, dstCrash, dstCrash, dstCrash,
		dstFlush, dstFlush,
	}
	n := rapid.IntRange(1, 60).Draw(t, "ops")
	for i := 0; i < n; i++ {
		sc.ops = append(sc.ops, dstOp{
			kind:   rapid.SampledFrom(weights).Draw(t, "kind"),
			node:   rapid.IntRange(0, 1).Draw(t, "node"),
			pick:   rapid.IntRange(0, 7).Draw(t, "pick"),
			writes: rapid.IntRange(1, 3).Draw(t, "writes"),
			then:   rapid.IntRange(0, 2).Draw(t, "then"),
		})
	}

	return sc
}

// dstNode is one end of the simulated channel.
type dstNode struct {
	name, peer string
	ch         *crashChannel
	act        *ChannelActor
	failure    error
}

// dstWorld is the simulated channel.
type dstWorld struct {
	t     *testing.T
	nodes [2]*dstNode

	// stats counts what the run exercised, across runs.
	stats *diffStats

	mu sync.Mutex

	// inflight[i] holds the messages on the wire to node i, in order.
	inflight [2][]lnwire.Message

	// transcript records every message sent, for the determinism check.
	transcript []string

	// bugs records channel failures a crash doesn't explain.
	bugs []string

	// site names the operation of the last crash.
	site string
}

// dstSender is the peer connection of one node.
type dstSender struct {
	w    *dstWorld
	from int
}

// SendMessage puts messages on the wire to the peer.
func (s *dstSender) SendMessage(_ bool, msgs ...lnwire.Message) error {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()

	for _, m := range msgs {
		s.w.inflight[1-s.from] = append(s.w.inflight[1-s.from], m)
		s.w.transcript = append(s.w.transcript, fmt.Sprintf(
			"%d>%v:%x", s.from, m.MsgType(), msgDigest(m),
		))
	}

	return nil
}

// msgDigest hashes a message's encoding.
func msgDigest(m lnwire.Message) []byte {
	var b bytes.Buffer
	if _, err := lnwire.WriteMessage(&b, m, 0); err != nil {
		return nil
	}
	h := sha256.Sum256(b.Bytes())

	return h[:4]
}

// spawn starts an actor over a node's channel and runs its side of
// channel_reestablish's first step: sending ours.
func (w *dstWorld) spawn(i int, lc *lnwallet.LightningChannel) {
	n := w.nodes[i]

	// A crash armed before a disconnection is still to come, and a crash
	// may arm the next one: either may land in the writes
	// channel_reestablish makes.
	var countdown, then int
	if n.ch != nil {
		n.ch.mu.Lock()
		countdown, then = n.ch.countdown, n.ch.then
		if n.ch.crashed {
			countdown, then = n.ch.then, 0
		}
		n.ch.mu.Unlock()
	}
	n.ch = &crashChannel{
		LightningChannel: lc,
		countdown:        countdown,
		then:             then,
	}
	n.failure = nil

	act, err := NewChannelActor(Config{
		Channel:     n.ch,
		Peer:        &dstSender{w: w, from: i},
		CheckLedger: true,
		Observe:     tableObserver(w.t),
		OnFailure: func(err error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			n.failure = err
		},
	})
	require.NoError(w.t, err)
	n.act = act
	require.NoError(w.t, act.Start(w.t.Context()))
}

// reconnect drops everything in flight, reloads both channels from disk,
// and runs channel_reestablish, as a disconnection or a crash does.
func (w *dstWorld) reconnect() {
	w.mu.Lock()
	w.inflight = [2][]lnwire.Message{}
	w.mu.Unlock()

	for i, n := range w.nodes {
		n.act.Stop()
		lc, err := lnwallet.RestartTestChannel(
			n.ch.LightningChannel,
		)
		require.NoError(w.t, err)
		w.spawn(i, lc)
	}
	synctest.Wait()

	// Each side's channel_reestablish is first on the wire.
	w.deliver(0)
	w.deliver(1)
}

// crashed reports whether either node crashed. A failure any other node
// saw is recorded as a bug: in an honest run, only a crash fails the
// channel.
func (w *dstWorld) crashed() bool {
	crashed := false
	for _, n := range w.nodes {
		if n.ch.isCrashed() {
			crashed = true
			w.site = n.ch.crashSite()

			continue
		}
		if err := n.failed(); err != nil {
			w.bugs = append(w.bugs, fmt.Sprintf("%s failed the "+
				"channel: %v", n.name, err))
		}
	}

	return crashed
}

// failed returns the node's channel failure, if any.
func (n *dstNode) failed() error {
	return n.failure
}

// deliver hands node i the next message on the wire to it.
func (w *dstWorld) deliver(i int) {
	w.mu.Lock()
	if len(w.inflight[i]) == 0 {
		w.mu.Unlock()
		return
	}
	msg := w.inflight[i][0]
	w.inflight[i] = w.inflight[i][1:]
	w.mu.Unlock()

	require.NoError(w.t, w.nodes[i].act.ReceiveMessage(w.t.Context(), msg))
	synctest.Wait()
}

// ledgerOfNode returns a node's ledger, if its channel is established.
func (w *dstWorld) ledgerOfNode(i int) (Ledger, bool) {
	state, err := w.nodes[i].act.CurrentState(w.t.Context())
	require.NoError(w.t, err)

	return ledgerOf(state)
}

// apply runs one workload step.
func (w *dstWorld) apply(o dstOp) {
	n := w.nodes[o.node]
	ctx := w.t.Context()
	chanID := n.ch.ChannelID()

	switch o.kind {
	case dstAdd:
		l, ok := w.ledgerOfNode(o.node)
		if !ok {
			return
		}
		id := l.Logs.Local.HtlcCounter
		_, _ = n.act.AddHTLC(ctx, &AddHTLC{
			Htlc: newAdd(chanID, n.name, id),
		})

	case dstSettle, dstFail:
		l, ok := w.ledgerOfNode(o.node)
		if !ok {
			return
		}
		ids := removable(l)
		if len(ids) == 0 {
			return
		}
		id := ids[o.pick%len(ids)]
		if o.kind == dstSettle {
			preimage := preimageFor(n.peer, id)
			_ = n.act.SettleHTLC(ctx, &SettleHTLC{
				Msg: &lnwire.UpdateFulfillHTLC{
					ChanID:          chanID,
					ID:              id,
					PaymentPreimage: preimage,
				},
			})
		} else {
			_ = n.act.FailHTLC(ctx, &FailHTLC{
				Msg: &lnwire.UpdateFailHTLC{
					ChanID: chanID,
					ID:     id,
					Reason: []byte("fail"),
				},
			})
		}

	case dstFee:
		// Only Alice opened the channel.
		rate := w.nodes[0].ch.CommitFeeRate() + 10
		_ = w.nodes[0].act.UpdateFee(ctx, &UpdateFee{FeePerKw: rate})

	case dstSign:
		_, _ = n.act.SignCommitment(ctx)

	case dstDeliver:
		w.deliver(o.node)

	case dstDisconnect:
		w.stats.hit("disconnect")
		w.reconnect()

	case dstCrash:
		n.ch.arm(o.writes, o.then)

	// A flush lets the protocol run until quiet, so that HTLCs lock in
	// often enough for crashes and disconnections to find them.
	case dstFlush:
		w.settle()
	}
	synctest.Wait()

	// A crash takes the connection down with it, and the reconnection
	// may crash again in the write channel_reestablish makes, at most
	// once, since a restarted channel's second countdown is spent. A
	// failure without a crash is a bug, which heal reports.
	w.recover()
}

// recover reconnects after every crash, counting each by its site.
func (w *dstWorld) recover() {
	for w.crashed() {
		w.stats.hit("crash")
		w.stats.hit("crash:" + w.site)
		w.reconnect()
	}
}

// settle has both nodes sign, and delivers everything in flight, until
// the wire is empty or a crash intervenes.
func (w *dstWorld) settle() {
	for round := 0; round < 3; round++ {
		for i := range w.nodes {
			_, _ = w.nodes[i].act.SignCommitment(w.t.Context())
			synctest.Wait()
		}
		for {
			w.mu.Lock()
			pending := len(w.inflight[0]) + len(w.inflight[1])
			w.mu.Unlock()
			if pending == 0 || w.anyCrashed() {
				return
			}
			w.deliver(0)
			w.deliver(1)
		}
	}
}

// anyCrashed reports whether either node crashed, without recording
// anything.
func (w *dstWorld) anyCrashed() bool {
	return w.nodes[0].ch.isCrashed() || w.nodes[1].ch.isCrashed()
}

// heal reconnects if needed, then has both sides resolve every HTLC they
// may, as the link eventually does, and signs and delivers until nothing is
// in flight, nothing is owed, and no HTLC is left.
func (w *dstWorld) heal() {
	for _, n := range w.nodes {
		n.ch.arm(0, 0)
	}
	w.recover()

	for round := 0; round < 20; round++ {
		for i := range w.nodes {
			l, ok := w.ledgerOfNode(i)
			if !ok {
				continue
			}
			for _, id := range removable(l) {
				kind := dstSettle
				if id%2 == 1 {
					kind = dstFail
				}
				w.apply(dstOp{kind: kind, node: i})
			}
		}
		for i := range w.nodes {
			_, _ = w.nodes[i].act.SignCommitment(w.t.Context())
			synctest.Wait()
		}
		for {
			w.mu.Lock()
			pending := len(w.inflight[0]) + len(w.inflight[1])
			w.mu.Unlock()
			if pending == 0 {
				break
			}
			w.deliver(0)
			w.deliver(1)
		}
	}
}

// check returns the oracles the healed channel violates.
func (w *dstWorld) check() []string {
	v := slices.Clone(w.bugs)
	violate := func(format string, args ...any) {
		v = append(v, fmt.Sprintf(format, args...))
	}

	for _, n := range w.nodes {
		if err := n.failed(); err != nil {
			violate("%s failed the channel: %v", n.name, err)
		}
	}
	if len(v) > 0 {
		return v
	}

	a, b := w.nodes[0].ch, w.nodes[1].ch
	aState, bState := a.State(), b.State()

	// Both sides agree on both commitments.
	for _, pair := range [][2]*channeldb.ChannelCommitment{
		{&aState.LocalCommitment, &bState.RemoteCommitment},
		{&aState.RemoteCommitment, &bState.LocalCommitment},
	} {
		x, y := pair[0], pair[1]
		if x.CommitHeight != y.CommitHeight ||
			x.LocalBalance != y.RemoteBalance ||
			x.RemoteBalance != y.LocalBalance ||
			len(x.Htlcs) != len(y.Htlcs) {

			violate("commitments differ: %d/%v/%v/%d vs "+
				"%d/%v/%v/%d", x.CommitHeight, x.LocalBalance,
				x.RemoteBalance, len(x.Htlcs), y.CommitHeight,
				y.RemoteBalance, y.LocalBalance, len(y.Htlcs))
		}
	}

	// Every HTLC was resolved.
	for _, c := range []*channeldb.ChannelCommitment{
		&aState.LocalCommitment, &aState.RemoteCommitment,
	} {
		if len(c.Htlcs) != 0 {
			violate("%d HTLCs left after healing", len(c.Htlcs))
		}
	}

	// Nothing is owed, and the channel is clean of pending commitments.
	for i, n := range w.nodes {
		l, ok := w.ledgerOfNode(i)
		if !ok {
			violate("%s not established after healing", n.name)
			continue
		}
		if l.OweCommitment(lntypes.Local) {
			violate("%s still owes a commitment", n.name)
		}
		if l.Chains.Remote.Pending.IsSome() {
			violate("%s still awaits a revocation", n.name)
		}
	}

	// Every HTLC is forwarded exactly once, and every removal of one
	// exactly once, counted over the forwarding packages on disk, which
	// is what survives crashes.
	for i, n := range w.nodes {
		peer := w.nodes[1-i]
		pkgs, err := n.ch.LoadFwdPkgs()
		if err != nil {
			violate("%s: loading forwarding packages: %v",
				n.name, err)
			continue
		}

		var adds, removals []uint64
		for _, pkg := range pkgs {
			for _, f := range forwardsOf(pkg) {
				w.stats.hit("forward:" + f.Kind.String())
				if f.Kind == KindAdd {
					adds = append(adds, f.ID)
				} else {
					removals = append(removals, f.ID)
				}
			}
		}

		// The peer's adds that are committed are exactly IDs 0 up
		// to its counter, since a crash that loses an add loses its
		// ID too, and each must be forwarded once.
		pl, _ := w.ledgerOfNode(1 - i)
		slices.Sort(adds)
		for id := uint64(0); id < pl.Logs.Local.HtlcCounter; id++ {
			if c := count(adds, id); c != 1 {
				violate("%s forwarded %s's HTLC %d %d times",
					n.name, peer.name, id, c)
			}
		}
		if uint64(len(adds)) != pl.Logs.Local.HtlcCounter {
			violate("%s forwarded %d adds, %s offered %d",
				n.name, len(adds), peer.name,
				pl.Logs.Local.HtlcCounter)
		}

		// Every one of our HTLCs was removed by the peer, and each
		// removal forwarded back once.
		nl, _ := w.ledgerOfNode(i)
		for id := uint64(0); id < nl.Logs.Local.HtlcCounter; id++ {
			if c := count(removals, id); c != 1 {
				violate("%s forwarded the removal of its "+
					"HTLC %d %d times", n.name, id, c)
			}
		}
		if htlcs := nl.Logs.Local.HtlcCounter; uint64(len(removals)) !=
			htlcs {

			violate("%s forwarded %d removals of its %d HTLCs",
				n.name, len(removals), htlcs)
		}
	}

	return v
}

// count returns how often x occurs in xs.
func count(xs []uint64, x uint64) int {
	c := 0
	for _, y := range xs {
		if y == x {
			c++
		}
	}

	return c
}

// runDSTScenario runs a workload in a fresh bubble, and returns the
// transcript of every message sent and the oracles it violated. It reports
// violations instead of failing, so the caller can fail the rapid run and
// let rapid shrink the scenario.
func runDSTScenario(t *testing.T, sc dstScenario,
	stats *diffStats) ([]string, []string) {

	var transcript, violations []string

	synctest.Test(t, func(t *testing.T) {
		alice, bob, err := lnwallet.CreateTestChannelsWithRand(
			t, sc.chanType, mrand.New(mrand.NewSource(sc.seed)),
		)
		require.NoError(t, err)

		w := &dstWorld{t: t, stats: stats}
		w.nodes[0] = &dstNode{name: "alice", peer: "bob"}
		w.nodes[1] = &dstNode{name: "bob", peer: "alice"}
		w.nodes[0].ch = &crashChannel{LightningChannel: alice}
		w.nodes[1].ch = &crashChannel{LightningChannel: bob}
		t.Cleanup(func() {
			for _, n := range w.nodes {
				if n.act != nil {
					n.act.Stop()
				}
			}
		})

		// lnd loads a channel from disk before every connection.
		for i, lc := range []*lnwallet.LightningChannel{alice, bob} {
			lc, err := lnwallet.RestartTestChannel(lc)
			require.NoError(t, err)
			w.spawn(i, lc)
		}
		synctest.Wait()
		w.deliver(0)
		w.deliver(1)

		for _, o := range sc.ops {
			w.apply(o)
		}
		w.heal()

		violations = w.check()
		transcript = w.transcript
	})

	return transcript, violations
}

// dstProperty returns the simulation property for a channel type.
func dstProperty(t *testing.T, chanType channeldb.ChannelType,
	stats *diffStats) func(*rapid.T) {

	return func(rt *rapid.T) {
		sc := drawDSTScenario(rt, chanType)
		if _, v := runDSTScenario(t, sc, stats); len(v) > 0 {
			rt.Fatalf("scenario %v violated: %v", sc.ops, v)
		}
	}
}

// TestDSTChannel runs random workloads against two channel actors in a
// synctest bubble, with disconnections and crashes, and checks agreement,
// quiescence and exactly-once forwarding after the channel heals.
func TestDSTChannel(t *testing.T) {
	t.Parallel()

	chanTypes := map[string]channeldb.ChannelType{
		"anchors": channeldb.SingleFunderTweaklessBit |
			channeldb.AnchorOutputsBit,
		"taproot": channeldb.SingleFunderTweaklessBit |
			channeldb.AnchorOutputsBit |
			channeldb.SimpleTaprootFeatureBit,
	}
	for name, chanType := range chanTypes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var stats diffStats
			rapid.Check(t, dstProperty(t, chanType, &stats))
			t.Logf("coverage: %v", stats.counts)

			if testing.Short() {
				return
			}
			for _, must := range []string{
				"crash", "crash:sign", "crash:revoke",
				"crash:revocation", "disconnect", "forward:Add",
				"forward:Settle", "forward:Fail",
			} {
				require.Positive(t, stats.counts[must],
					"never exercised %q", must)
			}
		})
	}
}

// TestDSTCrashInReestablish crashes a node in the one write
// channel_reestablish makes, which random workloads reach only a few times
// a run. Bob adds an HTLC and signs; Alice revokes her commitment for it and
// crashes, so she never sends the revocation, nor the commitment she owes
// Bob for his add. On reconnect she resends the revocation and signs that
// commitment, and crashes again right after writing it. The next reconnect
// must resend both, revocation first, and the channel must heal with the
// add forwarded exactly once.
func TestDSTCrashInReestablish(t *testing.T) {
	t.Parallel()

	sc := dstScenario{
		chanType: channeldb.SingleFunderTweaklessBit |
			channeldb.AnchorOutputsBit,
		ops: []dstOp{
			{kind: dstAdd, node: 1},
			{kind: dstSign, node: 1},
			{kind: dstCrash, node: 0, writes: 1, then: 1},
			{kind: dstDeliver, node: 0},
			{kind: dstDeliver, node: 0},
			{kind: dstFlush},
		},
	}
	var stats diffStats
	_, violations := runDSTScenario(t, sc, &stats)
	require.Empty(t, violations)
	require.Equal(t, 1, stats.counts["crash:revoke"], stats.counts)
	require.Equal(t, 1, stats.counts["crash:reestablish"], stats.counts)
	require.Equal(t, 1, stats.counts["forward:Add"], stats.counts)
}

// TestDSTDeterminism runs the same scenarios twice and requires the same
// transcript, byte for byte: nothing in a run may depend on goroutine
// scheduling. Taproot channels are left out, since lnd draws their MuSig2
// nonces from crypto/rand. Ten scenarios are enough to catch scheduling
// dependence, which would show in almost every run.
func TestDSTDeterminism(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit
	var runs atomic.Int32
	rapid.Check(t, func(rt *rapid.T) {
		if runs.Add(1) > 10 {
			return
		}
		sc := drawDSTScenario(rt, chanType)
		var stats diffStats
		first, _ := runDSTScenario(t, sc, &stats)
		second, _ := runDSTScenario(t, sc, &stats)
		require.Equal(rt, first, second)
	})
}

// FuzzDSTChannel runs the simulation property under Go's fuzzer.
func FuzzDSTChannel(f *testing.F) {
	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit
	f.Fuzz(func(t *testing.T, data []byte) {
		var stats diffStats
		rapid.MakeFuzz(dstProperty(t, chanType, &stats))(t, data)
	})
}

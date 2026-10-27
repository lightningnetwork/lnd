package chanfsm

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	mrand "math/rand"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// This file is the differential test between the old implementation, a
// LightningChannel driven directly the way the link drives it, and the new
// one, the same LightningChannel driven by the channel actor. Each run
// builds two identical worlds. In both, Bob is an honest legacy node. Alice
// is the implementation under test: legacy in the old world, the actor in
// the new one. Every step is applied to both worlds: a command at either
// node, the delivery of the next message in either direction, or a message
// a byzantine Bob injects at Alice.
//
// After each step the two worlds must agree: the same outcome for the step,
// the same messages sent (byte for byte, except for taproot channels, whose
// MuSig2 nonces are random), the same forwarding packages, and the same
// channel state at both nodes, including the peer's revocation points and
// the stored revocation secrets. The one allowed difference is the new
// implementation being safer: it refuses a message or command the old one
// accepts in violation of BOLT 2, or it refuses a message without changing
// the channel where the old one changes it while refusing. Each such case
// must match a known, named rule; any other difference fails the test, and
// so does the new implementation accepting anything the old one refuses.

// aliceImpl is Alice's side of the channel, in either world.
//
//nolint:interfacebloat
type aliceImpl interface {
	// addHTLC offers an HTLC with the given ID.
	addHTLC(id uint64) error

	// settleHTLC, failHTLC and malformHTLC remove one of Bob's HTLCs.
	settleHTLC(id uint64) error
	failHTLC(id uint64) error
	malformHTLC(id uint64) error

	// updateFee proposes a fee rate.
	updateFee(rate chainfee.SatPerKWeight) error

	// signCommitment signs if owed and allowed.
	signCommitment() error

	// receive handles a message from Bob. It returns the failure if the
	// channel failed.
	receive(msg lnwire.Message) error

	// drain returns and clears what Alice sent and forwarded.
	drain() ([]lnwire.Message, []*channeldb.FwdPkg)

	// channel is Alice's channel.
	channel() *lnwallet.LightningChannel

	// restart reloads Alice's channel from disk, losing everything
	// that was only in memory.
	restart() error

	// connect sends Alice's channel_reestablish.
	connect() error
}

// oldAlice is Alice as a legacy node.
type oldAlice struct {
	*legacyNode
}

func (a *oldAlice) addHTLC(id uint64) error {
	got, err := a.legacyNode.addHTLC()
	if err == nil && got != id {
		return fmt.Errorf("took ID %d, want %d", got, id)
	}

	return err
}

func (a *oldAlice) receive(msg lnwire.Message) error {
	return a.legacyNode.receive(msg)
}

func (a *oldAlice) drain() ([]lnwire.Message, []*channeldb.FwdPkg) {
	sent, fwds := a.sent, a.fwds
	a.sent, a.fwds = nil, nil

	return sent, fwds
}

func (a *oldAlice) channel() *lnwallet.LightningChannel {
	return a.lc
}

func (a *oldAlice) restart() error {
	return a.legacyNode.restart()
}

func (a *oldAlice) connect() error {
	return a.legacyNode.connect()
}

// newAlice is Alice as the channel actor.
type newAlice struct {
	t       *testing.T
	lc      *lnwallet.LightningChannel
	act     *ChannelActor
	observe func(Event, ChanState, ChanState, []Outbox)

	mu      sync.Mutex
	sent    []lnwire.Message
	fwds    []*channeldb.FwdPkg
	failure error
}

// SendMessage records what the actor sends to Bob.
func (a *newAlice) SendMessage(_ bool, msgs ...lnwire.Message) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.sent = append(a.sent, msgs...)

	return nil
}

// startNewAlice starts an actor over Alice's channel.
func startNewAlice(t *testing.T, lc *lnwallet.LightningChannel,
	observe func(Event, ChanState, ChanState, []Outbox)) *newAlice {

	a := &newAlice{t: t, lc: lc, observe: observe}
	a.spawn()

	return a
}

// spawn creates an actor for Alice's channel, which the next connect
// starts.
func (a *newAlice) spawn() {
	act, err := NewChannelActor(Config{
		Channel: a.lc,
		Peer:    a,
		OnForward: func(pkg *channeldb.FwdPkg) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.fwds = append(a.fwds, pkg)
		},
		OnFailure: func(err error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.failure = err
		},
		CheckLedger: true,
		Observe:     a.observe,
	})
	require.NoError(a.t, err)
	a.t.Cleanup(act.Stop)
	a.act = act
}

func (a *newAlice) restart() error {
	a.act.Stop()
	lc, err := lnwallet.RestartTestChannel(a.lc)
	if err != nil {
		return err
	}
	a.lc = lc
	a.spawn()

	return nil
}

func (a *newAlice) connect() error {
	if err := a.act.Start(a.t.Context()); err != nil {
		return err
	}

	return a.failed()
}

func (a *newAlice) addHTLC(id uint64) error {
	got, err := a.act.AddHTLC(a.t.Context(), &AddHTLC{
		Htlc: newAdd(a.lc.ChannelID(), "alice", id),
	})
	if err == nil && got != id {
		return fmt.Errorf("took ID %d, want %d", got, id)
	}

	return err
}

func (a *newAlice) settleHTLC(id uint64) error {
	return a.act.SettleHTLC(a.t.Context(), &SettleHTLC{
		Msg: &lnwire.UpdateFulfillHTLC{
			ChanID:          a.lc.ChannelID(),
			ID:              id,
			PaymentPreimage: preimageFor("bob", id),
		},
	})
}

func (a *newAlice) failHTLC(id uint64) error {
	return a.act.FailHTLC(a.t.Context(), &FailHTLC{
		Msg: &lnwire.UpdateFailHTLC{
			ChanID: a.lc.ChannelID(),
			ID:     id,
			Reason: []byte("fail"),
		},
	})
}

func (a *newAlice) malformHTLC(id uint64) error {
	return a.act.MalformedFailHTLC(a.t.Context(), &MalformedFailHTLC{
		Msg: &lnwire.UpdateFailMalformedHTLC{
			ChanID:      a.lc.ChannelID(),
			ID:          id,
			FailureCode: lnwire.CodeInvalidOnionVersion,
		},
	})
}

func (a *newAlice) updateFee(rate chainfee.SatPerKWeight) error {
	return a.act.UpdateFee(a.t.Context(), &UpdateFee{FeePerKw: rate})
}

func (a *newAlice) signCommitment() error {
	_, err := a.act.SignCommitment(a.t.Context())
	if err != nil {
		return err
	}

	return a.failed()
}

func (a *newAlice) receive(msg lnwire.Message) error {
	require.NoError(a.t, a.act.ReceiveMessage(a.t.Context(), msg))

	return a.failed()
}

func (a *newAlice) failed() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.failure
}

func (a *newAlice) drain() ([]lnwire.Message, []*channeldb.FwdPkg) {
	a.mu.Lock()
	defer a.mu.Unlock()

	sent, fwds := a.sent, a.fwds
	a.sent, a.fwds = nil, nil

	return sent, fwds
}

func (a *newAlice) channel() *lnwallet.LightningChannel {
	return a.lc
}

// world is one of the two worlds of a differential run.
type world struct {
	alice aliceImpl
	bob   *legacyNode

	// toAlice and toBob are the messages in flight.
	toAlice, toBob []lnwire.Message

	// bobRAAs are the revocations Alice received from Bob, for replays.
	bobRAAs []*lnwire.RevokeAndAck
}

// collect moves what both nodes sent into flight, and returns what Alice
// sent and forwarded.
func (w *world) collect() ([]lnwire.Message, []*channeldb.FwdPkg) {
	sent, fwds := w.alice.drain()
	w.toBob = append(w.toBob, sent...)
	w.toAlice = append(w.toAlice, w.bob.sent...)
	w.bob.sent = nil

	return sent, fwds
}

// fingerprint is the part of a channel's state a message could change: the
// protocol state, the commitment heights, and the peer's revocation points.
type fingerprint struct {
	snapshot     *lnwallet.ProtocolSnapshot
	localHeight  uint64
	remoteHeight uint64
	remoteCur    []byte
	remoteNext   []byte
	secrets      []byte
}

// fingerprintOf returns a channel's fingerprint.
func fingerprintOf(lc *lnwallet.LightningChannel) fingerprint {
	st := lc.State()
	f := fingerprint{
		snapshot:     lc.ProtocolSnapshot(),
		localHeight:  st.LocalCommitment.CommitHeight,
		remoteHeight: st.RemoteCommitment.CommitHeight,
	}
	if st.RemoteCurrentRevocation != nil {
		f.remoteCur = st.RemoteCurrentRevocation.SerializeCompressed()
	}
	if st.RemoteNextRevocation != nil {
		f.remoteNext = st.RemoteNextRevocation.SerializeCompressed()
	}

	var secrets bytes.Buffer
	if err := st.RevocationStore.Encode(&secrets); err == nil {
		f.secrets = secrets.Bytes()
	}

	return f
}

// diffRun is one differential run.
type diffRun struct {
	t        *testing.T
	rt       *rapid.T
	chanType channeldb.ChannelType
	exact    bool
	stats    *diffStats
	old, new *world

	// shadow is a ledger of Alice's channel, which picks the IDs that
	// make commands and injections meaningful. Both worlds' channels
	// must match it after every agreeing step.
	shadow func() Ledger
}

// newDiffRun builds the two worlds.
func newDiffRun(t *testing.T, rt *rapid.T, chanType channeldb.ChannelType,
	stats *diffStats) *diffRun {

	// Both worlds draw the channel's random parameters from the same
	// seed, so they start out identical.
	seed := rapid.Int64().Draw(rt, "seed")

	mkWorld := func(useActor bool) *world {
		aliceLC, bobLC, err := lnwallet.CreateTestChannelsWithRand(
			t, chanType, mrand.New(mrand.NewSource(seed)),
		)
		require.NoError(t, err)

		w := &world{bob: &legacyNode{
			name: "bob", peer: "alice", lc: bobLC,
		}}
		if useActor {
			w.alice = startNewAlice(t, aliceLC, tableObserver(t))
		} else {
			w.alice = &oldAlice{&legacyNode{
				name: "alice", peer: "bob", lc: aliceLC,
			}}
		}

		return w
	}

	r := &diffRun{
		t:        t,
		rt:       rt,
		chanType: chanType,
		exact:    !chanType.IsTaproot(),
		stats:    stats,
		old:      mkWorld(false),
		new:      mkWorld(true),
	}
	r.shadow = func() Ledger {
		l, err := LedgerFromSnapshot(
			r.old.alice.channel().ProtocolSnapshot(),
			lntypes.Local,
		)
		require.NoError(rt, err)

		return l
	}

	return r
}

// outcome is the result of one step in one world.
type outcome struct {
	// err is the step's error: a refused command, or a channel failure.
	err error

	// sent and fwds are what Alice sent and forwarded.
	sent []lnwire.Message
	fwds []*channeldb.FwdPkg

	// before and after fingerprint Alice's channel around the step.
	before, after fingerprint
}

// step applies the same action to both worlds and returns both outcomes.
func (r *diffRun) step(act func(w *world) error) (outcome, outcome) {
	do := func(w *world) outcome {
		o := outcome{before: fingerprintOf(w.alice.channel())}
		o.err = act(w)
		o.sent, o.fwds = w.collect()
		o.after = fingerprintOf(w.alice.channel())

		return o
	}

	return do(r.old), do(r.new)
}

// safer names the known ways the new implementation may differ by being
// safer. It returns the rule that explains the difference, or "".
type safer func(o, n outcome) string

// compare checks the two outcomes of a step. It returns false if the run
// must stop because the channel failed or the worlds legitimately diverged.
func (r *diffRun) compare(what string, o, n outcome, rules ...safer) bool {
	rt := r.rt

	switch {
	// Both accepted: everything observable must match.
	case o.err == nil && n.err == nil:
		r.sameMessages(what, o.sent, n.sent)
		r.sameFwds(what, o.fwds, n.fwds)
		r.sameState(what, o.after, n.after)
		r.sameBob(what)
		r.stats.hit("agree:" + what)

		return true

	// Both refused. When the new implementation refused without asking
	// the channel, its channel must be unchanged. The old one may have
	// changed its channel while refusing, which is one of the safer
	// cases.
	case o.err != nil && n.err != nil:
		r.stats.hit("both-refuse:" + what)
		for _, rule := range rules {
			if name := rule(o, n); name != "" {
				r.stats.hit("safer:" + name)
				return false
			}
		}
		r.sameState(what, o.after, n.after)

		return false

	// Only the new implementation refused: it must be a known rule.
	case o.err == nil && n.err != nil:
		for _, rule := range rules {
			if name := rule(o, n); name != "" {
				r.stats.hit("safer:" + name)
				return false
			}
		}
		rt.Fatalf("%s: new refused (%v) where old accepted", what,
			n.err)

	// The new implementation must never accept what the old refuses.
	default:
		rt.Fatalf("%s: new accepted where old refused (%v)", what,
			o.err)
	}

	return false
}

// sameMessages checks that both Alices sent the same messages.
func (r *diffRun) sameMessages(what string, o, n []lnwire.Message) {
	require.Len(r.rt, n, len(o), "%s: messages sent", what)
	for i := range o {
		require.IsType(r.rt, o[i], n[i], "%s: message %d", what, i)
		if !r.exact {
			continue
		}

		var ob, nb bytes.Buffer
		_, err := lnwire.WriteMessage(&ob, o[i], 0)
		require.NoError(r.rt, err)
		_, err = lnwire.WriteMessage(&nb, n[i], 0)
		require.NoError(r.rt, err)
		require.Equal(r.rt, ob.Bytes(), nb.Bytes(), "%s: message %d "+
			"(%T)", what, i, o[i])
	}
}

// sameFwds checks that both Alices forwarded the same packages.
func (r *diffRun) sameFwds(what string, o, n []*channeldb.FwdPkg) {
	require.Len(r.rt, n, len(o), "%s: forwarding packages", what)
	for i := range o {
		require.Equal(r.rt, o[i].Height, n[i].Height, what)
		require.Equal(r.rt, forwardsOf(o[i]), forwardsOf(n[i]), what)
		require.True(r.rt, reflect.DeepEqual(o[i], n[i]),
			"%s: forwarding package %d", what, i)
		for _, ref := range forwardsOf(o[i]) {
			r.stats.hit("forward:" + ref.Kind.String())
		}
	}
}

// sameState checks that both Alices' channels are in the same state.
func (r *diffRun) sameState(what string, o, n fingerprint) {
	require.Equal(r.rt, o.snapshot, n.snapshot, "%s: protocol state",
		what)
	require.Equal(r.rt, o.localHeight, n.localHeight, what)
	require.Equal(r.rt, o.remoteHeight, n.remoteHeight, what)
	require.Equal(r.rt, o.remoteCur, n.remoteCur, what)
	require.Equal(r.rt, o.remoteNext, n.remoteNext, what)
	require.Equal(r.rt, o.secrets, n.secrets, "%s: revocation store",
		what)

	oa, na := r.old.alice.channel(), r.new.alice.channel()
	ol, or := oa.CommitBalances()
	nl, nr := na.CommitBalances()
	require.Equal(r.rt, ol, nl, "%s: local balance", what)
	require.Equal(r.rt, or, nr, "%s: remote balance", what)
}

// sameBob checks that both Bobs are in the same state.
func (r *diffRun) sameBob(what string) {
	require.Equal(r.rt, r.old.bob.lc.ProtocolSnapshot(),
		r.new.bob.lc.ProtocolSnapshot(), "%s: bob", what)
	require.Equal(r.rt, r.old.bob.failed == nil, r.new.bob.failed == nil,
		"%s: bob failed", what)
}

// Rules for when the new implementation may differ by being safer.
var (
	// untouchedOnRefusal: both refused, but only the old implementation
	// changed its channel while doing so. This is the premature
	// revoke_and_ack: the old one rotates the peer's revocation points
	// and consumes a shachain slot before its database write fails.
	untouchedOnRefusal safer = func(o, n outcome) string {
		if o.err == nil || n.err == nil {
			return ""
		}
		if !reflect.DeepEqual(n.before, n.after) {
			return ""
		}
		if reflect.DeepEqual(o.before, o.after) {
			return ""
		}

		return "refused without changing the channel"
	}

	// earlyRemovalRefused: the peer removed an HTLC that isn't in our
	// current commitment, which BOLT 2 says the receiver must fail the
	// channel for. The old implementation accepts it, and only fails at
	// the next commitment_signed.
	earlyRemovalRefused safer = func(o, n outcome) string {
		var e *ErrHtlcNotCommitted
		if o.err == nil && n.err != nil && errors.As(n.err, &e) {
			return "removal of an uncommitted HTLC refused"
		}

		return ""
	}

	// localEarlyRemovalRefused: we tried to remove an HTLC before it is
	// irrevocably committed, which BOLT 2 forbids a sender. The old
	// implementation sends the removal.
	localEarlyRemovalRefused safer = func(o, n outcome) string {
		var e *ErrHtlcNotCommitted
		if o.err == nil && n.err != nil && errors.As(n.err, &e) {
			return "local removal before lock-in refused"
		}

		return ""
	}
)

// deliver delivers the next in-flight message in one direction, in both
// worlds.
func (r *diffRun) deliver(toAlice bool) bool {
	if toAlice {
		if len(r.old.toAlice) == 0 {
			return true
		}
		require.Len(r.rt, r.new.toAlice, len(r.old.toAlice))

		o, n := r.step(func(w *world) error {
			msg := w.toAlice[0]
			w.toAlice = w.toAlice[1:]
			if raa, ok := msg.(*lnwire.RevokeAndAck); ok {
				w.bobRAAs = append(w.bobRAAs, raa)
			}

			return w.alice.receive(msg)
		})

		return r.compare("deliver-to-alice", o, n)
	}

	if len(r.old.toBob) == 0 {
		return true
	}
	o, n := r.step(func(w *world) error {
		msg := w.toBob[0]
		w.toBob = w.toBob[1:]

		return w.bob.receive(msg)
	})

	return r.compare("deliver-to-bob", o, n)
}

// flush has both nodes sign and delivers everything in flight, twice.
func (r *diffRun) flush() bool {
	for round := 0; round < 2; round++ {
		if !r.aliceCommand("sign") {
			return false
		}
		r.bobCommand("sign")
		for len(r.old.toAlice)+len(r.old.toBob) > 0 {
			if !r.deliver(true) || !r.deliver(false) {
				return false
			}
		}
	}

	return true
}

// reconnect disconnects the two nodes in both worlds, losing every message
// in flight, reloads every channel from disk, and runs channel_reestablish.
// A byzantine Bob may forge his channel_reestablish.
func (r *diffRun) reconnect(forge func(*lnwire.ChannelReestablish)) bool {
	r.stats.hit("reconnect")
	o, n := r.step(func(w *world) error {
		w.toAlice, w.toBob, w.bob.sent = nil, nil, nil
		w.alice.drain()
		if err := w.alice.restart(); err != nil {
			return err
		}

		return w.bob.restart()
	})
	if !r.compare("restart", o, n) {
		return false
	}

	return r.handshake(forge)
}

// handshake has both nodes send their channel_reestablish, then delivers
// each, as the link does before anything else on a new connection.
func (r *diffRun) handshake(forge func(*lnwire.ChannelReestablish)) bool {
	o, n := r.step(func(w *world) error {
		if err := w.bob.connect(); err != nil {
			return err
		}
		if forge != nil {
			sent, ok := w.bob.sent[0].(*lnwire.ChannelReestablish)
			if !ok {
				return fmt.Errorf("bob sent %T first",
					w.bob.sent[0])
			}
			msg := *sent
			forge(&msg)
			w.bob.sent[0] = &msg
		}

		return w.alice.connect()
	})
	if !r.compare("connect", o, n) {
		return false
	}

	return r.deliver(true) && r.deliver(false)
}

// bobHTLCs returns Bob's HTLCs Alice may remove, and the ones she has but
// may not yet remove.
func (r *diffRun) bobHTLCs() ([]uint64, []uint64) {
	var ready, early []uint64
	l := r.shadow()
	for _, e := range l.Logs.Remote.Entries {
		if e.Kind != KindAdd || l.Logs.Remote.modified(e.HtlcIndex) {
			continue
		}
		if l.LockedIn(e) {
			ready = append(ready, e.HtlcIndex)
		} else {
			early = append(early, e.HtlcIndex)
		}
	}

	return ready, early
}

// aliceCommand runs one of Alice's commands in both worlds.
func (r *diffRun) aliceCommand(cmd string) bool {
	rt := r.rt

	switch cmd {
	case "add":
		id := r.shadow().Logs.Local.HtlcCounter
		o, n := r.step(func(w *world) error {
			return w.alice.addHTLC(id)
		})

		// A refused add is not a channel failure: both refuse it for
		// the same reason, the balance, and carry on.
		if o.err != nil && n.err != nil {
			r.stats.hit("both-refuse:add")
			r.sameState("add", o.after, n.after)
			return true
		}

		return r.compare("add", o, n)

	case "settle", "fail", "malform":
		ready, early := r.bobHTLCs()
		pool, name := ready, cmd

		// An early removal ends the run, so it is drawn rarely
		// enough for most runs to get deep into the protocol. Only an
		// early removal may be refused as early: refusing one of the
		// ready HTLCs would be a ledger bug, not a safer rule.
		var rules []safer
		early10 := rapid.IntRange(0, 9).Draw(rt, "early") == 0
		if len(early) > 0 && early10 {
			pool, name = early, cmd+"-early"
			rules = []safer{localEarlyRemovalRefused}
		}
		if len(pool) == 0 {
			return true
		}
		id := rapid.SampledFrom(pool).Draw(rt, "id")

		o, n := r.step(func(w *world) error {
			switch cmd {
			case "settle":
				return w.alice.settleHTLC(id)
			case "fail":
				return w.alice.failHTLC(id)
			default:
				return w.alice.malformHTLC(id)
			}
		})

		return r.compare(name, o, n, rules...)

	case "fee":
		rate := r.old.alice.channel().CommitFeeRate() +
			chainfee.SatPerKWeight(
				rapid.IntRange(-50, 50).Draw(rt, "delta"),
			)
		o, n := r.step(func(w *world) error {
			return w.alice.updateFee(rate)
		})

		return r.compare("fee", o, n)

	case "sign":
		o, n := r.step(func(w *world) error {
			return w.alice.signCommitment()
		})

		return r.compare("alice-sign", o, n)
	}

	panic(cmd)
}

// bobCommand runs one of honest Bob's commands in both worlds.
func (r *diffRun) bobCommand(cmd string) bool {
	rt := r.rt
	bobLedger := func() Ledger {
		l, err := LedgerFromSnapshot(
			r.old.bob.lc.ProtocolSnapshot(), lntypes.Remote,
		)
		require.NoError(rt, err)

		return l
	}

	var act func(w *world) error
	switch cmd {
	case "add":
		act = func(w *world) error {
			_, err := w.bob.addHTLC()
			return err
		}

	case "settle", "fail", "malform":
		ids := removable(bobLedger())
		if len(ids) == 0 {
			return true
		}
		id := rapid.SampledFrom(ids).Draw(rt, "bob-id")
		act = func(w *world) error {
			switch cmd {
			case "settle":
				return w.bob.settleHTLC(id)
			case "fail":
				return w.bob.failHTLC(id)
			default:
				return w.bob.malformHTLC(id)
			}
		}

	case "sign":
		act = func(w *world) error { return w.bob.signCommitment() }
	}

	o, n := r.step(act)
	require.Equal(rt, o.err == nil, n.err == nil, "bob %s", cmd)
	if o.err == nil {
		r.sameBob("bob-" + cmd)
	}

	return true
}

// bobResolveAll has Bob settle or fail every HTLC of Alice's he may remove,
// in both worlds.
func (r *diffRun) bobResolveAll() {
	l, err := LedgerFromSnapshot(
		r.old.bob.lc.ProtocolSnapshot(), lntypes.Remote,
	)
	require.NoError(r.rt, err)

	for _, id := range removable(l) {
		settle := id%2 == 0
		o, n := r.step(func(w *world) error {
			if settle {
				return w.bob.settleHTLC(id)
			}

			return w.bob.failHTLC(id)
		})
		require.NoError(r.rt, o.err)
		require.NoError(r.rt, n.err)
		r.sameBob("bob-resolve")
	}
}

// inject delivers a message from a byzantine Bob straight to Alice in both
// worlds, and compares the outcomes. Bob's channel doesn't see it.
func (r *diffRun) inject(kind string) bool {
	rt := r.rt
	chanID := r.old.alice.channel().ChannelID()
	l := r.shadow()

	var (
		msg   func(w *world) lnwire.Message
		rules []safer
	)
	switch kind {
	// A revoke_and_ack while Alice has no commitment outstanding for Bob,
	// revealing the secret of Bob's current commitment, which he knows.
	// This is the message the missing guard in ReceiveRevocation let
	// through.
	case "premature-raa":
		if l.Chains.Remote.Pending.IsSome() {
			return true
		}
		msg = func(w *world) lnwire.Message {
			st := w.bob.lc.State()
			h := st.LocalCommitment.CommitHeight
			secret, err := st.RevocationProducer.AtIndex(h)
			require.NoError(rt, err)
			next, err := st.RevocationProducer.AtIndex(h + 2)
			require.NoError(rt, err)

			raa := &lnwire.RevokeAndAck{
				ChanID: chanID,
				NextRevocationKey: input.ComputeCommitmentPoint(
					next[:],
				),
			}
			copy(raa.Revocation[:], secret[:])

			return raa
		}
		rules = []safer{untouchedOnRefusal}

	// A replay of the last revoke_and_ack Bob sent.
	case "replay-raa":
		if len(r.old.bobRAAs) == 0 {
			return true
		}
		msg = func(w *world) lnwire.Message {
			return w.bobRAAs[len(w.bobRAAs)-1]
		}
		rules = []safer{untouchedOnRefusal}

	// An add that skips an ID.
	case "add-skip-id":
		msg = func(w *world) lnwire.Message {
			skip := l.Logs.Remote.HtlcCounter + 1
			return newAdd(chanID, "bob", skip)
		}

	// A settle of an HTLC Alice never offered.
	case "settle-unknown":
		msg = func(w *world) lnwire.Message {
			return &lnwire.UpdateFulfillHTLC{
				ChanID: chanID,
				ID:     l.Logs.Local.HtlcCounter + 7,
			}
		}

	// A settle, fail or malformed fail of Alice's newest HTLC while it
	// isn't in her current commitment yet.
	case "settle-early", "fail-early", "malform-early":
		// Give Alice a fresh HTLC if every one of hers is committed.
		id, ok := uncommittedAdd(l)
		if !ok {
			if !r.aliceCommand("add") {
				return false
			}
			l = r.shadow()
			if id, ok = uncommittedAdd(l); !ok {
				return true
			}
		}
		msg = func(w *world) lnwire.Message {
			switch kind {
			case "settle-early":
				preimage := preimageFor("alice", id)
				return &lnwire.UpdateFulfillHTLC{
					ChanID:          chanID,
					ID:              id,
					PaymentPreimage: preimage,
				}
			case "fail-early":
				return &lnwire.UpdateFailHTLC{
					ChanID: chanID, ID: id,
				}
			default:
				return &lnwire.UpdateFailMalformedHTLC{
					ChanID:      chanID,
					ID:          id,
					FailureCode: lnwire.CodeInvalidOnionKey,
				}
			}
		}
		rules = []safer{earlyRemovalRefused}

	// A settle with the wrong preimage of an HTLC in Alice's commitment.
	case "settle-bad-preimage":
		id, ok := committedAdd(l)
		if !ok {
			return true
		}
		msg = func(w *world) lnwire.Message {
			return &lnwire.UpdateFulfillHTLC{
				ChanID:          chanID,
				ID:              id,
				PaymentPreimage: sha256.Sum256([]byte("wrong")),
			}
		}

	// A second removal of an HTLC Bob already removed.
	case "double-removal":
		id, ok := removedAdd(l)
		if !ok {
			return true
		}
		msg = func(w *world) lnwire.Message {
			return &lnwire.UpdateFailHTLC{ChanID: chanID, ID: id}
		}

	// A fee update from Bob, who isn't the initiator.
	case "fee-non-initiator":
		msg = func(w *world) lnwire.Message {
			return &lnwire.UpdateFee{ChanID: chanID, FeePerKw: 5000}
		}

	// A malformed fail without the BADONION bit.
	case "malform-no-badonion":
		id, ok := committedAdd(l)
		if !ok {
			return true
		}
		msg = func(w *world) lnwire.Message {
			return &lnwire.UpdateFailMalformedHTLC{
				ChanID:      chanID,
				ID:          id,
				FailureCode: lnwire.CodeTemporaryChannelFailure,
			}
		}

	// A channel_reestablish asking for a commitment we never signed.
	case "reestablish-future":
		r.stats.hit("inject:" + kind)
		return r.reconnect(func(m *lnwire.ChannelReestablish) {
			m.NextLocalCommitHeight += 2
		})

	// A channel_reestablish claiming we lost state, without the secret
	// that would prove it.
	case "reestablish-lost":
		r.stats.hit("inject:" + kind)
		return r.reconnect(func(m *lnwire.ChannelReestablish) {
			m.RemoteCommitTailHeight++
		})

	// A commitment_signed with a signature that isn't valid.
	case "bad-commit-sig":
		msg = func(w *world) lnwire.Message {
			return &lnwire.CommitSig{ChanID: chanID}
		}

	default:
		panic(kind)
	}

	o, n := r.step(func(w *world) error {
		return w.alice.receive(msg(w))
	})
	r.stats.hit("inject:" + kind)

	return r.compare("inject-"+kind, o, n, rules...)
}

// uncommittedAdd returns one of Alice's HTLCs that isn't in her current
// commitment.
func uncommittedAdd(l Ledger) (uint64, bool) {
	for _, e := range l.Logs.Local.Entries {
		if e.Kind == KindAdd && !l.Committed(e, lntypes.Local) {
			return e.HtlcIndex, true
		}
	}

	return 0, false
}

// committedAdd returns one of Alice's HTLCs in her current commitment that
// Bob hasn't removed.
func committedAdd(l Ledger) (uint64, bool) {
	for _, e := range l.Logs.Local.Entries {
		if e.Kind == KindAdd && l.Committed(e, lntypes.Local) &&
			!l.Logs.Local.modified(e.HtlcIndex) {

			return e.HtlcIndex, true
		}
	}

	return 0, false
}

// removedAdd returns one of Alice's HTLCs Bob already removed.
func removedAdd(l Ledger) (uint64, bool) {
	if len(l.Logs.Local.Modified) == 0 {
		return 0, false
	}

	return l.Logs.Local.Modified[0], true
}

// injections are the byzantine messages the test can inject. The premature
// revocation, the case the state machine exists to rule out, is listed
// several times so runs reach it often.
var injections = []string{
	"premature-raa", "premature-raa", "premature-raa", "premature-raa",
	"replay-raa", "add-skip-id", "settle-unknown",
	"settle-early", "fail-early", "malform-early", "settle-bad-preimage",
	"double-removal", "fee-non-initiator", "malform-no-badonion",
	"bad-commit-sig", "reestablish-future", "reestablish-future",
	"reestablish-future", "reestablish-lost", "reestablish-lost",
	"reestablish-lost",
}

// run runs one random differential execution.
func (r *diffRun) run() {
	rt := r.rt
	// A byzantine run injects messages after an honest prefix. An
	// injection is the last step whenever either implementation refuses
	// it.
	byzantine := rapid.Bool().Draw(rt, "byzantine")

	// lnd loads a channel from disk before every connection, the first
	// one included, and every connection starts with channel_reestablish.
	if !r.reconnect(nil) {
		return
	}

	reconnects := 0
	steps := rapid.IntRange(1, 120).Draw(rt, "steps")
	injectAt := -1
	if byzantine {
		injectAt = rapid.IntRange(steps/3, steps).Draw(rt, "inject-at")
	}
	for i := 0; i <= steps; i++ {
		// A byzantine run injects exactly once, at a drawn step.
		if i == injectAt {
			kind := rapid.SampledFrom(injections).Draw(
				rt, "injection",
			)
			if !r.inject(kind) || r.old.bob.failed != nil {
				return
			}

			continue
		}
		if i == steps {
			break
		}

		actions := []string{
			"alice", "alice", "bob", "bob", "to-alice", "to-alice",
			"to-alice", "to-bob", "to-bob", "to-bob",
		}
		if reconnects < 3 {
			actions = append(actions, "reconnect")
		}
		actions = append(actions, "flush", "flush")

		var ok bool
		action := rapid.SampledFrom(actions).Draw(rt, "action")
		switch action {
		case "alice":
			ok = r.aliceCommand(rapid.SampledFrom([]string{
				"add", "add", "settle", "settle", "fail",
				"malform", "fee", "sign", "sign",
			}).Draw(rt, "alice-cmd"))

		case "bob":
			ok = r.bobCommand(rapid.SampledFrom([]string{
				"add", "add", "settle", "settle", "settle",
				"fail", "malform", "sign", "sign",
			}).Draw(rt, "bob-cmd"))

		case "to-alice":
			ok = r.deliver(true)

		case "to-bob":
			ok = r.deliver(false)

		case "reconnect":
			reconnects++
			ok = r.reconnect(nil)

		// A flush lets the protocol run until quiet, so that HTLCs
		// lock in often enough for reconnections to find them.
		case "flush":
			ok = r.flush()
		}

		if !ok || r.old.bob.failed != nil {
			return
		}
	}

	// Let the honest protocol run to quiescence in both worlds: the
	// worlds must still agree, and nothing may be left unsigned. On the
	// way, Bob resolves every HTLC of Alice's he may, alternating settles
	// and fails, so that every run that gets here forwards both kinds of
	// removal back to Alice.
	for round := 0; round < 20; round++ {
		if round == 2 {
			r.bobResolveAll()
		}
		if !r.aliceCommand("sign") {
			return
		}
		r.bobCommand("sign")
		for len(r.old.toAlice)+len(r.old.toBob) > 0 {
			if !r.deliver(true) || !r.deliver(false) {
				return
			}
		}
	}
	r.stats.hit("quiesced")
	require.False(rt, r.shadow().OweCommitment(lntypes.Local))
}

// TestDifferentialActorVsLegacy is the differential test. Beyond each run's
// own checks, it insists that every step kind and every safer rule was
// exercised, so it can't pass by never reaching them.
func TestDifferentialActorVsLegacy(t *testing.T) {
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
			rapid.Check(t, func(rt *rapid.T) {
				newDiffRun(t, rt, chanType, &stats).run()
			})

			for k, v := range stats.counts {
				t.Logf("%-50s %d", k, v)
			}

			// Coverage floors only make sense for a run long
			// enough to reach them.
			if testing.Short() {
				return
			}
			for _, must := range []string{
				"safer:refused without changing the channel",
				"safer:removal of an uncommitted HTLC refused",
				"safer:local removal before lock-in refused",
				"forward:Add", "forward:Settle", "forward:Fail",
				"agree:deliver-to-alice", "quiesced",
				"agree:connect",
			} {
				require.Positive(t, stats.counts[must],
					"never exercised %q", must)
			}
		})
	}
}

// TestDifferentialInjections runs every byzantine injection in its own
// short runs, after an honest prefix that locks HTLCs in, so each is
// exercised on every run of the test rather than by chance.
func TestDifferentialInjections(t *testing.T) {
	t.Parallel()

	chanType := channeldb.SingleFunderTweaklessBit |
		channeldb.AnchorOutputsBit

	kinds := slices.Compact(slices.Sorted(slices.Values(injections)))
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()

			var (
				stats diffStats
				runs  atomic.Int32
			)
			rapid.Check(t, func(rt *rapid.T) {
				if runs.Add(1) > 10 {
					return
				}
				r := newDiffRun(t, rt, chanType, &stats)
				if !r.reconnect(nil) {
					return
				}
				for i := 0; i < 3; i++ {
					if !r.aliceCommand("add") ||
						!r.bobCommand("add") ||
						!r.flush() {

						return
					}
				}

				// Bob fails one of Alice's HTLCs, which Alice
				// receives but hasn't committed, for the
				// injections that need a removal outstanding.
				if !r.bobCommand("fail") || !r.deliver(true) {
					return
				}
				r.inject(kind)
			})
			require.Positive(t, stats.counts["inject:"+kind],
				"never injected %q", kind)
		})
	}
}

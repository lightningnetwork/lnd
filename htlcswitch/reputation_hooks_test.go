package htlcswitch

import (
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// mockReputationManager is a stub ReputationManager that records the hook calls
// the switch makes, used to assert the read-only reputation seam fires exactly
// once per forward/settle/fail with the correct circuit keys.
type mockReputationManager struct {
	mu       sync.Mutex
	forwards []repForward
	settles  []repResolve
	fails    []repResolve
}

type repForward struct {
	in, out       CircuitKey
	inAmt, outAmt lnwire.MilliSatoshi
	advertisedFee lnwire.MilliSatoshi
	cltv          uint32
	accountable   bool
}

type repResolve struct {
	in, out CircuitKey
}

func (r *mockReputationManager) OnForward(in, out CircuitKey, inAmt,
	outAmt, advertisedFee lnwire.MilliSatoshi, cltv uint32,
	accountable bool) {

	r.mu.Lock()
	defer r.mu.Unlock()
	r.forwards = append(r.forwards, repForward{
		in: in, out: out, inAmt: inAmt, outAmt: outAmt,
		advertisedFee: advertisedFee, cltv: cltv,
		accountable: accountable,
	})
}

func (r *mockReputationManager) OnSettle(in, out CircuitKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settles = append(r.settles, repResolve{in: in, out: out})
}

func (r *mockReputationManager) OnFail(in, out CircuitKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fails = append(r.fails, repResolve{in: in, out: out})
}

func (r *mockReputationManager) snapshot() ([]repForward, []repResolve,
	[]repResolve) {

	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]repForward(nil), r.forwards...),
		append([]repResolve(nil), r.settles...),
		append([]repResolve(nil), r.fails...)
}

// newReputationTestSwitch builds a switch with the given (possibly nil)
// reputation manager wired in, plus two linked mock channels (alice -> bob).
func newReputationTestSwitch(t *testing.T, repMgr ReputationManager) (*Switch,
	*mockChannelLink, *mockChannelLink) {

	t.Helper()

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create alice server: %v", err)
	}
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create bob server: %v", err)
	}

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}

	// Wire the reputation manager into the switch config before starting.
	s.cfg.ReputationManager = repMgr

	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	chanID1, chanID2, aliceChanID, bobChanID := genIDs()

	aliceLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)
	bobLink := newMockChannelLink(
		s, chanID2, bobChanID, emptyScid, bobPeer, true, false, false,
		false,
	)
	if err := s.AddLink(aliceLink); err != nil {
		t.Fatalf("unable to add alice link: %v", err)
	}
	if err := s.AddLink(bobLink); err != nil {
		t.Fatalf("unable to add bob link: %v", err)
	}

	return s, aliceLink, bobLink
}

// TestSwitchReputationForwardSettle asserts that forwarding then settling an
// HTLC fires OnForward and OnSettle exactly once with the correct circuit keys.
func TestSwitchReputationForwardSettle(t *testing.T) {
	t.Parallel()

	repMgr := &mockReputationManager{}
	s, aliceLink, bobLink := newReputationTestSwitch(t, repMgr)

	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := sha256.Sum256(preimage[:])

	addPkt := &htlcPacket{
		incomingChanID: aliceLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}
	if err := s.ForwardPackets(nil, addPkt); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bobLink.packets:
		if err := bobLink.completeCircuit(addPkt); err != nil {
			t.Fatalf("unable to complete circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("add was not propagated to destination")
	}

	forwards, _, _ := repMgr.snapshot()
	if len(forwards) != 1 {
		t.Fatalf("expected 1 OnForward, got %d", len(forwards))
	}
	if forwards[0].in.ChanID != aliceLink.ShortChanID() ||
		forwards[0].out.ChanID != bobLink.ShortChanID() {

		t.Fatalf("OnForward wrong keys: in=%v out=%v",
			forwards[0].in, forwards[0].out)
	}

	settlePkt := &htlcPacket{
		outgoingChanID: bobLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: preimage,
		},
	}
	if err := s.ForwardPackets(nil, settlePkt); err != nil {
		t.Fatal(err)
	}

	select {
	case pkt := <-aliceLink.packets:
		if err := aliceLink.deleteCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("settle was not propagated upstream")
	}

	_, settles, fails := repMgr.snapshot()
	if len(settles) != 1 {
		t.Fatalf("expected 1 OnSettle, got %d", len(settles))
	}
	if settles[0].in.ChanID != aliceLink.ShortChanID() {
		t.Fatalf("OnSettle wrong incoming key: %v", settles[0].in)
	}
	if len(fails) != 0 {
		t.Fatalf("expected 0 OnFail, got %d", len(fails))
	}
}

// TestSwitchReputationForwardFail asserts that forwarding then failing an HTLC
// fires OnForward and OnFail (not OnSettle).
func TestSwitchReputationForwardFail(t *testing.T) {
	t.Parallel()

	repMgr := &mockReputationManager{}
	s, aliceLink, bobLink := newReputationTestSwitch(t, repMgr)

	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := sha256.Sum256(preimage[:])

	addPkt := &htlcPacket{
		incomingChanID: aliceLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}
	if err := s.ForwardPackets(nil, addPkt); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bobLink.packets:
		if err := bobLink.completeCircuit(addPkt); err != nil {
			t.Fatalf("unable to complete circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("add was not propagated to destination")
	}

	failPkt := &htlcPacket{
		outgoingChanID: bobLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc:           &lnwire.UpdateFailHTLC{},
	}
	if err := s.ForwardPackets(nil, failPkt); err != nil {
		t.Fatal(err)
	}

	select {
	case pkt := <-aliceLink.packets:
		if err := aliceLink.deleteCircuit(pkt); err != nil {
			t.Fatalf("unable to remove circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fail was not propagated upstream")
	}

	forwards, settles, fails := repMgr.snapshot()
	if len(forwards) != 1 {
		t.Fatalf("expected 1 OnForward, got %d", len(forwards))
	}
	if len(fails) != 1 {
		t.Fatalf("expected 1 OnFail, got %d", len(fails))
	}
	if fails[0].in.ChanID != aliceLink.ShortChanID() {
		t.Fatalf("OnFail wrong incoming key: %v", fails[0].in)
	}
	if len(settles) != 0 {
		t.Fatalf("expected 0 OnSettle, got %d", len(settles))
	}
}

// panicReputationManager is a stub whose hooks always panic, used to prove the
// switch's forwarding path survives a misbehaving (buggy) reputation
// subsystem. It also counts calls so we can assert the guard self-disables.
type panicReputationManager struct {
	mu    sync.Mutex
	calls int
}

func (p *panicReputationManager) bump() {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
}

func (p *panicReputationManager) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls
}

func (p *panicReputationManager) OnForward(_, _ CircuitKey, _,
	_, _ lnwire.MilliSatoshi, _ uint32, _ bool) {

	p.bump()
	panic("boom from OnForward")
}

func (p *panicReputationManager) OnSettle(_, _ CircuitKey) {
	p.bump()
	panic("boom from OnSettle")
}

func (p *panicReputationManager) OnFail(_, _ CircuitKey) {
	p.bump()
	panic("boom from OnFail")
}

// mustNotPanic fails the test if fn panics (the guard should absorb it).
func mustNotPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic escaped the reputation guard: %v", r)
		}
	}()
	fn()
}

// TestGuardedReputationManagerRecovers asserts that the panic boundary around
// the reputation hooks (NewGuardedReputationManager) swallows a hook panic and
// permanently disables the subsystem (fail open) — so a bug in the log-only
// subsystem can never propagate to the caller. This is the unit-level proof;
// TestSwitchReputationPanicSurvives drives it through the live switch.
func TestGuardedReputationManagerRecovers(t *testing.T) {
	t.Parallel()

	inner := &panicReputationManager{}
	guard := NewGuardedReputationManager(inner)

	in := CircuitKey{ChanID: lnwire.NewShortChanIDFromInt(1)}
	out := CircuitKey{ChanID: lnwire.NewShortChanIDFromInt(2)}

	// The first call panics internally; the guard must recover so the
	// caller's goroutine is unaffected.
	mustNotPanic(t, func() {
		guard.OnForward(in, out, 1, 1, 0, 100, false)
	})
	if inner.callCount() != 1 {
		t.Fatalf("inner should have been called once, got %d",
			inner.callCount())
	}

	// After a panic the subsystem is disabled: subsequent calls short-
	// circuit without ever reaching the (panicking) inner manager.
	mustNotPanic(t, func() {
		guard.OnForward(in, out, 1, 1, 0, 100, false)
		guard.OnSettle(in, out)
		guard.OnFail(in, out)
	})
	if inner.callCount() != 1 {
		t.Fatalf("inner must not be called again once disabled, got %d",
			inner.callCount())
	}
}

// TestSwitchReputationPanicSurvives drives a forward through a live switch
// whose reputation manager panics in OnForward, and asserts the HTLC is still
// forwarded to the destination — i.e. a subsystem panic cannot take down the
// switch's forwarding goroutine.
func TestSwitchReputationPanicSurvives(t *testing.T) {
	t.Parallel()

	guard := NewGuardedReputationManager(&panicReputationManager{})
	s, aliceLink, bobLink := newReputationTestSwitch(t, guard)

	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := sha256.Sum256(preimage[:])

	addPkt := &htlcPacket{
		incomingChanID: aliceLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}
	if err := s.ForwardPackets(nil, addPkt); err != nil {
		t.Fatal(err)
	}

	// Despite OnForward panicking, the HTLC must still reach the
	// destination link — forwarding is unaffected.
	select {
	case <-bobLink.packets:
		if err := bobLink.completeCircuit(addPkt); err != nil {
			t.Fatalf("unable to complete circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("add was not propagated despite reputation panic")
	}
}

// TestSwitchReputationLocalSendSkipped asserts that a locally-originated HTLC
// (this node is the payment source) does NOT invoke the reputation hooks: only
// genuine forwards are observed. A false trigger here would pollute reputation
// with the node's own payments.
func TestSwitchReputationLocalSendSkipped(t *testing.T) {
	t.Parallel()

	repMgr := &mockReputationManager{}

	peer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	if err != nil {
		t.Fatalf("unable to create server: %v", err)
	}

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	if err != nil {
		t.Fatalf("unable to init switch: %v", err)
	}
	s.cfg.ReputationManager = repMgr
	if err := s.Start(); err != nil {
		t.Fatalf("unable to start switch: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	chanID, _, aliceChanID, _ := genIDs()
	link := newMockChannelLink(
		s, chanID, aliceChanID, emptyScid, peer, true, false, false,
		true,
	)
	if err := s.AddLink(link); err != nil {
		t.Fatalf("unable to add link: %v", err)
	}

	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := sha256.Sum256(preimage[:])

	// SendHTLC originates a payment from this node (incoming chan is
	// hop.Source), so it must not be treated as a forward.
	htlc := &lnwire.UpdateAddHTLC{PaymentHash: rhash, Amount: 1}
	if err := s.SendHTLC(link.ShortChanID(), 0, htlc); err != nil {
		t.Fatalf("unable to send local htlc: %v", err)
	}

	// Drain the add from the outgoing link so it is actually dispatched.
	select {
	case <-link.packets:
	case <-time.After(time.Second):
		t.Fatal("local add was not dispatched")
	}

	forwards, settles, fails := repMgr.snapshot()
	if len(forwards) != 0 {
		t.Fatalf("local send must not trigger OnForward, got %d",
			len(forwards))
	}
	if len(settles) != 0 || len(fails) != 0 {
		t.Fatalf("local send must not trigger resolutions, got "+
			"%d settles %d fails", len(settles), len(fails))
	}
}

// TestSwitchReputationNilManagerNoop asserts that with no reputation manager
// configured (the default), forwarding works and nothing panics — i.e. the
// hooks are safely skipped.
func TestSwitchReputationNilManagerNoop(t *testing.T) {
	t.Parallel()

	s, aliceLink, bobLink := newReputationTestSwitch(t, nil)

	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := sha256.Sum256(preimage[:])

	addPkt := &htlcPacket{
		incomingChanID: aliceLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bobLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}
	if err := s.ForwardPackets(nil, addPkt); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bobLink.packets:
		if err := bobLink.completeCircuit(addPkt); err != nil {
			t.Fatalf("unable to complete circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("add was not propagated with nil reputation manager")
	}
}

// accountableAddPkt builds a forwarding add packet from alice to bob whose
// incoming HTLC carries the experimental accountable bit set.
func accountableAddPkt(t *testing.T, alice,
	bob *mockChannelLink) *htlcPacket {

	t.Helper()

	preimage, err := genPreimage()
	if err != nil {
		t.Fatalf("unable to generate preimage: %v", err)
	}
	rhash := sha256.Sum256(preimage[:])

	return &htlcPacket{
		incomingChanID: alice.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: bob.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
			CustomRecords: lnwire.CustomRecords{
				uint64(lnwire.ExperimentalAccountableType): {
					lnwire.ExperimentalAccountable,
				},
			},
		},
	}
}

// TestSwitchReputationAdvertisedFee asserts that the switch feeds the outgoing
// link's ADVERTISED fee (not the offered in-out delta) to OnForward.
func TestSwitchReputationAdvertisedFee(t *testing.T) {
	t.Parallel()

	repMgr := &mockReputationManager{}
	s, aliceLink, bobLink := newReputationTestSwitch(t, repMgr)

	// The outgoing (bob) link advertises a fee distinct from any in-out
	// delta so we can prove the switch sources the advertised value.
	const wantFee = lnwire.MilliSatoshi(4242)
	bobLink.advertisedFee = wantFee

	addPkt := accountableAddPkt(t, aliceLink, bobLink)
	if err := s.ForwardPackets(nil, addPkt); err != nil {
		t.Fatal(err)
	}

	select {
	case <-bobLink.packets:
		if err := bobLink.completeCircuit(addPkt); err != nil {
			t.Fatalf("unable to complete circuit: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("add was not propagated to destination")
	}

	forwards, _, _ := repMgr.snapshot()
	if len(forwards) != 1 {
		t.Fatalf("expected 1 OnForward, got %d", len(forwards))
	}
	if forwards[0].advertisedFee != wantFee {
		t.Fatalf("advertised fee: got %d, want %d",
			forwards[0].advertisedFee, wantFee)
	}
}

// TestSwitchReputationAccountabilityGating asserts that the outgoing
// accountable bit fed to OnForward is derived the way the outgoing link derives
// it: even when the incoming HTLC is accountable, a node that does not forward
// the experimental accountability signal reports the forward as unaccountable.
func TestSwitchReputationAccountabilityGating(t *testing.T) {
	t.Parallel()

	t.Run("forwarded when enabled", func(t *testing.T) {
		t.Parallel()

		repMgr := &mockReputationManager{}
		s, aliceLink, bobLink := newReputationTestSwitch(t, repMgr)
		s.cfg.ShouldFwdExpAccountability = func() bool { return true }

		addPkt := accountableAddPkt(t, aliceLink, bobLink)
		if err := s.ForwardPackets(nil, addPkt); err != nil {
			t.Fatal(err)
		}

		select {
		case <-bobLink.packets:
			if err := bobLink.completeCircuit(addPkt); err != nil {
				t.Fatalf("complete circuit: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("add was not propagated to destination")
		}

		forwards, _, _ := repMgr.snapshot()
		if len(forwards) != 1 {
			t.Fatalf("expected 1 OnForward, got %d", len(forwards))
		}
		if !forwards[0].accountable {
			t.Fatalf("expected accountable=true when enabled")
		}
	})

	t.Run("gated off when disabled", func(t *testing.T) {
		t.Parallel()

		repMgr := &mockReputationManager{}
		s, aliceLink, bobLink := newReputationTestSwitch(t, repMgr)
		s.cfg.ShouldFwdExpAccountability = func() bool { return false }

		addPkt := accountableAddPkt(t, aliceLink, bobLink)
		if err := s.ForwardPackets(nil, addPkt); err != nil {
			t.Fatal(err)
		}

		select {
		case <-bobLink.packets:
			if err := bobLink.completeCircuit(addPkt); err != nil {
				t.Fatalf("complete circuit: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("add was not propagated to destination")
		}

		forwards, _, _ := repMgr.snapshot()
		if len(forwards) != 1 {
			t.Fatalf("expected 1 OnForward, got %d", len(forwards))
		}
		if forwards[0].accountable {
			t.Fatalf("expected accountable=false when gated off")
		}
	})
}

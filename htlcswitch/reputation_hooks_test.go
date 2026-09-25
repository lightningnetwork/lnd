package htlcswitch

import (
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
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
	in            CircuitKey
	out           lnwire.ShortChannelID
	inAmt, outAmt lnwire.MilliSatoshi
	advertisedFee lnwire.MilliSatoshi
	cltv          uint32
	height        uint32
	accountable   bool
}

type repResolve struct {
	in CircuitKey
}

func (r *mockReputationManager) OnForward(in CircuitKey,
	out lnwire.ShortChannelID, inAmt, outAmt,
	advertisedFee lnwire.MilliSatoshi, cltv, height uint32,
	accountable bool) {

	r.mu.Lock()
	defer r.mu.Unlock()
	r.forwards = append(r.forwards, repForward{
		in: in, out: out, inAmt: inAmt, outAmt: outAmt,
		advertisedFee: advertisedFee, cltv: cltv, height: height,
		accountable: accountable,
	})
}

func (r *mockReputationManager) OnSettle(in CircuitKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settles = append(r.settles, repResolve{in: in})
}

func (r *mockReputationManager) OnFail(in CircuitKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fails = append(r.fails, repResolve{in: in})
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
		forwards[0].out != bobLink.ShortChanID() {

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
	require.Len(t, settles, 1, "expected exactly 1 OnSettle")
	require.Equal(t, aliceLink.ShortChanID(), settles[0].in.ChanID,
		"OnSettle wrong incoming key")
	require.Empty(t, fails, "expected 0 OnFail")
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
	require.Len(t, forwards, 1, "expected exactly 1 OnForward")
	require.Len(t, fails, 1, "expected exactly 1 OnFail")
	require.Equal(t, aliceLink.ShortChanID(), fails[0].in.ChanID,
		"OnFail wrong incoming key")
	require.Empty(t, settles, "expected 0 OnSettle")
}

// panicReputationManager is a stub whose hooks always panic, used to prove the
// switch's forwarding path survives a misbehaving (buggy) reputation
// subsystem. It also counts calls, so a test can assert that the guard
// still forwards every hook to it.
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

func (p *panicReputationManager) OnForward(_ CircuitKey,
	_ lnwire.ShortChannelID, _, _, _ lnwire.MilliSatoshi, _, _ uint32,
	_ bool) {

	p.bump()
	panic("boom from OnForward")
}

func (p *panicReputationManager) OnSettle(_ CircuitKey) {
	p.bump()
	panic("boom from OnSettle")
}

func (p *panicReputationManager) OnFail(_ CircuitKey) {
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
// the reputation hooks (NewGuardedReputationManager) swallows a hook panic, so
// that a bug in the log-only subsystem can never propagate to the caller. This
// is the unit-level proof; TestSwitchReputationPanicSurvives drives it through
// the live switch.
func TestGuardedReputationManagerRecovers(t *testing.T) {
	t.Parallel()

	inner := &panicReputationManager{}
	guard := NewGuardedReputationManager(inner)

	in := CircuitKey{ChanID: lnwire.NewShortChanIDFromInt(1)}
	out := lnwire.NewShortChanIDFromInt(2)

	// Every hook panics internally; the guard must recover each time so the
	// caller's goroutine is unaffected.
	mustNotPanic(t, func() {
		guard.OnForward(in, out, 1, 1, 0, 100, 90, false)
		guard.OnSettle(in)
		guard.OnFail(in)
	})
	if inner.callCount() != 3 {
		t.Fatalf("every hook should have reached the inner manager, "+
			"got %d calls", inner.callCount())
	}
}

// TestSwitchReputationPanicSurvives drives a forward through a live switch
// whose reputation manager panics in OnForward, and asserts the HTLC is still
// forwarded to the destination, i.e. a subsystem panic cannot take down the
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
	// destination link, so forwarding is unaffected.
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
// configured (the default), forwarding works and nothing panics, i.e. the
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

// TestSwitchReputationAdvertisedFee asserts that the switch feeds the fee the
// node ADVERTISED for the forward (not the offered in-out delta) to OnForward:
// the outgoing link's outbound fee plus the incoming link's inbound fee,
// clamped at zero when an inbound discount pushes the total negative.
func TestSwitchReputationAdvertisedFee(t *testing.T) {
	t.Parallel()

	const outFee = lnwire.MilliSatoshi(4242)

	tests := []struct {
		name       string
		inboundFee models.InboundFee
		wantFee    lnwire.MilliSatoshi
	}{{
		// No inbound fee: only the outgoing link's advertised fee.
		name:    "outbound only",
		wantFee: outFee,
	}, {
		// A positive inbound fee is added on top.
		name:       "inbound fee added",
		inboundFee: models.InboundFee{Base: 100},
		wantFee:    outFee + 100,
	}, {
		// An inbound discount larger than the outbound fee clamps the
		// total at zero rather than going negative.
		name:       "inbound discount clamped",
		inboundFee: models.InboundFee{Base: -5000},
		wantFee:    0,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			repMgr := &mockReputationManager{}
			s, aliceLink, bobLink := newReputationTestSwitch(
				t, repMgr,
			)

			// The outgoing (bob) link advertises a fee distinct
			// from any in-out delta so we can prove the switch
			// sources the advertised value.
			bobLink.advertisedFee = outFee

			// The incoming link stamps its inbound fee schedule
			// onto the packet; mirror that here.
			addPkt := accountableAddPkt(t, aliceLink, bobLink)
			addPkt.inboundFee = test.inboundFee

			if err := s.ForwardPackets(nil, addPkt); err != nil {
				t.Fatal(err)
			}

			select {
			case <-bobLink.packets:
				err := bobLink.completeCircuit(addPkt)
				if err != nil {
					t.Fatalf("unable to complete "+
						"circuit: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("add was not propagated to " +
					"destination")
			}

			forwards, _, _ := repMgr.snapshot()
			if len(forwards) != 1 {
				t.Fatalf("expected 1 OnForward, got %d",
					len(forwards))
			}
			if forwards[0].advertisedFee != test.wantFee {
				t.Fatalf("advertised fee: got %d, want %d",
					forwards[0].advertisedFee,
					test.wantFee)
			}
		})
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

// TestSwitchReputationNonStrictForward asserts that when the switch forwards an
// HTLC over a channel other than the one the sender asked for (non-strict
// forwarding, where the requested link cannot take the HTLC but another link to
// the same peer can), the reputation manager is told about the channel the HTLC
// actually went out on, and the resolution reports that same channel.
//
// This matters because the manager records the pending HTLC against the
// channel reported at forward time: reporting the requested channel here would
// attribute the HTLC's reputation and in-flight risk to a channel it never
// went out on.
func TestSwitchReputationNonStrictForward(t *testing.T) {
	t.Parallel()

	repMgr := &mockReputationManager{}

	alicePeer, err := newMockServer(
		t, "alice", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create alice server")
	bobPeer, err := newMockServer(
		t, "bob", testStartingHeight, nil, testDefaultDelta,
	)
	require.NoError(t, err, "unable to create bob server")

	s, err := initSwitchWithTempDB(t, testStartingHeight)
	require.NoError(t, err, "unable to init switch")

	s.cfg.ReputationManager = repMgr
	require.NoError(t, s.Start())
	t.Cleanup(func() { _ = s.Stop() })

	chanID1, aliceChanID := genID()
	aliceLink := newMockChannelLink(
		s, chanID1, aliceChanID, emptyScid, alicePeer, true, false,
		false, false,
	)

	// Bob has two channels with us. The first is the one the sender asked
	// for, but it cannot take the HTLC, so the switch must fall back to the
	// second one.
	chanID2, bobChanID1 := genID()
	requestedLink := newMockChannelLink(
		s, chanID2, bobChanID1, emptyScid, bobPeer, true, false, false,
		false,
	)
	requestedLink.checkHtlcForwardResult = NewDetailedLinkError(
		lnwire.NewTemporaryChannelFailure(nil),
		OutgoingFailureInsufficientBalance,
	)

	chanID3, bobChanID2 := genID()
	chosenLink := newMockChannelLink(
		s, chanID3, bobChanID2, emptyScid, bobPeer, true, false, false,
		false,
	)

	require.NoError(t, s.AddLink(aliceLink))
	require.NoError(t, s.AddLink(requestedLink))
	require.NoError(t, s.AddLink(chosenLink))

	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
	rhash := sha256.Sum256(preimage[:])

	// The packet asks for Bob's first channel, which cannot forward it.
	addPkt := &htlcPacket{
		incomingChanID: aliceLink.ShortChanID(),
		incomingHTLCID: 0,
		outgoingChanID: requestedLink.ShortChanID(),
		obfuscator:     NewMockObfuscator(),
		htlc: &lnwire.UpdateAddHTLC{
			PaymentHash: rhash,
			Amount:      1,
		},
	}
	require.NoError(t, s.ForwardPackets(nil, addPkt))

	select {
	case <-chosenLink.packets:
		require.NoError(t, chosenLink.completeCircuit(addPkt))

	case <-requestedLink.packets:
		t.Fatal("htlc went out on the link that cannot forward it")

	case <-time.After(time.Second):
		t.Fatal("add was not propagated to destination")
	}

	// The forward must be reported against the channel actually used, not
	// the one the sender requested.
	forwards, _, _ := repMgr.snapshot()
	require.Len(t, forwards, 1)
	require.Equal(t, chosenLink.ShortChanID(), forwards[0].out,
		"forward must report the chosen outgoing channel")

	settlePkt := &htlcPacket{
		outgoingChanID: chosenLink.ShortChanID(),
		outgoingHTLCID: 0,
		amount:         1,
		htlc: &lnwire.UpdateFulfillHTLC{
			PaymentPreimage: preimage,
		},
	}
	require.NoError(t, s.ForwardPackets(nil, settlePkt))

	select {
	case pkt := <-aliceLink.packets:
		require.NoError(t, aliceLink.deleteCircuit(pkt))

	case <-time.After(time.Second):
		t.Fatal("settle was not propagated upstream")
	}

	// The resolution matches the add by its incoming circuit key.
	_, settles, _ := repMgr.snapshot()
	require.Len(t, settles, 1)
	require.Equal(t, forwards[0].in, settles[0].in,
		"resolve must report the same incoming circuit as the forward")
}

// TestSwitchReputationMailboxFailAdd asserts that an add failed back through
// the outgoing link's mailbox (mailbox.FailAdd) still reaches the reputation
// manager as a fail with the HTLC's incoming circuit key.
//
// This is the path taken when the outgoing link cannot commit the HTLC
// (channel.AddHTLC fails, the link is flushing, fee exposure is hit) or the
// mailbox delivery deadline elapses. No keystone was ever set for the circuit
// and the mailbox builds a fresh fail packet that carries no outgoing scid, so
// this resolution path cannot name the outgoing channel; the manager must be
// able to match the fail to the forward by circuit key alone.
func TestSwitchReputationMailboxFailAdd(t *testing.T) {
	t.Parallel()

	repMgr := &mockReputationManager{}
	s, aliceLink, bobLink := newReputationTestSwitch(t, repMgr)

	preimage, err := genPreimage()
	require.NoError(t, err, "unable to generate preimage")
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
	require.NoError(t, s.ForwardPackets(nil, addPkt))

	// Take the packet out of the outgoing link's mailbox, but do NOT call
	// completeCircuit: that is what sets the keystone, and the whole point
	// of this path is that the add fails before a keystone exists.
	var queued *htlcPacket
	select {
	case queued = <-bobLink.packets:
	case <-time.After(time.Second):
		t.Fatal("add was not propagated to the destination link")
	}

	forwards, _, _ := repMgr.snapshot()
	require.Len(t, forwards, 1)

	// The outgoing link cannot add the HTLC to its commitment, so it fails
	// the add back through the mailbox.
	bobLink.mailBox.FailAdd(queued)

	select {
	case <-aliceLink.packets:
	case <-time.After(time.Second):
		t.Fatal("fail was not propagated upstream")
	}

	// The fail must reach the manager with the same incoming circuit key
	// as the forward, so the pending HTLC it recorded can be resolved.
	require.Eventually(t, func() bool {
		_, _, fails := repMgr.snapshot()

		return len(fails) == 1
	}, 2*time.Second, 10*time.Millisecond)

	_, _, fails := repMgr.snapshot()
	require.Equal(t, forwards[0].in, fails[0].in,
		"mailbox fail must resolve the same incoming circuit as the "+
			"forward")
}

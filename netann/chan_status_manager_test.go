package netann_test

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
)

// randOutpoint creates a random wire.Outpoint.
func randOutpoint(t *testing.T) wire.OutPoint {
	t.Helper()

	var buf [36]byte
	_, err := io.ReadFull(rand.Reader, buf[:])
	if err != nil {
		t.Fatalf("unable to generate random outpoint: %v", err)
	}

	op := wire.OutPoint{}
	copy(op.Hash[:], buf[:32])
	op.Index = binary.BigEndian.Uint32(buf[32:])

	return op
}

var shortChanIDs uint64

// createChannel generates a channeldb.OpenChannel with a random chanpoint and
// short channel id.
func createChannel(t *testing.T) *channeldb.OpenChannel {
	t.Helper()

	sid := atomic.AddUint64(&shortChanIDs, 1)

	return &channeldb.OpenChannel{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(sid),
		ChannelFlags:    lnwire.FFAnnounceChannel,
		FundingOutpoint: randOutpoint(t),
	}
}

// createEdgePolicies generates an edge info and two directional edge policies.
// The remote party's public key is generated randomly, and then sorted against
// our `pubkey` with the direction bit set appropriately in the policies. Our
// update will be created with the disabled bit set if startEnabled is false.
func createEdgePolicies(t *testing.T, channel *channeldb.OpenChannel,
	pubkey *btcec.PublicKey, startEnabled bool) (*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy, *channeldb.ChannelEdgePolicy) {

	var (
		pubkey1 [33]byte
		pubkey2 [33]byte
		dir1    lnwire.ChanUpdateChanFlags
		dir2    lnwire.ChanUpdateChanFlags
	)

	// Set pubkey1 to OUR pubkey.
	copy(pubkey1[:], pubkey.SerializeCompressed())

	// Set the disabled bit appropriately on our update.
	if !startEnabled {
		dir1 |= lnwire.ChanUpdateDisabled
	}

	// Generate and set pubkey2 for THEIR pubkey.
	privKey2, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate key pair: %v", err)
	}
	copy(pubkey2[:], privKey2.PubKey().SerializeCompressed())

	// Set pubkey1 to the lower of the two pubkeys.
	if bytes.Compare(pubkey2[:], pubkey1[:]) < 0 {
		pubkey1, pubkey2 = pubkey2, pubkey1
		dir1, dir2 = dir2, dir1
	}

	// Now that the ordering has been established, set pubkey2's direction
	// bit.
	dir2 |= lnwire.ChanUpdateDirection

	return &channeldb.ChannelEdgeInfo{
			ChannelPoint:  channel.FundingOutpoint,
			NodeKey1Bytes: pubkey1,
			NodeKey2Bytes: pubkey2,
		},
		&channeldb.ChannelEdgePolicy{
			ChannelID:    channel.ShortChanID().ToUint64(),
			ChannelFlags: dir1,
			LastUpdate:   time.Now(),
			SigBytes:     make([]byte, 64),
		},
		&channeldb.ChannelEdgePolicy{
			ChannelID:    channel.ShortChanID().ToUint64(),
			ChannelFlags: dir2,
			LastUpdate:   time.Now(),
			SigBytes:     make([]byte, 64),
		}
}

type mockGraph struct {
	mu        sync.Mutex
	channels  []*channeldb.OpenChannel
	chanInfos map[wire.OutPoint]*channeldb.ChannelEdgeInfo
	chanPols1 map[wire.OutPoint]*channeldb.ChannelEdgePolicy
	chanPols2 map[wire.OutPoint]*channeldb.ChannelEdgePolicy
	sidToCid  map[lnwire.ShortChannelID]wire.OutPoint

	updates chan *lnwire.ChannelUpdate
}

func newMockGraph(t *testing.T, numChannels int,
	startActive, startEnabled bool, pubKey *btcec.PublicKey) *mockGraph {

	g := &mockGraph{
		channels:  make([]*channeldb.OpenChannel, 0, numChannels),
		chanInfos: make(map[wire.OutPoint]*channeldb.ChannelEdgeInfo),
		chanPols1: make(map[wire.OutPoint]*channeldb.ChannelEdgePolicy),
		chanPols2: make(map[wire.OutPoint]*channeldb.ChannelEdgePolicy),
		sidToCid:  make(map[lnwire.ShortChannelID]wire.OutPoint),
		updates:   make(chan *lnwire.ChannelUpdate, 2*numChannels),
	}

	for i := 0; i < numChannels; i++ {
		c := createChannel(t)

		info, pol1, pol2 := createEdgePolicies(
			t, c, pubKey, startEnabled,
		)

		g.addChannel(c)
		g.addEdgePolicy(c, info, pol1, pol2)
	}

	return g
}

func (g *mockGraph) FetchAllOpenChannels() ([]*channeldb.OpenChannel, error) {
	return g.chans(), nil
}

func (g *mockGraph) FetchChannelEdgesByOutpoint(
	op *wire.OutPoint) (*channeldb.ChannelEdgeInfo,
	*channeldb.ChannelEdgePolicy, *channeldb.ChannelEdgePolicy, error) {

	g.mu.Lock()
	defer g.mu.Unlock()

	info, ok := g.chanInfos[*op]
	if !ok {
		return nil, nil, nil, channeldb.ErrEdgeNotFound
	}

	pol1 := g.chanPols1[*op]
	pol2 := g.chanPols2[*op]

	return info, pol1, pol2, nil
}

func (g *mockGraph) ApplyChannelUpdate(update *lnwire.ChannelUpdate) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	outpoint, ok := g.sidToCid[update.ShortChannelID]
	if !ok {
		return fmt.Errorf("unknown short channel id: %v",
			update.ShortChannelID)
	}

	pol1 := g.chanPols1[outpoint]
	pol2 := g.chanPols2[outpoint]

	// Determine which policy we should update by making the flags on the
	// policies and updates, and seeing which match up.
	var update1 bool
	switch {
	case update.ChannelFlags&lnwire.ChanUpdateDirection ==
		pol1.ChannelFlags&lnwire.ChanUpdateDirection:
		update1 = true

	case update.ChannelFlags&lnwire.ChanUpdateDirection ==
		pol2.ChannelFlags&lnwire.ChanUpdateDirection:
		update1 = false

	default:
		return fmt.Errorf("unable to find policy to update")
	}

	timestamp := time.Unix(int64(update.Timestamp), 0)

	policy := &channeldb.ChannelEdgePolicy{
		ChannelID:    update.ShortChannelID.ToUint64(),
		ChannelFlags: update.ChannelFlags,
		LastUpdate:   timestamp,
		SigBytes:     make([]byte, 64),
	}

	if update1 {
		g.chanPols1[outpoint] = policy
	} else {
		g.chanPols2[outpoint] = policy
	}

	// Send the update to network. This channel should be sufficiently
	// buffered to avoid deadlocking.
	g.updates <- update

	return nil
}

func (g *mockGraph) chans() []*channeldb.OpenChannel {
	g.mu.Lock()
	defer g.mu.Unlock()

	channels := make([]*channeldb.OpenChannel, 0, len(g.channels))
	channels = append(channels, g.channels...)

	return channels
}

func (g *mockGraph) addChannel(channel *channeldb.OpenChannel) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.channels = append(g.channels, channel)
}

func (g *mockGraph) addEdgePolicy(c *channeldb.OpenChannel,
	info *channeldb.ChannelEdgeInfo,
	pol1, pol2 *channeldb.ChannelEdgePolicy) {

	g.mu.Lock()
	defer g.mu.Unlock()

	g.chanInfos[c.FundingOutpoint] = info
	g.chanPols1[c.FundingOutpoint] = pol1
	g.chanPols2[c.FundingOutpoint] = pol2
	g.sidToCid[c.ShortChanID()] = c.FundingOutpoint
}

func (g *mockGraph) removeChannel(channel *channeldb.OpenChannel) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for i, c := range g.channels {
		if c.FundingOutpoint != channel.FundingOutpoint {
			continue
		}

		g.channels = append(g.channels[:i], g.channels[i+1:]...)
		delete(g.chanInfos, c.FundingOutpoint)
		delete(g.chanPols1, c.FundingOutpoint)
		delete(g.chanPols2, c.FundingOutpoint)
		delete(g.sidToCid, c.ShortChanID())
		return
	}
}

type mockSwitch struct {
	mu       sync.Mutex
	isActive map[lnwire.ChannelID]bool
}

func newMockSwitch() *mockSwitch {
	return &mockSwitch{
		isActive: make(map[lnwire.ChannelID]bool),
	}
}

func (s *mockSwitch) HasActiveLink(chanID lnwire.ChannelID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If the link is found, we will returns it's active status. In the
	// real switch, it returns EligibleToForward().
	active, ok := s.isActive[chanID]
	if ok {
		return active
	}

	return false
}

func (s *mockSwitch) SetStatus(chanID lnwire.ChannelID, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.isActive[chanID] = active
}

func newManagerCfg(t *testing.T, numChannels int,
	startEnabled bool) (*netann.ChanStatusConfig, *mockGraph, *mockSwitch) {

	t.Helper()

	privKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate key pair: %v", err)
	}
	privKeySigner := &keychain.PrivKeyDigestSigner{PrivKey: privKey}

	graph := newMockGraph(
		t, numChannels, startEnabled, startEnabled, privKey.PubKey(),
	)
	htlcSwitch := newMockSwitch()

	cfg := &netann.ChanStatusConfig{
		ChanStatusSampleInterval: 50 * time.Millisecond,
		ChanEnableTimeout:        500 * time.Millisecond,
		ChanDisableTimeout:       time.Second,
		OurPubKey:                privKey.PubKey(),
		MessageSigner:            netann.NewNodeSigner(privKeySigner),
		IsChannelActive:          htlcSwitch.HasActiveLink,
		ApplyChannelUpdate:       graph.ApplyChannelUpdate,
		DB:                       graph,
		Graph:                    graph,
	}

	return cfg, graph, htlcSwitch
}

type testHarness struct {
	t                  *testing.T
	numChannels        int
	graph              *mockGraph
	htlcSwitch         *mockSwitch
	mgr                *netann.ChanStatusManager
	ourPubKey          *btcec.PublicKey
	safeDisableTimeout time.Duration
}

// newHarness returns a new testHarness for testing a ChanStatusManager. The
// mockGraph will be populated with numChannels channels. The startActive and
// startEnabled govern the initial state of the channels wrt the htlcswitch and
// the network, respectively.
func newHarness(t *testing.T, numChannels int,
	startActive, startEnabled bool) testHarness {

	cfg, graph, htlcSwitch := newManagerCfg(t, numChannels, startEnabled)

	mgr, err := netann.NewChanStatusManager(cfg)
	if err != nil {
		t.Fatalf("unable to create chan status manager: %v", err)
	}

	err = mgr.Start()
	if err != nil {
		t.Fatalf("unable to start chan status manager: %v", err)
	}

	h := testHarness{
		t:                  t,
		numChannels:        numChannels,
		graph:              graph,
		htlcSwitch:         htlcSwitch,
		mgr:                mgr,
		ourPubKey:          cfg.OurPubKey,
		safeDisableTimeout: (3 * cfg.ChanDisableTimeout) / 2, // 1.5x
	}

	// Initialize link status as requested.
	if startActive {
		h.markActive(h.graph.channels)
	} else {
		h.markInactive(h.graph.channels)
	}

	return h
}

// markActive updates the active status of the passed channels within the mock
// switch to active.
func (h *testHarness) markActive(channels []*channeldb.OpenChannel) {
	h.t.Helper()

	for _, channel := range channels {
		chanID := lnwire.NewChanIDFromOutPoint(&channel.FundingOutpoint)
		h.htlcSwitch.SetStatus(chanID, true)
	}
}

// markInactive updates the active status of the passed channels within the mock
// switch to inactive.
func (h *testHarness) markInactive(channels []*channeldb.OpenChannel) {
	h.t.Helper()

	for _, channel := range channels {
		chanID := lnwire.NewChanIDFromOutPoint(&channel.FundingOutpoint)
		h.htlcSwitch.SetStatus(chanID, false)
	}
}

// assertEnables requests enables for all of the passed channels, and asserts
// that the errors returned from RequestEnable matches expErr.
func (h *testHarness) assertEnables(channels []*channeldb.OpenChannel, expErr error) {
	h.t.Helper()

	for _, channel := range channels {
		h.assertEnable(channel.FundingOutpoint, expErr)
	}
}

// assertDisables requests disables for all of the passed channels, and asserts
// that the errors returned from RequestDisable matches expErr.
func (h *testHarness) assertDisables(channels []*channeldb.OpenChannel, expErr error) {
	h.t.Helper()

	for _, channel := range channels {
		h.assertDisable(channel.FundingOutpoint, expErr)
	}
}

// assertEnable requests an enable for the given outpoint, and asserts that the
// returned error matches expErr.
func (h *testHarness) assertEnable(outpoint wire.OutPoint, expErr error) {
	h.t.Helper()

	err := h.mgr.RequestEnable(outpoint, false)
	if err != expErr {
		h.t.Fatalf("expected enable error: %v, got %v", expErr, err)
	}
}

// assertDisable requests a disable for the given outpoint, and asserts that the
// returned error matches expErr.
func (h *testHarness) assertDisable(outpoint wire.OutPoint, expErr error) {
	h.t.Helper()

	err := h.mgr.RequestDisable(outpoint, false)
	if err != expErr {
		h.t.Fatalf("expected disable error: %v, got %v", expErr, err)
	}
}

// assertNoUpdates waits for the specified duration, and asserts that no updates
// are announced on the network.
func (h *testHarness) assertNoUpdates(duration time.Duration) {
	h.t.Helper()

	h.assertUpdates(nil, false, duration)
}

// assertUpdates waits for the specified duration, asserting that an update
// are receive on the network for each of the passed OpenChannels, and that all
// of their disable bits are set to match expEnabled. The expEnabled parameter
// is ignored if channels is nil.
func (h *testHarness) assertUpdates(channels []*channeldb.OpenChannel,
	expEnabled bool, duration time.Duration) {

	h.t.Helper()

	// Compute an index of the expected short channel ids for which we want
	// to received updates.
	expSids := sidsFromChans(channels)

	timeout := time.After(duration)
	recvdSids := make(map[lnwire.ShortChannelID]struct{})
	for {
		select {
		case upd := <-h.graph.updates:
			// Assert that the received short channel id is one that
			// we expect. If no updates were expected, this will
			// always fail on the first update received.
			if _, ok := expSids[upd.ShortChannelID]; !ok {
				h.t.Fatalf("received update for unexpected "+
					"short chan id: %v", upd.ShortChannelID)
			}

			// Assert that the disabled bit is set properly.
			enabled := upd.ChannelFlags&lnwire.ChanUpdateDisabled !=
				lnwire.ChanUpdateDisabled
			if expEnabled != enabled {
				h.t.Fatalf("expected enabled: %v, actual: %v",
					expEnabled, enabled)
			}

			recvdSids[upd.ShortChannelID] = struct{}{}

		case <-timeout:
			// Time is up, assert that the correct number of unique
			// updates was received.
			if len(recvdSids) == len(channels) {
				return
			}

			h.t.Fatalf("expected %d updates, got %d",
				len(channels), len(recvdSids))
		}
	}
}

// sidsFromChans returns an index contain the short channel ids of each channel
// provided in the list of OpenChannels.
func sidsFromChans(
	channels []*channeldb.OpenChannel) map[lnwire.ShortChannelID]struct{} {

	sids := make(map[lnwire.ShortChannelID]struct{})
	for _, channel := range channels {
		sids[channel.ShortChanID()] = struct{}{}
	}
	return sids
}

type stateMachineTest struct {
	name         string
	startEnabled bool
	startActive  bool
	fn           func(testHarness)
}

var stateMachineTests = []stateMachineTest{
	{
		name:         "active and enabled is stable",
		startActive:  true,
		startEnabled: true,
		fn: func(h testHarness) {
			// No updates should be sent because being active and
			// enabled should be a stable state.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "inactive and disabled is stable",
		startActive:  false,
		startEnabled: false,
		fn: func(h testHarness) {
			// No updates should be sent because being inactive and
			// disabled should be a stable state.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "start disabled request enable",
		startActive:  true, // can't request enable unless active
		startEnabled: false,
		fn: func(h testHarness) {
			// Request enables for all channels.
			h.assertEnables(h.graph.chans(), nil)
			// Expect to see them all enabled on the network.
			h.assertUpdates(
				h.graph.chans(), true, h.safeDisableTimeout,
			)
		},
	},
	{
		name:         "start enabled request disable",
		startActive:  true,
		startEnabled: true,
		fn: func(h testHarness) {
			// Request disables for all channels.
			h.assertDisables(h.graph.chans(), nil)
			// Expect to see them all disabled on the network.
			h.assertUpdates(
				h.graph.chans(), false, h.safeDisableTimeout,
			)
		},
	},
	{
		name:         "request enable already enabled",
		startActive:  true,
		startEnabled: true,
		fn: func(h testHarness) {
			// Request enables for already enabled channels.
			h.assertEnables(h.graph.chans(), nil)
			// Manager shouldn't send out any updates.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "request disabled already disabled",
		startActive:  false,
		startEnabled: false,
		fn: func(h testHarness) {
			// Request disables for already enabled channels.
			h.assertDisables(h.graph.chans(), nil)
			// Manager shouldn't sent out any updates.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "detect and disable inactive",
		startActive:  true,
		startEnabled: true,
		fn: func(h testHarness) {
			// Simulate disconnection and have links go inactive.
			h.markInactive(h.graph.chans())
			// Should see all channels passively disabled.
			h.assertUpdates(
				h.graph.chans(), false, h.safeDisableTimeout,
			)
		},
	},
	{
		name:         "quick flap stays active",
		startActive:  true,
		startEnabled: true,
		fn: func(h testHarness) {
			// Simulate disconnection and have links go inactive.
			h.markInactive(h.graph.chans())
			// Allow 2 sample intervals to pass, but not long
			// enough for a disable to occur.
			time.Sleep(100 * time.Millisecond)
			// Simulate reconnect by making channels active.
			h.markActive(h.graph.chans())
			// Request that all channels be reenabled.
			h.assertEnables(h.graph.chans(), nil)
			// Pending disable should have been canceled, and
			// no updates sent. Channels remain enabled on the
			// network.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "no passive enable from becoming active",
		startActive:  false,
		startEnabled: false,
		fn: func(h testHarness) {
			// Simulate reconnect by making channels active.
			h.markActive(h.graph.chans())
			// No updates should be sent without explicit enable.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "enable inactive channel fails",
		startActive:  false,
		startEnabled: false,
		fn: func(h testHarness) {
			// Request enable of inactive channels, expect error
			// indicating that channel was not active.
			h.assertEnables(
				h.graph.chans(), netann.ErrEnableInactiveChan,
			)
			// No updates should be sent as a result of the failure.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "enable unknown channel fails",
		startActive:  false,
		startEnabled: false,
		fn: func(h testHarness) {
			// Create channels unknown to the graph.
			unknownChans := []*channeldb.OpenChannel{
				createChannel(h.t),
				createChannel(h.t),
				createChannel(h.t),
			}
			// Request that they be enabled, which should return an
			// error as the graph doesn't have an edge for them.
			h.assertEnables(
				unknownChans, channeldb.ErrEdgeNotFound,
			)
			// No updates should be sent as a result of the failure.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "disable unknown channel fails",
		startActive:  false,
		startEnabled: false,
		fn: func(h testHarness) {
			// Create channels unknown to the graph.
			unknownChans := []*channeldb.OpenChannel{
				createChannel(h.t),
				createChannel(h.t),
				createChannel(h.t),
			}
			// Request that they be disabled, which should return an
			// error as the graph doesn't have an edge for them.
			h.assertDisables(
				unknownChans, channeldb.ErrEdgeNotFound,
			)
			// No updates should be sent as a result of the failure.
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
	{
		name:         "add new channels",
		startActive:  false,
		startEnabled: false,
		fn: func(h testHarness) {
			// Allow the manager to enter a steady state for the
			// initial channel set.
			h.assertNoUpdates(h.safeDisableTimeout)

			// Add a new channels to the graph, but don't yet add
			// the edge policies. We should see no updates sent
			// since the manager can't access the policies.
			newChans := []*channeldb.OpenChannel{
				createChannel(h.t),
				createChannel(h.t),
				createChannel(h.t),
			}
			for _, c := range newChans {
				h.graph.addChannel(c)
			}
			h.assertNoUpdates(h.safeDisableTimeout)

			// Check that trying to enable the channel with unknown
			// edges results in a failure.
			h.assertEnables(newChans, channeldb.ErrEdgeNotFound)

			// Now, insert edge policies for the channel into the
			// graph, starting with the channel enabled, and mark
			// the link active.
			for _, c := range newChans {
				info, pol1, pol2 := createEdgePolicies(
					h.t, c, h.ourPubKey, true,
				)
				h.graph.addEdgePolicy(c, info, pol1, pol2)
			}
			h.markActive(newChans)

			// We expect no updates to be sent since the channel is
			// enabled and active.
			h.assertNoUpdates(h.safeDisableTimeout)

			// Finally, assert that enabling the channel doesn't
			// return an error now that everything is in place.
			h.assertEnables(newChans, nil)
		},
	},
	{
		name:         "remove channels then disable",
		startActive:  true,
		startEnabled: true,
		fn: func(h testHarness) {
			// Allow the manager to enter a steady state for the
			// initial channel set.
			h.assertNoUpdates(h.safeDisableTimeout)

			// Select half of the current channels to remove.
			channels := h.graph.chans()
			rmChans := channels[:len(channels)/2]

			// Mark the channel inactive and remove them from the
			// graph. This should trigger the manager to attempt a
			// mark the channel disabled, but will unable to do so
			// because it can't find the edge policies.
			h.markInactive(rmChans)
			for _, c := range rmChans {
				h.graph.removeChannel(c)
			}
			h.assertNoUpdates(h.safeDisableTimeout)

			// Check that trying to enable the channel with unknown
			// edges results in a failure.
			h.assertDisables(rmChans, channeldb.ErrEdgeNotFound)
		},
	},
	{
		name:         "disable channels then remove",
		startActive:  true,
		startEnabled: true,
		fn: func(h testHarness) {
			// Allow the manager to enter a steady state for the
			// initial channel set.
			h.assertNoUpdates(h.safeDisableTimeout)

			// Select half of the current channels to remove.
			channels := h.graph.chans()
			rmChans := channels[:len(channels)/2]

			// Check that trying to enable the channel with unknown
			// edges results in a failure.
			h.assertDisables(rmChans, nil)

			// Since the channels are still in the graph, we expect
			// these channels to be disabled on the network.
			h.assertUpdates(rmChans, false, h.safeDisableTimeout)

			// Finally, remove  the channels from the graph and
			// assert no more updates are sent.
			for _, c := range rmChans {
				h.graph.removeChannel(c)
			}
			h.assertNoUpdates(h.safeDisableTimeout)
		},
	},
}

// TestChanStatusManagerStateMachine tests the possible state transitions that
// can be taken by the ChanStatusManager.
func TestChanStatusManagerStateMachine(t *testing.T) {
	t.Parallel()

	for _, test := range stateMachineTests {
		tc := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			const numChannels = 10
			h := newHarness(
				t, numChannels, tc.startActive, tc.startEnabled,
			)
			defer h.mgr.Stop()

			tc.fn(h)
		})
	}
}

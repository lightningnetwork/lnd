package autopilot

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

type moreChansResp struct {
	needMore bool
	numMore  uint32
	amt      btcutil.Amount
}

type moreChanArg struct {
	chans   []Channel
	balance btcutil.Amount
}

type mockHeuristic struct {
	moreChansResps chan moreChansResp
	moreChanArgs   chan moreChanArg

	directiveResps chan []AttachmentDirective
	directiveArgs  chan directiveArg

	quit chan struct{}
}

func (m *mockHeuristic) NeedMoreChans(chans []Channel,
	balance btcutil.Amount) (btcutil.Amount, uint32, bool) {

	if m.moreChanArgs != nil {
		moreChan := moreChanArg{
			chans:   chans,
			balance: balance,
		}

		select {
		case m.moreChanArgs <- moreChan:
		case <-m.quit:
			return 0, 0, false
		}
	}

	select {
	case resp := <-m.moreChansResps:
		return resp.amt, resp.numMore, resp.needMore
	case <-m.quit:
		return 0, 0, false
	}
}

type directiveArg struct {
	self  *btcec.PublicKey
	graph ChannelGraph
	amt   btcutil.Amount
	skip  map[NodeID]struct{}
}

func (m *mockHeuristic) Select(self *btcec.PublicKey, graph ChannelGraph,
	amtToUse btcutil.Amount, numChans uint32,
	skipChans map[NodeID]struct{}) ([]AttachmentDirective, error) {

	if m.directiveArgs != nil {
		directive := directiveArg{
			self:  self,
			graph: graph,
			amt:   amtToUse,
			skip:  skipChans,
		}

		select {
		case m.directiveArgs <- directive:
		case <-m.quit:
			return nil, errors.New("exiting")
		}
	}

	select {
	case resp := <-m.directiveResps:
		return resp, nil
	case <-m.quit:
		return nil, errors.New("exiting")
	}
}

var _ AttachmentHeuristic = (*mockHeuristic)(nil)

type openChanIntent struct {
	target  *btcec.PublicKey
	amt     btcutil.Amount
	private bool
}

type mockChanController struct {
	openChanSignals chan openChanIntent
	private         bool
}

func (m *mockChanController) OpenChannel(target *btcec.PublicKey,
	amt btcutil.Amount) error {

	m.openChanSignals <- openChanIntent{
		target:  target,
		amt:     amt,
		private: m.private,
	}

	return nil
}

func (m *mockChanController) CloseChannel(chanPoint *wire.OutPoint) error {
	return nil
}
func (m *mockChanController) SpliceIn(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*Channel, error) {
	return nil, nil
}
func (m *mockChanController) SpliceOut(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*Channel, error) {
	return nil, nil
}

var _ ChannelController = (*mockChanController)(nil)

// TestAgentChannelOpenSignal tests that upon receipt of a chanOpenUpdate, then
// agent modifies its local state accordingly, and reconsults the heuristic.
func TestAgentChannelOpenSignal(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent, 10),
	}
	memGraph, _, _ := newMemChanGraph()

	// With the dependencies we created, we can now create the initial
	// agent itself.
	testCfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			return 0, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			return false, nil
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}
	initialChans := []Channel{}
	agent, err := New(testCfg, initialChans)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll star the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	// We'll send an initial "no" response to advance the agent past its
	// initial check.
	select {
	case heuristic.moreChansResps <- moreChansResp{false, 0, 0}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Next we'll signal a new channel being opened by the backing LN node,
	// with a capacity of 1 BTC.
	newChan := Channel{
		ChanID:   randChanID(),
		Capacity: btcutil.SatoshiPerBitcoin,
	}
	agent.OnChannelOpen(newChan)

	// The agent should now query the heuristic in order to determine its
	// next action as it local state has now been modified.
	select {
	case heuristic.moreChansResps <- moreChansResp{false, 0, 0}:
		// At this point, the local state of the agent should
		// have also been updated to reflect that the LN node
		// now has an additional channel with one BTC.
		if _, ok := agent.chanState[newChan.ChanID]; !ok {
			t.Fatalf("internal channel state wasn't updated")
		}

	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// There shouldn't be a call to the Select method as we've returned
	// "false" for NeedMoreChans above.
	select {

	// If this send success, then Select was erroneously called and the
	// test should be failed.
	case heuristic.directiveResps <- []AttachmentDirective{}:
		t.Fatalf("Select was called but shouldn't have been")

	// This is the correct path as Select should've be called.
	default:
	}
}

// A mockFailingChanController always fails to open a channel.
type mockFailingChanController struct {
}

func (m *mockFailingChanController) OpenChannel(target *btcec.PublicKey,
	amt btcutil.Amount) error {
	return errors.New("failure")
}

func (m *mockFailingChanController) CloseChannel(chanPoint *wire.OutPoint) error {
	return nil
}
func (m *mockFailingChanController) SpliceIn(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*Channel, error) {
	return nil, nil
}
func (m *mockFailingChanController) SpliceOut(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*Channel, error) {
	return nil, nil
}

var _ ChannelController = (*mockFailingChanController)(nil)

// TestAgentChannelFailureSignal tests that if an autopilot channel fails to
// open, the agent is signalled to make a new decision.
func TestAgentChannelFailureSignal(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockFailingChanController{}
	memGraph, _, _ := newMemChanGraph()

	// With the dependencies we created, we can now create the initial
	// agent itself.
	testCfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			return 0, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			return false, nil
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}

	initialChans := []Channel{}
	agent, err := New(testCfg, initialChans)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll start the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	// First ensure the agent will attempt to open a new channel. Return
	// that we need more channels, and have 5BTC to use.
	select {
	case heuristic.moreChansResps <- moreChansResp{true, 1, 5 * btcutil.SatoshiPerBitcoin}:
	case <-time.After(time.Second * 10):
		t.Fatal("heuristic wasn't queried in time")
	}

	// At this point, the agent should now be querying the heuristic to
	// request attachment directives, return a fake so the agent will
	// attempt to open a channel.
	var fakeDirective = AttachmentDirective{
		NodeKey: self,
		NodeID:  NewNodeID(self),
		ChanAmt: btcutil.SatoshiPerBitcoin,
		Addrs: []net.Addr{
			&net.TCPAddr{
				IP: bytes.Repeat([]byte("a"), 16),
			},
		},
	}

	select {
	case heuristic.directiveResps <- []AttachmentDirective{fakeDirective}:
	case <-time.After(time.Second * 10):
		t.Fatal("heuristic wasn't queried in time")
	}

	// At this point the agent will attempt to create a channel and fail.

	// Now ensure that the controller loop is re-executed.
	select {
	case heuristic.moreChansResps <- moreChansResp{true, 1, 5 * btcutil.SatoshiPerBitcoin}:
	case <-time.After(time.Second * 10):
		t.Fatal("heuristic wasn't queried in time")
	}

	select {
	case heuristic.directiveResps <- []AttachmentDirective{}:
	case <-time.After(time.Second * 10):
		t.Fatal("heuristic wasn't queried in time")
	}
}

// TestAgentChannelCloseSignal ensures that once the agent receives an outside
// signal of a channel belonging to the backing LN node being closed, then it
// will query the heuristic to make its next decision.
func TestAgentChannelCloseSignal(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent),
	}
	memGraph, _, _ := newMemChanGraph()

	// With the dependencies we created, we can now create the initial
	// agent itself.
	testCfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			return 0, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			return false, nil
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}

	// We'll start the agent with two channels already being active.
	initialChans := []Channel{
		{
			ChanID:   randChanID(),
			Capacity: btcutil.SatoshiPerBitcoin,
		},
		{
			ChanID:   randChanID(),
			Capacity: btcutil.SatoshiPerBitcoin * 2,
		},
	}
	agent, err := New(testCfg, initialChans)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll star the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	// We'll send an initial "no" response to advance the agent past its
	// initial check.
	select {
	case heuristic.moreChansResps <- moreChansResp{false, 0, 0}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Next, we'll close both channels which should force the agent to
	// re-query the heuristic.
	agent.OnChannelClose(initialChans[0].ChanID, initialChans[1].ChanID)

	// The agent should now query the heuristic in order to determine its
	// next action as it local state has now been modified.
	select {
	case heuristic.moreChansResps <- moreChansResp{false, 0, 0}:
		// At this point, the local state of the agent should
		// have also been updated to reflect that the LN node
		// has no existing open channels.
		if len(agent.chanState) != 0 {
			t.Fatalf("internal channel state wasn't updated")
		}

	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// There shouldn't be a call to the Select method as we've returned
	// "false" for NeedMoreChans above.
	select {

	// If this send success, then Select was erroneously called and the
	// test should be failed.
	case heuristic.directiveResps <- []AttachmentDirective{}:
		t.Fatalf("Select was called but shouldn't have been")

	// This is the correct path as Select should've be called.
	default:
	}
}

// TestAgentBalanceUpdateIncrease ensures that once the agent receives an
// outside signal concerning a balance update, then it will re-query the
// heuristic to determine its next action.
func TestAgentBalanceUpdate(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent),
	}
	memGraph, _, _ := newMemChanGraph()

	// The wallet will start with 2 BTC available.
	var walletBalanceMtx sync.Mutex
	walletBalance := btcutil.Amount(btcutil.SatoshiPerBitcoin * 2)

	// With the dependencies we created, we can now create the initial
	// agent itself.
	testCfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			walletBalanceMtx.Lock()
			defer walletBalanceMtx.Unlock()
			return walletBalance, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			return false, nil
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}
	initialChans := []Channel{}
	agent, err := New(testCfg, initialChans)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll star the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	// We'll send an initial "no" response to advance the agent past its
	// initial check.
	select {
	case heuristic.moreChansResps <- moreChansResp{false, 0, 0}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Next we'll send a new balance update signal to the agent, adding 5
	// BTC to the amount of available funds.
	walletBalanceMtx.Lock()
	walletBalance += btcutil.SatoshiPerBitcoin * 5
	walletBalanceMtx.Unlock()

	agent.OnBalanceChange()

	// The agent should now query the heuristic in order to determine its
	// next action as it local state has now been modified.
	select {
	case heuristic.moreChansResps <- moreChansResp{false, 0, 0}:
		// At this point, the local state of the agent should
		// have also been updated to reflect that the LN node
		// now has an additional 5BTC available.
		if agent.totalBalance != walletBalance {
			t.Fatalf("expected %v wallet balance "+
				"instead have %v", agent.totalBalance,
				walletBalance)
		}

	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// There shouldn't be a call to the Select method as we've returned
	// "false" for NeedMoreChans above.
	select {

	// If this send success, then Select was erroneously called and the
	// test should be failed.
	case heuristic.directiveResps <- []AttachmentDirective{}:
		t.Fatalf("Select was called but shouldn't have been")

	// This is the correct path as Select should've be called.
	default:
	}
}

// TestAgentImmediateAttach tests that if an autopilot agent is created, and it
// has enough funds available to create channels, then it does so immediately.
func TestAgentImmediateAttach(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent),
	}
	memGraph, _, _ := newMemChanGraph()

	// The wallet will start with 10 BTC available.
	const walletBalance = btcutil.SatoshiPerBitcoin * 10

	// With the dependencies we created, we can now create the initial
	// agent itself.
	testCfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			return walletBalance, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			return false, nil
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}
	initialChans := []Channel{}
	agent, err := New(testCfg, initialChans)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll star the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	const numChans = 5

	// The very first thing the agent should do is query the NeedMoreChans
	// method on the passed heuristic. So we'll provide it with a response
	// that will kick off the main loop.
	select {

	// We'll send over a response indicating that it should
	// establish more channels, and give it a budget of 5 BTC to do
	// so.
	case heuristic.moreChansResps <- moreChansResp{
		needMore: true,
		numMore:  numChans,
		amt:      5 * btcutil.SatoshiPerBitcoin,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// At this point, the agent should now be querying the heuristic to
	// requests attachment directives. We'll generate 5 mock directives so
	// it can progress within its loop.
	directives := make([]AttachmentDirective, numChans)
	nodeKeys := make(map[NodeID]struct{})
	for i := 0; i < numChans; i++ {
		pub, err := randKey()
		if err != nil {
			t.Fatalf("unable to generate key: %v", err)
		}
		nodeID := NewNodeID(pub)
		directives[i] = AttachmentDirective{
			NodeKey: pub,
			NodeID:  nodeID,
			ChanAmt: btcutil.SatoshiPerBitcoin,
			Addrs: []net.Addr{
				&net.TCPAddr{
					IP: bytes.Repeat([]byte("a"), 16),
				},
			},
		}
		nodeKeys[nodeID] = struct{}{}
	}

	// With our fake directives created, we'll now send then to the agent
	// as a return value for the Select function.
	select {
	case heuristic.directiveResps <- directives:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Finally, we should receive 5 calls to the OpenChannel method with
	// the exact same parameters that we specified within the attachment
	// directives.
	for i := 0; i < numChans; i++ {
		select {
		case openChan := <-chanController.openChanSignals:
			if openChan.amt != btcutil.SatoshiPerBitcoin {
				t.Fatalf("invalid chan amt: expected %v, got %v",
					btcutil.SatoshiPerBitcoin, openChan.amt)
			}
			nodeID := NewNodeID(openChan.target)
			_, ok := nodeKeys[nodeID]
			if !ok {
				t.Fatalf("unexpected key: %v, not found",
					nodeID)
			}
			delete(nodeKeys, nodeID)
		case <-time.After(time.Second * 10):
			t.Fatalf("channel not opened in time")
		}
	}
}

// TestAgentPrivateChannels ensure that only requests for private channels are
// sent if set.
func TestAgentPrivateChannels(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	// The chanController should be initialized such that all of its open
	// channel requests are for private channels.
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent),
		private:         true,
	}
	memGraph, _, _ := newMemChanGraph()

	// The wallet will start with 10 BTC available.
	const walletBalance = btcutil.SatoshiPerBitcoin * 10

	// With the dependencies we created, we can now create the initial
	// agent itself.
	cfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			return walletBalance, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			return false, nil
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}
	agent, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll star the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	const numChans = 5

	// The very first thing the agent should do is query the NeedMoreChans
	// method on the passed heuristic. So we'll provide it with a response
	// that will kick off the main loop.  We'll send over a response
	// indicating that it should establish more channels, and give it a
	// budget of 5 BTC to do so.
	resp := moreChansResp{
		needMore: true,
		numMore:  numChans,
		amt:      5 * btcutil.SatoshiPerBitcoin,
	}
	select {
	case heuristic.moreChansResps <- resp:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// At this point, the agent should now be querying the heuristic to
	// requests attachment directives. We'll generate 5 mock directives so
	// it can progress within its loop.
	directives := make([]AttachmentDirective, numChans)
	for i := 0; i < numChans; i++ {
		pub, err := randKey()
		if err != nil {
			t.Fatalf("unable to generate key: %v", err)
		}
		directives[i] = AttachmentDirective{
			NodeKey: pub,
			NodeID:  NewNodeID(pub),
			ChanAmt: btcutil.SatoshiPerBitcoin,
			Addrs: []net.Addr{
				&net.TCPAddr{
					IP: bytes.Repeat([]byte("a"), 16),
				},
			},
		}
	}

	// With our fake directives created, we'll now send then to the agent
	// as a return value for the Select function.
	select {
	case heuristic.directiveResps <- directives:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Finally, we should receive 5 calls to the OpenChannel method, each
	// specifying that it's for a private channel.
	for i := 0; i < numChans; i++ {
		select {
		case openChan := <-chanController.openChanSignals:
			if !openChan.private {
				t.Fatal("expected open channel request to be private")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("channel not opened in time")
		}
	}
}

// TestAgentPendingChannelState ensures that the agent properly factors in its
// pending channel state when making decisions w.r.t if it needs more channels
// or not, and if so, who is eligible to open new channels to.
func TestAgentPendingChannelState(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent),
	}
	memGraph, _, _ := newMemChanGraph()

	// The wallet will start with 6 BTC available.
	var walletBalanceMtx sync.Mutex
	walletBalance := btcutil.Amount(btcutil.SatoshiPerBitcoin * 6)

	// With the dependencies we created, we can now create the initial
	// agent itself.
	testCfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			walletBalanceMtx.Lock()
			defer walletBalanceMtx.Unlock()

			return walletBalance, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			return false, nil
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}
	initialChans := []Channel{}
	agent, err := New(testCfg, initialChans)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll start the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	// Once again, we'll start by telling the agent as part of its first
	// query, that it needs more channels and has 3 BTC available for
	// attachment.  We'll send over a response indicating that it should
	// establish more channels, and give it a budget of 1 BTC to do so.
	select {
	case heuristic.moreChansResps <- moreChansResp{
		needMore: true,
		numMore:  1,
		amt:      btcutil.SatoshiPerBitcoin,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	heuristic.moreChanArgs = make(chan moreChanArg)

	// Next, the agent should deliver a query to the Select method of the
	// heuristic. We'll only return a single directive for a pre-chosen
	// node.
	nodeKey, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	nodeID := NewNodeID(nodeKey)
	nodeDirective := AttachmentDirective{
		NodeKey: nodeKey,
		NodeID:  nodeID,
		ChanAmt: 0.5 * btcutil.SatoshiPerBitcoin,
		Addrs: []net.Addr{
			&net.TCPAddr{
				IP: bytes.Repeat([]byte("a"), 16),
			},
		},
	}
	select {
	case heuristic.directiveResps <- []AttachmentDirective{nodeDirective}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	heuristic.directiveArgs = make(chan directiveArg)

	// A request to open the channel should've also been sent.
	select {
	case openChan := <-chanController.openChanSignals:
		if openChan.amt != nodeDirective.ChanAmt {
			t.Fatalf("invalid chan amt: expected %v, got %v",
				nodeDirective.ChanAmt, openChan.amt)
		}
		if !openChan.target.IsEqual(nodeKey) {
			t.Fatalf("unexpected key: expected %x, got %x",
				nodeKey.SerializeCompressed(),
				openChan.target.SerializeCompressed())
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("channel wasn't opened in time")
	}

	// Now, in order to test that the pending state was properly updated,
	// we'll trigger a balance update in order to trigger a query to the
	// heuristic.
	walletBalanceMtx.Lock()
	walletBalance += 0.4 * btcutil.SatoshiPerBitcoin
	walletBalanceMtx.Unlock()

	agent.OnBalanceChange()

	// The heuristic should be queried, and the argument for the set of
	// channels passed in should include the pending channels that
	// should've been created above.
	select {
	// The request that we get should include a pending channel for the
	// one that we just created, otherwise the agent isn't properly
	// updating its internal state.
	case req := <-heuristic.moreChanArgs:
		if len(req.chans) != 1 {
			t.Fatalf("should include pending chan in current "+
				"state, instead have %v chans", len(req.chans))
		}
		if req.chans[0].Capacity != nodeDirective.ChanAmt {
			t.Fatalf("wrong chan capacity: expected %v, got %v",
				req.chans[0].Capacity, nodeDirective.ChanAmt)
		}
		if req.chans[0].Node != nodeID {
			t.Fatalf("wrong node ID: expected %x, got %x",
				nodeID, req.chans[0].Node[:])
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("need more chans wasn't queried in time")
	}

	// We'll send across a response indicating that it *does* need more
	// channels.
	select {
	case heuristic.moreChansResps <- moreChansResp{true, 1, btcutil.SatoshiPerBitcoin}:
	case <-time.After(time.Second * 10):
		t.Fatalf("need more chans wasn't queried in time")
	}

	// The response above should prompt the agent to make a query to the
	// Select method. The arguments passed should reflect the fact that the
	// node we have a pending channel to, should be ignored.
	select {
	case req := <-heuristic.directiveArgs:
		if len(req.skip) == 0 {
			t.Fatalf("expected to skip %v nodes, instead "+
				"skipping %v", 1, len(req.skip))
		}
		if _, ok := req.skip[nodeID]; !ok {
			t.Fatalf("pending node not included in skip arguments")
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("select wasn't queried in time")
	}
}

// TestAgentPendingOpenChannel ensures that the agent queries its heuristic once
// it detects a channel is pending open. This allows the agent to use its own
// change outputs that have yet to confirm for funding transactions.
func TestAgentPendingOpenChannel(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent),
	}
	memGraph, _, _ := newMemChanGraph()

	// The wallet will start with 6 BTC available.
	const walletBalance = btcutil.SatoshiPerBitcoin * 6

	// With the dependencies we created, we can now create the initial
	// agent itself.
	cfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			return walletBalance, nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}
	agent, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll start the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	// We'll send an initial "no" response to advance the agent past its
	// initial check.
	select {
	case heuristic.moreChansResps <- moreChansResp{false, 0, 0}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Next, we'll signal that a new channel has been opened, but it is
	// still pending.
	agent.OnChannelPendingOpen()

	// The agent should now query the heuristic in order to determine its
	// next action as its local state has now been modified.
	select {
	case heuristic.moreChansResps <- moreChansResp{false, 0, 0}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// There shouldn't be a call to the Select method as we've returned
	// "false" for NeedMoreChans above.
	select {
	case heuristic.directiveResps <- []AttachmentDirective{}:
		t.Fatalf("Select was called but shouldn't have been")
	default:
	}
}

// TestAgentOnNodeUpdates tests that the agent will wake up in response to the
// OnNodeUpdates signal. This is useful in ensuring that autopilot is always
// pulling in the latest graph updates into its decision making. It also
// prevents the agent from stalling after an initial attempt that finds no nodes
// in the graph.
func TestAgentOnNodeUpdates(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent),
	}
	memGraph, _, _ := newMemChanGraph()

	// The wallet will start with 6 BTC available.
	const walletBalance = btcutil.SatoshiPerBitcoin * 6

	// With the dependencies we created, we can now create the initial
	// agent itself.
	cfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			return walletBalance, nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}
	agent, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll start the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	// We'll send an initial "yes" response to advance the agent past its
	// initial check. This will cause it to try to get directives from an
	// empty graph.
	select {
	case heuristic.moreChansResps <- moreChansResp{
		needMore: true,
		numMore:  2,
		amt:      walletBalance,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Send over an empty list of attachment directives, which should cause
	// the agent to return to waiting on a new signal.
	select {
	case heuristic.directiveResps <- []AttachmentDirective{}:
	case <-time.After(time.Second * 10):
		t.Fatalf("Select was not called but should have been")
	}

	// Simulate more nodes being added to the graph by informing the agent
	// that we have node updates.
	agent.OnNodeUpdates()

	// In response, the agent should wake up and see if it needs more
	// channels. Since we haven't done anything, we will send the same
	// response as before since we are still trying to open channels.
	select {
	case heuristic.moreChansResps <- moreChansResp{
		needMore: true,
		numMore:  2,
		amt:      walletBalance,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Again the agent should pull in the next set of attachment directives.
	// It's not important that this list is also empty, so long as the node
	// updates signal is causing the agent to make this attempt.
	select {
	case heuristic.directiveResps <- []AttachmentDirective{}:
	case <-time.After(time.Second * 10):
		t.Fatalf("Select was not called but should have been")
	}
}

// TestAgentSkipPendingConns asserts that the agent will not try to make
// duplicate connection requests to the same node, even if the attachment
// heuristic instructs the agent to do so. It also asserts that the agent
// stops tracking the pending connection once it finishes. Note that in
// practice, a failed connection would be inserted into the skip map passed to
// the attachment heuristic, though this does not assert that case.
func TestAgentSkipPendingConns(t *testing.T) {
	t.Parallel()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	heuristic := &mockHeuristic{
		moreChansResps: make(chan moreChansResp),
		directiveResps: make(chan []AttachmentDirective),
	}
	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent),
	}
	memGraph, _, _ := newMemChanGraph()

	// The wallet will start with 6 BTC available.
	const walletBalance = btcutil.SatoshiPerBitcoin * 6

	connect := make(chan chan error)

	// With the dependencies we created, we can now create the initial
	// agent itself.
	testCfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			return walletBalance, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			errChan := make(chan error)
			connect <- errChan
			err := <-errChan
			return false, err
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:           memGraph,
		MaxPendingOpens: 10,
	}
	initialChans := []Channel{}
	agent, err := New(testCfg, initialChans)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}

	// To ensure the heuristic doesn't block on quitting the agent, we'll
	// use the agent's quit chan to signal when it should also stop.
	heuristic.quit = agent.quit

	// With the autopilot agent and all its dependencies we'll start the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}
	defer agent.Stop()

	// We'll send an initial "yes" response to advance the agent past its
	// initial check. This will cause it to try to get directives from the
	// graph.
	select {
	case heuristic.moreChansResps <- moreChansResp{
		needMore: true,
		numMore:  1,
		amt:      walletBalance,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Next, the agent should deliver a query to the Select method of the
	// heuristic. We'll only return a single directive for a pre-chosen
	// node.
	nodeKey, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	nodeDirective := AttachmentDirective{
		NodeKey: nodeKey,
		NodeID:  NewNodeID(nodeKey),
		ChanAmt: 0.5 * btcutil.SatoshiPerBitcoin,
		Addrs: []net.Addr{
			&net.TCPAddr{
				IP: bytes.Repeat([]byte("a"), 16),
			},
		},
	}
	select {
	case heuristic.directiveResps <- []AttachmentDirective{nodeDirective}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	var errChan chan error
	select {
	case errChan = <-connect:
	case <-time.After(time.Second * 10):
		t.Fatalf("agent did not attempt connection")
	}

	// Signal the agent to go again, now that we've tried to connect.
	agent.OnNodeUpdates()

	// The heuristic again informs the agent that we need more channels.
	select {
	case heuristic.moreChansResps <- moreChansResp{
		needMore: true,
		numMore:  1,
		amt:      walletBalance,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Send a directive for the same node, which already has a pending conn.
	select {
	case heuristic.directiveResps <- []AttachmentDirective{nodeDirective}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// This time, the agent should skip trying to connect to the node with a
	// pending connection.
	select {
	case <-connect:
		t.Fatalf("agent should not have attempted connection")
	case <-time.After(time.Second * 3):
	}

	// Now, timeout the original request, which should still be waiting for
	// a response.
	select {
	case errChan <- fmt.Errorf("connection timeout"):
	case <-time.After(time.Second * 10):
		t.Fatalf("agent did not receive connection timeout")
	}

	// Signal the agent to try again, now that there are no pending conns.
	agent.OnNodeUpdates()

	// The heuristic again informs the agent that we need more channels.
	select {
	case heuristic.moreChansResps <- moreChansResp{
		needMore: true,
		numMore:  1,
		amt:      walletBalance,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// Send a directive for the same node, which already has a pending conn.
	select {
	case heuristic.directiveResps <- []AttachmentDirective{nodeDirective}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// This time, the agent should try the connection since the peer has
	// been removed from the pending map.
	select {
	case <-connect:
	case <-time.After(time.Second * 10):
		t.Fatalf("agent have attempted connection")
	}
}

package autopilot

import (
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
	numMore uint32
	amt     btcutil.Amount
}

type moreChanArg struct {
	chans   []Channel
	balance btcutil.Amount
}

type mockConstraints struct {
	moreChansResps chan moreChansResp
	moreChanArgs   chan moreChanArg
	quit           chan struct{}
}

func (m *mockConstraints) ChannelBudget(chans []Channel,
	balance btcutil.Amount) (btcutil.Amount, uint32) {

	if m.moreChanArgs != nil {
		moreChan := moreChanArg{
			chans:   chans,
			balance: balance,
		}

		select {
		case m.moreChanArgs <- moreChan:
		case <-m.quit:
			return 0, 0
		}
	}

	select {
	case resp := <-m.moreChansResps:
		return resp.amt, resp.numMore
	case <-m.quit:
		return 0, 0
	}
}

func (m *mockConstraints) MaxPendingOpens() uint16 {
	return 10
}

func (m *mockConstraints) MinChanSize() btcutil.Amount {
	return 1e7
}
func (m *mockConstraints) MaxChanSize() btcutil.Amount {
	return 1e8
}

var _ AgentConstraints = (*mockConstraints)(nil)

type mockHeuristic struct {
	nodeScoresResps chan map[NodeID]*NodeScore
	nodeScoresArgs  chan directiveArg

	quit chan struct{}
}

type directiveArg struct {
	graph ChannelGraph
	amt   btcutil.Amount
	chans []Channel
	nodes map[NodeID]struct{}
}

func (m *mockHeuristic) Name() string {
	return "mock"
}

func (m *mockHeuristic) NodeScores(g ChannelGraph, chans []Channel,
	chanSize btcutil.Amount, nodes map[NodeID]struct{}) (
	map[NodeID]*NodeScore, error) {

	if m.nodeScoresArgs != nil {
		directive := directiveArg{
			graph: g,
			amt:   chanSize,
			chans: chans,
			nodes: nodes,
		}

		select {
		case m.nodeScoresArgs <- directive:
		case <-m.quit:
			return nil, errors.New("exiting")
		}
	}

	select {
	case resp := <-m.nodeScoresResps:
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

type testContext struct {
	constraints    *mockConstraints
	heuristic      *mockHeuristic
	chanController ChannelController
	graph          testGraph
	agent          *Agent
	walletBalance  btcutil.Amount

	quit chan struct{}
	sync.Mutex
}

func setup(t *testing.T, initialChans []Channel) (*testContext, func()) {
	t.Helper()

	// First, we'll create all the dependencies that we'll need in order to
	// create the autopilot agent.
	self, err := randKey()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}

	quit := make(chan struct{})
	heuristic := &mockHeuristic{
		nodeScoresArgs:  make(chan directiveArg),
		nodeScoresResps: make(chan map[NodeID]*NodeScore),
		quit:            quit,
	}
	constraints := &mockConstraints{
		moreChansResps: make(chan moreChansResp),
		moreChanArgs:   make(chan moreChanArg),
		quit:           quit,
	}

	chanController := &mockChanController{
		openChanSignals: make(chan openChanIntent, 10),
	}
	memGraph, _, _ := newMemChanGraph()

	// We'll keep track of the funds available to the agent, to make sure
	// it correctly uses this value when querying the ChannelBudget.
	var availableFunds btcutil.Amount = 10 * btcutil.SatoshiPerBitcoin

	ctx := &testContext{
		constraints:    constraints,
		heuristic:      heuristic,
		chanController: chanController,
		graph:          memGraph,
		walletBalance:  availableFunds,
		quit:           quit,
	}

	// With the dependencies we created, we can now create the initial
	// agent itself.
	testCfg := Config{
		Self:           self,
		Heuristic:      heuristic,
		ChanController: chanController,
		WalletBalance: func() (btcutil.Amount, error) {
			ctx.Lock()
			defer ctx.Unlock()
			return ctx.walletBalance, nil
		},
		ConnectToPeer: func(*btcec.PublicKey, []net.Addr) (bool, error) {
			return false, nil
		},
		DisconnectPeer: func(*btcec.PublicKey) error {
			return nil
		},
		Graph:       memGraph,
		Constraints: constraints,
	}

	agent, err := New(testCfg, initialChans)
	if err != nil {
		t.Fatalf("unable to create agent: %v", err)
	}
	ctx.agent = agent

	// With the autopilot agent and all its dependencies we'll start the
	// primary controller goroutine.
	if err := agent.Start(); err != nil {
		t.Fatalf("unable to start agent: %v", err)
	}

	cleanup := func() {
		// We must close quit before agent.Stop(), to make sure
		// ChannelBudget won't block preventing the agent from exiting.
		close(quit)
		agent.Stop()
	}

	return ctx, cleanup
}

// respondMoreChans consumes the moreChanArgs element and responds to the agent
// with the given moreChansResp.
func respondMoreChans(t *testing.T, testCtx *testContext, resp moreChansResp) {
	t.Helper()

	// The agent should now query the heuristic.
	select {
	case <-testCtx.constraints.moreChanArgs:
	case <-time.After(time.Second * 3):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// We'll send the response.
	select {
	case testCtx.constraints.moreChansResps <- resp:
	case <-time.After(time.Second * 10):
		t.Fatalf("response wasn't sent in time")
	}
}

// respondMoreChans consumes the nodeScoresArgs element and responds to the
// agent with the given node scores.
func respondNodeScores(t *testing.T, testCtx *testContext,
	resp map[NodeID]*NodeScore) {
	t.Helper()

	// Send over an empty list of attachment directives, which should cause
	// the agent to return to waiting on a new signal.
	select {
	case <-testCtx.heuristic.nodeScoresArgs:
	case <-time.After(time.Second * 3):
		t.Fatalf("node scores weren't queried in time")
	}
	select {
	case testCtx.heuristic.nodeScoresResps <- resp:
	case <-time.After(time.Second * 10):
		t.Fatalf("node scores were not sent in time")
	}
}

// TestAgentChannelOpenSignal tests that upon receipt of a chanOpenUpdate, then
// agent modifies its local state accordingly, and reconsults the heuristic.
func TestAgentChannelOpenSignal(t *testing.T) {
	t.Parallel()

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	// We'll send an initial "no" response to advance the agent past its
	// initial check.
	respondMoreChans(t, testCtx, moreChansResp{0, 0})

	// Next we'll signal a new channel being opened by the backing LN node,
	// with a capacity of 1 BTC.
	newChan := Channel{
		ChanID:   randChanID(),
		Capacity: btcutil.SatoshiPerBitcoin,
	}
	testCtx.agent.OnChannelOpen(newChan)

	// The agent should now query the heuristic in order to determine its
	// next action as it local state has now been modified.
	respondMoreChans(t, testCtx, moreChansResp{0, 0})

	// At this point, the local state of the agent should
	// have also been updated to reflect that the LN node
	// now has an additional channel with one BTC.
	if _, ok := testCtx.agent.chanState[newChan.ChanID]; !ok {
		t.Fatalf("internal channel state wasn't updated")
	}

	// There shouldn't be a call to the Select method as we've returned
	// "false" for NeedMoreChans above.
	select {

	// If this send success, then Select was erroneously called and the
	// test should be failed.
	case testCtx.heuristic.nodeScoresResps <- map[NodeID]*NodeScore{}:
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

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	testCtx.chanController = &mockFailingChanController{}

	node, err := testCtx.graph.addRandNode()
	if err != nil {
		t.Fatalf("unable to add node: %v", err)
	}

	// First ensure the agent will attempt to open a new channel. Return
	// that we need more channels, and have 5BTC to use.
	respondMoreChans(t, testCtx, moreChansResp{1, 5 * btcutil.SatoshiPerBitcoin})

	// At this point, the agent should now be querying the heuristic to
	// request attachment directives, return a fake so the agent will
	// attempt to open a channel.
	var fakeDirective = &NodeScore{
		NodeID: NewNodeID(node),
		Score:  0.5,
	}

	respondNodeScores(
		t, testCtx, map[NodeID]*NodeScore{
			NewNodeID(node): fakeDirective,
		},
	)

	// At this point the agent will attempt to create a channel and fail.

	// Now ensure that the controller loop is re-executed.
	respondMoreChans(t, testCtx, moreChansResp{1, 5 * btcutil.SatoshiPerBitcoin})
	respondNodeScores(t, testCtx, map[NodeID]*NodeScore{})
}

// TestAgentChannelCloseSignal ensures that once the agent receives an outside
// signal of a channel belonging to the backing LN node being closed, then it
// will query the heuristic to make its next decision.
func TestAgentChannelCloseSignal(t *testing.T) {
	t.Parallel()
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

	testCtx, cleanup := setup(t, initialChans)
	defer cleanup()

	// We'll send an initial "no" response to advance the agent past its
	// initial check.
	respondMoreChans(t, testCtx, moreChansResp{0, 0})

	// Next, we'll close both channels which should force the agent to
	// re-query the heuristic.
	testCtx.agent.OnChannelClose(initialChans[0].ChanID, initialChans[1].ChanID)

	// The agent should now query the heuristic in order to determine its
	// next action as it local state has now been modified.
	respondMoreChans(t, testCtx, moreChansResp{0, 0})

	// At this point, the local state of the agent should
	// have also been updated to reflect that the LN node
	// has no existing open channels.
	if len(testCtx.agent.chanState) != 0 {
		t.Fatalf("internal channel state wasn't updated")
	}

	// There shouldn't be a call to the Select method as we've returned
	// "false" for NeedMoreChans above.
	select {

	// If this send success, then Select was erroneously called and the
	// test should be failed.
	case testCtx.heuristic.nodeScoresResps <- map[NodeID]*NodeScore{}:
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

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	// We'll send an initial "no" response to advance the agent past its
	// initial check.
	respondMoreChans(t, testCtx, moreChansResp{0, 0})

	// Next we'll send a new balance update signal to the agent, adding 5
	// BTC to the amount of available funds.
	testCtx.Lock()
	testCtx.walletBalance += btcutil.SatoshiPerBitcoin * 5
	testCtx.Unlock()

	testCtx.agent.OnBalanceChange()

	// The agent should now query the heuristic in order to determine its
	// next action as it local state has now been modified.
	respondMoreChans(t, testCtx, moreChansResp{0, 0})

	// At this point, the local state of the agent should
	// have also been updated to reflect that the LN node
	// now has an additional 5BTC available.
	if testCtx.agent.totalBalance != testCtx.walletBalance {
		t.Fatalf("expected %v wallet balance "+
			"instead have %v", testCtx.agent.totalBalance,
			testCtx.walletBalance)
	}

	// There shouldn't be a call to the Select method as we've returned
	// "false" for NeedMoreChans above.
	select {

	// If this send success, then Select was erroneously called and the
	// test should be failed.
	case testCtx.heuristic.nodeScoresResps <- map[NodeID]*NodeScore{}:
		t.Fatalf("Select was called but shouldn't have been")

	// This is the correct path as Select should've be called.
	default:
	}
}

// TestAgentImmediateAttach tests that if an autopilot agent is created, and it
// has enough funds available to create channels, then it does so immediately.
func TestAgentImmediateAttach(t *testing.T) {
	t.Parallel()

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	const numChans = 5

	// We'll generate 5 mock directives so it can progress within its loop.
	directives := make(map[NodeID]*NodeScore)
	nodeKeys := make(map[NodeID]struct{})
	for i := 0; i < numChans; i++ {
		pub, err := testCtx.graph.addRandNode()
		if err != nil {
			t.Fatalf("unable to generate key: %v", err)
		}
		nodeID := NewNodeID(pub)
		directives[nodeID] = &NodeScore{
			NodeID: nodeID,
			Score:  0.5,
		}
		nodeKeys[nodeID] = struct{}{}
	}
	// The very first thing the agent should do is query the NeedMoreChans
	// method on the passed heuristic. So we'll provide it with a response
	// that will kick off the main loop.
	respondMoreChans(t, testCtx,
		moreChansResp{
			numMore: numChans,
			amt:     5 * btcutil.SatoshiPerBitcoin,
		},
	)

	// At this point, the agent should now be querying the heuristic to
	// requests attachment directives.  With our fake directives created,
	// we'll now send then to the agent as a return value for the Select
	// function.
	respondNodeScores(t, testCtx, directives)

	// Finally, we should receive 5 calls to the OpenChannel method with
	// the exact same parameters that we specified within the attachment
	// directives.
	chanController := testCtx.chanController.(*mockChanController)
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

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	// The chanController should be initialized such that all of its open
	// channel requests are for private channels.
	testCtx.chanController.(*mockChanController).private = true

	const numChans = 5

	// We'll generate 5 mock directives so the pubkeys will be found in the
	// agent's graph, and it can progress within its loop.
	directives := make(map[NodeID]*NodeScore)
	for i := 0; i < numChans; i++ {
		pub, err := testCtx.graph.addRandNode()
		if err != nil {
			t.Fatalf("unable to generate key: %v", err)
		}
		directives[NewNodeID(pub)] = &NodeScore{
			NodeID: NewNodeID(pub),
			Score:  0.5,
		}
	}

	// The very first thing the agent should do is query the NeedMoreChans
	// method on the passed heuristic. So we'll provide it with a response
	// that will kick off the main loop.  We'll send over a response
	// indicating that it should establish more channels, and give it a
	// budget of 5 BTC to do so.
	resp := moreChansResp{
		numMore: numChans,
		amt:     5 * btcutil.SatoshiPerBitcoin,
	}
	respondMoreChans(t, testCtx, resp)

	// At this point, the agent should now be querying the heuristic to
	// requests attachment directives.  With our fake directives created,
	// we'll now send then to the agent as a return value for the Select
	// function.
	respondNodeScores(t, testCtx, directives)

	// Finally, we should receive 5 calls to the OpenChannel method, each
	// specifying that it's for a private channel.
	chanController := testCtx.chanController.(*mockChanController)
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

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	// We'll only return a single directive for a pre-chosen node.
	nodeKey, err := testCtx.graph.addRandNode()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	nodeID := NewNodeID(nodeKey)
	nodeDirective := &NodeScore{
		NodeID: nodeID,
		Score:  0.5,
	}

	// Once again, we'll start by telling the agent as part of its first
	// query, that it needs more channels and has 3 BTC available for
	// attachment.  We'll send over a response indicating that it should
	// establish more channels, and give it a budget of 1 BTC to do so.
	respondMoreChans(t, testCtx,
		moreChansResp{
			numMore: 1,
			amt:     btcutil.SatoshiPerBitcoin,
		},
	)

	respondNodeScores(t, testCtx,
		map[NodeID]*NodeScore{
			nodeID: nodeDirective,
		},
	)

	// A request to open the channel should've also been sent.
	chanController := testCtx.chanController.(*mockChanController)
	select {
	case openChan := <-chanController.openChanSignals:
		chanAmt := testCtx.constraints.MaxChanSize()
		if openChan.amt != chanAmt {
			t.Fatalf("invalid chan amt: expected %v, got %v",
				chanAmt, openChan.amt)
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
	testCtx.Lock()
	testCtx.walletBalance += 0.4 * btcutil.SatoshiPerBitcoin
	testCtx.Unlock()

	testCtx.agent.OnBalanceChange()

	// The heuristic should be queried, and the argument for the set of
	// channels passed in should include the pending channels that
	// should've been created above.
	select {
	// The request that we get should include a pending channel for the
	// one that we just created, otherwise the agent isn't properly
	// updating its internal state.
	case req := <-testCtx.constraints.moreChanArgs:
		chanAmt := testCtx.constraints.MaxChanSize()
		if len(req.chans) != 1 {
			t.Fatalf("should include pending chan in current "+
				"state, instead have %v chans", len(req.chans))
		}
		if req.chans[0].Capacity != chanAmt {
			t.Fatalf("wrong chan capacity: expected %v, got %v",
				req.chans[0].Capacity, chanAmt)
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
	case testCtx.constraints.moreChansResps <- moreChansResp{1, btcutil.SatoshiPerBitcoin}:
	case <-time.After(time.Second * 10):
		t.Fatalf("need more chans wasn't queried in time")
	}

	// The response above should prompt the agent to make a query to the
	// Select method. The arguments passed should reflect the fact that the
	// node we have a pending channel to, should be ignored.
	select {
	case req := <-testCtx.heuristic.nodeScoresArgs:
		if len(req.chans) == 0 {
			t.Fatalf("expected to skip %v nodes, instead "+
				"skipping %v", 1, len(req.chans))
		}
		if req.chans[0].Node != nodeID {
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

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	// We'll send an initial "no" response to advance the agent past its
	// initial check.
	respondMoreChans(t, testCtx, moreChansResp{0, 0})

	// Next, we'll signal that a new channel has been opened, but it is
	// still pending.
	testCtx.agent.OnChannelPendingOpen()

	// The agent should now query the heuristic in order to determine its
	// next action as its local state has now been modified.
	respondMoreChans(t, testCtx, moreChansResp{0, 0})

	// There shouldn't be a call to the Select method as we've returned
	// "false" for NeedMoreChans above.
	select {
	case testCtx.heuristic.nodeScoresResps <- map[NodeID]*NodeScore{}:
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

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	// We'll send an initial "yes" response to advance the agent past its
	// initial check. This will cause it to try to get directives from an
	// empty graph.
	respondMoreChans(
		t, testCtx,
		moreChansResp{
			numMore: 2,
			amt:     testCtx.walletBalance,
		},
	)

	// Send over an empty list of attachment directives, which should cause
	// the agent to return to waiting on a new signal.
	respondNodeScores(t, testCtx, map[NodeID]*NodeScore{})

	// Simulate more nodes being added to the graph by informing the agent
	// that we have node updates.
	testCtx.agent.OnNodeUpdates()

	// In response, the agent should wake up and see if it needs more
	// channels. Since we haven't done anything, we will send the same
	// response as before since we are still trying to open channels.
	respondMoreChans(
		t, testCtx,
		moreChansResp{
			numMore: 2,
			amt:     testCtx.walletBalance,
		},
	)

	// Again the agent should pull in the next set of attachment directives.
	// It's not important that this list is also empty, so long as the node
	// updates signal is causing the agent to make this attempt.
	respondNodeScores(t, testCtx, map[NodeID]*NodeScore{})
}

// TestAgentSkipPendingConns asserts that the agent will not try to make
// duplicate connection requests to the same node, even if the attachment
// heuristic instructs the agent to do so. It also asserts that the agent
// stops tracking the pending connection once it finishes. Note that in
// practice, a failed connection would be inserted into the skip map passed to
// the attachment heuristic, though this does not assert that case.
func TestAgentSkipPendingConns(t *testing.T) {
	t.Parallel()

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	connect := make(chan chan error)
	testCtx.agent.cfg.ConnectToPeer = func(*btcec.PublicKey, []net.Addr) (bool, error) {
		errChan := make(chan error)

		select {
		case connect <- errChan:
		case <-testCtx.quit:
			return false, errors.New("quit")
		}

		select {
		case err := <-errChan:
			return false, err
		case <-testCtx.quit:
			return false, errors.New("quit")
		}
	}

	// We'll only return a single directive for a pre-chosen node.
	nodeKey, err := testCtx.graph.addRandNode()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	nodeID := NewNodeID(nodeKey)
	nodeDirective := &NodeScore{
		NodeID: nodeID,
		Score:  0.5,
	}

	// We'll also add a second node to the graph, to keep the first one
	// company.
	nodeKey2, err := testCtx.graph.addRandNode()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	nodeID2 := NewNodeID(nodeKey2)

	// We'll send an initial "yes" response to advance the agent past its
	// initial check. This will cause it to try to get directives from the
	// graph.
	respondMoreChans(t, testCtx,
		moreChansResp{
			numMore: 1,
			amt:     testCtx.walletBalance,
		},
	)

	// Both nodes should be part of the arguments.
	select {
	case req := <-testCtx.heuristic.nodeScoresArgs:
		if len(req.nodes) != 2 {
			t.Fatalf("expected %v nodes, instead "+
				"had %v", 2, len(req.nodes))
		}
		if _, ok := req.nodes[nodeID]; !ok {
			t.Fatalf("node not included in arguments")
		}
		if _, ok := req.nodes[nodeID2]; !ok {
			t.Fatalf("node not included in arguments")
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("select wasn't queried in time")
	}

	// Respond with a scored directive. We skip node2 for now, implicitly
	// giving it a zero-score.
	select {
	case testCtx.heuristic.nodeScoresResps <- map[NodeID]*NodeScore{
		NewNodeID(nodeKey): nodeDirective,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// The agent should attempt connection to the node.
	var errChan chan error
	select {
	case errChan = <-connect:
	case <-time.After(time.Second * 10):
		t.Fatalf("agent did not attempt connection")
	}

	// Signal the agent to go again, now that we've tried to connect.
	testCtx.agent.OnNodeUpdates()

	// The heuristic again informs the agent that we need more channels.
	respondMoreChans(t, testCtx,
		moreChansResp{
			numMore: 1,
			amt:     testCtx.walletBalance,
		},
	)

	// Since the node now has a pending connection, it should be skipped
	// and not part of the nodes attempting to be scored.
	select {
	case req := <-testCtx.heuristic.nodeScoresArgs:
		if len(req.nodes) != 1 {
			t.Fatalf("expected %v nodes, instead "+
				"had %v", 1, len(req.nodes))
		}
		if _, ok := req.nodes[nodeID2]; !ok {
			t.Fatalf("node not included in arguments")
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("select wasn't queried in time")
	}

	// Respond with an emtpty score set.
	select {
	case testCtx.heuristic.nodeScoresResps <- map[NodeID]*NodeScore{}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// The agent should not attempt any connection, since no nodes were
	// scored.
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

	// The agent will now retry since the last connection attempt failed.
	// The heuristic again informs the agent that we need more channels.
	respondMoreChans(t, testCtx,
		moreChansResp{
			numMore: 1,
			amt:     testCtx.walletBalance,
		},
	)

	// The node should now be marked as "failed", which should make it
	// being skipped during scoring. Again check that it won't be among the
	// score request.
	select {
	case req := <-testCtx.heuristic.nodeScoresArgs:
		if len(req.nodes) != 1 {
			t.Fatalf("expected %v nodes, instead "+
				"had %v", 1, len(req.nodes))
		}
		if _, ok := req.nodes[nodeID2]; !ok {
			t.Fatalf("node not included in arguments")
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("select wasn't queried in time")
	}

	// Send a directive for the second node.
	nodeDirective2 := &NodeScore{
		NodeID: nodeID2,
		Score:  0.5,
	}
	select {
	case testCtx.heuristic.nodeScoresResps <- map[NodeID]*NodeScore{
		nodeID2: nodeDirective2,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// This time, the agent should try the connection to the second node.
	select {
	case <-connect:
	case <-time.After(time.Second * 10):
		t.Fatalf("agent should have attempted connection")
	}
}

// TestAgentQuitWhenPendingConns tests that we are able to stop the autopilot
// agent even though there are pending connections to nodes.
func TestAgentQuitWhenPendingConns(t *testing.T) {
	t.Parallel()

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	connect := make(chan chan error)

	testCtx.agent.cfg.ConnectToPeer = func(*btcec.PublicKey, []net.Addr) (bool, error) {
		errChan := make(chan error)

		select {
		case connect <- errChan:
		case <-testCtx.quit:
			return false, errors.New("quit")
		}

		select {
		case err := <-errChan:
			return false, err
		case <-testCtx.quit:
			return false, errors.New("quit")
		}
	}

	// We'll only return a single directive for a pre-chosen node.
	nodeKey, err := testCtx.graph.addRandNode()
	if err != nil {
		t.Fatalf("unable to generate key: %v", err)
	}
	nodeID := NewNodeID(nodeKey)
	nodeDirective := &NodeScore{
		NodeID: nodeID,
		Score:  0.5,
	}

	// We'll send an initial "yes" response to advance the agent past its
	// initial check. This will cause it to try to get directives from the
	// graph.
	respondMoreChans(t, testCtx,
		moreChansResp{
			numMore: 1,
			amt:     testCtx.walletBalance,
		},
	)

	// Check the args.
	select {
	case req := <-testCtx.heuristic.nodeScoresArgs:
		if len(req.nodes) != 1 {
			t.Fatalf("expected %v nodes, instead "+
				"had %v", 1, len(req.nodes))
		}
		if _, ok := req.nodes[nodeID]; !ok {
			t.Fatalf("node not included in arguments")
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("select wasn't queried in time")
	}

	// Respond with a scored directive.
	select {
	case testCtx.heuristic.nodeScoresResps <- map[NodeID]*NodeScore{
		NewNodeID(nodeKey): nodeDirective,
	}:
	case <-time.After(time.Second * 10):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// The agent should attempt connection to the node.
	select {
	case <-connect:
	case <-time.After(time.Second * 10):
		t.Fatalf("agent did not attempt connection")
	}

	// Make sure that we are able to stop the agent, even though there is a
	// pending connection.
	stopped := make(chan error)
	go func() {
		stopped <- testCtx.agent.Stop()
	}()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("error stopping agent: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("unable to stop agent")
	}
}

// respondWithScores checks that the moreChansRequest contains what we expect,
// and responds with the given node scores.
func respondWithScores(t *testing.T, testCtx *testContext,
	channelBudget btcutil.Amount, existingChans, newChans int,
	nodeScores map[NodeID]*NodeScore) {

	t.Helper()

	select {
	case testCtx.constraints.moreChansResps <- moreChansResp{
		numMore: uint32(newChans),
		amt:     channelBudget,
	}:
	case <-time.After(time.Second * 3):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// The agent should query for scores using the constraints returned
	// above. We expect the agent to use the maximum channel size when
	// opening channels.
	chanSize := testCtx.constraints.MaxChanSize()

	select {
	case req := <-testCtx.heuristic.nodeScoresArgs:
		// All nodes in the graph should be potential channel
		// candidates.
		if len(req.nodes) != len(nodeScores) {
			t.Fatalf("expected %v nodes, instead had %v",
				len(nodeScores), len(req.nodes))
		}

		// 'existingChans' is already open.
		if len(req.chans) != existingChans {
			t.Fatalf("expected %d existing channel, got %v",
				existingChans, len(req.chans))
		}
		if req.amt != chanSize {
			t.Fatalf("expected channel size of %v, got %v",
				chanSize, req.amt)
		}

	case <-time.After(time.Second * 3):
		t.Fatalf("select wasn't queried in time")
	}

	// Respond with the given scores.
	select {
	case testCtx.heuristic.nodeScoresResps <- nodeScores:
	case <-time.After(time.Second * 3):
		t.Fatalf("NodeScores wasn't queried in time")
	}
}

// checkChannelOpens asserts that the channel controller attempts open the
// number of channels we expect, and with the exact total allocation.
func checkChannelOpens(t *testing.T, testCtx *testContext,
	allocation btcutil.Amount, numChans int) []NodeID {

	var nodes []NodeID

	// The agent should attempt to open channels, totaling what we expect.
	var totalAllocation btcutil.Amount
	chanController := testCtx.chanController.(*mockChanController)
	for i := 0; i < numChans; i++ {
		select {
		case openChan := <-chanController.openChanSignals:
			totalAllocation += openChan.amt

			testCtx.Lock()
			testCtx.walletBalance -= openChan.amt
			testCtx.Unlock()

			nodes = append(nodes, NewNodeID(openChan.target))

		case <-time.After(time.Second * 3):
			t.Fatalf("channel not opened in time")
		}
	}

	if totalAllocation != allocation {
		t.Fatalf("expected agent to open channels totalling %v, "+
			"instead was %v", allocation, totalAllocation)
	}

	// Finally, make sure the agent won't try opening more channels.
	select {
	case <-chanController.openChanSignals:
		t.Fatalf("agent unexpectedly opened channel")

	case <-time.After(50 * time.Millisecond):
	}

	return nodes
}

// TestAgentChannelSizeAllocation tests that the autopilot agent opens channel
// of size that stays within the channel budget and size restrictions.
func TestAgentChannelSizeAllocation(t *testing.T) {
	t.Parallel()

	// Total number of nodes in our mock graph.
	const numNodes = 10

	testCtx, cleanup := setup(t, nil)
	defer cleanup()

	nodeScores := make(map[NodeID]*NodeScore)
	for i := 0; i < numNodes; i++ {
		nodeKey, err := testCtx.graph.addRandNode()
		if err != nil {
			t.Fatalf("unable to generate key: %v", err)
		}
		nodeID := NewNodeID(nodeKey)
		nodeScores[nodeID] = &NodeScore{
			NodeID: nodeID,
			Score:  0.5,
		}
	}

	// The agent should now query the heuristic in order to determine its
	// next action as it local state has now been modified.
	select {
	case arg := <-testCtx.constraints.moreChanArgs:
		if len(arg.chans) != 0 {
			t.Fatalf("expected agent to have no channels open, "+
				"had %v", len(arg.chans))
		}
		if arg.balance != testCtx.walletBalance {
			t.Fatalf("expectd agent to have %v balance, had %v",
				testCtx.walletBalance, arg.balance)
		}
	case <-time.After(time.Second * 3):
		t.Fatalf("heuristic wasn't queried in time")
	}

	// We'll return a response telling the agent to open 5 channels, with a
	// total channel budget of 5 BTC.
	var channelBudget btcutil.Amount = 5 * btcutil.SatoshiPerBitcoin
	numExistingChannels := 0
	numNewChannels := 5
	respondWithScores(
		t, testCtx, channelBudget, numExistingChannels,
		numNewChannels, nodeScores,
	)

	expectedAllocation := testCtx.constraints.MaxChanSize() * btcutil.Amount(numNewChannels)
	nodes := checkChannelOpens(
		t, testCtx, expectedAllocation, numNewChannels,
	)

	// Delete the selected nodes from our set of scores, to avoid scoring
	// nodes we already have channels to.
	for _, node := range nodes {
		delete(nodeScores, node)
	}

	// TODO(halseth): this loop is a hack to ensure all the attempted
	// channels are accounted for. This happens because the agent will
	// query the ChannelBudget before all the pending channels are added to
	// the map. Fix by adding them to the pending channels map before
	// executing directives in goroutines?
	waitForNumChans := func(expChans int) {
		t.Helper()

	Loop:
		for {
			select {
			case arg := <-testCtx.constraints.moreChanArgs:
				// As long as the number of existing channels
				// is below our expected number of channels,
				// we'll keep responding with "no more
				// channels".
				if len(arg.chans) != expChans {
					select {
					case testCtx.constraints.moreChansResps <- moreChansResp{0, 0}:
					case <-time.After(time.Second * 3):
						t.Fatalf("heuristic wasn't " +
							"queried in time")
					}
					continue
				}

				if arg.balance != testCtx.walletBalance {
					t.Fatalf("expectd agent to have %v "+
						"balance, had %v",
						testCtx.walletBalance,
						arg.balance)
				}
				break Loop

			case <-time.After(time.Second * 3):
				t.Fatalf("heuristic wasn't queried in time")
			}
		}
	}

	// Wait for the agent to have 5 channels.
	waitForNumChans(numNewChannels)

	// Set the channel budget to 1.5 BTC.
	channelBudget = btcutil.SatoshiPerBitcoin * 3 / 2

	// We'll return a response telling the agent to open 3 channels, with a
	// total channel budget of 1.5 BTC.
	numExistingChannels = 5
	numNewChannels = 3
	respondWithScores(
		t, testCtx, channelBudget, numExistingChannels,
		numNewChannels, nodeScores,
	)

	// To stay within the budget, we expect the autopilot to open 2
	// channels.
	checkChannelOpens(t, testCtx, channelBudget, 2)
}

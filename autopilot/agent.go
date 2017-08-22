package autopilot

import (
	"sync"
	"sync/atomic"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// Config couples all the items that that an autopilot agent needs to function.
// All items within the struct MUST be populated for the Agent to be able to
// carry out its duties.
type Config struct {
	// Self is the identity public key of the Lightning Network node that
	// is being driven by the agent. This is used to ensure that we don't
	// accidentally attempt to open a channel with ourselves.
	Self *btcec.PublicKey

	// Heuristic is an attachment heuristic which will govern to whom we
	// open channels to, and also what those channels look like in terms of
	// desired capacity. The Heuristic will take into account the current
	// state of the graph, our set of open channels, and the amount of
	// available funds when determining how channels are to be opened.
	// Additionally, a heuristic make also factor in extra-graph
	// information in order to make more pertinent recommendations.
	Heuristic AttachmentHeuristic

	// ChanController is an interface that is able to directly manage the
	// creation, closing and update of channels within the network.
	ChanController ChannelController

	// WalletBalance is a function closure that should return the current
	// available balance o the backing wallet.
	WalletBalance func() (btcutil.Amount, error)

	// Graph is an abstract channel graph that the Heuristic and the Agent
	// will use to make decisions w.r.t channel allocation and placement
	// within the graph.
	Graph ChannelGraph

	// TODO(roasbeef): add additional signals from fee rates and revenue of
	// currently opened channels
}

// channelState is a type that represents the set of active channels of the
// backing LN node that the Agent should be ware of. This type contains a few
// helper utility methods.
type channelState map[lnwire.ShortChannelID]Channel

// Channels returns a slice of all the active channels.
func (c channelState) Channels() []Channel {
	chans := make([]Channel, 0, len(c))
	for _, channel := range c {
		chans = append(chans, channel)
	}
	return chans
}

// ConnectedNodes returns the set of nodes we currently have a channel with.
// This information is needed as we want to avoid making repeated channels with
// any node.
func (c channelState) ConnectedNodes() map[NodeID]struct{} {
	nodes := make(map[NodeID]struct{})
	for _, channels := range c {
		nodes[channels.Node] = struct{}{}
	}

	// TODO(roasbeef): add outgoing, nodes, allow incoming and outgoing to
	// per node
	//  * only add node is chan as funding amt set

	return nodes
}

// Agent implements a closed-loop control system which seeks to autonomously
// optimize the allocation of satoshis within channels throughput the network's
// channel graph. An agent is configurable by swapping out different
// AttachmentHeuristic strategies. The agent uses external signals such as the
// wallet balance changing, or new channels being opened/closed for the local
// node as an indicator to re-examine its internal state, and the amount of
// available funds in order to make updated decisions w.r.t the channel graph.
// The Agent will automatically open, close, and splice in/out channel as
// necessary for it to step closer to its optimal state.
//
// TODO(roasbeef): prob re-word
type Agent struct {
	// Only to be used atomically.
	started uint32
	stopped uint32

	// cfg houses the configuration state of the Ant.
	cfg Config

	// chanState tracks the current set of open channels.
	chanState channelState

	// stateUpdates is a channel that any external state updates that may
	// affect the heuristics of the agent will be sent over.
	stateUpdates chan interface{}

	// totalBalance is the total number of satoshis the backing wallet is
	// known to control at any given instance. This value will be updated
	// when the agent receives external balance update signals.
	totalBalance btcutil.Amount

	quit chan struct{}
	wg   sync.WaitGroup
}

// New creates a new instance of the Agent instantiated using the passed
// configuration and initial channel state. The initial channel state slice
// should be populated with the set of Channels that are currently opened by
// the backing Lightning Node.
func New(cfg Config, initialState []Channel) (*Agent, error) {
	a := &Agent{
		cfg:          cfg,
		chanState:    make(map[lnwire.ShortChannelID]Channel),
		quit:         make(chan struct{}),
		stateUpdates: make(chan interface{}),
	}

	for _, c := range initialState {
		a.chanState[c.ChanID] = c
	}

	return a, nil
}

// Start starts the agent along with any goroutines it needs to perform its
// normal duties.
func (a *Agent) Start() error {
	if !atomic.CompareAndSwapUint32(&a.started, 0, 1) {
		return nil
	}

	log.Infof("Autopilot Agent starting")

	startingBalance, err := a.cfg.WalletBalance()
	if err != nil {
		return err
	}

	a.wg.Add(1)
	go a.controller(startingBalance)

	return nil
}

// Stop signals the Agent to gracefully shutdown. This function will block
// until all goroutines have exited.
func (a *Agent) Stop() error {
	if !atomic.CompareAndSwapUint32(&a.stopped, 0, 1) {
		return nil
	}

	log.Infof("Autopilot Agent stopping")

	close(a.quit)
	a.wg.Wait()

	return nil
}

// balanceUpdate is a type of external state update that reflects an
// increase/decrease in the funds currently available to the wallet.
type balanceUpdate struct {
	balanceDelta btcutil.Amount
}

// chanOpenUpdate is a type of external state update the indicates a new
// channel has been opened, either by the Agent itself (within the main
// controller loop), or by an external user to the system.
type chanOpenUpdate struct {
	newChan Channel
}

// chanCloseUpdate is a type of external state update that indicates that the
// backing Lightning Node has closed a previously open channel.
type chanCloseUpdate struct {
	closedChans []lnwire.ShortChannelID
}

// OnBalanceChange is a callback that should be executed each the balance of
// the backing wallet changes.
func (a *Agent) OnBalanceChange(delta btcutil.Amount) {
	go func() {
		a.stateUpdates <- &balanceUpdate{
			balanceDelta: delta,
		}
	}()
}

// OnChannelOpen is a callback that should be executed each time a new channel
// is manually opened by the user or any system outside the autopilot agent.
func (a *Agent) OnChannelOpen(c Channel) {
	go func() {
		a.stateUpdates <- &chanOpenUpdate{
			newChan: c,
		}
	}()
}

// OnChannelClose is a callback that should be executed each time a prior
// channel has been closed for any reason. This includes regular
// closes, force closes, and channel breaches.
func (a *Agent) OnChannelClose(closedChans ...lnwire.ShortChannelID) {
	go func() {
		a.stateUpdates <- &chanCloseUpdate{
			closedChans: closedChans,
		}
	}()
}

// mergeNodeMaps merges the Agent's set of nodes that it already has active
// channels open to, with the set of nodes that are pending new channels. This
// ensures that the Agent doesn't attempt to open any "duplicate" channels to
// the same node.
func mergeNodeMaps(a map[NodeID]struct{},
	b map[NodeID]Channel) map[NodeID]struct{} {

	c := make(map[NodeID]struct{}, len(a)+len(b))
	for nodeID := range a {
		c[nodeID] = struct{}{}
	}
	for nodeID := range b {
		c[nodeID] = struct{}{}
	}

	return c
}

// mergeChanState merges the Agent's set of active channels, with the set of
// channels awaiting confirmation. This ensures that the agent doesn't go over
// the prescribed channel limit or fund allocation limit.
func mergeChanState(pendingChans map[NodeID]Channel,
	activeChans channelState) []Channel {

	numChans := len(pendingChans) + len(activeChans)
	totalChans := make([]Channel, 0, numChans)

	for _, activeChan := range activeChans.Channels() {
		totalChans = append(totalChans, activeChan)
	}
	for _, pendingChan := range pendingChans {
		totalChans = append(totalChans, pendingChan)
	}

	return totalChans
}

// controller implements the closed-loop control system of the Agent. The
// controller will make a decision w.r.t channel placement within the graph
// based on: it's current internal state of the set of active channels open,
// and external state changes as a result of decisions it  makes w.r.t channel
// allocation, or attributes affecting its control loop being updated by the
// backing Lightning Node.
func (a *Agent) controller(startingBalance btcutil.Amount) {
	defer a.wg.Done()

	// We'll start off by assigning our starting balance, and injecting
	// that amount as an initial wake up to the main controller goroutine.
	a.OnBalanceChange(startingBalance)

	// TODO(roasbeef): do we in fact need to maintain order?
	//  * use sync.Cond if so

	// pendingOpens tracks the channels that we've requested to be
	// initiated, but haven't yet been confirmed as being fully opened.
	// This state is required as otherwise, we may go over our allotted
	// channel limit, or open multiple channels to the same node.
	pendingOpens := make(map[NodeID]Channel)
	var pendingMtx sync.Mutex

	// TODO(roasbeef): add 10-minute wake up timer
	for {
		select {
		// A new external signal has arrived. We'll use this to update
		// our internal state, then determine if we should trigger a
		// channel state modification (open/close, splice in/out).
		case signal := <-a.stateUpdates:
			log.Infof("Processing new external signal")

			switch update := signal.(type) {
			// The balance of the backing wallet has changed, if
			// more funds are now available, we may attempt to open
			// up an additional channel, or splice in funds to an
			// existing one.
			case *balanceUpdate:
				log.Debugf("Applying external balance state "+
					"update of: %v", update.balanceDelta)

				a.totalBalance += update.balanceDelta

			// A new channel has been opened successfully. This was
			// either opened by the Agent, or an external system
			// that is able to drive the Lightning Node.
			case *chanOpenUpdate:
				log.Debugf("New channel successfully opened, "+
					"updating state with: %v",
					spew.Sdump(update.newChan))

				newChan := update.newChan
				a.chanState[newChan.ChanID] = newChan

				pendingMtx.Lock()
				delete(pendingOpens, newChan.Node)
				pendingMtx.Unlock()

			// A channel has been closed, this may free up an
			// available slot, triggering a new channel update.
			case *chanCloseUpdate:
				log.Debugf("Applying closed channel "+
					"updates: %v",
					spew.Sdump(update.closedChans))

				for _, closedChan := range update.closedChans {
					delete(a.chanState, closedChan)
				}
			}

			log.Debugf("Pending channels: %v", spew.Sdump(pendingOpens))

			// With all the updates applied, we'll obtain a set of
			// the current active channels (confirmed channels),
			// and also factor in our set of unconfirmed channels.
			confirmedChans := a.chanState
			pendingMtx.Lock()
			totalChans := mergeChanState(pendingOpens, confirmedChans)
			pendingMtx.Unlock()

			// Now that we've updated our internal state, we'll
			// consult our channel attachment heuristic to
			// determine if we should open up any additional
			// channels or modify existing channels.
			availableFunds, needMore := a.cfg.Heuristic.NeedMoreChans(
				totalChans, a.totalBalance,
			)
			if !needMore {
				continue
			}

			log.Infof("Triggering attachment directive dispatch")

			// We're to attempt an attachment so we'll o obtain the
			// set of nodes that we currently have channels with so
			// we avoid duplicate edges.
			connectedNodes := a.chanState.ConnectedNodes()
			pendingMtx.Lock()
			nodesToSkip := mergeNodeMaps(connectedNodes, pendingOpens)
			pendingMtx.Unlock()

			// If we reach this point, then according to our
			// heuristic we should modify our channel state to tend
			// towards what it determines to the optimal state. So
			// we'll call Select to get a fresh batch of attachment
			// directives, passing in the amount of funds available
			// for us to use.
			chanCandidates, err := a.cfg.Heuristic.Select(
				a.cfg.Self, a.cfg.Graph, availableFunds,
				nodesToSkip,
			)
			if err != nil {
				log.Errorf("Unable to select candidates for "+
					"attachment: %v", err)
				continue
			}

			if len(chanCandidates) == 0 {
				log.Infof("No eligible candidates to connect to")
				continue
			}

			log.Infof("Attempting to execute channel attachment "+
				"directives: %v", spew.Sdump(chanCandidates))

			// For each recommended attachment directive, we'll
			// launch a new goroutine to attempt to carry out the
			// directive. If any of these succeed, then we'll
			// receive a new state update, taking us back to the
			// top of our controller loop.
			pendingMtx.Lock()
			for _, chanCandidate := range chanCandidates {
				nID := NewNodeID(chanCandidate.PeerKey)
				pendingOpens[nID] = Channel{
					Capacity: chanCandidate.ChanAmt,
					Node:     nID,
				}

				go func(directive AttachmentDirective) {
					pub := directive.PeerKey
					err := a.cfg.ChanController.OpenChannel(
						directive.PeerKey,
						directive.ChanAmt,
						directive.Addrs,
					)
					if err != nil {
						log.Warnf("Unable to open "+
							"channel to %x of %v: %v",
							pub.SerializeCompressed(),
							directive.ChanAmt, err)

						// As the attempt failed, we'll
						// clear it from the set of
						// pending channels.
						pendingMtx.Lock()
						nID := NewNodeID(directive.PeerKey)
						delete(pendingOpens, nID)
						pendingMtx.Unlock()

					}

				}(chanCandidate)
			}
			pendingMtx.Unlock()

		// The agent has been signalled to exit, so we'll bail out
		// immediately.
		case <-a.quit:
			return
		}
	}
}

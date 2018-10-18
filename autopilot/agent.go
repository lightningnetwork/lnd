package autopilot

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Config couples all the items that an autopilot agent needs to function.
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

	// ConnectToPeer attempts to connect to the peer using one of its
	// advertised addresses. The boolean returned signals whether the peer
	// was already connected.
	ConnectToPeer func(*btcec.PublicKey, []net.Addr) (bool, error)

	// DisconnectPeer attempts to disconnect the peer with the given public
	// key.
	DisconnectPeer func(*btcec.PublicKey) error

	// WalletBalance is a function closure that should return the current
	// available balance of the backing wallet.
	WalletBalance func() (btcutil.Amount, error)

	// Graph is an abstract channel graph that the Heuristic and the Agent
	// will use to make decisions w.r.t channel allocation and placement
	// within the graph.
	Graph ChannelGraph

	// MaxPendingOpens is the maximum number of pending channel
	// establishment goroutines that can be lingering. We cap this value in
	// order to control the level of parallelism caused by the autopilot
	// agent.
	MaxPendingOpens uint16

	// TODO(roasbeef): add additional signals from fee rates and revenue of
	// currently opened channels
}

// channelState is a type that represents the set of active channels of the
// backing LN node that the Agent should be aware of. This type contains a few
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

	// balanceUpdates is a channel where notifications about updates to the
	// wallet's balance will be sent. This channel will be buffered to
	// ensure we have at most one pending update of this type to handle at
	// a given time.
	balanceUpdates chan *balanceUpdate

	// nodeUpdates is a channel that changes to the graph node landscape
	// will be sent over. This channel will be buffered to ensure we have
	// at most one pending update of this type to handle at a given time.
	nodeUpdates chan *nodeUpdates

	// pendingOpenUpdates is a channel where updates about channel pending
	// opening will be sent. This channel will be buffered to ensure we
	// have at most one pending update of this type to handle at a given
	// time.
	pendingOpenUpdates chan *chanPendingOpenUpdate

	// chanOpenFailures is a channel where updates about channel open
	// failures will be sent. This channel will be buffered to ensure we
	// have at most one pending update of this type to handle at a given
	// time.
	chanOpenFailures chan *chanOpenFailureUpdate

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
		cfg:                cfg,
		chanState:          make(map[lnwire.ShortChannelID]Channel),
		quit:               make(chan struct{}),
		stateUpdates:       make(chan interface{}),
		balanceUpdates:     make(chan *balanceUpdate, 1),
		nodeUpdates:        make(chan *nodeUpdates, 1),
		chanOpenFailures:   make(chan *chanOpenFailureUpdate, 1),
		pendingOpenUpdates: make(chan *chanPendingOpenUpdate, 1),
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

	a.wg.Add(1)
	go a.controller()

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
}

// nodeUpdates is a type of external state update that reflects an addition or
// modification in channel graph node membership.
type nodeUpdates struct{}

// chanOpenUpdate is a type of external state update that indicates a new
// channel has been opened, either by the Agent itself (within the main
// controller loop), or by an external user to the system.
type chanOpenUpdate struct {
	newChan Channel
}

// chanPendingOpenUpdate is a type of external state update that indicates a new
// channel has been opened, either by the agent itself or an external subsystem,
// but is still pending.
type chanPendingOpenUpdate struct{}

// chanOpenFailureUpdate is a type of external state update that indicates
// a previous channel open failed, and that it might be possible to try again.
type chanOpenFailureUpdate struct{}

// chanCloseUpdate is a type of external state update that indicates that the
// backing Lightning Node has closed a previously open channel.
type chanCloseUpdate struct {
	closedChans []lnwire.ShortChannelID
}

// OnBalanceChange is a callback that should be executed each time the balance
// of the backing wallet changes.
func (a *Agent) OnBalanceChange() {
	select {
	case a.balanceUpdates <- &balanceUpdate{}:
	default:
	}
}

// OnNodeUpdates is a callback that should be executed each time our channel
// graph has new nodes or their node announcements are updated.
func (a *Agent) OnNodeUpdates() {
	select {
	case a.nodeUpdates <- &nodeUpdates{}:
	default:
	}
}

// OnChannelOpen is a callback that should be executed each time a new channel
// is manually opened by the user or any system outside the autopilot agent.
func (a *Agent) OnChannelOpen(c Channel) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		select {
		case a.stateUpdates <- &chanOpenUpdate{newChan: c}:
		case <-a.quit:
		}
	}()
}

// OnChannelPendingOpen is a callback that should be executed each time a new
// channel is opened, either by the agent or an external subsystems, but is
// still pending.
func (a *Agent) OnChannelPendingOpen() {
	select {
	case a.pendingOpenUpdates <- &chanPendingOpenUpdate{}:
	default:
	}
}

// OnChannelOpenFailure is a callback that should be executed when the
// autopilot has attempted to open a channel, but failed. In this case we can
// retry channel creation with a different node.
func (a *Agent) OnChannelOpenFailure() {
	select {
	case a.chanOpenFailures <- &chanOpenFailureUpdate{}:
	default:
	}
}

// OnChannelClose is a callback that should be executed each time a prior
// channel has been closed for any reason. This includes regular
// closes, force closes, and channel breaches.
func (a *Agent) OnChannelClose(closedChans ...lnwire.ShortChannelID) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		select {
		case a.stateUpdates <- &chanCloseUpdate{closedChans: closedChans}:
		case <-a.quit:
		}
	}()
}

// mergeNodeMaps merges the Agent's set of nodes that it already has active
// channels open to, with the other sets of nodes that should be removed from
// consideration during heuristic selection. This ensures that the Agent doesn't
// attempt to open any "duplicate" channels to the same node.
func mergeNodeMaps(c map[NodeID]Channel,
	skips ...map[NodeID]struct{}) map[NodeID]struct{} {

	numNodes := len(c)
	for _, skip := range skips {
		numNodes += len(skip)
	}

	res := make(map[NodeID]struct{}, len(c)+numNodes)
	for nodeID := range c {
		res[nodeID] = struct{}{}
	}
	for _, skip := range skips {
		for nodeID := range skip {
			res[nodeID] = struct{}{}
		}
	}

	return res
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
// based on: its current internal state of the set of active channels open,
// and external state changes as a result of decisions it  makes w.r.t channel
// allocation, or attributes affecting its control loop being updated by the
// backing Lightning Node.
func (a *Agent) controller() {
	defer a.wg.Done()

	// We'll start off by assigning our starting balance, and injecting
	// that amount as an initial wake up to the main controller goroutine.
	a.OnBalanceChange()

	// TODO(roasbeef): do we in fact need to maintain order?
	//  * use sync.Cond if so

	// failedNodes lists nodes that we've previously attempted to initiate
	// channels with, but didn't succeed.
	failedNodes := make(map[NodeID]struct{})

	// pendingConns tracks the nodes that we are attempting to make
	// connections to. This prevents us from making duplicate connection
	// requests to the same node.
	pendingConns := make(map[NodeID]struct{})

	// pendingOpens tracks the channels that we've requested to be
	// initiated, but haven't yet been confirmed as being fully opened.
	// This state is required as otherwise, we may go over our allotted
	// channel limit, or open multiple channels to the same node.
	pendingOpens := make(map[NodeID]Channel)
	var pendingMtx sync.Mutex

	updateBalance := func() {
		newBalance, err := a.cfg.WalletBalance()
		if err != nil {
			log.Warnf("unable to update wallet balance: %v", err)
			return
		}

		a.totalBalance = newBalance
	}

	// TODO(roasbeef): add 10-minute wake up timer
	for {
		select {
		// A new external signal has arrived. We'll use this to update
		// our internal state, then determine if we should trigger a
		// channel state modification (open/close, splice in/out).
		case signal := <-a.stateUpdates:
			log.Infof("Processing new external signal")

			switch update := signal.(type) {
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

				updateBalance()
			// A channel has been closed, this may free up an
			// available slot, triggering a new channel update.
			case *chanCloseUpdate:
				log.Debugf("Applying closed channel "+
					"updates: %v",
					spew.Sdump(update.closedChans))

				for _, closedChan := range update.closedChans {
					delete(a.chanState, closedChan)
				}

				updateBalance()
			}

		// A new channel has been opened by the agent or an external
		// subsystem, but is still pending confirmation.
		case <-a.pendingOpenUpdates:
			updateBalance()

		// The balance of the backing wallet has changed, if more funds
		// are now available, we may attempt to open up an additional
		// channel, or splice in funds to an existing one.
		case <-a.balanceUpdates:
			log.Debug("Applying external balance state update")

			updateBalance()

		// The channel we tried to open previously failed for whatever
		// reason.
		case <-a.chanOpenFailures:
			log.Debug("Retrying after previous channel open " +
				"failure.")

			updateBalance()

		// New nodes have been added to the graph or their node
		// announcements have been updated. We will consider opening
		// channels to these nodes if we haven't stabilized.
		case <-a.nodeUpdates:
			log.Infof("Node updates received, assessing " +
				"need for more channels")

		// The agent has been signalled to exit, so we'll bail out
		// immediately.
		case <-a.quit:
			return
		}

		pendingMtx.Lock()
		log.Debugf("Pending channels: %v", spew.Sdump(pendingOpens))
		pendingMtx.Unlock()

		// With all the updates applied, we'll obtain a set of the
		// current active channels (confirmed channels), and also
		// factor in our set of unconfirmed channels.
		confirmedChans := a.chanState
		pendingMtx.Lock()
		totalChans := mergeChanState(pendingOpens, confirmedChans)
		pendingMtx.Unlock()

		// Now that we've updated our internal state, we'll consult our
		// channel attachment heuristic to determine if we should open
		// up any additional channels or modify existing channels.
		availableFunds, numChans, needMore := a.cfg.Heuristic.NeedMoreChans(
			totalChans, a.totalBalance,
		)
		if !needMore {
			continue
		}

		log.Infof("Triggering attachment directive dispatch, "+
			"total_funds=%v", a.totalBalance)

		// We're to attempt an attachment so we'll obtain the set of
		// nodes that we currently have channels with so we avoid
		// duplicate edges.
		connectedNodes := a.chanState.ConnectedNodes()
		pendingMtx.Lock()
		nodesToSkip := mergeNodeMaps(pendingOpens,
			pendingConns, connectedNodes, failedNodes,
		)
		pendingMtx.Unlock()

		// If we reach this point, then according to our heuristic we
		// should modify our channel state to tend towards what it
		// determines to the optimal state. So we'll call Select to get
		// a fresh batch of attachment directives, passing in the
		// amount of funds available for us to use.
		chanCandidates, err := a.cfg.Heuristic.Select(
			a.cfg.Self, a.cfg.Graph, availableFunds,
			numChans, nodesToSkip,
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

		// Before proceeding, check to see if we have any slots
		// available to open channels. If there are any, we will attempt
		// to dispatch the retrieved directives since we can't be
		// certain which ones may actually succeed. If too many
		// connections succeed, we will they will be ignored and made
		// available to future heuristic selections.
		pendingMtx.Lock()
		if uint16(len(pendingOpens)) >= a.cfg.MaxPendingOpens {
			pendingMtx.Unlock()
			log.Debugf("Reached cap of %v pending "+
				"channel opens, will retry "+
				"after success/failure",
				a.cfg.MaxPendingOpens)
			continue
		}

		// For each recommended attachment directive, we'll launch a
		// new goroutine to attempt to carry out the directive. If any
		// of these succeed, then we'll receive a new state update,
		// taking us back to the top of our controller loop.
		for _, chanCandidate := range chanCandidates {
			// Skip candidates which we are already trying
			// to establish a connection with.
			nodeID := chanCandidate.NodeID
			if _, ok := pendingConns[nodeID]; ok {
				continue
			}
			pendingConns[nodeID] = struct{}{}

			go func(directive AttachmentDirective) {
				// We'll start out by attempting to connect to
				// the peer in order to begin the funding
				// workflow.
				pub := directive.NodeKey
				alreadyConnected, err := a.cfg.ConnectToPeer(
					pub, directive.Addrs,
				)
				if err != nil {
					log.Warnf("Unable to connect "+
						"to %x: %v",
						pub.SerializeCompressed(),
						err)

					// Since we failed to connect to them,
					// we'll mark them as failed so that we
					// don't attempt to connect to them
					// again.
					nodeID := NewNodeID(pub)
					pendingMtx.Lock()
					delete(pendingConns, nodeID)
					failedNodes[nodeID] = struct{}{}
					pendingMtx.Unlock()

					// Finally, we'll trigger the agent to
					// select new peers to connect to.
					a.OnChannelOpenFailure()

					return
				}

				// The connection was successful, though before
				// progressing we must check that we have not
				// already met our quota for max pending open
				// channels. This can happen if multiple
				// directives were spawned but fewer slots were
				// available, and other successful attempts
				// finished first.
				pendingMtx.Lock()
				if uint16(len(pendingOpens)) >=
					a.cfg.MaxPendingOpens {
					// Since we've reached our max number of
					// pending opens, we'll disconnect this
					// peer and exit. However, if we were
					// previously connected to them, then
					// we'll make sure to maintain the
					// connection alive.
					if alreadyConnected {
						// Since we succeeded in
						// connecting, we won't add this
						// peer to the failed nodes map,
						// but we will remove it from
						// pendingConns so that it can
						// be retried in the future.
						delete(pendingConns, nodeID)
						pendingMtx.Unlock()
						return
					}

					err = a.cfg.DisconnectPeer(
						pub,
					)
					if err != nil {
						log.Warnf("Unable to "+
							"disconnect peer "+
							"%x: %v",
							pub.SerializeCompressed(),
							err)
					}

					// Now that we have disconnected, we can
					// remove this node from our pending
					// conns map, permitting subsequent
					// connection attempts.
					delete(pendingConns, nodeID)
					pendingMtx.Unlock()
					return
				}

				// If we were successful, we'll track this peer
				// in our set of pending opens. We do this here
				// to ensure we don't stall on selecting new
				// peers if the connection attempt happens to
				// take too long.
				nodeID := directive.NodeID
				delete(pendingConns, nodeID)
				pendingOpens[nodeID] = Channel{
					Capacity: directive.ChanAmt,
					Node:     nodeID,
				}
				pendingMtx.Unlock()

				// We can then begin the funding workflow with
				// this peer.
				err = a.cfg.ChanController.OpenChannel(
					pub, directive.ChanAmt,
				)
				if err != nil {
					log.Warnf("Unable to open "+
						"channel to %x of %v: %v",
						pub.SerializeCompressed(),
						directive.ChanAmt, err)

					// As the attempt failed, we'll clear
					// the peer from the set of pending
					// opens and mark them as failed so we
					// don't attempt to open a channel to
					// them again.
					pendingMtx.Lock()
					delete(pendingOpens, nodeID)
					failedNodes[nodeID] = struct{}{}
					pendingMtx.Unlock()

					// Trigger the agent to re-evaluate
					// everything and possibly retry with a
					// different node.
					a.OnChannelOpenFailure()

					// Finally, we should also disconnect
					// the peer if we weren't already
					// connected to them beforehand by an
					// external subsystem.
					if alreadyConnected {
						return
					}

					err = a.cfg.DisconnectPeer(pub)
					if err != nil {
						log.Warnf("Unable to "+
							"disconnect peer "+
							"%x: %v",
							pub.SerializeCompressed(),
							err)
					}
				}

				// Since the channel open was successful and is
				// currently pending, we'll trigger the
				// autopilot agent to query for more peers.
				a.OnChannelPendingOpen()
			}(chanCandidate)
		}
		pendingMtx.Unlock()

	}
}

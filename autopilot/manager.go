package autopilot

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

// ManagerCfg houses a set of values and methods that is passed to the Manager
// for it to properly manage its autopilot agent.
type ManagerCfg struct {
	// Self is the public key of the lnd instance. It is used to making
	// sure the autopilot is not opening channels to itself.
	Self *btcec.PublicKey

	// PilotCfg is the config of the autopilot agent managed by the
	// Manager.
	PilotCfg *Config

	// ChannelState is a function closure that returns the current set of
	// channels managed by this node.
	ChannelState func() ([]Channel, error)

	// SubscribeTransactions is used to get a subscription for transactions
	// relevant to this node's wallet.
	SubscribeTransactions func() (lnwallet.TransactionSubscription, error)

	// SubscribeTopology is used to get a subscription for topology changes
	// on the network.
	SubscribeTopology func() (*routing.TopologyClient, error)
}

// Manager is struct that manages an autopilot agent, making it possible to
// enable and disable it at will, and hand it relevant external information.
// It implements the autopilot grpc service, which is used to get data about
// the running autopilot, and give it relevant information.
type Manager struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	cfg *ManagerCfg

	// pilot is the current autopilot agent. It will be nil if the agent is
	// disabled.
	pilot *Agent

	quit chan struct{}
	wg   sync.WaitGroup
	sync.Mutex
}

// NewManager creates a new instance of the Manager from the passed config.
func NewManager(cfg *ManagerCfg) (*Manager, error) {
	return &Manager{
		cfg:  cfg,
		quit: make(chan struct{}),
	}, nil
}

// Start starts the Manager.
func (m *Manager) Start() error {
	if !atomic.CompareAndSwapUint32(&m.started, 0, 1) {
		return nil
	}

	return nil
}

// Stop stops the Manager. If an autopilot agent is active, it will also be
// stopped.
func (m *Manager) Stop() error {
	if !atomic.CompareAndSwapUint32(&m.stopped, 0, 1) {
		return nil
	}

	if err := m.StopAgent(); err != nil {
		log.Errorf("Unable to stop pilot: %v", err)
	}

	close(m.quit)
	m.wg.Wait()

	return nil
}

// IsActive returns whether the autopilot agent is currently active.
func (m *Manager) IsActive() bool {
	m.Lock()
	defer m.Unlock()

	return m.pilot != nil
}

// StartAgent creates and starts an autopilot agent from the Manager's
// config.
func (m *Manager) StartAgent() error {
	m.Lock()
	defer m.Unlock()

	// Already active.
	if m.pilot != nil {
		return nil
	}

	// Next, we'll fetch the current state of open channels from the
	// database to use as initial state for the auto-pilot agent.
	initialChanState, err := m.cfg.ChannelState()
	if err != nil {
		return err
	}

	// Now that we have all the initial dependencies, we can create the
	// auto-pilot instance itself.
	pilot, err := New(*m.cfg.PilotCfg, initialChanState)
	if err != nil {
		return err
	}

	if err := pilot.Start(); err != nil {
		return err
	}

	// Finally, we'll need to subscribe to two things: incoming
	// transactions that modify the wallet's balance, and also any graph
	// topology updates.
	txnSubscription, err := m.cfg.SubscribeTransactions()
	if err != nil {
		pilot.Stop()
		return err
	}
	graphSubscription, err := m.cfg.SubscribeTopology()
	if err != nil {
		txnSubscription.Cancel()
		pilot.Stop()
		return err
	}

	m.pilot = pilot

	// We'll launch a goroutine to provide the agent with notifications
	// whenever the balance of the wallet changes.
	// TODO(halseth): can lead to panic if in process of shutting down.
	m.wg.Add(1)
	go func() {
		defer txnSubscription.Cancel()
		defer m.wg.Done()

		for {
			select {
			case <-txnSubscription.ConfirmedTransactions():
				pilot.OnBalanceChange()

			// We won't act upon new unconfirmed transaction, as
			// we'll only use confirmed outputs when funding.
			// However, we will still drain this request in order
			// to avoid goroutine leaks, and ensure we promptly
			// read from the channel if available.
			case <-txnSubscription.UnconfirmedTransactions():
			case <-pilot.quit:
				return
			case <-m.quit:
				return
			}
		}

	}()

	// We'll also launch a goroutine to provide the agent with
	// notifications for when the graph topology controlled by the node
	// changes.
	m.wg.Add(1)
	go func() {
		defer graphSubscription.Cancel()
		defer m.wg.Done()

		for {
			select {
			case topChange, ok := <-graphSubscription.TopologyChanges:
				// If the router is shutting down, then we will
				// as well.
				if !ok {
					return
				}

				for _, edgeUpdate := range topChange.ChannelEdgeUpdates {
					// If this isn't an advertisement by
					// the backing lnd node, then we'll
					// continue as we only want to add
					// channels that we've created
					// ourselves.
					if !edgeUpdate.AdvertisingNode.IsEqual(m.cfg.Self) {
						continue
					}

					// If this is indeed a channel we
					// opened, then we'll convert it to the
					// autopilot.Channel format, and notify
					// the pilot of the new channel.
					chanNode := NewNodeID(
						edgeUpdate.ConnectingNode,
					)
					chanID := lnwire.NewShortChanIDFromInt(
						edgeUpdate.ChanID,
					)
					edge := Channel{
						ChanID:   chanID,
						Capacity: edgeUpdate.Capacity,
						Node:     chanNode,
					}
					pilot.OnChannelOpen(edge)
				}

				// For each closed channel, we'll obtain
				// the chanID of the closed channel and send it
				// to the pilot.
				for _, chanClose := range topChange.ClosedChannels {
					chanID := lnwire.NewShortChanIDFromInt(
						chanClose.ChanID,
					)

					pilot.OnChannelClose(chanID)
				}

				// If new nodes were added to the graph, or nod
				// information has changed, we'll poke autopilot
				// to see if it can make use of them.
				if len(topChange.NodeUpdates) > 0 {
					pilot.OnNodeUpdates()
				}

			case <-pilot.quit:
				return
			case <-m.quit:
				return
			}
		}
	}()

	log.Debugf("Manager started autopilot agent")

	return nil
}

// StopAgent stops any active autopilot agent.
func (m *Manager) StopAgent() error {
	m.Lock()
	defer m.Unlock()

	// Not active, so we can return early.
	if m.pilot == nil {
		return nil
	}

	if err := m.pilot.Stop(); err != nil {
		return err
	}

	// Make sure to nil the current agent, indicating it is no longer
	// active.
	m.pilot = nil

	log.Debugf("Manager stopped autopilot agent")

	return nil
}

// QueryHeuristics queries the available autopilot heuristics for node scores.
func (m *Manager) QueryHeuristics(nodes []NodeID, localState bool) (
	HeuristicScores, error) {

	m.Lock()
	defer m.Unlock()

	n := make(map[NodeID]struct{})
	for _, node := range nodes {
		n[node] = struct{}{}
	}

	log.Debugf("Querying heuristics for %d nodes", len(n))
	return m.queryHeuristics(n, localState)
}

// HeuristicScores is an alias for a map that maps heuristic names to a map of
// scores for pubkeys.
type HeuristicScores map[string]map[NodeID]float64

// queryHeuristics gets node scores from all available simple heuristics, and
// the agent's current active heuristic.
//
// NOTE: Must be called with the manager's lock.
func (m *Manager) queryHeuristics(nodes map[NodeID]struct{}, localState bool) (
	HeuristicScores, error) {

	// If we want to take the local state into action when querying the
	// heuristics, we fetch it. If not we'll just pass an emply slice to
	// the heuristic.
	var totalChans []Channel
	var err error
	if localState {
		// Fetch the current set of channels.
		totalChans, err = m.cfg.ChannelState()
		if err != nil {
			return nil, err
		}

		// If the agent is active, we can merge the channel state with
		// the channels pending open.
		if m.pilot != nil {
			m.pilot.chanStateMtx.Lock()
			m.pilot.pendingMtx.Lock()
			totalChans = mergeChanState(
				m.pilot.pendingOpens, m.pilot.chanState,
			)
			m.pilot.pendingMtx.Unlock()
			m.pilot.chanStateMtx.Unlock()
		}
	}

	// As channel size we'll use the maximum size.
	chanSize := m.cfg.PilotCfg.Constraints.MaxChanSize()

	// We'll start by getting the scores from each available sub-heuristic,
	// in addition the current agent heuristic.
	report := make(HeuristicScores)
	for _, h := range append(availableHeuristics, m.cfg.PilotCfg.Heuristic) {
		name := h.Name()

		// If the agent heuristic is among the simple heuristics it
		// might get queried more than once. As an optimization we'll
		// just skip it the second time.
		if _, ok := report[name]; ok {
			continue
		}

		s, err := h.NodeScores(
			m.cfg.PilotCfg.Graph, totalChans, chanSize, nodes,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to get sub score: %v",
				err)
		}

		log.Debugf("Heuristic \"%v\" scored %d nodes", name, len(s))

		scores := make(map[NodeID]float64)
		for nID, score := range s {
			scores[nID] = score.Score
		}

		report[name] = scores
	}

	return report, nil
}

// SetNodeScores is used to set the scores of the given heuristic, if it is
// active, and ScoreSettable.
func (m *Manager) SetNodeScores(name string, scores map[NodeID]float64) error {
	// It must be ScoreSettable to be available for external
	// scores.
	s, ok := m.cfg.PilotCfg.Heuristic.(ScoreSettable)
	if !ok {
		return fmt.Errorf("current heuristic doesn't support " +
			"external scoring")
	}

	// Heuristic was found, set its node scores.
	applied, err := s.SetNodeScores(name, scores)
	if err != nil {
		return err
	}

	if !applied {
		return fmt.Errorf("heuristic with name %v not found", name)
	}

	return nil
}

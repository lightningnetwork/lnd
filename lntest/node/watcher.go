package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnutils"
)

type chanWatchType uint8

const (
	// watchOpenChannel specifies that this is a request to watch an open
	// channel event.
	watchOpenChannel chanWatchType = iota

	// watchCloseChannel specifies that this is a request to watch a close
	// channel event.
	watchCloseChannel

	// watchPolicyUpdate specifies that this is a request to watch a policy
	// update event.
	watchPolicyUpdate
)

// chanWatchRequest is a request to the lightningNetworkWatcher to be notified
// once it's detected within the test Lightning Network, that a channel has
// either been added or closed.
type chanWatchRequest struct {
	chanPoint wire.OutPoint

	chanWatchType chanWatchType

	eventChan chan struct{}

	advertisingNode    string
	policy             *lnrpc.RoutingPolicy
	includeUnannounced bool

	// handled is a channel that will be closed once the request has been
	// handled by the topologyWatcher goroutine.
	handled chan struct{}
}

// nodeWatcher is a topology watcher for a HarnessNode. It keeps track of all
// the topology updates seen in a given node, including NodeUpdate,
// ChannelEdgeUpdate, and ClosedChannelUpdate.
type nodeWatcher struct {
	// rpc is the RPC clients used for the current node.
	rpc *rpc.HarnessRPC

	// state is the node's current state.
	state *State

	// chanWatchRequests receives a request for watching a particular event
	// for a given channel.
	chanWatchRequests chan *chanWatchRequest

	// For each outpoint, we'll track an integer which denotes the number
	// of edges seen for that channel within the network. When this number
	// reaches 2, then it means that both edge advertisements has
	// propagated through the network.
	openChanWatchers  *lnutils.SyncMap[wire.OutPoint, []chan struct{}]
	closeChanWatchers *lnutils.SyncMap[wire.OutPoint, []chan struct{}]

	wg sync.WaitGroup
}

func newNodeWatcher(rpc *rpc.HarnessRPC, state *State) *nodeWatcher {
	return &nodeWatcher{
		rpc:               rpc,
		state:             state,
		chanWatchRequests: make(chan *chanWatchRequest, 100),
		openChanWatchers: &lnutils.SyncMap[
			wire.OutPoint, []chan struct{},
		]{},
		closeChanWatchers: &lnutils.SyncMap[
			wire.OutPoint, []chan struct{},
		]{},
	}
}

// GetNumChannelUpdates reads the num of channel updates inside a lock and
// returns the value.
func (nw *nodeWatcher) GetNumChannelUpdates(op wire.OutPoint) int {
	result, _ := nw.state.numChanUpdates.Load(op)
	return result
}

// GetPolicyUpdates returns the node's policyUpdates state.
func (nw *nodeWatcher) GetPolicyUpdates(op wire.OutPoint) PolicyUpdate {
	result, _ := nw.state.policyUpdates.Load(op)
	return result
}

// GetNodeUpdates reads the node updates inside a lock and returns the value.
func (nw *nodeWatcher) GetNodeUpdates(pubkey string) []*lnrpc.NodeUpdate {
	result, _ := nw.state.nodeUpdates.Load(pubkey)
	return result
}

// WaitForNumChannelUpdates will block until a given number of updates has been
// seen in the node's network topology.
func (nw *nodeWatcher) WaitForNumChannelUpdates(op wire.OutPoint,
	expected int) error {

	checkNumUpdates := func() error {
		num := nw.GetNumChannelUpdates(op)
		if num >= expected {
			return nil
		}

		return fmt.Errorf("timeout waiting for num channel updates, "+
			"want %d, got %d", expected, num)
	}

	return wait.NoError(checkNumUpdates, wait.DefaultTimeout)
}

// WaitForNumNodeUpdates will block until a given number of node updates has
// been seen in the node's network topology.
func (nw *nodeWatcher) WaitForNumNodeUpdates(pubkey string,
	expected int) ([]*lnrpc.NodeUpdate, error) {

	updates := make([]*lnrpc.NodeUpdate, 0)
	checkNumUpdates := func() error {
		updates = nw.GetNodeUpdates(pubkey)
		num := len(updates)
		if num >= expected {
			return nil
		}

		return fmt.Errorf("timeout waiting for num node updates, "+
			"want %d, got %d", expected, num)
	}
	err := wait.NoError(checkNumUpdates, wait.DefaultTimeout)

	return updates, err
}

// WaitForChannelOpen will block until a channel with the target outpoint is
// seen as being fully advertised within the network. A channel is considered
// "fully advertised" once both of its directional edges has been advertised in
// the node's network topology.
func (nw *nodeWatcher) WaitForChannelOpen(chanPoint *lnrpc.ChannelPoint) error {
	op := nw.rpc.MakeOutpoint(chanPoint)
	eventChan := make(chan struct{})
	nw.chanWatchRequests <- &chanWatchRequest{
		chanPoint:     op,
		eventChan:     eventChan,
		chanWatchType: watchOpenChannel,
		handled:       make(chan struct{}),
	}

	timer := time.After(wait.DefaultTimeout)
	select {
	case <-eventChan:
		return nil

	case <-timer:
		updates, err := syncMapToJSON(&nw.state.openChans.Map)
		if err != nil {
			return err
		}

		return fmt.Errorf("channel:%s not heard before timeout: "+
			"node has heard: %s", op, updates)
	}
}

// WaitForChannelClose will block until a channel with the target outpoint is
// seen as closed within the node's network topology. A channel is considered
// closed once a transaction spending the funding outpoint is seen within a
// confirmed block.
func (nw *nodeWatcher) WaitForChannelClose(
	chanPoint *lnrpc.ChannelPoint) (*lnrpc.ClosedChannelUpdate, error) {

	op := nw.rpc.MakeOutpoint(chanPoint)
	eventChan := make(chan struct{})
	nw.chanWatchRequests <- &chanWatchRequest{
		chanPoint:     op,
		eventChan:     eventChan,
		chanWatchType: watchCloseChannel,
		handled:       make(chan struct{}),
	}

	timer := time.After(wait.DefaultTimeout)
	select {
	case <-eventChan:
		closedChan, ok := nw.state.closedChans.Load(op)
		if !ok {
			return nil, fmt.Errorf("channel:%s expected to find "+
				"a closed channel in node's state:%s", op,
				nw.state)
		}
		return closedChan, nil

	case <-timer:
		return nil, fmt.Errorf("channel:%s not closed before timeout: "+
			"%s", op, nw.state)
	}
}

// WaitForChannelPolicyUpdate will block until a channel policy with the target
// outpoint and advertisingNode is seen within the network.
func (nw *nodeWatcher) WaitForChannelPolicyUpdate(
	advertisingNode *HarnessNode, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint, includeUnannounced bool) error {

	op := nw.rpc.MakeOutpoint(chanPoint)

	ticker := time.NewTicker(wait.PollInterval)
	timer := time.After(wait.DefaultTimeout)
	defer ticker.Stop()

	// onTimeout is a helper function that will be called in case the
	// expected policy is not found before the timeout.
	onTimeout := func() error {
		expected, err := json.MarshalIndent(policy, "", "\t")
		if err != nil {
			return fmt.Errorf("encode policy err: %w", err)
		}

		policies, err := syncMapToJSON(&nw.state.policyUpdates.Map)
		if err != nil {
			return err
		}

		return fmt.Errorf("policy not updated before timeout:"+
			"\nchannel: %v \nadvertisingNode: %s:%v"+
			"\nwant policy:%s\nhave updates:%s", op,
			advertisingNode.Name(), advertisingNode.PubKeyStr,
			expected, policies)
	}

	var eventChan = make(chan struct{})
	for {
		select {
		// Send a watch request every second.
		case <-ticker.C:
			// Did the event chan close in the meantime? We want to
			// avoid a "close of closed channel" panic since we're
			// re-using the same event chan for multiple requests.
			select {
			case <-eventChan:
				return nil
			default:
			}

			var handled = make(chan struct{})
			nw.chanWatchRequests <- &chanWatchRequest{
				chanPoint:          op,
				eventChan:          eventChan,
				chanWatchType:      watchPolicyUpdate,
				policy:             policy,
				advertisingNode:    advertisingNode.PubKeyStr,
				includeUnannounced: includeUnannounced,
				handled:            handled,
			}

			// We wait for the topologyWatcher to signal that
			// it has completed the handling of the request so that
			// we don't send a new request before the previous one
			// has been processed as this could lead to a double
			// closure of the eventChan channel.
			select {
			case <-handled:
			case <-timer:
				return onTimeout()
			}

		case <-eventChan:
			return nil

		case <-timer:
			return onTimeout()
		}
	}
}

// syncMapToJSON is a helper function that creates json bytes from the sync.Map
// used in the node. Expect the sync.Map to have map[string]interface.
func syncMapToJSON(state *sync.Map) ([]byte, error) {
	m := map[string]interface{}{}
	state.Range(func(k, v interface{}) bool {
		op := k.(wire.OutPoint)
		m[op.String()] = v
		return true
	})
	policies, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return nil, fmt.Errorf("encode polices err: %w", err)
	}

	return policies, nil
}

// topologyWatcher is a goroutine which is able to dispatch notifications once
// it has been observed that a target channel has been closed or opened within
// the network. In order to dispatch these notifications, the
// GraphTopologySubscription client exposed as part of the gRPC interface is
// used.
//
// NOTE: must be run as a goroutine.
func (nw *nodeWatcher) topologyWatcher(ctxb context.Context,
	started chan error) {

	graphUpdates := make(chan *lnrpc.GraphTopologyUpdate)

	client, err := nw.newTopologyClient(ctxb)
	started <- err

	// Exit if there's an error.
	if err != nil {
		return
	}

	// Start a goroutine to receive graph updates.
	nw.wg.Add(1)
	go func() {
		defer nw.wg.Done()

		// With the client being created, we now start receiving the
		// updates.
		err = nw.receiveTopologyClientStream(ctxb, client, graphUpdates)
		if err != nil {
			started <- fmt.Errorf("receiveTopologyClientStream "+
				"got err: %v", err)
		}
	}()

	for {
		select {
		// A new graph update has just been received, so we'll examine
		// the current set of registered clients to see if we can
		// dispatch any requests.
		case graphUpdate := <-graphUpdates:
			nw.handleChannelEdgeUpdates(graphUpdate.ChannelUpdates)
			nw.handleClosedChannelUpdate(graphUpdate.ClosedChans)
			nw.handleNodeUpdates(graphUpdate.NodeUpdates)

		// A new watch request, has just arrived. We'll either be able
		// to dispatch immediately, or need to add the client for
		// processing later.
		case watchRequest := <-nw.chanWatchRequests:
			switch watchRequest.chanWatchType {
			case watchOpenChannel:
				// TODO(roasbeef): add update type also, checks
				// for multiple of 2
				nw.handleOpenChannelWatchRequest(watchRequest)

			case watchCloseChannel:
				nw.handleCloseChannelWatchRequest(watchRequest)

			case watchPolicyUpdate:
				nw.handlePolicyUpdateWatchRequest(watchRequest)
			}

			// Signal to the caller that the request has been
			// handled.
			close(watchRequest.handled)

		case <-ctxb.Done():
			return
		}
	}
}

func (nw *nodeWatcher) handleNodeUpdates(updates []*lnrpc.NodeUpdate) {
	for _, nodeUpdate := range updates {
		nw.updateNodeStateNodeUpdates(nodeUpdate)
	}
}

// handleChannelEdgeUpdates takes a series of channel edge updates, extracts
// the outpoints, and saves them to harness node's internal state.
func (nw *nodeWatcher) handleChannelEdgeUpdates(
	updates []*lnrpc.ChannelEdgeUpdate) {

	// For each new channel, we'll increment the number of edges seen by
	// one.
	for _, newChan := range updates {
		op := nw.rpc.MakeOutpoint(newChan.ChanPoint)

		// Update the num of channel updates.
		nw.updateNodeStateNumChanUpdates(op)

		// Update the open channels.
		nw.updateNodeStateOpenChannel(op, newChan)

		// Check whether there's a routing policy update. If so, save
		// it to the node state.
		if newChan.RoutingPolicy != nil {
			nw.updateNodeStatePolicy(op, newChan)
		}
	}
}

// updateNodeStateNumChanUpdates updates the internal state of the node
// regarding the num of channel update seen.
func (nw *nodeWatcher) updateNodeStateNumChanUpdates(op wire.OutPoint) {
	oldNum, _ := nw.state.numChanUpdates.Load(op)
	nw.state.numChanUpdates.Store(op, oldNum+1)
}

// updateNodeStateNodeUpdates updates the internal state of the node regarding
// the node updates seen.
func (nw *nodeWatcher) updateNodeStateNodeUpdates(update *lnrpc.NodeUpdate) {
	oldUpdates, _ := nw.state.nodeUpdates.Load(update.IdentityKey)
	nw.state.nodeUpdates.Store(
		update.IdentityKey, append(oldUpdates, update),
	)
}

// updateNodeStateOpenChannel updates the internal state of the node regarding
// the open channels.
func (nw *nodeWatcher) updateNodeStateOpenChannel(op wire.OutPoint,
	newChan *lnrpc.ChannelEdgeUpdate) {

	// Load the old updates the node has heard so far.
	updates, _ := nw.state.openChans.Load(op)

	// Create a new update based on this newChan.
	newUpdate := &OpenChannelUpdate{
		AdvertisingNode: newChan.AdvertisingNode,
		ConnectingNode:  newChan.ConnectingNode,
		Timestamp:       time.Now(),
	}

	// Update the node's state.
	updates = append(updates, newUpdate)
	nw.state.openChans.Store(op, updates)

	// For this new channel, if the number of edges seen is less
	// than two, then the channel hasn't been fully announced yet.
	if len(updates) < 2 {
		return
	}

	// Otherwise, we'll notify all the registered watchers and
	// remove the dispatched watchers.
	watcherResult, loaded := nw.openChanWatchers.LoadAndDelete(op)
	if !loaded {
		return
	}

	for _, eventChan := range watcherResult {
		close(eventChan)
	}
}

// updateNodeStatePolicy updates the internal state of the node regarding the
// policy updates.
func (nw *nodeWatcher) updateNodeStatePolicy(op wire.OutPoint,
	newChan *lnrpc.ChannelEdgeUpdate) {

	// Init an empty policy map and overwrite it if the channel point can
	// be found in the node's policyUpdates.
	policies, ok := nw.state.policyUpdates.Load(op)
	if !ok {
		policies = make(PolicyUpdate)
	}

	node := newChan.AdvertisingNode

	// Append the policy to the slice and update the node's state.
	newPolicy := PolicyUpdateInfo{
		newChan.RoutingPolicy, newChan.ConnectingNode, time.Now(),
	}
	policies[node] = append(policies[node], &newPolicy)
	nw.state.policyUpdates.Store(op, policies)
}

// handleOpenChannelWatchRequest processes a watch open channel request by
// checking the number of the edges seen for a given channel point. If the
// number is no less than 2 then the channel is considered open. Otherwise, we
// will attempt to find it in its channel graph. If neither can be found, the
// request is added to a watch request list than will be handled by
// handleChannelEdgeUpdates.
func (nw *nodeWatcher) handleOpenChannelWatchRequest(req *chanWatchRequest) {
	targetChan := req.chanPoint

	// If this is an open request, then it can be dispatched if the number
	// of edges seen for the channel is at least two.
	result, _ := nw.state.openChans.Load(targetChan)
	if len(result) >= 2 {
		close(req.eventChan)
		return
	}

	// Otherwise, we'll add this to the list of open channel watchers for
	// this out point.
	watchers, _ := nw.openChanWatchers.Load(targetChan)
	nw.openChanWatchers.Store(
		targetChan, append(watchers, req.eventChan),
	)
}

// handleClosedChannelUpdate takes a series of closed channel updates, extracts
// the outpoints, saves them to harness node's internal state, and notifies all
// registered clients.
func (nw *nodeWatcher) handleClosedChannelUpdate(
	updates []*lnrpc.ClosedChannelUpdate) {

	// For each channel closed, we'll mark that we've detected a channel
	// closure while lnd was pruning the channel graph.
	for _, closedChan := range updates {
		op := nw.rpc.MakeOutpoint(closedChan.ChanPoint)

		nw.state.closedChans.Store(op, closedChan)

		// As the channel has been closed, we'll notify all register
		// watchers.
		watchers, loaded := nw.closeChanWatchers.LoadAndDelete(op)
		if !loaded {
			continue
		}

		for _, eventChan := range watchers {
			close(eventChan)
		}
	}
}

// handleCloseChannelWatchRequest processes a watch close channel request by
// checking whether the given channel point can be found in the node's internal
// state. If not, the request is added to a watch request list than will be
// handled by handleCloseChannelWatchRequest.
func (nw *nodeWatcher) handleCloseChannelWatchRequest(req *chanWatchRequest) {
	targetChan := req.chanPoint

	// If this is a close request, then it can be immediately dispatched if
	// we've already seen a channel closure for this channel.
	if _, ok := nw.state.closedChans.Load(targetChan); ok {
		close(req.eventChan)
		return
	}

	// Otherwise, we'll add this to the list of close channel watchers for
	// this out point.
	oldWatchers, _ := nw.closeChanWatchers.Load(targetChan)
	nw.closeChanWatchers.Store(
		targetChan, append(oldWatchers, req.eventChan),
	)
}

// handlePolicyUpdateWatchRequest checks that if the expected policy can be
// found either in the node's interval state or describe graph response. If
// found, it will signal the request by closing the event channel. Otherwise it
// does nothing but returns nil.
func (nw *nodeWatcher) handlePolicyUpdateWatchRequest(req *chanWatchRequest) {
	op := req.chanPoint

	var policies []*PolicyUpdateInfo

	// Get a list of known policies for this chanPoint+advertisingNode
	// combination. Start searching in the node state first.
	policyMap, ok := nw.state.policyUpdates.Load(op)
	if ok {
		policies, ok = policyMap[req.advertisingNode]
		if !ok {
			return
		}
	} else {
		// If it cannot be found in the node state, try searching it
		// from the node's DescribeGraph.
		policyMap := nw.getChannelPolicies(req.includeUnannounced)
		result, ok := policyMap[op.String()][req.advertisingNode]
		if !ok {
			return
		}
		for _, policy := range result {
			// Use empty from node to mark it being loaded from
			// DescribeGraph.
			policies = append(
				policies, &PolicyUpdateInfo{
					policy, "", time.Now(),
				},
			)
		}
	}

	// Check if the latest policy is matched.
	policy := policies[len(policies)-1]
	if CheckChannelPolicy(policy.RoutingPolicy, req.policy) == nil {
		close(req.eventChan)
		return
	}
}

type topologyClient lnrpc.Lightning_SubscribeChannelGraphClient

// newTopologyClient creates a topology client.
func (nw *nodeWatcher) newTopologyClient(
	ctx context.Context) (topologyClient, error) {

	req := &lnrpc.GraphTopologySubscription{}
	client, err := nw.rpc.LN.SubscribeChannelGraph(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%s: unable to create topology client: "+
			"%v (%s)", nw.rpc.Name, err, time.Now().String())
	}

	return client, nil
}

// receiveTopologyClientStream takes a topologyClient and receives graph
// updates.
//
// NOTE: must be run as a goroutine.
func (nw *nodeWatcher) receiveTopologyClientStream(ctxb context.Context,
	client topologyClient,
	receiver chan *lnrpc.GraphTopologyUpdate) error {

	for {
		update, err := client.Recv()

		switch {
		case err == nil:
			// Good case. We will send the update to the receiver.

		case strings.Contains(err.Error(), "EOF"):
			// End of subscription stream. Do nothing and quit.
			return nil

		case strings.Contains(err.Error(), context.Canceled.Error()):
			// End of subscription stream. Do nothing and quit.
			return nil

		default:
			// An expected error is returned, return and leave it
			// to be handled by the caller.
			return fmt.Errorf("graph subscription err: %w", err)
		}

		// Send the update or quit.
		select {
		case receiver <- update:
		case <-ctxb.Done():
			return nil
		}
	}
}

// getChannelPolicies queries the channel graph and formats the policies into
// the format defined in type policyUpdateMap.
func (nw *nodeWatcher) getChannelPolicies(include bool) policyUpdateMap {
	req := &lnrpc.ChannelGraphRequest{IncludeUnannounced: include}
	graph := nw.rpc.DescribeGraph(req)

	policyUpdates := policyUpdateMap{}

	for _, e := range graph.Edges {
		policies := policyUpdates[e.ChanPoint]

		// If the map[op] is nil, we need to initialize the map first.
		if policies == nil {
			policies = make(map[string][]*lnrpc.RoutingPolicy)
		}

		if e.Node1Policy != nil {
			policies[e.Node1Pub] = append(
				policies[e.Node1Pub], e.Node1Policy,
			)
		}

		if e.Node2Policy != nil {
			policies[e.Node2Pub] = append(
				policies[e.Node2Pub], e.Node2Policy,
			)
		}

		policyUpdates[e.ChanPoint] = policies
	}

	return policyUpdates
}

// CheckChannelPolicy checks that the policy matches the expected one.
func CheckChannelPolicy(policy, expectedPolicy *lnrpc.RoutingPolicy) error {
	if policy.FeeBaseMsat != expectedPolicy.FeeBaseMsat {
		return fmt.Errorf("expected base fee %v, got %v",
			expectedPolicy.FeeBaseMsat, policy.FeeBaseMsat)
	}
	if policy.FeeRateMilliMsat != expectedPolicy.FeeRateMilliMsat {
		return fmt.Errorf("expected fee rate %v, got %v",
			expectedPolicy.FeeRateMilliMsat,
			policy.FeeRateMilliMsat)
	}
	if policy.TimeLockDelta != expectedPolicy.TimeLockDelta {
		return fmt.Errorf("expected time lock delta %v, got %v",
			expectedPolicy.TimeLockDelta,
			policy.TimeLockDelta)
	}
	if policy.MinHtlc != expectedPolicy.MinHtlc {
		return fmt.Errorf("expected min htlc %v, got %v",
			expectedPolicy.MinHtlc, policy.MinHtlc)
	}
	if policy.MaxHtlcMsat != expectedPolicy.MaxHtlcMsat {
		return fmt.Errorf("expected max htlc %v, got %v",
			expectedPolicy.MaxHtlcMsat, policy.MaxHtlcMsat)
	}
	if policy.InboundFeeBaseMsat != expectedPolicy.InboundFeeBaseMsat {
		return fmt.Errorf("expected inbound base fee %v, got %v",
			expectedPolicy.InboundFeeBaseMsat,
			policy.InboundFeeBaseMsat)
	}
	if policy.InboundFeeRateMilliMsat !=
		expectedPolicy.InboundFeeRateMilliMsat {

		return fmt.Errorf("expected inbound fee rate %v, got %v",
			expectedPolicy.InboundFeeRateMilliMsat,
			policy.InboundFeeRateMilliMsat)
	}
	if policy.Disabled != expectedPolicy.Disabled {
		return errors.New("edge should be disabled but isn't")
	}

	// We now validate custom records.
	records := policy.CustomRecords

	if len(records) != len(expectedPolicy.CustomRecords) {
		return fmt.Errorf("expected %v CustomRecords, got %v, "+
			"records: %v", len(expectedPolicy.CustomRecords),
			len(records), records)
	}

	expectedRecords := expectedPolicy.CustomRecords
	for k, record := range records {
		expected, found := expectedRecords[k]
		if !found {
			return fmt.Errorf("CustomRecords %v not found", k)
		}

		if !bytes.Equal(record, expected) {
			return fmt.Errorf("want CustomRecords(%v) %s, got %s",
				k, expected, record)
		}
	}

	return nil
}

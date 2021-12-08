package lntest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
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

// closeChanWatchRequest is a request to the lightningNetworkWatcher to be
// notified once it's detected within the test Lightning Network, that a
// channel has either been added or closed.
type chanWatchRequest struct {
	chanPoint wire.OutPoint

	chanWatchType chanWatchType

	eventChan chan struct{}

	advertisingNode    string
	policy             *lnrpc.RoutingPolicy
	includeUnannounced bool
}

// GetNumChanUpdates reads the num of channel updates inside a lock and returns
// the value.
func (hn *HarnessNode) GetNumChanUpdates(op wire.OutPoint) int {
	result, ok := hn.state.numChanUpdates.Load(op)
	if ok {
		return result.(int)
	}
	return 0
}

// GetPolicyUpdates returns the node's policyUpdates state.
func (hn *HarnessNode) GetPolicyUpdates(op wire.OutPoint) NodePolicyUpdate {
	result, ok := hn.state.policyUpdates.Load(op)
	if ok {
		return result.(NodePolicyUpdate)
	}
	return nil
}

// GetNodeUpdates reads the node updates inside a lock and returns the value.
func (hn *HarnessNode) GetNodeUpdates(pubkey string) []*lnrpc.NodeUpdate {
	result, ok := hn.state.nodeUpdates.Load(pubkey)
	if ok {
		return result.([]*lnrpc.NodeUpdate)
	}
	return nil
}

// WaitForNetworkNumChanUpdates will block until a given number of updates has
// been seen the network.
func (hn *HarnessNode) WaitForNetworkNumChanUpdates(op wire.OutPoint,
	expected int) error {

	ticker := time.NewTicker(wait.PollInterval)
	timer := time.After(DefaultTimeout)
	defer ticker.Stop()

	for {
		num := hn.GetNumChanUpdates(op)

		select {
		// Check the num every 200ms.
		case <-ticker.C:
			if num >= expected {
				return nil
			}

		case <-timer:
			return fmt.Errorf("timeout waiting for num channel "+
				"updates, want %d, got %d", expected, num)
		}
	}
}

// WaitForNetworkNumNodeUpdates will block until a given number of node updates
// has been seen the network.
func (hn *HarnessNode) WaitForNetworkNumNodeUpdates(pubkey string,
	expected int) error {

	ticker := time.NewTicker(wait.PollInterval)
	timer := time.After(DefaultTimeout)
	defer ticker.Stop()

	for {
		num := len(hn.GetNodeUpdates(pubkey))

		select {
		// Check the num every 200ms.
		case <-ticker.C:
			if num >= expected {
				return nil
			}

		case <-timer:
			return fmt.Errorf("timeout waiting for num node "+
				"updates, want %d, got %d", expected, num)
		}
	}
}

// WaitForNetworkChannelOpen will block until a channel with the target
// outpoint is seen as being fully advertised within the network. A channel is
// considered "fully advertised" once both of its directional edges has been
// advertised within the test Lightning Network.
func (hn *HarnessNode) WaitForNetworkChannelOpen(
	chanPoint *lnrpc.ChannelPoint) error {

	op, err := MakeOutpoint(chanPoint)
	if err != nil {
		return fmt.Errorf("failed to create outpoint for %v "+
			"got err: %v", chanPoint, err)
	}

	eventChan := make(chan struct{})
	hn.chanWatchRequests <- &chanWatchRequest{
		chanPoint:     op,
		eventChan:     eventChan,
		chanWatchType: watchOpenChannel,
	}

	timer := time.After(DefaultTimeout)
	select {
	case <-eventChan:
		return nil
	case <-timer:
		return fmt.Errorf("channel:%s not opened before timeout: %s",
			op, hn)
	}
}

// WaitForNetworkChannelClose will block until a channel with the target
// outpoint is seen as closed within the network. A channel is considered
// closed once a transaction spending the funding outpoint is seen within a
// confirmed block.
func (hn *HarnessNode) WaitForNetworkChannelClose(
	chanPoint *lnrpc.ChannelPoint) (*lnrpc.ClosedChannelUpdate, error) {

	op, err := MakeOutpoint(chanPoint)
	if err != nil {
		return nil, fmt.Errorf("failed to create outpoint for %v "+
			"got err: %v", chanPoint, err)
	}

	eventChan := make(chan struct{})
	hn.chanWatchRequests <- &chanWatchRequest{
		chanPoint:     op,
		eventChan:     eventChan,
		chanWatchType: watchCloseChannel,
	}

	timer := time.After(DefaultTimeout)
	select {
	case <-eventChan:
		closedChan, ok := hn.state.closedChans.Load(op)
		if !ok {
			return nil, fmt.Errorf("channel:%s expected to find "+
				"a closed channel in node's state:%s", op, hn)
		}
		return closedChan.(*lnrpc.ClosedChannelUpdate), nil

	case <-timer:
		return nil, fmt.Errorf("channel:%s not closed before timeout: "+
			"%s", op, hn)
	}
}

// WaitForChannelPolicyUpdate will block until a channel policy with the target
// outpoint and advertisingNode is seen within the network.
func (hn *HarnessNode) WaitForChannelPolicyUpdate(
	advertisingNode string, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint, includeUnannounced bool) error {

	op, err := MakeOutpoint(chanPoint)
	if err != nil {
		return fmt.Errorf("failed to create outpoint for %v"+
			"got err: %v", chanPoint, err)
	}

	ticker := time.NewTicker(wait.PollInterval)
	timer := time.After(DefaultTimeout)
	defer ticker.Stop()

	eventChan := make(chan struct{})
	for {
		select {
		// Send a watch request every second.
		case <-ticker.C:
			// Did the event can close in the meantime? We want to
			// avoid a "close of closed channel" panic since we're
			// re-using the same event chan for multiple requests.
			select {
			case <-eventChan:
				return nil
			default:
			}

			hn.chanWatchRequests <- &chanWatchRequest{
				chanPoint:          op,
				eventChan:          eventChan,
				chanWatchType:      watchPolicyUpdate,
				policy:             policy,
				advertisingNode:    advertisingNode,
				includeUnannounced: includeUnannounced,
			}

		case <-eventChan:
			return nil

		case <-timer:
			expected, err := json.MarshalIndent(policy, "", "\t")
			if err != nil {
				return fmt.Errorf("encode policy err: %v", err)
			}
			policies, err := syncMapToJSON(hn.state.policyUpdates)
			if err != nil {
				return err
			}

			return fmt.Errorf("policy not updated before timeout:"+
				"\nchannel: %v \nadvertisingNode: %v"+
				"\nwant policy:%s\nhave updates:%s", op,
				advertisingNode, expected, policies)
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
		return nil, fmt.Errorf("encode polices err: %v", err)
	}

	return policies, nil
}

// lightningNetworkWatcher is a goroutine which is able to dispatch
// notifications once it has been observed that a target channel has been
// closed or opened within the network. In order to dispatch these
// notifications, the GraphTopologySubscription client exposed as part of the
// gRPC interface is used.
func (hn *HarnessNode) lightningNetworkWatcher(started chan error) {
	defer hn.wg.Done()

	graphUpdates := make(chan *lnrpc.GraphTopologyUpdate)

	// Start a goroutine to receive graph updates.
	hn.wg.Add(1)
	go func() {
		defer hn.wg.Done()

		// Create a topology client to receive graph updates.
		client, err := hn.newTopologyClient(hn.runCtx)
		started <- err

		err = hn.receiveTopologyClientStream(client, graphUpdates)
		if err != nil {
			hn.PrintErrf("receive topology client stream "+
				"got err:%v", err)
		}
	}()

	for {
		select {
		// A new graph update has just been received, so we'll examine
		// the current set of registered clients to see if we can
		// dispatch any requests.
		case graphUpdate := <-graphUpdates:
			hn.handleChannelEdgeUpdates(graphUpdate.ChannelUpdates)
			hn.handleClosedChannelUpdate(graphUpdate.ClosedChans)
			hn.handleNodeUpdates(graphUpdate.NodeUpdates)

		// A new watch request, has just arrived. We'll either be able
		// to dispatch immediately, or need to add the client for
		// processing later.
		case watchRequest := <-hn.chanWatchRequests:
			switch watchRequest.chanWatchType {
			case watchOpenChannel:
				// TODO(roasbeef): add update type also, checks
				// for multiple of 2
				hn.handleOpenChannelWatchRequest(watchRequest)

			case watchCloseChannel:
				hn.handleCloseChannelWatchRequest(watchRequest)

			case watchPolicyUpdate:
				hn.handlePolicyUpdateWatchRequest(watchRequest)
			}

		case <-hn.runCtx.Done():
			return
		}
	}
}

func (hn *HarnessNode) checkChanPointInGraph(chanPoint wire.OutPoint) bool {
	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	chanGraph, err := hn.rpc.LN.DescribeGraph(
		ctxt, &lnrpc.ChannelGraphRequest{},
	)
	if err != nil {
		return false
	}

	targetChanPoint := chanPoint.String()
	for _, chanEdge := range chanGraph.Edges {
		candidateChanPoint := chanEdge.ChanPoint
		if targetChanPoint == candidateChanPoint {
			return true
		}
	}

	return false
}

func (hn *HarnessNode) handleNodeUpdates(updates []*lnrpc.NodeUpdate) {
	for _, nodeUpdate := range updates {
		hn.updateNodeStateNodeUpdates(nodeUpdate)
	}
}

// handleChannelEdgeUpdates takes a series of channel edge updates, extracts
// the outpoints, and saves them to harness node's internal state.
func (hn *HarnessNode) handleChannelEdgeUpdates(
	updates []*lnrpc.ChannelEdgeUpdate) {

	// For each new channel, we'll increment the number of edges seen by
	// one.
	for _, newChan := range updates {
		op, err := MakeOutpoint(newChan.ChanPoint)
		if err != nil {
			hn.PrintErrf("failed to create outpoint for %v "+
				"got err: %v", newChan.ChanPoint, err)
			return
		}

		// Update the num of channel updates.
		hn.updateNodeStateNumChanUpdates(op)

		// Update the open channels.
		hn.updateNodeStateOpenChannel(op)

		// Check whether there's a routing policy update. If so, save
		// it to the node state.
		if newChan.RoutingPolicy != nil {
			hn.updateNodeStatePolicy(op, newChan)
		}
	}
}

// updateNodeStateNumChanUpdates updates the internal state of the node
// regarding the num of channel update seen.
func (hn *HarnessNode) updateNodeStateNumChanUpdates(op wire.OutPoint) {
	var oldNum int
	result, ok := hn.state.numChanUpdates.Load(op)
	if ok {
		oldNum = result.(int)
	}
	hn.state.numChanUpdates.Store(op, oldNum+1)
}

// updateNodeStateNodeUpdates updates the internal state of the node regarding
// the node updates seen.
func (hn *HarnessNode) updateNodeStateNodeUpdates(update *lnrpc.NodeUpdate) {
	var oldUpdates []*lnrpc.NodeUpdate

	result, ok := hn.state.nodeUpdates.Load(update.IdentityKey)
	if ok {
		oldUpdates = result.([]*lnrpc.NodeUpdate)
	}
	hn.state.nodeUpdates.Store(
		update.IdentityKey, append(oldUpdates, update),
	)
}

// updateNodeStateOpenChannel updates the internal state of the node regarding
// the open channels.
func (hn *HarnessNode) updateNodeStateOpenChannel(op wire.OutPoint) {
	// Whenever we call update, we create a default count of 1, and adds
	// more count from the node's openChans map if found.
	num := 1
	result, ok := hn.state.openChans.Load(op)
	if ok {
		num += result.(int)
	}
	hn.state.openChans.Store(op, num)

	// For this new channel, if the number of edges seen is less
	// than two, then the channel hasn't been fully announced yet.
	if numEdges := num; numEdges < 2 {
		return
	}

	// Otherwise, we'll notify all the registered watchers and
	// remove the dispatched watchers.
	watcherResult, loaded := hn.openChanWatchers.LoadAndDelete(op)
	if !loaded {
		return
	}
	events := watcherResult.([]chan struct{})
	for _, eventChan := range events {
		close(eventChan)
	}
}

// updateNodeStatePolicy updates the internal state of the node regarding the
// policy updates.
func (hn *HarnessNode) updateNodeStatePolicy(op wire.OutPoint,
	newChan *lnrpc.ChannelEdgeUpdate) {

	// Init an empty policy map and overwrite it if the channel point can
	// be found in the node's policyUpdates.
	policies := make(NodePolicyUpdate)
	result, ok := hn.state.policyUpdates.Load(op)
	if ok {
		policies = result.(NodePolicyUpdate)
	}

	node := newChan.AdvertisingNode

	// Append the policy to the slice and update the node's state.
	newPolicy := PolicyUpdate{
		newChan.RoutingPolicy, newChan.ConnectingNode, time.Now(),
	}
	policies[node] = append(policies[node], &newPolicy)
	hn.state.policyUpdates.Store(op, policies)
}

// handleOpenChannelWatchRequest processes a watch open channel request by
// checking the number of the edges seen for a given channel point. If the
// number is no less than 2 then the channel is considered open. Otherwise, we
// will attempt to find it in its channel graph. If neither can be found, the
// request is added to a watch request list than will be handled by
// handleChannelEdgeUpdates.
func (hn *HarnessNode) handleOpenChannelWatchRequest(req *chanWatchRequest) {
	targetChan := req.chanPoint

	// If this is an open request, then it can be dispatched if the number
	// of edges seen for the channel is at least two.
	result, ok := hn.state.openChans.Load(targetChan)
	if ok && result.(int) >= 2 {
		close(req.eventChan)
		return
	}

	// Before we add the channel to our set of open clients, we'll check to
	// see if the channel is already in the channel graph of the target
	// node. This lets us handle the case where a node has already seen a
	// channel before a notification has been requested, causing us to miss
	// it.
	chanFound := hn.checkChanPointInGraph(targetChan)
	if chanFound {
		close(req.eventChan)
		return
	}

	// Otherwise, we'll add this to the list of open channel watchers for
	// this out point.
	oldWatchers := make([]chan struct{}, 0)
	watchers, ok := hn.openChanWatchers.Load(targetChan)
	if ok {
		oldWatchers = watchers.([]chan struct{})
	}
	hn.openChanWatchers.Store(
		targetChan, append(oldWatchers, req.eventChan),
	)
}

// handleClosedChannelUpdate takes a series of closed channel updates, extracts
// the outpoints, saves them to harness node's internal state, and notifies all
// registered clients.
func (hn *HarnessNode) handleClosedChannelUpdate(
	updates []*lnrpc.ClosedChannelUpdate) {

	// For each channel closed, we'll mark that we've detected a channel
	// closure while lnd was pruning the channel graph.
	for _, closedChan := range updates {
		op, err := MakeOutpoint(closedChan.ChanPoint)
		if err != nil {
			hn.PrintErrf("failed to create outpoint for %v "+
				"got err: %v", closedChan.ChanPoint, err)
			return
		}

		hn.state.closedChans.Store(op, closedChan)

		// As the channel has been closed, we'll notify all register
		// watchers.
		result, loaded := hn.closeChanWatchers.LoadAndDelete(op)
		if !loaded {
			continue
		}

		watchers := result.([]chan struct{})
		for _, eventChan := range watchers {
			close(eventChan)
		}
	}
}

// handleCloseChannelWatchRequest processes a watch close channel request by
// checking whether the given channel point can be found in the node's internal
// state. If not, the request is added to a watch request list than will be
// handled by handleCloseChannelWatchRequest.
func (hn *HarnessNode) handleCloseChannelWatchRequest(req *chanWatchRequest) {
	targetChan := req.chanPoint

	// If this is a close request, then it can be immediately dispatched if
	// we've already seen a channel closure for this channel.
	if _, ok := hn.state.closedChans.Load(targetChan); ok {
		close(req.eventChan)
		return
	}

	// Otherwise, we'll add this to the list of close channel watchers for
	// this out point.
	oldWatchers := make([]chan struct{}, 0)
	result, ok := hn.closeChanWatchers.Load(targetChan)
	if ok {
		oldWatchers = result.([]chan struct{})
	}

	hn.closeChanWatchers.Store(
		targetChan, append(oldWatchers, req.eventChan),
	)
}

// handlePolicyUpdateWatchRequest checks that if the expected policy can be
// found either in the node's interval state or describe graph response. If
// found, it will signal the request by closing the event channel. Otherwise it
// does nothing but returns nil.
func (hn *HarnessNode) handlePolicyUpdateWatchRequest(req *chanWatchRequest) {
	op := req.chanPoint

	var policies []*PolicyUpdate

	// Get a list of known policies for this chanPoint+advertisingNode
	// combination. Start searching in the node state first.
	result, ok := hn.state.policyUpdates.Load(op)
	if ok {
		policyMap := result.(NodePolicyUpdate)
		policies, ok = policyMap[req.advertisingNode]
		if !ok {
			return
		}
	} else {
		// If it cannot be found in the node state, try searching it
		// from the node's DescribeGraph.
		policyMap := hn.getChannelPolicies(req.includeUnannounced)
		result, ok := policyMap[op.String()][req.advertisingNode]
		if !ok {
			return
		}
		for _, policy := range result {
			// Use empty from node to mark it being loaded from
			// DescribeGraph.
			policies = append(
				policies, &PolicyUpdate{policy, "", time.Now()},
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
func (hn *HarnessNode) newTopologyClient(
	ctx context.Context) (topologyClient, error) {

	req := &lnrpc.GraphTopologySubscription{}
	client, err := hn.rpc.LN.SubscribeChannelGraph(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%s(%d): unable to create topology "+
			"client: %v (%s)", hn.Name(), hn.NodeID, err,
			time.Now().String())
	}

	return client, nil
}

// receiveTopologyClientStream initializes a topologyClient to subscribe
// topology update events. Due to a race condition between the ChannelRouter
// starting and us making the subscription request, it's possible for our graph
// subscription to fail. In that case, we will retry the subscription until it
// succeeds or fail after 10 seconds.
//
// NOTE: must be run as a goroutine.
func (hn *HarnessNode) receiveTopologyClientStream(
	client topologyClient,
	receiver chan *lnrpc.GraphTopologyUpdate) error {

	// We use the context to time out when retrying graph subscription.
	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	for {
		update, err := client.Recv()

		switch {
		case err == nil:
			// Good case. We will send the update to the receiver.

		case strings.Contains(err.Error(), "router not started"):
			// If the router hasn't been started, we will retry
			// every 200 ms until it has been started or fail
			// after the ctxt is timed out.
			select {
			case <-ctxt.Done():
				return fmt.Errorf("graph subscription: " +
					"router not started before timeout")
			case <-time.After(wait.PollInterval):
			case <-hn.runCtx.Done():
				return nil
			}

			// Re-create the topology client.
			client, err = hn.newTopologyClient(hn.runCtx)
			if err != nil {
				return fmt.Errorf("create topologyClient "+
					"failed: %v", err)
			}

			continue

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
		case <-hn.runCtx.Done():
			return nil
		}
	}
}

// getChannelPolicies queries the channel graph and formats the policies into
// the format defined in type policyUpdateMap.
func (hn *HarnessNode) getChannelPolicies(include bool) policyUpdateMap {
	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	graph, err := hn.rpc.LN.DescribeGraph(ctxt, &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: include,
	})
	if err != nil {
		hn.PrintErrf("DescribeGraph got err: %v", err)
		return nil
	}

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

package node

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest/rpc"
	"github.com/lightningnetwork/lnd/lnutils"
)

type (
	// PolicyUpdate defines a type to store channel policy updates for a
	// given advertisingNode. It has the format,
	// {"advertisingNode": [policy1, policy2, ...]}.
	PolicyUpdate map[string][]*PolicyUpdateInfo

	// policyUpdateMap defines a type to store channel policy updates. It
	// has the format,
	// {
	//  "chanPoint1": {
	//       "advertisingNode1": [
	//              policy1, policy2, ...
	//       ],
	//       "advertisingNode2": [
	//              policy1, policy2, ...
	//       ]
	//  },
	//  "chanPoint2": ...
	// }.
	policyUpdateMap map[string]map[string][]*lnrpc.RoutingPolicy
)

// PolicyUpdateInfo stores the RoutingPolicy plus the connecting node info.
type PolicyUpdateInfo struct {
	*lnrpc.RoutingPolicy

	// ConnectingNode specifies the node that is connected with the
	// advertising node.
	ConnectingNode string `json:"connecting_node"`

	// Timestamp records the time the policy update is made.
	Timestamp time.Time `json:"timestamp"`
}

// OpenChannelUpdate stores the open channel updates.
type OpenChannelUpdate struct {
	// AdvertisingNode specifies the node that advertised this update.
	AdvertisingNode string `json:"advertising_node"`

	// ConnectingNode specifies the node that is connected with the
	// advertising node.
	ConnectingNode string `json:"connecting_node"`

	// Timestamp records the time the policy update is made.
	Timestamp time.Time `json:"timestamp"`
}

// openChannelCount stores the total number of channel related counts.
type openChannelCount struct {
	Active     int
	Inactive   int
	Pending    int
	Public     int
	Private    int
	NumUpdates uint64
}

// closedChannelCount stores the total number of closed, waiting and pending
// force close channels.
type closedChannelCount struct {
	PendingForceClose int
	WaitingClose      int
	Closed            int
}

// utxoCount counts the total confirmed and unconfirmed UTXOs.
type utxoCount struct {
	Confirmed   int
	Unconfirmed int
}

// edgeCount counts the total and public edges.
type edgeCount struct {
	Total  int
	Public int
}

// paymentCount counts the complete(settled/failed) and incomplete payments.
type paymentCount struct {
	Total           int
	Completed       int
	LastIndexOffset uint64
}

// invoiceCount counts the complete(settled/failed) and incomplete invoices.
type invoiceCount struct {
	Total           int
	Completed       int
	LastIndexOffset uint64
}

// walletBalance provides a summary over balances related the node's wallet.
type walletBalance struct {
	TotalBalance       int64
	ConfirmedBalance   int64
	UnconfirmedBalance int64
	AccountBalance     map[string]*lnrpc.WalletAccountBalance
}

// State records the current state for a given node. It provides a simple count
// over the node so that the test can track its state. For a channel-specific
// state check, use dedicated function to query the channel as each channel is
// meant to be unique.
type State struct {
	// rpc is the RPC clients used for the current node.
	rpc *rpc.HarnessRPC

	// OpenChannel gives the summary of open channel related counts.
	OpenChannel openChannelCount

	// CloseChannel gives the summary of close channel related counts.
	CloseChannel closedChannelCount

	// Wallet gives the summary of the wallet balance.
	Wallet walletBalance

	// HTLC counts the total active HTLCs.
	HTLC int

	// Edge counts the total private/public edges.
	Edge edgeCount

	// ChannelUpdate counts the total channel updates seen from the graph
	// subscription.
	ChannelUpdate int

	// NodeUpdate counts the total node announcements seen from the graph
	// subscription.
	NodeUpdate int

	// UTXO counts the total active UTXOs.
	UTXO utxoCount

	// Payment counts the total payment of the node.
	Payment paymentCount

	// Invoice counts the total invoices made by the node.
	Invoice invoiceCount

	// openChans records each opened channel and how many times it has
	// heard the announcements from its graph subscription.
	openChans *lnutils.SyncMap[wire.OutPoint, []*OpenChannelUpdate]

	// closedChans records each closed channel and its close channel update
	// message received from its graph subscription.
	closedChans *lnutils.SyncMap[wire.OutPoint, *lnrpc.ClosedChannelUpdate]

	// numChanUpdates records the number of channel updates seen by each
	// channel.
	numChanUpdates *lnutils.SyncMap[wire.OutPoint, int]

	// nodeUpdates records the node announcements seen by each node.
	nodeUpdates *lnutils.SyncMap[string, []*lnrpc.NodeUpdate]

	// policyUpdates defines a type to store channel policy updates. It has
	// the format,
	// {
	//  "chanPoint1": {
	//       "advertisingNode1": [
	//              policy1, policy2, ...
	//       ],
	//       "advertisingNode2": [
	//              policy1, policy2, ...
	//       ]
	//  },
	//  "chanPoint2": ...
	// }
	policyUpdates *lnutils.SyncMap[wire.OutPoint, PolicyUpdate]
}

// newState initialize a new state with every field being set to its zero
// value.
func newState(rpc *rpc.HarnessRPC) *State {
	return &State{
		rpc: rpc,
		openChans: &lnutils.SyncMap[
			wire.OutPoint, []*OpenChannelUpdate,
		]{},
		closedChans: &lnutils.SyncMap[
			wire.OutPoint, *lnrpc.ClosedChannelUpdate,
		]{},
		numChanUpdates: &lnutils.SyncMap[wire.OutPoint, int]{},
		nodeUpdates:    &lnutils.SyncMap[string, []*lnrpc.NodeUpdate]{},
		policyUpdates:  &lnutils.SyncMap[wire.OutPoint, PolicyUpdate]{},
	}
}

// updateChannelStats gives the stats on open channel related fields.
func (s *State) updateChannelStats() {
	req := &lnrpc.ListChannelsRequest{}
	resp := s.rpc.ListChannels(req)

	for _, channel := range resp.Channels {
		if channel.Active {
			s.OpenChannel.Active++
		} else {
			s.OpenChannel.Inactive++
		}

		if channel.Private {
			s.OpenChannel.Private++
		} else {
			s.OpenChannel.Public++
		}
		s.OpenChannel.NumUpdates += channel.NumUpdates
		s.HTLC += len(channel.PendingHtlcs)
	}
}

// updateCloseChannelStats gives the stats on close channel related fields.
func (s *State) updateCloseChannelStats() {
	resp := s.rpc.PendingChannels()
	s.CloseChannel.PendingForceClose += len(
		resp.PendingForceClosingChannels,
	)
	s.CloseChannel.WaitingClose += len(resp.WaitingCloseChannels)

	closeReq := &lnrpc.ClosedChannelsRequest{}
	closed := s.rpc.ClosedChannels(closeReq)

	s.CloseChannel.Closed += len(closed.Channels)
	s.OpenChannel.Pending += len(resp.PendingOpenChannels)
}

// updatePaymentStats counts the total payments made.
func (s *State) updatePaymentStats() {
	req := &lnrpc.ListPaymentsRequest{
		IndexOffset: s.Payment.LastIndexOffset,
	}
	resp := s.rpc.ListPayments(req)

	// Exit early when the there's no payment.
	//
	// NOTE: we need to exit early here because when there's no invoice the
	// `LastOffsetIndex` will be zero.
	if len(resp.Payments) == 0 {
		return
	}

	s.Payment.LastIndexOffset = resp.LastIndexOffset
	for _, payment := range resp.Payments {
		if payment.Status == lnrpc.Payment_FAILED ||
			payment.Status == lnrpc.Payment_SUCCEEDED {

			s.Payment.Completed++
		}
	}

	s.Payment.Total += len(resp.Payments)
}

// updateInvoiceStats counts the total invoices made.
func (s *State) updateInvoiceStats() {
	req := &lnrpc.ListInvoiceRequest{
		NumMaxInvoices: math.MaxUint64,
		IndexOffset:    s.Invoice.LastIndexOffset,
	}
	resp := s.rpc.ListInvoices(req)

	// Exit early when the there's no invoice.
	//
	// NOTE: we need to exit early here because when there's no invoice the
	// `LastOffsetIndex` will be zero.
	if len(resp.Invoices) == 0 {
		return
	}

	s.Invoice.LastIndexOffset = resp.LastIndexOffset
	for _, invoice := range resp.Invoices {
		if invoice.State == lnrpc.Invoice_SETTLED ||
			invoice.State == lnrpc.Invoice_CANCELED {

			s.Invoice.Completed++
		}
	}

	s.Invoice.Total += len(resp.Invoices)
}

// updateUTXOStats counts the total UTXOs made.
func (s *State) updateUTXOStats() {
	req := &walletrpc.ListUnspentRequest{}
	resp := s.rpc.ListUnspent(req)

	for _, utxo := range resp.Utxos {
		if utxo.Confirmations > 0 {
			s.UTXO.Confirmed++
		} else {
			s.UTXO.Unconfirmed++
		}
	}
}

// updateEdgeStats counts the total edges.
func (s *State) updateEdgeStats() {
	// filterDisabled is a helper closure that filters out disabled
	// channels.
	filterDisabled := func(edge *lnrpc.ChannelEdge) bool {
		if edge.Node1Policy != nil && edge.Node1Policy.Disabled {
			return false
		}
		if edge.Node2Policy != nil && edge.Node2Policy.Disabled {
			return false
		}

		return true
	}

	req := &lnrpc.ChannelGraphRequest{IncludeUnannounced: true}
	resp := s.rpc.DescribeGraph(req)
	s.Edge.Total = len(fn.Filter(resp.Edges, filterDisabled))

	req = &lnrpc.ChannelGraphRequest{IncludeUnannounced: false}
	resp = s.rpc.DescribeGraph(req)
	s.Edge.Public = len(fn.Filter(resp.Edges, filterDisabled))
}

// updateWalletBalance creates stats for the node's wallet balance.
func (s *State) updateWalletBalance() {
	resp := s.rpc.WalletBalance()

	s.Wallet.TotalBalance = resp.TotalBalance
	s.Wallet.ConfirmedBalance = resp.ConfirmedBalance
	s.Wallet.UnconfirmedBalance = resp.UnconfirmedBalance
	s.Wallet.AccountBalance = resp.AccountBalance
}

// updateState updates the internal state of the node.
func (s *State) updateState() {
	s.updateChannelStats()
	s.updateCloseChannelStats()
	s.updatePaymentStats()
	s.updateInvoiceStats()
	s.updateUTXOStats()
	s.updateEdgeStats()
	s.updateWalletBalance()
}

// String encodes the node's state for debugging.
func (s *State) String() string {
	stateBytes, err := json.MarshalIndent(s, "", "\t")
	if err != nil {
		return fmt.Sprintf("\n encode node state with err: %v", err)
	}

	return fmt.Sprintf("\n%s", stateBytes)
}

// resetEphermalStates resets the current state with a new HarnessRPC and empty
// private fields which are used to track state only valid for the last test.
func (s *State) resetEphermalStates(rpc *rpc.HarnessRPC) {
	s.rpc = rpc

	// Reset ephermal states which are used to record info from finished
	// tests.
	s.openChans = &lnutils.SyncMap[wire.OutPoint, []*OpenChannelUpdate]{}
	s.closedChans = &lnutils.SyncMap[
		wire.OutPoint, *lnrpc.ClosedChannelUpdate,
	]{}
	s.numChanUpdates = &lnutils.SyncMap[wire.OutPoint, int]{}
	s.nodeUpdates = &lnutils.SyncMap[string, []*lnrpc.NodeUpdate]{}
	s.policyUpdates = &lnutils.SyncMap[wire.OutPoint, PolicyUpdate]{}
}

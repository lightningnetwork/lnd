package lntest

import (
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// policyUpdateMap defines a type to store channel policy updates. It has the
// format,
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
type policyUpdateMap map[string]map[string][]*lnrpc.RoutingPolicy

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

// balanceCount provides a summary over balances related to channels.
type balanceCount struct {
	LocalBalance             *lnrpc.Amount
	RemoteBalance            *lnrpc.Amount
	UnsettledLocalBalance    *lnrpc.Amount
	UnsettledRemoteBalance   *lnrpc.Amount
	PendingOpenLocalBalance  *lnrpc.Amount
	PendingOpenRemoteBalance *lnrpc.Amount

	// Deprecated fields.
	Balance            int64
	PendingOpenBalance int64
}

// walletBalance provides a summary over balances related the node's wallet.
type walletBalance struct {
	TotalBalance       int64
	ConfirmedBalance   int64
	UnconfirmedBalance int64
	AccountBalance     map[string]*lnrpc.WalletAccountBalance
}

// nodeState records the current state for a given node. It provides a simple
// count over the node so that the test can track its state. For a
// channel-specific state check, use dedicated function to query the channel as
// each channel is meant to be unique.
type nodeState struct {
	// OpenChannel gives the summary of open channel related counts.
	OpenChannel openChannelCount

	// CloseChannel gives the summary of close channel related counts.
	CloseChannel closedChannelCount

	// Balance gives the summary of the channel balance.
	Balance balanceCount

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
	openChans map[wire.OutPoint]int

	// closedChans records each closed channel and its close channel update
	// message received from its graph subscription.
	closedChans map[wire.OutPoint]*lnrpc.ClosedChannelUpdate

	// numChanUpdates records the number of channel updates seen by each
	// channel.
	numChanUpdates map[wire.OutPoint]int

	// nodeUpdates records the node announcements seen by each node.
	nodeUpdates map[string][]*lnrpc.NodeUpdate

	// policyUpdates stores a slice of seen polices by each advertising
	// node and the outpoint.
	policyUpdates policyUpdateMap
}

// newState initialize a new state with every field being set to its zero
// value.
func newState() *nodeState {
	return &nodeState{
		openChans: make(map[wire.OutPoint]int),
		closedChans: make(
			map[wire.OutPoint]*lnrpc.ClosedChannelUpdate,
		),

		numChanUpdates: make(map[wire.OutPoint]int),
		nodeUpdates:    make(map[string][]*lnrpc.NodeUpdate),
		policyUpdates:  policyUpdateMap{},
	}
}

// updateChannelStats gives the stats on open channel related fields.
func (hn *HarnessNode) updateChannelStats(ctxt context.Context,
	state *nodeState) error {

	req := &lnrpc.ListChannelsRequest{}
	resp, err := hn.rpc.LN.ListChannels(ctxt, req)
	if err != nil {
		return fmt.Errorf("ListChannels failed: %w", err)
	}
	for _, channel := range resp.Channels {
		if channel.Active {
			state.OpenChannel.Active++
		} else {
			state.OpenChannel.Inactive++
		}

		if channel.Private {
			state.OpenChannel.Private++
		} else {
			state.OpenChannel.Public++
		}
		state.OpenChannel.NumUpdates += channel.NumUpdates
		state.HTLC += len(channel.PendingHtlcs)
	}

	return nil
}

// updateCloseChannelStats gives the stats on close channel related fields.
func (hn *HarnessNode) updateCloseChannelStats(ctxt context.Context,
	state *nodeState) error {

	req := &lnrpc.PendingChannelsRequest{}
	resp, err := hn.rpc.LN.PendingChannels(ctxt, req)
	if err != nil {
		return fmt.Errorf("PendingChannels failed: %w", err)
	}
	state.CloseChannel.PendingForceClose += len(
		resp.PendingForceClosingChannels,
	)
	state.CloseChannel.WaitingClose += len(resp.WaitingCloseChannels)

	closeReq := &lnrpc.ClosedChannelsRequest{}
	closed, err := hn.rpc.LN.ClosedChannels(ctxt, closeReq)
	if err != nil {
		return fmt.Errorf("ClosedChannels failed: %w", err)
	}
	state.CloseChannel.Closed += len(closed.Channels)

	state.OpenChannel.Pending += len(resp.PendingOpenChannels)

	return nil
}

// updatePaymentStats counts the total payments made.
func (hn *HarnessNode) updatePaymentStats(ctxt context.Context,
	state *nodeState) error {

	req := &lnrpc.ListPaymentsRequest{
		IndexOffset: state.Payment.LastIndexOffset,
	}
	resp, err := hn.rpc.LN.ListPayments(ctxt, req)
	if err != nil {
		return fmt.Errorf("ListPayments failed: %w", err)
	}

	state.Payment.LastIndexOffset = resp.LastIndexOffset
	for _, payment := range resp.Payments {
		if payment.Status == lnrpc.Payment_FAILED ||
			payment.Status == lnrpc.Payment_SUCCEEDED {

			state.Payment.Completed++
		}
	}

	state.Payment.Total += len(resp.Payments)

	return nil
}

// updateInvoiceStats counts the total invoices made.
func (hn *HarnessNode) updateInvoiceStats(ctxt context.Context,
	state *nodeState) error {

	req := &lnrpc.ListInvoiceRequest{
		NumMaxInvoices: math.MaxUint64,
		IndexOffset:    state.Invoice.LastIndexOffset,
	}
	resp, err := hn.rpc.LN.ListInvoices(ctxt, req)
	if err != nil {
		return fmt.Errorf("ListInvoices failed: %w", err)
	}

	state.Invoice.LastIndexOffset = resp.LastIndexOffset
	for _, invoice := range resp.Invoices {
		if invoice.State == lnrpc.Invoice_SETTLED ||
			invoice.State == lnrpc.Invoice_CANCELED {

			state.Invoice.Completed++
		}
	}

	state.Invoice.Total += len(resp.Invoices)

	return nil
}

// updateUTXOStats counts the total UTXOs made.
func (hn *HarnessNode) updateUTXOStats(ctxt context.Context,
	state *nodeState) error {

	req := &lnrpc.ListUnspentRequest{
		MaxConfs: math.MaxInt32,
		MinConfs: 0,
	}
	resp, err := hn.rpc.LN.ListUnspent(ctxt, req)
	if err != nil {
		return fmt.Errorf("ListUnspent failed: %w", err)
	}

	for _, utxo := range resp.Utxos {
		if utxo.Confirmations > 0 {
			state.UTXO.Confirmed++
		} else {
			state.UTXO.Unconfirmed++
		}
	}

	return nil
}

// updateEdgeStats counts the total edges.
func (hn *HarnessNode) updateEdgeStats(ctxt context.Context,
	state *nodeState) error {

	req := &lnrpc.ChannelGraphRequest{IncludeUnannounced: true}
	resp, err := hn.rpc.LN.DescribeGraph(ctxt, req)
	if err != nil {
		return fmt.Errorf("DescribeGraph failed: %w", err)
	}
	state.Edge.Total = len(resp.Edges)

	req = &lnrpc.ChannelGraphRequest{IncludeUnannounced: false}
	resp, err = hn.rpc.LN.DescribeGraph(ctxt, req)
	if err != nil {
		return fmt.Errorf("DescribeGraph failed: %w", err)
	}
	state.Edge.Public = len(resp.Edges)

	return nil
}

// updateChannelBalance creates stats for the node's channel balance.
func (hn *HarnessNode) updateChannelBalance(ctxt context.Context,
	state *nodeState) error {

	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := hn.rpc.LN.ChannelBalance(ctxt, req)
	if err != nil {
		return fmt.Errorf("ChannelBalance failed: %w", err)
	}

	state.Balance.LocalBalance = resp.LocalBalance
	state.Balance.RemoteBalance = resp.RemoteBalance
	state.Balance.UnsettledLocalBalance = resp.UnsettledLocalBalance
	state.Balance.UnsettledRemoteBalance = resp.UnsettledRemoteBalance
	state.Balance.PendingOpenLocalBalance = resp.PendingOpenLocalBalance
	state.Balance.PendingOpenRemoteBalance = resp.PendingOpenRemoteBalance

	return nil
}

// updateWalletBalance creates stats for the node's wallet balance.
func (hn *HarnessNode) updateWalletBalance(ctxt context.Context,
	state *nodeState) error {

	req := &lnrpc.WalletBalanceRequest{}
	resp, err := hn.rpc.LN.WalletBalance(ctxt, req)
	if err != nil {
		return fmt.Errorf("WalletBalance failed: %w", err)
	}

	state.Wallet.TotalBalance = resp.TotalBalance
	state.Wallet.ConfirmedBalance = resp.ConfirmedBalance
	state.Wallet.UnconfirmedBalance = resp.UnconfirmedBalance
	state.Wallet.AccountBalance = resp.AccountBalance

	return nil
}

// updateState updates the internal state of the node. It also resets the
// private fields in the nodeState.
func (hn *HarnessNode) updateState() error {
	ctxt, cancel := context.WithTimeout(hn.runCtx, DefaultTimeout)
	defer cancel()

	state := newState()

	if err := hn.updateChannelStats(ctxt, state); err != nil {
		return fmt.Errorf("update channel state failed: %v", err)
	}

	if err := hn.updateCloseChannelStats(ctxt, state); err != nil {
		return fmt.Errorf("update closed channel state failed: %v", err)
	}

	if err := hn.updatePaymentStats(ctxt, state); err != nil {
		return fmt.Errorf("update payment state failed: %v", err)
	}

	if err := hn.updateInvoiceStats(ctxt, state); err != nil {
		return fmt.Errorf("update invoice state failed: %v", err)
	}

	if err := hn.updateUTXOStats(ctxt, state); err != nil {
		return fmt.Errorf("update utxo state failed: %v", err)
	}

	if err := hn.updateEdgeStats(ctxt, state); err != nil {
		return fmt.Errorf("update edge state failed: %v", err)
	}

	if err := hn.updateChannelBalance(ctxt, state); err != nil {
		return fmt.Errorf("update channel balance failed: %v", err)
	}

	if err := hn.updateWalletBalance(ctxt, state); err != nil {
		return fmt.Errorf("update wallet balance failed: %v", err)
	}

	// Overwrite the old state.
	hn.state = state

	return nil
}

package lntest

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchtowerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// =====================
// Miner related RPCs.
// =====================

// NewMinerAddress creates a new address for the miner and asserts.
func (h *HarnessTest) NewMinerAddress() btcutil.Address {
	addr, err := h.net.Miner.NewAddress()
	require.NoError(h, err, "failed to create new miner address")
	return addr
}

// GetRawTransaction makes a RPC call to the miner's GetRawTransaction and
// asserts.
func (h *HarnessTest) GetRawTransaction(txid *chainhash.Hash) *btcutil.Tx {
	tx, err := h.net.Miner.Client.GetRawTransaction(txid)
	require.NoErrorf(h, err, "failed to get raw tx: %v", txid)
	return tx
}

// GetRawMempool makes a RPC call to the miner's GetRawMempool and
// asserts.
func (h *HarnessTest) GetRawMempool() []*chainhash.Hash {
	mempool, err := h.net.Miner.Client.GetRawMempool()
	require.NoError(h, err, "unable to get mempool")
	return mempool
}

// GetBestBlock makes a RPC request to miner and asserts.
func (h *HarnessTest) GetBestBlock() (*chainhash.Hash, int32) {
	blockHash, height, err := h.net.Miner.Client.GetBestBlock()
	require.NoError(h, err, "failed to GetBestBlock")
	return blockHash, height
}

// SendOutputsWithoutChange uses the miner to send the given outputs using the
// specified fee rate and returns the txid.
func (h *HarnessTest) SendOutputsWithoutChange(
	outputs []*wire.TxOut, feeRate btcutil.Amount) *chainhash.Hash {

	txid, err := h.net.Miner.SendOutputsWithoutChange(
		outputs, feeRate,
	)
	require.NoError(h, err, "failed to send output")

	return txid
}

// CreateTransaction uses the miner to create a transaction using the given
// outputs using the specified fee rate and returns the transaction.
func (h *HarnessTest) CreateTransaction(
	outputs []*wire.TxOut, feeRate btcutil.Amount) *wire.MsgTx {

	tx, err := h.net.Miner.CreateTransaction(outputs, feeRate, false)
	require.NoError(h, err, "failed to create transaction")

	return tx
}

// GenerateBlocks mine 'num' of blocks and returns them.
func (h *HarnessTest) GenerateBlocks(num uint32) []*chainhash.Hash {
	blockHashes, err := h.net.Miner.Client.Generate(num)
	require.NoError(h, err, "unable to generate blocks")
	require.Len(h, blockHashes, int(num), "wrong num of blocks generated")

	return blockHashes
}

// =====================
// LightningClient related RPCs.
// =====================

// GetRecoveryInfo uses the specifies node to make a RPC call to
// GetRecoveryInfo and asserts.
func (h *HarnessTest) GetRecoveryInfo(
	hn *HarnessNode) *lnrpc.GetRecoveryInfoResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.GetRecoveryInfoRequest{}
	resp, err := hn.rpc.LN.GetRecoveryInfo(ctxt, req)
	require.NoError(h, err, "failed to GetRecoveryInfo")

	return resp
}

// GetInfo calls the GetInfo RPC on a given node and asserts there's no error.
func (h *HarnessTest) GetInfo(n *HarnessNode) *lnrpc.GetInfoResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	info, err := n.rpc.LN.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	require.NoErrorf(h, err, "failed to GetInfo for node: %s", n.Cfg.Name)

	return info
}

// GetInfoAssertErr calls the GetInfo RPC on a given node and asserts there's
// an error.
func (h *HarnessTest) GetInfoAssertErr(n *HarnessNode) error {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := n.rpc.LN.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	require.Error(h, err, "expect an error from GetInfo")

	return err
}

// OpenChannelParams houses the params to specify when opening a new channel.
type OpenChannelParams struct {
	// Amt is the local amount being put into the channel.
	Amt btcutil.Amount

	// PushAmt is the amount that should be pushed to the remote when the
	// channel is opened.
	PushAmt btcutil.Amount

	// Private is a boolan indicating whether the opened channel should be
	// private.
	Private bool

	// SpendUnconfirmed is a boolean indicating whether we can utilize
	// unconfirmed outputs to fund the channel.
	SpendUnconfirmed bool

	// MinHtlc is the htlc_minimum_msat value set when opening the channel.
	MinHtlc lnwire.MilliSatoshi

	// RemoteMaxHtlcs is the remote_max_htlcs value set when opening the
	// channel, restricting the number of concurrent HTLCs the remote party
	// can add to a commitment.
	RemoteMaxHtlcs uint16

	// FundingShim is an optional funding shim that the caller can specify
	// in order to modify the channel funding workflow.
	FundingShim *lnrpc.FundingShim

	// SatPerVByte is the amount of satoshis to spend in chain fees per
	// virtual byte of the transaction.
	SatPerVByte btcutil.Amount

	// CommitmentType is the commitment type that should be used for the
	// channel to be opened.
	CommitmentType lnrpc.CommitmentType
}

type OpenChanClient lnrpc.Lightning_OpenChannelClient

// OpenChannelStreamAndAssert blocks until an OpenChannel request for a channel
// funding by alice succeeds. If it does, a stream client is returned to
// receive events about the opening channel.
func (h *HarnessTest) OpenChannelStreamAndAssert(a, b *HarnessNode,
	p OpenChannelParams) OpenChanClient {

	var client OpenChanClient
	// Wait until we are able to fund a channel successfully. This wait
	// prevents us from erroring out when trying to create a channel while
	// the node is starting up.
	err := wait.NoError(func() error {
		chanOpenUpdate, err := h.net.OpenChannel(a, b, p)
		client = chanOpenUpdate
		return err
	}, ChannelOpenTimeout)
	require.NoError(h, err, "timeout when open channel between %s "+
		"and %s", a.Name(), b.Name())

	return client
}

// ReceiveChanUpdate waits until a message is received on the stream or the
// timeout is reached.
func (h *HarnessTest) ReceiveChanUpdate(
	stream OpenChanClient) *lnrpc.OpenStatusUpdate {

	chanMsg := make(chan *lnrpc.OpenStatusUpdate)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout before chan pending "+
			"update sent")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// ListChannels list the channels for the given node and asserts it's
// successful.
func (h *HarnessTest) ListChannels(
	hn *HarnessNode) *lnrpc.ListChannelsResponse {

	req := &lnrpc.ListChannelsRequest{}

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.ListChannels(ctxt, req)
	require.NoErrorf(h, err, "unable ListChannels for %s", hn.Name())

	return resp
}

type CloseChanClient lnrpc.Lightning_CloseChannelClient

// CloseChannelStreamAndAssert blocks until an CloseChannel request for a
// given channel point succeeds. If it does, a stream client is returned
// to receive events about the closing channel.
func (h *HarnessTest) CloseChannelStreamAndAssert(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint, force bool) CloseChanClient {

	var (
		client CloseChanClient
		err    error
	)

	req := &lnrpc.CloseChannelRequest{
		ChannelPoint: chanPoint,
		Force:        force,
	}

	err = wait.NoError(func() error {
		// Use runCtx here instead of a timeout context to keep the
		// client alive for the entire test case.
		client, err = hn.rpc.LN.CloseChannel(h.runCtx, req)
		if err != nil {
			return fmt.Errorf("unable to close channel for node %s"+
				" with channel point %s", hn.Name(), chanPoint)
		}
		return nil
	}, ChannelCloseTimeout)

	require.NoError(h, err, "timeout closing channel")

	return client
}

// ReceiveCloseChannelUpdate waits until a message is received on the subscribe
// channel close stream or the timeout is reached.
func (h *HarnessTest) ReceiveCloseChannelUpdate(
	stream CloseChanClient) *lnrpc.CloseStatusUpdate {

	chanMsg := make(chan *lnrpc.CloseStatusUpdate)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout receiving close update")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// GetPendingChannels makes a RPC request tc PendingChannels and asserts
// there's no error.
func (h *HarnessTest) GetPendingChannels(
	hn *HarnessNode) *lnrpc.PendingChannelsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	var (
		resp *lnrpc.PendingChannelsResponse
		err  error
	)

	waitErr := wait.NoError(func() error {
		pendingChansRequest := &lnrpc.PendingChannelsRequest{}
		resp, err = hn.rpc.LN.PendingChannels(
			ctxt, pendingChansRequest,
		)

		// TODO(yy): the RPC call should not return an error. If an
		// error is returned, it indicates there's something wrong
		// regarding how we prepare the channel info. This could happen
		// when the channel happens to be transitioning states. We log
		// it here, but this needs to be fixed in the RPC server.
		if err != nil {
			h.Logf("%s PendingChannels got err: %v", hn.Name(), err)
		}
		return err
	}, DefaultTimeout)

	require.NoError(h, waitErr, "failed to get pending channels for %s",
		hn.Name())

	return resp
}

// ListPeers makes a RPC call to the node's ListPeers and asserts.
func (h *HarnessTest) ListPeers(hn *HarnessNode) *lnrpc.ListPeersResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.ListPeers(ctxt, &lnrpc.ListPeersRequest{})

	require.NoErrorf(h, err, "%s ListPeers failed with: %v",
		hn.Name(), err)

	return resp
}

// AddInvoice adds a invoice for the given node and asserts.
func (h *HarnessTest) AddInvoice(req *lnrpc.Invoice,
	hn *HarnessNode) *lnrpc.AddInvoiceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	invoice, err := hn.rpc.LN.AddInvoice(ctxt, req)
	require.NoError(h, err)

	return invoice
}

// ListInvoices list the node's invoice using the request and asserts.
func (h *HarnessTest) ListInvoices(hn *HarnessNode) *lnrpc.ListInvoiceResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ListInvoiceRequest{
		NumMaxInvoices: math.MaxUint64,
		IndexOffset:    hn.state.Invoice.LastIndexOffset,
	}
	resp, err := hn.rpc.LN.ListInvoices(ctxt, req)
	require.NoErrorf(h, err, "list invoice failed")

	return resp
}

// ListPayments lists the node's payments and asserts.
func (h *HarnessTest) ListPayments(hn *HarnessNode,
	includeIncomplete bool) *lnrpc.ListPaymentsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ListPaymentsRequest{
		IncludeIncomplete: includeIncomplete,
		IndexOffset:       hn.state.Payment.LastIndexOffset,
	}
	resp, err := hn.rpc.LN.ListPayments(ctxt, req)
	require.NoError(h, err, "failed to list payments")

	return resp
}

// GetTransactions makes a RPC call to GetTransactions and asserts.
func (h *HarnessTest) GetTransactions(
	hn *HarnessNode) *lnrpc.TransactionDetails {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.GetTransactions(
		ctxt, &lnrpc.GetTransactionsRequest{},
	)
	require.NoError(h, err, "failed to GetTransactions for %s", hn.Name())

	return resp
}

// WalletBalance makes a RPC call to WalletBalance and asserts.
func (h *HarnessTest) WalletBalance(
	hn *HarnessNode) *lnrpc.WalletBalanceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.WalletBalanceRequest{}
	resp, err := hn.rpc.LN.WalletBalance(ctxt, req)
	require.NoError(h, err, "failed to get wallet balance for node %s",
		hn.Name())

	return resp
}

// ListUnspent makes a RPC call to ListUnspent and asserts.
func (h *HarnessTest) ListUnspent(hn *HarnessNode, account string,
	max, min int32) *lnrpc.ListUnspentResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ListUnspentRequest{
		Account:  account,
		MaxConfs: max,
		MinConfs: min,
	}
	resp, err := hn.rpc.LN.ListUnspent(ctxt, req)
	require.NoError(h, err, "failed to list utxo for node %s", hn.Name())

	return resp
}

// NewAddress makes a RPC call to NewAddress and asserts.
func (h *HarnessTest) NewAddress(hn *HarnessNode,
	addrType lnrpc.AddressType) *lnrpc.NewAddressResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.NewAddressRequest{Type: addrType}
	resp, err := hn.rpc.LN.NewAddress(ctxt, req)
	require.NoError(h, err, "failed to create new address for node %s",
		hn.Name())

	return resp
}

// NewAddressWithAccount makes a RPC call to NewAddress and asserts.
func (h *HarnessTest) NewAddressWithAccount(hn *HarnessNode,
	addrType lnrpc.AddressType, account string) *lnrpc.NewAddressResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.NewAddressRequest{Type: addrType, Account: account}
	resp, err := hn.rpc.LN.NewAddress(ctxt, req)
	require.NoError(h, err, "failed to create new address for node %s",
		hn.Name())

	return resp
}

// SendCoinFromNode sends a given amount of money to the specified address from
// the passed node.
func (h *HarnessTest) SendCoinFromNode(hn *HarnessNode,
	req *lnrpc.SendCoinsRequest) *lnrpc.SendCoinsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.SendCoins(ctxt, req)
	require.NoError(h, err, "node %s failed to send coins to address %s",
		hn.Name(), req.Addr)

	return resp
}

// SendCoinFromNodeErr sends a given amount of money to the specified address
// from the passed node and asserts an error has returned.
func (h *HarnessTest) SendCoinFromNodeErr(hn *HarnessNode,
	req *lnrpc.SendCoinsRequest) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.LN.SendCoins(ctxt, req)
	require.Error(h, err, "node %s didn't not return an error", hn.Name())
}

// ChannelBalance gets the channel balance and asserts.
func (h *HarnessTest) ChannelBalance(
	hn *HarnessNode) *lnrpc.ChannelBalanceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := hn.rpc.LN.ChannelBalance(ctxt, req)

	require.NoError(h, err, "unable to get balance for node %s", hn.Name())
	return resp
}

// FundingStateStep makes a RPC call to FundingStateStep and asserts.
func (h *HarnessTest) FundingStateStep(hn *HarnessNode,
	msg *lnrpc.FundingTransitionMsg) *lnrpc.FundingStateStepResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.FundingStateStep(ctxt, msg)
	require.NoError(h, err, "failed to step state")

	return resp
}

// FundingStateStepAssertErr makes a RPC call to FundingStateStep and asserts
// there's an error.
func (h *HarnessTest) FundingStateStepAssertErr(hn *HarnessNode,
	msg *lnrpc.FundingTransitionMsg) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.LN.FundingStateStep(ctxt, msg)
	require.Error(h, err, "duplicate pending channel ID funding shim "+
		"registration should trigger an error")
}

// AbandonChannel makes a RPC call to AbandonChannel and asserts.
func (h *HarnessTest) AbandonChannel(hn *HarnessNode,
	req *lnrpc.AbandonChannelRequest) *lnrpc.AbandonChannelResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.AbandonChannel(ctxt, req)
	require.NoError(h, err, "abandon channel failed")

	return resp
}

// BatchOpenChannel makes a RPC call to BatchOpenChannel and asserts.
func (h *HarnessTest) BatchOpenChannel(hn *HarnessNode,
	req *lnrpc.BatchOpenChannelRequest) *lnrpc.BatchOpenChannelResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.BatchOpenChannel(ctxt, req)
	require.NoError(h, err, "failed to batch open channel")

	return resp
}

// BatchOpenChannelAssertErr makes a RPC call to BatchOpenChannel and asserts
// there's an error returned.
func (h *HarnessTest) BatchOpenChannelAssertErr(hn *HarnessNode,
	req *lnrpc.BatchOpenChannelRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.LN.BatchOpenChannel(ctxt, req)
	require.Error(h, err, "expecte batch open channel fail")

	return err
}

// QueryRoutes makes a RPC call to QueryRoutes and asserts.
func (h *HarnessTest) QueryRoutes(hn *HarnessNode,
	req *lnrpc.QueryRoutesRequest) *lnrpc.QueryRoutesResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	routes, err := hn.rpc.LN.QueryRoutes(ctxt, req)
	require.NoError(h, err, "failed to query routes")

	return routes
}

// SendToRoute makes a RPC call to SendToRoute and asserts.
func (h *HarnessTest) SendToRoute(
	hn *HarnessNode) lnrpc.Lightning_SendToRouteClient {

	// SendToRoute needs to have the context alive for the entire test case
	// as the returned client will be used for send and receive payment
	// stream. Thus we use runCtx here instead of a timeout context.
	client, err := hn.rpc.LN.SendToRoute(h.runCtx) // nolint:staticcheck
	require.NoError(h, err, "failed to send to route")

	return client
}

// SendToRouteSync makes a RPC call to SendToRouteSync and asserts.
func (h *HarnessTest) SendToRouteSync(hn *HarnessNode,
	req *lnrpc.SendToRouteRequest) *lnrpc.SendResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.SendToRouteSync(ctxt, req)
	require.NoErrorf(h, err, "unable to send to route for %s", hn.Name())

	return resp
}

// UpdateChannelPolicy makes a RPC call to UpdateChannelPolicy and asserts.
func (h *HarnessTest) UpdateChannelPolicy(hn *HarnessNode,
	req *lnrpc.PolicyUpdateRequest) *lnrpc.PolicyUpdateResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.UpdateChannelPolicy(ctxt, req)
	require.NoError(h, err, "failed to update policy")

	return resp
}

// DescribeGraph makes a RPC call to the node's DescribeGraph and asserts. It
// takes a bool to indicate whether we want to include private edges or not.
func (h *HarnessTest) DescribeGraph(hn *HarnessNode,
	includeUnannounced bool) *lnrpc.ChannelGraph {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: includeUnannounced,
	}
	resp, err := hn.rpc.LN.DescribeGraph(ctxt, req)
	require.NoError(h, err, "failed to describe graph")

	return resp
}

// SubscribeChannelEvents creates a subscription client for channel events and
// asserts its creation.
func (h *HarnessTest) SubscribeChannelEvents(
	hn *HarnessNode) lnrpc.Lightning_SubscribeChannelEventsClient {

	req := &lnrpc.ChannelEventSubscription{}

	// SubscribeChannelEvents needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	client, err := hn.rpc.LN.SubscribeChannelEvents(h.runCtx, req)
	require.NoError(h, err, "unable to create channel update client")

	return client
}

// LookupInvoice queries the node's invoices using the specified rHash.
func (h *HarnessTest) LookupInvoice(hn *HarnessNode,
	rHash []byte) *lnrpc.Invoice {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	payHash := &lnrpc.PaymentHash{RHash: rHash}
	resp, err := hn.rpc.LN.LookupInvoice(ctxt, payHash)
	require.NoError(h, err, "unable to lookup invoice")

	return resp
}

// LookupInvoiceV2 queries the node's invoices using the invoice client's
// LookupInvoiceV2.
func (h *HarnessTest) LookupInvoiceV2(hn *HarnessNode,
	req *invoicesrpc.LookupInvoiceMsg) *lnrpc.Invoice {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Invoice.LookupInvoiceV2(ctxt, req)
	require.NoError(h, err, "unable to lookup invoice")

	return resp
}

// ExportAllChanBackups makes a RPC call to the node's ExportAllChannelBackups
// and asserts.
func (h *HarnessTest) ExportAllChanBackups(
	hn *HarnessNode) *lnrpc.ChanBackupSnapshot {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ChanBackupExportRequest{}
	chanBackup, err := hn.rpc.LN.ExportAllChannelBackups(ctxt, req)
	require.NoError(h, err, "unable to obtain channel backup")

	return chanBackup
}

// ExportChanBackup makes a RPC call to the node's ExportChannelBackup
// and asserts.
func (h *HarnessTest) ExportChanBackup(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.ChannelBackup {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ExportChannelBackupRequest{
		ChanPoint: chanPoint,
	}
	chanBackup, err := hn.rpc.LN.ExportChannelBackup(ctxt, req)
	require.NoError(h, err, "unable to obtain channel backup")

	return chanBackup
}

// RestoreChanBackups makes a RPC call to the node's RestoreChannelBackups and
// asserts.
func (h *HarnessTest) RestoreChanBackups(hn *HarnessNode,
	req *lnrpc.RestoreChanBackupRequest) *lnrpc.RestoreBackupResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.RestoreChannelBackups(ctxt, req)
	require.NoError(h, err, "unable to restore backups")

	return resp
}

// ConnectPeerAssertErr makes a RPC call to ConnectPeer and asserts an error
// returned.
func (h *HarnessTest) ConnectPeerAssertErr(hn *HarnessNode,
	req *lnrpc.ConnectPeerRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.LN.ConnectPeer(ctxt, req)
	require.Error(h, err, "expected an error from ConnectPeer")

	return err
}

// DisconnectPeer calls the DisconnectPeer RPC on a given node with a specified
// public key string and asserts there's no error.
func (h *HarnessTest) DisconnectPeer(hn *HarnessNode,
	pubkey string) *lnrpc.DisconnectPeerResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.DisconnectPeerRequest{PubKey: pubkey}

	resp, err := hn.rpc.LN.DisconnectPeer(ctxt, req)
	require.NoError(h, err, "failed to disconnect peer")

	return resp
}

// DecodePayReq makes a RPC call to node's DecodePayReq and asserts.
func (h *HarnessTest) DecodePayReq(hn *HarnessNode, req string) *lnrpc.PayReq {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	payReq := &lnrpc.PayReqString{PayReq: req}
	resp, err := hn.rpc.LN.DecodePayReq(ctxt, payReq)
	require.NoError(h, err, "failed to decode pay req")

	return resp
}

// ForwardingHistory makes a RPC call to the node's ForwardingHistory and
// asserts.
func (h *HarnessTest) ForwardingHistory(
	hn *HarnessNode) *lnrpc.ForwardingHistoryResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.ForwardingHistory(
		ctxt, &lnrpc.ForwardingHistoryRequest{},
	)
	require.NoError(h, err, "failed to query forwarding history")

	return resp
}

// DeleteAllPayments makes a RPC call to the node's DeleteAllPayments and
// asserts.
func (h *HarnessTest) DeleteAllPayments(hn *HarnessNode) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.DeleteAllPaymentsRequest{}
	_, err := hn.rpc.LN.DeleteAllPayments(ctxt, req)
	require.NoError(h, err, "unable to delete payments")
}

type BackupSubscriber lnrpc.Lightning_SubscribeChannelBackupsClient

// SubscribeChannelBackups creates a client to listen to channel backup stream.
func (h *HarnessTest) SubscribeChannelBackups(
	hn *HarnessNode) BackupSubscriber {

	// Use runCtx here instead of timeout context to keep the stream client
	// alive.
	backupStream, err := hn.rpc.LN.SubscribeChannelBackups(
		h.runCtx, &lnrpc.ChannelBackupSubscription{},
	)
	require.NoError(h, err, "unable to create backup stream")

	return backupStream
}

// VerifyChanBackup makes a RPC call to node's VerifyChanBackup and asserts.
func (h *HarnessTest) VerifyChanBackup(hn *HarnessNode,
	ss *lnrpc.ChanBackupSnapshot) *lnrpc.VerifyChanBackupResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.VerifyChanBackup(ctxt, ss)
	require.NoError(h, err, "unable to verify backup")

	return resp
}

// SignMessage makes a RPC call to node's SignMessage and asserts.
func (h *HarnessTest) SignMessage(hn *HarnessNode,
	msg []byte) *lnrpc.SignMessageResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.SignMessageRequest{Msg: msg}
	resp, err := hn.rpc.LN.SignMessage(ctxt, req)
	require.NoError(h, err, "SignMessage rpc call failed")

	return resp
}

// VerifyMessage makes a RPC call to node's VerifyMessage and asserts.
func (h *HarnessTest) VerifyMessage(hn *HarnessNode, msg []byte,
	sig string) *lnrpc.VerifyMessageResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.VerifyMessageRequest{Msg: msg, Signature: sig}
	resp, err := hn.rpc.LN.VerifyMessage(ctxt, req)
	require.NoError(h, err, "VerifyMessage failed")

	return resp
}

// ClosedChannels makes a RPC call to node's ClosedChannels and asserts.
func (h *HarnessTest) ClosedChannels(hn *HarnessNode,
	abandoned bool) *lnrpc.ClosedChannelsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ClosedChannelsRequest{
		Abandoned: abandoned,
	}
	resp, err := hn.rpc.LN.ClosedChannels(ctxt, req)
	require.NoError(h, err, "list closed channels failed")

	return resp
}

type InvoiceUpdateClient lnrpc.Lightning_SubscribeInvoicesClient

// SubscribeInvoices creates a subscription client for invoice events and
// asserts its creation.
//
// NOTE: make sure to subscribe an invoice as early as possible as it takes
// some time for the lnd to create the subscription client. If an invoice is
// added right after the subscription, it may be missed. However, if AddIndex
// or SettleIndex is used in the request, it will be fine as a backlog will
// always be sent.
// TODO(yy): add an invoice subscription to each node like the topologyClient?
func (h *HarnessTest) SubscribeInvoices(hn *HarnessNode,
	req *lnrpc.InvoiceSubscription) InvoiceUpdateClient {

	// SubscribeInvoices needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	client, err := hn.rpc.LN.SubscribeInvoices(h.runCtx, req)
	require.NoError(h, err, "unable to create invoice subscription client")

	return client
}

// ReceiveInvoiceUpdate waits until a message is received on the subscribe
// invoice stream or the timeout is reached.
func (h *HarnessTest) ReceiveInvoiceUpdate(
	stream InvoiceUpdateClient) *lnrpc.Invoice {

	chanMsg := make(chan *lnrpc.Invoice)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout receiving invoice update")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

type MiddlewareClient lnrpc.Lightning_RegisterRPCMiddlewareClient

// RegisterRPCMiddleware makes a RPC call to the node's RegisterRPCMiddleware
// and asserts. It also returns a cancel context which can cancel the context
// used by the client.
func (h *HarnessTest) RegisterRPCMiddleware(
	hn *HarnessNode) (MiddlewareClient, context.CancelFunc) {

	ctxt, cancel := context.WithCancel(h.runCtx)

	stream, err := hn.rpc.LN.RegisterRPCMiddleware(ctxt)
	require.NoError(h, err, "failed to register rpc middleware")

	return stream, cancel
}

// =====================
// RouterClient related RPCs.
// =====================

type PaymentClient routerrpc.Router_SendPaymentV2Client

// SendPayment sends a payment using the given node and payment request. It
// also asserts the payment being sent successfully.
func (h *HarnessTest) SendPayment(hn *HarnessNode,
	req *routerrpc.SendPaymentRequest) PaymentClient {

	// SendPayment needs to have the context alive for the entire test case
	// as the router relies on the context to propagate HTLCs. Thus we use
	// runCtx here instead of a timeout context.
	stream, err := hn.rpc.Router.SendPaymentV2(h.runCtx, req)
	require.NoErrorf(h, err, "%s failed to send payment", hn.Name())
	return stream
}

// SendToRouteV2 makes a RPC call to SendToRouteV2 and asserts.
func (h *HarnessTest) SendToRouteV2(hn *HarnessNode,
	req *routerrpc.SendToRouteRequest) *lnrpc.HTLCAttempt {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Router.SendToRouteV2(ctxt, req)
	require.NoErrorf(h, err, "unable to send to route v2 for %s", hn.Name())

	return resp
}

// UpdateChanStatus makes a UpdateChanStatus RPC call to node's RouterClient
// and asserts.
func (h *HarnessTest) UpdateChanStatus(hn *HarnessNode,
	req *routerrpc.UpdateChanStatusRequest) *routerrpc.UpdateChanStatusResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Router.UpdateChanStatus(ctxt, req)
	require.NoErrorf(h, err, "UpdateChanStatus failed")

	return resp
}

type HtlcEventsClient routerrpc.Router_SubscribeHtlcEventsClient

// SubscribeHtlcEvents makes a subscription to the HTLC events and returns a
// htlc event client.
func (h *HarnessTest) SubscribeHtlcEvents(hn *HarnessNode) HtlcEventsClient {
	// Use runCtx here to keep the client alive for the scope of the test.
	client, err := hn.rpc.Router.SubscribeHtlcEvents(
		h.runCtx, &routerrpc.SubscribeHtlcEventsRequest{},
	)
	require.NoError(h, err, "could not subscribe events")

	return client
}

// ReceiveHtlcEvent waits until a message is received on the subscribe
// htlc event stream or the timeout is reached.
func (h *HarnessTest) ReceiveHtlcEvent(
	stream HtlcEventsClient) *routerrpc.HtlcEvent {

	chanMsg := make(chan *routerrpc.HtlcEvent)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout receiving htlc "+
			"event update")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// GetMissionControlConfig makes a RPC call to the node's
// GetMissionControlConfig and asserts.
func (h *HarnessTest) GetMissionControlConfig(
	hn *HarnessNode) *routerrpc.GetMissionControlConfigResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &routerrpc.GetMissionControlConfigRequest{}
	resp, err := hn.rpc.Router.GetMissionControlConfig(ctxt, req)
	require.NoError(h, err, "get mission control failed")

	return resp
}

// SetMissionControlConfig makes a RPC call to the node's
// SetMissionControlConfig and asserts.
func (h *HarnessTest) SetMissionControlConfig(hn *HarnessNode,
	config *routerrpc.MissionControlConfig) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &routerrpc.SetMissionControlConfigRequest{Config: config}
	_, err := hn.rpc.Router.SetMissionControlConfig(ctxt, req)
	require.NoError(h, err, "set mission control failed")
}

// ResetMissionControl makes a RPC call to the node's ResetMissionControl and
// asserts.
func (h *HarnessTest) ResetMissionControl(hn *HarnessNode) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &routerrpc.ResetMissionControlRequest{}
	_, err := hn.rpc.Router.ResetMissionControl(ctxt, req)
	require.NoError(h, err, "reset mission control failed")
}

// QueryProbability makes a RPC call to the node's QueryProbability and
// asserts.
func (h *HarnessTest) QueryProbability(hn *HarnessNode,
	req *routerrpc.QueryProbabilityRequest) *routerrpc.QueryProbabilityResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Router.QueryProbability(ctxt, req)
	require.NoError(h, err, "query probability failed")

	return resp
}

// XImportMissionControl makes a RPC call to the node's XImportMissionControl
// and asserts.
func (h *HarnessTest) XImportMissionControl(hn *HarnessNode,
	req *routerrpc.XImportMissionControlRequest) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.Router.XImportMissionControl(ctxt, req)
	require.NoError(h, err, "x import mission control failed")
}

// XImportMissionControlAssertErr makes a RPC call to the node's XImportMissionControl
// and asserts an error occurred.
func (h *HarnessTest) XImportMissionControlAssertErr(hn *HarnessNode,
	req *routerrpc.XImportMissionControlRequest) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.Router.XImportMissionControl(ctxt, req)
	require.Error(h, err, "expect an error from x import mission control")
}

type TrackPaymentClient routerrpc.Router_TrackPaymentV2Client

// TrackPaymentV2 creates a subscription client for given invoice and
// asserts its creation.
func (h *HarnessTest) TrackPaymentV2(hn *HarnessNode,
	payHash []byte) TrackPaymentClient {

	req := &routerrpc.TrackPaymentRequest{PaymentHash: payHash}

	// TrackPaymentV2 needs to have the context alive for the entire test
	// case as the returned client will be used for send and receive events
	// stream. Thus we use runCtx here instead of a timeout context.
	client, err := hn.rpc.Router.TrackPaymentV2(h.runCtx, req)
	require.NoError(h, err, "unable to create track payment client")

	return client
}

// ReceiveTrackPayment waits until a message is received on the track payment
// stream or the timeout is reached.
func (h *HarnessTest) ReceiveTrackPayment(
	stream TrackPaymentClient) *lnrpc.Payment {

	chanMsg := make(chan *lnrpc.Payment)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout trakcing payment")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// BuildRoute makes a RPC call to the node's RouterClient and asserts.
func (h *HarnessTest) BuildRoute(hn *HarnessNode,
	req *routerrpc.BuildRouteRequest) *routerrpc.BuildRouteResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Router.BuildRoute(ctxt, req)
	require.NoError(h, err, "failed to build route")
	return resp
}

type InterceptorClient routerrpc.Router_HtlcInterceptorClient

// HtlcInterceptor makes a RPC call to the node's RouterClient and asserts.
func (h *HarnessTest) HtlcInterceptor(hn *HarnessNode) (
	InterceptorClient, context.CancelFunc) {

	// HtlcInterceptor needs to have the context alive for the entire test
	// case as the returned client will be used for send and receive events
	// stream. Thus we use cancel context here instead of a timeout
	// context.
	ctxt, cancel := context.WithCancel(h.runCtx)
	resp, err := hn.rpc.Router.HtlcInterceptor(ctxt)

	require.NoError(h, err, "failed to create HtlcInterceptor")

	return resp, cancel
}

// ReceiveHtlcInterceptor waits until a message is received on the htlc
// interceptor stream or the timeout is reached.
func (h *HarnessTest) ReceiveHtlcInterceptor(
	stream InterceptorClient) *routerrpc.ForwardHtlcInterceptRequest {

	chanMsg := make(chan *routerrpc.ForwardHtlcInterceptRequest)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout intercepting htlc")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// =====================
// InvoiceClient related RPCs.
// =====================

// AddHoldInvoice adds a hold invoice for the given node and asserts.
func (h *HarnessTest) AddHoldInvoice(
	req *invoicesrpc.AddHoldInvoiceRequest,
	hn *HarnessNode) *invoicesrpc.AddHoldInvoiceResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	invoice, err := hn.rpc.Invoice.AddHoldInvoice(ctxt, req)
	require.NoError(h, err)

	return invoice
}

// SettleInvoice settles a given invoice and asserts.
func (h *HarnessTest) SettleInvoice(hn *HarnessNode,
	preimage []byte) *invoicesrpc.SettleInvoiceResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &invoicesrpc.SettleInvoiceMsg{Preimage: preimage}
	resp, err := hn.rpc.Invoice.SettleInvoice(ctxt, req)
	require.NoError(h, err)

	return resp
}

// CancelInvoice cancels a given invoice and asserts.
func (h *HarnessTest) CancelInvoice(hn *HarnessNode,
	preimage []byte) *invoicesrpc.CancelInvoiceResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &invoicesrpc.CancelInvoiceMsg{PaymentHash: preimage}
	resp, err := hn.rpc.Invoice.CancelInvoice(ctxt, req)
	require.NoError(h, err)

	return resp
}

type SingleInvoiceClient invoicesrpc.Invoices_SubscribeSingleInvoiceClient

// SubscribeSingleInvoice creates a subscription client for given invoice and
// asserts its creation.
func (h *HarnessTest) SubscribeSingleInvoice(hn *HarnessNode,
	rHash []byte) SingleInvoiceClient {

	req := &invoicesrpc.SubscribeSingleInvoiceRequest{RHash: rHash}

	// SubscribeSingleInvoice needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	client, err := hn.rpc.Invoice.SubscribeSingleInvoice(h.runCtx, req)
	require.NoError(h, err, "unable to create single invoice client")

	return client
}

// ReceiveSingleInvoice waits until a message is received on the subscribe
// single invoice stream or the timeout is reached.
func (h *HarnessTest) ReceiveSingleInvoice(
	stream SingleInvoiceClient) *lnrpc.Invoice {

	chanMsg := make(chan *lnrpc.Invoice)
	errChan := make(chan error)
	go func() {
		// Consume one message. This will block until the message is
		// received.
		resp, err := stream.Recv()
		if err != nil {
			errChan <- err
			return
		}
		chanMsg <- resp
	}()

	select {
	case <-time.After(DefaultTimeout):
		require.Fail(h, "timeout", "timeout receiving single invoice")

	case err := <-errChan:
		require.Failf(h, "err from stream",
			"received err from stream: %v", err)

	case updateMsg := <-chanMsg:
		return updateMsg
	}

	return nil
}

// =====================
// WalletKitClient related RPCs.
// =====================

// DeriveKey makes a RPC call to the DeriveKey and asserts.
func (h *HarnessTest) DeriveKey(hn *HarnessNode,
	kl *signrpc.KeyLocator) *signrpc.KeyDescriptor {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	key, err := hn.rpc.WalletKit.DeriveKey(ctxt, kl)
	require.NoError(h, err, "failed to derive key for node %s", hn.Name())

	return key
}

// DeriveNextKey makes a RPC call to the DeriveNextKey and asserts.
func (h *HarnessTest) DeriveNextKey(hn *HarnessNode,
	req *walletrpc.KeyReq) *signrpc.KeyDescriptor {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	key, err := hn.rpc.WalletKit.DeriveNextKey(ctxt, req)
	require.NoError(h, err, "failed to derive next key for node %s",
		hn.Name())

	return key
}

// FundPsbt makes a RPC call to node's FundPsbt and asserts.
func (h *HarnessTest) FundPsbt(hn *HarnessNode,
	req *walletrpc.FundPsbtRequest) *walletrpc.FundPsbtResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.FundPsbt(ctxt, req)
	require.NoError(h, err, "fund psbt failed")

	return resp
}

// FinalizePsbt makes a RPC call to node's FundPsbt and asserts.
func (h *HarnessTest) FinalizePsbt(hn *HarnessNode,
	req *walletrpc.FinalizePsbtRequest) *walletrpc.FinalizePsbtResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.FinalizePsbt(ctxt, req)
	require.NoError(h, err, "finalize psbt failed")

	return resp
}

// LabelTransactionAssertErr makes a RPC call to the node's LabelTransaction
// and asserts an error is returned. It then returns the error.
func (h *HarnessTest) LabelTransactionAssertErr(hn *HarnessNode,
	req *walletrpc.LabelTransactionRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.WalletKit.LabelTransaction(ctxt, req)
	require.Error(h, err, "expected error returned")

	return err
}

// LabelTransaction makes a RPC call to the node's LabelTransaction
// and asserts no error is returned.
func (h *HarnessTest) LabelTransaction(hn *HarnessNode,
	req *walletrpc.LabelTransactionRequest) {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.WalletKit.LabelTransaction(ctxt, req)
	require.NoError(h, err, "failed to label transaction")
}

// ListSweeps makes a ListSweeps RPC call to the node's WalletKit client.
func (h *HarnessTest) ListSweeps(hn *HarnessNode,
	verbose bool) *walletrpc.ListSweepsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &walletrpc.ListSweepsRequest{Verbose: verbose}
	resp, err := hn.rpc.WalletKit.ListSweeps(ctxt, req)
	require.NoError(h, err, "failed to ListSweeps")

	return resp
}

// BumpFee makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessTest) BumpFee(hn *HarnessNode,
	req *walletrpc.BumpFeeRequest) *walletrpc.BumpFeeResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.BumpFee(ctxt, req)
	require.NoError(h, err, "unable to bump fee")

	return resp
}

// PendingSweeps makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessTest) PendingSweeps(
	hn *HarnessNode) *walletrpc.PendingSweepsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &walletrpc.PendingSweepsRequest{}

	resp, err := hn.rpc.WalletKit.PendingSweeps(ctxt, req)
	require.NoError(h, err, "unable to get pending sweeps")

	return resp
}

// ListAccounts makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessTest) ListAccounts(hn *HarnessNode,
	req *walletrpc.ListAccountsRequest) *walletrpc.ListAccountsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.ListAccounts(ctxt, req)
	require.NoError(h, err, "failed to ListAccounts")

	return resp
}

// ImportAccount makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessTest) ImportAccount(hn *HarnessNode,
	req *walletrpc.ImportAccountRequest) *walletrpc.ImportAccountResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.ImportAccount(ctxt, req)
	require.NoError(h, err, "failed to import account")

	return resp
}

// PublishTransaction makes a RPC call to the node's WalletKitClient and
// asserts.
func (h *HarnessTest) PublishTransaction(hn *HarnessNode,
	req *walletrpc.Transaction) *walletrpc.PublishResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.PublishTransaction(ctxt, req)
	require.NoError(h, err, "failed to publish tx")

	return resp
}

// ImportPublicKey makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessTest) ImportPublicKey(hn *HarnessNode,
	req *walletrpc.ImportPublicKeyRequest) *walletrpc.ImportPublicKeyResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.ImportPublicKey(ctxt, req)
	require.NoError(h, err, "failed to import public key")

	return resp
}

// SendOutputs makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessTest) SendOutputs(hn *HarnessNode,
	req *walletrpc.SendOutputsRequest) *walletrpc.SendOutputsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.SendOutputs(ctxt, req)
	require.NoError(h, err, "failed to SendOutputs")

	return resp
}

// SignPsbt makes a RPC call to the node's WalletKitClient and asserts.
func (h *HarnessTest) SignPsbt(hn *HarnessNode,
	req *walletrpc.SignPsbtRequest) *walletrpc.SignPsbtResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WalletKit.SignPsbt(ctxt, req)
	require.NoError(h, err, "failed to SignPsbt")

	return resp
}

// =====================
// WatchtowerClient and WatchtowerClientClient related RPCs.
// =====================

// GetInfoWatchtower makes a RPC call to the watchtower of the given node and
// asserts.
func (h *HarnessTest) GetInfoWatchtower(
	hn *HarnessNode) *watchtowerrpc.GetInfoResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &watchtowerrpc.GetInfoRequest{}
	info, err := hn.rpc.Watchtower.GetInfo(ctxt, req)
	require.NoError(h, err, "failed to GetInfo from watchtower")

	return info
}

// AddTower makes a RPC call to the WatchtowerClient of the given node and
// asserts.
func (h *HarnessTest) AddTower(hn *HarnessNode,
	req *wtclientrpc.AddTowerRequest) *wtclientrpc.AddTowerResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.WatchtowerClient.AddTower(ctxt, req)
	require.NoError(h, err, "failed to AddTower")

	return resp
}

// WatchtowerStats makes a RPC call to the WatchtowerClient of the given node
// and asserts.
func (h *HarnessTest) WatchtowerStats(
	hn *HarnessNode) *wtclientrpc.StatsResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &wtclientrpc.StatsRequest{}
	resp, err := hn.rpc.WatchtowerClient.Stats(ctxt, req)
	require.NoError(h, err, "failed to call Stats")

	return resp
}

// =====================
// Signer related RPCs.
// =====================

// DeriveSharedKey makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessTest) DeriveSharedKey(hn *HarnessNode,
	req *signrpc.SharedKeyRequest) *signrpc.SharedKeyResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Signer.DeriveSharedKey(ctxt, req)
	require.NoError(h, err, "calling DeriveSharedKey failed")
	return resp
}

// DeriveSharedKeyErr makes a RPC call to the node's SignerClient and asserts
// there is an error.
func (h *HarnessTest) DeriveSharedKeyErr(hn *HarnessNode,
	req *signrpc.SharedKeyRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.Signer.DeriveSharedKey(ctxt, req)
	require.Error(h, err, "expected error from calling DeriveSharedKey")
	return err
}

// SignOutputRaw makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessTest) SignOutputRaw(hn *HarnessNode,
	req *signrpc.SignReq) *signrpc.SignResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Signer.SignOutputRaw(ctxt, req)
	require.NoError(h, err, "failed to sign raw output")

	return resp
}

// SignOutputRawErr makes a RPC call to the node's SignerClient and asserts an
// error is returned.
func (h *HarnessTest) SignOutputRawErr(hn *HarnessNode,
	req *signrpc.SignReq) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.Signer.SignOutputRaw(ctxt, req)
	require.Error(h, err, "expect to fail to sign raw output")

	return err
}

// MuSig2CreateSession makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessTest) MuSig2CreateSession(hn *HarnessNode,
	req *signrpc.MuSig2SessionRequest) *signrpc.MuSig2SessionResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Signer.MuSig2CreateSession(ctxt, req)
	require.NoError(h, err, "failed to create musig2 session")

	return resp
}

// MuSig2CombineKeys makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessTest) MuSig2CombineKeys(hn *HarnessNode,
	req *signrpc.MuSig2CombineKeysRequest) *signrpc.MuSig2CombineKeysResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Signer.MuSig2CombineKeys(ctxt, req)
	require.NoError(h, err, "failed to combine keys")

	return resp
}

// MuSig2RegisterNonces makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessTest) MuSig2RegisterNonces(hn *HarnessNode,
	req *signrpc.MuSig2RegisterNoncesRequest) *signrpc.MuSig2RegisterNoncesResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Signer.MuSig2RegisterNonces(ctxt, req)
	require.NoError(h, err, "failed to register nonces")

	return resp
}

// MuSig2Sign makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessTest) MuSig2Sign(hn *HarnessNode,
	req *signrpc.MuSig2SignRequest) *signrpc.MuSig2SignResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Signer.MuSig2Sign(ctxt, req)
	require.NoError(h, err, "failed to sign")

	return resp
}

// MuSig2SignErr makes a RPC call to the node's SignerClient and asserts an
// error is returned.
func (h *HarnessTest) MuSig2SignErr(hn *HarnessNode,
	req *signrpc.MuSig2SignRequest) error {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.Signer.MuSig2Sign(ctxt, req)
	require.Error(h, err, "expect an error")

	return err
}

// MuSig2CombineSig makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessTest) MuSig2CombineSig(hn *HarnessNode,
	req *signrpc.MuSig2CombineSigRequest) *signrpc.MuSig2CombineSigResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Signer.MuSig2CombineSig(ctxt, req)
	require.NoError(h, err, "failed to combine sig")

	return resp
}

// MuSig2Cleanup makes a RPC call to the node's SignerClient and asserts.
func (h *HarnessTest) MuSig2Cleanup(hn *HarnessNode,
	req *signrpc.MuSig2CleanupRequest) *signrpc.MuSig2CleanupResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Signer.MuSig2Cleanup(ctxt, req)
	require.NoError(h, err, "failed to cleanup")

	return resp
}

// =====================
// ChainClient related RPCs.
// =====================

type ConfNtfnClient chainrpc.ChainNotifier_RegisterConfirmationsNtfnClient

// RegisterConfirmationsNtfn creates a notification client to watch a given
// transaction being confirmed.
func (h *HarnessTest) RegisterConfirmationsNtfn(hn *HarnessNode,
	req *chainrpc.ConfRequest) ConfNtfnClient {

	// RegisterConfirmationsNtfn needs to have the context alive for the
	// entire test case as the returned client will be used for send and
	// receive events stream. Thus we use runCtx here instead of a timeout
	// context.
	client, err := hn.rpc.ChainClient.RegisterConfirmationsNtfn(
		h.runCtx, req,
	)
	require.NoError(h, err, "unable to register conf client")

	return client
}

type SpendClient chainrpc.ChainNotifier_RegisterSpendNtfnClient

// RegisterSpendNtfn creates a notification client to watch a given
// transaction being spent.
func (h *HarnessTest) RegisterSpendNtfn(hn *HarnessNode,
	req *chainrpc.SpendRequest) SpendClient {

	// RegisterSpendNtfn needs to have the context alive for the entire
	// test case as the returned client will be used for send and receive
	// events stream. Thus we use runCtx here instead of a timeout context.
	client, err := hn.rpc.ChainClient.RegisterSpendNtfn(
		h.runCtx, req,
	)
	require.NoError(h, err, "unable to register spend client")

	return client
}

// =====================
// PeerClient related RPCs.
// =====================

type (
	AnnReq  *peersrpc.NodeAnnouncementUpdateRequest
	AnnResp *peersrpc.NodeAnnouncementUpdateResponse
)

// UpdateNodeAnnouncement makes an UpdateNodeAnnouncement RPC call the the
// peersrpc client and asserts.
func (h *HarnessTest) UpdateNodeAnnouncement(hn *HarnessNode,
	req AnnReq) AnnResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Peer.UpdateNodeAnnouncement(ctxt, req)
	require.NoError(h, err, "failed to update announcement")

	return resp
}

// UpdateNodeAnnouncementErr makes an UpdateNodeAnnouncement RPC call the the
// peersrpc client and asserts an error is returned.
func (h *HarnessTest) UpdateNodeAnnouncementErr(hn *HarnessNode, req AnnReq) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := hn.rpc.Peer.UpdateNodeAnnouncement(ctxt, req)
	require.Error(h, err, "expect an error from update announcement")
}

package lntest

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestCase defines a test case that's been used in the integration test.
type TestCase struct {
	// Name specifies the test name.
	Name string

	// TestFunc is the test case wrapped in a function.
	TestFunc func(t *HarnessTest)
}

// HarnessTest wraps a regular testing.T providing enhanced error detection
// and propagation. All error will be augmented with a full stack-trace in
// order to aid in debugging. Additionally, any panics caused by active
// test cases will also be handled and represented as fatals.
type HarnessTest struct {
	*testing.T

	// net is a reference to the current network harness.
	net *NetworkHarness

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context
	cancel context.CancelFunc
}

// NewHarnessTest creates a new instance of a harnessTest from a regular
// testing.T instance.
func NewHarnessTest(t *testing.T, net *NetworkHarness) *HarnessTest {
	ctxt, cancel := context.WithCancel(context.Background())
	return &HarnessTest{t, net, ctxt, cancel}
}

// RunTestCase executes a harness test case. Any errors or panics will be
// represented as fatal.
func (h *HarnessTest) RunTestCase(testCase *TestCase) {
	defer func() {
		// Canel the run context.
		h.cancel()

		if err := recover(); err != nil {
			description := errors.Wrap(err, 2).ErrorStack()
			h.Fatalf("Failed: (%v) panic with: \n%v",
				testCase.Name, description)
		}
	}()

	testCase.TestFunc(h)
}

// FailNow is called by require at the very end if a test failed.
func (h *HarnessTest) FailNow() {
	h.T.FailNow()
}

// Subtest creates a child HarnessTest.
func (h *HarnessTest) Subtest(t *testing.T) *HarnessTest {
	return NewHarnessTest(t, h.net)
}

// NewNode creates a new node and asserts its creation. The node is guaranteed
// to have finished its initialization and all its subservers are started.
func (h *HarnessTest) NewNode(name string, extraArgs []string) *HarnessNode {
	node, err := h.net.newNode(
		name, extraArgs, false, nil, h.net.dbBackend, true,
	)
	require.NoErrorf(h, err, "unable to create new node for %s", name)

	return node
}

// Shutdown shuts down the given node and asserts that no errors occur.
func (h *HarnessTest) Shutdown(node *HarnessNode) {
	// The process may not be in a state to always shutdown immediately, so
	// we'll retry up to a hard limit to ensure we eventually shutdown.
	err := wait.NoError(func() error {
		return h.net.ShutdownNode(node)
	}, DefaultTimeout)
	require.NoErrorf(h, err, "unable to shutdown %v", node.Name())
}

// ConnectNodes creates a connection between the two nodes and asserts the
// connection is succeeded.
func (h *HarnessTest) ConnectNodes(a, b *HarnessNode) {
	ctx, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	bobInfo := h.GetInfo(b)

	req := &lnrpc.ConnectPeerRequest{
		Addr: &lnrpc.LightningAddress{
			Pubkey: bobInfo.IdentityPubkey,
			Host:   b.Cfg.P2PAddr(),
		},
	}
	err := h.net.connect(ctx, req, a)
	require.NoErrorf(h, err, "unable to connect %s to %s",
		a.Name(), b.Name())

	h.assertPeerConnected(a, b)
}

// GetInfo calls the GetInfo RPC on a given node and asserts there's no error.
func (h *HarnessTest) GetInfo(n *HarnessNode) *lnrpc.GetInfoResponse {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	info, err := n.rpc.LN.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	require.NoErrorf(h, err, "failed to GetInfo for node: %s", n.Cfg.Name)

	return info
}

// SetFeeEstimate sets a fee rate to be returned from fee estimator.
func (h *HarnessTest) SetFeeEstimate(fee chainfee.SatPerKWeight) {
	h.net.feeService.setFee(fee)
}

// EnsureConnected will try to connect to two nodes, returning no error if they
// are already connected. If the nodes were not connected previously, this will
// behave the same as ConnectNodes. If a pending connection request has already
// been made, the method will block until the two nodes appear in each other's
// peers list, or until the DefaultTimeout expires.
func (h *HarnessTest) EnsureConnected(a, b *HarnessNode) {
	tryConnect := func(a, b *HarnessNode) error {
		bInfo := h.GetInfo(b)

		req := &lnrpc.ConnectPeerRequest{
			Addr: &lnrpc.LightningAddress{
				Pubkey: bInfo.IdentityPubkey,
				Host:   b.Cfg.P2PAddr(),
			},
		}

		ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
		defer cancel()

		err := wait.NoError(func() error {
			err := h.net.connect(ctxt, req, a)
			switch {
			// Request was successful, wait for both to display the
			// connection.
			case err == nil:
				return nil

			// If the two are already connected, we return early
			// with no error.
			case strings.Contains(
				err.Error(), "already connected to peer",
			):
				return nil

			default:
				return err
			}
		}, DefaultTimeout)
		if err != nil {
			return fmt.Errorf("connection not succeeded within %d "+
				"seconds: %v", DefaultTimeout, err)
		}

		return nil
	}

	aErr := tryConnect(a, b)
	bErr := tryConnect(b, a)

	switch {
	// If both reported already being connected to each other, we can exit
	// early.
	case aErr == nil && bErr == nil:

	// Return any critical errors returned by either alice.
	case aErr != nil:
		require.Failf(h, "ensure connection between %s and %s failed "+
			"with error from %s: %v",
			a.Cfg.Name, b.Cfg.Name, a.Cfg.Name, aErr)

	// Return any critical errors returned by either bob.
	case bErr != nil:
		require.Failf(h, "ensure connection between %s and %s failed "+
			"with error from %s: %v",
			a.Cfg.Name, b.Cfg.Name, b.Cfg.Name, bErr)

	// Otherwise one or both requested a connection, so we wait for the
	// peers lists to reflect the connection.
	default:
	}

	h.assertPeerConnected(a, b)
	h.assertPeerConnected(b, a)
}

// SendCoins attempts to send amt satoshis from the internal mining node to the
// targeted lightning node using a P2WKH address. 6 blocks are mined after in
// order to confirm the transaction.
func (h *HarnessTest) SendCoins(amt btcutil.Amount, hn *HarnessNode) {
	err := h.net.SendCoinsOfType(
		amt, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH, true,
	)
	require.NoErrorf(h, err, "unable to send coins for %s", hn.Cfg.Name)
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

	// Wait until we are able to fund a channel successfully. This wait
	// prevents us from erroring out when trying to create a channel while
	// the node is starting up.
	var (
		chanOpenUpdate OpenChanClient
		err            error
	)

	err = wait.NoError(func() error {
		chanOpenUpdate, err = h.net.OpenChannel(a, b, p)
		return err
	}, ChannelOpenTimeout)

	require.NoError(h, err, "unable to open channel between %s and %s",
		a.Cfg.Name, b.Cfg.Name)

	return chanOpenUpdate
}

// OpenChannel attempts to open a channel with the specified parameters
// extended from Alice to Bob. Additionally, the following items are asserted,
//   * the funding transaction should be found within a block
//   * Alice and Bob have seen the channel edge update.
//   * Alice and Bob can report the status of the new channel.
func (h *HarnessTest) OpenChannel(alice, bob *HarnessNode,
	p OpenChannelParams) *lnrpc.ChannelPoint {

	chanOpenUpdate := h.OpenChannelStreamAndAssert(alice, bob, p)

	// Mine 6 blocks, then wait for Alice's node to notify us that the
	// channel has been opened. The funding transaction should be found
	// within the first newly mined block. We mine 6 blocks so that in the
	// case that the channel is public, it is announced to the network.
	block := h.MineBlocksAndAssertTx(6, 1)[0]

	fundingChanPoint, err := h.net.WaitForChannelOpen(chanOpenUpdate)
	require.NoError(h, err, "error while waiting for channel open")

	fundingTxID, err := lnrpc.GetChanPointFundingTxid(fundingChanPoint)
	require.NoError(h, err, "unable to get txid")

	h.AssertTxInBlock(block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.
	chanPoint := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: fundingChanPoint.OutputIndex,
	}

	// Check that both alice and bob have seen the channel from
	// their channel watch request.
	h.AssertChannelOpen(alice, fundingChanPoint)
	h.AssertChannelOpen(bob, fundingChanPoint)

	// Finally, check that the channel can be seen in their ListChannels.
	h.AssertChannelExists(alice, &chanPoint)
	h.AssertChannelExists(bob, &chanPoint)

	return fundingChanPoint
}

// AssertChannelOpen asserts that a given channel outpoint is seen by the
// passed node.
func (h *HarnessTest) AssertChannelOpen(n *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) {

	err := n.WaitForNetworkChannelOpen(chanPoint)
	require.NoErrorf(h, err, "%s didn't report channel", n.Cfg.Name)
}

// AssertChannelExists asserts that an active channel identified by the
// specified channel point exists from the point-of-view of the node.
func (h *HarnessTest) AssertChannelExists(hn *HarnessNode,
	cp *wire.OutPoint) { //nolint: interfacer

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		resp := h.ListChannels(hn)

		for _, channel := range resp.Channels {
			if channel.ChannelPoint == cp.String() {
				// Check whether our channel is active, failing
				// early if it is not.
				if channel.Active {
					return nil
				}
				return fmt.Errorf("channel point not active")
			}
		}

		return fmt.Errorf("outpoint %v not found", cp)
	}, DefaultTimeout)

	require.NoError(h, err, "timeout checking for channel point: %v", cp)
}

// AssertInvoiceState asserts that a given invoice has became the desired
// state before timeout.
func (h *HarnessTest) AssertInvoiceState(hn *HarnessNode, payHash lntypes.Hash,
	state lnrpc.Invoice_InvoiceState) {

	require.NoError(h, hn.waitForInvoiceState(payHash, state), "%s "+
		"failed to wait invoice:%v to be state: %v", hn.Name(),
		payHash, state)
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

// MineBlocksAndAssertTx mine 'num' of blocks and check that blocks are present
// in node blockchain. numTxs should be set to the number of transactions
// (excluding the coinbase) we expect to be included in the first mined block.
func (h *HarnessTest) MineBlocksAndAssertTx(num uint32,
	numTxs int) []*wire.MsgBlock {

	// If we expect transactions to be included in the blocks we'll mine,
	// we wait here until they are seen in the miner's mempool.
	var txids []*chainhash.Hash
	if numTxs > 0 {
		txids = h.AssertNumTxsInMempool(numTxs)
	}

	blocks := h.MineBlocks(num)

	// Finally, assert that all the transactions were included in the first
	// block.
	for _, txid := range txids {
		h.AssertTxInBlock(blocks[0], txid)
	}

	return blocks
}

// MineBlocks mine 'num' of blocks and check that blocks are present in
// node blockchain.
func (h *HarnessTest) MineBlocks(num uint32) []*wire.MsgBlock {
	blocks := make([]*wire.MsgBlock, num)

	blockHashes, err := h.net.Miner.Client.Generate(num)
	require.NoError(h, err, "unable to generate blocks")

	for i, blockHash := range blockHashes {
		block, err := h.net.Miner.Client.GetBlock(blockHash)
		require.NoError(h, err, "unable to get block")

		blocks[i] = block
	}

	return blocks
}

// AssertTxInBlock asserts that a given txid can be found in the passed block.
func (h *HarnessTest) AssertTxInBlock(block *wire.MsgBlock,
	txid *chainhash.Hash) {

	blockTxes := make([]chainhash.Hash, 0)

	for _, tx := range block.Transactions {
		sha := tx.TxHash()
		blockTxes = append(blockTxes, sha)

		if bytes.Equal(txid[:], sha[:]) {
			return
		}
	}

	require.Failf(h, "tx was not included in block", "tx:%v, block has:%v",
		txid, blockTxes)
}

// AssertTxInMempool asserts a given transaction can be found in the mempool.
func (h *HarnessTest) AssertTxInMempool(op wire.OutPoint) *wire.MsgTx {
	var msgTx *wire.MsgTx

	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mempool, err := h.net.Miner.Client.GetRawMempool()
		require.NoError(h, err, "unable to get mempool")

		if len(mempool) == 0 {
			return fmt.Errorf("empty mempool")
		}

		for _, txid := range mempool {
			tx, err := h.net.Miner.Client.GetRawTransaction(txid)
			// We require the RPC call to be succeeded and won't
			// wait for it as it's an unexpected behavior.
			require.NoErrorf(h, err, "unable to fetch tx: %v", txid)

			msgTx = tx.MsgTx()
			for _, txIn := range msgTx.TxIn {
				if txIn.PreviousOutPoint == op {
					return nil
				}
			}
		}

		return fmt.Errorf("outpoint %v not found in mempool", op)
	}, MinerMempoolTimeout)

	require.NoError(h, err, "timeout checking mempool")

	return msgTx
}

// AssertNumTxsInMempool polls until finding the desired number of transactions
// in the provided miner's mempool. It will asserrt if this number is not met
// after the given timeout.
func (h *HarnessTest) AssertNumTxsInMempool(n int) []*chainhash.Hash {
	var (
		mempool []*chainhash.Hash
		err     error
	)

	err = wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		mempool, err = h.net.Miner.Client.GetRawMempool()
		require.NoError(h, err, "unable to get mempool")

		if len(mempool) == n {
			return nil
		}

		return fmt.Errorf("want %v, got %v in mempool: %v",
			n, len(mempool), mempool)
	}, MinerMempoolTimeout)
	require.NoError(h, err, "assert tx in mempool timeout")

	return mempool
}

// assertPeerConnected asserts that the given node b is connected to a.
func (h *HarnessTest) assertPeerConnected(a, b *HarnessNode) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	err := wait.NoError(func() error {
		resp, err := a.rpc.LN.ListPeers(ctxt, &lnrpc.ListPeersRequest{})
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		require.NoErrorf(h, err, "%s ListPeers failed with: %v",
			a.Name(), err)

		// If node B is seen in the ListPeers response from node A,
		// then we can return true as the connection has been fully
		// established.
		for _, peer := range resp.Peers {
			if peer.PubKey == b.PubKeyStr {
				return nil
			}
		}

		return fmt.Errorf("%s not found in %s's ListPeers",
			b.Name(), a.Name())
	}, DefaultTimeout)

	require.NoError(h, err, "unable to connect %s to %s, got error: "+
		"peers not connected within %v seconds",
		a.Name(), b.Name(), DefaultTimeout)
}

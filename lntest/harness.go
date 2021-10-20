package lntest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb/etcd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	// noFeeLimitMsat is used to specify we will put no requirements on fee
	// charged when choosing a route path.
	noFeeLimitMsat = math.MaxInt64
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

// Alice returns the pre-defined node Alice on the harness net.
func (h *HarnessTest) Alice() *HarnessNode {
	return h.net.Alice
}

// Bob returns the pre-defined node Bob on the harness net.
func (h *HarnessTest) Bob() *HarnessNode {
	return h.net.Bob
}

func (h *HarnessTest) Miner() *HarnessMiner {
	return h.net.Miner
}

// IsNeutrinoBackend returns a bool indicating whether the node is using a
// neutrino as its backend. This is useful when we want to skip certain tests
// which cannot be done with a neutrino backend.
func (h *HarnessTest) IsNeutrinoBackend() bool {
	return h.net.BackendCfg.Name() == NeutrinoBackendName
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

// NewNodeWithSeedEtcd starts a new node with seed that'll use an external
// etcd database as its (remote) channel and wallet DB. The passsed cluster
// flag indicates that we'd like the node to join the cluster leader election.
func (h *HarnessTest) NewNodeWithSeedEtcd(name string, etcdCfg *etcd.Config,
	password []byte, entropy []byte, statelessInit, cluster bool,
	leaderSessionTTL int) (*HarnessNode, []string, []byte) {

	// We don't want to use the embedded etcd instance.
	const dbBackend = BackendBbolt

	extraArgs := extraArgsEtcd(etcdCfg, name, cluster, leaderSessionTTL)
	node, seed, mac, err := h.net.newNodeWithSeed(
		name, extraArgs, password, entropy, statelessInit, dbBackend,
	)
	require.NoError(h, err, "failed to create new node with seed etcd")

	return node, seed, mac
}

// NewNodeWithSeedEtcd starts a new node with seed that'll use an external
// etcd database as its (remote) channel and wallet DB. The passsed cluster
// flag indicates that we'd like the node to join the cluster leader election.
// If the wait flag is false then we won't wait until RPC is available (this is
// useful when the node is not expected to become the leader right away).
func (h *HarnessTest) NewNodeEtcd(name string, etcdCfg *etcd.Config,
	password []byte, cluster, wait bool,
	leaderSessionTTL int) *HarnessNode {

	// We don't want to use the embedded etcd instance.
	const dbBackend = BackendBbolt

	extraArgs := extraArgsEtcd(etcdCfg, name, cluster, leaderSessionTTL)
	node, err := h.net.newNode(
		name, extraArgs, true, password, dbBackend, wait,
	)
	require.NoError(h, err, "failed to create new node with etcd")

	return node
}

// NewNodeWithSeed fully initializes a new HarnessNode after creating a fresh
// aezeed. The provided password is used as both the aezeed password and the
// wallet password. The generated mnemonic is returned along with the
// initialized harness node.
func (h *HarnessTest) NewNodeWithSeed(name string,
	extraArgs []string, password []byte,
	statelessInit bool) (*HarnessNode, []string, []byte) {

	node, seed, mac, err := h.net.newNodeWithSeed(
		name, extraArgs, password, nil, statelessInit, h.net.dbBackend,
	)
	require.NoError(h, err, "failed to create new node with seed")

	return node, seed, mac
}

// RestoreNodeWithSeed fully initializes a HarnessNode using a chosen mnemonic,
// password, recovery window, and optionally a set of static channel backups.
// After providing the initialization request to unlock the node, this method
// will finish initializing the LightningClient such that the HarnessNode can
// be used for regular rpc operations.
func (h *HarnessTest) RestoreNodeWithSeed(name string, extraArgs []string,
	password []byte, mnemonic []string, rootKey string,
	recoveryWindow int32, chanBackups *lnrpc.ChanBackupSnapshot,
	opts ...NodeOption) *HarnessNode {

	node, err := h.net.newNode(
		name, extraArgs, true, password, h.net.dbBackend, true, opts...,
	)
	require.NoError(h, err, "restore node failed to create new node")

	initReq := &lnrpc.InitWalletRequest{
		WalletPassword:     password,
		CipherSeedMnemonic: mnemonic,
		AezeedPassphrase:   password,
		ExtendedMasterKey:  rootKey,
		RecoveryWindow:     recoveryWindow,
		ChannelBackups:     chanBackups,
	}

	err = wait.NoError(func() error {
		_, err := node.Init(initReq)
		return err
	}, DefaultTimeout)
	require.NoError(h, err, "restore node failed to init node")

	// With the node started, we can now record its public key within the
	// global mapping.
	h.net.RegisterNode(node)

	return node
}

// GetBestBlock makes a RPC request to miner and asserts.
func (h *HarnessTest) GetBestBlock() (*chainhash.Hash, int32) {
	blockHash, height, err := h.net.Miner.Client.GetBestBlock()
	require.NoError(h, err, "failed to GetBestBlock")
	return blockHash, height
}

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

// DisconnectNodes disconnects the given two nodes and asserts the
// disconnection is succeeded. The request is made from node a and sent to node
// b.
func (h *HarnessTest) DisconnectNodes(a, b *HarnessNode) {
	err := h.net.DisconnectNodes(a, b)
	require.NoError(h, err, "failed to disconnect nodes")
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

// SetFeeEstimateWithConf sets a fee rate of a specified conf target to be
// returned from fee estimator.
func (h *HarnessTest) SetFeeEstimateWithConf(
	fee chainfee.SatPerKWeight, conf uint32) {

	h.net.feeService.setFeeWithConf(fee, conf)
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

// SendCoinsUnconfirmed sends coins from the internal mining node to the target
// lightning node using a P2WPKH address. No blocks are mined after, so the
// transaction remains unconfirmed.
func (h *HarnessTest) SendCoinsUnconfirmed(amt btcutil.Amount,
	hn *HarnessNode) {

	err := h.net.SendCoinsOfType(
		amt, hn, lnrpc.AddressType_WITNESS_PUBKEY_HASH, false,
	)
	require.NoErrorf(h, err, "unable to send unconfirmed coins for %s",
		hn.Cfg.Name)
}

// SendCoinsNP2WKH attempts to send amt satoshis from the internal mining node
// to the targeted lightning node using a NP2WKH address.
func (h *HarnessTest) SendCoinsNP2WKH(amt btcutil.Amount, target *HarnessNode) {
	err := h.net.SendCoinsOfType(
		amt, target, lnrpc.AddressType_NESTED_PUBKEY_HASH, true,
	)
	require.NoErrorf(h, err, "unable to send NP2WKH coins for %s",
		target.Cfg.Name)
}

// SendCoinsP2TR attempts to send amt satoshis from the internal mining node to
// the targeted lightning node using a P2TR address.
func (h *HarnessTest) SendCoinsP2TR(amt btcutil.Amount, target *HarnessNode) {
	err := h.net.SendCoinsOfType(
		amt, target, lnrpc.AddressType_TAPROOT_PUBKEY, true,
	)
	require.NoErrorf(h, err, "unable to send P2TR coins for %s",
		target.Cfg.Name)
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

// WaitForChannelOpen takes a open channel client and consumes the stream to
// assert the channel is open.
func (h *HarnessTest) WaitForChannelOpen(
	client OpenChanClient) *lnrpc.ChannelPoint {

	fundingChanPoint, err := h.net.WaitForChannelOpen(client)
	require.NoError(h, err, "error while waiting for channel open")

	return fundingChanPoint
}

// OpenPendingChannel opens a channel between the two nodes and asserts it's
// pending.
func (h *HarnessTest) OpenPendingChannel(from, to *HarnessNode,
	chanAmt, pushAmt btcutil.Amount) *lnrpc.PendingUpdate {

	update, err := h.net.OpenPendingChannel(from, to, chanAmt, pushAmt)
	require.NoError(h, err, "unable to open channel")
	return update
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

	fundingChanPoint := h.WaitForChannelOpen(chanOpenUpdate)

	fundingTxID := h.GetChanPointFundingTxid(fundingChanPoint)
	h.AssertTxInBlock(block, fundingTxID)

	// The channel should be listed in the peer information returned by
	// both peers.

	// Check that both alice and bob have seen the channel from
	// their channel watch request.
	h.AssertChannelOpen(alice, fundingChanPoint)
	h.AssertChannelOpen(bob, fundingChanPoint)

	// Finally, check that the channel can be seen in their ListChannels.
	h.AssertChannelExists(alice, fundingChanPoint)
	h.AssertChannelExists(bob, fundingChanPoint)

	return fundingChanPoint
}

// OpenChannelAssertErr opens a channel between node a and b, asserts that the
// expected error is returned from the channel opening.
func (h *HarnessTest) OpenChannelAssertErr(a, b *HarnessNode,
	p OpenChannelParams, expectedErr error) {

	err := wait.NoError(func() error {
		_, err := h.net.OpenChannel(a, b, p)
		if err == nil {
			return fmt.Errorf("no error returned")
		}

		// Use string comparison here as we haven't codified all the
		// RPC errors yet.
		if strings.Contains(err.Error(), expectedErr.Error()) {
			return nil
		}

		return fmt.Errorf("unexpected error returned, want %v, got %v",
			expectedErr, err)
	}, ChannelOpenTimeout)

	require.NoError(h, err, "timeout checking error from open channel")
}

// OpenChannelPsbt attempts to open a channel between srcNode and destNode with
// the passed channel funding parameters. It will assert if the expected step
// of funding the PSBT is not received from the source node.
func (h *HarnessTest) OpenChannelPsbt(srcNode, destNode *HarnessNode,
	p OpenChannelParams) (OpenChanClient, []byte) {

	// Wait until srcNode and destNode have the latest chain synced.
	// Otherwise, we may run into a check within the funding manager that
	// prevents any funding workflows from being kicked off if the chain
	// isn't yet synced.
	err := srcNode.WaitForBlockchainSync()
	require.NoError(h, err, "unable to sync srcNode chain")
	err = destNode.WaitForBlockchainSync()
	require.NoError(h, err, "unable to sync destNode chain")

	// Send the request to open a channel to the source node now. This will
	// open a long-lived stream where we'll receive status updates about
	// the progress of the channel.
	// respStream := h.OpenChannelStreamAndAssert(srcNode, destNode, p)
	req := &lnrpc.OpenChannelRequest{
		NodePubkey:         destNode.PubKey[:],
		LocalFundingAmount: int64(p.Amt),
		PushSat:            int64(p.PushAmt),
		Private:            p.Private,
		SpendUnconfirmed:   p.SpendUnconfirmed,
		MinHtlcMsat:        int64(p.MinHtlc),
		FundingShim:        p.FundingShim,
	}
	respStream, err := srcNode.OpenChannel(h.runCtx, req)
	require.NoErrorf(h, err, "unable to open channel between "+
		"%s and %s", srcNode.Name(), destNode.Name())

	// Consume the "PSBT funding ready" update. This waits until the node
	// notifies us that the PSBT can now be funded.
	resp := h.ReceiveChanUpdate(respStream)
	upd, ok := resp.Update.(*lnrpc.OpenStatusUpdate_PsbtFund)
	require.Truef(h, ok, "expected PSBT funding update, got %v", resp)

	return respStream, upd.PsbtFund.Psbt
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
	cp *lnrpc.ChannelPoint) *lnrpc.Channel {

	var (
		channel *lnrpc.Channel
		err     error
	)

	err = wait.NoError(func() error {
		channel, err = h.findChannel(hn, cp)
		if err != nil {
			return err
		}

		// Check whether our channel is active, exit early if it is.
		if channel.Active {
			return nil
		}

		return fmt.Errorf("channel point not active")

	}, DefaultTimeout)

	require.NoError(h, err, "timeout checking for channel point: %v", cp)

	return channel
}

// AssertInvoiceState asserts that a given invoice has became the desired
// state before timeout and returns the invoice found.
func (h *HarnessTest) AssertInvoiceState(hn *HarnessNode, payHash lntypes.Hash,
	state lnrpc.Invoice_InvoiceState) *lnrpc.Invoice {

	invoice, err := hn.waitForInvoiceState(payHash, state)
	require.NoError(h, err, "%s failed to wait invoice:%v to be state: %v",
		hn.Name(), payHash, state)

	return invoice
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

// ConnectMiner connects the miner with the chain backend in the network.
func (h *HarnessTest) ConnectMiner() {
	err := h.net.BackendCfg.ConnectMiner()
	require.NoError(h, err, "failed to connect miner")
}

// DisconnectMiner removes the connection between the miner and the chain
// backend in the network.
func (h *HarnessTest) DisconnectMiner() {
	err := h.net.BackendCfg.DisconnectMiner()
	require.NoError(h, err, "failed to disconnect miner")
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
			// We require the RPC call to be succeeded and won't
			// wait for it as it's an unexpected behavior.
			tx := h.GetRawTransaction(txid)

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

// Random32Bytes generates a random 32 bytes which can be used as a pay hash,
// preimage, etc.
func (h *HarnessTest) Random32Bytes() []byte {
	randBuf := make([]byte, 32)

	_, err := rand.Read(randBuf)
	require.NoErrorf(h, err, "internal error, cannot generate random bytes")

	return randBuf
}

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

// AssertPaymentStatusFromStream takes a client stream and asserts the payment
// is in desired status before timeout. The payment found is returned once
// succeeded.
func (h *HarnessTest) AssertPaymentStatusFromStream(stream PaymentClient,
	status lnrpc.Payment_PaymentStatus) *lnrpc.Payment {

	var target *lnrpc.Payment
	err := wait.NoError(func() error {
		// Consume one message. This will block until the
		// message is received.
		payment, err := stream.Recv()

		// We will not wait if there's an error returned from
		// the stream.
		if err != nil {
			return fmt.Errorf("payment stream "+
				"got err: %w", err)
		}

		// Return if the desired payment state is reached.
		if payment.Status == status {
			target = payment
			return nil
		}

		// Return the err so that it can be used for debugging
		// when timeout is reached.
		return fmt.Errorf("payment status, got %v, want %v",
			payment.Status, status)

	}, DefaultTimeout)

	require.NoError(h, err, "timeout while waiting payment")

	return target
}

// assertNodeNumChannels polls the provided node's list channels rpc until it
// reaches the desired number of total channels.
func (h *HarnessTest) AssertNodeNumChannels(hn *HarnessNode, numChannels int) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		chanInfo := h.ListChannels(hn)

		// Return true if the query returned the expected number of
		// channels.
		num := len(chanInfo.Channels)
		if num != numChannels {
			return fmt.Errorf("expected %v channels, got %v",
				numChannels, num)
		}
		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "timeout checking node's num of channels")
}

// AssertNumActiveHtlcs asserts that a given number of HTLCs are seen in the
// node's channels.
func (h *HarnessTest) AssertNumActiveHtlcs(hn *HarnessNode, num int) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		nodeChans := h.ListChannels(hn)

		total := 0
		for _, channel := range nodeChans.Channels {
			total += len(channel.PendingHtlcs)
		}
		if total != num {
			return fmt.Errorf("expected %v HTLCs, got %v",
				num, total)
		}

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "assert active htlcs timed out")
}

// AssertActiveHtlcs makes sure all the passed nodes have the _exact_ HTLCs
// matching payHashes on _all_ their channels.
func (h *HarnessTest) AssertActiveHtlcs(hn *HarnessNode, payHashes ...[]byte) {
	checkPayHashesMatch := func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		nodeChans := h.ListChannels(hn)

		for _, ch := range nodeChans.Channels {
			// Record all payment hashes active for this channel.
			htlcHashes := make(map[string]struct{})

			for _, htlc := range ch.PendingHtlcs {
				h := hex.EncodeToString(htlc.HashLock)
				_, ok := htlcHashes[h]
				if ok {
					return fmt.Errorf("duplicate HashLock")
				}
				htlcHashes[h] = struct{}{}
			}

			// Channel should have exactly the payHashes active.
			if len(payHashes) != len(htlcHashes) {
				return fmt.Errorf("node [%s:%x] had %v "+
					"htlcs active, expected %v",
					hn.Name(), hn.PubKey[:],
					len(htlcHashes), len(payHashes))
			}

			// Make sure all the payHashes are active.
			for _, payHash := range payHashes {
				h := hex.EncodeToString(payHash)
				if _, ok := htlcHashes[h]; ok {
					continue
				}
				return fmt.Errorf("node [%s:%x] didn't have: "+
					"the payHash %v active", hn.Name(),
					hn.PubKey[:], h)
			}
		}

		return nil
	}

	err := wait.NoError(checkPayHashesMatch, DefaultTimeout)
	require.NoError(h, err)
}

// OutPointFromChannelPoint creates an outpoint from a given channel point.
func (h *HarnessTest) OutPointFromChannelPoint(
	cp *lnrpc.ChannelPoint) wire.OutPoint {

	txid := h.GetChanPointFundingTxid(cp)
	return wire.OutPoint{
		Hash:  *txid,
		Index: cp.OutputIndex,
	}
}

// GetNumTxsFromMempool polls until finding the desired number of transactions
// in the miner's mempool and returns the full transactions to the caller.
func (h *HarnessTest) GetNumTxsFromMempool(n int) []*wire.MsgTx {
	txids := h.AssertNumTxsInMempool(n)

	var txes []*wire.MsgTx
	for _, txid := range txids {
		tx := h.GetRawTransaction(txid)
		txes = append(txes, tx.MsgTx())
	}
	return txes
}

// GetRawTransaction makes a RPC call to the miner's GetRawTransaction and
// asserts.
func (h *HarnessTest) GetRawTransaction(txid *chainhash.Hash) *btcutil.Tx {
	tx, err := h.net.Miner.Client.GetRawTransaction(txid)
	require.NoErrorf(h, err, "failed to get raw tx: %v", txid)
	return tx
}

// assertAllTxesSpendFrom asserts that all txes in the list spend from the
// given tx.
func (h *HarnessTest) AssertAllTxesSpendFrom(txes []*wire.MsgTx,
	prevTxid chainhash.Hash) {

	for _, tx := range txes {
		if tx.TxIn[0].PreviousOutPoint.Hash != prevTxid {
			require.Failf(h, "", "tx %v did not spend from %v",
				tx.TxHash(), prevTxid)
		}
	}
}

// AssertTxSpendFrom asserts that a given tx is spent from a previous tx.
// tx.
func (h *HarnessTest) AssertTxSpendFrom(tx *wire.MsgTx,
	prevTxid chainhash.Hash) {

	if tx.TxIn[0].PreviousOutPoint.Hash != prevTxid {
		require.Failf(h, "", "tx %v did not spend from %v",
			tx.TxHash(), prevTxid)
	}
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

type PendingForceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel

// AssertChannelPendingForceClose asserts that the given channel found in the
// node is pending force close. Returns the PendingForceClose if found.
func (h *HarnessTest) AssertChannelPendingForceClose(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) PendingForceClose {

	var target PendingForceClose

	op := h.OutPointFromChannelPoint(chanPoint)

	err := wait.NoError(func() error {
		resp := h.GetPendingChannels(hn)

		forceCloseChans := resp.PendingForceClosingChannels
		for _, ch := range forceCloseChans {
			if ch.Channel.ChannelPoint == op.String() {
				target = ch
				return nil
			}
		}

		return fmt.Errorf("%v: channel %s not found in pending "+
			"force close", hn.Name(), chanPoint)
	}, DefaultTimeout)
	require.NoError(h, err, "assert pending force close timed out")

	return target
}

type WaitingCloseChannel *lnrpc.PendingChannelsResponse_WaitingCloseChannel

// AssertChannelWaitingClose asserts that the given channel found in the node
// is waiting close. Returns the WaitingCloseChannel if found.
func (h *HarnessTest) AssertChannelWaitingClose(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) WaitingCloseChannel {

	var target WaitingCloseChannel

	op := h.OutPointFromChannelPoint(chanPoint)

	err := wait.NoError(func() error {
		resp := h.GetPendingChannels(hn)

		for _, waitingClose := range resp.WaitingCloseChannels {
			if waitingClose.Channel.ChannelPoint == op.String() {
				target = waitingClose
				return nil
			}
		}

		return fmt.Errorf("%v: channel %s not found in waiting close",
			hn.Name(), chanPoint)
	}, DefaultTimeout)
	require.NoError(h, err, "assert channel waiting close timed out")

	return target
}

// AssertNumPendingCloseChannels checks that a PendingChannels response from
// the node reports the expected number of pending force channels and waiting
// close channels.
func (h *HarnessTest) AssertNumPendingCloseChannels(hn *HarnessNode,
	expWaitingClose, expPendingForceClose int) {

	err := wait.NoError(func() error {
		resp := h.GetPendingChannels(hn)

		n := len(resp.WaitingCloseChannels)
		if n != expWaitingClose {
			return fmt.Errorf("expected to find %d channels "+
				"waiting close, found %d", expWaitingClose, n)
		}

		n = len(resp.PendingForceClosingChannels)
		if n != expPendingForceClose {
			return fmt.Errorf("expected to find %d channel "+
				"pending force close, found %d",
				expPendingForceClose, n)
		}

		return nil
	}, DefaultTimeout)

	require.NoErrorf(h, err, "assert pending channels timeout")
}

// closeChannel attempts to close a channel identified by the passed channel
// point owned by the passed Lightning node. A fully blocking channel closure
// is attempted, therefore the passed context should be a child derived via
// timeout from a base parent. Additionally, once the channel has been detected
// as closed, an assertion checks that the transaction is found within a block.
// Finally, this assertion verifies that the node always sends out a disable
// update when closing the channel if the channel was previously enabled.
//
// NOTE: This method assumes that the provided funding point is confirmed
// on-chain AND that the edge exists in the node's channel graph. If the funding
// transactions was reorged out at some point, use closeReorgedChannelAndAssert.
func (h *HarnessTest) CloseChannel(node *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	return h.CloseChannelAndAssertType(node, fundingChanPoint, false, force)
}

func (h *HarnessTest) CloseChannelAndAssertType(node *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint,
	anchors, force bool) *chainhash.Hash {

	// Fetch the current channel policy. If the channel is currently
	// enabled, we will register for graph notifications before closing to
	// assert that the node sends out a disabling update as a result of the
	// channel being closed.
	curPolicy := h.getChannelPolicies(
		node, node.PubKeyStr, fundingChanPoint,
	)[0]
	expectDisable := !curPolicy.Disabled

	closeUpdates, _, err := h.net.CloseChannel(
		node, fundingChanPoint, force,
	)
	require.NoError(h, err, "unable to close channel")

	// If the channel policy was enabled prior to the closure, wait until we
	// received the disabled update.
	if expectDisable {
		curPolicy.Disabled = true
		h.AssertChannelPolicyUpdate(
			node, node.PubKeyStr,
			curPolicy, fundingChanPoint, false,
		)
	}

	return h.assertChannelClosed(
		node, fundingChanPoint, anchors, closeUpdates,
	)
}

// CloseChannelAssertErr closes the given channel and asserts an error
// returned.
func (h *HarnessTest) CloseChannelAssertErr(hn *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) {

	_, _, err := h.net.CloseChannel(hn, fundingChanPoint, force)
	require.Error(h, err, "expect close channel to return an error")
}

// closeReorgedChannelAndAssert attempts to close a channel identified by the
// passed channel point owned by the passed Lightning node. Once the channel
// has been detected as closed, an assertion checks that the transaction is
// found within a block.
//
// NOTE: This method does not verify that the node sends a disable update for
// the closed channel.
func (h *HarnessTest) CloseReorgedChannel(hn *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, force bool) *chainhash.Hash {

	closeUpdates, _, err := h.net.CloseChannel(hn, fundingChanPoint, force)
	require.NoError(h, err, "unable to close channel")

	return h.assertChannelClosed(
		hn, fundingChanPoint, false, closeUpdates,
	)
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
		client, err = hn.CloseChannel(h.runCtx, req)
		if err != nil {
			return fmt.Errorf("unable to close channel for node %s"+
				" with channel point %s", hn.Name(), chanPoint)
		}
		return nil
	}, ChannelCloseTimeout)

	require.NoError(h, err, "timeout closing channel")

	return client
}

// AssertChannelPolicyUpdate checks that the required policy update has
// happened on the given node.
func (h *HarnessTest) AssertChannelPolicyUpdate(node *HarnessNode,
	advertisingNode string, policy *lnrpc.RoutingPolicy,
	chanPoint *lnrpc.ChannelPoint, includeUnannounced bool) {

	require.NoError(
		h, node.WaitForChannelPolicyUpdate(
			advertisingNode, policy,
			chanPoint, includeUnannounced,
		), "error while waiting for channel update",
	)
}

// assertChannelClosed asserts that the channel is properly cleaned up after
// initiating a cooperative or local close.
func (h *HarnessTest) assertChannelClosed(hn *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint, anchors bool,
	closeUpdates lnrpc.Lightning_CloseChannelClient) *chainhash.Hash {

	txid := h.GetChanPointFundingTxid(fundingChanPoint)
	chanPointStr := fmt.Sprintf("%v:%v", txid, fundingChanPoint.OutputIndex)

	// If the channel appears in list channels, ensure that its state
	// contains ChanStatusCoopBroadcasted.
	listChansResp := h.ListChannels(hn)

	for _, channel := range listChansResp.Channels {
		// Skip other channels.
		if channel.ChannelPoint != chanPointStr {
			continue
		}

		// Assert that the channel is in coop broadcasted.
		require.Contains(
			h, channel.ChanStatusFlags,
			channeldb.ChanStatusCoopBroadcasted.String(),
			"channel not coop broadcasted",
		)
	}

	// At this point, the channel should now be marked as being in the
	// state of "waiting close".
	pendingChanResp := h.GetPendingChannels(hn)

	var found bool
	for _, pendingClose := range pendingChanResp.WaitingCloseChannels {
		if pendingClose.Channel.ChannelPoint == chanPointStr {
			found = true
			break
		}
	}
	require.True(h, found, "channel not marked as waiting close")

	// We'll now, generate a single block, wait for the final close status
	// update, then ensure that the closing transaction was included in the
	// block. If there are anchors, we also expect an anchor sweep.
	expectedTxes := 1
	if anchors {
		expectedTxes = 2
	}

	block := h.MineBlocksAndAssertTx(1, expectedTxes)[0]

	closingTxid, err := h.net.WaitForChannelClose(closeUpdates)
	require.NoError(h, err, "error while waiting for channel close")

	h.AssertTxInBlock(block, closingTxid)

	// Finally, the transaction should no longer be in the waiting close
	// state as we've just mined a block that should include the closing
	// transaction.
	err = wait.NoError(func() error {
		pendingChanResp := h.GetPendingChannels(hn)

		for _, pending := range pendingChanResp.WaitingCloseChannels {
			if pending.Channel.ChannelPoint == chanPointStr {
				return fmt.Errorf("found channel %s still in "+
					"waiting closing", chanPointStr)
			}
		}

		return nil
	}, DefaultTimeout)
	require.NoError(
		h, err, "closing transaction not marked as fully closed",
	)

	return closingTxid
}

// getChannelPolicies queries the channel graph and retrieves the current edge
// policies for the provided channel points.
func (h *HarnessTest) getChannelPolicies(hn *HarnessNode,
	advertisingNode string,
	chanPoints ...*lnrpc.ChannelPoint) []*lnrpc.RoutingPolicy {

	chanGraph := h.DescribeGraph(hn, true)

	var policies []*lnrpc.RoutingPolicy
	err := wait.NoError(func() error {
	out:
		for _, chanPoint := range chanPoints {
			for _, e := range chanGraph.Edges {
				if e.ChanPoint != txStr(chanPoint) {
					continue
				}

				if e.Node1Pub == advertisingNode {
					policies = append(policies,
						e.Node1Policy)
				} else {
					policies = append(policies,
						e.Node2Policy)
				}

				continue out
			}

			// If we've iterated over all the known edges and we
			// weren't able to find this specific one, then we'll
			// fail.
			return fmt.Errorf("did not find edge %v",
				txStr(chanPoint))
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err)

	return policies
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

// assertPeerConnected asserts that the given node b is connected to a.
func (h *HarnessTest) assertPeerConnected(a, b *HarnessNode) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		resp := h.ListPeers(a)

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

// AssertConnected asserts that two peers are connected.
func (h *HarnessTest) AssertConnected(a, b *HarnessNode) {
	h.assertPeerConnected(a, b)
	h.assertPeerConnected(b, a)
}

// assertPeerNotConnected asserts that the given node b is not connected to a.
func (h *HarnessTest) assertPeerNotConnected(a, b *HarnessNode) {
	err := wait.NoError(func() error {
		// We require the RPC call to be succeeded and won't wait for
		// it as it's an unexpected behavior.
		resp := h.ListPeers(a)

		// If node B is seen in the ListPeers response from node A,
		// then we return false as the connection has been fully
		// established.
		for _, peer := range resp.Peers {
			if peer.PubKey == b.PubKeyStr {
				return fmt.Errorf("peers %s and %s still "+
					"connected", a.Name(), b.Name())
			}
		}
		return nil

	}, DefaultTimeout)

	require.NoError(h, err, "timeout checking peers not connected")
}

// AssertNotConnected asserts that two peers are not connected.
func (h *HarnessTest) AssertNotConnected(a, b *HarnessNode) {
	h.assertPeerNotConnected(a, b)
	h.assertPeerNotConnected(b, a)
}

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

// AddInvoice adds a invoice for the given node and asserts.
func (h *HarnessTest) AddInvoice(req *lnrpc.Invoice,
	hn *HarnessNode) *lnrpc.AddInvoiceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	invoice, err := hn.rpc.LN.AddInvoice(ctxt, req)
	require.NoError(h, err)

	return invoice
}

// SuspendNode suspends a running node and assert.
func (h *HarnessTest) SuspendNode(hn *HarnessNode) func() error {
	restartFunc, err := h.net.SuspendNode(hn)
	require.NoError(h, err)
	return restartFunc
}

// SettleInvoice settles a given invoice and asserts.
func (h *HarnessTest) SettleInvoice(hn *HarnessNode,
	req *invoicesrpc.SettleInvoiceMsg) *invoicesrpc.SettleInvoiceResp {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.Invoice.SettleInvoice(ctxt, req)
	require.NoError(h, err)

	return resp
}

// ListInvoices list the node's invoice using the request and asserts.
func (h *HarnessTest) ListInvoices(hn *HarnessNode,
	req *lnrpc.ListInvoiceRequest) *lnrpc.ListInvoiceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

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
	}
	resp, err := hn.rpc.LN.ListPayments(ctxt, req)
	require.NoError(h, err, "failed to list payments")

	return resp
}

// AssertPaymentStatus asserts that the given node list a payment with the
// given preimage has the expected status.
func (h *HarnessTest) AssertPaymentStatus(hn *HarnessNode,
	preimage lntypes.Preimage, status lnrpc.Payment_PaymentStatus) {

	paymentsResp := h.ListPayments(hn, true)

	payHash := preimage.Hash()
	var found bool
	for _, p := range paymentsResp.Payments {
		if p.PaymentHash != payHash.String() {
			continue
		}

		require.Equal(h, status, p.Status, "payment status not match")

		found = true

		switch status {

		// If this expected status is SUCCEEDED, we expect the final
		// preimage.
		case lnrpc.Payment_SUCCEEDED:
			require.Equal(h, preimage.String(), p.PaymentPreimage,
				"preimage not match")

			// Otherwise we expect an all-zero preimage.
		default:
			require.Equal(h, (lntypes.Preimage{}).String(),
				p.PaymentPreimage, "expected zero preimage")
		}

	}

	require.Truef(h, found, "payment with payment hash %v not found "+
		"in response", payHash)
}

// AssertHTLCStage asserts the HTLC is in the expected stage or timeout.
func (h *HarnessTest) AssertHTLCStage(hn *HarnessNode, stage uint32) {
	checkStage := func() error {
		pendingChanResp := h.GetPendingChannels(hn)
		if len(pendingChanResp.PendingForceClosingChannels) == 0 {
			return fmt.Errorf("zero pending force closing channels")
		}

		ch := pendingChanResp.PendingForceClosingChannels[0]
		if ch.LimboBalance == 0 {
			return fmt.Errorf("zero balance")
		}

		if len(ch.PendingHtlcs) != 1 {
			return fmt.Errorf("got %d pending htlcs, want 1",
				len(ch.PendingHtlcs))
		}
		if stage != ch.PendingHtlcs[0].Stage {
			return fmt.Errorf("got stage: %v, want stage: %v",
				ch.PendingHtlcs[0].Stage, stage)
		}

		return nil
	}

	require.NoErrorf(h, wait.NoError(checkStage, DefaultTimeout),
		"timeout waiting for htlc stage")
}

// NewMinerAddress creates a new address for the miner and asserts.
func (h *HarnessTest) NewMinerAddress() btcutil.Address {
	addr, err := h.net.Miner.NewAddress()
	require.NoError(h, err, "failed to create new miner address")
	return addr
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

// txStr returns the string representation of the channel's funding transaction.
func txStr(chanPoint *lnrpc.ChannelPoint) string {
	fundingTxID, err := lnrpc.GetChanPointFundingTxid(chanPoint)
	if err != nil {
		return ""
	}
	cp := wire.OutPoint{
		Hash:  *fundingTxID,
		Index: chanPoint.OutputIndex,
	}
	return cp.String()
}

// GetWalletBalance makes a RPC call to WalletBalance and asserts.
func (h *HarnessTest) GetWalletBalance(
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
func (h *HarnessTest) ListUnspent(hn *HarnessNode,
	max, min int32) *lnrpc.ListUnspentResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ListUnspentRequest{
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

// GetChannelBalance gets the channel balance and asserts.
func (h *HarnessTest) GetChannelBalance(
	hn *HarnessNode) *lnrpc.ChannelBalanceResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	req := &lnrpc.ChannelBalanceRequest{}
	resp, err := hn.rpc.LN.ChannelBalance(ctxt, req)

	require.NoError(h, err, "unable to get balance for node %s", hn.Name())
	return resp
}

// AssertChannelBalanceResp makes a ChannelBalance request and checks the
// returned response matches the expected.
func (h *HarnessTest) AssertChannelBalanceResp(hn *HarnessNode,
	expected *lnrpc.ChannelBalanceResponse) { // nolint:interfacer

	resp := h.GetChannelBalance(hn)
	require.True(h, proto.Equal(expected, resp), "balance is incorrect "+
		"got: %v, want: %v", resp, expected,
	)
}

// GetChannelCommitType retrieves the active channel commitment type for the
// given chan point.
func (h *HarnessTest) GetChannelCommitType(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) lnrpc.CommitmentType {

	channels := h.ListChannels(hn)
	for _, c := range channels.Channels {
		if c.ChannelPoint == txStr(chanPoint) {
			return c.CommitmentType
		}
	}

	require.Fail(h, "get channel commit type failed", "channel point %v "+
		"not found", chanPoint)

	return 0
}

// DeriveKey makes a RPC call the DeriveKey and asserts.
func (h *HarnessTest) DeriveKey(hn *HarnessNode,
	kl *signrpc.KeyLocator) *signrpc.KeyDescriptor {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	key, err := hn.rpc.WalletKit.DeriveKey(ctxt, kl)
	require.NoError(h, err, "failed to derive key for node %s", hn.Name())

	return key
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

// AssertNumOpenChannelsPending asserts that a pair of nodes have the expected
// number of pending channels between them.
func (h *HarnessTest) AssertNumOpenChannelsPending(alice, bob *HarnessNode,
	expected int) {

	err := wait.NoError(func() error {
		aliceChans := h.GetPendingChannels(alice)
		bobChans := h.GetPendingChannels(bob)

		aliceNumChans := len(aliceChans.PendingOpenChannels)
		bobNumChans := len(bobChans.PendingOpenChannels)

		aliceStateCorrect := aliceNumChans == expected
		if !aliceStateCorrect {
			return fmt.Errorf("number of pending channels for "+
				"alice incorrect. expected %v, got %v",
				expected, aliceNumChans)
		}

		bobStateCorrect := bobNumChans == expected
		if !bobStateCorrect {
			return fmt.Errorf("number of pending channels for bob "+
				"incorrect. expected %v, got %v", expected,
				bobNumChans)
		}

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "num of pending open channels not match")
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

// RestartNode restarts a given node and asserts.
func (h *HarnessTest) RestartNode(hn *HarnessNode, callback func() error,
	chanBackups ...*lnrpc.ChanBackupSnapshot) {

	err := h.net.RestartNode(hn, callback, chanBackups...)
	require.NoErrorf(h, err, "failed to restart node %s", hn.Name())
}

// AssertTxAtHeight gets all of the transactions that a node's wallet has a
// record of at the target height, and finds and returns the tx with the target
// txid, failing if it is not found.
func (h *HarnessTest) AssertTxAtHeight(hn *HarnessNode, height int32,
	txid string) *lnrpc.Transaction {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	txns, err := hn.rpc.LN.GetTransactions(
		ctxt, &lnrpc.GetTransactionsRequest{
			StartHeight: height,
			EndHeight:   height,
		},
	)
	require.NoError(h, err, "could not get transactions")

	for _, tx := range txns.Transactions {
		if tx.TxHash == txid {
			return tx
		}
	}

	require.Failf(h, "fail to find tx", "tx:%v not found at height:%v",
		txid, height)

	return nil
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

// AssertChannelPolicy asserts that the passed node's known channel policy for
// the passed chanPoint is consistent with the expected policy values.
func (h *HarnessTest) AssertChannelPolicy(hn *HarnessNode,
	advertisingNode string, expectedPolicy *lnrpc.RoutingPolicy,
	chanPoints ...*lnrpc.ChannelPoint) {

	policies := h.getChannelPolicies(hn, advertisingNode, chanPoints...)
	for _, policy := range policies {
		err := CheckChannelPolicy(policy, expectedPolicy)
		require.NoError(h, err, "check policy failed")
	}
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

// UpdateChannelPolicy makes a RPC call to UpdateChannelPolicy and asserts.
func (h *HarnessTest) UpdateChannelPolicy(hn *HarnessNode,
	req *lnrpc.PolicyUpdateRequest) *lnrpc.PolicyUpdateResponse {

	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := hn.rpc.LN.UpdateChannelPolicy(ctxt, req)
	require.NoError(h, err, "failed to update policy")

	return resp
}

// CleanupForceClose mines a force close commitment found in the mempool and
// the following sweep transaction from the force closing node.
func (h *HarnessTest) CleanupForceClose(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) {

	// Wait for the channel to be marked pending force close.
	h.WaitForChannelPendingForceClose(hn, chanPoint)

	// Mine enough blocks for the node to sweep its funds from the force
	// closed channel.
	//
	// The commit sweep resolver is able to broadcast the sweep tx up to
	// one block before the CSV elapses, so wait until defaulCSV-1.
	h.MineBlocks(DefaultCSV - 1)

	// The node should now sweep the funds, clean up by mining the sweeping
	// tx.
	h.MineBlocksAndAssertTx(1, 1)
}

// WaitForChannelPendingForceClose waits for the node to report that the
// channel is pending force close, and that the UTXO nursery is aware of it.
func (h *HarnessTest) WaitForChannelPendingForceClose(hn *HarnessNode,
	fundingChanPoint *lnrpc.ChannelPoint) {

	txid, err := lnrpc.GetChanPointFundingTxid(fundingChanPoint)
	require.NoError(h, err, "failed to get chan point from txid")

	op := wire.OutPoint{
		Hash:  *txid,
		Index: fundingChanPoint.OutputIndex,
	}

	err = wait.NoError(func() error {
		pendingChanResp := h.GetPendingChannels(hn)

		forceClose, err := FindForceClosedChannel(pendingChanResp, &op)
		if err != nil {
			return err
		}

		// We must wait until the UTXO nursery has received the channel
		// and is aware of its maturity height.
		if forceClose.MaturityHeight == 0 {
			return fmt.Errorf("channel had maturity height of 0")
		}

		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while finding force closed channels")
}

// AssertAmountPaid checks that the ListChannels command of the provided
// node list the total amount sent and received as expected for the
// provided channel.
func (h *HarnessTest) AssertAmountPaid(channelName string, hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint, amountSent, amountReceived int64) {

	checkAmountPaid := func() error {
		// Find the targeted channel.
		channel, err := h.findChannel(hn, chanPoint)
		if err != nil {
			return fmt.Errorf("assert amount failed: %v", err)
		}

		if channel.TotalSatoshisSent != amountSent {
			return fmt.Errorf("%v: incorrect amount"+
				" sent: %v != %v", channelName,
				channel.TotalSatoshisSent,
				amountSent)
		}
		if channel.TotalSatoshisReceived !=
			amountReceived {
			return fmt.Errorf("%v: incorrect amount"+
				" received: %v != %v",
				channelName,
				channel.TotalSatoshisReceived,
				amountReceived)
		}

		return nil
	}

	// As far as HTLC inclusion in commitment transaction might be
	// postponed we will try to check the balance couple of times,
	// and then if after some period of time we receive wrong
	// balance return the error.
	err := wait.NoError(checkAmountPaid, DefaultTimeout)
	require.NoError(h, err, "timeout while checking amount paid")
}

// WaitForNodeBlockHeight queries the node for its current block height until
// it reaches the passed height.
func (h *HarnessTest) WaitForNodeBlockHeight(hn *HarnessNode, height int32) {
	err := wait.NoError(func() error {
		info := h.GetInfo(hn)
		if int32(info.BlockHeight) != height {
			return fmt.Errorf("expected block height to "+
				"be %v, was %v", height, info.BlockHeight)
		}
		return nil
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while waiting for height")
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

// AssertNumEdges checks that an expected number of edges can be found in the
// node specified.
func (h *HarnessTest) AssertNumEdges(hn *HarnessNode,
	expected int, includeUnannounced bool) {

	err := wait.NoError(func() error {
		chanGraph := h.DescribeGraph(hn, includeUnannounced)

		if len(chanGraph.Edges) == expected {
			return nil
		}

		return fmt.Errorf("expected to find %d edge in the graph, "+
			"found %d", expected, len(chanGraph.Edges))
	}, DefaultTimeout)

	require.NoError(h, err, "timeout while checking for edges")
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

// AssertLastHTLCError checks that the last sent HTLC of the last payment sent
// by the given node failed with the expected failure code.
func (h *HarnessTest) AssertLastHTLCError(hn *HarnessNode,
	code lnrpc.Failure_FailureCode) {

	paymentsResp := h.ListPayments(hn, true)

	payments := paymentsResp.Payments
	require.NotZero(h, len(payments), "no payments found")

	payment := payments[len(payments)-1]
	htlcs := payment.Htlcs
	require.NotZero(h, len(htlcs), "no htlcs")

	htlc := htlcs[len(htlcs)-1]
	require.NotNil(h, htlc.Failure, "expected htlc failure")

	require.Equal(h, code, htlc.Failure.Code, "unexpected failure code")
}

// GetChanPointFundingTxid takes a channel point and converts it into a chain
// hash.
func (h *HarnessTest) GetChanPointFundingTxid(
	cp *lnrpc.ChannelPoint) *chainhash.Hash {

	txid, err := lnrpc.GetChanPointFundingTxid(cp)
	require.NoError(h, err, "unable to get txid")

	return txid
}

// findChannel tries to find a target channel in the node using the given
// channel point.
func (h *HarnessTest) findChannel(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) (*lnrpc.Channel, error) {

	// Get the funding point.
	fp := h.OutPointFromChannelPoint(chanPoint)
	channelInfo := h.ListChannels(hn)

	// Find the target channel first.
	for _, channel := range channelInfo.Channels {
		if channel.ChannelPoint == fp.String() {
			return channel, nil
		}
	}

	return nil, fmt.Errorf("channel not found")
}

// QueryChannelByChanPoint tries to find a channel matching the channel point
// and asserts. It returns the channel found.
func (h *HarnessTest) QueryChannelByChanPoint(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.Channel {

	channel, err := h.findChannel(hn, chanPoint)
	require.NoError(h, err, "failed to query channel")
	return channel
}

// CreatePayReqs is a helper method that will create a slice of payment
// requests for the given node.
func (h *HarnessTest) CreatePayReqs(hn *HarnessNode, paymentAmt btcutil.Amount,
	numInvoices int) ([]string, [][]byte, []*lnrpc.Invoice) {

	payReqs := make([]string, numInvoices)
	rHashes := make([][]byte, numInvoices)
	invoices := make([]*lnrpc.Invoice, numInvoices)
	for i := 0; i < numInvoices; i++ {
		preimage := h.Random32Bytes()

		invoice := &lnrpc.Invoice{
			Memo:      "testing",
			RPreimage: preimage,
			Value:     int64(paymentAmt),
		}
		resp := h.AddInvoice(invoice, hn)

		// Set the payment address in the invoice so the caller can
		// properly use it.
		invoice.PaymentAddr = resp.PaymentAddr

		payReqs[i] = resp.PaymentRequest
		rHashes[i] = resp.RHash
		invoices[i] = invoice
	}

	return payReqs, rHashes, invoices
}

// CompletePaymentRequests sends payments from a lightning node to complete all
// payment requests. If the awaitResponse parameter is true, this function does
// not return until all payments successfully complete without errors.
func (h *HarnessTest) CompletePaymentRequests(hn *HarnessNode,
	paymentRequests []string, awaitResponse bool) {

	// We start by getting the current state of the client's channels. This
	// is needed to ensure the payments actually have been committed before
	// we return.
	listResp := h.ListChannels(hn)

	// send sends a payment and asserts if it doesn't succeeded.
	send := func(payReq string) {
		payStream := h.SendPayment(
			hn,
			&routerrpc.SendPaymentRequest{
				PaymentRequest: payReq,
				TimeoutSeconds: 60,
				FeeLimitMsat:   noFeeLimitMsat,
			},
		)

		// If we are not waiting for response, exit early.
		if !awaitResponse {
			return
		}

		h.AssertPaymentStatusFromStream(
			payStream, lnrpc.Payment_SUCCEEDED,
		)
	}

	// Launch all payments sequentially.
	for _, payReq := range paymentRequests {
		payReqCopy := payReq
		send(payReqCopy)
	}

	// We are not waiting for feedback in the form of a response, but we
	// should still wait long enough for the server to receive and handle
	// the send before cancelling the request. We wait for the number of
	// updates to one of our channels has increased before we return.
	err := wait.NoError(func() error {
		newListResp := h.ListChannels(hn)

		// If the number of open channels is now lower than before
		// attempting the payments, it means one of the payments
		// triggered a force closure (for example, due to an incorrect
		// preimage). Return early since it's clear the payment was
		// attempted.
		if len(newListResp.Channels) < len(listResp.Channels) {
			return nil
		}

		for _, c1 := range listResp.Channels {
			for _, c2 := range newListResp.Channels {
				if c1.ChannelPoint != c2.ChannelPoint {
					continue
				}

				// If this channel has an increased numbr of
				// updates, we assume the payments are
				// committed, and we can return.
				if c2.NumUpdates > c1.NumUpdates {
					return nil
				}
			}
		}

		return fmt.Errorf("channel not updated after sending payments")
	}, DefaultTimeout)
	require.NoError(h, err, "timeout while checking for channel updates")
}

// AssertLocalBalance checks the local balance of the given channel is
// expected. The channel found using the specified channel point is returned.
func (h *HarnessTest) AssertLocalBalance(hn *HarnessNode,
	cp *lnrpc.ChannelPoint, balance int64) *lnrpc.Channel {

	var result *lnrpc.Channel

	// Get the funding point.
	err := wait.NoError(func() error {
		// Find the target channel first.
		target, err := h.findChannel(hn, cp)

		// Exit early if the channel is not found.
		if err != nil {
			return fmt.Errorf("check balance failed: %v", err)
		}

		result = target

		// Check local balance.
		if target.LocalBalance == balance {
			return nil
		}

		return fmt.Errorf("balance is incorrect, got %v, expected %v",
			target.LocalBalance, balance)

	}, DefaultTimeout)

	require.NoError(h, err, "timeout while chekcing for balance")

	return result
}

// BackupDb created a db backup for the specified node and asserts.
func (h *HarnessTest) BackupDb(hn *HarnessNode) {
	require.NoError(h, h.net.BackupDb(hn), "failed to copy db files")
}

// RestoreDb restores a db backup for the specified node and asserts.
func (h *HarnessTest) RestoreDb(hn *HarnessNode) {
	require.NoError(h, h.net.RestoreDb(hn), "failed to restore db")
}

// AssertNumUpdates checks the num of updates is expected from the given
// channel.
//
// NOTE: doesn't wait.
func (h *HarnessTest) AssertNumUpdates(hn *HarnessNode,
	num uint64, cp *lnrpc.ChannelPoint) {

	// Find the target channel first.
	target, err := h.findChannel(hn, cp)
	require.NoError(h, err, "unable to find channel")

	require.Equal(h, num, target.NumUpdates, "NumUpdates not matched")
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

// AssertNodeStarted waits until the node is fully started or asserts a timeout
// error.
func (h *HarnessTest) AssertNodeStarted(hn *HarnessNode) {
	require.NoError(h, hn.WaitUntilServerActive())
}

// AssertNumUTXOs waits for the given number of UTXOs to be available or fails
// if that isn't the case before the default timeout.
func (h *HarnessTest) AssertNumUTXOs(hn *HarnessNode, expectedUtxos int) {
	err := wait.NoError(func() error {
		resp := h.ListUnspent(hn, math.MaxInt32, 1)
		if len(resp.Utxos) != expectedUtxos {
			return fmt.Errorf("not enough UTXOs, got %d wanted %d",
				len(resp.Utxos), expectedUtxos)
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err, "timeout waiting for UTXOs")
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

// AssertZombieChannel asserts that a given channel found using the chanID is
// marked as zombie.
func (h *HarnessTest) AssertZombieChannel(hn *HarnessNode, chanID uint64) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	err := wait.NoError(func() error {
		_, err := hn.rpc.LN.GetChanInfo(
			ctxt, &lnrpc.ChanInfoRequest{ChanId: chanID},
		)
		if err == nil {
			return fmt.Errorf("expected error but got nil")
		}

		if !strings.Contains(err.Error(), "marked as zombie") {
			return fmt.Errorf("expected error to contain '%s' but "+
				"was '%v'", "marked as zombie", err)
		}

		return nil
	}, DefaultTimeout)
	require.NoError(h, err, "timeout while checking zombie channel")
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

// AssertNumChannelUpdates asserts that a given number of channel updates has
// been seen in the specified node.
func (h *HarnessTest) AssertNumChannelUpdates(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint, num int) {

	op := h.OutPointFromChannelPoint(chanPoint)
	err := hn.WaitForNetworkNumChanUpdates(op, num)
	require.NoError(h, err, "failed to assert num of channel updates")
}

// AssertNumNodeAnns asserts that a given number of node announcements has been
// seen in the specified node.
func (h *HarnessTest) AssertNumNodeAnns(hn *HarnessNode,
	pubkey string, num int) {

	// We will get the current number of channel updates first and add it
	// to our expected number of newly created channel updates.
	err := hn.WaitForNetworkNumNodeUpdates(pubkey, num)
	require.NoError(h, err, "failed to assert num of channel updates")
}

// AssertChannelClosedFromGraph asserts a given channel is closed by checking
// the graph topology subscription of the specified node. Returns the closed
// channel update if found.
func (h *HarnessTest) AssertChannelClosedFromGraph(hn *HarnessNode,
	chanPoint *lnrpc.ChannelPoint) *lnrpc.ClosedChannelUpdate {

	closedChan, err := hn.WaitForNetworkChannelClose(chanPoint)
	require.NoError(h, err, "failed to wait for channel close")

	return closedChan
}

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

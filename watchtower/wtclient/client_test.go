package wtclient_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtmock"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
	"github.com/stretchr/testify/require"
)

const (
	csvDelay uint32 = 144

	towerAddrStr  = "18.28.243.2:9911"
	towerAddr2Str = "19.29.244.3:9912"
)

var (
	revPrivBytes = []byte{
		0x8f, 0x4b, 0x51, 0x83, 0xa9, 0x34, 0xbd, 0x5f,
		0x74, 0x6c, 0x9d, 0x5c, 0xae, 0x88, 0x2d, 0x31,
		0x06, 0x90, 0xdd, 0x8c, 0x9b, 0x31, 0xbc, 0xd1,
		0x78, 0x91, 0x88, 0x2a, 0xf9, 0x74, 0xa0, 0xef,
	}

	toLocalPrivBytes = []byte{
		0xde, 0x17, 0xc1, 0x2f, 0xdc, 0x1b, 0xc0, 0xc6,
		0x59, 0x5d, 0xf9, 0xc1, 0x3e, 0x89, 0xbc, 0x6f,
		0x01, 0x85, 0x45, 0x76, 0x26, 0xce, 0x9c, 0x55,
		0x3b, 0xc9, 0xec, 0x3d, 0xd8, 0x8b, 0xac, 0xa8,
	}

	toRemotePrivBytes = []byte{
		0x28, 0x59, 0x6f, 0x36, 0xb8, 0x9f, 0x19, 0x5d,
		0xcb, 0x07, 0x48, 0x8a, 0xe5, 0x89, 0x71, 0x74,
		0x70, 0x4c, 0xff, 0x1e, 0x9c, 0x00, 0x93, 0xbe,
		0xe2, 0x2e, 0x68, 0x08, 0x4c, 0xb4, 0x0f, 0x4f,
	}

	// addr is the server's reward address given to watchtower clients.
	addr, _ = btcutil.DecodeAddress(
		"tb1pw8gzj8clt3v5lxykpgacpju5n8xteskt7gxhmudu6pa70nwfhe6s3unsyk",
		&chaincfg.TestNet3Params,
	)

	addrScript, _ = txscript.PayToAddrScript(addr)

	waitTime = 5 * time.Second
)

// randPrivKey generates a new secp keypair, and returns the public key.
func randPrivKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()

	sk, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate pubkey")

	return sk
}

type mockNet struct {
	mu            sync.RWMutex
	connCallbacks map[string]func(wtserver.Peer)
}

func newMockNet() *mockNet {
	return &mockNet{
		connCallbacks: make(map[string]func(peer wtserver.Peer)),
	}
}

func (m *mockNet) Dial(_, _ string, _ time.Duration) (net.Conn, error) {
	return nil, nil
}

func (m *mockNet) LookupHost(_ string) ([]string, error) {
	panic("not implemented")
}

func (m *mockNet) LookupSRV(_, _, _ string) (string, []*net.SRV, error) {
	panic("not implemented")
}

func (m *mockNet) ResolveTCPAddr(_, _ string) (*net.TCPAddr, error) {
	panic("not implemented")
}

func (m *mockNet) AuthDial(local keychain.SingleKeyECDH,
	netAddr *lnwire.NetAddress, _ tor.DialFunc) (wtserver.Peer, error) {

	localPk := local.PubKey()
	localAddr := &net.TCPAddr{
		IP:   net.IP{0x32, 0x31, 0x30, 0x29},
		Port: 36723,
	}

	localPeer, remotePeer := wtmock.NewMockConn(
		localPk, netAddr.IdentityKey, localAddr, netAddr.Address, 0,
	)

	m.mu.RLock()
	defer m.mu.RUnlock()
	cb, ok := m.connCallbacks[netAddr.String()]
	if !ok {
		return nil, fmt.Errorf("no callback registered for this peer")
	}

	cb(remotePeer)

	return localPeer, nil
}

func (m *mockNet) registerConnCallback(netAddr *lnwire.NetAddress,
	cb func(wtserver.Peer)) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.connCallbacks[netAddr.String()] = cb
}

func (m *mockNet) removeConnCallback(netAddr *lnwire.NetAddress) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.connCallbacks, netAddr.String())
}

type mockChannel struct {
	mu            sync.Mutex
	commitHeight  uint64
	retributions  map[uint64]*lnwallet.BreachRetribution
	localBalance  lnwire.MilliSatoshi
	remoteBalance lnwire.MilliSatoshi

	revSK     *btcec.PrivateKey
	revPK     *btcec.PublicKey
	revKeyLoc keychain.KeyLocator

	toRemoteSK     *btcec.PrivateKey
	toRemotePK     *btcec.PublicKey
	toRemoteKeyLoc keychain.KeyLocator

	toLocalPK *btcec.PublicKey // only need to generate to-local script

	dustLimit lnwire.MilliSatoshi
	csvDelay  uint32
}

func newMockChannel(t *testing.T, signer *wtmock.MockSigner,
	localAmt, remoteAmt lnwire.MilliSatoshi) *mockChannel {

	// Generate the revocation, to-local, and to-remote keypairs.
	revSK := randPrivKey(t)
	revPK := revSK.PubKey()

	toLocalSK := randPrivKey(t)
	toLocalPK := toLocalSK.PubKey()

	toRemoteSK := randPrivKey(t)
	toRemotePK := toRemoteSK.PubKey()

	// Register the revocation secret key and the to-remote secret key with
	// the signer. We will not need to sign with the to-local key, as this
	// is to be known only by the counterparty.
	revKeyLoc := signer.AddPrivKey(revSK)
	toRemoteKeyLoc := signer.AddPrivKey(toRemoteSK)

	c := &mockChannel{
		retributions:   make(map[uint64]*lnwallet.BreachRetribution),
		localBalance:   localAmt,
		remoteBalance:  remoteAmt,
		revSK:          revSK,
		revPK:          revPK,
		revKeyLoc:      revKeyLoc,
		toLocalPK:      toLocalPK,
		toRemoteSK:     toRemoteSK,
		toRemotePK:     toRemotePK,
		toRemoteKeyLoc: toRemoteKeyLoc,
		dustLimit:      546000,
		csvDelay:       144,
	}

	// Create the initial remote commitment with the initial balances.
	c.createRemoteCommitTx(t)

	return c
}

func (c *mockChannel) createRemoteCommitTx(t *testing.T) {
	t.Helper()

	// Construct the to-local witness script.
	toLocalScript, err := input.CommitScriptToSelf(
		c.csvDelay, c.toLocalPK, c.revPK,
	)
	require.NoError(t, err, "unable to create to-local script")

	// Compute the to-local witness script hash.
	toLocalScriptHash, err := input.WitnessScriptHash(toLocalScript)
	require.NoError(t, err, "unable to create to-local witness script hash")

	// Compute the to-remote witness script hash.
	toRemoteScriptHash, err := input.CommitScriptUnencumbered(c.toRemotePK)
	require.NoError(t, err, "unable to create to-remote script")

	// Construct the remote commitment txn, containing the to-local and
	// to-remote outputs. The balances are flipped since the transaction is
	// from the PoV of the remote party. We don't need any inputs for this
	// test. We increment the version with the commit height to ensure that
	// all commitment transactions are unique even if the same distribution
	// of funds is used more than once.
	commitTxn := &wire.MsgTx{
		Version: int32(c.commitHeight + 1),
	}

	var (
		toLocalSignDesc  *input.SignDescriptor
		toRemoteSignDesc *input.SignDescriptor
	)

	var outputIndex int
	if c.remoteBalance >= c.dustLimit {
		commitTxn.TxOut = append(commitTxn.TxOut, &wire.TxOut{
			Value:    int64(c.remoteBalance.ToSatoshis()),
			PkScript: toLocalScriptHash,
		})

		// Create the sign descriptor used to sign for the to-local
		// input.
		toLocalSignDesc = &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				KeyLocator: c.revKeyLoc,
				PubKey:     c.revPK,
			},
			WitnessScript: toLocalScript,
			Output:        commitTxn.TxOut[outputIndex],
			HashType:      txscript.SigHashAll,
		}
		outputIndex++
	}
	if c.localBalance >= c.dustLimit {
		commitTxn.TxOut = append(commitTxn.TxOut, &wire.TxOut{
			Value:    int64(c.localBalance.ToSatoshis()),
			PkScript: toRemoteScriptHash,
		})

		// Create the sign descriptor used to sign for the to-remote
		// input.
		toRemoteSignDesc = &input.SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				KeyLocator: c.toRemoteKeyLoc,
				PubKey:     c.toRemotePK,
			},
			WitnessScript: toRemoteScriptHash,
			Output:        commitTxn.TxOut[outputIndex],
			HashType:      txscript.SigHashAll,
		}
		outputIndex++
	}

	txid := commitTxn.TxHash()

	var (
		toLocalOutPoint  wire.OutPoint
		toRemoteOutPoint wire.OutPoint
	)

	outputIndex = 0
	if toLocalSignDesc != nil {
		toLocalOutPoint = wire.OutPoint{
			Hash:  txid,
			Index: uint32(outputIndex),
		}
		outputIndex++
	}
	if toRemoteSignDesc != nil {
		toRemoteOutPoint = wire.OutPoint{
			Hash:  txid,
			Index: uint32(outputIndex),
		}
		outputIndex++
	}

	commitKeyRing := &lnwallet.CommitmentKeyRing{
		RevocationKey: c.revPK,
		ToRemoteKey:   c.toLocalPK,
		ToLocalKey:    c.toRemotePK,
	}

	retribution := &lnwallet.BreachRetribution{
		BreachTxHash:         commitTxn.TxHash(),
		RevokedStateNum:      c.commitHeight,
		KeyRing:              commitKeyRing,
		RemoteDelay:          c.csvDelay,
		LocalOutpoint:        toRemoteOutPoint,
		LocalOutputSignDesc:  toRemoteSignDesc,
		RemoteOutpoint:       toLocalOutPoint,
		RemoteOutputSignDesc: toLocalSignDesc,
	}

	c.retributions[c.commitHeight] = retribution
	c.commitHeight++
}

// advanceState creates the next channel state and retribution without altering
// channel balances.
func (c *mockChannel) advanceState(t *testing.T) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.createRemoteCommitTx(t)
}

// sendPayment creates the next channel state and retribution after transferring
// amt to the remote party.
func (c *mockChannel) sendPayment(t *testing.T, amt lnwire.MilliSatoshi) {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	require.GreaterOrEqualf(t, c.localBalance, amt, "insufficient funds "+
		"to send, need: %v, have: %v", amt, c.localBalance)

	c.localBalance -= amt
	c.remoteBalance += amt
	c.createRemoteCommitTx(t)
}

// receivePayment creates the next channel state and retribution after
// transferring amt to the local party.
func (c *mockChannel) receivePayment(t *testing.T, amt lnwire.MilliSatoshi) {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	require.GreaterOrEqualf(t, c.remoteBalance, amt, "insufficient funds "+
		"to recv, need: %v, have: %v", amt, c.remoteBalance)

	c.localBalance += amt
	c.remoteBalance -= amt
	c.createRemoteCommitTx(t)
}

// getState retrieves the channel's commitment and retribution at state i.
func (c *mockChannel) getState(
	i uint64) (chainhash.Hash, *lnwallet.BreachRetribution) {

	c.mu.Lock()
	defer c.mu.Unlock()

	retribution := c.retributions[i]

	return retribution.BreachTxHash, retribution
}

type testHarness struct {
	t          *testing.T
	cfg        harnessCfg
	signer     *wtmock.MockSigner
	capacity   lnwire.MilliSatoshi
	clientDB   *wtmock.ClientDB
	clientCfg  *wtclient.Config
	client     wtclient.Client
	serverAddr *lnwire.NetAddress
	serverDB   *wtmock.TowerDB
	serverCfg  *wtserver.Config
	server     *wtserver.Server
	net        *mockNet

	blockEvents *mockBlockSub
	height      int32

	channelEvents *mockSubscription
	sendUpdatesOn bool

	mu             sync.Mutex
	channels       map[lnwire.ChannelID]*mockChannel
	closedChannels map[lnwire.ChannelID]uint32

	quit chan struct{}
}

type harnessCfg struct {
	localBalance       lnwire.MilliSatoshi
	remoteBalance      lnwire.MilliSatoshi
	policy             wtpolicy.Policy
	noRegisterChan0    bool
	noAckCreateSession bool
	noServerStart      bool
}

func newHarness(t *testing.T, cfg harnessCfg) *testHarness {
	towerTCPAddr, err := net.ResolveTCPAddr("tcp", towerAddrStr)
	require.NoError(t, err, "Unable to resolve tower TCP addr")

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err, "Unable to generate tower private key")
	privKeyECDH := &keychain.PrivKeyECDH{PrivKey: privKey}

	towerPubKey := privKey.PubKey()

	towerAddr := &lnwire.NetAddress{
		IdentityKey: towerPubKey,
		Address:     towerTCPAddr,
	}

	const timeout = 200 * time.Millisecond
	serverDB := wtmock.NewTowerDB()

	serverCfg := &wtserver.Config{
		DB:           serverDB,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		NodeKeyECDH:  privKeyECDH,
		NewAddress: func() (btcutil.Address, error) {
			return addr, nil
		},
		NoAckCreateSession: cfg.noAckCreateSession,
	}

	signer := wtmock.NewMockSigner()
	mockNet := newMockNet()
	clientDB := wtmock.NewClientDB()

	h := &testHarness{
		t:              t,
		cfg:            cfg,
		signer:         signer,
		capacity:       cfg.localBalance + cfg.remoteBalance,
		clientDB:       clientDB,
		serverAddr:     towerAddr,
		serverDB:       serverDB,
		serverCfg:      serverCfg,
		net:            mockNet,
		blockEvents:    newMockBlockSub(t),
		channelEvents:  newMockSubscription(t),
		channels:       make(map[lnwire.ChannelID]*mockChannel),
		closedChannels: make(map[lnwire.ChannelID]uint32),
		quit:           make(chan struct{}),
	}
	t.Cleanup(func() {
		close(h.quit)
	})

	fetchChannel := func(id lnwire.ChannelID) (
		*channeldb.ChannelCloseSummary, error) {

		h.mu.Lock()
		defer h.mu.Unlock()

		height, ok := h.closedChannels[id]
		if !ok {
			return nil, channeldb.ErrClosedChannelNotFound
		}

		return &channeldb.ChannelCloseSummary{CloseHeight: height}, nil
	}

	h.clientCfg = &wtclient.Config{
		Signer: signer,
		SubscribeChannelEvents: func() (subscribe.Subscription, error) {
			return h.channelEvents, nil
		},
		FetchClosedChannel: fetchChannel,
		ChainNotifier:      h.blockEvents,
		Dial:               mockNet.Dial,
		DB:                 clientDB,
		AuthDial:           mockNet.AuthDial,
		SecretKeyRing:      wtmock.NewSecretKeyRing(),
		Policy:             cfg.policy,
		NewAddress: func() ([]byte, error) {
			return addrScript, nil
		},
		ReadTimeout:       timeout,
		WriteTimeout:      timeout,
		MinBackoff:        time.Millisecond,
		MaxBackoff:        time.Second,
		ForceQuitDelay:    10 * time.Second,
		SessionCloseRange: 1,
	}

	if !cfg.noServerStart {
		h.startServer()
		t.Cleanup(h.stopServer)
	}

	h.startClient()
	t.Cleanup(h.client.ForceQuit)

	h.makeChannel(0, h.cfg.localBalance, h.cfg.remoteBalance)
	if !cfg.noRegisterChan0 {
		h.registerChannel(0)
	}

	return h
}

// mine mimics the mining of new blocks by sending new block notifications.
func (h *testHarness) mine(numBlocks int) {
	h.t.Helper()

	for i := 0; i < numBlocks; i++ {
		h.height++
		h.blockEvents.sendNewBlock(h.height)
	}
}

// startServer creates a new server using the harness's current serverCfg and
// starts it after pointing the mockNet's callback to the new server.
func (h *testHarness) startServer() {
	h.t.Helper()

	var err error
	h.server, err = wtserver.New(h.serverCfg)
	require.NoError(h.t, err)

	h.net.registerConnCallback(h.serverAddr, h.server.InboundPeerConnected)

	require.NoError(h.t, h.server.Start())
}

// stopServer stops the main harness server.
func (h *testHarness) stopServer() {
	h.t.Helper()

	h.net.removeConnCallback(h.serverAddr)

	require.NoError(h.t, h.server.Stop())
}

// startClient creates a new server using the harness's current clientCf and
// starts it.
func (h *testHarness) startClient() {
	h.t.Helper()

	towerTCPAddr, err := net.ResolveTCPAddr("tcp", towerAddrStr)
	require.NoError(h.t, err)
	towerAddr := &lnwire.NetAddress{
		IdentityKey: h.serverCfg.NodeKeyECDH.PubKey(),
		Address:     towerTCPAddr,
	}

	h.client, err = wtclient.New(h.clientCfg)
	require.NoError(h.t, err)
	require.NoError(h.t, h.client.Start())
	require.NoError(h.t, h.client.AddTower(towerAddr))
}

// chanIDFromInt creates a unique channel id given a unique integral id.
func chanIDFromInt(id uint64) lnwire.ChannelID {
	var chanID lnwire.ChannelID
	binary.BigEndian.PutUint64(chanID[:8], id)
	return chanID
}

// makeChannel creates new channel with id, using the localAmt and remoteAmt as
// the starting balances. The channel will be available by using h.channel(id).
//
// NOTE: The method fails if channel for id already exists.
func (h *testHarness) makeChannel(id uint64,
	localAmt, remoteAmt lnwire.MilliSatoshi) {

	h.t.Helper()

	chanID := chanIDFromInt(id)
	c := newMockChannel(h.t, h.signer, localAmt, remoteAmt)

	c.mu.Lock()
	_, ok := h.channels[chanID]
	if !ok {
		h.channels[chanID] = c
	}
	c.mu.Unlock()

	require.Falsef(h.t, ok, "channel %d already created", id)
}

// channel retrieves the channel corresponding to id.
//
// NOTE: The method fails if a channel for id does not exist.
func (h *testHarness) channel(id uint64) *mockChannel {
	h.t.Helper()

	h.mu.Lock()
	c, ok := h.channels[chanIDFromInt(id)]
	h.mu.Unlock()
	require.Truef(h.t, ok, "unable to fetch channel %d", id)

	return c
}

// closeChannel marks a channel as closed.
//
// NOTE: The method fails if a channel for id does not exist.
func (h *testHarness) closeChannel(id uint64, height uint32) {
	h.t.Helper()

	h.mu.Lock()
	defer h.mu.Unlock()

	chanID := chanIDFromInt(id)

	_, ok := h.channels[chanID]
	require.Truef(h.t, ok, "unable to fetch channel %d", id)

	h.closedChannels[chanID] = height
	delete(h.channels, chanID)

	chanPointHash, err := chainhash.NewHash(chanID[:])
	require.NoError(h.t, err)

	if !h.sendUpdatesOn {
		return
	}

	h.channelEvents.sendUpdate(channelnotifier.ClosedChannelEvent{
		CloseSummary: &channeldb.ChannelCloseSummary{
			ChanPoint: wire.OutPoint{
				Hash:  *chanPointHash,
				Index: 0,
			},
			CloseHeight: height,
		},
	})
}

// registerChannel registers the channel identified by id with the client.
func (h *testHarness) registerChannel(id uint64) {
	h.t.Helper()

	chanID := chanIDFromInt(id)
	err := h.client.RegisterChannel(chanID)
	require.NoError(h.t, err)
}

// advanceChannelN calls advanceState on the channel identified by id the number
// of provided times and returns the breach hints corresponding to the new
// states.
func (h *testHarness) advanceChannelN(id uint64, n int) []blob.BreachHint {
	h.t.Helper()

	channel := h.channel(id)

	var hints []blob.BreachHint
	for i := uint64(0); i < uint64(n); i++ {
		channel.advanceState(h.t)
		breachTxID, _ := h.channel(id).getState(i)
		hints = append(hints, blob.NewBreachHintFromHash(&breachTxID))
	}

	return hints
}

// backupStates instructs the channel identified by id to send backups to the
// client for states in the range [to, from).
func (h *testHarness) backupStates(id, from, to uint64, expErr error) {
	h.t.Helper()

	for i := from; i < to; i++ {
		h.backupState(id, i, expErr)
	}
}

// backupStates instructs the channel identified by id to send a backup for
// state i.
func (h *testHarness) backupState(id, i uint64, expErr error) {
	h.t.Helper()

	_, retribution := h.channel(id).getState(i)

	chanID := chanIDFromInt(id)
	err := h.client.BackupState(
		&chanID, retribution, channeldb.SingleFunderBit,
	)
	require.ErrorIs(h.t, err, expErr)
}

// sendPayments instructs the channel identified by id to send amt to the remote
// party for each state in from-to times and returns the breach hints for states
// [from, to).
func (h *testHarness) sendPayments(id, from, to uint64,
	amt lnwire.MilliSatoshi) []blob.BreachHint {

	h.t.Helper()

	channel := h.channel(id)

	var hints []blob.BreachHint
	for i := from; i < to; i++ {
		h.channel(id).sendPayment(h.t, amt)
		breachTxID, _ := channel.getState(i)
		hints = append(hints, blob.NewBreachHintFromHash(&breachTxID))
	}

	return hints
}

// receivePayment instructs the channel identified by id to recv amt from the
// remote party for each state in from-to times and returns the breach hints for
// states [from, to).
func (h *testHarness) recvPayments(id, from, to uint64,
	amt lnwire.MilliSatoshi) []blob.BreachHint {

	h.t.Helper()

	channel := h.channel(id)

	var hints []blob.BreachHint
	for i := from; i < to; i++ {
		channel.receivePayment(h.t, amt)
		breachTxID, _ := channel.getState(i)
		hints = append(hints, blob.NewBreachHintFromHash(&breachTxID))
	}

	return hints
}

// waitServerUpdates blocks until the breach hints provided all appear in the
// watchtower's database or the timeout expires. This is used to test that the
// client in fact sends the updates to the server, even if it is offline.
func (h *testHarness) waitServerUpdates(hints []blob.BreachHint,
	timeout time.Duration) {

	h.t.Helper()

	// If no breach hints are provided, we will wait out the full timeout to
	// assert that no updates appear.
	wantUpdates := len(hints) > 0

	hintSet := make(map[blob.BreachHint]struct{})
	for _, hint := range hints {
		hintSet[hint] = struct{}{}
	}

	require.Lenf(h.t, hints, len(hintSet), "breach hints are not unique, "+
		"list-len: %d set-len: %d", len(hints), len(hintSet))

	// Closure to assert the server's matches are consistent with the hint
	// set.
	serverHasHints := func(matches []wtdb.Match) bool {
		if len(hintSet) != len(matches) {
			return false
		}

		for _, match := range matches {
			_, ok := hintSet[match.Hint]
			require.Truef(h.t, ok, "match %v in db is not in "+
				"hint set", match.Hint)
		}

		return true
	}

	failTimeout := time.After(timeout)
	for {
		select {
		case <-time.After(time.Second):
			matches, err := h.serverDB.QueryMatches(hints)
			require.NoError(h.t, err, "unable to query for hints")

			if wantUpdates && serverHasHints(matches) {
				return
			}

			if wantUpdates {
				h.t.Logf("Received %d/%d\n", len(matches),
					len(hints))
			}

		case <-failTimeout:
			matches, err := h.serverDB.QueryMatches(hints)
			require.NoError(h.t, err, "unable to query for hints")
			require.Truef(h.t, serverHasHints(matches), "breach "+
				"hints not received, only got %d/%d",
				len(matches), len(hints))
			return
		}
	}
}

// assertUpdatesForPolicy queries the server db for matches using the provided
// breach hints, then asserts that each match has a session with the expected
// policy.
func (h *testHarness) assertUpdatesForPolicy(hints []blob.BreachHint,
	expPolicy wtpolicy.Policy) {

	// Query for matches on the provided hints.
	matches, err := h.serverDB.QueryMatches(hints)
	require.NoError(h.t, err)

	// Assert that the number of matches is exactly the number of provided
	// hints.
	require.Lenf(h.t, matches, len(hints), "expected: %d matches, got: %d",
		len(hints), len(matches))

	// Assert that all of the matches correspond to a session with the
	// expected policy.
	for _, match := range matches {
		matchPolicy := match.SessionInfo.Policy
		require.Equal(h.t, expPolicy, matchPolicy)
	}
}

// addTower adds a tower found at `addr` to the client.
func (h *testHarness) addTower(addr *lnwire.NetAddress) {
	h.t.Helper()

	err := h.client.AddTower(addr)
	require.NoError(h.t, err)
}

// removeTower removes a tower from the client. If `addr` is specified, then the
// only said address is removed from the tower.
func (h *testHarness) removeTower(pubKey *btcec.PublicKey, addr net.Addr) {
	h.t.Helper()

	err := h.client.RemoveTower(pubKey, addr)
	require.NoError(h.t, err)
}

// relevantSessions returns a list of session IDs that have acked updates for
// the given channel ID.
func (h *testHarness) relevantSessions(chanID uint64) []wtdb.SessionID {
	h.t.Helper()

	var (
		sessionIDs []wtdb.SessionID
		cID        = chanIDFromInt(chanID)
	)

	collectSessions := wtdb.WithPerNumAckedUpdates(
		func(session *wtdb.ClientSession, id lnwire.ChannelID,
			_ uint16) {

			if !bytes.Equal(id[:], cID[:]) {
				return
			}

			sessionIDs = append(sessionIDs, session.ID)
		},
	)

	_, err := h.clientDB.ListClientSessions(nil, collectSessions)
	require.NoError(h.t, err)

	return sessionIDs
}

// isSessionClosable returns true if the given session has been marked as
// closable in the DB.
func (h *testHarness) isSessionClosable(id wtdb.SessionID) bool {
	h.t.Helper()

	cs, err := h.clientDB.ListClosableSessions()
	require.NoError(h.t, err)

	_, ok := cs[id]

	return ok
}

// mockSubscription is a mock subscription client that blocks on sends into the
// updates channel.
type mockSubscription struct {
	t       *testing.T
	updates chan interface{}

	// Embed the subscription interface in this mock so that we satisfy it.
	subscribe.Subscription
}

// newMockSubscription creates a mock subscription.
func newMockSubscription(t *testing.T) *mockSubscription {
	t.Helper()

	return &mockSubscription{
		t:       t,
		updates: make(chan interface{}),
	}
}

// sendUpdate sends an update into our updates channel, mocking the dispatch of
// an update from a subscription server. This call will fail the test if the
// update is not consumed within our timeout.
func (m *mockSubscription) sendUpdate(update interface{}) {
	select {
	case m.updates <- update:

	case <-time.After(waitTime):
		m.t.Fatalf("update: %v timeout", update)
	}
}

// Updates returns the updates channel for the mock.
func (m *mockSubscription) Updates() <-chan interface{} {
	return m.updates
}

// mockBlockSub mocks out the ChainNotifier.
type mockBlockSub struct {
	t      *testing.T
	events chan *chainntnfs.BlockEpoch

	chainntnfs.ChainNotifier
}

// newMockBlockSub creates a new mockBlockSub.
func newMockBlockSub(t *testing.T) *mockBlockSub {
	t.Helper()

	return &mockBlockSub{
		t:      t,
		events: make(chan *chainntnfs.BlockEpoch),
	}
}

// RegisterBlockEpochNtfn returns a channel that can be used to listen for new
// blocks.
func (m *mockBlockSub) RegisterBlockEpochNtfn(_ *chainntnfs.BlockEpoch) (
	*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: m.events,
	}, nil
}

// sendNewBlock will send a new block on the notification channel.
func (m *mockBlockSub) sendNewBlock(height int32) {
	select {
	case m.events <- &chainntnfs.BlockEpoch{Height: height}:

	case <-time.After(waitTime):
		m.t.Fatalf("timed out sending block: %d", height)
	}
}

const (
	localBalance  = lnwire.MilliSatoshi(100000000)
	remoteBalance = lnwire.MilliSatoshi(200000000)
)

var defaultTxPolicy = wtpolicy.TxPolicy{
	BlobType:     blob.TypeAltruistCommit,
	SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
}

type clientTest struct {
	name string
	cfg  harnessCfg
	fn   func(*testHarness)
}

var clientTests = []clientTest{
	{
		// Asserts that client will return the ErrUnregisteredChannel
		// error when trying to backup states for a channel that has not
		// been registered (and received it's pkscript).
		name: "backup unregistered channel",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 20000,
			},
			noRegisterChan0: true,
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 5
				chanID     = 0
			)

			// Advance the channel and backup the retributions. We
			// expect ErrUnregisteredChannel to be returned since
			// the channel was not registered during harness
			// creation.
			h.advanceChannelN(chanID, numUpdates)
			h.backupStates(
				chanID, 0, numUpdates,
				wtclient.ErrUnregisteredChannel,
			)
		},
	},
	{
		// Asserts that the client returns an ErrClientExiting when
		// trying to backup channels after the Stop method has been
		// called.
		name: "backup after stop",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 20000,
			},
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 5
				chanID     = 0
			)

			// Stop the client, subsequent backups should fail.
			h.client.Stop()

			// Advance the channel and try to back up the states. We
			// expect ErrClientExiting to be returned from
			// BackupState.
			h.advanceChannelN(chanID, numUpdates)
			h.backupStates(
				chanID, 0, numUpdates,
				wtclient.ErrClientExiting,
			)
		},
	},
	{
		// Asserts that the client will continue to back up all states
		// that have previously been enqueued before it finishes
		// exiting.
		name: "backup reliable flush",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 5
				chanID     = 0
			)

			// Generate numUpdates retributions and back them up to
			// the tower.
			hints := h.advanceChannelN(chanID, numUpdates)
			h.backupStates(chanID, 0, numUpdates, nil)

			// Stop the client in the background, to assert the
			// pipeline is always flushed before it exits.
			go h.client.Stop()

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, time.Second)
		},
	},
	{
		// Assert that the client will not send out backups for states
		// whose justice transactions are ineligible for backup, e.g.
		// creating dust outputs.
		name: "backup dust ineligible",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: 1000000, // high sweep fee creates dust
				},
				MaxUpdates: 20000,
			},
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 5
				chanID     = 0
			)

			// Create the retributions and queue them for backup.
			h.advanceChannelN(chanID, numUpdates)
			h.backupStates(chanID, 0, numUpdates, nil)

			// Ensure that no updates are received by the server,
			// since they should all be marked as ineligible.
			h.waitServerUpdates(nil, time.Second)
		},
	},
	{
		// Verifies that the client will properly retransmit a committed
		// state update to the watchtower after a restart if the update
		// was not acked while the client was active last.
		name: "committed update restart",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 20000,
			},
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 5
				chanID     = 0
			)

			hints := h.advanceChannelN(0, numUpdates)

			var numSent uint64

			// Add the first two states to the client's pipeline.
			h.backupStates(chanID, 0, 2, nil)
			numSent = 2

			// Wait for both to be reflected in the server's
			// database.
			h.waitServerUpdates(hints[:numSent], time.Second)

			// Now, restart the server and prevent it from acking
			// state updates.
			h.stopServer()
			h.serverCfg.NoAckUpdates = true
			h.startServer()

			// Send the next state update to the tower. Since the
			// tower isn't acking state updates, we expect this
			// update to be committed and sent by the session queue,
			// but it will never receive an ack.
			h.backupState(chanID, numSent, nil)
			numSent++

			// Force quit the client to abort the state updates it
			// has queued. The sleep ensures that the session queues
			// have enough time to commit the state updates before
			// the client is killed.
			time.Sleep(time.Second)
			h.client.ForceQuit()

			// Restart the server and allow it to ack the updates
			// after the client retransmits the unacked update.
			h.stopServer()
			h.serverCfg.NoAckUpdates = false
			h.startServer()

			// Restart the client and allow it to process the
			// committed update.
			h.startClient()

			// Wait for the committed update to be accepted by the
			// tower.
			h.waitServerUpdates(hints[:numSent], time.Second)

			// Finally, send the rest of the updates and wait for
			// the tower to receive the remaining states.
			h.backupStates(chanID, numSent, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, time.Second)

		},
	},
	{
		// Asserts that the client will continue to retry sending state
		// updates if it doesn't receive an ack from the server. The
		// client is expected to flush everything in its in-memory
		// pipeline once the server begins sending acks again.
		name: "no ack from server",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 100
				chanID     = 0
			)

			// Generate the retributions that will be backed up.
			hints := h.advanceChannelN(chanID, numUpdates)

			// Restart the server and prevent it from acking state
			// updates.
			h.stopServer()
			h.serverCfg.NoAckUpdates = true
			h.startServer()

			// Now, queue the retributions for backup.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Stop the client in the background, to assert the
			// pipeline is always flushed before it exits.
			go h.client.Stop()

			// Give the client time to saturate a large number of
			// session queues for which the server has not acked the
			// state updates that it has received.
			time.Sleep(time.Second)

			// Restart the server and allow it to ack the updates
			// after the client retransmits the unacked updates.
			h.stopServer()
			h.serverCfg.NoAckUpdates = false
			h.startServer()

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, waitTime)
		},
	},
	{
		// Asserts that the client is able to send state updates to the
		// tower for a full range of channel values, assuming the sweep
		// fee rates permit it. We expect all of these to be successful
		// since a sweep transactions spending only from one output is
		// less expensive than one that sweeps both.
		name: "send and recv",
		cfg: harnessCfg{
			localBalance:  100000001, // ensure (% amt != 0)
			remoteBalance: 200000001, // ensure (% amt != 0)
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 1000,
			},
		},
		fn: func(h *testHarness) {
			var (
				capacity   = h.cfg.localBalance + h.cfg.remoteBalance
				paymentAmt = lnwire.MilliSatoshi(2000000)
				numSends   = uint64(h.cfg.localBalance / paymentAmt)
				numRecvs   = uint64(capacity / paymentAmt)
				numUpdates = numSends + numRecvs // 200 updates
				chanID     = uint64(0)
			)

			// Send money to the remote party until all funds are
			// depleted.
			sendHints := h.sendPayments(chanID, 0, numSends, paymentAmt)

			// Now, sequentially receive the entire channel balance
			// from the remote party.
			recvHints := h.recvPayments(chanID, numSends, numUpdates, paymentAmt)

			// Collect the hints generated by both sending and
			// receiving.
			hints := append(sendHints, recvHints...)

			// Backup the channel's states the client.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, 3*time.Second)
		},
	},
	{
		// Asserts that the client is able to support multiple links.
		name: "multiple link backup",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 5
				numChans   = 10
			)

			// Initialize and register an additional 9 channels.
			for id := uint64(1); id < 10; id++ {
				h.makeChannel(
					id, h.cfg.localBalance,
					h.cfg.remoteBalance,
				)
				h.registerChannel(id)
			}

			// Generate the retributions for all 10 channels and
			// collect the breach hints.
			var hints []blob.BreachHint
			for id := uint64(0); id < 10; id++ {
				chanHints := h.advanceChannelN(id, numUpdates)
				hints = append(hints, chanHints...)
			}

			// Provided all retributions to the client from all
			// channels.
			for id := uint64(0); id < 10; id++ {
				h.backupStates(id, 0, numUpdates, nil)
			}

			// Test reliable flush under multi-client scenario.
			go h.client.Stop()

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, 10*time.Second)
		},
	},
	{
		name: "create session no ack",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
			noAckCreateSession: true,
		},
		fn: func(h *testHarness) {
			const (
				chanID     = 0
				numUpdates = 3
			)

			// Generate the retributions that will be backed up.
			hints := h.advanceChannelN(chanID, numUpdates)

			// Now, queue the retributions for backup.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Since the client is unable to create a session, the
			// server should have no updates.
			h.waitServerUpdates(nil, time.Second)

			// Force quit the client since it has queued backups.
			h.client.ForceQuit()

			// Restart the server and allow it to ack session
			// creation.
			h.stopServer()
			h.serverCfg.NoAckCreateSession = false
			h.startServer()

			// Restart the client with the same policy, which will
			// immediately try to overwrite the old session with an
			// identical one.
			h.startClient()

			// Now, queue the retributions for backup.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, waitTime)

			// Assert that the server has updates for the clients
			// most recent policy.
			h.assertUpdatesForPolicy(hints, h.clientCfg.Policy)
		},
	},
	{
		name: "create session no ack change policy",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
			noAckCreateSession: true,
		},
		fn: func(h *testHarness) {
			const (
				chanID     = 0
				numUpdates = 3
			)

			// Generate the retributions that will be backed up.
			hints := h.advanceChannelN(chanID, numUpdates)

			// Now, queue the retributions for backup.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Since the client is unable to create a session, the
			// server should have no updates.
			h.waitServerUpdates(nil, time.Second)

			// Force quit the client since it has queued backups.
			h.client.ForceQuit()

			// Restart the server and allow it to ack session
			// creation.
			h.stopServer()
			h.serverCfg.NoAckCreateSession = false
			h.startServer()

			// Restart the client with a new policy, which will
			// immediately try to overwrite the prior session with
			// the old policy.
			h.clientCfg.Policy.SweepFeeRate *= 2
			h.startClient()

			// Now, queue the retributions for backup.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, waitTime)

			// Assert that the server has updates for the clients
			// most recent policy.
			h.assertUpdatesForPolicy(hints, h.clientCfg.Policy)
		},
	},
	{
		// Asserts that the client will not request a new session if
		// already has an existing session with the same TxPolicy. This
		// permits the client to continue using policies that differ in
		// operational parameters, but don't manifest in different
		// justice transactions.
		name: "create session change policy same txpolicy",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 10,
			},
		},
		fn: func(h *testHarness) {
			const (
				chanID     = 0
				numUpdates = 6
			)

			// Generate the retributions that will be backed up.
			hints := h.advanceChannelN(chanID, numUpdates)

			// Now, queue the first half of the retributions.
			h.backupStates(chanID, 0, numUpdates/2, nil)

			// Wait for the server to collect the first half.
			h.waitServerUpdates(hints[:numUpdates/2], time.Second)

			// Stop the client, which should have no more backups.
			require.NoError(h.t, h.client.Stop())

			// Record the policy that the first half was stored
			// under. We'll expect the second half to also be stored
			// under the original policy, since we are only adjusting
			// the MaxUpdates. The client should detect that the
			// two policies have equivalent TxPolicies and continue
			// using the first.
			expPolicy := h.clientCfg.Policy

			// Restart the client with a new policy.
			h.clientCfg.Policy.MaxUpdates = 20
			h.startClient()

			// Now, queue the second half of the retributions.
			h.backupStates(chanID, numUpdates/2, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, waitTime)

			// Assert that the server has updates for the client's
			// original policy.
			h.assertUpdatesForPolicy(hints, expPolicy)
		},
	},
	{
		// Asserts that the client will deduplicate backups presented by
		// a channel both in memory and after a restart. The client
		// should only accept backups with a commit height greater than
		// any processed already processed for a given policy.
		name: "dedup backups",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 10
				chanID     = 0
			)

			// Generate the retributions that will be backed up.
			hints := h.advanceChannelN(chanID, numUpdates)

			// Queue the first half of the retributions twice, the
			// second batch should be entirely deduped by the
			// client's in-memory tracking.
			h.backupStates(chanID, 0, numUpdates/2, nil)
			h.backupStates(chanID, 0, numUpdates/2, nil)

			// Wait for the first half of the updates to be
			// populated in the server's database.
			h.waitServerUpdates(hints[:len(hints)/2], waitTime)

			// Restart the client, so we can ensure the deduping is
			// maintained across restarts.
			require.NoError(h.t, h.client.Stop())
			h.startClient()

			// Try to back up the full range of retributions. Only
			// the second half should actually be sent.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, waitTime)
		},
	},
	{
		// Asserts that the client can continue making backups to a
		// tower that's been re-added after it's been removed.
		name: "re-add removed tower",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const (
				chanID     = 0
				numUpdates = 4
			)

			// Create four channel updates and only back up the
			// first two.
			hints := h.advanceChannelN(chanID, numUpdates)
			h.backupStates(chanID, 0, numUpdates/2, nil)
			h.waitServerUpdates(hints[:numUpdates/2], waitTime)

			// Fully remove the tower, causing its existing sessions
			// to be marked inactive.
			h.removeTower(h.serverAddr.IdentityKey, nil)

			// Back up the remaining states. Since the tower has
			// been removed, it shouldn't receive any updates.
			h.backupStates(chanID, numUpdates/2, numUpdates, nil)
			h.waitServerUpdates(nil, time.Second)

			// Re-add the tower. We prevent the tower from acking
			// session creation to ensure the inactive sessions are
			// not used.
			h.stopServer()
			h.serverCfg.NoAckCreateSession = true
			h.startServer()
			h.addTower(h.serverAddr)
			h.waitServerUpdates(nil, time.Second)

			// Finally, allow the tower to ack session creation,
			// allowing the state updates to be sent through the new
			// session.
			h.stopServer()
			h.serverCfg.NoAckCreateSession = false
			h.startServer()
			h.waitServerUpdates(hints[numUpdates/2:], waitTime)
		},
	},
	{
		// Asserts that the client's force quite delay will properly
		// shutdown the client if it is unable to completely drain the
		// task pipeline.
		name: "force unclean shutdown",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const (
				chanID     = 0
				numUpdates = 6
				maxUpdates = 5
			)

			// Advance the channel to create all states.
			hints := h.advanceChannelN(chanID, numUpdates)

			// Back up 4 of the 5 states for the negotiated session.
			h.backupStates(chanID, 0, maxUpdates-1, nil)
			h.waitServerUpdates(hints[:maxUpdates-1], waitTime)

			// Now, restart the tower and prevent it from acking any
			// new sessions. We do this here as once the last slot
			// is exhausted the client will attempt to renegotiate.
			h.stopServer()
			h.serverCfg.NoAckCreateSession = true
			h.startServer()

			// Back up the remaining two states. Once the first is
			// processed, the session will be exhausted but the
			// client won't be able to regnegotiate a session for
			// the final state. We'll only wait for the first five
			// states to arrive at the tower.
			h.backupStates(chanID, maxUpdates-1, numUpdates, nil)
			h.waitServerUpdates(hints[:maxUpdates], waitTime)

			// Finally, stop the client which will continue to
			// attempt session negotiation since it has one more
			// state to process. After the force quite delay
			// expires, the client should force quite itself and
			// allow the test to complete.
			h.stopServer()
		},
	},
	{
		// Assert that if a client changes the address for a server and
		// then tries to back up updates then the client will switch to
		// the new address.
		name: "change address of existing session",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const (
				chanID     = 0
				numUpdates = 6
				maxUpdates = 5
			)

			// Advance the channel to create all states.
			hints := h.advanceChannelN(chanID, numUpdates)

			h.backupStates(chanID, 0, numUpdates/2, nil)

			// Wait for the first half of the updates to be
			// populated in the server's database.
			h.waitServerUpdates(hints[:len(hints)/2], waitTime)

			// Stop the server.
			h.stopServer()

			// Change the address of the server.
			towerTCPAddr, err := net.ResolveTCPAddr(
				"tcp", towerAddr2Str,
			)
			require.NoError(h.t, err)

			oldAddr := h.serverAddr.Address
			towerAddr := &lnwire.NetAddress{
				IdentityKey: h.serverAddr.IdentityKey,
				Address:     towerTCPAddr,
			}
			h.serverAddr = towerAddr

			// Add the new tower address to the client.
			err = h.client.AddTower(towerAddr)
			require.NoError(h.t, err)

			// Remove the old tower address from the client.
			err = h.client.RemoveTower(
				towerAddr.IdentityKey, oldAddr,
			)
			require.NoError(h.t, err)

			// Restart the server.
			h.startServer()

			// Now attempt to back up the rest of the updates.
			h.backupStates(chanID, numUpdates/2, maxUpdates, nil)

			// Assert that the server does receive the updates.
			h.waitServerUpdates(hints[:maxUpdates], waitTime)
		},
	},
	{
		// Assert that a user is able to remove a tower address during
		// session negotiation as long as the address in question is not
		// currently being used.
		name: "removing a tower during session negotiation",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
			noServerStart: true,
		},
		fn: func(h *testHarness) {
			// The server has not started yet and so no session
			// negotiation with the server will be in progress, so
			// the client should be able to remove the server.
			err := wait.NoError(func() error {
				return h.client.RemoveTower(
					h.serverAddr.IdentityKey, nil,
				)
			}, waitTime)
			require.NoError(h.t, err)

			// Set the server up so that its Dial function hangs
			// when the client calls it. This will force the client
			// to remain in the state where it has locked the
			// address of the server.
			h.server, err = wtserver.New(h.serverCfg)
			require.NoError(h.t, err)

			cancel := make(chan struct{})
			h.net.registerConnCallback(
				h.serverAddr, func(peer wtserver.Peer) {
					select {
					case <-h.quit:
					case <-cancel:
					}
				},
			)

			// Also add a new tower address.
			towerTCPAddr, err := net.ResolveTCPAddr(
				"tcp", towerAddr2Str,
			)
			require.NoError(h.t, err)

			towerAddr := &lnwire.NetAddress{
				IdentityKey: h.serverAddr.IdentityKey,
				Address:     towerTCPAddr,
			}

			// Register the new address in the mock-net.
			h.net.registerConnCallback(
				towerAddr, h.server.InboundPeerConnected,
			)

			// Now start the server.
			require.NoError(h.t, h.server.Start())

			// Re-add the server to the client
			err = h.client.AddTower(h.serverAddr)
			require.NoError(h.t, err)

			// Also add the new tower address.
			err = h.client.AddTower(towerAddr)
			require.NoError(h.t, err)

			// Assert that if the client attempts to remove the
			// tower's first address, then it will error due to
			// address currently being locked for session
			// negotiation.
			err = wait.Predicate(func() bool {
				err = h.client.RemoveTower(
					h.serverAddr.IdentityKey,
					h.serverAddr.Address,
				)
				return errors.Is(err, wtclient.ErrAddrInUse)
			}, waitTime)
			require.NoError(h.t, err)

			// Assert that the second address can be removed since
			// it is not being used for session negotiation.
			err = wait.NoError(func() error {
				return h.client.RemoveTower(
					h.serverAddr.IdentityKey, towerTCPAddr,
				)
			}, waitTime)
			require.NoError(h.t, err)

			// Allow the dial to the first address to stop hanging.
			close(cancel)

			// Assert that the client can now remove the first
			// address.
			err = wait.NoError(func() error {
				return h.client.RemoveTower(
					h.serverAddr.IdentityKey, nil,
				)
			}, waitTime)
			require.NoError(h.t, err)
		},
	}, {
		name: "assert that sessions are correctly marked as closable",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const numUpdates = 5

			// In this test we assert that a channel is correctly
			// marked as closed and that sessions are also correctly
			// marked as closable.

			// We start with the sendUpdatesOn parameter set to
			// false so that we can test that channels are correctly
			// evaluated at startup.
			h.sendUpdatesOn = false

			// Advance channel 0 to create all states and back them
			// all up. This will saturate the session with updates
			// for channel 0 which means that the session should be
			// considered closable when channel 0 is closed.
			hints := h.advanceChannelN(0, numUpdates)
			h.backupStates(0, 0, numUpdates, nil)
			h.waitServerUpdates(hints, waitTime)

			// We expect only 1 session to have updates for this
			// channel.
			sessionIDs := h.relevantSessions(0)
			require.Len(h.t, sessionIDs, 1)

			// Since channel 0 is still open, the session should not
			// yet be closable.
			require.False(h.t, h.isSessionClosable(sessionIDs[0]))

			// Close the channel.
			h.closeChannel(0, 1)

			// Since updates are currently not being sent, we expect
			// the session to still not be marked as closable.
			require.False(h.t, h.isSessionClosable(sessionIDs[0]))

			// Restart the client.
			h.client.ForceQuit()
			h.startClient()

			// The session should now have been marked as closable.
			err := wait.Predicate(func() bool {
				return h.isSessionClosable(sessionIDs[0])
			}, waitTime)
			require.NoError(h.t, err)

			// Now we set sendUpdatesOn to true and do the same with
			// a new channel. A restart should now not be necessary
			// anymore.
			h.sendUpdatesOn = true

			h.makeChannel(
				1, h.cfg.localBalance, h.cfg.remoteBalance,
			)
			h.registerChannel(1)

			hints = h.advanceChannelN(1, numUpdates)
			h.backupStates(1, 0, numUpdates, nil)
			h.waitServerUpdates(hints, waitTime)

			// Determine the ID of the session of interest.
			sessionIDs = h.relevantSessions(1)

			// We expect only 1 session to have updates for this
			// channel.
			require.Len(h.t, sessionIDs, 1)

			// Assert that the session is not yet closable since
			// the channel is still open.
			require.False(h.t, h.isSessionClosable(sessionIDs[0]))

			// Now close the channel.
			h.closeChannel(1, 1)

			// Since the updates have been turned on, the session
			// should now show up as closable.
			err = wait.Predicate(func() bool {
				return h.isSessionClosable(sessionIDs[0])
			}, waitTime)
			require.NoError(h.t, err)

			// Now we test that a session must be exhausted with all
			// channels closed before it is seen as closable.
			h.makeChannel(
				2, h.cfg.localBalance, h.cfg.remoteBalance,
			)
			h.registerChannel(2)

			// Fill up only half of the session updates.
			hints = h.advanceChannelN(2, numUpdates)
			h.backupStates(2, 0, numUpdates/2, nil)
			h.waitServerUpdates(hints[:numUpdates/2], waitTime)

			// Determine the ID of the session of interest.
			sessionIDs = h.relevantSessions(2)

			// We expect only 1 session to have updates for this
			// channel.
			require.Len(h.t, sessionIDs, 1)

			// Now close the channel.
			h.closeChannel(2, 1)

			// The session should _not_ be closable due to it not
			// being exhausted yet.
			require.False(h.t, h.isSessionClosable(sessionIDs[0]))

			// Create a new channel.
			h.makeChannel(
				3, h.cfg.localBalance, h.cfg.remoteBalance,
			)
			h.registerChannel(3)

			hints = h.advanceChannelN(3, numUpdates)
			h.backupStates(3, 0, numUpdates, nil)
			h.waitServerUpdates(hints, waitTime)

			// Close it.
			h.closeChannel(3, 1)

			// Now the session should be closable.
			err = wait.Predicate(func() bool {
				return h.isSessionClosable(sessionIDs[0])
			}, waitTime)
			require.NoError(h.t, err)

			// Now we will mine a few blocks. This will cause the
			// necessary session-close-range to be exceeded meaning
			// that the client should send the DeleteSession message
			// to the server. We will assert that both the client
			// and server have deleted the appropriate sessions and
			// channel info.

			// Before we mine blocks, assert that the client
			// currently has 3 closable sessions.
			closableSess, err := h.clientDB.ListClosableSessions()
			require.NoError(h.t, err)
			require.Len(h.t, closableSess, 3)

			// Assert that the server is also aware of all of these
			// sessions.
			for sid := range closableSess {
				_, err := h.serverDB.GetSessionInfo(&sid)
				require.NoError(h.t, err)
			}

			// Also make a note of the total number of sessions the
			// client has.
			sessions, err := h.clientDB.ListClientSessions(nil)
			require.NoError(h.t, err)
			require.Len(h.t, sessions, 4)

			h.mine(3)

			// The client should no longer have any closable
			// sessions and the total list of client sessions should
			// no longer include the three that it previously had
			// marked as closable. The server should also no longer
			// have these sessions in its DB.
			err = wait.Predicate(func() bool {
				sess, err := h.clientDB.ListClientSessions(nil)
				require.NoError(h.t, err)

				cs, err := h.clientDB.ListClosableSessions()
				require.NoError(h.t, err)

				if len(sess) != 1 || len(cs) != 0 {
					return false
				}

				for sid := range closableSess {
					_, ok := sess[sid]
					if ok {
						return false
					}

					_, err := h.serverDB.GetSessionInfo(
						&sid,
					)
					if !errors.Is(
						err, wtdb.ErrSessionNotFound,
					) {
						return false
					}
				}

				return true

			}, waitTime)
			require.NoError(h.t, err)
		},
	},
	{
		// Demonstrate that the client is unable to recover after
		// deleting its database by skipping through key indices until
		// it gets to one that does not result in the
		// CreateSessionCodeAlreadyExists error code being returned from
		// the server.
		name: "continue after client database deletion",
		cfg: harnessCfg{
			localBalance:  localBalance,
			remoteBalance: remoteBalance,
			policy: wtpolicy.Policy{
				TxPolicy:   defaultTxPolicy,
				MaxUpdates: 5,
			},
		},
		fn: func(h *testHarness) {
			const (
				numUpdates = 5
				chanID     = 0
			)

			// Generate numUpdates retributions.
			hints := h.advanceChannelN(chanID, numUpdates)

			// Back half of the states up.
			h.backupStates(chanID, 0, numUpdates/2, nil)

			// Wait for the updates to be populated in the server's
			// database.
			h.waitServerUpdates(hints[:numUpdates/2], waitTime)

			// Now stop the client and reset its database.
			require.NoError(h.t, h.client.Stop())

			db := wtmock.NewClientDB()
			h.clientDB = db
			h.clientCfg.DB = db

			// Restart the client.
			h.startClient()

			// We need to re-register the channel due to the client
			// db being reset.
			h.registerChannel(0)

			// Attempt to back up the remaining tasks.
			h.backupStates(chanID, numUpdates/2, numUpdates, nil)

			// Show that the server does get the remaining updates.
			h.waitServerUpdates(hints[numUpdates/2:], waitTime)
		},
	},
}

// TestClient executes the client test suite, asserting the ability to backup
// states in a number of failure cases and it's reliability during shutdown.
func TestClient(t *testing.T) {
	for _, test := range clientTests {
		tc := test
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, tc.cfg)

			tc.fn(h)
		})
	}
}

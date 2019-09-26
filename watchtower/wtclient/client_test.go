// +build dev

package wtclient_test

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtmock"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
)

const (
	csvDelay uint32 = 144

	towerAddrStr = "18.28.243.2:9911"
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
		"mrX9vMRYLfVy1BnZbc5gZjuyaqH3ZW2ZHz", &chaincfg.TestNet3Params,
	)

	addrScript, _ = txscript.PayToAddrScript(addr)
)

// randPrivKey generates a new secp keypair, and returns the public key.
func randPrivKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()

	sk, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate pubkey: %v", err)
	}

	return sk
}

type mockNet struct {
	mu           sync.RWMutex
	connCallback func(wtserver.Peer)
}

func newMockNet(cb func(wtserver.Peer)) *mockNet {
	return &mockNet{
		connCallback: cb,
	}
}

func (m *mockNet) Dial(network string, address string) (net.Conn, error) {
	return nil, nil
}

func (m *mockNet) LookupHost(host string) ([]string, error) {
	panic("not implemented")
}

func (m *mockNet) LookupSRV(service string, proto string, name string) (string, []*net.SRV, error) {
	panic("not implemented")
}

func (m *mockNet) ResolveTCPAddr(network string, address string) (*net.TCPAddr, error) {
	panic("not implemented")
}

func (m *mockNet) AuthDial(localPriv *btcec.PrivateKey, netAddr *lnwire.NetAddress,
	dialer func(string, string) (net.Conn, error)) (wtserver.Peer, error) {

	localPk := localPriv.PubKey()
	localAddr := &net.TCPAddr{
		IP:   net.IP{0x32, 0x31, 0x30, 0x29},
		Port: 36723,
	}

	localPeer, remotePeer := wtmock.NewMockConn(
		localPk, netAddr.IdentityKey, localAddr, netAddr.Address, 0,
	)

	m.mu.RLock()
	m.connCallback(remotePeer)
	m.mu.RUnlock()

	return localPeer, nil
}

func (m *mockNet) setConnCallback(cb func(wtserver.Peer)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connCallback = cb
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
	if err != nil {
		t.Fatalf("unable to create to-local script: %v", err)
	}

	// Compute the to-local witness script hash.
	toLocalScriptHash, err := input.WitnessScriptHash(toLocalScript)
	if err != nil {
		t.Fatalf("unable to create to-local witness script hash: %v", err)
	}

	// Compute the to-remote witness script hash.
	toRemoteScriptHash, err := input.CommitScriptUnencumbered(c.toRemotePK)
	if err != nil {
		t.Fatalf("unable to create to-remote script: %v", err)
	}

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
		NoDelayKey:    c.toLocalPK,
		DelayKey:      c.toRemotePK,
	}

	retribution := &lnwallet.BreachRetribution{
		BreachTransaction:    commitTxn,
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

	if c.localBalance < amt {
		t.Fatalf("insufficient funds to send, need: %v, have: %v",
			amt, c.localBalance)
	}

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

	if c.remoteBalance < amt {
		t.Fatalf("insufficient funds to recv, need: %v, have: %v",
			amt, c.remoteBalance)
	}

	c.localBalance += amt
	c.remoteBalance -= amt
	c.createRemoteCommitTx(t)
}

// getState retrieves the channel's commitment and retribution at state i.
func (c *mockChannel) getState(i uint64) (*wire.MsgTx, *lnwallet.BreachRetribution) {
	c.mu.Lock()
	defer c.mu.Unlock()

	retribution := c.retributions[i]

	return retribution.BreachTransaction, retribution
}

type testHarness struct {
	t         *testing.T
	cfg       harnessCfg
	signer    *wtmock.MockSigner
	capacity  lnwire.MilliSatoshi
	clientDB  *wtmock.ClientDB
	clientCfg *wtclient.Config
	client    wtclient.Client
	serverDB  *wtmock.TowerDB
	serverCfg *wtserver.Config
	server    *wtserver.Server
	net       *mockNet

	mu       sync.Mutex
	channels map[lnwire.ChannelID]*mockChannel
}

type harnessCfg struct {
	localBalance       lnwire.MilliSatoshi
	remoteBalance      lnwire.MilliSatoshi
	policy             wtpolicy.Policy
	noRegisterChan0    bool
	noAckCreateSession bool
}

func newHarness(t *testing.T, cfg harnessCfg) *testHarness {
	towerTCPAddr, err := net.ResolveTCPAddr("tcp", towerAddrStr)
	if err != nil {
		t.Fatalf("Unable to resolve tower TCP addr: %v", err)
	}

	privKey, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("Unable to generate tower private key: %v", err)
	}

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
		NodePrivKey:  privKey,
		NewAddress: func() (btcutil.Address, error) {
			return addr, nil
		},
		NoAckCreateSession: cfg.noAckCreateSession,
	}

	server, err := wtserver.New(serverCfg)
	if err != nil {
		t.Fatalf("unable to create wtserver: %v", err)
	}

	signer := wtmock.NewMockSigner()
	mockNet := newMockNet(server.InboundPeerConnected)
	clientDB := wtmock.NewClientDB()

	clientCfg := &wtclient.Config{
		Signer: signer,
		Dial: func(string, string) (net.Conn, error) {
			return nil, nil
		},
		DB:            clientDB,
		AuthDial:      mockNet.AuthDial,
		SecretKeyRing: wtmock.NewSecretKeyRing(),
		Policy:        cfg.policy,
		NewAddress: func() ([]byte, error) {
			return addrScript, nil
		},
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		MinBackoff:   time.Millisecond,
		MaxBackoff:   10 * time.Millisecond,
	}
	client, err := wtclient.New(clientCfg)
	if err != nil {
		t.Fatalf("Unable to create wtclient: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("Unable to start wtserver: %v", err)
	}

	if err = client.Start(); err != nil {
		server.Stop()
		t.Fatalf("Unable to start wtclient: %v", err)
	}
	if err := client.AddTower(towerAddr); err != nil {
		server.Stop()
		t.Fatalf("Unable to add tower to wtclient: %v", err)
	}

	h := &testHarness{
		t:         t,
		cfg:       cfg,
		signer:    signer,
		capacity:  cfg.localBalance + cfg.remoteBalance,
		clientDB:  clientDB,
		clientCfg: clientCfg,
		client:    client,
		serverDB:  serverDB,
		serverCfg: serverCfg,
		server:    server,
		net:       mockNet,
		channels:  make(map[lnwire.ChannelID]*mockChannel),
	}

	h.makeChannel(0, h.cfg.localBalance, h.cfg.remoteBalance)
	if !cfg.noRegisterChan0 {
		h.registerChannel(0)
	}

	return h
}

// startServer creates a new server using the harness's current serverCfg and
// starts it after pointing the mockNet's callback to the new server.
func (h *testHarness) startServer() {
	h.t.Helper()

	var err error
	h.server, err = wtserver.New(h.serverCfg)
	if err != nil {
		h.t.Fatalf("unable to create wtserver: %v", err)
	}

	h.net.setConnCallback(h.server.InboundPeerConnected)

	if err := h.server.Start(); err != nil {
		h.t.Fatalf("unable to start wtserver: %v", err)
	}
}

// startClient creates a new server using the harness's current clientCf and
// starts it.
func (h *testHarness) startClient() {
	h.t.Helper()

	towerTCPAddr, err := net.ResolveTCPAddr("tcp", towerAddrStr)
	if err != nil {
		h.t.Fatalf("Unable to resolve tower TCP addr: %v", err)
	}
	towerAddr := &lnwire.NetAddress{
		IdentityKey: h.serverCfg.NodePrivKey.PubKey(),
		Address:     towerTCPAddr,
	}

	h.client, err = wtclient.New(h.clientCfg)
	if err != nil {
		h.t.Fatalf("unable to create wtclient: %v", err)
	}
	if err := h.client.Start(); err != nil {
		h.t.Fatalf("unable to start wtclient: %v", err)
	}
	if err := h.client.AddTower(towerAddr); err != nil {
		h.t.Fatalf("unable to add tower to wtclient: %v", err)
	}
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

	if ok {
		h.t.Fatalf("channel %d already created", id)
	}
}

// channel retrieves the channel corresponding to id.
//
// NOTE: The method fails if a channel for id does not exist.
func (h *testHarness) channel(id uint64) *mockChannel {
	h.t.Helper()

	h.mu.Lock()
	c, ok := h.channels[chanIDFromInt(id)]
	h.mu.Unlock()
	if !ok {
		h.t.Fatalf("unable to fetch channel %d", id)
	}

	return c
}

// registerChannel registers the channel identified by id with the client.
func (h *testHarness) registerChannel(id uint64) {
	h.t.Helper()

	chanID := chanIDFromInt(id)
	err := h.client.RegisterChannel(chanID)
	if err != nil {
		h.t.Fatalf("unable to register channel %d: %v", id, err)
	}
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
		commitTx, _ := h.channel(id).getState(i)
		breachTxID := commitTx.TxHash()
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
	err := h.client.BackupState(&chanID, retribution, false)
	if err != expErr {
		h.t.Fatalf("back error mismatch, want: %v, got: %v",
			expErr, err)
	}
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
		commitTx, _ := channel.getState(i)
		breachTxID := commitTx.TxHash()
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
		commitTx, _ := channel.getState(i)
		breachTxID := commitTx.TxHash()
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

	if len(hints) != len(hintSet) {
		h.t.Fatalf("breach hints are not unique, list-len: %d "+
			"set-len: %d", len(hints), len(hintSet))
	}

	// Closure to assert the server's matches are consistent with the hint
	// set.
	serverHasHints := func(matches []wtdb.Match) bool {
		if len(hintSet) != len(matches) {
			return false
		}

		for _, match := range matches {
			if _, ok := hintSet[match.Hint]; ok {
				continue
			}

			h.t.Fatalf("match %v in db is not in hint set",
				match.Hint)
		}

		return true
	}

	failTimeout := time.After(timeout)
	for {
		select {
		case <-time.After(time.Second):
			matches, err := h.serverDB.QueryMatches(hints)
			switch {
			case err != nil:
				h.t.Fatalf("unable to query for hints: %v", err)

			case wantUpdates && serverHasHints(matches):
				return

			case wantUpdates:
				h.t.Logf("Received %d/%d\n", len(matches),
					len(hints))
			}

		case <-failTimeout:
			matches, err := h.serverDB.QueryMatches(hints)
			switch {
			case err != nil:
				h.t.Fatalf("unable to query for hints: %v", err)

			case serverHasHints(matches):
				return

			default:
				h.t.Fatalf("breach hints not received, only "+
					"got %d/%d", len(matches), len(hints))
			}
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
	if err != nil {
		h.t.Fatalf("unable to query for matches: %v", err)
	}

	// Assert that the number of matches is exactly the number of provided
	// hints.
	if len(matches) != len(hints) {
		h.t.Fatalf("expected: %d matches, got: %d", len(hints),
			len(matches))
	}

	// Assert that all of the matches correspond to a session with the
	// expected policy.
	for _, match := range matches {
		matchPolicy := match.SessionInfo.Policy
		if expPolicy != matchPolicy {
			h.t.Fatalf("expected session to have policy: %v, "+
				"got: %v", expPolicy, matchPolicy)
		}
	}
}

const (
	localBalance  = lnwire.MilliSatoshi(100000000)
	remoteBalance = lnwire.MilliSatoshi(200000000)
)

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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
			h.server.Stop()
			h.serverCfg.NoAckUpdates = true
			h.startServer()
			defer h.server.Stop()

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
			h.server.Stop()
			h.serverCfg.NoAckUpdates = false
			h.startServer()
			defer h.server.Stop()

			// Restart the client and allow it to process the
			// committed update.
			h.startClient()
			defer h.client.ForceQuit()

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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
			h.server.Stop()
			h.serverCfg.NoAckUpdates = true
			h.startServer()
			defer h.server.Stop()

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
			h.server.Stop()
			h.serverCfg.NoAckUpdates = false
			h.startServer()
			defer h.server.Stop()

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, 5*time.Second)
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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
			h.server.Stop()
			h.serverCfg.NoAckCreateSession = false
			h.startServer()
			defer h.server.Stop()

			// Restart the client with the same policy, which will
			// immediately try to overwrite the old session with an
			// identical one.
			h.startClient()
			defer h.client.ForceQuit()

			// Now, queue the retributions for backup.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, 5*time.Second)

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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
			h.server.Stop()
			h.serverCfg.NoAckCreateSession = false
			h.startServer()
			defer h.server.Stop()

			// Restart the client with a new policy, which will
			// immediately try to overwrite the prior session with
			// the old policy.
			h.clientCfg.Policy.SweepFeeRate *= 2
			h.startClient()
			defer h.client.ForceQuit()

			// Now, queue the retributions for backup.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, 5*time.Second)

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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
			h.client.Stop()

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
			defer h.client.ForceQuit()

			// Now, queue the second half of the retributions.
			h.backupStates(chanID, numUpdates/2, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, 5*time.Second)

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
				TxPolicy: wtpolicy.TxPolicy{
					BlobType:     blob.TypeAltruistCommit,
					SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
				},
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
			h.waitServerUpdates(hints[:len(hints)/2], 5*time.Second)

			// Restart the client, so we can ensure the deduping is
			// maintained across restarts.
			h.client.Stop()
			h.startClient()
			defer h.client.ForceQuit()

			// Try to back up the full range of retributions. Only
			// the second half should actually be sent.
			h.backupStates(chanID, 0, numUpdates, nil)

			// Wait for all of the updates to be populated in the
			// server's database.
			h.waitServerUpdates(hints, 5*time.Second)
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
			defer h.server.Stop()
			defer h.client.ForceQuit()

			tc.fn(h)
		})
	}
}

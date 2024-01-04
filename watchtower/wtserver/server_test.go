package wtserver_test

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtmock"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
	"github.com/stretchr/testify/require"
)

var (
	// addr is the server's reward address given to watchtower clients.
	addr, _ = btcutil.DecodeAddress(
		"tb1pw8gzj8clt3v5lxykpgacpju5n8xteskt7gxhmudu6pa70nwfhe6s3unsyk",
		&chaincfg.TestNet3Params,
	)

	addrScript, _ = txscript.PayToAddrScript(addr)

	testnetChainHash = *chaincfg.TestNet3Params.GenesisHash

	testBlob = make(
		[]byte, blob.NonceSize+blob.V0PlaintextSize+
			blob.CiphertextExpansion,
	)
)

// randPubKey generates a new secp keypair, and returns the public key.
func randPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	sk, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate pubkey")

	return sk.PubKey()
}

// initServer creates and starts a new server using the server.DB and timeout.
// If the provided database is nil, a mock db will be used.
func initServer(t *testing.T, db wtserver.DB,
	timeout time.Duration) wtserver.Interface {

	t.Helper()

	if db == nil {
		db = wtmock.NewTowerDB()
	}

	s, err := wtserver.New(&wtserver.Config{
		DB:           db,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		NewAddress: func() (btcutil.Address, error) {
			return addr, nil
		},
		ChainHash: testnetChainHash,
	})
	require.NoError(t, err, "unable to create server")

	if err = s.Start(); err != nil {
		t.Fatalf("unable to start server: %v", err)
	}
	t.Cleanup(func() {
		require.NoError(t, s.Stop())
	})

	return s
}

// TestServerOnlyAcceptOnePeer checks that the server will reject duplicate
// peers with the same session id by disconnecting them. This is accomplished by
// connecting two distinct peers with the same session id, and trying to send
// messages on both connections. Since one should be rejected, we verify that
// only one of the connections is able to send messages.
func TestServerOnlyAcceptOnePeer(t *testing.T) {
	t.Parallel()

	const timeoutDuration = 500 * time.Millisecond

	s := initServer(t, nil, timeoutDuration)

	localPub := randPubKey(t)

	// Create two peers using the same session id.
	peerPub := randPubKey(t)
	peer1 := wtmock.NewMockPeer(localPub, peerPub, nil, 0)
	peer2 := wtmock.NewMockPeer(localPub, peerPub, nil, 0)

	// Serialize a Init message to be sent by both peers.
	init := wtwire.NewInitMessage(
		lnwire.NewRawFeatureVector(), testnetChainHash,
	)

	var b bytes.Buffer
	_, err := wtwire.WriteMessage(&b, init, 0)
	require.NoError(t, err, "unable to write message")

	msg := b.Bytes()

	// Connect both peers to the server simultaneously.
	s.InboundPeerConnected(peer1)
	s.InboundPeerConnected(peer2)

	// Use a timeout of twice the server's timeouts, to ensure the server
	// has time to process the messages.
	timeout := time.After(2 * timeoutDuration)

	// Try to send a message on either peer, and record the opposite peer as
	// the one we assume to be rejected.
	var (
		rejectedPeer *wtmock.MockPeer
		acceptedPeer *wtmock.MockPeer
	)
	select {
	case peer1.IncomingMsgs <- msg:
		acceptedPeer = peer1
		rejectedPeer = peer2
	case peer2.IncomingMsgs <- msg:
		acceptedPeer = peer2
		rejectedPeer = peer1
	case <-timeout:
		t.Fatalf("unable to send message via either peer")
	}

	// Try again to send a message, this time only via the assumed-rejected
	// peer. We expect our conservative timeout to expire, as the server
	// isn't reading from this peer. Before the timeout, the accepted peer
	// should also receive a reply to its Init message.
	select {
	case <-acceptedPeer.OutgoingMsgs:
		select {
		case rejectedPeer.IncomingMsgs <- msg:
			t.Fatalf("rejected peer should not have received message")
		case <-timeout:
			// Accepted peer got reply, rejected peer go nothing.
		}
	case rejectedPeer.IncomingMsgs <- msg:
		t.Fatalf("rejected peer should not have received message")
	case <-timeout:
		t.Fatalf("accepted peer should have received init message")
	}
}

type createSessionTestCase struct {
	name            string
	initMsg         *wtwire.Init
	createMsg       *wtwire.CreateSession
	expReply        *wtwire.CreateSessionReply
	expDupReply     *wtwire.CreateSessionReply
	sendStateUpdate bool
}

var createSessionTests = []createSessionTestCase{
	{
		name: "duplicate session create altruist anchor commit",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistAnchorCommit,
			MaxUpdates:   1000,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		expReply: &wtwire.CreateSessionReply{
			Code: wtwire.CodeOK,
			Data: []byte{},
		},
		expDupReply: &wtwire.CreateSessionReply{
			Code: wtwire.CodeOK,
			Data: []byte{},
		},
	},
	{
		name: "duplicate session create",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   1000,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		expReply: &wtwire.CreateSessionReply{
			Code: wtwire.CodeOK,
			Data: []byte{},
		},
		expDupReply: &wtwire.CreateSessionReply{
			Code: wtwire.CodeOK,
			Data: []byte{},
		},
	},
	{
		name: "duplicate session create after use",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   1000,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		expReply: &wtwire.CreateSessionReply{
			Code: wtwire.CodeOK,
			Data: []byte{},
		},
		expDupReply: &wtwire.CreateSessionReply{
			Code:        wtwire.CreateSessionCodeAlreadyExists,
			LastApplied: 1,
			Data:        []byte{},
		},
		sendStateUpdate: true,
	},
	{
		name: "duplicate session create reward",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeRewardCommit,
			MaxUpdates:   1000,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		expReply: &wtwire.CreateSessionReply{
			Code: wtwire.CodeOK,
			Data: addrScript,
		},
		expDupReply: &wtwire.CreateSessionReply{
			Code: wtwire.CodeOK,
			Data: addrScript,
		},
	},
	{
		name: "reject unsupported blob type",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     0,
			MaxUpdates:   1000,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		expReply: &wtwire.CreateSessionReply{
			Code: wtwire.CreateSessionCodeRejectBlobType,
			Data: []byte{},
		},
	},
	// TODO(conner): add policy rejection tests
}

// TestServerCreateSession checks the server's behavior in response to a
// table-driven set of CreateSession messages.
func TestServerCreateSession(t *testing.T) {
	t.Parallel()

	for i, test := range createSessionTests {
		t.Run(test.name, func(t *testing.T) {
			testServerCreateSession(t, i, test)
		})
	}
}

func testServerCreateSession(t *testing.T, i int, test createSessionTestCase) {
	const timeoutDuration = 500 * time.Millisecond

	s := initServer(t, nil, timeoutDuration)

	localPub := randPubKey(t)

	// Create a new client and connect to server.
	peerPub := randPubKey(t)
	peer := wtmock.NewMockPeer(localPub, peerPub, nil, 0)
	connect(t, s, peer, test.initMsg, timeoutDuration)

	// Send the CreateSession message, and wait for a reply.
	sendMsg(t, test.createMsg, peer, timeoutDuration)

	reply := recvReply(
		t, "MsgCreateSessionReply", peer, timeoutDuration,
	).(*wtwire.CreateSessionReply)

	// Verify that the server's response matches our expectation.
	if !reflect.DeepEqual(reply, test.expReply) {
		t.Fatalf("[test %d] expected reply %v, got %d",
			i, test.expReply, reply)
	}

	// Assert that the server closes the connection after processing the
	// CreateSession.
	assertConnClosed(t, peer, 2*timeoutDuration)

	// If this test did not request sending a duplicate CreateSession, we can
	// continue to the next test.
	if test.expDupReply == nil {
		return
	}

	if test.sendStateUpdate {
		peer = wtmock.NewMockPeer(localPub, peerPub, nil, 0)
		connect(t, s, peer, test.initMsg, timeoutDuration)
		update := &wtwire.StateUpdate{
			SeqNum:        1,
			IsComplete:    1,
			EncryptedBlob: testBlob,
		}
		sendMsg(t, update, peer, timeoutDuration)

		assertConnClosed(t, peer, 2*timeoutDuration)
	}

	// Simulate a peer with the same session id connection to the server
	// again.
	peer = wtmock.NewMockPeer(localPub, peerPub, nil, 0)
	connect(t, s, peer, test.initMsg, timeoutDuration)

	// Send the _same_ CreateSession message as the first attempt.
	sendMsg(t, test.createMsg, peer, timeoutDuration)

	reply = recvReply(
		t, "MsgCreateSessionReply", peer, timeoutDuration,
	).(*wtwire.CreateSessionReply)

	// Ensure that the server's reply matches our expected response for a
	// duplicate send.
	if !reflect.DeepEqual(reply, test.expDupReply) {
		t.Fatalf("[test %d] expected reply %v, got %v",
			i, test.expDupReply, reply)
	}

	// Finally, check that the server tore down the connection.
	assertConnClosed(t, peer, 2*timeoutDuration)
}

type stateUpdateTestCase struct {
	name      string
	initMsg   *wtwire.Init
	createMsg *wtwire.CreateSession
	updates   []*wtwire.StateUpdate
	replies   []*wtwire.StateUpdateReply
}

var stateUpdateTests = []stateUpdateTestCase{
	// Valid update sequence, send seqnum == lastapplied as last update.
	{
		name: "perm fail after sending seqnum equal lastapplied",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   3,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 2, LastApplied: 1, EncryptedBlob: testBlob},
			{SeqNum: 3, LastApplied: 2, EncryptedBlob: testBlob},
			{SeqNum: 3, LastApplied: 3, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			{Code: wtwire.CodeOK, LastApplied: 3},
			{
				Code:        wtwire.CodePermanentFailure,
				LastApplied: 3,
			},
		},
	},
	// Send update that skips next expected sequence number.
	{
		name: "skip sequence number",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   4,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 2, LastApplied: 0, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{
				Code:        wtwire.StateUpdateCodeSeqNumOutOfOrder,
				LastApplied: 0,
			},
		},
	},
	// Send update that reverts to older sequence number.
	{
		name: "revert to older seqnum",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   4,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 2, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 1, LastApplied: 0, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			{
				Code:        wtwire.StateUpdateCodeSeqNumOutOfOrder,
				LastApplied: 2,
			},
		},
	},
	// Send update echoing a last applied that is lower than previous value.
	{
		name: "revert to older lastapplied",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   4,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 2, LastApplied: 1, EncryptedBlob: testBlob},
			{SeqNum: 3, LastApplied: 2, EncryptedBlob: testBlob},
			{SeqNum: 4, LastApplied: 1, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			{Code: wtwire.CodeOK, LastApplied: 3},
			{Code: wtwire.StateUpdateCodeClientBehind, LastApplied: 3},
		},
	},
	// Valid update sequence with disconnection, ensure resumes resume.
	// Client echos last applied as they are received.
	{
		name: "resume after disconnect",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   4,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 2, LastApplied: 1, EncryptedBlob: testBlob},
			nil, // Wait for read timeout to drop conn, then reconnect.
			{SeqNum: 3, LastApplied: 2, EncryptedBlob: testBlob},
			{SeqNum: 4, LastApplied: 3, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			nil,
			{Code: wtwire.CodeOK, LastApplied: 3},
			{Code: wtwire.CodeOK, LastApplied: 4},
		},
	},
	// Valid update sequence with disconnection, resume next update. Client
	// doesn't echo last applied until last message.
	{
		name: "resume after disconnect lagging lastapplied",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   4,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 2, LastApplied: 0, EncryptedBlob: testBlob},
			nil, // Wait for read timeout to drop conn, then reconnect.
			{SeqNum: 3, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 4, LastApplied: 3, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			nil,
			{Code: wtwire.CodeOK, LastApplied: 3},
			{Code: wtwire.CodeOK, LastApplied: 4},
		},
	},
	// Valid update sequence with disconnection, resume last update.  Client
	// doesn't echo last applied until last message.
	{
		name: "resume after disconnect lagging lastapplied",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   4,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 2, LastApplied: 0, EncryptedBlob: testBlob},
			nil, // Wait for read timeout to drop conn, then reconnect.
			{SeqNum: 2, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 3, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 4, LastApplied: 3, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			nil,
			{Code: wtwire.CodeOK, LastApplied: 2},
			{Code: wtwire.CodeOK, LastApplied: 3},
			{Code: wtwire.CodeOK, LastApplied: 4},
		},
	},
	// Send update with sequence number that exceeds MaxUpdates.
	{
		name: "seqnum exceed maxupdates",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   3,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0, EncryptedBlob: testBlob},
			{SeqNum: 2, LastApplied: 1, EncryptedBlob: testBlob},
			{SeqNum: 3, LastApplied: 2, EncryptedBlob: testBlob},
			{SeqNum: 4, LastApplied: 3, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			{Code: wtwire.CodeOK, LastApplied: 3},
			{
				Code:        wtwire.StateUpdateCodeMaxUpdatesExceeded,
				LastApplied: 3,
			},
		},
	},
	// Ensure sequence number 0 causes permanent failure.
	{
		name: "perm fail after seqnum 0",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			testnetChainHash,
		),
		createMsg: &wtwire.CreateSession{
			BlobType:     blob.TypeAltruistCommit,
			MaxUpdates:   3,
			RewardBase:   0,
			RewardRate:   0,
			SweepFeeRate: 10000,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 0, LastApplied: 0, EncryptedBlob: testBlob},
		},
		replies: []*wtwire.StateUpdateReply{
			{
				Code:        wtwire.CodePermanentFailure,
				LastApplied: 0,
			},
		},
	},
}

// TestServerStateUpdates tests the behavior of the server in response to
// watchtower clients sending StateUpdate messages, after having already
// established an open session. The test asserts that the server responds
// with the appropriate failure codes in a number of failure conditions where
// the server and client desynchronize. It also checks the ability of the client
// to disconnect, connect, and continue updating from the last successful state
// update.
func TestServerStateUpdates(t *testing.T) {
	t.Parallel()

	for _, test := range stateUpdateTests {
		t.Run(test.name, func(t *testing.T) {
			testServerStateUpdates(t, test)
		})
	}
}

func testServerStateUpdates(t *testing.T, test stateUpdateTestCase) {
	const timeoutDuration = 100 * time.Millisecond

	s := initServer(t, nil, timeoutDuration)

	localPub := randPubKey(t)

	// Create a new client and connect to the server.
	peerPub := randPubKey(t)
	peer := wtmock.NewMockPeer(localPub, peerPub, nil, 0)
	connect(t, s, peer, test.initMsg, timeoutDuration)

	// Register a session for this client to use in the subsequent tests.
	sendMsg(t, test.createMsg, peer, timeoutDuration)
	initReply := recvReply(
		t, "MsgCreateSessionReply", peer, timeoutDuration,
	).(*wtwire.CreateSessionReply)

	// Fail if the server rejected our proposed CreateSession message.
	if initReply.Code != wtwire.CodeOK {
		t.Fatalf("server rejected session init")
	}

	// Check that the server closed the connection used to register the
	// session.
	assertConnClosed(t, peer, 2*timeoutDuration)

	// Now that the original connection has been closed, connect a new
	// client with the same session id.
	peer = wtmock.NewMockPeer(localPub, peerPub, nil, 0)
	connect(t, s, peer, test.initMsg, timeoutDuration)

	// Send the intended StateUpdate messages in series.
	for j, update := range test.updates {
		// A nil update signals that we should wait for the prior
		// connection to die, before re-register with the same session
		// identifier.
		if update == nil {
			assertConnClosed(t, peer, 2*timeoutDuration)

			peer = wtmock.NewMockPeer(localPub, peerPub, nil, 0)
			connect(t, s, peer, test.initMsg, timeoutDuration)

			continue
		}

		// Send the state update and verify it against our expected
		// response.
		sendMsg(t, update, peer, timeoutDuration)
		reply := recvReply(
			t, "MsgStateUpdateReply", peer, timeoutDuration,
		).(*wtwire.StateUpdateReply)

		if !reflect.DeepEqual(reply, test.replies[j]) {
			t.Fatalf("[update %d] expected reply "+
				"%v, got %d", j,
				test.replies[j], reply)
		}
	}

	// Check that the final connection is properly cleaned up by the server.
	assertConnClosed(t, peer, 2*timeoutDuration)
}

// TestServerDeleteSession asserts the response to a DeleteSession request, and
// checking that the proper error is returned when the session doesn't exist and
// that a successful deletion does not disrupt other sessions.
func TestServerDeleteSession(t *testing.T) {
	db := wtmock.NewTowerDB()

	localPub := randPubKey(t)

	// Initialize two distinct peers with different session ids.
	peerPub1 := randPubKey(t)
	peerPub2 := randPubKey(t)

	id1 := wtdb.NewSessionIDFromPubKey(peerPub1)
	id2 := wtdb.NewSessionIDFromPubKey(peerPub2)

	// Create closure to simplify assertions on session existence with the
	// server's database.
	hasSession := func(t *testing.T, id *wtdb.SessionID, shouldHave bool) {
		t.Helper()

		_, err := db.GetSessionInfo(id)
		switch {
		case shouldHave && err != nil:
			t.Fatalf("expected server to have session %s, got: %v",
				id, err)
		case !shouldHave && err != wtdb.ErrSessionNotFound:
			t.Fatalf("expected ErrSessionNotFound for session %s, "+
				"got: %v", id, err)
		}
	}

	initMsg := wtwire.NewInitMessage(
		lnwire.NewRawFeatureVector(),
		testnetChainHash,
	)

	createSession := &wtwire.CreateSession{
		BlobType:     blob.TypeAltruistCommit,
		MaxUpdates:   1000,
		RewardBase:   0,
		RewardRate:   0,
		SweepFeeRate: 10000,
	}

	const timeoutDuration = 100 * time.Millisecond

	s := initServer(t, db, timeoutDuration)

	// Create a session for peer2 so that the server's db isn't completely
	// empty.
	peer2 := wtmock.NewMockPeer(localPub, peerPub2, nil, 0)
	connect(t, s, peer2, initMsg, timeoutDuration)
	sendMsg(t, createSession, peer2, timeoutDuration)
	assertConnClosed(t, peer2, 2*timeoutDuration)

	// Our initial assertions are that peer2 has a valid session, but peer1
	// has not created one.
	hasSession(t, &id1, false)
	hasSession(t, &id2, true)

	peer1Msgs := []struct {
		send   wtwire.Message
		recv   wtwire.Message
		assert func(t *testing.T)
	}{
		{
			// Deleting unknown session should fail.
			send: &wtwire.DeleteSession{},
			recv: &wtwire.DeleteSessionReply{
				Code: wtwire.DeleteSessionCodeNotFound,
			},
			assert: func(t *testing.T) {
				// Peer2 should still be only session.
				hasSession(t, &id1, false)
				hasSession(t, &id2, true)
			},
		},
		{
			// Create session for peer1.
			send: createSession,
			recv: &wtwire.CreateSessionReply{
				Code: wtwire.CodeOK,
				Data: []byte{},
			},
			assert: func(t *testing.T) {
				// Both peers should have sessions.
				hasSession(t, &id1, true)
				hasSession(t, &id2, true)
			},
		},

		{
			// Delete peer1's session.
			send: &wtwire.DeleteSession{},
			recv: &wtwire.DeleteSessionReply{
				Code: wtwire.CodeOK,
			},
			assert: func(t *testing.T) {
				// Peer1's session should have been removed.
				hasSession(t, &id1, false)
				hasSession(t, &id2, true)
			},
		},
	}

	// Now as peer1, process the canned messages defined above. This will:
	// 1. Try to delete an unknown session and get a not found error code.
	// 2. Create a new session using the same parameters as peer2.
	// 3. Delete the newly created session and get an OK.
	for _, msg := range peer1Msgs {
		peer1 := wtmock.NewMockPeer(localPub, peerPub1, nil, 0)
		connect(t, s, peer1, initMsg, timeoutDuration)
		sendMsg(t, msg.send, peer1, timeoutDuration)
		reply := recvReply(
			t, msg.recv.MsgType().String(), peer1, timeoutDuration,
		)

		if !reflect.DeepEqual(reply, msg.recv) {
			t.Fatalf("expected reply: %v, got: %v", msg.recv, reply)
		}

		assertConnClosed(t, peer1, 2*timeoutDuration)

		// Invoke assertions after completing the request/response
		// dance.
		msg.assert(t)
	}
}

func connect(t *testing.T, s wtserver.Interface, peer *wtmock.MockPeer,
	initMsg *wtwire.Init, timeout time.Duration) {

	t.Helper()

	s.InboundPeerConnected(peer)
	sendMsg(t, initMsg, peer, timeout)
	recvReply(t, "MsgInit", peer, timeout)
}

// sendMsg sends a wtwire.Message message via a wtmock.MockPeer.
func sendMsg(t *testing.T, msg wtwire.Message,
	peer *wtmock.MockPeer, timeout time.Duration) {

	t.Helper()

	var b bytes.Buffer
	_, err := wtwire.WriteMessage(&b, msg, 0)
	if err != nil {
		t.Fatalf("unable to encode %T message: %v",
			msg, err)
	}

	select {
	case peer.IncomingMsgs <- b.Bytes():
	case <-time.After(2 * timeout):
		t.Fatalf("unable to send %T message", msg)
	}
}

// recvReply receives a message from the server, and parses it according to
// expected reply type. The supported replies are CreateSessionReply and
// StateUpdateReply.
func recvReply(t *testing.T, name string, peer *wtmock.MockPeer,
	timeout time.Duration) wtwire.Message {

	t.Helper()

	var (
		msg wtwire.Message
		err error
	)

	select {
	case b := <-peer.OutgoingMsgs:
		msg, err = wtwire.ReadMessage(bytes.NewReader(b), 0)
		if err != nil {
			t.Fatalf("unable to decode server "+
				"reply: %v", err)
		}

	case <-time.After(2 * timeout):
		t.Fatalf("server did not reply")
	}

	switch name {
	case "MsgInit":
		if _, ok := msg.(*wtwire.Init); !ok {
			t.Fatalf("expected %s reply message, "+
				"got %T", name, msg)
		}
	case "MsgCreateSessionReply":
		if _, ok := msg.(*wtwire.CreateSessionReply); !ok {
			t.Fatalf("expected %s reply message, "+
				"got %T", name, msg)
		}
	case "MsgStateUpdateReply":
		if _, ok := msg.(*wtwire.StateUpdateReply); !ok {
			t.Fatalf("expected %s reply message, "+
				"got %T", name, msg)
		}
	case "MsgDeleteSessionReply":
		if _, ok := msg.(*wtwire.DeleteSessionReply); !ok {
			t.Fatalf("expected %s reply message, "+
				"got %T", name, msg)
		}
	}

	return msg
}

// assertConnClosed checks that the peer's connection is closed before the
// timeout expires.
func assertConnClosed(t *testing.T, peer *wtmock.MockPeer, duration time.Duration) {
	t.Helper()

	select {
	case <-peer.Quit:
	case <-time.After(duration):
		t.Fatalf("expected connection to be closed")
	}
}

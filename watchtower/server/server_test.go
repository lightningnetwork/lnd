// +build dev

package server_test

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/server"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// addr is the server's reward address given to watchtower clients.
var addr, _ = btcutil.DecodeAddress(
	"mrX9vMRYLfVy1BnZbc5gZjuyaqH3ZW2ZHz", &chaincfg.TestNet3Params,
)

// randPubKey generates a new secp keypair, and returns the public key.
func randPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	sk, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to generate pubkey: %v", err)
	}

	return sk.PubKey()
}

// initServer creates and starts a new server using the server.DB and timeout.
// If the provided database is nil, a mock db will be used.
func initServer(t *testing.T, db server.DB,
	timeout time.Duration) server.Interface {

	t.Helper()

	if db == nil {
		db = wtdb.NewMockDB()
	}

	s, err := server.New(&server.Config{
		DB:           db,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		NewAddress: func() (btcutil.Address, error) {
			return addr, nil
		},
	})
	if err != nil {
		t.Fatalf("unable to create server: %v", err)
	}

	if err = s.Start(); err != nil {
		t.Fatalf("unable to start server: %v", err)
	}

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
	defer s.Stop()

	// Create two peers using the same session id.
	peerPub := randPubKey(t)
	peer1 := server.NewMockPeer(peerPub, nil, 0)
	peer2 := server.NewMockPeer(peerPub, nil, 0)

	// Serialize a Init message to be sent by both peers.
	init := wtwire.NewInitMessage(
		lnwire.NewRawFeatureVector(), lnwire.NewRawFeatureVector(),
	)

	var b bytes.Buffer
	_, err := wtwire.WriteMessage(&b, init, 0)
	if err != nil {
		t.Fatalf("unable to write message: %v", err)
	}

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
		rejectedPeer *server.MockPeer
		acceptedPeer *server.MockPeer
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
	name        string
	initMsg     *wtwire.Init
	createMsg   *wtwire.CreateSession
	expReply    *wtwire.CreateSessionReply
	expDupReply *wtwire.CreateSessionReply
}

var createSessionTests = []createSessionTestCase{
	{
		name: "reject duplicate session create",
		initMsg: wtwire.NewInitMessage(
			lnwire.NewRawFeatureVector(),
			lnwire.NewRawFeatureVector(),
		),
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   1000,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		expReply: &wtwire.CreateSessionReply{
			Code: wtwire.CodeOK,
			Data: []byte(addr.ScriptAddress()),
		},
		expDupReply: &wtwire.CreateSessionReply{
			Code: wtwire.CreateSessionCodeAlreadyExists,
			Data: []byte(addr.ScriptAddress()),
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
	defer s.Stop()

	// Create a new client and connect to server.
	peerPub := randPubKey(t)
	peer := server.NewMockPeer(peerPub, nil, 0)
	connect(t, i, s, peer, test.initMsg, timeoutDuration)

	// Send the CreateSession message, and wait for a reply.
	sendMsg(t, i, test.createMsg, peer, timeoutDuration)

	reply := recvReply(
		t, i, "CreateSessionReply", peer, timeoutDuration,
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

	// Simulate a peer with the same session id connection to the server
	// again.
	peer = server.NewMockPeer(peerPub, nil, 0)
	connect(t, i, s, peer, test.initMsg, timeoutDuration)

	// Send the _same_ CreateSession message as the first attempt.
	sendMsg(t, i, test.createMsg, peer, timeoutDuration)

	reply = recvReply(
		t, i, "CreateSessionReply", peer, timeoutDuration,
	).(*wtwire.CreateSessionReply)

	// Ensure that the server's reply matches our expected response for a
	// duplicate send.
	if !reflect.DeepEqual(reply, test.expDupReply) {
		t.Fatalf("[test %d] expected reply %v, got %d",
			i, test.expReply, reply)
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
		initMsg: &wtwire.Init{&lnwire.Init{
			LocalFeatures:  lnwire.NewRawFeatureVector(),
			GlobalFeatures: lnwire.NewRawFeatureVector(),
		}},
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   3,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0},
			{SeqNum: 2, LastApplied: 1},
			{SeqNum: 3, LastApplied: 2},
			{SeqNum: 3, LastApplied: 3},
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
		initMsg: &wtwire.Init{&lnwire.Init{
			LocalFeatures:  lnwire.NewRawFeatureVector(),
			GlobalFeatures: lnwire.NewRawFeatureVector(),
		}},
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   4,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 2, LastApplied: 0},
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
		initMsg: &wtwire.Init{&lnwire.Init{
			LocalFeatures:  lnwire.NewRawFeatureVector(),
			GlobalFeatures: lnwire.NewRawFeatureVector(),
		}},
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   4,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0},
			{SeqNum: 2, LastApplied: 0},
			{SeqNum: 1, LastApplied: 0},
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
		initMsg: &wtwire.Init{&lnwire.Init{
			LocalFeatures:  lnwire.NewRawFeatureVector(),
			GlobalFeatures: lnwire.NewRawFeatureVector(),
		}},
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   4,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0},
			{SeqNum: 2, LastApplied: 1},
			{SeqNum: 3, LastApplied: 2},
			{SeqNum: 4, LastApplied: 1},
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
		initMsg: &wtwire.Init{&lnwire.Init{
			LocalFeatures:  lnwire.NewRawFeatureVector(),
			GlobalFeatures: lnwire.NewRawFeatureVector(),
		}},
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   4,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0},
			{SeqNum: 2, LastApplied: 1},
			nil, // Wait for read timeout to drop conn, then reconnect.
			{SeqNum: 3, LastApplied: 2},
			{SeqNum: 4, LastApplied: 3},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			nil,
			{Code: wtwire.CodeOK, LastApplied: 3},
			{Code: wtwire.CodeOK, LastApplied: 4},
		},
	},
	// Valid update sequence with disconnection, ensure resumes resume.
	// Client doesn't echo last applied until last message.
	{
		name: "resume after disconnect lagging lastapplied",
		initMsg: &wtwire.Init{&lnwire.Init{
			LocalFeatures:  lnwire.NewRawFeatureVector(),
			GlobalFeatures: lnwire.NewRawFeatureVector(),
		}},
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   4,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0},
			{SeqNum: 2, LastApplied: 0},
			nil, // Wait for read timeout to drop conn, then reconnect.
			{SeqNum: 3, LastApplied: 0},
			{SeqNum: 4, LastApplied: 3},
		},
		replies: []*wtwire.StateUpdateReply{
			{Code: wtwire.CodeOK, LastApplied: 1},
			{Code: wtwire.CodeOK, LastApplied: 2},
			nil,
			{Code: wtwire.CodeOK, LastApplied: 3},
			{Code: wtwire.CodeOK, LastApplied: 4},
		},
	},
	// Send update with sequence number that exceeds MaxUpdates.
	{
		name: "seqnum exceed maxupdates",
		initMsg: &wtwire.Init{&lnwire.Init{
			LocalFeatures:  lnwire.NewRawFeatureVector(),
			GlobalFeatures: lnwire.NewRawFeatureVector(),
		}},
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   3,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 1, LastApplied: 0},
			{SeqNum: 2, LastApplied: 1},
			{SeqNum: 3, LastApplied: 2},
			{SeqNum: 4, LastApplied: 3},
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
		initMsg: &wtwire.Init{&lnwire.Init{
			LocalFeatures:  lnwire.NewRawFeatureVector(),
			GlobalFeatures: lnwire.NewRawFeatureVector(),
		}},
		createMsg: &wtwire.CreateSession{
			BlobVersion:  0,
			MaxUpdates:   3,
			RewardRate:   0,
			SweepFeeRate: 1,
		},
		updates: []*wtwire.StateUpdate{
			{SeqNum: 0, LastApplied: 0},
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

	for i, test := range stateUpdateTests {
		t.Run(test.name, func(t *testing.T) {
			testServerStateUpdates(t, i, test)
		})
	}
}

func testServerStateUpdates(t *testing.T, i int, test stateUpdateTestCase) {
	const timeoutDuration = 100 * time.Millisecond

	s := initServer(t, nil, timeoutDuration)
	defer s.Stop()

	// Create a new client and connect to the server.
	peerPub := randPubKey(t)
	peer := server.NewMockPeer(peerPub, nil, 0)
	connect(t, i, s, peer, test.initMsg, timeoutDuration)

	// Register a session for this client to use in the subsequent tests.
	sendMsg(t, i, test.createMsg, peer, timeoutDuration)
	initReply := recvReply(
		t, i, "CreateSessionReply", peer, timeoutDuration,
	).(*wtwire.CreateSessionReply)

	// Fail if the server rejected our proposed CreateSession message.
	if initReply.Code != wtwire.CodeOK {
		t.Fatalf("[test %d] server rejected session init", i)
	}

	// Check that the server closed the connection used to register the
	// session.
	assertConnClosed(t, peer, 2*timeoutDuration)

	// Now that the original connection has been closed, connect a new
	// client with the same session id.
	peer = server.NewMockPeer(peerPub, nil, 0)
	connect(t, i, s, peer, test.initMsg, timeoutDuration)

	// Send the intended StateUpdate messages in series.
	for j, update := range test.updates {
		// A nil update signals that we should wait for the prior
		// connection to die, before re-register with the same session
		// identifier.
		if update == nil {
			assertConnClosed(t, peer, 2*timeoutDuration)

			peer = server.NewMockPeer(peerPub, nil, 0)
			connect(t, i, s, peer, test.initMsg, timeoutDuration)

			continue
		}

		// Send the state update and verify it against our expected
		// response.
		sendMsg(t, i, update, peer, timeoutDuration)
		reply := recvReply(
			t, i, "StateUpdateReply", peer, timeoutDuration,
		).(*wtwire.StateUpdateReply)

		if !reflect.DeepEqual(reply, test.replies[j]) {
			t.Fatalf("[test %d, update %d] expected reply "+
				"%v, got %d", i, j,
				test.replies[j], reply)
		}
	}

	// Check that the final connection is properly cleaned up by the server.
	assertConnClosed(t, peer, 2*timeoutDuration)
}

func connect(t *testing.T, i int, s server.Interface, peer *server.MockPeer,
	initMsg *wtwire.Init, timeout time.Duration) {

	s.InboundPeerConnected(peer)
	sendMsg(t, i, initMsg, peer, timeout)
	recvReply(t, i, "Init", peer, timeout)
}

// sendMsg sends a wtwire.Message message via a server.MockPeer.
func sendMsg(t *testing.T, i int, msg wtwire.Message,
	peer *server.MockPeer, timeout time.Duration) {

	t.Helper()

	var b bytes.Buffer
	_, err := wtwire.WriteMessage(&b, msg, 0)
	if err != nil {
		t.Fatalf("[test %d] unable to encode %T message: %v",
			i, msg, err)
	}

	select {
	case peer.IncomingMsgs <- b.Bytes():
	case <-time.After(2 * timeout):
		t.Fatalf("[test %d] unable to send %T message", i, msg)
	}
}

// recvReply receives a message from the server, and parses it according to
// expected reply type. The supported replies are CreateSessionReply and
// StateUpdateReply.
func recvReply(t *testing.T, i int, name string,
	peer *server.MockPeer, timeout time.Duration) wtwire.Message {

	t.Helper()

	var (
		msg wtwire.Message
		err error
	)

	select {
	case b := <-peer.OutgoingMsgs:
		msg, err = wtwire.ReadMessage(bytes.NewReader(b), 0)
		if err != nil {
			t.Fatalf("[test %d] unable to decode server "+
				"reply: %v", i, err)
		}

	case <-time.After(2 * timeout):
		t.Fatalf("[test %d] server did not reply", i)
	}

	switch name {
	case "Init":
		if _, ok := msg.(*wtwire.Init); !ok {
			t.Fatalf("[test %d] expected %s reply "+
				"message, got %T", i, name, msg)
		}
	case "CreateSessionReply":
		if _, ok := msg.(*wtwire.CreateSessionReply); !ok {
			t.Fatalf("[test %d] expected %s reply "+
				"message, got %T", i, name, msg)
		}
	case "StateUpdateReply":
		if _, ok := msg.(*wtwire.StateUpdateReply); !ok {
			t.Fatalf("[test %d] expected %s reply "+
				"message, got %T", i, name, msg)
		}
	}

	return msg
}

// assertConnClosed checks that the peer's connection is closed before the
// timeout expires.
func assertConnClosed(t *testing.T, peer *server.MockPeer, duration time.Duration) {
	t.Helper()

	select {
	case <-peer.Quit:
	case <-time.After(duration):
		t.Fatalf("expected connection to be closed")
	}
}

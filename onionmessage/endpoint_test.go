package onionmessage

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/stretchr/testify/require"
)

// endpointHarness wires up the minimal components required to exercise
// SendMessage end-to-end.
type endpointHarness struct {
	endpoint     *OnionEndpoint
	server       *subscribe.Server
	client       *subscribe.Client
	actorSystem  *actor.ActorSystem
	receptionist *actor.Receptionist
	resolver     *mockNodeIDResolver
	router       *sphinx.Router
	nodeKey      *btcec.PrivateKey
}

func newEndpointHarness(t *testing.T) *endpointHarness {
	t.Helper()

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	router := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: nodeKey},
		sphinx.NewMemoryReplayLog(),
	)
	router.Start()

	server := subscribe.NewServer()
	require.NoError(t, server.Start())

	client, err := server.Subscribe()
	require.NoError(t, err)

	actorSystem := actor.NewActorSystem()
	receptionist := actorSystem.Receptionist()
	resolver := newMockNodeIDResolver()

	endpoint, err := NewOnionEndpoint(
		receptionist, router, resolver, WithMessageServer(server),
	)
	require.NoError(t, err)

	return &endpointHarness{
		endpoint:     endpoint,
		server:       server,
		client:       client,
		actorSystem:  actorSystem,
		receptionist: receptionist,
		resolver:     resolver,
		router:       router,
		nodeKey:      nodeKey,
	}
}

func (h *endpointHarness) cleanup() {
	h.client.Cancel()
	_ = h.server.Stop()
	h.router.Stop()
	_ = h.actorSystem.Shutdown()
}

func encodeBlindedRouteData(t *testing.T,
	data *record.BlindedRouteData) []byte {
	t.Helper()

	buf, err := record.EncodeBlindedRouteData(data)
	require.NoError(t, err)

	return buf
}

func pubKeyToArray(pk *btcec.PublicKey) [33]byte {
	var out [33]byte
	copy(out[:], pk.SerializeCompressed())
	return out
}

func requireUpdate(t *testing.T, c *subscribe.Client) *OnionMessageUpdate {
	t.Helper()

	select {
	case raw := <-c.Updates():
		u, ok := raw.(*OnionMessageUpdate)
		require.True(t, ok, "unexpected update type")
		return u
	case <-time.After(time.Second):
		require.FailNow(t, "no update received")
		return nil
	}
}

func requireNoUpdate(t *testing.T, c *subscribe.Client) {
	t.Helper()

	select {
	case <-c.Updates():
		require.FailNow(t, "unexpected update received")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestOnionEndpointSendMessageForwardNextNode(t *testing.T) {
	h := newEndpointHarness(t)
	defer h.cleanup()

	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	forwarded := make(chan *lnwire.OnionMessage, 1)
	SpawnOnionPeerActor(
		h.actorSystem,
		func(msg *lnwire.OnionMessage) { forwarded <- msg },
		pubKeyToArray(nextNodePub),
	)

	nextNode := fn.NewLeft[*btcec.PublicKey, lnwire.ShortChannelID](
		nextNodePub,
	)
	rdA := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	rdB := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: encodeBlindedRouteData(t, rdA)},
		{NodePub: nextNodePub, PlainText: encodeBlindedRouteData(t, rdB)},
	}

	onionMsg, cipherText := buildOnionMessage(t, hops)

	msg := msgmux.PeerMsg{
		Message: onionMsg,
		PeerPub: *nextNodePub,
	}

	require.True(t, h.endpoint.SendMessage(context.Background(), msg))

	update := requireUpdate(t, h.client)
	require.Equal(t, pubKeyToArray(nextNodePub), update.Peer)
	require.Equal(t, onionMsg.OnionBlob, update.OnionBlob)
	require.Equal(t, cipherText, update.EncryptedRecipientData)

	select {
	case forwardedMsg := <-forwarded:
		require.NotNil(t, forwardedMsg)
		require.NotEmpty(t, forwardedMsg.OnionBlob)
	case <-time.After(time.Second):
		require.FailNow(t, "forwarded message not received")
	}
}

func TestOnionEndpointSendMessageForwardSCID(t *testing.T) {
	h := newEndpointHarness(t)
	defer h.cleanup()

	nextNodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	nextNodePub := nextNodeKey.PubKey()

	scid := lnwire.NewShortChanIDFromInt(555)
	h.resolver.addPeer(scid, nextNodePub)

	forwarded := make(chan *lnwire.OnionMessage, 1)
	SpawnOnionPeerActor(
		h.actorSystem,
		func(msg *lnwire.OnionMessage) { forwarded <- msg },
		pubKeyToArray(nextNodePub),
	)

	nextNode := fn.NewRight[*btcec.PublicKey, lnwire.ShortChannelID](scid)
	rdA := record.NewNonFinalBlindedRouteDataOnionMessage(
		nextNode, nil, nil,
	)
	rdB := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: encodeBlindedRouteData(t, rdA)},
		{NodePub: nextNodePub, PlainText: encodeBlindedRouteData(t, rdB)},
	}

	onionMsg, cipherText := buildOnionMessage(t, hops)

	msg := msgmux.PeerMsg{
		Message: onionMsg,
		PeerPub: *nextNodePub,
	}

	require.True(t, h.endpoint.SendMessage(context.Background(), msg))

	update := requireUpdate(t, h.client)
	require.Equal(t, pubKeyToArray(nextNodePub), update.Peer)
	require.Equal(t, onionMsg.OnionBlob, update.OnionBlob)
	require.Equal(t, cipherText, update.EncryptedRecipientData)

	select {
	case forwardedMsg := <-forwarded:
		require.NotNil(t, forwardedMsg)
	case <-time.After(time.Second):
		require.FailNow(t, "forwarded message not received")
	}
}

func TestOnionEndpointSendMessageDeliver(t *testing.T) {
	h := newEndpointHarness(t)
	defer h.cleanup()

	rd := &record.BlindedRouteData{}

	hops := []*sphinx.HopInfo{
		{NodePub: h.nodeKey.PubKey(), PlainText: encodeBlindedRouteData(t, rd)},
	}

	onionMsg, cipherText := buildOnionMessage(t, hops)

	msg := msgmux.PeerMsg{
		Message: onionMsg,
		PeerPub: *h.nodeKey.PubKey(),
	}

	require.True(t, h.endpoint.SendMessage(context.Background(), msg))

	update := requireUpdate(t, h.client)
	require.Equal(t, pubKeyToArray(h.nodeKey.PubKey()), update.Peer)
	require.Equal(t, onionMsg.OnionBlob, update.OnionBlob)
	require.Equal(t, cipherText, update.EncryptedRecipientData)

	custom := update.CustomRecords[uint64(lnwire.InvoiceRequestNamespaceType)]
	require.Equal(t, []byte{1, 2, 3}, custom)
}

func TestOnionEndpointSendMessageProcessError(t *testing.T) {
	h := newEndpointHarness(t)
	defer h.cleanup()

	// Provide an invalid onion blob so processOnionMessage fails to decode.
	onionMsg := &lnwire.OnionMessage{
		PathKey:   h.nodeKey.PubKey(),
		OnionBlob: []byte{1, 2, 3},
	}

	msg := msgmux.PeerMsg{
		Message: onionMsg,
		PeerPub: *h.nodeKey.PubKey(),
	}

	require.False(t, h.endpoint.SendMessage(context.Background(), msg))
	requireNoUpdate(t, h.client)
}

func TestOnionEndpointCanHandle(t *testing.T) {
	t.Parallel()

	endpoint := &OnionEndpoint{}

	t.Run("onion message", func(t *testing.T) {
		t.Parallel()

		msg := msgmux.PeerMsg{Message: &lnwire.OnionMessage{}}
		require.True(t, endpoint.CanHandle(msg))
	})

	t.Run("non onion message", func(t *testing.T) {
		t.Parallel()

		msg := msgmux.PeerMsg{Message: &lnwire.Ping{}}
		require.False(t, endpoint.CanHandle(msg))
	})
}

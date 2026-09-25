package gossipsync

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/actor/timeout"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// recordingConn is a PeerConn that records what it's sent.
type recordingConn struct {
	mu   sync.Mutex
	sent []lnwire.Message
}

// PubKey returns a fixed key.
func (c *recordingConn) PubKey() [33]byte {
	return [33]byte{7}
}

// SendMessageLazy records msgs.
func (c *recordingConn) SendMessageLazy(_ bool, msgs ...lnwire.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msgs...)

	return nil
}

// count returns how many messages were sent.
func (c *recordingConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.sent)
}

// gatedGraph is a simGraph whose FilterChannelRange blocks until its gate
// opens, so a test can hold the responder inside a query.
type gatedGraph struct {
	*simGraph
	entered chan struct{}
	gate    chan struct{}
}

// FilterChannelRange signals entry, then waits for the gate.
func (g *gatedGraph) FilterChannelRange(chain chainhash.Hash, start,
	end uint32, ts bool) ([]graphdb.BlockChannelRange, error) {

	g.entered <- struct{}{}
	<-g.gate

	return g.simGraph.FilterChannelRange(chain, start, end, ts)
}

// TestResponderReleasesTokenBetweenPages asserts that a backlog replay
// returns its filter token after each page, so a peer's query answered
// between pages doesn't hold a token that other peers' replays need, and that
// the replay still delivers its whole backlog afterward.
func TestResponderReleasesTokenBetweenPages(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()

		system := actor.NewActorSystem()
		t.Cleanup(func() { require.NoError(t, system.Shutdown()) })

		timeouts := timeout.NewActor()
		timeoutRef, err := actor.RegisterWithSystem(
			system, "timeout",
			actor.NewServiceKey[timeout.Msg, timeout.Resp](
				"timeout",
			),
			timeouts,
		)
		require.NoError(t, err)
		timeouts.Start(timeoutRef)

		// A backlog of three pages: 30 channels, each an
		// announcement and two updates.
		graph := &gatedGraph{
			simGraph: newSimGraph(),
			entered:  make(chan struct{}, 1),
			gate:     make(chan struct{}),
		}
		now := time.Now()
		for i := range 30 {
			graph.addChannel(lnwire.ShortChannelID{
				BlockHeight: uint32(100 + i),
			}, now)
		}

		cfg := &ResponderConfig{
			ChainHash:    simChain,
			Graph:        graph,
			Encoding:     lnwire.EncodingSortedPlain,
			ChunkSize:    DefaultRangeChunkSize,
			FilterTokens: make(chan struct{}, 1),
			PageSize:     32,
		}
		cfg.FilterTokens <- struct{}{}

		conn := &recordingConn{}
		behavior := newResponderActor(
			cfg, route.Vertex{7}, 1, &sender{conn: conn},
			timeoutRef, func(int, func(i, j int)) {},
		)
		ref, err := responderKey(route.Vertex{7}).Spawn(
			system, "responder", behavior,
		)
		require.NoError(t, err)
		behavior.self = ref

		// The peer sets a filter covering the backlog, then queries
		// our channel range before the second page is sent.
		filter := &lnwire.GossipTimestampRange{
			ChainHash:      simChain,
			FirstTimestamp: uint32(now.Add(-time.Hour).Unix()),
			TimestampRange: ^uint32(0),
		}
		ref.Tell(ctx, &peerFilterMsg{filter: filter})
		ref.Tell(ctx, &peerQueryMsg{query: &lnwire.QueryChannelRange{
			ChainHash: simChain, NumBlocks: 1_000,
		}})

		// While the query is being answered, at least one whole page
		// has been sent and the token is back in the pool. The actor
		// may have sent a second page before the query reached its
		// mailbox, so only whole pages are asserted.
		<-graph.entered
		sent := conn.count()
		require.Positive(t, sent)
		require.Less(t, sent, 90)
		require.Zero(t, sent%32)
		require.Len(t, cfg.FilterTokens, 1,
			"replay held its token across a query")

		// Once the query is answered, the replay finishes: 90 backlog
		// messages plus the one range reply.
		close(graph.gate)
		synctest.Wait()
		require.Equal(t, 91, conn.count())
		require.Len(t, cfg.FilterTokens, 1)
	})
}

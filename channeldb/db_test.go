package channeldb

import (
	"image/color"
	"math/rand"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	testAddr = &net.TCPAddr{IP: (net.IP)([]byte{0xA, 0x0, 0x0, 0x1}),
		Port: 9000}
	anotherAddr, _ = net.ResolveTCPAddr("tcp",
		"[2001:db8:85a3:0:0:8a2e:370:7334]:80")
	testAddrs = []net.Addr{testAddr}

	testFeatures = lnwire.NewFeatureVector(nil, lnwire.Features)
)

func TestOpenWithCreate(t *testing.T) {
	t.Parallel()

	// Checking for db file existence is not possible with postgres.
	if kvdb.PostgresBackend {
		t.Skip()
	}

	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName := t.TempDir()

	// Next, open thereby creating channeldb for the first time.
	dbPath := filepath.Join(tempDirName, "cdb")
	backend, cleanup, err := kvdb.GetTestBackend(dbPath, "cdb")
	require.NoError(t, err, "unable to get test db backend")
	t.Cleanup(cleanup)

	cdb, err := CreateWithBackend(backend)
	require.NoError(t, err, "unable to create channeldb")
	if err := cdb.Close(); err != nil {
		t.Fatalf("unable to close channeldb: %v", err)
	}

	// The path should have been successfully created.
	if !fileExists(dbPath) {
		t.Fatalf("channeldb failed to create data directory")
	}

	// Now, reopen the same db in dry run migration mode. Since we have not
	// applied any migrations, this should ignore the flag and not fail.
	OpenForTesting(t, dbPath, OptionDryRunMigration(true))
}

// TestWipe tests that the database wipe operation completes successfully
// and that the buckets are deleted. It also checks that attempts to fetch
// information while the buckets are not set return the correct errors.
func TestWipe(t *testing.T) {
	t.Parallel()

	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName := t.TempDir()

	// Next, open thereby creating channeldb for the first time.
	dbPath := filepath.Join(tempDirName, "cdb")
	backend, cleanup, err := kvdb.GetTestBackend(dbPath, "cdb")
	require.NoError(t, err, "unable to get test db backend")
	t.Cleanup(cleanup)

	fullDB, err := CreateWithBackend(backend)
	require.NoError(t, err, "unable to create channeldb")
	defer fullDB.Close()

	if err := fullDB.Wipe(); err != nil {
		t.Fatalf("unable to wipe channeldb: %v", err)
	}

	cdb := fullDB.ChannelStateDB()

	// Check correct errors are returned
	openChannels, err := cdb.FetchAllOpenChannels()
	require.NoError(t, err, "fetching open channels")
	require.Equal(t, 0, len(openChannels))

	closedChannels, err := cdb.FetchClosedChannels(false)
	require.NoError(t, err, "fetching closed channels")
	require.Equal(t, 0, len(closedChannels))
}

// TestMultiSourceAddrsForNode tests the we're able to properly obtain all the
// addresses for a target node from multiple backends - in this case, the
// channel db and graph db.
func TestMultiSourceAddrsForNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	graph := newMockAddrSource(t)
	t.Cleanup(func() {
		graph.AssertExpectations(t)
	})

	// We'll make a test vertex, but this node will only have half the
	// number of addresses it usually does.
	testNode := createTestVertex(t)
	nodePub, err := testNode.PubKey()
	require.NoError(t, err)
	graph.On("AddrsForNode", ctx, nodePub).Return(
		true, []net.Addr{testAddr}, nil,
	).Once()

	// Next, we'll make a link node with the same pubkey, but with an
	// additional address.
	linkNode := NewLinkNode(
		fullDB.channelStateDB.linkNodeDB, wire.MainNet, nodePub,
		anotherAddr,
	)
	require.NoError(t, linkNode.Sync())

	// Create a multi-backend address source from the channel db and graph
	// db.
	addrSource := NewMultiAddrSource(fullDB, graph)

	// Now that we've created a link node, as well as a vertex for the
	// node, we'll query for all its addresses.
	known, nodeAddrs, err := addrSource.AddrsForNode(ctx, nodePub)
	require.NoError(t, err, "unable to obtain node addrs")
	require.True(t, known)

	expectedAddrs := make(map[string]struct{})
	expectedAddrs[testAddr.String()] = struct{}{}
	expectedAddrs[anotherAddr.String()] = struct{}{}

	// Finally, ensure that all the expected addresses are found.
	require.Len(t, nodeAddrs, len(expectedAddrs))

	for _, addr := range nodeAddrs {
		require.Contains(t, expectedAddrs, addr.String())
	}
}

// TestAbandonChannel tests that the AbandonChannel method is able to properly
// remove a channel from the database and add a close channel summary. If
// called after a channel has already been removed, the method shouldn't return
// an error.
func TestAbandonChannel(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// If we attempt to abandon the state of a channel that doesn't exist
	// in the open or closed channel bucket, then we should receive an
	// error.
	err = cdb.AbandonChannel(&wire.OutPoint{}, 0)
	if err == nil {
		t.Fatalf("removing non-existent channel should have failed")
	}

	// We'll now create a new channel in a pending state to abandon
	// shortly.
	chanState := createTestChannel(t, cdb)

	// We should now be able to abandon the channel without any errors.
	closeHeight := uint32(11)
	err = cdb.AbandonChannel(&chanState.FundingOutpoint, closeHeight)
	require.NoError(t, err, "unable to abandon channel")

	// At this point, the channel should no longer be found in the set of
	// open channels.
	_, err = cdb.FetchChannel(chanState.FundingOutpoint)
	if err != ErrChannelNotFound {
		t.Fatalf("channel should not have been found: %v", err)
	}

	// However we should be able to retrieve a close channel summary for
	// the channel.
	_, err = cdb.FetchClosedChannel(&chanState.FundingOutpoint)
	require.NoError(t, err, "unable to fetch closed channel")

	// Finally, if we attempt to abandon the channel again, we should get a
	// nil error as the channel has already been abandoned.
	err = cdb.AbandonChannel(&chanState.FundingOutpoint, closeHeight)
	require.NoError(t, err, "unable to abandon channel")
}

func createNode(priv *btcec.PrivateKey) *models.Node {
	updateTime := rand.Int63()

	pub := priv.PubKey().SerializeCompressed()
	n := models.NewV1Node(
		route.NewVertex(priv.PubKey()),
		&models.NodeV1Fields{
			AuthSigBytes: testSig.Serialize(),
			LastUpdate:   time.Unix(updateTime, 0),
			Color:        color.RGBA{1, 2, 3, 0},
			Alias:        "kek" + string(pub),
			Features:     testFeatures.RawFeatureVector,
			Addresses:    testAddrs,
		},
	)

	return n
}

func createTestVertex(t *testing.T) *models.Node {
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return createNode(priv)
}

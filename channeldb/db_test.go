package channeldb

import (
	"image/color"
	"math"
	"math/rand"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/shachain"
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

func genRandomChannelShell() (*ChannelShell, error) {
	var testPriv [32]byte
	if _, err := rand.Read(testPriv[:]); err != nil {
		return nil, err
	}

	_, pub := btcec.PrivKeyFromBytes(testPriv[:])

	var chanPoint wire.OutPoint
	if _, err := rand.Read(chanPoint.Hash[:]); err != nil {
		return nil, err
	}

	chanPoint.Index = uint32(rand.Intn(math.MaxUint16))

	chanStatus := ChanStatusDefault | ChanStatusRestored

	var shaChainPriv [32]byte
	if _, err := rand.Read(testPriv[:]); err != nil {
		return nil, err
	}
	revRoot, err := chainhash.NewHash(shaChainPriv[:])
	if err != nil {
		return nil, err
	}
	shaChainProducer := shachain.NewRevocationProducer(*revRoot)

	commitParams := CommitmentParams{
		CsvDelay: uint16(rand.Int63()),
	}

	channel := &OpenChannel{
		ChainHash:       rev,
		FundingOutpoint: chanPoint,
		ShortChannelID: lnwire.NewShortChanIDFromInt(
			uint64(rand.Int63()),
		),
		IdentityPub: pub,
		LocalChanCfg: ChannelConfig{
			CommitmentParams: commitParams,
			PaymentBasePoint: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: keychain.KeyFamily(
						rand.Int63(),
					),
					Index: uint32(rand.Int63()),
				},
			},
		},
		RemoteCurrentRevocation: pub,
		IsPending:               false,
		RevocationStore:         shachain.NewRevocationStore(),
		RevocationProducer:      shaChainProducer,
	}
	channel.SetChannelStatusForStore(chanStatus)

	return &ChannelShell{
		NodeAddrs: []net.Addr{&net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 18555,
		}},
		Chan: channel,
	}, nil
}

// TestRestoreChannelShells tests that we're able to insert a partially channel
// populated to disk. This is useful for channel recovery purposes. We should
// find the new channel shell on disk, and also the db should be populated with
// an edge for that channel.
func TestRestoreChannelShells(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// First, we'll make our channel shell, it will only have the minimal
	// amount of information required for us to initiate the data loss
	// protection feature.
	channelShell, err := genRandomChannelShell()
	require.NoError(t, err, "unable to gen channel shell")

	// With the channel shell constructed, we'll now insert it into the
	// database with the restoration method.
	if err := cdb.RestoreChannelShells(channelShell); err != nil {
		t.Fatalf("unable to restore channel shell: %v", err)
	}

	// Now that the channel has been inserted, we'll attempt to query for
	// it to ensure we can properly locate it via various means.
	//
	// First, we'll attempt to query for all channels that we have with the
	// node public key that was restored.
	nodeChans, err := cdb.FetchOpenChannels(channelShell.Chan.IdentityPub)
	require.NoError(t, err, "unable find channel")

	// We should now find a single channel from the database.
	if len(nodeChans) != 1 {
		t.Fatalf("unable to find restored channel by node "+
			"pubkey: %v", err)
	}

	// Ensure that it isn't possible to modify the commitment state machine
	// of this restored channel.
	channel := nodeChans[0]
	_, err = channel.UpdateCommitment(nil, nil)
	if err != ErrNoRestoredChannelMutation {
		t.Fatalf("able to mutate restored channel")
	}
	err = channel.AppendRemoteCommitChain(nil)
	if err != ErrNoRestoredChannelMutation {
		t.Fatalf("able to mutate restored channel")
	}
	err = channel.AdvanceCommitChainTail(
		nil, nil, dummyLocalOutputIndex, dummyRemoteOutIndex,
	)
	if err != ErrNoRestoredChannelMutation {
		t.Fatalf("able to mutate restored channel")
	}

	// That single channel should have the proper channel point, and also
	// the expected set of flags to indicate that it was a restored
	// channel.
	if nodeChans[0].FundingOutpoint != channelShell.Chan.FundingOutpoint {
		t.Fatalf("wrong funding outpoint: expected %v, got %v",
			nodeChans[0].FundingOutpoint,
			channelShell.Chan.FundingOutpoint)
	}
	if !nodeChans[0].HasChanStatus(ChanStatusRestored) {
		t.Fatalf("node has wrong status flags: %v",
			nodeChans[0].ChanStatus())
	}

	// We should also be able to find the channel if we query for it
	// directly.
	_, err = cdb.FetchChannel(channelShell.Chan.FundingOutpoint)
	require.NoError(t, err, "unable to fetch channel")

	// We should also be able to find the link node that was inserted by
	// its public key.
	linkNode, err := fullDB.channelStateDB.linkNodeDB.FetchLinkNode(
		channelShell.Chan.IdentityPub,
	)
	require.NoError(t, err, "unable to fetch link node")

	// The node should have the same address, as specified in the channel
	// shell.
	if reflect.DeepEqual(linkNode.Addresses, channelShell.NodeAddrs) {
		t.Fatalf("addr mismatch: expected %v, got %v",
			linkNode.Addresses, channelShell.NodeAddrs)
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

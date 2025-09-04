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
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntypes"
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

// TestFetchClosedChannelForID tests that we are able to properly retrieve a
// ChannelCloseSummary from the DB given a ChannelID.
func TestFetchClosedChannelForID(t *testing.T) {
	t.Parallel()

	const numChans = 101

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create the test channel state, that we will mutate the index of the
	// funding point.
	state := createTestChannelState(t, cdb)

	// Now run through the number of channels, and modify the outpoint index
	// to create new channel IDs.
	for i := uint32(0); i < numChans; i++ {
		// Save the open channel to disk.
		state.FundingOutpoint.Index = i

		// Write the channel to disk in a pending state.
		createTestChannel(
			t, cdb,
			fundingPointOption(state.FundingOutpoint),
			openChannelOption(),
		)

		// Close the channel. To make sure we retrieve the correct
		// summary later, we make them differ in the SettledBalance.
		closeSummary := &ChannelCloseSummary{
			ChanPoint:      state.FundingOutpoint,
			RemotePub:      state.IdentityPub,
			SettledBalance: btcutil.Amount(500 + i),
		}
		if err := state.CloseChannel(closeSummary); err != nil {
			t.Fatalf("unable to close channel: %v", err)
		}
	}

	// Now run though them all again and make sure we are able to retrieve
	// summaries from the DB.
	for i := uint32(0); i < numChans; i++ {
		state.FundingOutpoint.Index = i

		// We calculate the ChannelID and use it to fetch the summary.
		cid := lnwire.NewChanIDFromOutPoint(state.FundingOutpoint)
		fetchedSummary, err := cdb.FetchClosedChannelForID(cid)
		if err != nil {
			t.Fatalf("unable to fetch close summary: %v", err)
		}

		// Make sure we retrieved the correct one by checking the
		// SettledBalance.
		if fetchedSummary.SettledBalance != btcutil.Amount(500+i) {
			t.Fatalf("summaries don't match: expected %v got %v",
				btcutil.Amount(500+i),
				fetchedSummary.SettledBalance)
		}
	}

	// As a final test we make sure that we get ErrClosedChannelNotFound
	// for a ChannelID we didn't add to the DB.
	state.FundingOutpoint.Index++
	cid := lnwire.NewChanIDFromOutPoint(state.FundingOutpoint)
	_, err = cdb.FetchClosedChannelForID(cid)
	if err != ErrClosedChannelNotFound {
		t.Fatalf("expected ErrClosedChannelNotFound, instead got: %v", err)
	}
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

// TestFetchChannel tests that we're able to fetch an arbitrary channel from
// disk.
func TestFetchChannel(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create an open channel.
	channelState := createTestChannel(t, cdb, openChannelOption())

	// Next, attempt to fetch the channel by its chan point.
	dbChannel, err := cdb.FetchChannel(channelState.FundingOutpoint)
	require.NoError(t, err, "unable to fetch channel")

	// The decoded channel state should be identical to what we stored
	// above.
	require.Equal(t, channelState, dbChannel)

	// Next, attempt to fetch the channel by its channel ID.
	chanID := lnwire.NewChanIDFromOutPoint(channelState.FundingOutpoint)
	dbChannel, err = cdb.FetchChannelByID(nil, chanID)
	require.NoError(t, err, "unable to fetch channel")

	// The decoded channel state should be identical to what we stored
	// above.
	require.Equal(t, channelState, dbChannel)

	// If we attempt to query for a non-existent channel, then we should
	// get an error.
	channelState2 := createTestChannelState(t, cdb)
	require.NoError(t, err, "unable to create channel state")

	uniqueOutputIndex.Add(1)
	channelState2.FundingOutpoint.Index = uniqueOutputIndex.Load()

	_, err = cdb.FetchChannel(channelState2.FundingOutpoint)
	require.ErrorIs(t, err, ErrChannelNotFound)

	chanID2 := lnwire.NewChanIDFromOutPoint(channelState2.FundingOutpoint)
	_, err = cdb.FetchChannelByID(nil, chanID2)
	require.ErrorIs(t, err, ErrChannelNotFound)
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

	return &ChannelShell{
		NodeAddrs: []net.Addr{&net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 18555,
		}},
		Chan: &OpenChannel{
			chanStatus:      chanStatus,
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
						Family: keychain.KeyFamily(rand.Int63()),
						Index:  uint32(rand.Int63()),
					},
				},
			},
			RemoteCurrentRevocation: pub,
			IsPending:               false,
			RevocationStore:         shachain.NewRevocationStore(),
			RevocationProducer:      shaChainProducer,
		},
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
			nodeChans[0].chanStatus)
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

// TestFetchChannels tests the filtering of open channels in fetchChannels.
// It tests the case where no filters are provided (which is equivalent to
// FetchAllOpenChannels) and every combination of pending and waiting close.
func TestFetchChannels(t *testing.T) {
	// Create static channel IDs for each kind of channel retrieved by
	// fetchChannels so that the expected channel IDs can be set in tests.
	var (
		// Pending is a channel that is pending open, and has not had
		// a close initiated.
		pendingChan = lnwire.NewShortChanIDFromInt(1)

		// pendingWaitingClose is a channel that is pending open and
		// has has its closing transaction broadcast.
		pendingWaitingChan = lnwire.NewShortChanIDFromInt(2)

		// openChan is a channel that has confirmed on chain.
		openChan = lnwire.NewShortChanIDFromInt(3)

		// openWaitingChan is a channel that has confirmed on chain,
		// and it waiting for its close transaction to confirm.
		openWaitingChan = lnwire.NewShortChanIDFromInt(4)
	)

	tests := []struct {
		name             string
		filters          []fetchChannelsFilter
		expectedChannels map[lnwire.ShortChannelID]bool
	}{
		{
			name:    "get all channels",
			filters: []fetchChannelsFilter{},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingChan:        true,
				pendingWaitingChan: true,
				openChan:           true,
				openWaitingChan:    true,
			},
		},
		{
			name: "pending channels",
			filters: []fetchChannelsFilter{
				pendingChannelFilter(true),
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingChan:        true,
				pendingWaitingChan: true,
			},
		},
		{
			name: "open channels",
			filters: []fetchChannelsFilter{
				pendingChannelFilter(false),
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				openChan:        true,
				openWaitingChan: true,
			},
		},
		{
			name: "waiting close channels",
			filters: []fetchChannelsFilter{
				waitingCloseFilter(true),
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingWaitingChan: true,
				openWaitingChan:    true,
			},
		},
		{
			name: "not waiting close channels",
			filters: []fetchChannelsFilter{
				waitingCloseFilter(false),
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingChan: true,
				openChan:    true,
			},
		},
		{
			name: "pending waiting",
			filters: []fetchChannelsFilter{
				pendingChannelFilter(true),
				waitingCloseFilter(true),
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingWaitingChan: true,
			},
		},
		{
			name: "pending, not waiting",
			filters: []fetchChannelsFilter{
				pendingChannelFilter(true),
				waitingCloseFilter(false),
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				pendingChan: true,
			},
		},
		{
			name: "open waiting",
			filters: []fetchChannelsFilter{
				pendingChannelFilter(false),
				waitingCloseFilter(true),
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				openWaitingChan: true,
			},
		},
		{
			name: "open, not waiting",
			filters: []fetchChannelsFilter{
				pendingChannelFilter(false),
				waitingCloseFilter(false),
			},
			expectedChannels: map[lnwire.ShortChannelID]bool{
				openChan: true,
			},
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fullDB, err := MakeTestDB(t)
			if err != nil {
				t.Fatalf("unable to make test "+
					"database: %v", err)
			}

			cdb := fullDB.ChannelStateDB()

			// Create a pending channel that is not awaiting close.
			createTestChannel(
				t, cdb, channelIDOption(pendingChan),
			)

			// Create a pending channel which has has been marked as
			// broadcast, indicating that its closing transaction is
			// waiting to confirm.
			pendingClosing := createTestChannel(
				t, cdb,
				channelIDOption(pendingWaitingChan),
			)

			err = pendingClosing.MarkCoopBroadcasted(
				nil, lntypes.Local,
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Create a open channel that is not awaiting close.
			createTestChannel(
				t, cdb,
				channelIDOption(openChan),
				openChannelOption(),
			)

			// Create a open channel which has has been marked as
			// broadcast, indicating that its closing transaction is
			// waiting to confirm.
			openClosing := createTestChannel(
				t, cdb,
				channelIDOption(openWaitingChan),
				openChannelOption(),
			)
			err = openClosing.MarkCoopBroadcasted(
				nil, lntypes.Local,
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			channels, err := fetchChannels(cdb, test.filters...)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(channels) != len(test.expectedChannels) {
				t.Fatalf("expected: %v channels, "+
					"got: %v", len(test.expectedChannels),
					len(channels))
			}

			for _, ch := range channels {
				_, ok := test.expectedChannels[ch.ShortChannelID]
				if !ok {
					t.Fatalf("fetch channels unexpected "+
						"channel: %v", ch.ShortChannelID)
				}
			}
		})
	}
}

// TestFetchHistoricalChannel tests lookup of historical channels.
func TestFetchHistoricalChannel(t *testing.T) {
	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create a an open channel in the database.
	channel := createTestChannel(t, cdb, openChannelOption())

	// First, try to lookup a channel when nothing is in the bucket. As the
	// bucket is auto-created (on start up), we'll get a channel not found
	// error.
	_, err = cdb.FetchHistoricalChannel(&channel.FundingOutpoint)
	if err != ErrChannelNotFound {
		t.Fatalf("expected no bucket, got: %v", err)
	}

	// Close the channel so that it will be written to the historical
	// bucket. The values provided in the channel close summary are the
	// minimum required for this call to run without panicking.
	if err := channel.CloseChannel(&ChannelCloseSummary{
		ChanPoint:      channel.FundingOutpoint,
		RemotePub:      channel.IdentityPub,
		SettledBalance: btcutil.Amount(500),
	}); err != nil {
		t.Fatalf("unexpected error closing channel: %v", err)
	}

	histChannel, err := cdb.FetchHistoricalChannel(&channel.FundingOutpoint)
	require.NoError(t, err, "unexpected error getting channel")

	// FetchHistoricalChannel will attach the cdb to channel.Db, we set it
	// here so that we can check that all other fields on the channel equal
	// those on the historical channel.
	channel.Db = cdb

	if !reflect.DeepEqual(histChannel, channel) {
		t.Fatalf("expected: %v, got: %v", channel, histChannel)
	}

	// Create an outpoint that will not be in the db and look it up.
	badOutpoint := &wire.OutPoint{
		Hash:  channel.FundingOutpoint.Hash,
		Index: channel.FundingOutpoint.Index + 1,
	}
	_, err = cdb.FetchHistoricalChannel(badOutpoint)
	if err != ErrChannelNotFound {
		t.Fatalf("expected chan not found, got: %v", err)
	}
}

// TestFetchPermTempPeer tests that we're able to call FetchPermAndTempPeers
// successfully.
func TestFetchPermTempPeer(t *testing.T) {
	t.Parallel()

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test database")

	cdb := fullDB.ChannelStateDB()

	// Create an open channel.
	privKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate new private key")

	pubKey1 := privKey1.PubKey()

	channelState1 := createTestChannel(
		t, cdb, openChannelOption(), pubKeyOption(pubKey1),
	)

	// Next, assert that the channel exists in the database.
	_, err = cdb.FetchChannel(channelState1.FundingOutpoint)
	require.NoError(t, err, "unable to fetch channel")

	// Create a pending channel.
	privKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate private key")

	pubKey2 := privKey2.PubKey()
	channelState2 := createTestChannel(t, cdb, pubKeyOption(pubKey2))

	// Assert that the channel exists in the database.
	_, err = cdb.FetchChannel(channelState2.FundingOutpoint)
	require.NoError(t, err, "unable to fetch channel")

	// Create a closed channel.
	privKey3, err := btcec.NewPrivateKey()
	require.NoError(t, err, "unable to generate new private key")

	pubKey3 := privKey3.PubKey()

	_ = createTestChannel(
		t, cdb, pubKeyOption(pubKey3), openChannelOption(),
		closedChannelOption(),
	)

	// Fetch the ChanCount for our peers.
	peerChanInfo, err := cdb.FetchPermAndTempPeers(key[:])
	require.NoError(t, err, "unable to fetch perm and temp peers")

	// There should only be three entries.
	require.Len(t, peerChanInfo, 3)

	// The first entry should have OpenClosed set to true and Pending set
	// to 0.
	count1, found := peerChanInfo[string(pubKey1.SerializeCompressed())]
	require.True(t, found, "unable to find peer 1 in peerChanInfo")
	require.True(
		t, count1.HasOpenOrClosedChan,
		"couldn't find peer 1's channels",
	)
	require.Zero(
		t, count1.PendingOpenCount,
		"peer 1 doesn't have 0 pending-open",
	)

	count2, found := peerChanInfo[string(pubKey2.SerializeCompressed())]
	require.True(t, found, "unable to find peer 2 in peerChanInfo")
	require.False(
		t, count2.HasOpenOrClosedChan, "found erroneous channels",
	)
	require.Equal(t, uint64(1), count2.PendingOpenCount)

	count3, found := peerChanInfo[string(pubKey3.SerializeCompressed())]
	require.True(t, found, "unable to find peer 3 in peerChanInfo")
	require.True(
		t, count3.HasOpenOrClosedChan,
		"couldn't find peer 3's channels",
	)
	require.Zero(
		t, count3.PendingOpenCount,
		"peer 3 doesn't have 0 pending-open",
	)
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

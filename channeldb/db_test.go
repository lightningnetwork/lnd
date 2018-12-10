package channeldb

import (
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
)

func TestOpenWithCreate(t *testing.T) {
	t.Parallel()

	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDirName)

	// Next, open thereby creating channeldb for the first time.
	dbPath := filepath.Join(tempDirName, "cdb")
	cdb, err := Open(dbPath)
	if err != nil {
		t.Fatalf("unable to create channeldb: %v", err)
	}
	if err := cdb.Close(); err != nil {
		t.Fatalf("unable to close channeldb: %v", err)
	}

	// The path should have been successfully created.
	if !fileExists(dbPath) {
		t.Fatalf("channeldb failed to create data directory")
	}
}

// TestWipe tests that the database wipe operation completes successfully
// and that the buckets are deleted. It also checks that attempts to fetch
// information while the buckets are not set return the correct errors.
func TestWipe(t *testing.T) {
	t.Parallel()

	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDirName)

	// Next, open thereby creating channeldb for the first time.
	dbPath := filepath.Join(tempDirName, "cdb")
	cdb, err := Open(dbPath)
	if err != nil {
		t.Fatalf("unable to create channeldb: %v", err)
	}
	defer cdb.Close()

	if err := cdb.Wipe(); err != nil {
		t.Fatalf("unable to wipe channeldb: %v", err)
	}
	// Check correct errors are returned
	_, err = cdb.FetchAllOpenChannels()
	if err != ErrNoActiveChannels {
		t.Fatalf("fetching open channels: expected '%v' instead got '%v'",
			ErrNoActiveChannels, err)
	}
	_, err = cdb.FetchClosedChannels(false)
	if err != ErrNoClosedChannels {
		t.Fatalf("fetching closed channels: expected '%v' instead got '%v'",
			ErrNoClosedChannels, err)
	}
}

// TestFetchClosedChannelForID tests that we are able to properly retrieve a
// ChannelCloseSummary from the DB given a ChannelID.
func TestFetchClosedChannelForID(t *testing.T) {
	t.Parallel()

	const numChans = 101

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	// Create the test channel state, that we will mutate the index of the
	// funding point.
	state, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}

	// Now run through the number of channels, and modify the outpoint index
	// to create new channel IDs.
	for i := uint32(0); i < numChans; i++ {
		// Save the open channel to disk.
		state.FundingOutpoint.Index = i
		if err := state.FullSync(); err != nil {
			t.Fatalf("unable to save and serialize channel "+
				"state: %v", err)
		}

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
		cid := lnwire.NewChanIDFromOutPoint(&state.FundingOutpoint)
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
	cid := lnwire.NewChanIDFromOutPoint(&state.FundingOutpoint)
	_, err = cdb.FetchClosedChannelForID(cid)
	if err != ErrClosedChannelNotFound {
		t.Fatalf("expected ErrClosedChannelNotFound, instead got: %v", err)
	}
}

// TestAddrsForNode tests the we're able to properly obtain all the addresses
// for a target node.
func TestAddrsForNode(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	graph := cdb.ChannelGraph()

	// We'll make a test vertex to insert into the database, as the source
	// node, but this node will only have half the number of addresses it
	// usually does.
	testNode, err := createTestVertex(cdb)
	if err != nil {
		t.Fatalf("unable to create test node: %v", err)
	}
	testNode.Addresses = []net.Addr{testAddr}
	if err := graph.SetSourceNode(testNode); err != nil {
		t.Fatalf("unable to set source node: %v", err)
	}

	// Next, we'll make a link node with the same pubkey, but with an
	// additional address.
	nodePub, err := testNode.PubKey()
	if err != nil {
		t.Fatalf("unable to recv node pub: %v", err)
	}
	linkNode := cdb.NewLinkNode(
		wire.MainNet, nodePub, anotherAddr,
	)
	if err := linkNode.Sync(); err != nil {
		t.Fatalf("unable to sync link node: %v", err)
	}

	// Now that we've created a link node, as well as a vertex for the
	// node, we'll query for all its addresses.
	nodeAddrs, err := cdb.AddrsForNode(nodePub)
	if err != nil {
		t.Fatalf("unable to obtain node addrs: %v", err)
	}

	expectedAddrs := make(map[string]struct{})
	expectedAddrs[testAddr.String()] = struct{}{}
	expectedAddrs[anotherAddr.String()] = struct{}{}

	// Finally, ensure that all the expected addresses are found.
	if len(nodeAddrs) != len(expectedAddrs) {
		t.Fatalf("expected %v addrs, got %v",
			len(expectedAddrs), len(nodeAddrs))
	}
	for _, addr := range nodeAddrs {
		if _, ok := expectedAddrs[addr.String()]; !ok {
			t.Fatalf("unexpected addr: %v", addr)
		}
	}
}

// TestFetchChannel tests that we're able to fetch an arbitrary channel from
// disk.
func TestFetchChannel(t *testing.T) {
	t.Parallel()

	cdb, cleanUp, err := makeTestDB()
	if err != nil {
		t.Fatalf("unable to make test database: %v", err)
	}
	defer cleanUp()

	// Create the test channel state that we'll sync to the database
	// shortly.
	channelState, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}

	// Mark the channel as pending, then immediately mark it as open to it
	// can be fully visible.
	addr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}
	if err := channelState.SyncPending(addr, 9); err != nil {
		t.Fatalf("unable to save and serialize channel state: %v", err)
	}
	err = channelState.MarkAsOpen(lnwire.NewShortChanIDFromInt(99))
	if err != nil {
		t.Fatalf("unable to mark channel open: %v", err)
	}

	// Next, attempt to fetch the channel by its chan point.
	dbChannel, err := cdb.FetchChannel(channelState.FundingOutpoint)
	if err != nil {
		t.Fatalf("unable to fetch channel: %v", err)
	}

	// The decoded channel state should be identical to what we stored
	// above.
	if !reflect.DeepEqual(channelState, dbChannel) {
		t.Fatalf("channel state doesn't match:: %v vs %v",
			spew.Sdump(channelState), spew.Sdump(dbChannel))
	}

	// If we attempt to query for a non-exist ante channel, then we should
	// get an error.
	channelState2, err := createTestChannelState(cdb)
	if err != nil {
		t.Fatalf("unable to create channel state: %v", err)
	}
	channelState2.FundingOutpoint.Index ^= 1

	_, err = cdb.FetchChannel(channelState2.FundingOutpoint)
	if err == nil {
		t.Fatalf("expected query to fail")
	}
}

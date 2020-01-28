package discovery

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	defaultEncoding   = lnwire.EncodingSortedPlain
	latestKnownHeight = 1337
)

var (
	defaultChunkSize = encodingTypeToChunkSize[defaultEncoding]
)

type horizonQuery struct {
	chain chainhash.Hash
	start time.Time
	end   time.Time
}
type filterRangeReq struct {
	startHeight, endHeight uint32
}

type mockChannelGraphTimeSeries struct {
	highestID lnwire.ShortChannelID

	horizonReq  chan horizonQuery
	horizonResp chan []lnwire.Message

	filterReq  chan []lnwire.ShortChannelID
	filterResp chan []lnwire.ShortChannelID

	filterRangeReqs chan filterRangeReq
	filterRangeResp chan []lnwire.ShortChannelID

	annReq  chan []lnwire.ShortChannelID
	annResp chan []lnwire.Message

	updateReq  chan lnwire.ShortChannelID
	updateResp chan []*lnwire.ChannelUpdate
}

func newMockChannelGraphTimeSeries(
	hID lnwire.ShortChannelID) *mockChannelGraphTimeSeries {

	return &mockChannelGraphTimeSeries{
		highestID: hID,

		horizonReq:  make(chan horizonQuery, 1),
		horizonResp: make(chan []lnwire.Message, 1),

		filterReq:  make(chan []lnwire.ShortChannelID, 1),
		filterResp: make(chan []lnwire.ShortChannelID, 1),

		filterRangeReqs: make(chan filterRangeReq, 1),
		filterRangeResp: make(chan []lnwire.ShortChannelID, 1),

		annReq:  make(chan []lnwire.ShortChannelID, 1),
		annResp: make(chan []lnwire.Message, 1),

		updateReq:  make(chan lnwire.ShortChannelID, 1),
		updateResp: make(chan []*lnwire.ChannelUpdate, 1),
	}
}

func (m *mockChannelGraphTimeSeries) HighestChanID(chain chainhash.Hash) (*lnwire.ShortChannelID, error) {
	return &m.highestID, nil
}
func (m *mockChannelGraphTimeSeries) UpdatesInHorizon(chain chainhash.Hash,
	startTime time.Time, endTime time.Time) ([]lnwire.Message, error) {

	m.horizonReq <- horizonQuery{
		chain, startTime, endTime,
	}

	return <-m.horizonResp, nil
}
func (m *mockChannelGraphTimeSeries) FilterKnownChanIDs(chain chainhash.Hash,
	superSet []lnwire.ShortChannelID) ([]lnwire.ShortChannelID, error) {

	m.filterReq <- superSet

	return <-m.filterResp, nil
}
func (m *mockChannelGraphTimeSeries) FilterChannelRange(chain chainhash.Hash,
	startHeight, endHeight uint32) ([]channeldb.BlockChannelRange, error) {

	m.filterRangeReqs <- filterRangeReq{startHeight, endHeight}
	reply := <-m.filterRangeResp

	channelsPerBlock := make(map[uint32][]lnwire.ShortChannelID)
	for _, cid := range reply {
		channelsPerBlock[cid.BlockHeight] = append(
			channelsPerBlock[cid.BlockHeight], cid,
		)
	}

	// Return the channel ranges in ascending block height order.
	blocks := make([]uint32, 0, len(channelsPerBlock))
	for block := range channelsPerBlock {
		blocks = append(blocks, block)
	}
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i] < blocks[j]
	})

	channelRanges := make([]channeldb.BlockChannelRange, 0, len(channelsPerBlock))
	for _, block := range blocks {
		channelRanges = append(channelRanges, channeldb.BlockChannelRange{
			Height:   block,
			Channels: channelsPerBlock[block],
		})
	}

	return channelRanges, nil
}
func (m *mockChannelGraphTimeSeries) FetchChanAnns(chain chainhash.Hash,
	shortChanIDs []lnwire.ShortChannelID) ([]lnwire.Message, error) {

	m.annReq <- shortChanIDs

	return <-m.annResp, nil
}
func (m *mockChannelGraphTimeSeries) FetchChanUpdates(chain chainhash.Hash,
	shortChanID lnwire.ShortChannelID) ([]*lnwire.ChannelUpdate, error) {

	m.updateReq <- shortChanID

	return <-m.updateResp, nil
}

var _ ChannelGraphTimeSeries = (*mockChannelGraphTimeSeries)(nil)

// newTestSyncer creates a new test instance of a GossipSyncer. A buffered
// message channel is returned for intercepting messages sent from the syncer,
// in addition to a mock channel series which allows the test to control which
// messages the syncer knows of or wishes to filter out. The variadic flags are
// treated as positional arguments where the first index signals that the syncer
// should spawn a channelGraphSyncer and second index signals that the syncer
// should spawn a replyHandler. Any flags beyond the first two are currently
// ignored. If no flags are provided, both a channelGraphSyncer and replyHandler
// will be spawned by default.
func newTestSyncer(hID lnwire.ShortChannelID,
	encodingType lnwire.ShortChanIDEncoding, chunkSize int32,
	flags ...bool) (chan []lnwire.Message,
	*GossipSyncer, *mockChannelGraphTimeSeries) {

	syncChannels := true
	replyQueries := true
	if len(flags) > 0 {
		syncChannels = flags[0]
	}
	if len(flags) > 1 {
		replyQueries = flags[1]
	}

	msgChan := make(chan []lnwire.Message, 20)
	cfg := gossipSyncerCfg{
		channelSeries:  newMockChannelGraphTimeSeries(hID),
		encodingType:   encodingType,
		chunkSize:      chunkSize,
		batchSize:      chunkSize,
		noSyncChannels: !syncChannels,
		noReplyQueries: !replyQueries,
		sendToPeer: func(msgs ...lnwire.Message) error {
			msgChan <- msgs
			return nil
		},
		sendToPeerSync: func(msgs ...lnwire.Message) error {
			msgChan <- msgs
			return nil
		},
		delayedQueryReplyInterval: 2 * time.Second,
		bestHeight: func() uint32 {
			return latestKnownHeight
		},
		markGraphSynced:          func() {},
		maxQueryChanRangeReplies: maxQueryChanRangeReplies,
	}
	syncer := newGossipSyncer(cfg)

	return msgChan, syncer, cfg.channelSeries.(*mockChannelGraphTimeSeries)
}

// TestGossipSyncerFilterGossipMsgsNoHorizon tests that if the remote peer
// doesn't have a horizon set, then we won't send any incoming messages to it.
func TestGossipSyncerFilterGossipMsgsNoHorizon(t *testing.T) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	msgChan, syncer, _ := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)

	// With the syncer created, we'll create a set of messages to filter
	// through the gossiper to the target peer.
	msgs := []msgWithSenders{
		{
			msg: &lnwire.NodeAnnouncement{Timestamp: uint32(time.Now().Unix())},
		},
		{
			msg: &lnwire.NodeAnnouncement{Timestamp: uint32(time.Now().Unix())},
		},
	}

	// We'll then attempt to filter the set of messages through the target
	// peer.
	syncer.FilterGossipMsgs(msgs...)

	// As the remote peer doesn't yet have a gossip timestamp set, we
	// shouldn't receive any outbound messages.
	select {
	case msg := <-msgChan:
		t.Fatalf("received message but shouldn't have: %v",
			spew.Sdump(msg))

	case <-time.After(time.Millisecond * 10):
	}
}

func unixStamp(a int64) uint32 {
	t := time.Unix(a, 0)
	return uint32(t.Unix())
}

// TestGossipSyncerFilterGossipMsgsAll tests that we're able to properly filter
// out a set of incoming messages based on the set remote update horizon for a
// peer. We tests all messages type, and all time straddling. We'll also send a
// channel ann that already has a channel update on disk.
func TestGossipSyncerFilterGossipMsgsAllInMemory(t *testing.T) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	msgChan, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)

	// We'll create then apply a remote horizon for the target peer with a
	// set of manually selected timestamps.
	remoteHorizon := &lnwire.GossipTimestampRange{
		FirstTimestamp: unixStamp(25000),
		TimestampRange: uint32(1000),
	}
	syncer.remoteUpdateHorizon = remoteHorizon

	// With the syncer created, we'll create a set of messages to filter
	// through the gossiper to the target peer. Our message will consist of
	// one node announcement above the horizon, one below. Additionally,
	// we'll include a chan ann with an update below the horizon, one
	// with an update timestamp above the horizon, and one without any
	// channel updates at all.
	msgs := []msgWithSenders{
		{
			// Node ann above horizon.
			msg: &lnwire.NodeAnnouncement{Timestamp: unixStamp(25001)},
		},
		{
			// Node ann below horizon.
			msg: &lnwire.NodeAnnouncement{Timestamp: unixStamp(5)},
		},
		{
			// Node ann above horizon.
			msg: &lnwire.NodeAnnouncement{Timestamp: unixStamp(999999)},
		},
		{
			// Ann tuple below horizon.
			msg: &lnwire.ChannelAnnouncement{
				ShortChannelID: lnwire.NewShortChanIDFromInt(10),
			},
		},
		{
			msg: &lnwire.ChannelUpdate{
				ShortChannelID: lnwire.NewShortChanIDFromInt(10),
				Timestamp:      unixStamp(5),
			},
		},
		{
			// Ann tuple above horizon.
			msg: &lnwire.ChannelAnnouncement{
				ShortChannelID: lnwire.NewShortChanIDFromInt(15),
			},
		},
		{
			msg: &lnwire.ChannelUpdate{
				ShortChannelID: lnwire.NewShortChanIDFromInt(15),
				Timestamp:      unixStamp(25002),
			},
		},
		{
			// Ann tuple beyond horizon.
			msg: &lnwire.ChannelAnnouncement{
				ShortChannelID: lnwire.NewShortChanIDFromInt(20),
			},
		},
		{
			msg: &lnwire.ChannelUpdate{
				ShortChannelID: lnwire.NewShortChanIDFromInt(20),
				Timestamp:      unixStamp(999999),
			},
		},
		{
			// Ann w/o an update at all, the update in the DB will
			// be below the horizon.
			msg: &lnwire.ChannelAnnouncement{
				ShortChannelID: lnwire.NewShortChanIDFromInt(25),
			},
		},
	}

	// Before we send off the query, we'll ensure we send the missing
	// channel update for that final ann. It will be below the horizon, so
	// shouldn't be sent anyway.
	errCh := make(chan error, 1)
	go func() {
		select {
		case <-time.After(time.Second * 15):
			errCh <- errors.New("no query received")
			return
		case query := <-chanSeries.updateReq:
			// It should be asking for the chan updates of short
			// chan ID 25.
			expectedID := lnwire.NewShortChanIDFromInt(25)
			if expectedID != query {
				errCh <- fmt.Errorf("wrong query id: expected %v, got %v",
					expectedID, query)
				return
			}

			// If so, then we'll send back the missing update.
			chanSeries.updateResp <- []*lnwire.ChannelUpdate{
				{
					ShortChannelID: lnwire.NewShortChanIDFromInt(25),
					Timestamp:      unixStamp(5),
				},
			}
			errCh <- nil
		}
	}()

	// We'll then instruct the gossiper to filter this set of messages.
	syncer.FilterGossipMsgs(msgs...)

	// Out of all the messages we sent in, we should only get 2 of them
	// back.
	select {
	case <-time.After(time.Second * 15):
		t.Fatalf("no msgs received")

	case msgs := <-msgChan:
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages instead got %v "+
				"messages: %v", len(msgs), spew.Sdump(msgs))
		}
	}

	// Wait for error from goroutine.
	select {
	case <-time.After(time.Second * 30):
		t.Fatalf("goroutine did not return within 30 seconds")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestGossipSyncerApplyNoHistoricalGossipFilter tests that once a gossip filter
// is applied for the remote peer, then we don't send the peer all known
// messages which are within their desired time horizon.
func TestGossipSyncerApplyNoHistoricalGossipFilter(t *testing.T) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	_, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)
	syncer.cfg.ignoreHistoricalFilters = true

	// We'll apply this gossip horizon for the remote peer.
	remoteHorizon := &lnwire.GossipTimestampRange{
		FirstTimestamp: unixStamp(25000),
		TimestampRange: uint32(1000),
	}

	// After applying the gossip filter, the chan series should not be
	// queried using the updated horizon.
	errChan := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		select {
		// No query received, success.
		case <-time.After(3 * time.Second):
			errChan <- nil

		// Unexpected query received.
		case <-chanSeries.horizonReq:
			errChan <- errors.New("chan series should not have been " +
				"queried")
		}
	}()

	// We'll now attempt to apply the gossip filter for the remote peer.
	syncer.ApplyGossipFilter(remoteHorizon)

	// Ensure that the syncer's remote horizon was properly updated.
	if !reflect.DeepEqual(syncer.remoteUpdateHorizon, remoteHorizon) {
		t.Fatalf("expected remote horizon: %v, got: %v",
			remoteHorizon, syncer.remoteUpdateHorizon)
	}

	// Wait for the query check to finish.
	wg.Wait()

	// Assert that no query was made as a result of applying the gossip
	// filter.
	err := <-errChan
	if err != nil {
		t.Fatalf(err.Error())
	}
}

// TestGossipSyncerApplyGossipFilter tests that once a gossip filter is applied
// for the remote peer, then we send the peer all known messages which are
// within their desired time horizon.
func TestGossipSyncerApplyGossipFilter(t *testing.T) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	msgChan, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)

	// We'll apply this gossip horizon for the remote peer.
	remoteHorizon := &lnwire.GossipTimestampRange{
		FirstTimestamp: unixStamp(25000),
		TimestampRange: uint32(1000),
	}

	// Before we apply the horizon, we'll dispatch a response to the query
	// that the syncer will issue.
	errCh := make(chan error, 1)
	go func() {
		select {
		case <-time.After(time.Second * 15):
			errCh <- errors.New("no query recvd")
			return
		case query := <-chanSeries.horizonReq:
			// The syncer should have translated the time range
			// into the proper star time.
			if remoteHorizon.FirstTimestamp != uint32(query.start.Unix()) {
				errCh <- fmt.Errorf("wrong query stamp: expected %v, got %v",
					remoteHorizon.FirstTimestamp, query.start)
				return
			}

			// For this first response, we'll send back an empty
			// set of messages. As result, we shouldn't send any
			// messages.
			chanSeries.horizonResp <- []lnwire.Message{}
			errCh <- nil
		}
	}()

	// We'll now attempt to apply the gossip filter for the remote peer.
	err := syncer.ApplyGossipFilter(remoteHorizon)
	if err != nil {
		t.Fatalf("unable to apply filter: %v", err)
	}

	// There should be no messages in the message queue as we didn't send
	// the syncer and messages within the horizon.
	select {
	case msgs := <-msgChan:
		t.Fatalf("expected no msgs, instead got %v", spew.Sdump(msgs))
	default:
	}

	// Wait for error result from goroutine.
	select {
	case <-time.After(time.Second * 30):
		t.Fatalf("goroutine did not return within 30 seconds")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}

	// If we repeat the process, but give the syncer a set of valid
	// messages, then these should be sent to the remote peer.
	go func() {
		select {
		case <-time.After(time.Second * 15):
			errCh <- errors.New("no query recvd")
			return
		case query := <-chanSeries.horizonReq:
			// The syncer should have translated the time range
			// into the proper star time.
			if remoteHorizon.FirstTimestamp != uint32(query.start.Unix()) {
				errCh <- fmt.Errorf("wrong query stamp: expected %v, got %v",
					remoteHorizon.FirstTimestamp, query.start)
				return
			}

			// For this first response, we'll send back a proper
			// set of messages that should be echoed back.
			chanSeries.horizonResp <- []lnwire.Message{
				&lnwire.ChannelUpdate{
					ShortChannelID: lnwire.NewShortChanIDFromInt(25),
					Timestamp:      unixStamp(5),
				},
			}
			errCh <- nil
		}
	}()
	err = syncer.ApplyGossipFilter(remoteHorizon)
	if err != nil {
		t.Fatalf("unable to apply filter: %v", err)
	}

	// We should get back the exact same message.
	select {
	case <-time.After(time.Second * 15):
		t.Fatalf("no msgs received")

	case msgs := <-msgChan:
		if len(msgs) != 1 {
			t.Fatalf("wrong messages: expected %v, got %v",
				1, len(msgs))
		}
	}

	// Wait for error result from goroutine.
	select {
	case <-time.After(time.Second * 30):
		t.Fatalf("goroutine did not return within 30 seconds")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestGossipSyncerQueryChannelRangeWrongChainHash tests that if we receive a
// channel range query for the wrong chain, then we send back a response with no
// channels and complete=0.
func TestGossipSyncerQueryChannelRangeWrongChainHash(t *testing.T) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	msgChan, syncer, _ := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)

	// We'll now ask the syncer to reply to a channel range query, but for a
	// chain that it isn't aware of.
	query := &lnwire.QueryChannelRange{
		ChainHash:        *chaincfg.SimNetParams.GenesisHash,
		FirstBlockHeight: 0,
		NumBlocks:        math.MaxUint32,
	}
	err := syncer.replyChanRangeQuery(query)
	if err != nil {
		t.Fatalf("unable to process short chan ID's: %v", err)
	}

	select {
	case <-time.After(time.Second * 15):
		t.Fatalf("no msgs received")

	case msgs := <-msgChan:
		// We should get back exactly one message, that's a
		// ReplyChannelRange with a matching query, and a complete value
		// of zero.
		if len(msgs) != 1 {
			t.Fatalf("wrong messages: expected %v, got %v",
				1, len(msgs))
		}

		msg, ok := msgs[0].(*lnwire.ReplyChannelRange)
		if !ok {
			t.Fatalf("expected lnwire.ReplyChannelRange, got %T", msg)
		}

		if msg.ChainHash != query.ChainHash {
			t.Fatalf("wrong chain hash: expected %v got %v",
				query.ChainHash, msg.ChainHash)
		}
		if msg.Complete != 0 {
			t.Fatalf("expected complete set to 0, got %v",
				msg.Complete)
		}
	}
}

// TestGossipSyncerReplyShortChanIDsWrongChainHash tests that if we get a chan
// ID query for the wrong chain, then we send back only a short ID end with
// complete=0.
func TestGossipSyncerReplyShortChanIDsWrongChainHash(t *testing.T) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	msgChan, syncer, _ := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)

	// We'll now ask the syncer to reply to a chan ID query, but for a
	// chain that it isn't aware of.
	err := syncer.replyShortChanIDs(&lnwire.QueryShortChanIDs{
		ChainHash: *chaincfg.SimNetParams.GenesisHash,
	})
	if err != nil {
		t.Fatalf("unable to process short chan ID's: %v", err)
	}

	select {
	case <-time.After(time.Second * 15):
		t.Fatalf("no msgs received")
	case msgs := <-msgChan:

		// We should get back exactly one message, that's a
		// ReplyShortChanIDsEnd with a matching chain hash, and a
		// complete value of zero.
		if len(msgs) != 1 {
			t.Fatalf("wrong messages: expected %v, got %v",
				1, len(msgs))
		}

		msg, ok := msgs[0].(*lnwire.ReplyShortChanIDsEnd)
		if !ok {
			t.Fatalf("expected lnwire.ReplyShortChanIDsEnd "+
				"instead got %T", msg)
		}

		if msg.ChainHash != *chaincfg.SimNetParams.GenesisHash {
			t.Fatalf("wrong chain hash: expected %v, got %v",
				msg.ChainHash, chaincfg.SimNetParams.GenesisHash)
		}
		if msg.Complete != 0 {
			t.Fatalf("complete set incorrectly")
		}
	}
}

// TestGossipSyncerReplyShortChanIDs tests that in the case of a known chain
// hash for a QueryShortChanIDs, we'll return the set of matching
// announcements, as well as an ending ReplyShortChanIDsEnd message.
func TestGossipSyncerReplyShortChanIDs(t *testing.T) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	msgChan, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)

	queryChanIDs := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(1),
		lnwire.NewShortChanIDFromInt(2),
		lnwire.NewShortChanIDFromInt(3),
	}

	queryReply := []lnwire.Message{
		&lnwire.ChannelAnnouncement{
			ShortChannelID: lnwire.NewShortChanIDFromInt(20),
		},
		&lnwire.ChannelUpdate{
			ShortChannelID: lnwire.NewShortChanIDFromInt(20),
			Timestamp:      unixStamp(999999),
		},
		&lnwire.NodeAnnouncement{Timestamp: unixStamp(25001)},
	}

	// We'll then craft a reply to the upcoming query for all the matching
	// channel announcements for a particular set of short channel ID's.
	errCh := make(chan error, 1)
	go func() {
		select {
		case <-time.After(time.Second * 15):
			errCh <- errors.New("no query recvd")
			return
		case chanIDs := <-chanSeries.annReq:
			// The set of chan ID's should match exactly.
			if !reflect.DeepEqual(chanIDs, queryChanIDs) {
				errCh <- fmt.Errorf("wrong chan IDs: expected %v, got %v",
					queryChanIDs, chanIDs)
				return
			}

			// If they do, then we'll send back a response with
			// some canned messages.
			chanSeries.annResp <- queryReply
			errCh <- nil
		}
	}()

	// With our set up above complete, we'll now attempt to obtain a reply
	// from the channel syncer for our target chan ID query.
	err := syncer.replyShortChanIDs(&lnwire.QueryShortChanIDs{
		ShortChanIDs: queryChanIDs,
	})
	if err != nil {
		t.Fatalf("unable to query for chan IDs: %v", err)
	}

	for i := 0; i < len(queryReply)+1; i++ {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no msgs received")

		// We should get back exactly 4 messages. The first 3 are the
		// same messages we sent above, and the query end message.
		case msgs := <-msgChan:
			if len(msgs) != 1 {
				t.Fatalf("wrong number of messages: "+
					"expected %v, got %v", 1, len(msgs))
			}

			isQueryReply := i < len(queryReply)
			finalMsg, ok := msgs[0].(*lnwire.ReplyShortChanIDsEnd)

			switch {
			case isQueryReply &&
				!reflect.DeepEqual(queryReply[i], msgs[0]):

				t.Fatalf("wrong message: expected %v, got %v",
					spew.Sdump(queryReply[i]),
					spew.Sdump(msgs[0]))

			case !isQueryReply && !ok:
				t.Fatalf("expected lnwire.ReplyShortChanIDsEnd"+
					" instead got %T", msgs[3])

			case !isQueryReply && finalMsg.Complete != 1:
				t.Fatalf("complete wasn't set")
			}
		}
	}

	// Wait for error from goroutine.
	select {
	case <-time.After(time.Second * 30):
		t.Fatalf("goroutine did not return within 30 seconds")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestGossipSyncerReplyChanRangeQuery tests that if we receive a
// QueryChannelRange message, then we'll properly send back a chunked reply to
// the remote peer.
func TestGossipSyncerReplyChanRangeQuery(t *testing.T) {
	t.Parallel()

	// We'll use a smaller chunk size so we can easily test all the edge
	// cases.
	const chunkSize = 2

	// We'll now create our test gossip syncer that will shortly respond to
	// our canned query.
	msgChan, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding, chunkSize,
	)

	// Next, we'll craft a query to ask for all the new chan ID's after
	// block 100.
	const startingBlockHeight = 100
	const numBlocks = 50
	const endingBlockHeight = startingBlockHeight + numBlocks - 1
	query := &lnwire.QueryChannelRange{
		FirstBlockHeight: uint32(startingBlockHeight),
		NumBlocks:        uint32(numBlocks),
	}

	// We'll then launch a goroutine to reply to the query with a set of 5
	// responses. This will ensure we get two full chunks, and one partial
	// chunk.
	queryResp := []lnwire.ShortChannelID{
		{
			BlockHeight: uint32(startingBlockHeight),
		},
		{
			BlockHeight: 102,
		},
		{
			BlockHeight: 104,
		},
		{
			BlockHeight: 106,
		},
		{
			BlockHeight: 108,
		},
	}

	errCh := make(chan error, 1)
	go func() {
		select {
		case <-time.After(time.Second * 15):
			errCh <- errors.New("no query recvd")
			return
		case filterReq := <-chanSeries.filterRangeReqs:
			// We should be querying for block 100 to 150.
			if filterReq.startHeight != startingBlockHeight &&
				filterReq.endHeight != endingBlockHeight {

				errCh <- fmt.Errorf("wrong height range: %v",
					spew.Sdump(filterReq))
				return
			}

			// If the proper request was sent, then we'll respond
			// with our set of short channel ID's.
			chanSeries.filterRangeResp <- queryResp
			errCh <- nil
		}
	}()

	// With our goroutine active, we'll now issue the query.
	if err := syncer.replyChanRangeQuery(query); err != nil {
		t.Fatalf("unable to issue query: %v", err)
	}

	// At this point, we'll now wait for the syncer to send the chunked
	// reply. We should get three sets of messages as two of them should be
	// full, while the other is the final fragment.
	const numExpectedChunks = 3
	var prevResp *lnwire.ReplyChannelRange
	respMsgs := make([]lnwire.ShortChannelID, 0, 5)
	for i := 0; i < numExpectedChunks; i++ {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no msgs received")

		case msg := <-msgChan:
			resp := msg[0]
			rangeResp, ok := resp.(*lnwire.ReplyChannelRange)
			if !ok {
				t.Fatalf("expected ReplyChannelRange instead got %T", msg)
			}

			// We'll determine the correct values of each field in
			// each response based on the order that they were sent.
			var (
				expectedFirstBlockHeight uint32
				expectedNumBlocks        uint32
				expectedComplete         uint8
			)

			switch {
			// The first reply should range from our starting block
			// height until it reaches its maximum capacity of
			// channels.
			case i == 0:
				expectedFirstBlockHeight = startingBlockHeight
				expectedNumBlocks = 4

			// The last reply should range starting from the next
			// block of our previous reply up until the ending
			// height of the query. It should also have the Complete
			// bit set.
			case i == numExpectedChunks-1:
				expectedFirstBlockHeight = prevResp.LastBlockHeight() + 1
				expectedNumBlocks = endingBlockHeight - expectedFirstBlockHeight + 1
				expectedComplete = 1

			// Any intermediate replies should range starting from
			// the next block of our previous reply up until it
			// reaches its maximum capacity of channels.
			default:
				expectedFirstBlockHeight = prevResp.LastBlockHeight() + 1
				expectedNumBlocks = 4
			}

			switch {
			case rangeResp.FirstBlockHeight != expectedFirstBlockHeight:
				t.Fatalf("FirstBlockHeight in resp #%d "+
					"incorrect: expected %v, got %v", i+1,
					expectedFirstBlockHeight,
					rangeResp.FirstBlockHeight)

			case rangeResp.NumBlocks != expectedNumBlocks:
				t.Fatalf("NumBlocks in resp #%d incorrect: "+
					"expected %v, got %v", i+1,
					expectedNumBlocks, rangeResp.NumBlocks)

			case rangeResp.Complete != expectedComplete:
				t.Fatalf("Complete in resp #%d incorrect: "+
					"expected %v, got %v", i+1,
					expectedComplete, rangeResp.Complete)
			}

			prevResp = rangeResp
			respMsgs = append(respMsgs, rangeResp.ShortChanIDs...)
		}
	}

	// We should get back exactly 5 short chan ID's, and they should match
	// exactly the ID's we sent as a reply.
	if len(respMsgs) != len(queryResp) {
		t.Fatalf("expected %v chan ID's, instead got %v",
			len(queryResp), spew.Sdump(respMsgs))
	}
	if !reflect.DeepEqual(queryResp, respMsgs) {
		t.Fatalf("mismatched response: expected %v, got %v",
			spew.Sdump(queryResp), spew.Sdump(respMsgs))
	}

	// Wait for error from goroutine.
	select {
	case <-time.After(time.Second * 30):
		t.Fatalf("goroutine did not return within 30 seconds")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestGossipSyncerReplyChanRangeQuery tests a variety of
// QueryChannelRange messages to ensure the underlying queries are
// executed with the correct block range
func TestGossipSyncerReplyChanRangeQueryBlockRange(t *testing.T) {
	t.Parallel()

	// First create our test gossip syncer that will handle and
	// respond to the test queries
	_, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding, math.MaxInt32,
	)

	// Next construct test queries with various startBlock and endBlock
	// ranges
	queryReqs := []*lnwire.QueryChannelRange{
		// full range example
		{
			FirstBlockHeight: uint32(0),
			NumBlocks:        uint32(math.MaxUint32),
		},

		// small query example that does not overflow
		{
			FirstBlockHeight: uint32(1000),
			NumBlocks:        uint32(100),
		},

		// overflow example
		{
			FirstBlockHeight: uint32(1000),
			NumBlocks:        uint32(math.MaxUint32),
		},
	}

	// Next construct the expected filterRangeReq startHeight and endHeight
	// values that we will compare to the captured values
	expFilterReqs := []filterRangeReq{
		{
			startHeight: uint32(0),
			endHeight:   uint32(math.MaxUint32 - 1),
		},
		{
			startHeight: uint32(1000),
			endHeight:   uint32(1099),
		},
		{
			startHeight: uint32(1000),
			endHeight:   uint32(math.MaxUint32),
		},
	}

	// We'll then launch a goroutine to capture the filterRangeReqs for
	// each request and return those results once all queries have been
	// received
	resultsCh := make(chan []filterRangeReq, 1)
	errCh := make(chan error, 1)
	go func() {
		// We will capture the values supplied to the chanSeries here
		// and return the results once all the requests have been
		// collected
		capFilterReqs := []filterRangeReq{}

		for filterReq := range chanSeries.filterRangeReqs {
			// capture the filter request so we can compare to the
			// expected values later
			capFilterReqs = append(capFilterReqs, filterReq)

			// Reply with an empty result for each query to allow
			// unblock the caller
			queryResp := []lnwire.ShortChannelID{}
			chanSeries.filterRangeResp <- queryResp

			// Once we have collected all results send the results
			// back to the main thread and terminate the goroutine
			if len(capFilterReqs) == len(expFilterReqs) {
				resultsCh <- capFilterReqs
				return
			}

		}
	}()

	// We'll launch a goroutine to send the query sequentially. This
	// goroutine ensures that the timeout logic below on the mainthread
	// will be reached
	go func() {
		for _, query := range queryReqs {
			if err := syncer.replyChanRangeQuery(query); err != nil {
				errCh <- fmt.Errorf("unable to issue query: %v", err)
				return
			}
		}
	}()

	// Wait for the results to be collected and validate that the
	// collected results match the expected results, the timeout to
	// expire, or an error to occur
	select {
	case capFilterReq := <-resultsCh:
		if !reflect.DeepEqual(expFilterReqs, capFilterReq) {
			t.Fatalf("mismatched filter reqs: expected %v, got %v",
				spew.Sdump(expFilterReqs), spew.Sdump(capFilterReq))
		}
	case <-time.After(time.Second * 10):
		t.Fatalf("goroutine did not return within 10 seconds")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestGossipSyncerReplyChanRangeQueryNoNewChans tests that if we issue a reply
// for a channel range query, and we don't have any new channels, then we send
// back a single response that signals completion.
func TestGossipSyncerReplyChanRangeQueryNoNewChans(t *testing.T) {
	t.Parallel()

	// We'll now create our test gossip syncer that will shortly respond to
	// our canned query.
	msgChan, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)

	// Next, we'll craft a query to ask for all the new chan ID's after
	// block 100.
	query := &lnwire.QueryChannelRange{
		FirstBlockHeight: 100,
		NumBlocks:        50,
	}

	// We'll then launch a goroutine to reply to the query no new channels.
	resp := []lnwire.ShortChannelID{}
	errCh := make(chan error, 1)
	go func() {
		select {
		case <-time.After(time.Second * 15):
			errCh <- errors.New("no query recvd")
			return
		case filterReq := <-chanSeries.filterRangeReqs:
			// We should be querying for block 100 to 150.
			if filterReq.startHeight != 100 && filterReq.endHeight != 150 {
				errCh <- fmt.Errorf("wrong height range: %v",
					spew.Sdump(filterReq))
				return
			}
			// If the proper request was sent, then we'll respond
			// with our blank set of short chan ID's.
			chanSeries.filterRangeResp <- resp
			errCh <- nil
		}
	}()

	// With our goroutine active, we'll now issue the query.
	if err := syncer.replyChanRangeQuery(query); err != nil {
		t.Fatalf("unable to issue query: %v", err)
	}

	// We should get back exactly one message, and the message should
	// indicate that this is the final in the series.
	select {
	case <-time.After(time.Second * 15):
		t.Fatalf("no msgs received")

	case msg := <-msgChan:
		resp := msg[0]
		rangeResp, ok := resp.(*lnwire.ReplyChannelRange)
		if !ok {
			t.Fatalf("expected ReplyChannelRange instead got %T", msg)
		}

		if len(rangeResp.ShortChanIDs) != 0 {
			t.Fatalf("expected no chan ID's, instead "+
				"got: %v", spew.Sdump(rangeResp.ShortChanIDs))
		}
		if rangeResp.Complete != 1 {
			t.Fatalf("complete wasn't set")
		}
	}

	// Wait for error from goroutine.
	select {
	case <-time.After(time.Second * 30):
		t.Fatalf("goroutine did not return within 30 seconds")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestGossipSyncerGenChanRangeQuery tests that given the current best known
// channel ID, we properly generate an correct initial channel range response.
func TestGossipSyncerGenChanRangeQuery(t *testing.T) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	const startingHeight = 200
	_, syncer, _ := newTestSyncer(
		lnwire.ShortChannelID{BlockHeight: startingHeight},
		defaultEncoding, defaultChunkSize,
	)

	// If we now ask the syncer to generate an initial range query, it
	// should return a start height that's back chanRangeQueryBuffer
	// blocks.
	rangeQuery, err := syncer.genChanRangeQuery(false)
	if err != nil {
		t.Fatalf("unable to resp: %v", err)
	}

	firstHeight := uint32(startingHeight - chanRangeQueryBuffer)
	if rangeQuery.FirstBlockHeight != firstHeight {
		t.Fatalf("incorrect chan range query: expected %v, %v",
			rangeQuery.FirstBlockHeight,
			startingHeight-chanRangeQueryBuffer)
	}
	if rangeQuery.NumBlocks != latestKnownHeight-firstHeight {
		t.Fatalf("wrong num blocks: expected %v, got %v",
			latestKnownHeight-firstHeight, rangeQuery.NumBlocks)
	}

	// Generating a historical range query should result in a start height
	// of 0.
	rangeQuery, err = syncer.genChanRangeQuery(true)
	if err != nil {
		t.Fatalf("unable to resp: %v", err)
	}
	if rangeQuery.FirstBlockHeight != 0 {
		t.Fatalf("incorrect chan range query: expected %v, %v", 0,
			rangeQuery.FirstBlockHeight)
	}
	if rangeQuery.NumBlocks != latestKnownHeight {
		t.Fatalf("wrong num blocks: expected %v, got %v",
			latestKnownHeight, rangeQuery.NumBlocks)
	}
}

// TestGossipSyncerProcessChanRangeReply tests that we'll properly buffer
// replied channel replies until we have the complete version.
func TestGossipSyncerProcessChanRangeReply(t *testing.T) {
	t.Parallel()

	t.Run("legacy", func(t *testing.T) {
		testGossipSyncerProcessChanRangeReply(t, true)
	})
	t.Run("block ranges", func(t *testing.T) {
		testGossipSyncerProcessChanRangeReply(t, false)
	})
}

// testGossipSyncerProcessChanRangeReply tests that we'll properly buffer
// replied channel replies until we have the complete version. The legacy
// option, if set, uses the Complete field of the reply to determine when we've
// received all expected replies. Otherwise, it looks at the block ranges of
// each reply instead.
func testGossipSyncerProcessChanRangeReply(t *testing.T, legacy bool) {
	t.Parallel()

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	highestID := lnwire.ShortChannelID{
		BlockHeight: latestKnownHeight,
	}
	_, syncer, chanSeries := newTestSyncer(
		highestID, defaultEncoding, defaultChunkSize,
	)

	startingState := syncer.state

	query, err := syncer.genChanRangeQuery(true)
	if err != nil {
		t.Fatalf("unable to generate channel range query: %v", err)
	}

	// When interpreting block ranges, the first reply should start from
	// our requested first block, and the last should end at our requested
	// last block.
	replies := []*lnwire.ReplyChannelRange{
		{
			FirstBlockHeight: 0,
			NumBlocks:        11,
			ShortChanIDs: []lnwire.ShortChannelID{
				{
					BlockHeight: 10,
				},
			},
		},
		{
			FirstBlockHeight: 11,
			NumBlocks:        1,
			ShortChanIDs: []lnwire.ShortChannelID{
				{
					BlockHeight: 11,
				},
			},
		},
		{
			FirstBlockHeight: 12,
			NumBlocks:        query.NumBlocks - 12,
			Complete:         1,
			ShortChanIDs: []lnwire.ShortChannelID{
				{
					BlockHeight: 12,
				},
			},
		},
	}

	// Each reply query is the same as the original query in the legacy
	// mode.
	if legacy {
		replies[0].FirstBlockHeight = query.FirstBlockHeight
		replies[0].NumBlocks = query.NumBlocks

		replies[1].FirstBlockHeight = query.FirstBlockHeight
		replies[1].NumBlocks = query.NumBlocks

		replies[2].FirstBlockHeight = query.FirstBlockHeight
		replies[2].NumBlocks = query.NumBlocks
	}

	// We'll begin by sending the syncer a set of non-complete channel
	// range replies.
	if err := syncer.processChanRangeReply(replies[0]); err != nil {
		t.Fatalf("unable to process reply: %v", err)
	}
	if err := syncer.processChanRangeReply(replies[1]); err != nil {
		t.Fatalf("unable to process reply: %v", err)
	}

	// At this point, we should still be in our starting state as the query
	// hasn't finished.
	if syncer.state != startingState {
		t.Fatalf("state should not have transitioned")
	}

	expectedReq := []lnwire.ShortChannelID{
		{
			BlockHeight: 10,
		},
		{
			BlockHeight: 11,
		},
		{
			BlockHeight: 12,
		},
	}

	// As we're about to send the final response, we'll launch a goroutine
	// to respond back with a filtered set of chan ID's.
	errCh := make(chan error, 1)
	go func() {
		select {
		case <-time.After(time.Second * 15):
			errCh <- errors.New("no query received")
			return

		case req := <-chanSeries.filterReq:
			// We should get a request for the entire range of short
			// chan ID's.
			if !reflect.DeepEqual(expectedReq, req) {
				errCh <- fmt.Errorf("wrong request: expected %v, got %v",
					expectedReq, req)
				return
			}

			// We'll send back only the last two to simulate filtering.
			chanSeries.filterResp <- expectedReq[1:]
			errCh <- nil
		}
	}()

	// If we send the final message, then we should transition to
	// queryNewChannels as we've sent a non-empty set of new channels.
	if err := syncer.processChanRangeReply(replies[2]); err != nil {
		t.Fatalf("unable to process reply: %v", err)
	}

	if syncer.syncState() != queryNewChannels {
		t.Fatalf("wrong state: expected %v instead got %v",
			queryNewChannels, syncer.state)
	}
	if !reflect.DeepEqual(syncer.newChansToQuery, expectedReq[1:]) {
		t.Fatalf("wrong set of chans to query: expected %v, got %v",
			syncer.newChansToQuery, expectedReq[1:])
	}

	// Wait for error from goroutine.
	select {
	case <-time.After(time.Second * 30):
		t.Fatalf("goroutine did not return within 30 seconds")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestGossipSyncerSynchronizeChanIDs tests that we properly request chunks of
// the short chan ID's which were unknown to us. We'll ensure that we request
// chunk by chunk, and after the last chunk, we return true indicating that we
// can transition to the synced stage.
func TestGossipSyncerSynchronizeChanIDs(t *testing.T) {
	t.Parallel()

	// We'll modify the chunk size to be a smaller value, so we can ensure
	// our chunk parsing works properly. With this value we should get 3
	// queries: two full chunks, and one lingering chunk.
	const chunkSize = 2

	// First, we'll create a GossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	msgChan, syncer, _ := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding, chunkSize,
	)

	// Next, we'll construct a set of chan ID's that we should query for,
	// and set them as newChansToQuery within the state machine.
	newChanIDs := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(1),
		lnwire.NewShortChanIDFromInt(2),
		lnwire.NewShortChanIDFromInt(3),
		lnwire.NewShortChanIDFromInt(4),
		lnwire.NewShortChanIDFromInt(5),
	}
	syncer.newChansToQuery = newChanIDs

	for i := 0; i < chunkSize*2; i += 2 {
		// With our set up complete, we'll request a sync of chan ID's.
		done, err := syncer.synchronizeChanIDs()
		if err != nil {
			t.Fatalf("unable to sync chan IDs: %v", err)
		}

		// At this point, we shouldn't yet be done as only 2 items
		// should have been queried for.
		if done {
			t.Fatalf("syncer shown as done, but shouldn't be!")
		}

		// We should've received a new message from the syncer.
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no msgs received")

		case msg := <-msgChan:
			queryMsg, ok := msg[0].(*lnwire.QueryShortChanIDs)
			if !ok {
				t.Fatalf("expected QueryShortChanIDs instead "+
					"got %T", msg)
			}

			// The query message should have queried for the first
			// two chan ID's, and nothing more.
			if !reflect.DeepEqual(queryMsg.ShortChanIDs, newChanIDs[i:i+chunkSize]) {
				t.Fatalf("wrong query: expected %v, got %v",
					spew.Sdump(newChanIDs[i:i+chunkSize]),
					queryMsg.ShortChanIDs)
			}
		}

		// With the proper message sent out, the internal state of the
		// syncer should reflect that it still has more channels to
		// query for.
		if !reflect.DeepEqual(syncer.newChansToQuery, newChanIDs[i+chunkSize:]) {
			t.Fatalf("incorrect chans to query for: expected %v, got %v",
				spew.Sdump(newChanIDs[i+chunkSize:]),
				syncer.newChansToQuery)
		}
	}

	// At this point, only one more channel should be lingering for the
	// syncer to query for.
	if !reflect.DeepEqual(newChanIDs[chunkSize*2:], syncer.newChansToQuery) {
		t.Fatalf("wrong chans to query: expected %v, got %v",
			newChanIDs[chunkSize*2:], syncer.newChansToQuery)
	}

	// If we issue another query, the syncer should tell us that it's done.
	done, err := syncer.synchronizeChanIDs()
	if err != nil {
		t.Fatalf("unable to sync chan IDs: %v", err)
	}
	if done {
		t.Fatalf("syncer should be finished!")
	}

	select {
	case <-time.After(time.Second * 15):
		t.Fatalf("no msgs received")

	case msg := <-msgChan:
		queryMsg, ok := msg[0].(*lnwire.QueryShortChanIDs)
		if !ok {
			t.Fatalf("expected QueryShortChanIDs instead "+
				"got %T", msg)
		}

		// The query issued should simply be the last item.
		if !reflect.DeepEqual(queryMsg.ShortChanIDs, newChanIDs[chunkSize*2:]) {
			t.Fatalf("wrong query: expected %v, got %v",
				spew.Sdump(newChanIDs[chunkSize*2:]),
				queryMsg.ShortChanIDs)
		}

		// There also should be no more channels to query.
		if len(syncer.newChansToQuery) != 0 {
			t.Fatalf("should be no more chans to query for, "+
				"instead have %v",
				spew.Sdump(syncer.newChansToQuery))
		}
	}
}

// TestGossipSyncerDelayDOS tests that the gossip syncer will begin delaying
// queries after its prescribed allotment of undelayed query responses. Once
// this happens, all query replies should be delayed by the configurated
// interval.
func TestGossipSyncerDelayDOS(t *testing.T) {
	t.Parallel()

	// We'll modify the chunk size to be a smaller value, since we'll be
	// sending a modest number of queries. After exhausting our undelayed
	// gossip queries, we'll send two extra queries and ensure that they are
	// delayed properly.
	const chunkSize = 2
	const numDelayedQueries = 2
	const delayTolerance = time.Millisecond * 200

	// First, we'll create two GossipSyncer instances with a canned
	// sendToPeer message to allow us to intercept their potential sends.
	highestID := lnwire.ShortChannelID{
		BlockHeight: 1144,
	}
	msgChan1, syncer1, chanSeries1 := newTestSyncer(
		highestID, defaultEncoding, chunkSize, true, false,
	)
	syncer1.Start()
	defer syncer1.Stop()

	msgChan2, syncer2, chanSeries2 := newTestSyncer(
		highestID, defaultEncoding, chunkSize, false, true,
	)
	syncer2.Start()
	defer syncer2.Stop()

	// Record the delayed query reply interval used by each syncer.
	delayedQueryInterval := syncer1.cfg.delayedQueryReplyInterval

	// Record the number of undelayed queries allowed by the syncers.
	numUndelayedQueries := syncer1.cfg.maxUndelayedQueryReplies

	// We will send enough queries to exhaust the undelayed responses, and
	// then send two more queries which should be delayed. An additional one
	// is subtracted from the total since undelayed message will be consumed
	// by the initial QueryChannelRange.
	numQueryResponses := numUndelayedQueries + numDelayedQueries - 1

	// The total number of responses must include the initial reply each
	// syncer will make to QueryChannelRange.
	numTotalQueries := 1 + numQueryResponses

	// The total number of channels each syncer needs to request must be
	// scaled by the chunk size being used.
	numTotalChans := numQueryResponses * chunkSize

	// Construct enough channels so that all of the queries will have enough
	// channels. Since syncer1 won't know of any channels, their sets are
	// inherently disjoint.
	var syncer2Chans []lnwire.ShortChannelID
	for i := 0; i < numTotalChans; i++ {
		syncer2Chans = append([]lnwire.ShortChannelID{
			{
				BlockHeight: highestID.BlockHeight - uint32(i) - 1,
				TxIndex:     uint32(i),
			},
		}, syncer2Chans...)
	}

	// We'll kick off the test by asserting syncer1 sends over the
	// QueryChannelRange message the other node.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("didn't get msg from syncer1")

	case msgs := <-msgChan1:
		for _, msg := range msgs {
			// The message MUST be a QueryChannelRange message.
			_, ok := msg.(*lnwire.QueryChannelRange)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"QueryChannelRange for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer2.queryMsgs <- msg:

			}
		}
	}

	// At this point, we'll need to a response from syncer2's channel
	// series. This will cause syncer1 to simply request the entire set of
	// channels from syncer2. This will count as the first undelayed
	// response for sycner2.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries2.filterRangeReqs:
		// We'll send back all the channels that it should know of.
		chanSeries2.filterRangeResp <- syncer2Chans
	}

	// At this point, we'll assert that the ReplyChannelRange message is
	// sent by sycner2.
	for i := 0; i < numQueryResponses; i++ {
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer2")

		case msgs := <-msgChan2:
			for _, msg := range msgs {
				// The message MUST be a ReplyChannelRange message.
				_, ok := msg.(*lnwire.ReplyChannelRange)
				if !ok {
					t.Fatalf("wrong message: expected "+
						"QueryChannelRange for %T", msg)
				}

				select {
				case <-time.After(time.Second * 2):
					t.Fatalf("node 2 didn't read msg")

				case syncer1.gossipMsgs <- msg:
				}
			}
		}
	}

	// We'll now have syncer1 process the received sids from syncer2.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries1.filterReq:
		chanSeries1.filterResp <- syncer2Chans
	}

	// At this point, syncer1 should start to send out initial requests to
	// query the chan IDs of the remote party. We'll keep track of the
	// number of queries made using the iterated value, which starts at one
	// due the initial contribution of the QueryChannelRange msgs.
	for i := 1; i < numTotalQueries; i++ {
		expDelayResponse := i >= numUndelayedQueries
		queryBatch(t,
			msgChan1, msgChan2,
			syncer1, syncer2,
			chanSeries2,
			expDelayResponse,
			delayedQueryInterval,
			delayTolerance,
		)
	}
}

// queryBatch is a helper method that will query for a single batch of channels
// from a peer and assert the responses. The method can also be used to assert
// the same transition happens, but is delayed by the remote peer's DOS
// rate-limiting. The provided chanSeries should belong to syncer2.
//
// The state transition performed is the following:
//   syncer1  -- QueryShortChanIDs -->   syncer2
//                                       chanSeries.FetchChanAnns()
//   syncer1 <-- ReplyShortChanIDsEnd -- syncer2
//
// If expDelayResponse is true, this method will assert that the call the
// FetchChanAnns happens between:
//   [delayedQueryInterval-delayTolerance, delayedQueryInterval+delayTolerance].
func queryBatch(t *testing.T,
	msgChan1, msgChan2 chan []lnwire.Message,
	syncer1, syncer2 *GossipSyncer,
	chanSeries *mockChannelGraphTimeSeries,
	expDelayResponse bool,
	delayedQueryInterval, delayTolerance time.Duration) {

	t.Helper()

	// First, we'll assert that syncer1 sends a QueryShortChanIDs message to
	// the remote peer.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("didn't get msg from syncer2")

	case msgs := <-msgChan1:
		for _, msg := range msgs {
			// The message MUST be a QueryShortChanIDs message.
			_, ok := msg.(*lnwire.QueryShortChanIDs)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"QueryShortChanIDs for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer2.queryMsgs <- msg:
			}
		}
	}

	// We'll then respond to with an empty set of replies (as it doesn't
	// affect the test).
	switch {

	// If this query has surpassed the undelayed query threshold, we will
	// impose stricter timing constraints on the response times. We'll first
	// test that syncer2's chanSeries doesn't immediately receive a query,
	// and then check that the query hasn't gone unanswered entirely.
	case expDelayResponse:
		// Create a before and after timeout to test, our test
		// will ensure the messages are delivered to the peer
		// in this timeframe.
		before := time.After(
			delayedQueryInterval - delayTolerance,
		)
		after := time.After(
			delayedQueryInterval + delayTolerance,
		)

		// First, ensure syncer2 doesn't try to respond up until the
		// before time fires.
		select {
		case <-before:
			// Query is delayed, proceed.

		case <-chanSeries.annReq:
			t.Fatalf("DOSy query was not delayed")
		}

		// If syncer2 doesn't attempt a response within the allowed
		// interval, then the messages are probably lost.
		select {
		case <-after:
			t.Fatalf("no delayed query received")

		case <-chanSeries.annReq:
			chanSeries.annResp <- []lnwire.Message{}
		}

	// Otherwise, syncer2 should query its chanSeries promtly.
	default:
		select {
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("no query recvd")

		case <-chanSeries.annReq:
			chanSeries.annResp <- []lnwire.Message{}
		}
	}

	// Finally, assert that syncer2 replies to syncer1 with a
	// ReplyShortChanIDsEnd.
	select {
	case <-time.After(50 * time.Millisecond):
		t.Fatalf("didn't get msg from syncer2")

	case msgs := <-msgChan2:
		for _, msg := range msgs {
			// The message MUST be a ReplyShortChanIDsEnd message.
			_, ok := msg.(*lnwire.ReplyShortChanIDsEnd)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"ReplyShortChanIDsEnd for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer1.gossipMsgs <- msg:
			}
		}
	}
}

// TestGossipSyncerRoutineSync tests all state transitions of the main syncer
// goroutine. This ensures that given an encounter with a peer that has a set
// of distinct channels, then we'll properly synchronize our channel state with
// them.
func TestGossipSyncerRoutineSync(t *testing.T) {
	t.Parallel()

	// We'll modify the chunk size to be a smaller value, so we can ensure
	// our chunk parsing works properly. With this value we should get 3
	// queries: two full chunks, and one lingering chunk.
	const chunkSize = 2

	// First, we'll create two GossipSyncer instances with a canned
	// sendToPeer message to allow us to intercept their potential sends.
	highestID := lnwire.ShortChannelID{
		BlockHeight: 1144,
	}
	msgChan1, syncer1, chanSeries1 := newTestSyncer(
		highestID, defaultEncoding, chunkSize, true, false,
	)
	syncer1.Start()
	defer syncer1.Stop()

	msgChan2, syncer2, chanSeries2 := newTestSyncer(
		highestID, defaultEncoding, chunkSize, false, true,
	)
	syncer2.Start()
	defer syncer2.Stop()

	// Although both nodes are at the same height, syncer will have 3 chan
	// ID's that syncer1 doesn't know of.
	syncer2Chans := []lnwire.ShortChannelID{
		{BlockHeight: highestID.BlockHeight - 3},
		{BlockHeight: highestID.BlockHeight - 2},
		{BlockHeight: highestID.BlockHeight - 1},
	}

	// We'll kick off the test by passing over the QueryChannelRange
	// messages from syncer1 to syncer2.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("didn't get msg from syncer1")

	case msgs := <-msgChan1:
		for _, msg := range msgs {
			// The message MUST be a QueryChannelRange message.
			_, ok := msg.(*lnwire.QueryChannelRange)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"QueryChannelRange for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer2.queryMsgs <- msg:

			}
		}
	}

	// At this point, we'll need to send a response from syncer2 to syncer1
	// using syncer2's channels This will cause syncer1 to simply request
	// the entire set of channels from the other.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries2.filterRangeReqs:
		// We'll send back all the channels that it should know of.
		chanSeries2.filterRangeResp <- syncer2Chans
	}

	// At this point, we'll assert that syncer2 replies with the
	// ReplyChannelRange messages. Two replies are expected since the chunk
	// size is 2, and we need to query for 3 channels.
	for i := 0; i < chunkSize; i++ {
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer2")

		case msgs := <-msgChan2:
			for _, msg := range msgs {
				// The message MUST be a ReplyChannelRange message.
				_, ok := msg.(*lnwire.ReplyChannelRange)
				if !ok {
					t.Fatalf("wrong message: expected "+
						"QueryChannelRange for %T", msg)
				}

				select {
				case <-time.After(time.Second * 2):
					t.Fatalf("node 2 didn't read msg")

				case syncer1.gossipMsgs <- msg:
				}
			}
		}
	}

	// We'll now send back a chunked response from syncer2 back to sycner1.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries1.filterReq:
		chanSeries1.filterResp <- syncer2Chans
	}

	// At this point, syncer1 should start to send out initial requests to
	// query the chan IDs of the remote party. As the chunk size is 2,
	// they'll need 2 rounds in order to fully reconcile the state.
	for i := 0; i < chunkSize; i++ {
		queryBatch(t,
			msgChan1, msgChan2,
			syncer1, syncer2,
			chanSeries2,
			false, 0, 0,
		)
	}

	// At this stage syncer1 should now be sending over its initial
	// GossipTimestampRange messages as it should be fully synced.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("didn't get msg from syncer1")

	case msgs := <-msgChan1:
		for _, msg := range msgs {
			// The message MUST be a GossipTimestampRange message.
			_, ok := msg.(*lnwire.GossipTimestampRange)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"QueryChannelRange for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer2.gossipMsgs <- msg:

			}
		}
	}
}

// TestGossipSyncerAlreadySynced tests that if we attempt to synchronize two
// syncers that have the exact same state, then they'll skip straight to the
// final state and not perform any channel queries.
func TestGossipSyncerAlreadySynced(t *testing.T) {
	t.Parallel()

	// We'll modify the chunk size to be a smaller value, so we can ensure
	// our chunk parsing works properly. With this value we should get 3
	// queries: two full chunks, and one lingering chunk.
	const chunkSize = 2
	const numChans = 3

	// First, we'll create two GossipSyncer instances with a canned
	// sendToPeer message to allow us to intercept their potential sends.
	highestID := lnwire.ShortChannelID{
		BlockHeight: 1144,
	}
	msgChan1, syncer1, chanSeries1 := newTestSyncer(
		highestID, defaultEncoding, chunkSize,
	)
	syncer1.Start()
	defer syncer1.Stop()

	msgChan2, syncer2, chanSeries2 := newTestSyncer(
		highestID, defaultEncoding, chunkSize,
	)
	syncer2.Start()
	defer syncer2.Stop()

	// The channel state of both syncers will be identical. They should
	// recognize this, and skip the sync phase below.
	var syncer1Chans, syncer2Chans []lnwire.ShortChannelID
	for i := numChans; i > 0; i-- {
		shortChanID := lnwire.ShortChannelID{
			BlockHeight: highestID.BlockHeight - uint32(i),
		}
		syncer1Chans = append(syncer1Chans, shortChanID)
		syncer2Chans = append(syncer2Chans, shortChanID)
	}

	// We'll now kick off the test by allowing both side to send their
	// QueryChannelRange messages to each other.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("didn't get msg from syncer1")

	case msgs := <-msgChan1:
		for _, msg := range msgs {
			// The message MUST be a QueryChannelRange message.
			_, ok := msg.(*lnwire.QueryChannelRange)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"QueryChannelRange for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer2.queryMsgs <- msg:

			}
		}
	}
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("didn't get msg from syncer2")

	case msgs := <-msgChan2:
		for _, msg := range msgs {
			// The message MUST be a QueryChannelRange message.
			_, ok := msg.(*lnwire.QueryChannelRange)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"QueryChannelRange for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer1.queryMsgs <- msg:

			}
		}
	}

	// We'll now send back the range each side should send over: the set of
	// channels they already know about.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries1.filterRangeReqs:
		// We'll send all the channels that it should know of.
		chanSeries1.filterRangeResp <- syncer1Chans
	}
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries2.filterRangeReqs:
		// We'll send back all the channels that it should know of.
		chanSeries2.filterRangeResp <- syncer2Chans
	}

	// Next, we'll thread through the replies of both parties. As the chunk
	// size is 2, and they both know of 3 channels, it'll take two around
	// and two chunks.
	for i := 0; i < chunkSize; i++ {
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer1")

		case msgs := <-msgChan1:
			for _, msg := range msgs {
				// The message MUST be a ReplyChannelRange message.
				_, ok := msg.(*lnwire.ReplyChannelRange)
				if !ok {
					t.Fatalf("wrong message: expected "+
						"QueryChannelRange for %T", msg)
				}

				select {
				case <-time.After(time.Second * 2):
					t.Fatalf("node 2 didn't read msg")

				case syncer2.gossipMsgs <- msg:
				}
			}
		}
	}
	for i := 0; i < chunkSize; i++ {
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer2")

		case msgs := <-msgChan2:
			for _, msg := range msgs {
				// The message MUST be a ReplyChannelRange message.
				_, ok := msg.(*lnwire.ReplyChannelRange)
				if !ok {
					t.Fatalf("wrong message: expected "+
						"QueryChannelRange for %T", msg)
				}

				select {
				case <-time.After(time.Second * 2):
					t.Fatalf("node 2 didn't read msg")

				case syncer1.gossipMsgs <- msg:
				}
			}
		}
	}

	// Now that both sides have the full responses, we'll send over the
	// channels that they need to filter out. As both sides have the exact
	// same set of channels, they should skip to the final state.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries1.filterReq:
		chanSeries1.filterResp <- []lnwire.ShortChannelID{}
	}
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries2.filterReq:
		chanSeries2.filterResp <- []lnwire.ShortChannelID{}
	}

	// As both parties are already synced, the next message they send to
	// each other should be the GossipTimestampRange message.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("didn't get msg from syncer1")

	case msgs := <-msgChan1:
		for _, msg := range msgs {
			// The message MUST be a GossipTimestampRange message.
			_, ok := msg.(*lnwire.GossipTimestampRange)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"QueryChannelRange for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer2.gossipMsgs <- msg:

			}
		}
	}
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("didn't get msg from syncer1")

	case msgs := <-msgChan2:
		for _, msg := range msgs {
			// The message MUST be a GossipTimestampRange message.
			_, ok := msg.(*lnwire.GossipTimestampRange)
			if !ok {
				t.Fatalf("wrong message: expected "+
					"QueryChannelRange for %T", msg)
			}

			select {
			case <-time.After(time.Second * 2):
				t.Fatalf("node 2 didn't read msg")

			case syncer1.gossipMsgs <- msg:

			}
		}
	}
}

// TestGossipSyncerSyncTransitions ensures that the gossip syncer properly
// carries out its duties when accepting a new sync transition request.
func TestGossipSyncerSyncTransitions(t *testing.T) {
	t.Parallel()

	assertMsgSent := func(t *testing.T, msgChan chan []lnwire.Message,
		msg lnwire.Message) {

		t.Helper()

		var msgSent lnwire.Message
		select {
		case msgs := <-msgChan:
			if len(msgs) != 1 {
				t.Fatal("expected to send a single message at "+
					"a time, got %d", len(msgs))
			}
			msgSent = msgs[0]
		case <-time.After(time.Second):
			t.Fatalf("expected to send %T message", msg)
		}

		if !reflect.DeepEqual(msgSent, msg) {
			t.Fatalf("expected to send message: %v\ngot: %v",
				spew.Sdump(msg), spew.Sdump(msgSent))
		}
	}

	tests := []struct {
		name          string
		entrySyncType SyncerType
		finalSyncType SyncerType
		assert        func(t *testing.T, msgChan chan []lnwire.Message,
			syncer *GossipSyncer)
	}{
		{
			name:          "active to passive",
			entrySyncType: ActiveSync,
			finalSyncType: PassiveSync,
			assert: func(t *testing.T, msgChan chan []lnwire.Message,
				g *GossipSyncer) {

				// When transitioning from active to passive, we
				// should expect to see a new local update
				// horizon sent to the remote peer indicating
				// that it would not like to receive any future
				// updates.
				assertMsgSent(t, msgChan, &lnwire.GossipTimestampRange{
					FirstTimestamp: uint32(zeroTimestamp.Unix()),
					TimestampRange: 0,
				})

				syncState := g.syncState()
				if syncState != chansSynced {
					t.Fatalf("expected syncerState %v, "+
						"got %v", chansSynced, syncState)
				}
			},
		},
		{
			name:          "passive to active",
			entrySyncType: PassiveSync,
			finalSyncType: ActiveSync,
			assert: func(t *testing.T, msgChan chan []lnwire.Message,
				g *GossipSyncer) {

				// When transitioning from historical to active,
				// we should expect to see a new local update
				// horizon sent to the remote peer indicating
				// that it would like to receive any future
				// updates.
				firstTimestamp := uint32(time.Now().Unix())
				assertMsgSent(t, msgChan, &lnwire.GossipTimestampRange{
					FirstTimestamp: firstTimestamp,
					TimestampRange: math.MaxUint32,
				})

				syncState := g.syncState()
				if syncState != chansSynced {
					t.Fatalf("expected syncerState %v, "+
						"got %v", chansSynced, syncState)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// We'll start each test by creating our syncer. We'll
			// initialize it with a state of chansSynced, as that's
			// the only time when it can process sync transitions.
			msgChan, syncer, _ := newTestSyncer(
				lnwire.ShortChannelID{
					BlockHeight: latestKnownHeight,
				},
				defaultEncoding, defaultChunkSize,
			)
			syncer.setSyncState(chansSynced)

			// We'll set the initial syncType to what the test
			// demands.
			syncer.setSyncType(test.entrySyncType)

			// We'll then start the syncer in order to process the
			// request.
			syncer.Start()
			defer syncer.Stop()

			syncer.ProcessSyncTransition(test.finalSyncType)

			// The syncer should now have the expected final
			// SyncerType that the test expects.
			syncType := syncer.SyncType()
			if syncType != test.finalSyncType {
				t.Fatalf("expected syncType %v, got %v",
					test.finalSyncType, syncType)
			}

			// Finally, we'll run a set of assertions for each test
			// to ensure the syncer performed its expected duties
			// after processing its sync transition.
			test.assert(t, msgChan, syncer)
		})
	}
}

// TestGossipSyncerHistoricalSync tests that a gossip syncer can perform a
// historical sync with the remote peer.
func TestGossipSyncerHistoricalSync(t *testing.T) {
	t.Parallel()

	// We'll create a new gossip syncer and manually override its state to
	// chansSynced. This is necessary as the syncer can only process
	// historical sync requests in this state.
	msgChan, syncer, _ := newTestSyncer(
		lnwire.ShortChannelID{BlockHeight: latestKnownHeight},
		defaultEncoding, defaultChunkSize,
	)
	syncer.setSyncType(PassiveSync)
	syncer.setSyncState(chansSynced)

	syncer.Start()
	defer syncer.Stop()

	syncer.historicalSync()

	// We should expect to see a single lnwire.QueryChannelRange message be
	// sent to the remote peer with a FirstBlockHeight of 0.
	expectedMsg := &lnwire.QueryChannelRange{
		FirstBlockHeight: 0,
		NumBlocks:        latestKnownHeight,
	}

	select {
	case msgs := <-msgChan:
		if len(msgs) != 1 {
			t.Fatalf("expected to send a single "+
				"lnwire.QueryChannelRange message, got %d",
				len(msgs))
		}
		if !reflect.DeepEqual(msgs[0], expectedMsg) {
			t.Fatalf("expected to send message: %v\ngot: %v",
				spew.Sdump(expectedMsg), spew.Sdump(msgs[0]))
		}
	case <-time.After(time.Second):
		t.Fatalf("expected to send a lnwire.QueryChannelRange message")
	}
}

// TestGossipSyncerSyncedSignal ensures that we receive a signal when a gossip
// syncer reaches its terminal chansSynced state.
func TestGossipSyncerSyncedSignal(t *testing.T) {
	t.Parallel()

	// We'll create a new gossip syncer and manually override its state to
	// chansSynced.
	_, syncer, _ := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)
	syncer.setSyncState(chansSynced)

	// We'll go ahead and request a signal to be notified of when it reaches
	// this state.
	signalChan := syncer.ResetSyncedSignal()

	// Starting the gossip syncer should cause the signal to be delivered.
	syncer.Start()

	select {
	case <-signalChan:
	case <-time.After(time.Second):
		t.Fatal("expected to receive chansSynced signal")
	}

	syncer.Stop()

	// We'll try this again, but this time we'll request the signal after
	// the syncer is active and has already reached its chansSynced state.
	_, syncer, _ = newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding,
		defaultChunkSize,
	)

	syncer.setSyncState(chansSynced)

	syncer.Start()
	defer syncer.Stop()

	signalChan = syncer.ResetSyncedSignal()

	// The signal should be delivered immediately.
	select {
	case <-signalChan:
	case <-time.After(time.Second):
		t.Fatal("expected to receive chansSynced signal")
	}
}

// TestGossipSyncerMaxChannelRangeReplies ensures that a gossip syncer
// transitions its state after receiving the maximum possible number of replies
// for a single QueryChannelRange message, and that any further replies after
// said limit are not processed.
func TestGossipSyncerMaxChannelRangeReplies(t *testing.T) {
	t.Parallel()

	msgChan, syncer, chanSeries := newTestSyncer(
		lnwire.ShortChannelID{BlockHeight: latestKnownHeight},
		defaultEncoding, defaultChunkSize,
	)

	// We'll tune the maxQueryChanRangeReplies to a more sensible value for
	// the sake of testing.
	syncer.cfg.maxQueryChanRangeReplies = 100

	syncer.Start()
	defer syncer.Stop()

	// Upon initialization, the syncer should submit a QueryChannelRange
	// request.
	var query *lnwire.QueryChannelRange
	select {
	case msgs := <-msgChan:
		require.Len(t, msgs, 1)
		require.IsType(t, &lnwire.QueryChannelRange{}, msgs[0])
		query = msgs[0].(*lnwire.QueryChannelRange)

	case <-time.After(time.Second):
		t.Fatal("expected query channel range request msg")
	}

	// We'll send the maximum number of replies allowed to a
	// QueryChannelRange request with each reply consuming only one block in
	// order to transition the syncer's state.
	for i := uint32(0); i < syncer.cfg.maxQueryChanRangeReplies; i++ {
		reply := &lnwire.ReplyChannelRange{
			ChainHash:        query.ChainHash,
			FirstBlockHeight: query.FirstBlockHeight,
			NumBlocks:        query.NumBlocks,
			ShortChanIDs: []lnwire.ShortChannelID{
				{
					BlockHeight: query.FirstBlockHeight + i,
				},
			},
		}
		reply.FirstBlockHeight = query.FirstBlockHeight + i
		reply.NumBlocks = 1
		require.NoError(t, syncer.ProcessQueryMsg(reply, nil))
	}

	// We should receive a filter request for the syncer's local channels
	// after processing all of the replies. We'll send back a nil response
	// indicating that no new channels need to be synced, so it should
	// transition to its final chansSynced state.
	select {
	case <-chanSeries.filterReq:
	case <-time.After(time.Second):
		t.Fatal("expected local filter request of known channels")
	}
	select {
	case chanSeries.filterResp <- nil:
	case <-time.After(time.Second):
		t.Fatal("timed out sending filter response")
	}
	assertSyncerStatus(t, syncer, chansSynced, ActiveSync)

	// Finally, attempting to process another reply for the same query
	// should result in an error.
	require.Error(t, syncer.ProcessQueryMsg(&lnwire.ReplyChannelRange{
		ChainHash:        query.ChainHash,
		FirstBlockHeight: query.FirstBlockHeight,
		NumBlocks:        query.NumBlocks,
		ShortChanIDs: []lnwire.ShortChannelID{
			{
				BlockHeight: query.LastBlockHeight() + 1,
			},
		},
	}, nil))
}

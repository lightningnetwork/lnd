package discovery

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	defaultEncoding = lnwire.EncodingSortedPlain
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
	startHeight, endHeight uint32) ([]lnwire.ShortChannelID, error) {

	m.filterRangeReqs <- filterRangeReq{startHeight, endHeight}

	return <-m.filterRangeResp, nil
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

func newTestSyncer(hID lnwire.ShortChannelID,
	encodingType lnwire.ShortChanIDEncoding, chunkSize int32,
) (chan []lnwire.Message, *gossipSyncer, *mockChannelGraphTimeSeries) {

	msgChan := make(chan []lnwire.Message, 20)
	cfg := gossipSyncerCfg{
		syncChanUpdates: true,
		channelSeries:   newMockChannelGraphTimeSeries(hID),
		encodingType:    encodingType,
		chunkSize:       chunkSize,
		sendToPeer: func(msgs ...lnwire.Message) error {
			msgChan <- msgs
			return nil
		},
		delayedQueryReplyInterval: 2 * time.Second,
	}
	syncer := newGossiperSyncer(cfg)

	return msgChan, syncer, cfg.channelSeries.(*mockChannelGraphTimeSeries)
}

// TestGossipSyncerFilterGossipMsgsNoHorizon tests that if the remote peer
// doesn't have a horizon set, then we won't send any incoming messages to it.
func TestGossipSyncerFilterGossipMsgsNoHorizon(t *testing.T) {
	t.Parallel()

	// First, we'll create a gossipSyncer instance with a canned sendToPeer
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

	// First, we'll create a gossipSyncer instance with a canned sendToPeer
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
	go func() {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no query recvd")

		case query := <-chanSeries.updateReq:

			// It should be asking for the chan updates of short
			// chan ID 25.
			expectedID := lnwire.NewShortChanIDFromInt(25)
			if expectedID != query {
				t.Fatalf("wrong query id: expected %v, got %v",
					expectedID, query)
			}

			// If so, then we'll send back the missing update.
			chanSeries.updateResp <- []*lnwire.ChannelUpdate{
				{
					ShortChannelID: lnwire.NewShortChanIDFromInt(25),
					Timestamp:      unixStamp(5),
				},
			}
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
}

// TestGossipSyncerApplyGossipFilter tests that once a gossip filter is applied
// for the remote peer, then we send the peer all known messages which are
// within their desired time horizon.
func TestGossipSyncerApplyGossipFilter(t *testing.T) {
	t.Parallel()

	// First, we'll create a gossipSyncer instance with a canned sendToPeer
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
	go func() {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no query recvd")

		case query := <-chanSeries.horizonReq:
			// The syncer should have translated the time range
			// into the proper star time.
			if remoteHorizon.FirstTimestamp != uint32(query.start.Unix()) {
				t.Fatalf("wrong query stamp: expected %v, got %v",
					remoteHorizon.FirstTimestamp, query.start)
			}

			// For this first response, we'll send back an empty
			// set of messages. As result, we shouldn't send any
			// messages.
			chanSeries.horizonResp <- []lnwire.Message{}
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

	// If we repeat the process, but give the syncer a set of valid
	// messages, then these should be sent to the remote peer.
	go func() {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no query recvd")

		case query := <-chanSeries.horizonReq:
			// The syncer should have translated the time range
			// into the proper star time.
			if remoteHorizon.FirstTimestamp != uint32(query.start.Unix()) {
				t.Fatalf("wrong query stamp: expected %v, got %v",
					remoteHorizon.FirstTimestamp, query.start)
			}

			// For this first response, we'll send back a proper
			// set of messages that should be echoed back.
			chanSeries.horizonResp <- []lnwire.Message{
				&lnwire.ChannelUpdate{
					ShortChannelID: lnwire.NewShortChanIDFromInt(25),
					Timestamp:      unixStamp(5),
				},
			}
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
}

// TestGossipSyncerReplyShortChanIDsWrongChainHash tests that if we get a chan
// ID query for the wrong chain, then we send back only a short ID end with
// complete=0.
func TestGossipSyncerReplyShortChanIDsWrongChainHash(t *testing.T) {
	t.Parallel()

	// First, we'll create a gossipSyncer instance with a canned sendToPeer
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

	// First, we'll create a gossipSyncer instance with a canned sendToPeer
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
	go func() {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no query recvd")

		case chanIDs := <-chanSeries.annReq:
			// The set of chan ID's should match exactly.
			if !reflect.DeepEqual(chanIDs, queryChanIDs) {
				t.Fatalf("wrong chan IDs: expected %v, got %v",
					queryChanIDs, chanIDs)
			}

			// If they do, then we'll send back a response with
			// some canned messages.
			chanSeries.annResp <- queryReply
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

	select {
	case <-time.After(time.Second * 15):
		t.Fatalf("no msgs received")

		// We should get back exactly 4 messages. The first 3 are the same
		// messages we sent above, and the query end message.
	case msgs := <-msgChan:
		if len(msgs) != 4 {
			t.Fatalf("wrong messages: expected %v, got %v",
				4, len(msgs))
		}

		if !reflect.DeepEqual(queryReply, msgs[:3]) {
			t.Fatalf("wrong set of messages: expected %v, got %v",
				spew.Sdump(queryReply), spew.Sdump(msgs[:3]))
		}

		finalMsg, ok := msgs[3].(*lnwire.ReplyShortChanIDsEnd)
		if !ok {
			t.Fatalf("expected lnwire.ReplyShortChanIDsEnd "+
				"instead got %T", msgs[3])
		}
		if finalMsg.Complete != 1 {
			t.Fatalf("complete wasn't set")
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
	query := &lnwire.QueryChannelRange{
		FirstBlockHeight: 100,
		NumBlocks:        50,
	}

	// We'll then launch a goroutine to reply to the query with a set of 5
	// responses. This will ensure we get two full chunks, and one partial
	// chunk.
	resp := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(1),
		lnwire.NewShortChanIDFromInt(2),
		lnwire.NewShortChanIDFromInt(3),
		lnwire.NewShortChanIDFromInt(4),
		lnwire.NewShortChanIDFromInt(5),
	}
	go func() {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no query recvd")

		case filterReq := <-chanSeries.filterRangeReqs:
			// We should be querying for block 100 to 150.
			if filterReq.startHeight != 100 && filterReq.endHeight != 150 {
				t.Fatalf("wrong height range: %v", spew.Sdump(filterReq))
			}

			// If the proper request was sent, then we'll respond
			// with our set of short channel ID's.
			chanSeries.filterRangeResp <- resp
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

			// If this is not the last chunk, then Complete should
			// be set to zero. Otherwise, it should be one.
			switch {
			case i < 2 && rangeResp.Complete != 0:
				t.Fatalf("non-final chunk should have "+
					"Complete=0: %v", spew.Sdump(rangeResp))

			case i == 2 && rangeResp.Complete != 1:
				t.Fatalf("final chunk should have "+
					"Complete=1: %v", spew.Sdump(rangeResp))
			}

			respMsgs = append(respMsgs, rangeResp.ShortChanIDs...)
		}
	}

	// We should get back exactly 5 short chan ID's, and they should match
	// exactly the ID's we sent as a reply.
	if len(respMsgs) != len(resp) {
		t.Fatalf("expected %v chan ID's, instead got %v",
			len(resp), spew.Sdump(respMsgs))
	}
	if !reflect.DeepEqual(resp, respMsgs) {
		t.Fatalf("mismatched response: expected %v, got %v",
			spew.Sdump(resp), spew.Sdump(respMsgs))
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
	go func() {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no query recvd")

		case filterReq := <-chanSeries.filterRangeReqs:
			// We should be querying for block 100 to 150.
			if filterReq.startHeight != 100 && filterReq.endHeight != 150 {
				t.Fatalf("wrong height range: %v",
					spew.Sdump(filterReq))
			}

			// If the proper request was sent, then we'll respond
			// with our blank set of short chan ID's.
			chanSeries.filterRangeResp <- resp
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
}

// TestGossipSyncerGenChanRangeQuery tests that given the current best known
// channel ID, we properly generate an correct initial channel range response.
func TestGossipSyncerGenChanRangeQuery(t *testing.T) {
	t.Parallel()

	// First, we'll create a gossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	const startingHeight = 200
	_, syncer, _ := newTestSyncer(
		lnwire.ShortChannelID{BlockHeight: startingHeight},
		defaultEncoding, defaultChunkSize,
	)

	// If we now ask the syncer to generate an initial range query, it
	// should return a start height that's back chanRangeQueryBuffer
	// blocks.
	rangeQuery, err := syncer.genChanRangeQuery()
	if err != nil {
		t.Fatalf("unable to resp: %v", err)
	}

	firstHeight := uint32(startingHeight - chanRangeQueryBuffer)
	if rangeQuery.FirstBlockHeight != firstHeight {
		t.Fatalf("incorrect chan range query: expected %v, %v",
			rangeQuery.FirstBlockHeight,
			startingHeight-chanRangeQueryBuffer)
	}
	if rangeQuery.NumBlocks != math.MaxUint32-firstHeight {
		t.Fatalf("wrong num blocks: expected %v, got %v",
			rangeQuery.NumBlocks, math.MaxUint32-firstHeight)
	}
}

// TestGossipSyncerProcessChanRangeReply tests that we'll properly buffer
// replied channel replies until we have the complete version. If no new
// channels were discovered, then we should go directly to the chanSsSynced
// state. Otherwise, we should go to the queryNewChannels states.
func TestGossipSyncerProcessChanRangeReply(t *testing.T) {
	t.Parallel()

	// First, we'll create a gossipSyncer instance with a canned sendToPeer
	// message to allow us to intercept their potential sends.
	_, syncer, chanSeries := newTestSyncer(
		lnwire.NewShortChanIDFromInt(10), defaultEncoding, defaultChunkSize,
	)

	startingState := syncer.state

	replies := []*lnwire.ReplyChannelRange{
		{
			ShortChanIDs: []lnwire.ShortChannelID{
				lnwire.NewShortChanIDFromInt(10),
			},
		},
		{
			ShortChanIDs: []lnwire.ShortChannelID{
				lnwire.NewShortChanIDFromInt(11),
			},
		},
		{
			Complete: 1,
			ShortChanIDs: []lnwire.ShortChannelID{
				lnwire.NewShortChanIDFromInt(12),
			},
		},
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
		lnwire.NewShortChanIDFromInt(10),
		lnwire.NewShortChanIDFromInt(11),
		lnwire.NewShortChanIDFromInt(12),
	}

	// As we're about to send the final response, we'll launch a goroutine
	// to respond back with a filtered set of chan ID's.
	go func() {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no query recvd")

		case req := <-chanSeries.filterReq:
			// We should get a request for the entire range of short
			// chan ID's.
			if !reflect.DeepEqual(expectedReq, req) {
				t.Fatalf("wrong request: expected %v, got %v",
					expectedReq, req)
			}

			// We'll send back only the last two to simulate filtering.
			chanSeries.filterResp <- expectedReq[1:]
		}
	}()

	// If we send the final message, then we should transition to
	// queryNewChannels as we've sent a non-empty set of new channels.
	if err := syncer.processChanRangeReply(replies[2]); err != nil {
		t.Fatalf("unable to process reply: %v", err)
	}

	if syncer.SyncState() != queryNewChannels {
		t.Fatalf("wrong state: expected %v instead got %v",
			queryNewChannels, syncer.state)
	}
	if !reflect.DeepEqual(syncer.newChansToQuery, expectedReq[1:]) {
		t.Fatalf("wrong set of chans to query: expected %v, got %v",
			syncer.newChansToQuery, expectedReq[1:])
	}

	// We'll repeat our final reply again, but this time we won't send any
	// new channels. As a result, we should transition over to the
	// chansSynced state.
	go func() {
		select {
		case <-time.After(time.Second * 15):
			t.Fatalf("no query recvd")

		case req := <-chanSeries.filterReq:
			// We should get a request for the entire range of short
			// chan ID's.
			if !reflect.DeepEqual(expectedReq[2], req[0]) {
				t.Fatalf("wrong request: expected %v, got %v",
					expectedReq[2], req[0])
			}

			// We'll send back only the last two to simulate filtering.
			chanSeries.filterResp <- []lnwire.ShortChannelID{}
		}
	}()
	if err := syncer.processChanRangeReply(replies[2]); err != nil {
		t.Fatalf("unable to process reply: %v", err)
	}

	if syncer.SyncState() != chansSynced {
		t.Fatalf("wrong state: expected %v instead got %v",
			chansSynced, syncer.state)
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

	// First, we'll create a gossipSyncer instance with a canned sendToPeer
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

	// First, we'll create two gossipSyncer instances with a canned
	// sendToPeer message to allow us to intercept their potential sends.
	startHeight := lnwire.ShortChannelID{
		BlockHeight: 1144,
	}
	msgChan1, syncer1, chanSeries1 := newTestSyncer(
		startHeight, defaultEncoding, chunkSize,
	)
	syncer1.Start()
	defer syncer1.Stop()

	msgChan2, syncer2, chanSeries2 := newTestSyncer(
		startHeight, defaultEncoding, chunkSize,
	)
	syncer2.Start()
	defer syncer2.Stop()

	// Record the delayed query reply interval used by each syncer.
	delayedQueryInterval := syncer1.cfg.delayedQueryReplyInterval

	// Record the number of undelayed queries allowed by the syncers.
	numUndelayedQueries := syncer1.cfg.maxUndelayedQueryReplies

	// We will send enough queries to exhaust the undelayed responses, and
	// then send two more queries which should be delayed.
	numQueryResponses := numUndelayedQueries + numDelayedQueries

	// The total number of responses must include the initial reply each
	// syner will make to QueryChannelRange.
	numTotalQueries := 1 + numQueryResponses

	// The total number of channels each syncer needs to request must be
	// scaled by the chunk size being used.
	numTotalChans := numQueryResponses * chunkSize

	// Although both nodes are at the same height, they'll have a
	// completely disjoint set of chan ID's that they know of.
	var syncer1Chans []lnwire.ShortChannelID
	for i := 0; i < numTotalChans; i++ {
		syncer1Chans = append(
			syncer1Chans, lnwire.NewShortChanIDFromInt(uint64(i)),
		)
	}
	var syncer2Chans []lnwire.ShortChannelID
	for i := numTotalChans; i < numTotalChans+numTotalChans; i++ {
		syncer2Chans = append(
			syncer2Chans, lnwire.NewShortChanIDFromInt(uint64(i)),
		)
	}

	// We'll kick off the test by passing over the QueryChannelRange
	// messages from one node to the other.
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

			case syncer2.gossipMsgs <- msg:

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

			case syncer1.gossipMsgs <- msg:

			}
		}
	}

	// At this point, we'll need to send responses to both nodes from their
	// respective channel series. Both nodes will simply request the entire
	// set of channels from the other. This will count as the first
	// undelayed response for each syncer.
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

	// At this point, we'll forward the ReplyChannelRange messages to both
	// parties. After receiving the set of channels known to the remote peer
	for i := 0; i < numQueryResponses; i++ {
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

	// We'll now send back a chunked response for both parties of the known
	// short chan ID's.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries1.filterReq:
		chanSeries1.filterResp <- syncer2Chans
	}
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries2.filterReq:
		chanSeries2.filterResp <- syncer1Chans
	}

	// At this point, both parties should start to send out initial
	// requests to query the chan IDs of the remote party. We'll keep track
	// of the number of queries made using the iterated value, which starts
	// at one due the initial contribution of the QueryChannelRange msgs.
	for i := 1; i < numTotalQueries; i++ {
		// Both parties should now have sent out the initial requests
		// to query the chan IDs of the other party.
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer1")

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

				case syncer2.gossipMsgs <- msg:

				}
			}
		}
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer2")

		case msgs := <-msgChan2:
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

				case syncer1.gossipMsgs <- msg:

				}
			}
		}

		// We'll then respond to both parties with an empty set of
		// replies (as it doesn't affect the test).
		switch {

		// If this query has surpassed the undelayed query threshold, we
		// will impose stricter timing constraints on the response
		// times. We'll first test that the peers don't immediately
		// receive a query, and then check that both queries haven't
		// gone unanswered entirely.
		case i >= numUndelayedQueries:
			// Create a before and after timeout to test, our test
			// will ensure the messages are delivered to the peers
			// in this timeframe.
			before := time.After(
				delayedQueryInterval - delayTolerance,
			)
			after := time.After(
				delayedQueryInterval + delayTolerance,
			)

			// First, ensure neither peer tries to respond up until
			// the before time fires.
			select {
			case <-before:
				// Queries are delayed, proceed.

			case <-chanSeries1.annReq:
				t.Fatalf("DOSy query was not delayed")

			case <-chanSeries2.annReq:
				t.Fatalf("DOSy query was not delayed")
			}

			// Next, we'll need to test that both queries are
			// received before the after timer expires. To account
			// for ordering, we will try to pull a message from both
			// peers, and then test that the opposite peer also
			// receives the message promptly.
			var (
				firstChanSeries *mockChannelGraphTimeSeries
				laterChanSeries *mockChannelGraphTimeSeries
			)

			// If neither peer attempts a response within the
			// allowed interval, then the messages are probably
			// lost. Otherwise, process the message and record the
			// induced ordering.
			select {
			case <-after:
				t.Fatalf("no delayed query received")

			case <-chanSeries1.annReq:
				chanSeries1.annResp <- []lnwire.Message{}
				firstChanSeries = chanSeries1
				laterChanSeries = chanSeries2

			case <-chanSeries2.annReq:
				chanSeries2.annResp <- []lnwire.Message{}
				firstChanSeries = chanSeries2
				laterChanSeries = chanSeries1
			}

			// Finally, using the same interval timeout as before,
			// ensure the later peer also responds promptly. We also
			// assert that the first peer doesn't attempt another
			// response.
			select {
			case <-after:
				t.Fatalf("no delayed query received")

			case <-firstChanSeries.annReq:
				t.Fatalf("spurious undelayed response")

			case <-laterChanSeries.annReq:
				laterChanSeries.annResp <- []lnwire.Message{}
			}

		// Otherwise, we still haven't exceeded our undelayed query
		// limit. Assert that both peers promptly attempt a response to
		// the queries.
		default:
			select {
			case <-time.After(50 * time.Millisecond):
				t.Fatalf("no query recvd")

			case <-chanSeries1.annReq:
				chanSeries1.annResp <- []lnwire.Message{}
			}
			select {
			case <-time.After(50 * time.Millisecond):
				t.Fatalf("no query recvd")

			case <-chanSeries2.annReq:
				chanSeries2.annResp <- []lnwire.Message{}
			}
		}

		// Finally, both sides should then receive a
		// ReplyShortChanIDsEnd as the first chunk has been replied to.
		select {
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("didn't get msg from syncer1")

		case msgs := <-msgChan1:
			for _, msg := range msgs {
				// The message MUST be a ReplyShortChanIDsEnd message.
				_, ok := msg.(*lnwire.ReplyShortChanIDsEnd)
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

	// First, we'll create two gossipSyncer instances with a canned
	// sendToPeer message to allow us to intercept their potential sends.
	startHeight := lnwire.ShortChannelID{
		BlockHeight: 1144,
	}
	msgChan1, syncer1, chanSeries1 := newTestSyncer(
		startHeight, defaultEncoding, chunkSize,
	)
	syncer1.Start()
	defer syncer1.Stop()

	msgChan2, syncer2, chanSeries2 := newTestSyncer(
		startHeight, defaultEncoding, chunkSize,
	)
	syncer2.Start()
	defer syncer2.Stop()

	// Although both nodes are at the same height, they'll have a
	// completely disjoint set of 3 chan ID's that they know of.
	syncer1Chans := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(1),
		lnwire.NewShortChanIDFromInt(2),
		lnwire.NewShortChanIDFromInt(3),
	}
	syncer2Chans := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(4),
		lnwire.NewShortChanIDFromInt(5),
		lnwire.NewShortChanIDFromInt(6),
	}

	// We'll kick off the test by passing over the QueryChannelRange
	// messages from one node to the other.
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

			case syncer2.gossipMsgs <- msg:

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

			case syncer1.gossipMsgs <- msg:

			}
		}
	}

	// At this point, we'll need to send responses to both nodes from their
	// respective channel series. Both nodes will simply request the entire
	// set of channels from the other.
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

	// At this point, we'll forward the ReplyChannelRange messages to both
	// parties. Two replies are expected since the chunk size is 2, and we
	// need to query for 3 channels.
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

	// We'll now send back a chunked response for both parties of the known
	// short chan ID's.
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries1.filterReq:
		chanSeries1.filterResp <- syncer2Chans
	}
	select {
	case <-time.After(time.Second * 2):
		t.Fatalf("no query recvd")

	case <-chanSeries2.filterReq:
		chanSeries2.filterResp <- syncer1Chans
	}

	// At this point, both parties should start to send out initial
	// requests to query the chan IDs of the remote party. As the chunk
	// size is 2, they'll need 2 rounds in order to fully reconcile the
	// state.
	for i := 0; i < chunkSize; i++ {
		// Both parties should now have sent out the initial requests
		// to query the chan IDs of the other party.
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer1")

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

				case syncer2.gossipMsgs <- msg:

				}
			}
		}
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer2")

		case msgs := <-msgChan2:
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

				case syncer1.gossipMsgs <- msg:

				}
			}
		}

		// We'll then respond to both parties with an empty set of replies (as
		// it doesn't affect the test).
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("no query recvd")

		case <-chanSeries1.annReq:
			chanSeries1.annResp <- []lnwire.Message{}
		}
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("no query recvd")

		case <-chanSeries2.annReq:
			chanSeries2.annResp <- []lnwire.Message{}
		}

		// Both sides should then receive a ReplyShortChanIDsEnd as the first
		// chunk has been replied to.
		select {
		case <-time.After(time.Second * 2):
			t.Fatalf("didn't get msg from syncer1")

		case msgs := <-msgChan1:
			for _, msg := range msgs {
				// The message MUST be a ReplyShortChanIDsEnd message.
				_, ok := msg.(*lnwire.ReplyShortChanIDsEnd)
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

	// At this stage both parties should now be sending over their initial
	// GossipTimestampRange messages as they should both be fully synced.
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

// TestGossipSyncerAlreadySynced tests that if we attempt to synchronize two
// syncers that have the exact same state, then they'll skip straight to the
// final state and not perform any channel queries.
func TestGossipSyncerAlreadySynced(t *testing.T) {
	t.Parallel()

	// We'll modify the chunk size to be a smaller value, so we can ensure
	// our chunk parsing works properly. With this value we should get 3
	// queries: two full chunks, and one lingering chunk.
	const chunkSize = 2

	// First, we'll create two gossipSyncer instances with a canned
	// sendToPeer message to allow us to intercept their potential sends.
	startHeight := lnwire.ShortChannelID{
		BlockHeight: 1144,
	}
	msgChan1, syncer1, chanSeries1 := newTestSyncer(
		startHeight, defaultEncoding, chunkSize,
	)
	syncer1.Start()
	defer syncer1.Stop()

	msgChan2, syncer2, chanSeries2 := newTestSyncer(
		startHeight, defaultEncoding, chunkSize,
	)
	syncer2.Start()
	defer syncer2.Stop()

	// The channel state of both syncers will be identical. They should
	// recognize this, and skip the sync phase below.
	syncer1Chans := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(1),
		lnwire.NewShortChanIDFromInt(2),
		lnwire.NewShortChanIDFromInt(3),
	}
	syncer2Chans := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(1),
		lnwire.NewShortChanIDFromInt(2),
		lnwire.NewShortChanIDFromInt(3),
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

			case syncer2.gossipMsgs <- msg:

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

			case syncer1.gossipMsgs <- msg:

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

package channeldb

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestForwardingLogBasicStorageAndQuery tests that we're able to store and
// then query for items that have previously been added to the event log.
func TestForwardingLogBasicStorageAndQuery(t *testing.T) {
	t.Parallel()

	// First, we'll set up a test database, and use that to instantiate the
	// forwarding event log that we'll be using for the duration of the
	// test.
	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	initialTime := time.Unix(1234, 0)
	timestamp := time.Unix(1234, 0)

	// We'll create 100 random events, which each event being spaced 10
	// minutes after the prior event.
	numEvents := 100
	events := make([]ForwardingEvent, numEvents)
	for i := 0; i < numEvents; i++ {
		events[i] = ForwardingEvent{
			Timestamp:      timestamp,
			IncomingChanID: lnwire.NewShortChanIDFromInt(uint64(rand.Int63())),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(uint64(rand.Int63())),
			AmtIn:          lnwire.MilliSatoshi(rand.Int63()),
			AmtOut:         lnwire.MilliSatoshi(rand.Int63()),
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}

		timestamp = timestamp.Add(time.Minute * 10)
	}

	// Now that all of our set of events constructed, we'll add them to the
	// database in a batch manner.
	if err := log.AddForwardingEvents(events); err != nil {
		t.Fatalf("unable to add events: %v", err)
	}

	// With our events added we'll now construct a basic query to retrieve
	// all of the events.
	eventQuery := ForwardingEventQuery{
		StartTime:    initialTime,
		EndTime:      timestamp,
		IndexOffset:  0,
		NumMaxEvents: 1000,
	}
	timeSlice, err := log.Query(eventQuery)
	require.NoError(t, err, "unable to query for events")

	// The set of returned events should match identically, as they should
	// be returned in sorted order.
	if !reflect.DeepEqual(events, timeSlice.ForwardingEvents) {
		t.Fatalf("event mismatch: expected %v vs %v",
			spew.Sdump(events), spew.Sdump(timeSlice.ForwardingEvents))
	}

	// The offset index of the final entry should be numEvents, so the
	// number of total events we've written.
	if timeSlice.LastIndexOffset != uint32(numEvents) {
		t.Fatalf("wrong final offset: expected %v, got %v",
			timeSlice.LastIndexOffset, numEvents)
	}
}

// TestForwardingLogQueryOptions tests that the query offset works properly. So
// if we add a series of events, then we should be able to seek within the
// timeslice accordingly. This exercises the index offset and num max event
// field in the query, and also the last index offset field int he response.
func TestForwardingLogQueryOptions(t *testing.T) {
	t.Parallel()

	// First, we'll set up a test database, and use that to instantiate the
	// forwarding event log that we'll be using for the duration of the
	// test.
	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	initialTime := time.Unix(1234, 0)
	endTime := time.Unix(1234, 0)

	// We'll create 20 random events, which each event being spaced 10
	// minutes after the prior event.
	numEvents := 20
	events := make([]ForwardingEvent, numEvents)
	for i := 0; i < numEvents; i++ {
		events[i] = ForwardingEvent{
			Timestamp:      endTime,
			IncomingChanID: lnwire.NewShortChanIDFromInt(uint64(rand.Int63())),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(uint64(rand.Int63())),
			AmtIn:          lnwire.MilliSatoshi(rand.Int63()),
			AmtOut:         lnwire.MilliSatoshi(rand.Int63()),
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}

		endTime = endTime.Add(time.Minute * 10)
	}

	// Now that all of our set of events constructed, we'll add them to the
	// database in a batch manner.
	if err := log.AddForwardingEvents(events); err != nil {
		t.Fatalf("unable to add events: %v", err)
	}

	// With all of our events added, we should be able to query for the
	// first 10 events using the max event query field.
	eventQuery := ForwardingEventQuery{
		StartTime:    initialTime,
		EndTime:      endTime,
		IndexOffset:  0,
		NumMaxEvents: 10,
	}
	timeSlice, err := log.Query(eventQuery)
	require.NoError(t, err, "unable to query for events")

	// We should get exactly 10 events back.
	if len(timeSlice.ForwardingEvents) != 10 {
		t.Fatalf("wrong number of events: expected %v, got %v", 10,
			len(timeSlice.ForwardingEvents))
	}

	// The set of events returned should be the first 10 events that we
	// added.
	if !reflect.DeepEqual(events[:10], timeSlice.ForwardingEvents) {
		t.Fatalf("wrong response: expected %v, got %v",
			spew.Sdump(events[:10]),
			spew.Sdump(timeSlice.ForwardingEvents))
	}

	// The final offset should be the exact number of events returned.
	if timeSlice.LastIndexOffset != 10 {
		t.Fatalf("wrong index offset: expected %v, got %v", 10,
			timeSlice.LastIndexOffset)
	}

	// If we use the final offset to query again, then we should get 10
	// more events, that are the last 10 events we wrote.
	eventQuery.IndexOffset = 10
	timeSlice, err = log.Query(eventQuery)
	require.NoError(t, err, "unable to query for events")

	// We should get exactly 10 events back once again.
	if len(timeSlice.ForwardingEvents) != 10 {
		t.Fatalf("wrong number of events: expected %v, got %v", 10,
			len(timeSlice.ForwardingEvents))
	}

	// The events that we got back should be the last 10 events that we
	// wrote out.
	if !reflect.DeepEqual(events[10:], timeSlice.ForwardingEvents) {
		t.Fatalf("wrong response: expected %v, got %v",
			spew.Sdump(events[10:]),
			spew.Sdump(timeSlice.ForwardingEvents))
	}

	// Finally, the last index offset should be 20, or the number of
	// records we've written out.
	if timeSlice.LastIndexOffset != 20 {
		t.Fatalf("wrong index offset: expected %v, got %v", 20,
			timeSlice.LastIndexOffset)
	}
}

// TestForwardingLogQueryLimit tests that we're able to properly limit the
// number of events that are returned as part of a query.
func TestForwardingLogQueryLimit(t *testing.T) {
	t.Parallel()

	// First, we'll set up a test database, and use that to instantiate the
	// forwarding event log that we'll be using for the duration of the
	// test.
	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	initialTime := time.Unix(1234, 0)
	endTime := time.Unix(1234, 0)

	// We'll create 200 random events, which each event being spaced 10
	// minutes after the prior event.
	numEvents := 200
	events := make([]ForwardingEvent, numEvents)
	for i := 0; i < numEvents; i++ {
		events[i] = ForwardingEvent{
			Timestamp:      endTime,
			IncomingChanID: lnwire.NewShortChanIDFromInt(uint64(rand.Int63())),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(uint64(rand.Int63())),
			AmtIn:          lnwire.MilliSatoshi(rand.Int63()),
			AmtOut:         lnwire.MilliSatoshi(rand.Int63()),
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}

		endTime = endTime.Add(time.Minute * 10)
	}

	// Now that all of our set of events constructed, we'll add them to the
	// database in a batch manner.
	if err := log.AddForwardingEvents(events); err != nil {
		t.Fatalf("unable to add events: %v", err)
	}

	// Once the events have been written out, we'll issue a query over the
	// entire range, but restrict the number of events to the first 100.
	eventQuery := ForwardingEventQuery{
		StartTime:    initialTime,
		EndTime:      endTime,
		IndexOffset:  0,
		NumMaxEvents: 100,
	}
	timeSlice, err := log.Query(eventQuery)
	require.NoError(t, err, "unable to query for events")

	// We should get exactly 100 events back.
	if len(timeSlice.ForwardingEvents) != 100 {
		t.Fatalf("wrong number of events: expected %v, got %v", 10,
			len(timeSlice.ForwardingEvents))
	}

	// The set of events returned should be the first 100 events that we
	// added.
	if !reflect.DeepEqual(events[:100], timeSlice.ForwardingEvents) {
		t.Fatalf("wrong response: expected %v, got %v",
			spew.Sdump(events[:100]),
			spew.Sdump(timeSlice.ForwardingEvents))
	}

	// The final offset should be the exact number of events returned.
	if timeSlice.LastIndexOffset != 100 {
		t.Fatalf("wrong index offset: expected %v, got %v", 100,
			timeSlice.LastIndexOffset)
	}
}

// TestForwardingLogMakeUniqueTimestamps makes sure the function that creates
// unique timestamps does it job correctly.
func TestForwardingLogMakeUniqueTimestamps(t *testing.T) {
	t.Parallel()

	// Create a list of events where some of the timestamps collide. We
	// expect no existing timestamp to be overwritten, instead the "gaps"
	// between them should be filled.
	inputSlice := []ForwardingEvent{
		{Timestamp: time.Unix(0, 1001)},
		{Timestamp: time.Unix(0, 2001)},
		{Timestamp: time.Unix(0, 1001)},
		{Timestamp: time.Unix(0, 1002)},
		{Timestamp: time.Unix(0, 1004)},
		{Timestamp: time.Unix(0, 1004)},
		{Timestamp: time.Unix(0, 1007)},
		{Timestamp: time.Unix(0, 1001)},
	}
	expectedSlice := []ForwardingEvent{
		{Timestamp: time.Unix(0, 1001)},
		{Timestamp: time.Unix(0, 1002)},
		{Timestamp: time.Unix(0, 1003)},
		{Timestamp: time.Unix(0, 1004)},
		{Timestamp: time.Unix(0, 1005)},
		{Timestamp: time.Unix(0, 1006)},
		{Timestamp: time.Unix(0, 1007)},
		{Timestamp: time.Unix(0, 2001)},
	}

	makeUniqueTimestamps(inputSlice)

	for idx, in := range inputSlice {
		expect := expectedSlice[idx]
		assert.Equal(
			t, expect.Timestamp.UnixNano(), in.Timestamp.UnixNano(),
		)
	}
}

// TestForwardingLogStoreEvent makes sure forwarding events are stored without
// colliding on duplicate timestamps.
func TestForwardingLogStoreEvent(t *testing.T) {
	t.Parallel()

	// First, we'll set up a test database, and use that to instantiate the
	// forwarding event log that we'll be using for the duration of the
	// test.
	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	// We'll create 20 random events, with each event having a timestamp
	// with just one nanosecond apart.
	numEvents := 20
	events := make([]ForwardingEvent, numEvents)
	ts := time.Now().UnixNano()
	for i := 0; i < numEvents; i++ {
		events[i] = ForwardingEvent{
			Timestamp:      time.Unix(0, ts+int64(i)),
			IncomingChanID: lnwire.NewShortChanIDFromInt(uint64(rand.Int63())),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(uint64(rand.Int63())),
			AmtIn:          lnwire.MilliSatoshi(rand.Int63()),
			AmtOut:         lnwire.MilliSatoshi(rand.Int63()),
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}
	}

	// Now that all of our events are constructed, we'll add them to the
	// database in a batched manner.
	if err := log.AddForwardingEvents(events); err != nil {
		t.Fatalf("unable to add events: %v", err)
	}

	// Because timestamps are de-duplicated when adding them in a single
	// batch before they even hit the DB, we add the same events again but
	// in a new batch. They now have to be de-duplicated on the DB level.
	if err := log.AddForwardingEvents(events); err != nil {
		t.Fatalf("unable to add second batch of events: %v", err)
	}

	// With all of our events added, we should be able to query for all
	// events with a range of just 40 nanoseconds (2 times 20 events, all
	// spaced one nanosecond apart).
	eventQuery := ForwardingEventQuery{
		StartTime:    time.Unix(0, ts),
		EndTime:      time.Unix(0, ts+int64(numEvents*2)),
		IndexOffset:  0,
		NumMaxEvents: uint32(numEvents * 3),
	}
	timeSlice, err := log.Query(eventQuery)
	require.NoError(t, err, "unable to query for events")

	// We should get exactly 40 events back.
	if len(timeSlice.ForwardingEvents) != numEvents*2 {
		t.Fatalf("wrong number of events: expected %v, got %v",
			numEvents*2, len(timeSlice.ForwardingEvents))
	}

	// The timestamps should be spaced out evenly and in order.
	for i := 0; i < numEvents*2; i++ {
		eventTs := timeSlice.ForwardingEvents[i].Timestamp.UnixNano()
		if eventTs != ts+int64(i) {
			t.Fatalf("unexpected timestamp of event %d: expected "+
				"%d, got %d", i, ts+int64(i), eventTs)
		}
	}
}

// TestForwardingLogDecodeForwardingEvent tests that we're able to decode
// forwarding events that don't have the new incoming and outgoing htlc
// indices.
func TestForwardingLogDecodeForwardingEvent(t *testing.T) {
	t.Parallel()

	// First, we'll set up a test database, and use that to instantiate the
	// forwarding event log that we'll be using for the duration of the
	// test.
	db, err := MakeTestDB(t)
	require.NoError(t, err)

	log := ForwardingLog{
		db: db,
	}

	initialTime := time.Unix(1234, 0)
	endTime := time.Unix(1234, 0)

	// We'll create forwarding events that don't have the incoming and
	// outgoing htlc indices.
	numEvents := 10
	events := make([]ForwardingEvent, numEvents)
	for i := range numEvents {
		events[i] = ForwardingEvent{
			Timestamp: endTime,
			IncomingChanID: lnwire.NewShortChanIDFromInt(
				uint64(rand.Int63()),
			),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(
				uint64(rand.Int63()),
			),
			AmtIn:  lnwire.MilliSatoshi(rand.Int63()),
			AmtOut: lnwire.MilliSatoshi(rand.Int63()),
		}

		endTime = endTime.Add(time.Minute * 10)
	}

	// Now that all of our events are constructed, we'll add them to the
	// database.
	err = writeOldFormatEvents(db, events)
	require.NoError(t, err)

	// With all of our events added, we'll now query for them and ensure
	// that the incoming and outgoing htlc indices are set to 0 (default
	// value) for all events.
	eventQuery := ForwardingEventQuery{
		StartTime:    initialTime,
		EndTime:      endTime,
		IndexOffset:  0,
		NumMaxEvents: uint32(numEvents * 3),
	}
	timeSlice, err := log.Query(eventQuery)
	require.NoError(t, err)
	require.Equal(t, numEvents, len(timeSlice.ForwardingEvents))

	for _, event := range timeSlice.ForwardingEvents {
		require.Equal(t, fn.None[uint64](), event.IncomingHtlcID)
		require.Equal(t, fn.None[uint64](), event.OutgoingHtlcID)
	}
}

// writeOldFormatEvents writes forwarding events to the database in the old
// format (without incoming and outgoing htlc indices). This is used to test
// backward compatibility.
func writeOldFormatEvents(db *DB, events []ForwardingEvent) error {
	return kvdb.Batch(db.Backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(forwardingLogBucket)
		if err != nil {
			return err
		}

		for _, event := range events {
			var timestamp [8]byte
			byteOrder.PutUint64(timestamp[:], uint64(
				event.Timestamp.UnixNano(),
			))

			// Use the old event size (32 bytes) for writing old
			// format events.
			var eventBytes [32]byte
			eventBuf := bytes.NewBuffer(eventBytes[0:0:32])

			// Write only the original fields without incoming and
			// outgoing htlc indices.
			if err := WriteElements(
				eventBuf, event.IncomingChanID,
				event.OutgoingChanID, event.AmtIn, event.AmtOut,
			); err != nil {
				return err
			}

			if err := bucket.Put(
				timestamp[:], eventBuf.Bytes(),
			); err != nil {
				return err
			}
		}

		return nil
	})
}

// TestForwardingLogQueryChanIDs tests that querying the forwarding log with
// various combinations of incoming and/or outgoing channel IDs returns the
// correct subset of forwarding events.
func TestForwardingLogQueryChanIDs(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{db: db}

	initialTime := time.Unix(1234, 0)
	endTime := initialTime

	numEvents := 10
	incomingChanIDs := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(2001),
		lnwire.NewShortChanIDFromInt(2002),
		lnwire.NewShortChanIDFromInt(2003),
	}
	outgoingChanIDs := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(3001),
		lnwire.NewShortChanIDFromInt(3002),
		lnwire.NewShortChanIDFromInt(3003),
	}

	events := make([]ForwardingEvent, numEvents)
	for i := 0; i < numEvents; i++ {
		events[i] = ForwardingEvent{
			Timestamp:      endTime,
			IncomingChanID: incomingChanIDs[i%len(incomingChanIDs)],
			OutgoingChanID: outgoingChanIDs[i%len(outgoingChanIDs)],
			AmtIn:          lnwire.MilliSatoshi(rand.Int63()),
			AmtOut:         lnwire.MilliSatoshi(rand.Int63()),
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}
		endTime = endTime.Add(10 * time.Minute)
	}

	require.NoError(
		t,
		log.AddForwardingEvents(events),
		"unable to add events",
	)

	tests := []struct {
		name     string
		query    ForwardingEventQuery
		expected func(e ForwardingEvent) bool
	}{
		{
			name: "only incomingChanIDs filter",
			query: ForwardingEventQuery{
				StartTime: initialTime,
				EndTime:   endTime,
				IncomingChanIDs: fn.NewSet(
					incomingChanIDs[0].ToUint64(),
					incomingChanIDs[1].ToUint64(),
				),
				IndexOffset:  0,
				NumMaxEvents: 10,
			},
			expected: func(e ForwardingEvent) bool {
				return e.IncomingChanID == incomingChanIDs[0] ||
					e.IncomingChanID == incomingChanIDs[1]
			},
		},
		{
			name: "only outgoingChanIDs filter",
			query: ForwardingEventQuery{
				StartTime: initialTime,
				EndTime:   endTime,
				OutgoingChanIDs: fn.NewSet(
					outgoingChanIDs[0].ToUint64(),
					outgoingChanIDs[1].ToUint64(),
				),
				IndexOffset:  0,
				NumMaxEvents: 10,
			},
			expected: func(e ForwardingEvent) bool {
				return e.OutgoingChanID == outgoingChanIDs[0] ||
					e.OutgoingChanID == outgoingChanIDs[1]
			},
		},
		{
			name: "incoming and outgoingChanIDs filter",
			query: ForwardingEventQuery{
				StartTime: initialTime,
				EndTime:   endTime,
				IncomingChanIDs: fn.NewSet(
					incomingChanIDs[0].ToUint64(),
					incomingChanIDs[1].ToUint64(),
				),
				OutgoingChanIDs: fn.NewSet(
					outgoingChanIDs[0].ToUint64(),
					outgoingChanIDs[1].ToUint64(),
				),
				IndexOffset:  0,
				NumMaxEvents: 10,
			},
			expected: func(e ForwardingEvent) bool {
				return e.IncomingChanID ==
					incomingChanIDs[0] ||
					e.IncomingChanID ==
						incomingChanIDs[1] ||
					e.OutgoingChanID ==
						outgoingChanIDs[0] ||
					e.OutgoingChanID ==
						outgoingChanIDs[1]
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := log.Query(tc.query)
			require.NoError(t, err, "query failed")

			expected := make([]ForwardingEvent, 0)
			for _, e := range events {
				if tc.expected(e) {
					expected = append(expected, e)
				}
			}

			require.Equal(t, expected, result.ForwardingEvents)
		})
	}
}

// TestForwardingLogDeletion tests the basic deletion functionality of the
// forwarding log.
func TestForwardingLogDeletion(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	// Create 50 events spanning 500 minutes (10 min intervals).
	initialTime := time.Unix(1000, 0)
	timestamp := initialTime
	numEvents := 50
	events := make([]ForwardingEvent, numEvents)

	var expectedTotalFees int64
	for i := 0; i < numEvents; i++ {
		amtIn := lnwire.MilliSatoshi(10000 + rand.Intn(5000))
		amtOut := lnwire.MilliSatoshi(9000 + rand.Intn(4000))
		events[i] = ForwardingEvent{
			Timestamp:      timestamp,
			IncomingChanID: lnwire.NewShortChanIDFromInt(uint64(i)),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(uint64(i + 100)),
			AmtIn:          amtIn,
			AmtOut:         amtOut,
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}
		expectedTotalFees += int64(amtIn - amtOut)
		timestamp = timestamp.Add(time.Minute * 10)
	}

	// Add all events to the database.
	err = log.AddForwardingEvents(events)
	require.NoError(t, err, "unable to add events")

	// Delete all events (use timestamp after the last event).
	deleteTime := timestamp.Add(time.Minute)
	stats, err := log.DeleteForwardingEvents(deleteTime, 0)
	require.NoError(t, err, "unable to delete events")

	// Verify statistics.
	assert.Equal(t, uint64(numEvents), stats.NumEventsDeleted,
		"wrong number of events deleted")
	assert.Equal(t, expectedTotalFees, stats.TotalFeeMsat,
		"wrong total fees")

	// Verify all events were deleted by querying.
	query := ForwardingEventQuery{
		StartTime:    initialTime,
		EndTime:      timestamp,
		NumMaxEvents: 1000,
	}
	result, err := log.Query(query)
	require.NoError(t, err, "query failed")
	assert.Empty(t, result.ForwardingEvents, "events should be deleted")
}

// TestForwardingLogPartialDeletion tests that we can delete a subset of events
// based on time.
func TestForwardingLogPartialDeletion(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	initialTime := time.Unix(2000, 0)
	timestamp := initialTime
	numEvents := 100
	events := make([]ForwardingEvent, numEvents)

	for i := 0; i < numEvents; i++ {
		events[i] = ForwardingEvent{
			Timestamp:      timestamp,
			IncomingChanID: lnwire.NewShortChanIDFromInt(uint64(i)),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(
				uint64(i + 100),
			),
			AmtIn:          lnwire.MilliSatoshi(10000),
			AmtOut:         lnwire.MilliSatoshi(9500),
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}
		timestamp = timestamp.Add(time.Minute * 10)
	}

	err = log.AddForwardingEvents(events)
	require.NoError(t, err, "unable to add events")

	// Delete only the first 50 events (events 0-49). The 50th event is at
	// initialTime + 50*10 minutes.
	deleteTime := events[49].Timestamp.Add(time.Nanosecond)
	stats, err := log.DeleteForwardingEvents(deleteTime, 0)
	require.NoError(t, err, "unable to delete events")

	// Should have deleted exactly 50 events.
	assert.Equal(
		t, uint64(50), stats.NumEventsDeleted,
		"wrong number of events deleted",
	)

	// Fee per event is 500 msat, so total should be 50 * 500 = 25000.
	assert.Equal(t, int64(25000), stats.TotalFeeMsat, "wrong total fees")

	// Query to verify remaining events (should be 50 events left).
	query := ForwardingEventQuery{
		StartTime:    initialTime,
		EndTime:      timestamp,
		NumMaxEvents: 1000,
	}
	result, err := log.Query(query)
	require.NoError(t, err, "query failed")
	assert.Len(
		t, result.ForwardingEvents, 50,
		"wrong number of remaining events",
	)

	// The remaining events should be events[50:].
	assert.Equal(
		t, events[50:], result.ForwardingEvents,
		"wrong events remaining",
	)
}

// TestForwardingLogBatchDeletion tests that deletion works correctly with
// different batch sizes.
func TestForwardingLogBatchDeletion(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	initialTime := time.Unix(3000, 0)
	timestamp := initialTime
	numEvents := 250
	events := make([]ForwardingEvent, numEvents)

	for i := 0; i < numEvents; i++ {
		events[i] = ForwardingEvent{
			Timestamp:      timestamp,
			IncomingChanID: lnwire.NewShortChanIDFromInt(uint64(i)),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(uint64(i + 100)),
			AmtIn:          lnwire.MilliSatoshi(10000),
			AmtOut:         lnwire.MilliSatoshi(9000),
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}
		timestamp = timestamp.Add(time.Minute)
	}

	err = log.AddForwardingEvents(events)
	require.NoError(t, err, "unable to add events")

	// Delete with a small batch size to test multiple batches.
	deleteTime := timestamp.Add(time.Minute)
	stats, err := log.DeleteForwardingEvents(deleteTime, 75)
	require.NoError(t, err, "unable to delete events")

	// Should have deleted all events across multiple batches.
	assert.Equal(t, uint64(numEvents), stats.NumEventsDeleted,
		"wrong number of events deleted")

	// Fee per event is 1000 msat.
	expectedFees := int64(numEvents * 1000)
	assert.Equal(t, expectedFees, stats.TotalFeeMsat, "wrong total fees")

	// Verify all deleted.
	query := ForwardingEventQuery{
		StartTime:    initialTime,
		EndTime:      timestamp,
		NumMaxEvents: 1000,
	}
	result, err := log.Query(query)
	require.NoError(t, err, "query failed")
	assert.Empty(t, result.ForwardingEvents, "events should be deleted")
}

// TestForwardingLogDeleteEmpty tests deletion on an empty database.
func TestForwardingLogDeleteEmpty(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	// Try to delete from empty database.
	deleteTime := time.Now()
	stats, err := log.DeleteForwardingEvents(deleteTime, 0)
	require.NoError(t, err, "delete should not error on empty db")

	// Should have deleted 0 events with 0 fees.
	assert.Equal(
		t, uint64(0), stats.NumEventsDeleted,
		"should delete 0 events",
	)
	assert.Equal(
		t, int64(0), stats.TotalFeeMsat, "should have 0 fees",
	)
}

// TestForwardingLogDeleteTimeBoundary tests deletion at exact time boundaries.
func TestForwardingLogDeleteTimeBoundary(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	baseTime := time.Unix(5000, 0)
	events := []ForwardingEvent{
		{
			Timestamp:      baseTime,
			IncomingChanID: lnwire.NewShortChanIDFromInt(1),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(101),
			AmtIn:          10000,
			AmtOut:         9000,
			IncomingHtlcID: fn.Some(uint64(0)),
			OutgoingHtlcID: fn.Some(uint64(0)),
		},
		{
			Timestamp:      baseTime.Add(time.Hour),
			IncomingChanID: lnwire.NewShortChanIDFromInt(2),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(102),
			AmtIn:          10000,
			AmtOut:         9000,
			IncomingHtlcID: fn.Some(uint64(1)),
			OutgoingHtlcID: fn.Some(uint64(1)),
		},
		{
			Timestamp:      baseTime.Add(2 * time.Hour),
			IncomingChanID: lnwire.NewShortChanIDFromInt(3),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(103),
			AmtIn:          10000,
			AmtOut:         9000,
			IncomingHtlcID: fn.Some(uint64(2)),
			OutgoingHtlcID: fn.Some(uint64(2)),
		},
	}

	err = log.AddForwardingEvents(events)
	require.NoError(t, err, "unable to add events")

	// Delete events at exactly the second event's timestamp. This should
	// delete events at baseTime and baseTime+1h.
	deleteTime := baseTime.Add(time.Hour)
	stats, err := log.DeleteForwardingEvents(deleteTime, 0)
	require.NoError(t, err, "unable to delete events")

	// Should delete exactly 2 events (those at or before deleteTime).
	assert.Equal(
		t, uint64(2), stats.NumEventsDeleted,
		"wrong number of events deleted",
	)

	query := ForwardingEventQuery{
		StartTime:    baseTime,
		EndTime:      baseTime.Add(3 * time.Hour),
		NumMaxEvents: 10,
	}
	result, err := log.Query(query)
	require.NoError(t, err, "query failed")

	// We should have 1 event remaining.
	assert.Len(
		t, result.ForwardingEvents, 1, "wrong number remaining",
	)
	assert.Equal(
		t, events[2], result.ForwardingEvents[0],
		"wrong event remaining",
	)
}

// TestForwardingLogDeleteMaxBatchSize tests that the max batch size is
// enforced.
func TestForwardingLogDeleteMaxBatchSize(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	// Create some events.
	initialTime := time.Unix(6000, 0)
	events := []ForwardingEvent{
		{
			Timestamp:      initialTime,
			IncomingChanID: lnwire.NewShortChanIDFromInt(1),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(101),
			AmtIn:          10000,
			AmtOut:         9000,
			IncomingHtlcID: fn.Some(uint64(0)),
			OutgoingHtlcID: fn.Some(uint64(0)),
		},
	}

	err = log.AddForwardingEvents(events)
	require.NoError(t, err, "unable to add events")

	// Try to delete with a batch size larger than MaxResponseEvents.
	deleteTime := initialTime.Add(time.Hour)
	stats, err := log.DeleteForwardingEvents(
		deleteTime, MaxResponseEvents+1000,
	)
	require.NoError(t, err, "delete should succeed")

	// Should have deleted the event (batch size should be capped).
	assert.Equal(
		t, uint64(1), stats.NumEventsDeleted, "event should be deleted",
	)
}

// TestForwardingLogDeleteIdempotent tests that deletion is idempotent.
func TestForwardingLogDeleteIdempotent(t *testing.T) {
	t.Parallel()

	db, err := MakeTestDB(t)
	require.NoError(t, err, "unable to make test db")

	log := ForwardingLog{
		db: db,
	}

	// Create events.
	initialTime := time.Unix(7000, 0)
	timestamp := initialTime
	events := make([]ForwardingEvent, 10)
	for i := 0; i < 10; i++ {
		events[i] = ForwardingEvent{
			Timestamp:      timestamp,
			IncomingChanID: lnwire.NewShortChanIDFromInt(uint64(i)),
			OutgoingChanID: lnwire.NewShortChanIDFromInt(uint64(i + 100)),
			AmtIn:          lnwire.MilliSatoshi(10000),
			AmtOut:         lnwire.MilliSatoshi(9000),
			IncomingHtlcID: fn.Some(uint64(i)),
			OutgoingHtlcID: fn.Some(uint64(i)),
		}
		timestamp = timestamp.Add(time.Minute)
	}

	err = log.AddForwardingEvents(events)
	require.NoError(t, err, "unable to add events")

	deleteTime := timestamp
	stats1, err := log.DeleteForwardingEvents(deleteTime, 0)
	require.NoError(t, err, "first delete failed")
	assert.Equal(t, uint64(10), stats1.NumEventsDeleted)

	// Delete again with same time - should delete 0 events.
	stats2, err := log.DeleteForwardingEvents(deleteTime, 0)
	require.NoError(t, err, "second delete failed")
	assert.Equal(
		t, uint64(0), stats2.NumEventsDeleted, "should be idempotent",
	)
	assert.Equal(t, int64(0), stats2.TotalFeeMsat, "should have no fees")
}

// TestForwardingLogDeleteInvariants uses property-based testing to verify key
// invariants of the deletion logic.
func TestForwardingLogDeleteInvariants(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		db, err := MakeTestDB(t)
		if err != nil {
			rt.Fatalf("unable to make test db: %v", err)
		}

		log := ForwardingLog{
			db: db,
		}

		// Generate a random set of events.
		baseTime := time.Unix(
			rapid.Int64Range(10000, 100000).Draw(rt, "base_time"),
			0,
		)
		numEvents := rapid.IntRange(1, 100).Draw(rt, "num_events")

		events := make([]ForwardingEvent, numEvents)
		timestamp := baseTime
		for i := 0; i < numEvents; i++ {
			amtIn := rapid.Uint64Range(
				1000, 100000).Draw(rt, "amt_in")
			amtOut := rapid.Uint64Range(
				500, uint64(amtIn)).Draw(rt, "amt_out")

			events[i] = ForwardingEvent{
				Timestamp: timestamp,
				IncomingChanID: lnwire.NewShortChanIDFromInt(
					rapid.Uint64().Draw(rt, "in_chan"),
				),
				OutgoingChanID: lnwire.NewShortChanIDFromInt(
					rapid.Uint64().Draw(rt, "out_chan"),
				),
				AmtIn:          lnwire.MilliSatoshi(amtIn),
				AmtOut:         lnwire.MilliSatoshi(amtOut),
				IncomingHtlcID: fn.Some(uint64(i)),
				OutgoingHtlcID: fn.Some(uint64(i)),
			}
			// Add random interval between events (1 second to 1
			// hour).
			interval := rapid.Int64Range(1, 3600).Draw(
				rt, "interval",
			)
			timestamp = timestamp.Add(
				time.Duration(interval) * time.Second,
			)
		}

		// Add events to database.
		err = log.AddForwardingEvents(events)
		if err != nil {
			rt.Fatalf("unable to add events: %v", err)
		}

		// Pick a random delete time somewhere in the middle or after.
		// This gives us a mix of partial and full deletions.
		deleteIndex := rapid.IntRange(0, numEvents).Draw(
			rt, "delete_index",
		)

		var deleteTime time.Time
		if deleteIndex < numEvents {
			deleteTime = events[deleteIndex].Timestamp
		} else {
			deleteTime = timestamp.Add(time.Hour)
		}

		// Pick a random batch size, then delete with that batch size.
		batchSize := rapid.IntRange(1, 100).Draw(rt, "batch_size")
		stats, err := log.DeleteForwardingEvents(deleteTime, batchSize)
		if err != nil {
			rt.Fatalf("delete failed: %v", err)
		}

		// Invariant 1: Number of deleted events should match count
		// before delete time.
		expectedDeleted := 0
		var expectedFees int64
		for _, event := range events {
			if event.Timestamp.Before(deleteTime) ||
				event.Timestamp.Equal(deleteTime) {

				expectedDeleted++

				expectedFees += int64(event.AmtIn - event.AmtOut)
			}
		}
		if stats.NumEventsDeleted != uint64(expectedDeleted) {
			rt.Fatalf("deleted count doesn't match: expected %d, got %d",
				expectedDeleted, stats.NumEventsDeleted)
		}

		// Invariant 2: Total fees should equal sum of deleted event
		// fees.
		if stats.TotalFeeMsat != expectedFees {
			rt.Fatalf("total fees don't match: expected %d, got %d",
				expectedFees, stats.TotalFeeMsat)
		}

		// Invariant 3: Query should only return events after delete
		// time.
		query := ForwardingEventQuery{
			StartTime:    baseTime,
			EndTime:      timestamp.Add(time.Hour),
			NumMaxEvents: uint32(numEvents * 2),
		}
		result, err := log.Query(query)
		if err != nil {
			rt.Fatalf("query failed: %v", err)
		}

		expectedRemaining := numEvents - expectedDeleted
		if len(result.ForwardingEvents) != expectedRemaining {
			rt.Fatalf("wrong number of remaining events: "+
				"expected %d, got %d",
				expectedRemaining, len(result.ForwardingEvents))
		}

		// Invariant 4: All remaining events should be after delete
		// time.
		for _, event := range result.ForwardingEvents {
			if !event.Timestamp.After(deleteTime) {
				rt.Fatalf("remaining event is not "+
					"after delete time: %v <= %v",
					event.Timestamp, deleteTime)
			}
		}

		// Invariant 5: Second deletion should be idempotent.
		stats2, err := log.DeleteForwardingEvents(deleteTime, batchSize)
		if err != nil {
			rt.Fatalf("second delete failed: %v", err)
		}
		if stats2.NumEventsDeleted != 0 {
			rt.Fatalf("second delete should delete nothing, got %d",
				stats2.NumEventsDeleted)
		}
		if stats2.TotalFeeMsat != 0 {
			rt.Fatalf("second delete should have no fees, got %d",
				stats2.TotalFeeMsat)
		}
	})
}

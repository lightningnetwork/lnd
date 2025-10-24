package channeldb

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// forwardingLogBucket is the bucket that we'll use to store the
	// forwarding log. The forwarding log contains a time series database
	// of the forwarding history of a lightning daemon. Each key within the
	// bucket is a timestamp (in nano seconds since the unix epoch), and
	// the value a slice of a forwarding event for that timestamp.
	forwardingLogBucket = []byte("circuit-fwd-log")
)

const (
	// forwardingEventSize is the size of a forwarding event. The breakdown
	// is as follows:
	//
	//  * 8 byte incoming chan ID || 8 byte outgoing chan ID || 8 byte value in
	//    || 8 byte value out || 8 byte incoming htlc id || 8 byte
	//    outgoing htlc id
	//
	// From the value in and value out, callers can easily compute the
	// total fee extract from a forwarding event.
	forwardingEventSize = 48

	// MaxResponseEvents is the max number of forwarding events that will
	// be returned by a single query response. This size was selected to
	// safely remain under gRPC's 4MiB message size response limit. As each
	// full forwarding event (including the timestamp) is 40 bytes, we can
	// safely return 50k entries in a single response.
	MaxResponseEvents = 50000
)

// ForwardingLog returns an instance of the ForwardingLog object backed by the
// target database instance.
func (d *DB) ForwardingLog() *ForwardingLog {
	return &ForwardingLog{
		db: d,
	}
}

// ForwardingLog is a time series database that logs the fulfillment of payment
// circuits by a lightning network daemon. The log contains a series of
// forwarding events which map a timestamp to a forwarding event. A forwarding
// event describes which channels were used to create+settle a circuit, and the
// amount involved. Subtracting the outgoing amount from the incoming amount
// reveals the fee charged for the forwarding service.
type ForwardingLog struct {
	db *DB
}

// ForwardingEvent is an event in the forwarding log's time series. Each
// forwarding event logs the creation and tear-down of a payment circuit. A
// circuit is created once an incoming HTLC has been fully forwarded, and
// destroyed once the payment has been settled.
type ForwardingEvent struct {
	// Timestamp is the settlement time of this payment circuit.
	Timestamp time.Time

	// IncomingChanID is the incoming channel ID of the payment circuit.
	IncomingChanID lnwire.ShortChannelID

	// OutgoingChanID is the outgoing channel ID of the payment circuit.
	OutgoingChanID lnwire.ShortChannelID

	// AmtIn is the amount of the incoming HTLC. Subtracting this from the
	// outgoing amount gives the total fees of this payment circuit.
	AmtIn lnwire.MilliSatoshi

	// AmtOut is the amount of the outgoing HTLC. Subtracting the incoming
	// amount from this gives the total fees for this payment circuit.
	AmtOut lnwire.MilliSatoshi

	// IncomingHtlcID is the ID of the incoming HTLC in the payment circuit.
	// If this is not set, the value will be nil. This field is added in
	// v0.20 and is made optional to make it backward compatible with
	// existing forwarding events created before it's introduction.
	IncomingHtlcID fn.Option[uint64]

	// OutgoingHtlcID is the ID of the outgoing HTLC in the payment circuit.
	// If this is not set, the value will be nil. This field is added in
	// v0.20 and is made optional to make it backward compatible with
	// existing forwarding events created before it's introduction.
	OutgoingHtlcID fn.Option[uint64]
}

// encodeForwardingEvent writes out the target forwarding event to the passed
// io.Writer, using the expected DB format. Note that the timestamp isn't
// serialized as this will be the key value within the bucket.
func encodeForwardingEvent(w io.Writer, f *ForwardingEvent) error {
	// We check for the HTLC IDs if they are set. If they are not,
	// from v0.20 upward, we return an error to make it clear they are
	// required.
	incomingID, err := f.IncomingHtlcID.UnwrapOrErr(
		errors.New("incoming HTLC ID must be set"),
	)
	if err != nil {
		return err
	}

	outgoingID, err := f.OutgoingHtlcID.UnwrapOrErr(
		errors.New("outgoing HTLC ID must be set"),
	)
	if err != nil {
		return err
	}

	return WriteElements(
		w, f.IncomingChanID, f.OutgoingChanID, f.AmtIn, f.AmtOut,
		incomingID, outgoingID,
	)
}

// decodeForwardingEvent attempts to decode the raw bytes of a serialized
// forwarding event into the target ForwardingEvent. Note that the timestamp
// won't be decoded, as the caller is expected to set this due to the bucket
// structure of the forwarding log.
func decodeForwardingEvent(r io.Reader, f *ForwardingEvent) error {
	// Decode the original fields of the forwarding event.
	err := ReadElements(
		r, &f.IncomingChanID, &f.OutgoingChanID, &f.AmtIn, &f.AmtOut,
	)
	if err != nil {
		return err
	}

	// Decode the incoming and outgoing htlc IDs. For backward compatibility
	// with older records that don't have these fields, we handle EOF by
	// setting the ID to nil. Any other error is treated as a read failure.
	var incomingHtlcID, outgoingHtlcID uint64
	err = ReadElements(r, &incomingHtlcID, &outgoingHtlcID)
	switch {
	case err == nil:
		f.IncomingHtlcID = fn.Some(incomingHtlcID)
		f.OutgoingHtlcID = fn.Some(outgoingHtlcID)

		return nil

	case errors.Is(err, io.EOF):
		return nil

	default:
		return err
	}
}

// AddForwardingEvents adds a series of forwarding events to the database.
// Before inserting, the set of events will be sorted according to their
// timestamp. This ensures that all writes to disk are sequential.
func (f *ForwardingLog) AddForwardingEvents(events []ForwardingEvent) error {
	// Before we create the database transaction, we'll ensure that the set
	// of forwarding events are properly sorted according to their
	// timestamp and that no duplicate timestamps exist to avoid collisions
	// in the key we are going to store the events under.
	makeUniqueTimestamps(events)

	var timestamp [8]byte

	return kvdb.Batch(f.db.Backend, func(tx kvdb.RwTx) error {
		// First, we'll fetch the bucket that stores our time series
		// log.
		logBucket, err := tx.CreateTopLevelBucket(
			forwardingLogBucket,
		)
		if err != nil {
			return err
		}

		// With the bucket obtained, we can now begin to write out the
		// series of events.
		for _, event := range events {
			err := storeEvent(logBucket, event, timestamp[:])
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// storeEvent tries to store a forwarding event into the given bucket by trying
// to avoid collisions. If a key for the event timestamp already exists in the
// database, the timestamp is incremented in nanosecond intervals until a "free"
// slot is found.
func storeEvent(bucket walletdb.ReadWriteBucket, event ForwardingEvent,
	timestampScratchSpace []byte) error {

	// First, we'll serialize this timestamp into our
	// timestamp buffer.
	byteOrder.PutUint64(
		timestampScratchSpace, uint64(event.Timestamp.UnixNano()),
	)

	// Next we'll loop until we find a "free" slot in the bucket to store
	// the event under. This should almost never happen unless we're running
	// on a system that has a very bad system clock that doesn't properly
	// resolve to nanosecond scale. We try up to 100 times (which would come
	// to a maximum shift of 0.1 microsecond which is acceptable for most
	// use cases). If we don't find a free slot, we just give up and let
	// the collision happen. Something must be wrong with the data in that
	// case, even on a very fast machine forwarding payments _will_ take a
	// few microseconds at least so we should find a nanosecond slot
	// somewhere.
	const maxTries = 100
	tries := 0
	for tries < maxTries {
		val := bucket.Get(timestampScratchSpace)
		if val == nil {
			break
		}

		// Collision, try the next nanosecond timestamp.
		nextNano := event.Timestamp.UnixNano() + 1
		event.Timestamp = time.Unix(0, nextNano)
		byteOrder.PutUint64(timestampScratchSpace, uint64(nextNano))
		tries++
	}

	// With the key encoded, we'll then encode the event
	// into our buffer, then write it out to disk.
	var eventBytes [forwardingEventSize]byte
	eventBuf := bytes.NewBuffer(eventBytes[0:0:forwardingEventSize])
	err := encodeForwardingEvent(eventBuf, &event)
	if err != nil {
		return err
	}
	return bucket.Put(timestampScratchSpace, eventBuf.Bytes())
}

// ForwardingEventQuery represents a query to the forwarding log payment
// circuit time series database. The query allows a caller to retrieve all
// records for a particular time slice, offset in that time slice, limiting the
// total number of responses returned.
type ForwardingEventQuery struct {
	// StartTime is the start time of the time slice.
	StartTime time.Time

	// EndTime is the end time of the time slice.
	EndTime time.Time

	// IndexOffset is the offset within the time slice to start at. This
	// can be used to start the response at a particular record.
	IndexOffset uint32

	// NumMaxEvents is the max number of events to return.
	NumMaxEvents uint32

	// IncomingChanIds is the list of channels to filter HTLCs being
	// received from a particular channel.
	// If the list is empty, then it is ignored.
	IncomingChanIDs fn.Set[uint64]

	// OutgoingChanIds is the list of channels to filter HTLCs being
	// forwarded to a particular channel.
	// If the list is empty, then it is ignored.
	OutgoingChanIDs fn.Set[uint64]
}

// ForwardingLogTimeSlice is the response to a forwarding query. It includes
// the original query, the set  events that match the query, and an integer
// which represents the offset index of the last item in the set of returned
// events. This integer allows callers to resume their query using this offset
// in the event that the query's response exceeds the max number of returnable
// events.
type ForwardingLogTimeSlice struct {
	ForwardingEventQuery

	// ForwardingEvents is the set of events in our time series that answer
	// the query embedded above.
	ForwardingEvents []ForwardingEvent

	// LastIndexOffset is the index of the last element in the set of
	// returned ForwardingEvents above. Callers can use this to resume
	// their query in the event that the time slice has too many events to
	// fit into a single response.
	LastIndexOffset uint32
}

// Query allows a caller to query the forwarding event time series for a
// particular time slice. The caller can control the precise time as well as
// the number of events to be returned.
//
// TODO(roasbeef): rename?
func (f *ForwardingLog) Query(q ForwardingEventQuery) (ForwardingLogTimeSlice,
	error) {

	var resp ForwardingLogTimeSlice

	// If the user provided an index offset, then we'll not know how many
	// records we need to skip. We'll also keep track of the record offset
	// as that's part of the final return value.
	recordsToSkip := q.IndexOffset
	recordOffset := q.IndexOffset

	err := kvdb.View(f.db, func(tx kvdb.RTx) error {
		// If the bucket wasn't found, then there aren't any events to
		// be returned.
		logBucket := tx.ReadBucket(forwardingLogBucket)
		if logBucket == nil {
			return ErrNoForwardingEvents
		}

		// We'll be using a cursor to seek into the database, so we'll
		// populate byte slices that represent the start of the key
		// space we're interested in, and the end.
		var startTime, endTime [8]byte
		byteOrder.PutUint64(startTime[:], uint64(q.StartTime.UnixNano()))
		byteOrder.PutUint64(endTime[:], uint64(q.EndTime.UnixNano()))

		// If we know that a set of log events exists, then we'll begin
		// our seek through the log in order to satisfy the query.
		// We'll continue until either we reach the end of the range,
		// or reach our max number of events.
		logCursor := logBucket.ReadCursor()
		timestamp, eventBytes := logCursor.Seek(startTime[:])
		//nolint:ll
		for ; timestamp != nil && bytes.Compare(timestamp, endTime[:]) <= 0; timestamp, eventBytes = logCursor.Next() {
			// If our current return payload exceeds the max number
			// of events, then we'll exit now.
			if uint32(len(resp.ForwardingEvents)) >= q.NumMaxEvents {
				return nil
			}

			// If no incoming or outgoing channel IDs were provided
			// and we're not yet past the user defined offset, then
			// we'll continue to seek forward.
			if recordsToSkip > 0 &&
				q.IncomingChanIDs.IsEmpty() &&
				q.OutgoingChanIDs.IsEmpty() {

				recordsToSkip--
				continue
			}

			// At this point, we've skipped enough records to start
			// to collate our query. For each record, we'll
			// increment the final record offset so the querier can
			// utilize pagination to seek further.
			readBuf := bytes.NewReader(eventBytes)
			if readBuf.Len() == 0 {
				continue
			}

			currentTime := time.Unix(
				0, int64(byteOrder.Uint64(timestamp)),
			)

			var event ForwardingEvent
			err := decodeForwardingEvent(readBuf, &event)
			if err != nil {
				return err
			}

			// Check if the incoming channel ID matches the
			// filter criteria. Either no filtering is
			// applied (IsEmpty), or the ID is explicitly
			// included.
			incomingMatch := q.IncomingChanIDs.IsEmpty() ||
				q.IncomingChanIDs.Contains(
					event.IncomingChanID.ToUint64(),
				)

			// Check if the outgoing channel ID matches the
			// filter criteria. Either no filtering is
			// applied (IsEmpty), or  the ID is explicitly
			// included.
			outgoingMatch := q.OutgoingChanIDs.IsEmpty() ||
				q.OutgoingChanIDs.Contains(
					event.OutgoingChanID.ToUint64(),
				)

			// Skip this event if it doesn't match the
			// filters.
			if !incomingMatch || !outgoingMatch {
				continue
			}
			// If we're not yet past the user defined offset
			// then we'll continue to seek forward.
			if recordsToSkip > 0 {
				recordsToSkip--
				continue
			}

			event.Timestamp = currentTime
			resp.ForwardingEvents = append(
				resp.ForwardingEvents,
				event,
			)
			recordOffset++
		}

		return nil
	}, func() {
		resp = ForwardingLogTimeSlice{
			ForwardingEventQuery: q,
		}
	})
	if err != nil && err != ErrNoForwardingEvents {
		return ForwardingLogTimeSlice{}, err
	}

	resp.LastIndexOffset = recordOffset

	return resp, nil
}

// DeleteStats contains statistics about a forwarding history deletion
// operation.
type DeleteStats struct {
	// NumEventsDeleted is the total number of forwarding events that were
	// deleted from the database.
	NumEventsDeleted uint64

	// TotalFeeMsat is the sum of all fees (AmtIn - AmtOut) from the
	// deleted events, expressed in millisatoshis.
	TotalFeeMsat int64
}

// DeleteForwardingEvents deletes all forwarding events older than the specified
// endTime from the database. The deletion is performed in batches to avoid
// holding large database transactions. This method returns statistics about the
// deletion including the number of events deleted and the total fees earned
// from those events.
//
// The batchSize parameter controls how many events are deleted per database
// transaction. If batchSize is 0, a default of 10000 is used. The maximum
// allowed batch size is MaxResponseEvents (50000) to prevent resource exhaustion.
func (f *ForwardingLog) DeleteForwardingEvents(endTime time.Time,
	batchSize int) (DeleteStats, error) {

	// Set default batch size if not specified, and enforce maximum.
	if batchSize <= 0 {
		batchSize = 10000
	}
	if batchSize > MaxResponseEvents {
		batchSize = MaxResponseEvents
	}

	var stats DeleteStats

	// We'll continue deleting batches until there are no more events to
	// delete.
	for {
		var (
			batchDeleted int
			batchFees    int64
		)

		err := kvdb.Update(f.db, func(tx kvdb.RwTx) error {
			// Fetch the forwarding log bucket. If it doesn't exist,
			// there's nothing to delete.
			logBucket := tx.ReadWriteBucket(forwardingLogBucket)
			if logBucket == nil {
				return ErrNoForwardingEvents
			}

			// We'll use a cursor to iterate through events in time
			// order.
			cursor := logBucket.ReadWriteCursor()

			// Next, encode the end time as our upper bound for
			// deletion.
			var endTimeBytes [8]byte
			byteOrder.PutUint64(
				endTimeBytes[:], uint64(endTime.UnixNano()),
			)

			// Collect keys to delete in this batch. We can't delete
			// while iterating as it may corrupt the cursor.
			keysToDelete := make([][]byte, 0, batchSize)

			// Seek to the beginning and iterate through events
			// until we reach the end time or batch limit.
			//
			//nolint:lll
			for timestamp, eventBytes := cursor.First(); timestamp != nil; timestamp, eventBytes = cursor.Next() {
				// Stop if we've reached or passed the end time.
				if bytes.Compare(
					timestamp, endTimeBytes[:],
				) > 0 {
					break
				}

				// Stop if we've reached the batch size limit.
				if len(keysToDelete) >= batchSize {
					break
				}

				// Decode the event, as we need to parse it to
				// obtain the fee.
				readBuf := bytes.NewReader(eventBytes)
				if readBuf.Len() > 0 {
					var event ForwardingEvent
					err := decodeForwardingEvent(
						readBuf, &event,
					)
					if err != nil {
						return err
					}

					// Calculate the fee for this event. Fee
					// is the difference between incoming
					// and outgoing amounts.
					fee := int64(event.AmtIn - event.AmtOut)
					batchFees += fee
				}

				// Make a copy of the key to delete later.
				keyCopy := make([]byte, len(timestamp))
				copy(keyCopy, timestamp)
				keysToDelete = append(keysToDelete, keyCopy)
			}

			// Now delete all the collected keys.
			for _, key := range keysToDelete {
				if err := logBucket.Delete(key); err != nil {
					return err
				}
			}

			batchDeleted = len(keysToDelete)
			return nil
		}, func() {
			batchDeleted = 0
			batchFees = 0
		})

		if err != nil {
			// If the bucket doesn't exist, we're done.
			if err == ErrNoForwardingEvents {
				break
			}
			return stats, err
		}

		// Update our running statistics.
		stats.NumEventsDeleted += uint64(batchDeleted)
		stats.TotalFeeMsat += batchFees

		// If we deleted fewer events than the batch size, we're done.
		if batchDeleted < batchSize {
			break
		}

		// Otherwise, continue with the next batch.
	}

	return stats, nil
}

// makeUniqueTimestamps takes a slice of forwarding events, sorts it by the
// event timestamps and then makes sure there are no duplicates in the
// timestamps. If duplicates are found, some of the timestamps are increased on
// the nanosecond scale until only unique values remain. This is a fix to
// address the problem that in some environments (looking at you, Windows) the
// system clock has such a bad resolution that two serial invocations of
// time.Now() might return the same timestamp, even if some time has elapsed
// between the calls.
func makeUniqueTimestamps(events []ForwardingEvent) {
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})

	// Now that we know the events are sorted by timestamp, we can go
	// through the list and fix all duplicates until only unique values
	// remain.
	for outer := 0; outer < len(events)-1; outer++ {
		current := events[outer].Timestamp.UnixNano()
		next := events[outer+1].Timestamp.UnixNano()

		// We initially sorted the slice. So if the current is now
		// greater or equal to the next one, it's either because it's a
		// duplicate or because we increased the current in the last
		// iteration.
		if current >= next {
			next = current + 1
			events[outer+1].Timestamp = time.Unix(0, next)
		}
	}
}

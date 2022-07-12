package migration30

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	mig24 "github.com/lightningnetwork/lnd/channeldb/migration24"
	mig26 "github.com/lightningnetwork/lnd/channeldb/migration26"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/kvdb"
)

// recordsPerTx specifies the number of records to be migrated in each database
// transaction. In the worst case, each old revocation log is 28,057 bytes.
// 20,000 records would consume 0.56 GB of ram, which is feasible for a modern
// machine.
//
// NOTE: we could've used more ram but it doesn't help with the speed of the
// migration since the most of the CPU time is used for calculating the output
// indexes.
const recordsPerTx = 20_000

// MigrateRevocationLog migrates the old revocation logs into the newer format
// and deletes them once finished, with the deletion only happens once ALL the
// old logs have been migrates.
func MigrateRevocationLog(db kvdb.Backend) error {
	log.Infof("Migrating revocation logs, might take a while...")

	var (
		err error

		// finished is used to exit the for loop.
		finished bool

		// total is the number of total records.
		total uint64

		// migrated is the number of already migrated records.
		migrated uint64
	)

	// First of all, read the stats of the revocation logs.
	total, migrated, err = logMigrationStat(db)
	if err != nil {
		return err
	}
	log.Infof("Total logs=%d, migrated=%d", total, migrated)

	// Exit early if the old logs have already been migrated and deleted.
	if total == 0 {
		log.Info("Migration already finished!")
		return nil
	}

	for {
		if finished {
			log.Infof("Migrating old revocation logs finished, " +
				"now checking the migration results...")
			break
		}

		// Process the migration.
		err = kvdb.Update(db, func(tx kvdb.RwTx) error {
			finished, err = processMigration(tx)
			if err != nil {
				return err
			}
			return nil
		}, func() {})
		if err != nil {
			return err
		}

		// Each time we finished the above process, we'd read the stats
		// again to understand the current progress.
		total, migrated, err = logMigrationStat(db)
		if err != nil {
			return err
		}

		// Calculate and log the progress if the progress is less than
		// one.
		progress := float64(migrated) / float64(total) * 100
		if progress >= 100 {
			continue
		}

		log.Infof("Migration progress: %.3f%%, still have: %d",
			progress, total-migrated)
	}

	// Once the migration completes, we can now safety delete the old
	// revocation logs.
	if err := deleteOldBuckets(db); err != nil {
		return fmt.Errorf("deleteOldBuckets err: %v", err)
	}

	log.Info("Old revocation log buckets removed!")
	return nil
}

// processMigration finds the next un-migrated revocation logs, reads a max
// number of `recordsPerTx` records, converts them into the new revocation logs
// and save them to disk.
func processMigration(tx kvdb.RwTx) (bool, error) {
	openChanBucket := tx.ReadWriteBucket(openChannelBucket)

	// If no bucket is found, we can exit early.
	if openChanBucket == nil {
		return false, fmt.Errorf("root bucket not found")
	}

	// Locate the next migration height.
	locator, err := locateNextUpdateNum(openChanBucket)
	if err != nil {
		return false, fmt.Errorf("locator got error: %v", err)
	}

	// If the returned locator is nil, we've done migrating the logs.
	if locator == nil {
		return true, nil
	}

	// Read a list of old revocation logs.
	entryMap, err := readOldRevocationLogs(openChanBucket, locator)
	if err != nil {
		return false, fmt.Errorf("read old logs err: %v", err)
	}

	// Migrate the revocation logs.
	return false, writeRevocationLogs(openChanBucket, entryMap)
}

// deleteOldBuckets iterates all the channel buckets and deletes the old
// revocation buckets.
func deleteOldBuckets(db kvdb.Backend) error {
	// locators records all the chan buckets found in the database.
	var locators []*updateLocator

	// reader is a helper closure that saves the locator found. Each
	// locator is relatively small(33+32+36+8=109 bytes), assuming 1 GB of
	// ram we can fit roughly 10 million records. Since each record
	// corresponds to a channel, we should have more than enough memory to
	// read them all.
	reader := func(_ kvdb.RwBucket, l *updateLocator) error { // nolint:unparam
		locators = append(locators, l)
		return nil
	}

	// remover is a helper closure that removes the old revocation log
	// bucket under the specified chan bucket by the given locator.
	remover := func(rootBucket kvdb.RwBucket, l *updateLocator) error {
		chanBucket, err := l.locateChanBucket(rootBucket)
		if err != nil {
			return err
		}

		return chanBucket.DeleteNestedBucket(
			revocationLogBucketDeprecated,
		)
	}

	// Perform the deletion in one db transaction. This should not cause
	// any memory issue as the deletion doesn't load any data from the
	// buckets.
	return kvdb.Update(db, func(tx kvdb.RwTx) error {
		openChanBucket := tx.ReadWriteBucket(openChannelBucket)

		// Exit early if there's no bucket.
		if openChanBucket == nil {
			return nil
		}

		// Iterate the buckets to find all the locators.
		err := iterateBuckets(openChanBucket, nil, reader)
		if err != nil {
			return err
		}

		// Iterate the locators and delete all the old revocation log
		// buckets.
		for _, l := range locators {
			err := remover(openChanBucket, l)
			// If the bucket doesn't exist, we can exit safety.
			if err != nil && err != kvdb.ErrBucketNotFound {
				return err
			}
		}

		return nil
	}, func() {})
}

// writeRevocationLogs unwraps the entryMap and writes the new revocation logs.
func writeRevocationLogs(openChanBucket kvdb.RwBucket,
	entryMap logEntries) error {

	for locator, logs := range entryMap {
		// Find the channel bucket.
		chanBucket, err := locator.locateChanBucket(openChanBucket)
		if err != nil {
			return fmt.Errorf("locateChanBucket err: %v", err)
		}

		// Create the new log bucket.
		logBucket, err := chanBucket.CreateBucketIfNotExists(
			revocationLogBucket,
		)
		if err != nil {
			return fmt.Errorf("create log bucket err: %v", err)
		}

		// Write the new logs.
		for _, entry := range logs {
			var b bytes.Buffer
			err := serializeRevocationLog(&b, entry.log)
			if err != nil {
				return err
			}

			logEntrykey := mig24.MakeLogKey(entry.commitHeight)
			err = logBucket.Put(logEntrykey[:], b.Bytes())
			if err != nil {
				return fmt.Errorf("putRevocationLog err: %v",
					err)
			}
		}
	}

	return nil
}

// logMigrationStat reads the buckets to provide stats over current migration
// progress. The returned values are the numbers of total records and already
// migrated records.
func logMigrationStat(db kvdb.Backend) (uint64, uint64, error) {
	var (
		err error

		// total is the number of total records.
		total uint64

		// unmigrated is the number of unmigrated records.
		unmigrated uint64
	)

	err = kvdb.Update(db, func(tx kvdb.RwTx) error {
		total, unmigrated, err = fetchLogStats(tx)
		return err
	}, func() {})

	log.Debugf("Total logs=%d, unmigrated=%d", total, unmigrated)
	return total, total - unmigrated, err
}

// fetchLogStats iterates all the chan buckets to provide stats about the logs.
// The returned values are num of total records, and num of un-migrated
// records.
func fetchLogStats(tx kvdb.RwTx) (uint64, uint64, error) {
	var (
		total           uint64
		totalUnmigrated uint64
	)

	openChanBucket := tx.ReadWriteBucket(openChannelBucket)

	// If no bucket is found, we can exit early.
	if openChanBucket == nil {
		return 0, 0, fmt.Errorf("root bucket not found")
	}

	// counter is a helper closure used to count the number of records
	// based on the given bucket.
	counter := func(chanBucket kvdb.RwBucket, bucket []byte) uint64 {
		// Read the sub-bucket level 4.
		logBucket := chanBucket.NestedReadBucket(bucket)

		// Exit early if we don't have the bucket.
		if logBucket == nil {
			return 0
		}

		// Jump to the end of the cursor.
		key, _ := logBucket.ReadCursor().Last()

		// Since the CommitHeight is a zero-based monotonically
		// increased index, its value plus one reflects the total
		// records under this chan bucket.
		lastHeight := binary.BigEndian.Uint64(key) + 1

		return lastHeight
	}

	// countTotal is a callback function used to count the total number of
	// records.
	countTotal := func(chanBucket kvdb.RwBucket, l *updateLocator) error {
		total += counter(chanBucket, revocationLogBucketDeprecated)
		return nil
	}

	// countUnmigrated is a callback function used to count the total
	// number of un-migrated records.
	countUnmigrated := func(chanBucket kvdb.RwBucket,
		l *updateLocator) error {

		totalUnmigrated += counter(
			chanBucket, revocationLogBucketDeprecated,
		)
		return nil
	}

	// Locate the next migration height.
	locator, err := locateNextUpdateNum(openChanBucket)
	if err != nil {
		return 0, 0, fmt.Errorf("locator got error: %v", err)
	}

	// If the returned locator is not nil, we still have un-migrated
	// records so we need to count them. Otherwise we've done migrating the
	// logs.
	if locator != nil {
		err = iterateBuckets(openChanBucket, locator, countUnmigrated)
		if err != nil {
			return 0, 0, err
		}
	}

	// Count the total number of records by supplying a nil locator.
	err = iterateBuckets(openChanBucket, nil, countTotal)
	if err != nil {
		return 0, 0, err
	}

	return total, totalUnmigrated, err
}

// logEntry houses the info needed to write a new revocation log.
type logEntry struct {
	log          *RevocationLog
	commitHeight uint64
	ourIndex     uint32
	theirIndex   uint32
	locator      *updateLocator
}

// logEntries maps a bucket locator to a list of entries under that bucket.
type logEntries map[*updateLocator][]*logEntry

// result is made of two channels that's used to send back the constructed new
// revocation log or an error.
type result struct {
	newLog  chan *logEntry
	errChan chan error
}

// readOldRevocationLogs finds a list of old revocation logs and converts them
// into the new revocation logs.
func readOldRevocationLogs(openChanBucket kvdb.RwBucket,
	locator *updateLocator) (logEntries, error) {

	entries := make(logEntries)
	results := make([]*result, 0)

	var wg sync.WaitGroup

	// collectLogs is a helper closure that reads all newly created
	// revocation logs sent over the result channels.
	//
	// NOTE: the order of the logs cannot be guaranteed, which is fine as
	// boltdb will take care of the orders when saving them.
	collectLogs := func() error {
		wg.Wait()

		for _, r := range results {
			select {
			case entry := <-r.newLog:
				entries[entry.locator] = append(
					entries[entry.locator], entry,
				)

			case err := <-r.errChan:
				return err
			}
		}

		return nil
	}

	// createLog is a helper closure that constructs a new revocation log.
	//
	// NOTE: used as a goroutine.
	createLog := func(chanState *mig26.OpenChannel,
		c mig.ChannelCommitment, l *updateLocator, r *result) {

		defer wg.Done()

		// Find the output indexes.
		ourIndex, theirIndex, err := findOutputIndexes(chanState, &c)
		if err != nil {
			r.errChan <- err
		}

		// Convert the old logs into the new logs. We do this early in
		// the read tx so the old large revocation log can be set to
		// nil here so save us some memory space.
		newLog, err := convertRevocationLog(&c, ourIndex, theirIndex)
		if err != nil {
			r.errChan <- err
		}
		// Create the entry that will be used to create the new log.
		entry := &logEntry{
			log:          newLog,
			commitHeight: c.CommitHeight,
			ourIndex:     ourIndex,
			theirIndex:   theirIndex,
			locator:      l,
		}

		r.newLog <- entry
	}

	// innerCb is the stepping function used when iterating the old log
	// bucket.
	innerCb := func(chanState *mig26.OpenChannel, l *updateLocator,
		_, v []byte) error {

		reader := bytes.NewReader(v)
		c, err := mig.DeserializeChanCommit(reader)
		if err != nil {
			return err
		}

		r := &result{
			newLog:  make(chan *logEntry, 1),
			errChan: make(chan error, 1),
		}
		results = append(results, r)

		// We perform the log creation in a goroutine as it takes some
		// time to compute and find output indexes.
		wg.Add(1)
		go createLog(chanState, c, l, r)

		// Check the records read so far and signals exit when we've
		// reached our memory cap.
		if len(results) >= recordsPerTx {
			return errExit
		}

		return nil
	}

	// cb is the callback function to be used when iterating the buckets.
	cb := func(chanBucket kvdb.RwBucket, l *updateLocator) error {
		// Read the open channel.
		c := &mig26.OpenChannel{}
		err := mig26.FetchChanInfo(chanBucket, c, false)
		if err != nil {
			return fmt.Errorf("unable to fetch chan info: %v", err)
		}

		err = fetchChanRevocationState(chanBucket, c)
		if err != nil {
			return fmt.Errorf("unable to fetch revocation "+
				"state: %v", err)
		}

		// Read the sub-bucket level 4.
		logBucket := chanBucket.NestedReadBucket(
			revocationLogBucketDeprecated,
		)
		// Exit early if we don't have the old bucket.
		if logBucket == nil {
			return nil
		}

		// Init the map key when needed.
		_, ok := entries[l]
		if !ok {
			entries[l] = make([]*logEntry, 0, recordsPerTx)
		}

		return iterator(
			logBucket, locator.nextHeight,
			func(k, v []byte) error {
				// Reset the nextHeight for following chan
				// buckets.
				locator.nextHeight = nil
				return innerCb(c, l, k, v)
			},
		)
	}

	err := iterateBuckets(openChanBucket, locator, cb)
	// If there's an error and it's not exit signal, we won't collect the
	// logs from the result channels.
	if err != nil && err != errExit {
		return nil, err
	}

	// Otherwise, collect the logs.
	err = collectLogs()

	return entries, err
}

// convertRevocationLog uses the fields `CommitTx` and `Htlcs` from a
// ChannelCommitment to construct a revocation log entry.
func convertRevocationLog(commit *mig.ChannelCommitment,
	ourOutputIndex, theirOutputIndex uint32) (*RevocationLog, error) {

	// Sanity check that the output indexes can be safely converted.
	if ourOutputIndex > math.MaxUint16 {
		return nil, ErrOutputIndexTooBig
	}
	if theirOutputIndex > math.MaxUint16 {
		return nil, ErrOutputIndexTooBig
	}

	rl := &RevocationLog{
		OurOutputIndex:   uint16(ourOutputIndex),
		TheirOutputIndex: uint16(theirOutputIndex),
		CommitTxHash:     commit.CommitTx.TxHash(),
		HTLCEntries:      make([]*HTLCEntry, 0, len(commit.Htlcs)),
	}

	for _, htlc := range commit.Htlcs {
		// Skip dust HTLCs.
		if htlc.OutputIndex < 0 {
			continue
		}

		// Sanity check that the output indexes can be safely
		// converted.
		if htlc.OutputIndex > math.MaxUint16 {
			return nil, ErrOutputIndexTooBig
		}

		entry := &HTLCEntry{
			RHash:         htlc.RHash,
			RefundTimeout: htlc.RefundTimeout,
			Incoming:      htlc.Incoming,
			OutputIndex:   uint16(htlc.OutputIndex),
			Amt:           htlc.Amt.ToSatoshis(),
		}
		rl.HTLCEntries = append(rl.HTLCEntries, entry)
	}

	return rl, nil
}

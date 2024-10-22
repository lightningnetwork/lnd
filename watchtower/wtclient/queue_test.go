package wtclient

import (
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtmock"
	"github.com/stretchr/testify/require"
)

const (
	maxInMemItems = 5
	waitTime      = time.Second * 2
)

// TestDiskOverflowQueue tests that the DiskOverflowQueue behaves as expected.
func TestDiskOverflowQueue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(*testing.T, wtdb.Queue[*wtdb.BackupID])
	}{
		{
			name: "overflow to disk",
			run:  testOverflowToDisk,
		},
		{
			name: "startup with smaller buffer size",
			run:  testRestartWithSmallerBufferSize,
		},
		{
			name: "start stop queue",
			run:  testStartStopQueue,
		},
	}

	initDB := func() wtdb.Queue[*wtdb.BackupID] {
		dbCfg := &kvdb.BoltConfig{
			DBTimeout: kvdb.DefaultDBTimeout,
		}

		bdb, err := wtdb.NewBoltBackendCreator(
			true, t.TempDir(), "wtclient.db",
		)(dbCfg)
		require.NoError(t, err)

		db, err := wtdb.OpenClientDB(bdb)
		require.NoError(t, err)

		t.Cleanup(func() {
			require.NoError(t, db.Close())
		})

		return db.GetDBQueue([]byte("test-namespace"))
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(tt *testing.T) {
			tt.Parallel()

			test.run(tt, initDB())
		})
	}
}

// testOverflowToDisk is a basic test that ensures that the queue correctly
// overflows items to disk and then correctly reloads them.
func testOverflowToDisk(t *testing.T, db wtdb.Queue[*wtdb.BackupID]) {
	// Generate some backup IDs that we want to add to the queue.
	tasks := genBackupIDs(10)

	// New mock logger.
	log := newMockLogger(t.Logf)

	// Init the queue with the mock DB.
	q, err := NewDiskOverflowQueue[*wtdb.BackupID](
		db, maxInMemItems, log,
	)
	require.NoError(t, err)

	// Start the queue.
	require.NoError(t, q.Start())

	// Initially there should be no items on disk.
	assertNumDisk(t, db, 0)

	// Start filling up the queue.
	enqueue(t, q, tasks[0])
	enqueue(t, q, tasks[1])
	enqueue(t, q, tasks[2])
	enqueue(t, q, tasks[3])
	enqueue(t, q, tasks[4])

	// The queue should now be full, so any new items should be persisted to
	// disk.
	enqueue(t, q, tasks[5])
	waitForNumDisk(t, db, 1)

	// Now pop all items from the queue to ensure that the item
	// from disk is loaded in properly once there is space.
	require.Equal(t, tasks[0], getNext(t, q, 0))
	require.Equal(t, tasks[1], getNext(t, q, 1))
	require.Equal(t, tasks[2], getNext(t, q, 2))
	require.Equal(t, tasks[3], getNext(t, q, 3))
	require.Equal(t, tasks[4], getNext(t, q, 4))
	require.Equal(t, tasks[5], getNext(t, q, 5))

	// There should no longer be any items in the disk queue.
	assertNumDisk(t, db, 0)

	require.NoError(t, q.Stop())
}

// testRestartWithSmallerBufferSize tests that if the queue is restarted with
// a smaller in-memory buffer size that it was initially started with, then
// tasks are still loaded in the correct order.
func testRestartWithSmallerBufferSize(t *testing.T,
	db wtdb.Queue[*wtdb.BackupID]) {

	const (
		firstMaxInMemItems  = 5
		secondMaxInMemItems = 2
	)

	// Generate some backup IDs that we want to add to the queue.
	tasks := genBackupIDs(10)

	// New mock logger.
	log := newMockLogger(t.Logf)

	// Init the queue with the mock DB and an initial max in-mem
	// items number.
	q, err := NewDiskOverflowQueue[*wtdb.BackupID](
		db, firstMaxInMemItems, log,
	)
	require.NoError(t, err)
	require.NoError(t, q.Start())

	// Add 7 items to the queue. The first 5 will go into the in-mem
	// queue, the other 2 will be persisted to the main disk queue.
	enqueue(t, q, tasks[0])
	enqueue(t, q, tasks[1])
	enqueue(t, q, tasks[2])
	enqueue(t, q, tasks[3])
	enqueue(t, q, tasks[4])
	enqueue(t, q, tasks[5])
	enqueue(t, q, tasks[6])

	waitForNumDisk(t, db, 2)

	// Now stop the queue and re-initialise it with a smaller
	// buffer maximum.
	require.NoError(t, q.Stop())

	// Check that there are now 7 items in the disk queue.
	waitForNumDisk(t, db, 7)

	// Re-init the queue with a smaller max buffer size.
	q, err = NewDiskOverflowQueue[*wtdb.BackupID](
		db, secondMaxInMemItems, log,
	)
	require.NoError(t, err)
	require.NoError(t, q.Start())

	// Once more we shall repeat the above restart process just to ensure
	// that in-memory items are correctly re-written and read from the DB.
	waitForNumDisk(t, db, 5)
	require.NoError(t, q.Stop())
	waitForNumDisk(t, db, 7)
	q, err = NewDiskOverflowQueue[*wtdb.BackupID](
		db, secondMaxInMemItems, log,
	)
	require.NoError(t, err)
	require.NoError(t, q.Start())
	waitForNumDisk(t, db, 5)

	// Make sure that items are popped off the queue in the correct
	// order.
	require.Equal(t, tasks[0], getNext(t, q, 0))
	require.Equal(t, tasks[1], getNext(t, q, 1))
	require.Equal(t, tasks[2], getNext(t, q, 2))
	require.Equal(t, tasks[3], getNext(t, q, 3))
	require.Equal(t, tasks[4], getNext(t, q, 4))
	require.Equal(t, tasks[5], getNext(t, q, 5))
	require.Equal(t, tasks[6], getNext(t, q, 6))

	require.NoError(t, q.Stop())
}

// testStartStopQueue is a stress test that pushes a large number of tasks
// through the queue while also restarting the queue a couple of times
// throughout.
func testStartStopQueue(t *testing.T, db wtdb.Queue[*wtdb.BackupID]) {
	// Generate a lot of backup IDs that we want to add to the
	// queue one after the other.
	tasks := genBackupIDs(200_000)

	// New mock logger.
	log := newMockLogger(t.Logf)

	// Init the queue with the mock DB.
	q, err := NewDiskOverflowQueue[*wtdb.BackupID](
		db, DefaultMaxTasksInMemQueue, log,
	)
	require.NoError(t, err)

	// Start the queue.
	require.NoError(t, q.Start())

	// Initially there should be no items on disk.
	assertNumDisk(t, db, 0)

	// We need to guard the queue with a mutex since we will be
	// stopping, re-creating and starting the queue multiple times.
	var (
		queueMtx sync.RWMutex
		wg       sync.WaitGroup
		sendDone = make(chan struct{})
	)

	// This goroutine will constantly try to add new items to the
	// queue, even if the queue is stopped.
	wg.Add(1)
	go func() {
		defer wg.Done()

		for idx := range tasks {
			queueMtx.RLock()
			err := q.QueueBackupID(tasks[idx])
			require.NoError(t, err)
			queueMtx.RUnlock()
		}
	}()

	// This goroutine will repeatedly stop, re-create and start the
	// queue until we're done sending items.
	wg.Add(1)
	go func() {
		defer wg.Done()

		numRestarts := 0
		for {
			select {
			case <-sendDone:
				t.Logf("Restarted queue %d times",
					numRestarts)

				return
			case <-time.After(100 * time.Millisecond):
			}

			queueMtx.Lock()
			require.NoError(t, q.Stop())
			q, err = NewDiskOverflowQueue[*wtdb.BackupID](
				db, DefaultMaxTasksInMemQueue, log,
			)
			require.NoError(t, err)
			require.NoError(t, q.Start())
			queueMtx.Unlock()

			numRestarts++
		}
	}()

	// We should be able to read all items from the queue, not being
	// affected by restarts, other than needing to wait for the
	// queue to be started again.
	results := make([]*wtdb.BackupID, 0, len(tasks))
	for i := 0; i < len(tasks); i++ {
		queueMtx.RLock()
		task := getNext(t, q, i)
		queueMtx.RUnlock()

		results = append(results, task)
	}
	close(sendDone)
	require.Equal(t, tasks, results)

	require.NoError(t, q.Stop())
	wg.Wait()
}

func getNext(t *testing.T, q *DiskOverflowQueue[*wtdb.BackupID],
	i int) *wtdb.BackupID {

	var item *wtdb.BackupID
	select {
	case item = <-q.NextBackupID():
	case <-time.After(waitTime):
		t.Fatalf("task %d not received in time", i)
	}

	return item
}

func enqueue(t *testing.T, q *DiskOverflowQueue[*wtdb.BackupID],
	task *wtdb.BackupID) {

	err := q.QueueBackupID(task)
	require.NoError(t, err)
}

func waitForNumDisk(t *testing.T, db wtdb.Queue[*wtdb.BackupID], num int) {
	err := wait.Predicate(func() bool {
		n, err := db.Len()
		require.NoError(t, err)

		return n == uint64(num)
	}, waitTime)
	require.NoError(t, err)
}

func assertNumDisk(t *testing.T, db wtdb.Queue[*wtdb.BackupID], num int) {
	n, err := db.Len()
	require.NoError(t, err)
	require.EqualValues(t, num, n)
}

func genBackupIDs(num int) []*wtdb.BackupID {
	ids := make([]*wtdb.BackupID, num)
	for i := 0; i < num; i++ {
		ids[i] = newBackupID(i)
	}

	return ids
}

func newBackupID(id int) *wtdb.BackupID {
	return &wtdb.BackupID{CommitHeight: uint64(id)}
}

// BenchmarkDiskOverflowQueue benchmarks the performance of adding and removing
// items from the DiskOverflowQueue using an in-memory disk db.
func BenchmarkDiskOverflowQueue(b *testing.B) {
	enqueue := func(q *DiskOverflowQueue[*wtdb.BackupID],
		task *wtdb.BackupID) {

		err := q.QueueBackupID(task)
		require.NoError(b, err)
	}

	getNext := func(q *DiskOverflowQueue[*wtdb.BackupID],
		i int) *wtdb.BackupID {

		var item *wtdb.BackupID
		select {
		case item = <-q.NextBackupID():
		case <-time.After(time.Second * 2):
			b.Fatalf("task %d not received in time", i)
		}

		return item
	}

	// Generate some backup IDs that we want to add to the queue.
	tasks := genBackupIDs(b.N)

	// Create a mock db.
	db := wtmock.NewQueueDB[*wtdb.BackupID]()

	// New mock logger.
	log := newMockLogger(b.Logf)

	// Init the queue with the mock DB.
	q, err := NewDiskOverflowQueue[*wtdb.BackupID](db, 5, log)
	require.NoError(b, err)

	// Start the queue.
	require.NoError(b, q.Start())

	// Start filling up the queue.
	for n := 0; n < b.N; n++ {
		enqueue(q, tasks[n])
	}

	// Pop all the items off the queue.
	for n := 0; n < b.N; n++ {
		require.Equal(b, tasks[n], getNext(q, n))
	}

	require.NoError(b, q.Stop())
}

type mockLogger struct {
	log func(string, ...any)

	btclog.Logger
}

func newMockLogger(logger func(string, ...any)) *mockLogger {
	return &mockLogger{log: logger}
}

// Errorf formats message according to format specifier and writes to log.
//
// NOTE: this is part of the btclog.Logger interface.
func (l *mockLogger) Errorf(format string, params ...any) {
	l.log("[ERR]: "+format, params...)
}

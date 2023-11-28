package wtdb_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/stretchr/testify/require"
)

// TestDiskQueue ensures that the ClientDBs disk queue methods behave as is
// expected of a queue.
func TestDiskQueue(t *testing.T) {
	t.Parallel()

	dbCfg := &kvdb.BoltConfig{
		DBTimeout: kvdb.DefaultDBTimeout,
	}

	// Construct the ClientDB.
	bdb, err := wtdb.NewBoltBackendCreator(
		true, t.TempDir(), "wtclient.db",
	)(dbCfg)
	require.NoError(t, err)

	db, err := wtdb.OpenClientDB(bdb)
	require.NoError(t, err)

	t.Cleanup(func() {
		err = db.Close()
		require.NoError(t, err)
	})

	// In order to test that the queue's `onItemWrite` call back (which in
	// this case will be set to maybeUpdateMaxCommitHeight) is executed as
	// expected, we need to register a channel so that we can later assert
	// that it's max height field was updated properly.
	var chanID lnwire.ChannelID
	err = db.RegisterChannel(chanID, []byte{})
	require.NoError(t, err)

	namespace := []byte("test-namespace")
	queue := db.GetDBQueue(namespace)

	addTasksToTail := func(tasks ...*wtdb.BackupID) {
		err := queue.Push(tasks...)
		require.NoError(t, err)
	}

	addTasksToHead := func(tasks ...*wtdb.BackupID) {
		err := queue.PushHead(tasks...)
		require.NoError(t, err)
	}

	assertNumTasks := func(expNum int) {
		num, err := queue.Len()
		require.NoError(t, err)
		require.EqualValues(t, expNum, num)
	}

	popAndAssert := func(expTasks ...*wtdb.BackupID) {
		tasks, err := queue.PopUpTo(len(expTasks))
		require.NoError(t, err)
		require.EqualValues(t, expTasks, tasks)
	}

	// Create a few tasks that we use throughout the test.
	task1 := &wtdb.BackupID{CommitHeight: 1}
	task2 := &wtdb.BackupID{CommitHeight: 2}
	task3 := &wtdb.BackupID{CommitHeight: 3}
	task4 := &wtdb.BackupID{CommitHeight: 4}
	task5 := &wtdb.BackupID{CommitHeight: 5}
	task6 := &wtdb.BackupID{CommitHeight: 6}

	// Namespace 1 should initially have no items.
	assertNumTasks(0)

	// Now add a few items to the tail of the queue.
	addTasksToTail(task1, task2)

	// Check that the number of tasks is now two.
	assertNumTasks(2)

	// Pop a task, check that it is task 1 and assert that the number of
	// items left is now 1.
	popAndAssert(task1)
	assertNumTasks(1)

	// Pop a task, check that it is task 2 and assert that the number of
	// items left is now 0.
	popAndAssert(task2)
	assertNumTasks(0)

	// Once again add a few tasks.
	addTasksToTail(task3, task4)

	// Now push some tasks to the head of the queue.
	addTasksToHead(task6, task5)

	// Ensure that both the disk queue lengths are added together when
	// querying the length of the queue.
	assertNumTasks(4)

	// Ensure that the order that the tasks are popped is correct.
	popAndAssert(task6, task5, task3, task4)

	// We also want to test that the head queue works as expected and that.
	// To do this, we first push 4, 5 and 6 to the queue.
	addTasksToTail(task4, task5, task6)

	// Now we push 1, 2 and 3 to the head.
	addTasksToHead(task1, task2, task3)

	// Now, only pop item 1 from the queue and then re-add it to the head.
	popAndAssert(task1)
	addTasksToHead(task1)

	// This should not have changed the order of the tasks, they should
	// still appear in the correct order.
	popAndAssert(task1, task2, task3, task4, task5, task6)

	// Finally, we check that the `onItemWrite` call back was executed by
	// the queue. We do this by checking that the channel's recorded max
	// commitment height was set correctly. It should be equal to the height
	// recorded in task6.
	infos, err := db.FetchChanInfos()
	require.NoError(t, err)
	require.Len(t, infos, 1)

	info, ok := infos[chanID]
	require.True(t, ok)
	require.True(t, info.MaxHeight.IsSome())
	info.MaxHeight.WhenSome(func(height uint64) {
		require.EqualValues(t, task6.CommitHeight, height)
	})
}

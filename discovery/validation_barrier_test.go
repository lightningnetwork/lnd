package discovery

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestValidationBarrierSemaphore checks basic properties of the validation
// barrier's semaphore wrt. enqueuing/dequeuing.
func TestValidationBarrierSemaphore(t *testing.T) {
	t.Parallel()

	const (
		numTasks        = 8
		numPendingTasks = 8
		timeout         = 50 * time.Millisecond
	)

	quit := make(chan struct{})
	barrier := NewValidationBarrier(numTasks, quit)

	var scidMtx sync.RWMutex
	currentScid := lnwire.ShortChannelID{}

	// Saturate the semaphore with jobs.
	for i := 0; i < numTasks; i++ {
		scidMtx.Lock()
		dummyUpdate := &lnwire.ChannelUpdate1{
			ShortChannelID: currentScid,
		}
		currentScid.TxIndex++
		scidMtx.Unlock()

		_, err := barrier.InitJobDependencies(dummyUpdate)
		require.NoError(t, err)
	}

	// Spawn additional tasks that will signal completion when added.
	jobAdded := make(chan struct{})
	for i := 0; i < numPendingTasks; i++ {
		go func() {
			scidMtx.Lock()
			dummyUpdate := &lnwire.ChannelUpdate1{
				ShortChannelID: currentScid,
			}
			currentScid.TxIndex++
			scidMtx.Unlock()

			_, err := barrier.InitJobDependencies(dummyUpdate)
			require.NoError(t, err)

			jobAdded <- struct{}{}
		}()
	}

	// Check that no jobs are added while semaphore is full.
	select {
	case <-time.After(timeout):
		// Expected since no slots open.
	case <-jobAdded:
		t.Fatalf("job should not have been added")
	}

	// Complete jobs one at a time and verify that they get added.
	for i := 0; i < numPendingTasks; i++ {
		barrier.CompleteJob()

		select {
		case <-time.After(timeout):
			t.Fatalf("timeout waiting for job to be added")
		case <-jobAdded:
			// Expected since one slot opened up.
		}
	}
}

// TestValidationBarrierQuit checks that pending validation tasks will return an
// error from WaitForDependants if the barrier's quit signal is canceled.
func TestValidationBarrierQuit(t *testing.T) {
	t.Parallel()

	const (
		numTasks = 8
		timeout  = 50 * time.Millisecond
	)

	quit := make(chan struct{})
	barrier := NewValidationBarrier(2*numTasks, quit)

	// Create a set of unique channel announcements that we will prep for
	// validation.
	anns := make([]*lnwire.ChannelAnnouncement1, 0, numTasks)
	parentJobIDs := make([]JobID, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		anns = append(anns, &lnwire.ChannelAnnouncement1{
			ShortChannelID: lnwire.NewShortChanIDFromInt(uint64(i)),
			NodeID1:        nodeIDFromInt(uint64(2 * i)),
			NodeID2:        nodeIDFromInt(uint64(2*i + 1)),
		})
		parentJobID, err := barrier.InitJobDependencies(anns[i])
		require.NoError(t, err)

		parentJobIDs = append(parentJobIDs, parentJobID)
	}

	// Create a set of channel updates, that must wait until their
	// associated channel announcement has been verified.
	chanUpds := make([]*lnwire.ChannelUpdate1, 0, numTasks)
	childJobIDs := make([]JobID, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		chanUpds = append(chanUpds, &lnwire.ChannelUpdate1{
			ShortChannelID: lnwire.NewShortChanIDFromInt(uint64(i)),
		})
		childJob, err := barrier.InitJobDependencies(chanUpds[i])
		require.NoError(t, err)

		childJobIDs = append(childJobIDs, childJob)
	}

	// Spawn additional tasks that will send the error returned after
	// waiting for the announcements to finish. In the background, we will
	// iteratively queue the channel updates, which will send back the error
	// returned from waiting.
	jobErrs := make(chan error)
	for i := 0; i < numTasks; i++ {
		go func(ii int) {
			jobErrs <- barrier.WaitForParents(
				childJobIDs[ii], chanUpds[ii],
			)
		}(i)
	}

	// Check that no jobs are added while semaphore is full.
	select {
	case <-time.After(timeout):
		// Expected since no slots open.
	case <-jobErrs:
		t.Fatalf("job should not have been signaled")
	}

	// Complete the first half of jobs, one at a time, verifying that they
	// get signaled. Then, quit the barrier and check that all others exit
	// with the correct error.
	for i := 0; i < numTasks; i++ {
		switch {
		case i < numTasks/2:
			err := barrier.SignalDependents(
				anns[i], parentJobIDs[i],
			)
			require.NoError(t, err)

			barrier.CompleteJob()

		// At midpoint, quit the validation barrier.
		case i == numTasks/2:
			close(quit)
		}

		var err error
		select {
		case <-time.After(timeout):
			t.Fatalf("timeout waiting for job to be signaled")
		case err = <-jobErrs:
		}

		switch {
		// First half should return without failure.
		case i < numTasks/2 && err != nil:
			t.Fatalf("unexpected failure while waiting: %v", err)

		// Last half should return the shutdown error.
		case i >= numTasks/2 && !errors.Is(
			err, ErrVBarrierShuttingDown,
		):

			t.Fatalf("expected failure after quitting: want %v, "+
				"got %v", ErrVBarrierShuttingDown, err)
		}
	}
}

// TestValidationBarrierParentJobsClear tests that creating two parent jobs for
// ChannelUpdate / NodeAnnouncement1 will pause child jobs until the set of
// parent jobs has cleared.
func TestValidationBarrierParentJobsClear(t *testing.T) {
	t.Parallel()

	const (
		numTasks = 8
		timeout  = time.Second
	)

	quit := make(chan struct{})
	barrier := NewValidationBarrier(numTasks, quit)

	sharedScid := lnwire.NewShortChanIDFromInt(0)
	sharedNodeID := nodeIDFromInt(0)

	// Create a set of gossip messages that depend on each other. ann1 and
	// ann2 share the ShortChannelID field. ann1 and ann3 share both the
	// ShortChannelID field and the NodeID1 field. These shared values let
	// us test the "set" properties of the ValidationBarrier.
	ann1 := &lnwire.ChannelAnnouncement1{
		ShortChannelID: sharedScid,
		NodeID1:        sharedNodeID,
		NodeID2:        nodeIDFromInt(1),
	}
	parentID1, err := barrier.InitJobDependencies(ann1)
	require.NoError(t, err)

	ann2 := &lnwire.ChannelAnnouncement1{
		ShortChannelID: sharedScid,
		NodeID1:        nodeIDFromInt(2),
		NodeID2:        nodeIDFromInt(3),
	}
	parentID2, err := barrier.InitJobDependencies(ann2)
	require.NoError(t, err)

	ann3 := &lnwire.ChannelAnnouncement1{
		ShortChannelID: sharedScid,
		NodeID1:        sharedNodeID,
		NodeID2:        nodeIDFromInt(10),
	}
	parentID3, err := barrier.InitJobDependencies(ann3)
	require.NoError(t, err)

	// Create the ChannelUpdate & NodeAnnouncement1 messages.
	upd1 := &lnwire.ChannelUpdate1{
		ShortChannelID: sharedScid,
	}
	childID1, err := barrier.InitJobDependencies(upd1)
	require.NoError(t, err)

	node1 := &lnwire.NodeAnnouncement1{
		NodeID: sharedNodeID,
	}
	childID2, err := barrier.InitJobDependencies(node1)
	require.NoError(t, err)

	run := func(vb *ValidationBarrier, childJobID JobID, job interface{},
		resp chan error, start chan error) {

		close(start)

		err := vb.WaitForParents(childJobID, job)
		resp <- err
	}

	errChan := make(chan error, 2)

	startChan1 := make(chan error, 1)
	startChan2 := make(chan error, 1)

	go run(barrier, childID1, upd1, errChan, startChan1)
	go run(barrier, childID2, node1, errChan, startChan2)

	// Wait for the start signal since we are testing the case where the
	// parent jobs only complete _after_ the child jobs have called. Note
	// that there is technically an edge case where we receive the start
	// signal and call SignalDependents before WaitForParents can actually
	// be called in the goroutine launched above. In this case, which
	// arises due to our inability to control precisely when these VB
	// methods are scheduled (as they are in different goroutines), the
	// test should still pass as we want to test that validation jobs are
	// completing and not stalling. In other words, this issue with the
	// test itself is good as it actually randomizes some of the ordering,
	// occasionally. This tests that the VB is robust against ordering /
	// concurrency issues.
	select {
	case <-startChan1:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for startChan1")
	}

	select {
	case <-startChan2:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for startChan2")
	}

	// Now we can call SignalDependents for our parent jobs.
	err = barrier.SignalDependents(ann1, parentID1)
	require.NoError(t, err)

	err = barrier.SignalDependents(ann2, parentID2)
	require.NoError(t, err)

	err = barrier.SignalDependents(ann3, parentID3)
	require.NoError(t, err)

	select {
	case <-errChan:
	case <-time.After(timeout):
		t.Fatal("unexpected timeout waiting for first error signal")
	}

	select {
	case <-errChan:
	case <-time.After(timeout):
		t.Fatal("unexpected timeout waiting for second error signal")
	}
}

// nodeIDFromInt creates a node ID by writing a uint64 to the first 8 bytes.
func nodeIDFromInt(i uint64) [33]byte {
	var nodeID [33]byte
	binary.BigEndian.PutUint64(nodeID[:8], i)
	return nodeID
}

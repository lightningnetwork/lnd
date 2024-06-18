package graph_test

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/graph"
	"github.com/lightningnetwork/lnd/lnwire"
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
	barrier := graph.NewValidationBarrier(numTasks, quit)

	// Saturate the semaphore with jobs.
	for i := 0; i < numTasks; i++ {
		barrier.InitJobDependencies(nil)
	}

	// Spawn additional tasks that will signal completion when added.
	jobAdded := make(chan struct{})
	for i := 0; i < numPendingTasks; i++ {
		go func() {
			barrier.InitJobDependencies(nil)
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
	barrier := graph.NewValidationBarrier(2*numTasks, quit)

	// Create a set of unique channel announcements that we will prep for
	// validation.
	anns := make([]*lnwire.ChannelAnnouncement, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		anns = append(anns, &lnwire.ChannelAnnouncement{
			ShortChannelID: lnwire.NewShortChanIDFromInt(uint64(i)),
			NodeID1:        nodeIDFromInt(uint64(2 * i)),
			NodeID2:        nodeIDFromInt(uint64(2*i + 1)),
		})
		barrier.InitJobDependencies(anns[i])
	}

	// Create a set of channel updates, that must wait until their
	// associated channel announcement has been verified.
	chanUpds := make([]*lnwire.ChannelUpdate, 0, numTasks)
	for i := 0; i < numTasks; i++ {
		chanUpds = append(chanUpds, &lnwire.ChannelUpdate{
			ShortChannelID: lnwire.NewShortChanIDFromInt(uint64(i)),
		})
		barrier.InitJobDependencies(chanUpds[i])
	}

	// Spawn additional tasks that will send the error returned after
	// waiting for the announcements to finish. In the background, we will
	// iteratively queue the channel updates, which will send back the error
	// returned from waiting.
	jobErrs := make(chan error)
	for i := 0; i < numTasks; i++ {
		go func(ii int) {
			jobErrs <- barrier.WaitForDependants(chanUpds[ii])
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
		// Signal completion for the first half of tasks, but only allow
		// dependents to be processed as well for the second quarter.
		case i < numTasks/4:
			barrier.SignalDependants(anns[i], false)
			barrier.CompleteJob()

		case i < numTasks/2:
			barrier.SignalDependants(anns[i], true)
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
		case i < numTasks/4 && !graph.IsError(
			err, graph.ErrParentValidationFailed,
		):
			t.Fatalf("unexpected failure while waiting: %v", err)

		case i >= numTasks/4 && i < numTasks/2 && err != nil:
			t.Fatalf("unexpected failure while waiting: %v", err)

		// Last half should return the shutdown error.
		case i >= numTasks/2 && !graph.IsError(
			err, graph.ErrVBarrierShuttingDown,
		):
			t.Fatalf("expected failure after quitting: want %v, "+
				"got %v", graph.ErrVBarrierShuttingDown, err)
		}
	}
}

// nodeIDFromInt creates a node ID by writing a uint64 to the first 8 bytes.
func nodeIDFromInt(i uint64) [33]byte {
	var nodeID [33]byte
	binary.BigEndian.PutUint64(nodeID[:8], i)
	return nodeID
}

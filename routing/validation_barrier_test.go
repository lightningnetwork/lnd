package routing_test

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

// TestValidationBarrierSemaphore checks basic properties of the validation
// barrier's semaphore wrt. enqueuing/dequeuing.
func TestValidationBarrierSemaphore(t *testing.T) {
	const (
		numTasks        = 8
		numPendingTasks = 8
		timeout         = 50 * time.Millisecond
	)

	quit := make(chan struct{})
	barrier := routing.NewValidationBarrier(numTasks, quit)

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
	const (
		numTasks = 8
		timeout  = 50 * time.Millisecond
	)

	quit := make(chan struct{})
	barrier := routing.NewValidationBarrier(2*numTasks, quit)

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
		// First half, signal completion and task semaphore
		case i < numTasks/2:
			barrier.SignalDependants(anns[i])
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
		case i >= numTasks/2 && err != routing.ErrVBarrierShuttingDown:
			t.Fatalf("expected failure after quitting: want %v, "+
				"got %v", routing.ErrVBarrierShuttingDown, err)
		}
	}
}

// nodeIDFromInt creates a node ID by writing a uint64 to the first 8 bytes.
func nodeIDFromInt(i uint64) [33]byte {
	var nodeID [33]byte
	binary.BigEndian.PutUint64(nodeID[:8], i)
	return nodeID
}

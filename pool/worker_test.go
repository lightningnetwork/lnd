package pool_test

import (
	"bytes"
	crand "crypto/rand"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/buffer"
	"github.com/lightningnetwork/lnd/pool"
)

type workerPoolTest struct {
	name       string
	newPool    func() interface{}
	numWorkers int
}

// TestConcreteWorkerPools asserts the behavior of any concrete implementations
// of worker pools provided by the pool package. Currently this tests the
// pool.Read and pool.Write instances.
func TestConcreteWorkerPools(t *testing.T) {
	const (
		gcInterval     = time.Second
		expiryInterval = 250 * time.Millisecond
		numWorkers     = 5
		workerTimeout  = 500 * time.Millisecond
	)

	tests := []workerPoolTest{
		{
			name: "write pool",
			newPool: func() interface{} {
				bp := pool.NewWriteBuffer(
					gcInterval, expiryInterval,
				)

				return pool.NewWrite(
					bp, numWorkers, workerTimeout,
				)
			},
			numWorkers: numWorkers,
		},
		{
			name: "read pool",
			newPool: func() interface{} {
				bp := pool.NewReadBuffer(
					gcInterval, expiryInterval,
				)

				return pool.NewRead(
					bp, numWorkers, workerTimeout,
				)
			},
			numWorkers: numWorkers,
		},
	}

	for _, test := range tests {
		testWorkerPool(t, test)
	}
}

func testWorkerPool(t *testing.T, test workerPoolTest) {
	t.Run(test.name+" non blocking", func(t *testing.T) {
		t.Parallel()

		p := test.newPool()
		startGeneric(t, p)
		defer stopGeneric(t, p)

		submitNonblockingGeneric(t, p, test.numWorkers)
	})

	t.Run(test.name+" blocking", func(t *testing.T) {
		t.Parallel()

		p := test.newPool()
		startGeneric(t, p)
		defer stopGeneric(t, p)

		submitBlockingGeneric(t, p, test.numWorkers)
	})

	t.Run(test.name+" partial blocking", func(t *testing.T) {
		t.Parallel()

		p := test.newPool()
		startGeneric(t, p)
		defer stopGeneric(t, p)

		submitPartialBlockingGeneric(t, p, test.numWorkers)
	})
}

// submitNonblockingGeneric asserts that queueing tasks to the worker pool and
// allowing them all to unblock simultaneously results in all of the tasks being
// completed in a timely manner.
func submitNonblockingGeneric(t *testing.T, p interface{}, nWorkers int) {
	// We'll submit 2*nWorkers tasks that will all be unblocked
	// simultaneously.
	nUnblocked := 2 * nWorkers

	// First we'll queue all of the tasks for the pool.
	errChan := make(chan error)
	semChan := make(chan struct{})
	for i := 0; i < nUnblocked; i++ {
		go func() { errChan <- submitGeneric(p, semChan) }()
	}

	// Since we haven't signaled the semaphore, none of the them should
	// complete.
	pullNothing(t, errChan)

	// Now, unblock them all simultaneously. All of the tasks should then be
	// processed in parallel. Afterward, no more errors should come through.
	close(semChan)
	pullParllel(t, nUnblocked, errChan)
	pullNothing(t, errChan)
}

// submitBlockingGeneric asserts that submitting blocking tasks to the pool and
// unblocking each sequentially results in a single task being processed at a
// time.
func submitBlockingGeneric(t *testing.T, p interface{}, nWorkers int) {
	// We'll submit 2*nWorkers tasks that will be unblocked sequentially.
	nBlocked := 2 * nWorkers

	// First, queue all of the blocking tasks for the pool.
	errChan := make(chan error)
	semChan := make(chan struct{})
	for i := 0; i < nBlocked; i++ {
		go func() { errChan <- submitGeneric(p, semChan) }()
	}

	// Since we haven't signaled the semaphore, none of them should
	// complete.
	pullNothing(t, errChan)

	// Now, pull each blocking task sequentially from the pool. Afterwards,
	// no more errors should come through.
	pullSequntial(t, nBlocked, errChan, semChan)
	pullNothing(t, errChan)

}

// submitPartialBlockingGeneric tests that so long as one worker is not blocked,
// any other non-blocking submitted tasks can still be processed.
func submitPartialBlockingGeneric(t *testing.T, p interface{}, nWorkers int) {
	// We'll submit nWorkers-1 tasks that will be initially blocked, the
	// remainder will all be unblocked simultaneously. After the unblocked
	// tasks have finished, we will sequentially unblock the nWorkers-1
	// tasks that were first submitted.
	nBlocked := nWorkers - 1
	nUnblocked := 2*nWorkers - nBlocked

	// First, submit all of the blocking tasks to the pool.
	errChan := make(chan error)
	semChan := make(chan struct{})
	for i := 0; i < nBlocked; i++ {
		go func() { errChan <- submitGeneric(p, semChan) }()
	}

	// Since these are all blocked, no errors should be returned yet.
	pullNothing(t, errChan)

	// Now, add all of the non-blocking task to the pool.
	semChanNB := make(chan struct{})
	for i := 0; i < nUnblocked; i++ {
		go func() { errChan <- submitGeneric(p, semChanNB) }()
	}

	// Since we haven't unblocked the second batch, we again expect no tasks
	// to finish.
	pullNothing(t, errChan)

	// Now, unblock the unblocked task and pull all of them. After they have
	// been pulled, we should see no more tasks.
	close(semChanNB)
	pullParllel(t, nUnblocked, errChan)
	pullNothing(t, errChan)

	// Finally, unblock each the blocked tasks we added initially, and
	// assert that no further errors come through.
	pullSequntial(t, nBlocked, errChan, semChan)
	pullNothing(t, errChan)
}

func pullNothing(t *testing.T, errChan chan error) {
	t.Helper()

	select {
	case err := <-errChan:
		t.Fatalf("received unexpected error before semaphore "+
			"release: %v", err)

	case <-time.After(time.Second):
	}
}

func pullParllel(t *testing.T, n int, errChan chan error) {
	t.Helper()

	for i := 0; i < n; i++ {
		select {
		case err := <-errChan:
			if err != nil {
				t.Fatal(err)
			}

		case <-time.After(time.Second):
			t.Fatalf("task %d was not processed in time", i)
		}
	}
}

func pullSequntial(t *testing.T, n int, errChan chan error, semChan chan struct{}) {
	t.Helper()

	for i := 0; i < n; i++ {
		// Signal for another task to unblock.
		select {
		case semChan <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatalf("task %d was not unblocked", i)
		}

		// Wait for the error to arrive, we expect it to be non-nil.
		select {
		case err := <-errChan:
			if err != nil {
				t.Fatal(err)
			}

		case <-time.After(time.Second):
			t.Fatalf("task %d was not processed in time", i)
		}
	}
}

func startGeneric(t *testing.T, p interface{}) {
	t.Helper()

	var err error
	switch pp := p.(type) {
	case *pool.Write:
		err = pp.Start()

	case *pool.Read:
		err = pp.Start()

	default:
		t.Fatalf("unknown worker pool type: %T", p)
	}

	if err != nil {
		t.Fatalf("unable to start worker pool: %v", err)
	}
}

func stopGeneric(t *testing.T, p interface{}) {
	t.Helper()

	var err error
	switch pp := p.(type) {
	case *pool.Write:
		err = pp.Stop()

	case *pool.Read:
		err = pp.Stop()

	default:
		t.Fatalf("unknown worker pool type: %T", p)
	}

	if err != nil {
		t.Fatalf("unable to stop worker pool: %v", err)
	}
}

func submitGeneric(p interface{}, sem <-chan struct{}) error {
	var err error
	switch pp := p.(type) {
	case *pool.Write:
		err = pp.Submit(func(buf *bytes.Buffer) error {
			// Verify that the provided buffer has been reset to be
			// zero length.
			if buf.Len() != 0 {
				return fmt.Errorf("buf should be length zero, "+
					"instead has length %d", buf.Len())
			}

			// Verify that the capacity of the buffer has the
			// correct underlying size of a buffer.WriteSize.
			if buf.Cap() != buffer.WriteSize {
				return fmt.Errorf("buf should have capacity "+
					"%d, instead has capacity %d",
					buffer.WriteSize, buf.Cap())
			}

			// Sample some random bytes that we'll use to dirty the
			// buffer.
			b := make([]byte, rand.Intn(buf.Cap()))
			_, err := io.ReadFull(crand.Reader, b)
			if err != nil {
				return err
			}

			// Write the random bytes the buffer.
			_, err = buf.Write(b)

			// Wait until this task is signaled to exit.
			<-sem

			return err
		})

	case *pool.Read:
		err = pp.Submit(func(buf *buffer.Read) error {
			// Assert that all of the bytes in the provided array
			// are zero, indicating that the buffer was reset
			// between uses.
			for i := range buf[:] {
				if buf[i] != 0x00 {
					return fmt.Errorf("byte %d of "+
						"buffer.Read should be "+
						"0, instead is %d", i, buf[i])
				}
			}

			// Sample some random bytes to read into the buffer.
			_, err := io.ReadFull(crand.Reader, buf[:])

			// Wait until this task is signaled to exit.
			<-sem

			return err

		})

	default:
		return fmt.Errorf("unknown worker pool type: %T", p)
	}

	if err != nil {
		return fmt.Errorf("unable to submit task: %v", err)
	}

	return nil
}

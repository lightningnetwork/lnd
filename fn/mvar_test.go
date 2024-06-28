package fn

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/stretchr/testify/require"
)

// blockTimeout is a parameter that defines all of the waiting periods for the
// tests in this file. Generally there is a tradeoff here where the higher this
// value is, the less flaky the tests will be, at the expense of the tests
// taking longer to execute.
const blockTimeout = time.Millisecond

// TestTakeNewEmptyBlocks ensures that if we initialize an empty MVar and
// immediately try to Take from it, it will block.
func TestTakeNewEmptyBlocks(t *testing.T) {
	t.Parallel()

	m := NewEmptyMVar[uint8]()
	require.True(t, blocks(func() { m.Take() }))
}

// TestTakeNewMVarProceeds ensures that if we initialize an MVar with a value
// in it and immediately try to Take from it, it will succeed.
func TestTakeNewMVarProceeds(t *testing.T) {
	t.Parallel()

	m := NewMVar[uint8](0)
	require.False(t, blocks(func() { m.Take() }))
}

// TestPutNewMVarBlocks ensures that if we initialize an MVar with a value in
// it and immediately try to Put a new value into it, it will block.
func TestPutNewMVarBlocks(t *testing.T) {
	t.Parallel()

	m := NewMVar[uint8](0)
	require.True(t, blocks(func() { m.Put(1) }))
}

// TestPutNewEmptyProceeds ensures that if we initialize an empty Mvar and then
// try to Put a new value into it, it will succeed.
func TestPutNewEmptyProceeds(t *testing.T) {
	t.Parallel()

	m := NewEmptyMVar[uint8]()
	require.False(t, blocks(func() { m.Put(1) }))
}

// TestPutWhenEmptyLeavesFull ensures that we successfully leave the Mvar in a
// full state after executing a Put in an empty state.
func TestPutWhenEmptyLeavesFull(t *testing.T) {
	t.Parallel()

	m := NewEmptyMVar[uint8]()
	m.Put(0)
	if m.IsEmpty() {
		t.Fatal("Put left empty")
	}
}

// TestTakeWhenFullLeavesEmpty ensures that we successfully leave the Mvar in an
// empty state after executing a Take in a full state.
func TestTakeWhenFullLeavesEmpty(t *testing.T) {
	t.Parallel()

	m := NewMVar[uint8](0)
	m.Take()
	if m.IsFull() {
		t.Fatal("Take left full")
	}
}

// TestReadWhenFullLeavesFull ensures that a Read when in a full state does not
// change the state of the MVar.
func TestReadWhenFullLeavesFull(t *testing.T) {
	t.Parallel()

	m := NewMVar[uint8](0)
	m.Read()
	if m.IsEmpty() {
		t.Fatal("Read left empty")
	}
}

// TestTakeAfterTryTakeBlocks ensures that regardless of what state the Mvar
// begins in, if we try to Take immediately after a TryTake, it will always
// block.
func TestTakeAfterTryTakeBlocks(t *testing.T) {
	t.Parallel()

	err := quick.Check(func(set bool, n uint8) bool {
		m := gen(set, n)
		m.TryTake()
		return blocks(func() { m.Take() })
	}, nil)
	require.NoError(t, err, "Take after TryTake did not block")
}

// TestPutAfterTryPutBlocks ensures that regardless of what state the MVar
// begins in, if we try to Put immediately after a TryPut, it will always block.
func TestPutAfterTryPutBlocks(t *testing.T) {
	t.Parallel()

	err := quick.Check(func(set bool, n uint8) bool {
		m := gen(set, n)
		m.TryPut(0)
		return blocks(func() { m.Put(1) })
	}, nil)
	require.NoError(t, err, "Put after TryPut did not block")
}

// TestTryTakeLeavesEmpty ensures that regardless of what state the MVar begins
// in, if we execute a TryTake, the resulting MVar state is empty.
func TestTryTakeLeavesEmpty(t *testing.T) {
	t.Parallel()

	err := quick.Check(func(set bool, n uint8) bool {
		m := gen(set, n)
		m.TryTake()
		return m.IsEmpty()
	}, nil)
	require.NoError(t, err, "TryTake did not leave empty")
}

// TestTryPutLeavesFull ensures that regardless of what state the MVar begins
// in, if we execute a TryPut, the resulting MVar state is full.
func TestTryPutLeavesFull(t *testing.T) {
	t.Parallel()

	err := quick.Check(func(set bool, n uint8) bool {
		m := gen(set, n)
		m.TryPut(n)
		return m.IsFull()
	}, nil)
	require.NoError(t, err, "TryPut did not leave full")
}

// TestReadWhenEmptyBlocks ensures that if an MVar is in an empty state, a
// Read operation will block.
func TestReadWhenEmptyBlocks(t *testing.T) {
	t.Parallel()

	err := quick.Check(func(set bool, n uint8) bool {
		m := gen(set, n)
		return implies(m.IsEmpty(), blocks(func() { m.Read() }))
	}, nil)
	require.NoError(t, err, "Read did not block when empty")
}

// TestTryReadNops ensures that a TryRead will not change the state of the MVar.
// It implicitly tests a second property which is that TryRead will never block.
func TestTryReadNops(t *testing.T) {
	t.Parallel()

	err := quick.Check(func(set bool, n uint8) bool {
		m := gen(set, n)
		before := m.IsEmpty()
		tryReadBlocked := blocks(func() { m.TryRead() })
		after := m.IsEmpty()

		return before == after && !tryReadBlocked
	}, nil)
	require.NoError(t, err, "TryRead did not leave state unchanged")
}

// TestPutWakesAllReaders ensures the property that if we have many blocked
// Read operations that are waiting for the MVar to be filled, all of them are
// woken up when we execute a Put operation.
func TestPutWakesAllReaders(t *testing.T) {
	t.Parallel()

	m := NewEmptyMVar[uint8]()
	v := uint8(21)
	n := uint32(10)

	counter := atomic.Uint32{}
	wg := sync.WaitGroup{}
	for i := uint32(0); i < n; i++ {
		wg.Add(1)
		go func() {
			x := m.Read()
			if x == v {
				counter.Add(1)
			}
			wg.Done()
		}()
	}
	m.Put(v)
	wg.Wait()
	require.Equal(t, counter.Load(), n, "not all readers given same value")
}

// TestPutWakesReadersBeforeTaker ensures the property that a waiting taker
// does not preempt any waiting readers. This test construction is a bit
// delicate, using a Sleep to ensure that all goroutines that are set up to
// wait on the Put have gotten to the point where they actually block.
func TestPutWakesReadersBeforeTaker(t *testing.T) {
	t.Parallel()

	m := NewEmptyMVar[uint8]()
	v := uint8(21)
	n := uint32(10)

	counter := atomic.Uint32{}
	wg := sync.WaitGroup{}

	// Set up taker first.
	wg.Add(1)
	go func() {
		x := m.Take()
		if x == v {
			counter.Add(1)
		}
		wg.Done()
	}()

	// Set up readers.
	for i := uint32(0); i < n; i++ {
		wg.Add(1)
		go func() {
			x := m.Read()
			if x == v {
				counter.Add(1)
			}
			wg.Done()
		}()
	}

	time.Sleep(blockTimeout) // Forgive me

	m.Put(v)
	wg.Wait()
	require.Equal(
		t, counter.Load(), n+1, "readers did not wake before taker",
	)
}

// TestPutWakesOneTaker ensures the property that only a single blocked Take
// operation wakes when a Put comes in. This test construction is a bit delicate
// using a Sleep to wait for the counter to be incremented by the Take goroutine
// after waking.
func TestPutWakesOneTaker(t *testing.T) {
	t.Parallel()

	m := NewEmptyMVar[uint8]()
	v := uint8(21)
	n := uint32(10)

	counter := atomic.Uint32{}
	wg := sync.WaitGroup{}
	for i := uint32(0); i < n; i++ {
		wg.Add(1)
		go func() {
			x := m.Take()
			if x == v {
				counter.Add(1)
			}
			wg.Done()
		}()
	}
	m.Put(v)

	time.Sleep(blockTimeout) // Forgive me

	require.Equal(
		t,
		counter.Load(),
		uint32(1),
		"put wakes zero or more than one taker ",
	)
}

// TestTakeWakesPutter ensures that if there is a blocked Put operation due to
// the MVar being full, that it unblocks when a Take operation is executed. This
// is because the Take operation would set the MVar to an empty state, allowing
// the blocked Put to proceed.
func TestTakeWakesPutter(t *testing.T) {
	t.Parallel()

	m := NewMVar[uint8](0)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() { m.Put(1); wg.Done() }()
	m.Take()
	wg.Wait()
	require.Equal(t, m.Read(), uint8(1))
}

// TestReadLeavesAllBlockedPutsBlocked ensures that a Read operation does not
// allow a blocked Put to proceed.
func TestReadLeavesAllBlockedPutsBlocked(t *testing.T) {
	t.Parallel()

	m := NewMVar[uint8](0)

	unblocked := make(chan struct{})

	go func() { m.Put(1); unblocked <- struct{}{} }()

	v := m.Read()
	require.Equal(t, uint8(0), v)

	select {
	case <-unblocked:
		t.Fatalf("Put unblocked by Read")
	case <-time.NewTimer(blockTimeout).C:
	}
}

// TestBlockingInterleaveStress ensures that with a random interleaving of Puts,
// Reads, and Takes, nothing panics.
func TestBlockingInterleaveStress(t *testing.T) {
	t.Parallel()

	m := NewMVar[uint8](0)

	wg := sync.WaitGroup{}
	for i := uint8(0); i < 100; i++ {
		i := i
		wg.Add(3)
		go func() {
			m.Put(i)
			wg.Done()
		}()
		go func() {
			m.Take()
			wg.Done()
		}()
		go func() {
			m.Read()
			wg.Done()
		}()
	}
	wg.Wait()
}

// blocks is a helper function to decide if the supplied function blocks or not.
// This is not fool-proof since it does make a judgement call based off of the
// file-global timeout parameter.
func blocks(f func()) bool {
	unblocked := make(chan struct{})
	go func() { f(); unblocked <- struct{}{} }()

	select {
	case <-unblocked:
		return false

	case <-time.NewTimer(blockTimeout).C:
		return true
	}
}

// implies is a helper function that computes the `=>` operation from boolean
// algebra. It is true when the first argument is false, or if both arguments
// are true.
func implies(b bool, b2 bool) bool {
	return !b || b && b2
}

// gen is a helper function whose first argument decides if the returned MVar
// should be full or empty, and the second argument decides what value should
// be in the MVar if it is full. We use this because it is substantially easier
// than teaching testing.Quick how to generate random MVar[uint8] values.
func gen(set bool, n uint8) MVar[uint8] {
	if set {
		return NewMVar[uint8](n)
	}

	return NewEmptyMVar[uint8]()
}

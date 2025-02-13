package chainio

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestNotifyAndWaitOnConsumerErr asserts when the consumer returns an error,
// it's returned by notifyAndWait.
func TestNotifyAndWaitOnConsumerErr(t *testing.T) {
	t.Parallel()

	// Create a mock consumer.
	consumer := &MockConsumer{}
	defer consumer.AssertExpectations(t)
	consumer.On("Name").Return("mocker")

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Mock ProcessBlock to return an error.
	consumer.On("ProcessBlock", mockBeat).Return(errDummy).Once()

	// Call the method under test.
	err := notifyAndWait(mockBeat, consumer, DefaultProcessBlockTimeout)

	// We expect the error to be returned.
	require.ErrorIs(t, err, errDummy)
}

// TestNotifyAndWaitOnConsumerErr asserts when the consumer successfully
// processed the beat, no error is returned.
func TestNotifyAndWaitOnConsumerSuccess(t *testing.T) {
	t.Parallel()

	// Create a mock consumer.
	consumer := &MockConsumer{}
	defer consumer.AssertExpectations(t)
	consumer.On("Name").Return("mocker")

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Mock ProcessBlock to return nil.
	consumer.On("ProcessBlock", mockBeat).Return(nil).Once()

	// Call the method under test.
	err := notifyAndWait(mockBeat, consumer, DefaultProcessBlockTimeout)

	// We expect a nil error to be returned.
	require.NoError(t, err)
}

// TestNotifyAndWaitOnConsumerTimeout asserts when the consumer times out
// processing the block, the timeout error is returned.
func TestNotifyAndWaitOnConsumerTimeout(t *testing.T) {
	t.Parallel()

	// Set timeout to be 10ms.
	processBlockTimeout := 10 * time.Millisecond

	// Create a mock consumer.
	consumer := &MockConsumer{}
	defer consumer.AssertExpectations(t)
	consumer.On("Name").Return("mocker")

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Mock ProcessBlock to return nil but blocks on returning.
	consumer.On("ProcessBlock", mockBeat).Return(nil).Run(
		func(args mock.Arguments) {
			// Sleep one second to block on the method.
			time.Sleep(processBlockTimeout * 100)
		}).Once()

	// Call the method under test.
	err := notifyAndWait(mockBeat, consumer, processBlockTimeout)

	// We expect a timeout error to be returned.
	require.ErrorIs(t, err, ErrProcessBlockTimeout)
}

// TestDispatchSequential checks that the beat is sent to the consumers
// sequentially.
func TestDispatchSequential(t *testing.T) {
	t.Parallel()

	// Create three mock consumers.
	consumer1 := &MockConsumer{}
	defer consumer1.AssertExpectations(t)
	consumer1.On("Name").Return("mocker1")

	consumer2 := &MockConsumer{}
	defer consumer2.AssertExpectations(t)
	consumer2.On("Name").Return("mocker2")

	consumer3 := &MockConsumer{}
	defer consumer3.AssertExpectations(t)
	consumer3.On("Name").Return("mocker3")

	consumers := []Consumer{consumer1, consumer2, consumer3}

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// prevConsumer specifies the previous consumer that was called.
	var prevConsumer string

	// Mock the ProcessBlock on consumers to return immediately.
	consumer1.On("ProcessBlock", mockBeat).Return(nil).Run(
		func(args mock.Arguments) {
			// Check the order of the consumers.
			//
			// The first consumer should have no previous consumer.
			require.Empty(t, prevConsumer)

			// Set the consumer as the previous consumer.
			prevConsumer = consumer1.Name()
		}).Once()

	consumer2.On("ProcessBlock", mockBeat).Return(nil).Run(
		func(args mock.Arguments) {
			// Check the order of the consumers.
			//
			// The second consumer should see consumer1.
			require.Equal(t, consumer1.Name(), prevConsumer)

			// Set the consumer as the previous consumer.
			prevConsumer = consumer2.Name()
		}).Once()

	consumer3.On("ProcessBlock", mockBeat).Return(nil).Run(
		func(args mock.Arguments) {
			// Check the order of the consumers.
			//
			// The third consumer should see consumer2.
			require.Equal(t, consumer2.Name(), prevConsumer)

			// Set the consumer as the previous consumer.
			prevConsumer = consumer3.Name()
		}).Once()

	// Call the method under test.
	err := DispatchSequential(mockBeat, consumers)
	require.NoError(t, err)

	// Check the previous consumer is the last consumer.
	require.Equal(t, consumer3.Name(), prevConsumer)
}

// TestRegisterQueue tests the RegisterQueue function.
func TestRegisterQueue(t *testing.T) {
	t.Parallel()

	// Create two mock consumers.
	consumer1 := &MockConsumer{}
	defer consumer1.AssertExpectations(t)
	consumer1.On("Name").Return("mocker1")

	consumer2 := &MockConsumer{}
	defer consumer2.AssertExpectations(t)
	consumer2.On("Name").Return("mocker2")

	consumers := []Consumer{consumer1, consumer2}

	// Create a mock chain notifier.
	mockNotifier := &chainntnfs.MockChainNotifier{}
	defer mockNotifier.AssertExpectations(t)

	// Create a new dispatcher.
	b := NewBlockbeatDispatcher(mockNotifier)

	// Register the consumers.
	b.RegisterQueue(consumers)

	// Assert that the consumers have been registered.
	//
	// We should have one queue.
	require.Len(t, b.consumerQueues, 1)

	// The queue should have two consumers.
	queue, ok := b.consumerQueues[1]
	require.True(t, ok)
	require.Len(t, queue, 2)
}

// TestStartDispatcher tests the Start method.
func TestStartDispatcher(t *testing.T) {
	t.Parallel()

	// Create a mock chain notifier.
	mockNotifier := &chainntnfs.MockChainNotifier{}
	defer mockNotifier.AssertExpectations(t)

	// Create a new dispatcher.
	b := NewBlockbeatDispatcher(mockNotifier)

	// Start the dispatcher without consumers should return an error.
	err := b.Start()
	require.Error(t, err)

	// Create a consumer and register it.
	consumer := &MockConsumer{}
	defer consumer.AssertExpectations(t)
	consumer.On("Name").Return("mocker1")
	b.RegisterQueue([]Consumer{consumer})

	// Mock the chain notifier to return an error.
	mockNotifier.On("RegisterBlockEpochNtfn",
		mock.Anything).Return(nil, errDummy).Once()

	// Start the dispatcher now should return the error.
	err = b.Start()
	require.ErrorIs(t, err, errDummy)

	// Mock the chain notifier to return a valid notifier.
	blockEpochs := &chainntnfs.BlockEpochEvent{}
	mockNotifier.On("RegisterBlockEpochNtfn",
		mock.Anything).Return(blockEpochs, nil).Once()

	// Start the dispatcher now should not return an error.
	err = b.Start()
	require.NoError(t, err)
}

// TestDispatchBlocks asserts the blocks are properly dispatched to the queues.
func TestDispatchBlocks(t *testing.T) {
	t.Parallel()

	// Create a mock chain notifier.
	mockNotifier := &chainntnfs.MockChainNotifier{}
	defer mockNotifier.AssertExpectations(t)

	// Create a new dispatcher.
	b := NewBlockbeatDispatcher(mockNotifier)

	// Create the beat and attach it to the dispatcher.
	epoch := chainntnfs.BlockEpoch{Height: 1}
	beat := NewBeat(epoch)
	b.beat = beat

	// Create a consumer and register it.
	consumer := &MockConsumer{}
	defer consumer.AssertExpectations(t)
	consumer.On("Name").Return("mocker1")
	b.RegisterQueue([]Consumer{consumer})

	// Mock the consumer to return nil error on ProcessBlock. This
	// implicitly asserts that the step `notifyQueues` is successfully
	// reached in the `dispatchBlocks` method.
	consumer.On("ProcessBlock", mock.Anything).Return(nil).Once()

	// Create a test epoch chan.
	epochChan := make(chan *chainntnfs.BlockEpoch, 1)
	blockEpochs := &chainntnfs.BlockEpochEvent{
		Epochs: epochChan,
		Cancel: func() {},
	}

	// Call the method in a goroutine.
	done := make(chan struct{})
	b.wg.Add(1)
	go func() {
		defer close(done)
		b.dispatchBlocks(blockEpochs)
	}()

	// Send an epoch.
	epoch = chainntnfs.BlockEpoch{Height: 2}
	epochChan <- &epoch

	// Wait for the dispatcher to process the epoch.
	time.Sleep(100 * time.Millisecond)

	// Stop the dispatcher.
	b.Stop()

	// We expect the dispatcher to stop immediately.
	_, err := fn.RecvOrTimeout(done, time.Second)
	require.NoError(t, err)
}

// TestNotifyQueuesSuccess checks when the dispatcher successfully notifies all
// the queues, no error is returned.
func TestNotifyQueuesSuccess(t *testing.T) {
	t.Parallel()

	// Create two mock consumers.
	consumer1 := &MockConsumer{}
	defer consumer1.AssertExpectations(t)
	consumer1.On("Name").Return("mocker1")

	consumer2 := &MockConsumer{}
	defer consumer2.AssertExpectations(t)
	consumer2.On("Name").Return("mocker2")

	// Create two queues.
	queue1 := []Consumer{consumer1}
	queue2 := []Consumer{consumer2}

	// Create a mock chain notifier.
	mockNotifier := &chainntnfs.MockChainNotifier{}
	defer mockNotifier.AssertExpectations(t)

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Create a new dispatcher.
	b := NewBlockbeatDispatcher(mockNotifier)

	// Register the queues.
	b.RegisterQueue(queue1)
	b.RegisterQueue(queue2)

	// Attach the blockbeat.
	b.beat = mockBeat

	// Mock the consumers to return nil error on ProcessBlock for
	// both calls.
	consumer1.On("ProcessBlock", mockBeat).Return(nil).Once()
	consumer2.On("ProcessBlock", mockBeat).Return(nil).Once()

	// Notify the queues. The mockers will be asserted in the end to
	// validate the calls.
	err := b.notifyQueues()
	require.NoError(t, err)
}

// TestNotifyQueuesError checks when one of the queue returns an error, this
// error is returned by the method.
func TestNotifyQueuesError(t *testing.T) {
	t.Parallel()

	// Create a mock consumer.
	consumer := &MockConsumer{}
	defer consumer.AssertExpectations(t)
	consumer.On("Name").Return("mocker1")

	// Create one queue.
	queue := []Consumer{consumer}

	// Create a mock chain notifier.
	mockNotifier := &chainntnfs.MockChainNotifier{}
	defer mockNotifier.AssertExpectations(t)

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Create a new dispatcher.
	b := NewBlockbeatDispatcher(mockNotifier)

	// Register the queues.
	b.RegisterQueue(queue)

	// Attach the blockbeat.
	b.beat = mockBeat

	// Mock the consumer to return an error on ProcessBlock.
	consumer.On("ProcessBlock", mockBeat).Return(errDummy).Once()

	// Notify the queues. The mockers will be asserted in the end to
	// validate the calls.
	err := b.notifyQueues()
	require.ErrorIs(t, err, errDummy)
}

// TestCurrentHeight asserts `CurrentHeight` returns the expected block height.
func TestCurrentHeight(t *testing.T) {
	t.Parallel()

	testHeight := int32(1000)

	// Create a mock chain notifier.
	mockNotifier := &chainntnfs.MockChainNotifier{}
	defer mockNotifier.AssertExpectations(t)

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)
	mockBeat.On("Height").Return(testHeight).Once()

	// Create a mock consumer.
	consumer := &MockConsumer{}
	defer consumer.AssertExpectations(t)
	consumer.On("Name").Return("mocker1")

	// Create one queue.
	queue := []Consumer{consumer}

	// Create a new dispatcher.
	b := NewBlockbeatDispatcher(mockNotifier)

	// Register the queues.
	b.RegisterQueue(queue)

	// Attach the blockbeat.
	b.beat = mockBeat

	// Mock the chain notifier to return a valid notifier.
	blockEpochs := &chainntnfs.BlockEpochEvent{
		Cancel: func() {},
	}
	mockNotifier.On("RegisterBlockEpochNtfn",
		mock.Anything).Return(blockEpochs, nil).Once()

	// Start the dispatcher now should not return an error.
	err := b.Start()
	require.NoError(t, err)

	// Make a query on the current height and assert it equals to
	// testHeight.
	height := b.CurrentHeight()
	require.Equal(t, testHeight, height)

	// Stop the dispatcher.
	b.Stop()

	// Make a query on the current height and assert it equals to 0.
	height = b.CurrentHeight()
	require.Zero(t, height)
}

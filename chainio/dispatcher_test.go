package chainio

import (
	"testing"
	"time"

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

	// Mock the ProcessBlock on consumers to reutrn immediately.
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

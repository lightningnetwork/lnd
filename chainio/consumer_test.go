package chainio

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestNewBeatConsumer tests the NewBeatConsumer function.
func TestNewBeatConsumer(t *testing.T) {
	t.Parallel()

	quitChan := make(chan struct{})
	name := "test"

	// Test the NewBeatConsumer function.
	b := NewBeatConsumer(quitChan, name)

	// Assert the state.
	require.Equal(t, quitChan, b.quit)
	require.Equal(t, name, b.name)
	require.NotNil(t, b.BlockbeatChan)
}

// TestProcessBlockSuccess tests when the block is processed successfully, no
// error is returned.
func TestProcessBlockSuccess(t *testing.T) {
	t.Parallel()

	// Create a test consumer.
	quitChan := make(chan struct{})
	b := NewBeatConsumer(quitChan, "test")

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Mock the consumer's err chan.
	consumerErrChan := make(chan error, 1)
	b.errChan = consumerErrChan

	// Call the method under test.
	resultChan := make(chan error, 1)
	go func() {
		resultChan <- b.ProcessBlock(mockBeat)
	}()

	// Assert the beat is sent to the blockbeat channel.
	beat, err := fn.RecvOrTimeout(b.BlockbeatChan, time.Second)
	require.NoError(t, err)
	require.Equal(t, mockBeat, beat)

	// Send nil to the consumer's error channel.
	consumerErrChan <- nil

	// Assert the result of ProcessBlock is nil.
	result, err := fn.RecvOrTimeout(resultChan, time.Second)
	require.NoError(t, err)
	require.Nil(t, result)
}

// TestProcessBlockConsumerQuitBeforeSend tests when the consumer is quit
// before sending the beat, the method returns immediately.
func TestProcessBlockConsumerQuitBeforeSend(t *testing.T) {
	t.Parallel()

	// Create a test consumer.
	quitChan := make(chan struct{})
	b := NewBeatConsumer(quitChan, "test")

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Call the method under test.
	resultChan := make(chan error, 1)
	go func() {
		resultChan <- b.ProcessBlock(mockBeat)
	}()

	// Instead of reading the BlockbeatChan, close the quit channel.
	close(quitChan)

	// Assert ProcessBlock returned nil.
	result, err := fn.RecvOrTimeout(resultChan, time.Second)
	require.NoError(t, err)
	require.Nil(t, result)
}

// TestProcessBlockConsumerQuitAfterSend tests when the consumer is quit after
// sending the beat, the method returns immediately.
func TestProcessBlockConsumerQuitAfterSend(t *testing.T) {
	t.Parallel()

	// Create a test consumer.
	quitChan := make(chan struct{})
	b := NewBeatConsumer(quitChan, "test")

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Mock the consumer's err chan.
	consumerErrChan := make(chan error, 1)
	b.errChan = consumerErrChan

	// Call the method under test.
	resultChan := make(chan error, 1)
	go func() {
		resultChan <- b.ProcessBlock(mockBeat)
	}()

	// Assert the beat is sent to the blockbeat channel.
	beat, err := fn.RecvOrTimeout(b.BlockbeatChan, time.Second)
	require.NoError(t, err)
	require.Equal(t, mockBeat, beat)

	// Instead of sending nil to the consumer's error channel, close the
	// quit channel.
	close(quitChan)

	// Assert ProcessBlock returned nil.
	result, err := fn.RecvOrTimeout(resultChan, time.Second)
	require.NoError(t, err)
	require.Nil(t, result)
}

// TestNotifyBlockProcessedSendErr asserts the error can be sent and read by
// the beat via NotifyBlockProcessed.
func TestNotifyBlockProcessedSendErr(t *testing.T) {
	t.Parallel()

	// Create a test consumer.
	quitChan := make(chan struct{})
	b := NewBeatConsumer(quitChan, "test")

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Mock the consumer's err chan.
	consumerErrChan := make(chan error, 1)
	b.errChan = consumerErrChan

	// Call the method under test.
	done := make(chan error)
	go func() {
		defer close(done)
		b.NotifyBlockProcessed(mockBeat, errDummy)
	}()

	// Assert the error is sent to the beat's err chan.
	result, err := fn.RecvOrTimeout(consumerErrChan, time.Second)
	require.NoError(t, err)
	require.ErrorIs(t, result, errDummy)

	// Assert the done channel is closed.
	result, err = fn.RecvOrTimeout(done, time.Second)
	require.NoError(t, err)
	require.Nil(t, result)
}

// TestNotifyBlockProcessedOnQuit asserts NotifyBlockProcessed exits
// immediately when the quit channel is closed.
func TestNotifyBlockProcessedOnQuit(t *testing.T) {
	t.Parallel()

	// Create a test consumer.
	quitChan := make(chan struct{})
	b := NewBeatConsumer(quitChan, "test")

	// Create a mock beat.
	mockBeat := &MockBlockbeat{}
	defer mockBeat.AssertExpectations(t)
	mockBeat.On("logger").Return(clog)

	// Mock the consumer's err chan - we don't buffer it so it will block
	// on sending the error.
	consumerErrChan := make(chan error)
	b.errChan = consumerErrChan

	// Call the method under test.
	done := make(chan error)
	go func() {
		defer close(done)
		b.NotifyBlockProcessed(mockBeat, errDummy)
	}()

	// Close the quit channel so the method will return.
	close(b.quit)

	// Assert the done channel is closed.
	result, err := fn.RecvOrTimeout(done, time.Second)
	require.NoError(t, err)
	require.Nil(t, result)
}

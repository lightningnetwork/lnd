package invoices

import (
	"fmt"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

var (
	defaultTimeout = 50 * time.Millisecond
)

// TestHtlcModificationInterceptor tests the basic functionality of the HTLC
// modification interceptor.
func TestHtlcModificationInterceptor(t *testing.T) {
	interceptor := NewHtlcModificationInterceptor()
	request := HtlcModifyRequest{
		WireCustomRecords: lnwire.CustomRecords{
			lnwire.MinCustomRecordsTlvType: []byte{1, 2, 3},
		},
		ExitHtlcCircuitKey: CircuitKey{
			ChanID: lnwire.NewShortChanIDFromInt(1),
			HtlcID: 1,
		},
		ExitHtlcAmt: 1234,
	}
	expectedResponse := HtlcModifyResponse{
		AmountPaid: 345,
	}
	interceptCallbackCalled := make(chan HtlcModifyRequest, 1)
	successInterceptCallback := func(
		req HtlcModifyRequest) (*HtlcModifyResponse, error) {

		interceptCallbackCalled <- req

		return &expectedResponse, nil
	}
	errorInterceptCallback := func(
		req HtlcModifyRequest) (*HtlcModifyResponse, error) {

		interceptCallbackCalled <- req

		return nil, fmt.Errorf("something went wrong")
	}
	responseCallbackCalled := make(chan HtlcModifyResponse, 1)
	responseCallback := func(resp HtlcModifyResponse) {
		responseCallbackCalled <- resp
	}

	// Create a session without setting a callback first.
	err := interceptor.Intercept(request, responseCallback)
	require.NoError(t, err)

	// Set the callback and create a new session.
	done, _, err := interceptor.RegisterInterceptor(
		successInterceptCallback,
	)
	require.NoError(t, err)

	err = interceptor.Intercept(request, responseCallback)
	require.NoError(t, err)

	// The intercept callback should be called now.
	select {
	case req := <-interceptCallbackCalled:
		require.Equal(t, request, req)

	case <-time.After(defaultTimeout):
		t.Fatal("intercept callback not called")
	}

	// And the result should make it back to the response callback.
	select {
	case resp := <-responseCallbackCalled:
		require.Equal(t, expectedResponse, resp)

	case <-time.After(defaultTimeout):
		t.Fatal("response callback not called")
	}

	// If we try to set a new callback without first returning the previous
	// one, we should get an error.
	_, _, err = interceptor.RegisterInterceptor(successInterceptCallback)
	require.ErrorIs(t, err, ErrInterceptorClientAlreadyConnected)

	// Reset the callback, then try to set a new one.
	done()
	done2, _, err := interceptor.RegisterInterceptor(errorInterceptCallback)
	require.NoError(t, err)
	defer done2()

	// We should now get an error when intercepting.
	err = interceptor.Intercept(request, responseCallback)
	require.ErrorContains(t, err, "something went wrong")

	// The success callback should not be called.
	select {
	case resp := <-responseCallbackCalled:
		t.Fatalf("unexpected response: %v", resp)

	case <-time.After(defaultTimeout):
		// Expected.
	}
}

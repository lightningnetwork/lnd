//go:build switchrpc
// +build switchrpc

package switchrpc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// TestSendOnion is a unit test that rigorously verifies the behavior of the
// SendOnion RPC handler in isolation. It ensures that the server correctly
// implements its side of the RPC contract by sending the correct signals back
// to the client under various conditions. The test guarantees:
//  1. Correct translation of internal htlcswitch errors (e.g., ErrDuplicateAdd)
//     into the specific error codes defined in the protobuf contract.
//  2. Robust validation of incoming requests, ensuring malformed requests are
//     rejected with the appropriate gRPC status code.
//  3. Correct propagation of success signals when the underlying dispatcher
//     succeeds.
func TestSendOnion(t *testing.T) {
	t.Parallel()

	// Create a valid request that can be used as a template for tests.
	makeValidRequest := func() *SendOnionRequest {
		return &SendOnionRequest{
			OnionBlob:      make([]byte, lnwire.OnionPacketSize),
			PaymentHash:    make([]byte, 32),
			AttemptId:      1,
			Amount:         1000,
			FirstHopChanId: 12345,
		}
	}

	//nolint:ll
	testCases := []struct {
		name string

		// setup is a function that modifies the server or request for a
		// specific test case.
		setup func(*testing.T, *Server, *SendOnionRequest)

		// expectedErrCode is the gRPC error code we expect from the
		// call.
		expectedErrCode codes.Code

		// expectedResponse is the expected response from the RPC call.
		expectedResponse *SendOnionResponse
	}{
		{
			name: "valid request",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				// Mock a successful dispatch.
				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = nil
			},
			expectedResponse: &SendOnionResponse{Success: true},
		},
		{
			name: "missing onion blob",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.OnionBlob = nil
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "invalid onion blob size",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.OnionBlob = make([]byte, 1)
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "missing payment hash",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.PaymentHash = nil
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "zero amount",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				req.Amount = 0
			},
			expectedErrCode: codes.InvalidArgument,
		},
		{
			name: "dispatcher internal error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				// Mock a generic error from the dispatcher.
				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = errors.New("internal error")
			},
			expectedResponse: &SendOnionResponse{
				Success:      false,
				ErrorMessage: "internal error",
				ErrorCode:    ErrorCode_INTERNAL,
			},
		},
		{
			// The ErrDuplicateAdd error is the means by which an
			// rpc client is safe to retry the SendOnion rpc until
			// an explicit acknowledgement of htlc dispatch can be
			// received from the server. The ability to retry and
			// rely on duplicate prevention is useful under
			// scenarios where the status of htlc dispatch is
			// uncertain (eg: network timeout or after restart).
			name: "dispatcher duplicate htlc error",
			setup: func(t *testing.T, s *Server,
				req *SendOnionRequest) {

				// Mock a duplicate error from the dispatcher,
				// which is the new way to signal a duplicate
				// attempt for the same ID.
				payer, ok := s.cfg.HtlcDispatcher.(*mockPayer)
				require.True(t, ok)
				payer.sendErr = htlcswitch.ErrDuplicateAdd
			},
			expectedResponse: &SendOnionResponse{
				Success:      false,
				ErrorMessage: htlcswitch.ErrDuplicateAdd.Error(),
				ErrorCode:    ErrorCode_DUPLICATE_HTLC,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Create a new server for each test case to ensure
			// isolation.
			server, _, err := New(&Config{
				HtlcDispatcher: &mockPayer{},
			})
			require.NoError(t, err)

			req := makeValidRequest()

			// Apply the test-specific setup.
			if tc.setup != nil {
				tc.setup(t, server, req)
			}

			resp, err := server.SendOnion(t.Context(), req)

			// Check for gRPC level errors.
			if tc.expectedErrCode != codes.OK {
				require.Error(t, err)
				s, ok := status.FromError(err)
				require.True(t, ok)
				require.Equal(t, tc.expectedErrCode, s.Code())

				return
			}

			// If no gRPC error was expected, check the response.
			require.NoError(t, err)
			require.Equal(t, tc.expectedResponse, resp)
		})
	}
}

// TestTranslateErrorForRPC tests the TranslateErrorForRPC function.
func TestTranslateErrorForRPC(t *testing.T) {
	t.Parallel()

	failureIndex := 1
	mockWireMsg := lnwire.NewTemporaryChannelFailure(nil)
	mockForwardingErr := htlcswitch.NewForwardingError(
		mockWireMsg, failureIndex,
	)
	mockClearTextErr := htlcswitch.NewLinkError(mockWireMsg)

	var buf bytes.Buffer
	err := lnwire.EncodeFailure(&buf, mockWireMsg, 0)
	require.NoError(t, err)

	encodedMsg := hex.EncodeToString(buf.Bytes())

	//nolint:ll
	tests := []struct {
		name         string
		err          error
		expectedMsg  string
		expectedCode ErrorCode
	}{
		{
			name:         "ErrPaymentIDNotFound",
			err:          htlcswitch.ErrPaymentIDNotFound,
			expectedMsg:  htlcswitch.ErrPaymentIDNotFound.Error(),
			expectedCode: ErrorCode_PAYMENT_ID_NOT_FOUND,
		},
		{
			name:         "ErrUnreadableFailureMessage",
			err:          htlcswitch.ErrUnreadableFailureMessage,
			expectedMsg:  htlcswitch.ErrUnreadableFailureMessage.Error(),
			expectedCode: ErrorCode_UNREADABLE_FAILURE_MESSAGE,
		},
		{
			name:         "ErrSwitchExiting",
			err:          htlcswitch.ErrSwitchExiting,
			expectedMsg:  htlcswitch.ErrSwitchExiting.Error(),
			expectedCode: ErrorCode_SWITCH_EXITING,
		},
		{
			name:         "ForwardingError",
			err:          mockForwardingErr,
			expectedMsg:  fmt.Sprintf("%d@%s", failureIndex, encodedMsg),
			expectedCode: ErrorCode_FORWARDING_ERROR,
		},
		{
			name:         "ClearTextError",
			err:          mockClearTextErr,
			expectedMsg:  encodedMsg,
			expectedCode: ErrorCode_CLEAR_TEXT_ERROR,
		},
		{
			name:         "Unknown Error",
			err:          errors.New("some unexpected error"),
			expectedMsg:  "some unexpected error",
			expectedCode: ErrorCode_INTERNAL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, code := translateErrorForRPC(tt.err)
			require.Contains(t, msg, tt.expectedMsg)
			require.Equal(t, tt.expectedCode, code)
		})
	}
}

// TestParseForwardingError tests the ParseForwardingError function.
func TestParseForwardingError(t *testing.T) {
	t.Parallel()

	mockWireMsg := lnwire.NewTemporaryChannelFailure(nil)

	var buf bytes.Buffer
	err := lnwire.EncodeFailure(&buf, mockWireMsg, 0)
	require.NoError(t, err)

	encodedMsg := hex.EncodeToString(buf.Bytes())

	tests := []struct {
		name         string
		errStr       string
		expectedIdx  int
		expectedWire lnwire.FailureMessage
		expectsError bool
	}{
		{
			name:         "Valid ForwardingError",
			errStr:       fmt.Sprintf("1@%s", encodedMsg),
			expectedIdx:  1,
			expectedWire: mockWireMsg,
			expectsError: false,
		},
		{
			name:         "Invalid Format",
			errStr:       "invalid_format",
			expectsError: true,
		},
		{
			name:         "Invalid Index",
			errStr:       "invalid@" + encodedMsg,
			expectsError: true,
		},
		{
			name:         "Invalid Wire Message",
			errStr:       "1@invalid",
			expectsError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fwdErr, err := ParseForwardingError(tt.errStr)

			if tt.expectsError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(
				t, tt.expectedIdx, fwdErr.FailureSourceIdx,
			)
			require.Equal(t, tt.expectedWire, fwdErr.WireMessage())
		})
	}
}

// TestForwardingErrorEncodeDecode tests the encoding and decoding of a
// forwarding error.
func TestForwardingErrorEncodeDecode(t *testing.T) {
	t.Parallel()

	mockWireMsg := lnwire.NewTemporaryChannelFailure(nil)
	mockForwardingErr := htlcswitch.NewForwardingError(mockWireMsg, 1)

	// Encode the forwarding error.
	encodedError, _ := translateErrorForRPC(mockForwardingErr)

	// Decode the forwarding error.
	decodedError, err := ParseForwardingError(encodedError)
	require.NoError(t, err, "decoding failed")

	// Assert the decoded error matches the original.
	require.Equal(t, mockForwardingErr.FailureSourceIdx,
		decodedError.FailureSourceIdx)
	require.Equal(t, mockForwardingErr.WireMessage(),
		decodedError.WireMessage())
}

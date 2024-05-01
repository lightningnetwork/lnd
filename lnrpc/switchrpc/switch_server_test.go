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
)

func TestTranslateErrorForRPC(t *testing.T) {
	failureIndex := 1
	mockWireMsg := lnwire.NewTemporaryChannelFailure(nil)
	mockForwardingErr := htlcswitch.NewForwardingError(mockWireMsg, failureIndex)
	mockClearTextErr := htlcswitch.NewLinkError(mockWireMsg)

	var buf bytes.Buffer
	lnwire.EncodeFailure(&buf, mockWireMsg, 0)
	encodedMsg := hex.EncodeToString(buf.Bytes())

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
			expectedCode: ErrorCode_ERROR_CODE_PAYMENT_ID_NOT_FOUND,
		},
		{
			name:         "ErrUnreadableFailureMessage",
			err:          htlcswitch.ErrUnreadableFailureMessage,
			expectedMsg:  htlcswitch.ErrUnreadableFailureMessage.Error(),
			expectedCode: ErrorCode_ERROR_CODE_UNREADABLE_FAILURE_MESSAGE,
		},
		{
			name:         "ErrSwitchExiting",
			err:          htlcswitch.ErrSwitchExiting,
			expectedMsg:  htlcswitch.ErrSwitchExiting.Error(),
			expectedCode: ErrorCode_ERROR_CODE_SWITCH_EXITING,
		},
		{
			name:         "ForwardingError",
			err:          mockForwardingErr,
			expectedMsg:  fmt.Sprintf("%d@%s", failureIndex, encodedMsg),
			expectedCode: ErrorCode_ERROR_CODE_FORWARDING_ERROR,
		},
		{
			name:         "ClearTextError",
			err:          mockClearTextErr,
			expectedMsg:  encodedMsg,
			expectedCode: ErrorCode_ERROR_CODE_CLEAR_TEXT_ERROR,
		},
		{
			name:         "Unknown Error",
			err:          errors.New("some unexpected error"),
			expectedMsg:  "some unexpected error",
			expectedCode: ErrorCode_ERROR_CODE_INTERNAL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, code := TranslateErrorForRPC(tt.err)
			require.Contains(t, msg, tt.expectedMsg)
			require.Equal(t, tt.expectedCode, code)
		})
	}
}

func TestParseForwardingError(t *testing.T) {
	mockWireMsg := lnwire.NewTemporaryChannelFailure(nil)

	var buf bytes.Buffer
	lnwire.EncodeFailure(&buf, mockWireMsg, 0)
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
			require.Equal(t, tt.expectedIdx, fwdErr.FailureSourceIdx)
			require.Equal(t, tt.expectedWire, fwdErr.WireMessage())
		})
	}
}

func TestForwardingErrorEncodeDecode(t *testing.T) {
	mockWireMsg := lnwire.NewTemporaryChannelFailure(nil)
	mockForwardingErr := htlcswitch.NewForwardingError(mockWireMsg, 1)

	// Encode the forwarding error.
	encodedError, _ := TranslateErrorForRPC(mockForwardingErr)

	// Decode the forwarding error.
	decodedError, err := ParseForwardingError(encodedError)
	require.NoError(t, err, "decoding failed")

	// Assert the decoded error matches the original.
	require.Equal(t, mockForwardingErr.FailureSourceIdx, decodedError.FailureSourceIdx)
	require.Equal(t, mockForwardingErr.WireMessage(), decodedError.WireMessage())
}

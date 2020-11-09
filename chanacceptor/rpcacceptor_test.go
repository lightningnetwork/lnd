package chanacceptor

import (
	"errors"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// TestValidateAcceptorResponse test validation of acceptor responses.
func TestValidateAcceptorResponse(t *testing.T) {
	customError := errors.New("custom error")

	tests := []struct {
		name        string
		response    lnrpc.ChannelAcceptResponse
		accept      bool
		acceptorErr error
		error       error
	}{
		{
			name: "accepted with error",
			response: lnrpc.ChannelAcceptResponse{
				Accept: true,
				Error:  customError.Error(),
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       errAcceptWithError,
		},
		{
			name: "custom error too long",
			response: lnrpc.ChannelAcceptResponse{
				Accept: false,
				Error:  strings.Repeat(" ", maxErrorLength+1),
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       errCustomLength,
		},
		{
			name: "accepted",
			response: lnrpc.ChannelAcceptResponse{
				Accept: true,
			},
			accept:      true,
			acceptorErr: nil,
			error:       nil,
		},
		{
			name: "rejected with error",
			response: lnrpc.ChannelAcceptResponse{
				Accept: false,
				Error:  customError.Error(),
			},
			accept:      false,
			acceptorErr: customError,
			error:       nil,
		},
		{
			name: "rejected with no error",
			response: lnrpc.ChannelAcceptResponse{
				Accept: false,
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			accept, acceptErr, err := validateAcceptorResponse(
				test.response,
			)
			require.Equal(t, test.accept, accept)
			require.Equal(t, test.acceptorErr, acceptErr)
			require.Equal(t, test.error, err)
		})
	}
}

package chanacceptor

import (
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestValidateAcceptorResponse test validation of acceptor responses.
func TestValidateAcceptorResponse(t *testing.T) {
	var (
		customError = errors.New("custom error")
		validAddr   = "bcrt1qwrmq9uca0t3dy9t9wtuq5tm4405r7tfzyqn9pp"
		addr, _     = chancloser.ParseUpfrontShutdownAddress(
			validAddr, &chaincfg.RegressionNetParams,
		)
	)

	tests := []struct {
		name        string
		dustLimit   btcutil.Amount
		response    *lnrpc.ChannelAcceptResponse
		accept      bool
		acceptorErr error
		error       error
		shutdown    lnwire.DeliveryAddress
	}{
		{
			name: "accepted with error",
			response: &lnrpc.ChannelAcceptResponse{
				Accept: true,
				Error:  customError.Error(),
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       errAcceptWithError,
		},
		{
			name: "custom error too long",
			response: &lnrpc.ChannelAcceptResponse{
				Accept: false,
				Error:  strings.Repeat(" ", maxErrorLength+1),
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       errCustomLength,
		},
		{
			name: "accepted",
			response: &lnrpc.ChannelAcceptResponse{
				Accept:          true,
				UpfrontShutdown: validAddr,
			},
			accept:      true,
			acceptorErr: nil,
			error:       nil,
			shutdown:    addr,
		},
		{
			name: "rejected with error",
			response: &lnrpc.ChannelAcceptResponse{
				Accept: false,
				Error:  customError.Error(),
			},
			accept:      false,
			acceptorErr: customError,
			error:       nil,
		},
		{
			name: "rejected with no error",
			response: &lnrpc.ChannelAcceptResponse{
				Accept: false,
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       nil,
		},
		{
			name: "invalid upfront shutdown",
			response: &lnrpc.ChannelAcceptResponse{
				Accept:          true,
				UpfrontShutdown: "invalid addr",
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       errInvalidUpfrontShutdown,
		},
		{
			name:      "reserve too low",
			dustLimit: 100,
			response: &lnrpc.ChannelAcceptResponse{
				Accept:     true,
				ReserveSat: 10,
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       errInsufficientReserve,
		},
		{
			name:      "max htlcs too high",
			dustLimit: 100,
			response: &lnrpc.ChannelAcceptResponse{
				Accept:       true,
				MaxHtlcCount: 1 + input.MaxHTLCNumber/2,
			},
			accept:      false,
			acceptorErr: errChannelRejected,
			error:       errMaxHtlcTooHigh,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			// Create an acceptor, everything can be nil because
			// we just need the params.
			acceptor := NewRPCAcceptor(
				nil, nil, 0, &chaincfg.RegressionNetParams, nil,
			)

			accept, acceptErr, shutdown, err := acceptor.validateAcceptorResponse(
				test.dustLimit, test.response,
			)
			require.Equal(t, test.accept, accept)
			require.Equal(t, test.acceptorErr, acceptErr)
			require.Equal(t, test.error, err)
			require.Equal(t, test.shutdown, shutdown)
		})
	}
}

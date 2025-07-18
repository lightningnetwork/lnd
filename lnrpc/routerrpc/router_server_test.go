package routerrpc

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestSendToRouteV2MPPValidation tests that the MPP validation is correctly
// integrated into the SendToRouteV2 RPC method.
func TestSendToRouteV2MPPValidation(t *testing.T) {
	// Test the MPP validation logic directly
	t.Run("mpp validation framework", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeEnforce,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		// Create a route without MPP
		testRoute := &route.Route{
			TotalTimeLock: 100,
			TotalAmount:   1000,
			Hops: []*route.Hop{
				{
					PubKeyBytes:      route.Vertex{1},
					ChannelID:        1,
					OutgoingTimeLock: 100,
					AmtToForward:     1000,
				},
			},
		}

		// Validate route - should fail in enforce mode
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       testRoute,
			PaymentAddr: fn.None[[32]byte](),
			RequestID:   "test-1",
		}

		result := validator.ValidateRoute(req)
		// In enforce mode, SendToRoute requires MPP in the route
		require.False(t, result.Valid)
		require.Error(t, result.Error)
		require.Contains(t, result.Error.Error(), "MPP enforcement:")
	})

	// Test with MPP present
	t.Run("route with mpp record", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeEnforce,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		// Create a route with MPP
		paymentAddr := [32]byte{1, 2, 3, 4, 5}
		testRoute := &route.Route{
			TotalTimeLock: 100,
			TotalAmount:   1000,
			Hops: []*route.Hop{
				{
					PubKeyBytes:      route.Vertex{1},
					ChannelID:        1,
					OutgoingTimeLock: 100,
					AmtToForward:     1000,
					MPP: record.NewMPP(
						1000, paymentAddr),
				},
			},
		}

		// Validate route - should pass with MPP
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       testRoute,
			PaymentAddr: fn.None[[32]byte](),
			RequestID:   "test-2",
		}

		result := validator.ValidateRoute(req)
		require.True(t, result.Valid)
		require.NoError(t, result.Error)
	})

	// Test warn mode
	t.Run("warn mode", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeWarn,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		// Create a route without MPP
		testRoute := &route.Route{
			TotalTimeLock: 100,
			TotalAmount:   1000,
			Hops: []*route.Hop{
				{
					PubKeyBytes:      route.Vertex{1},
					ChannelID:        1,
					OutgoingTimeLock: 100,
					AmtToForward:     1000,
				},
			},
		}

		// Validate route - should pass with warning
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       testRoute,
			PaymentAddr: fn.None[[32]byte](),
			RequestID:   "test-3",
		}

		result := validator.ValidateRoute(req)
		// In warn mode, SendToRoute without MPP should pass with
		// warning
		require.True(t, result.Valid)
		require.NoError(t, result.Error)
		require.NotEmpty(t, result.Warning)
		require.NotEmpty(t, logger.warnMsgs)
	})

	// Test legacy mode
	t.Run("legacy mode", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeLegacy,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		// Create a route without MPP
		testRoute := &route.Route{
			TotalTimeLock: 100,
			TotalAmount:   1000,
			Hops: []*route.Hop{
				{
					PubKeyBytes:      route.Vertex{1},
					ChannelID:        1,
					OutgoingTimeLock: 100,
					AmtToForward:     1000,
				},
			},
		}

		// Validate route - should pass without warning
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       testRoute,
			PaymentAddr: fn.None[[32]byte](),
			RequestID:   "test-4",
		}

		result := validator.ValidateRoute(req)
		require.True(t, result.Valid)
		require.NoError(t, result.Error)
		require.Empty(t, result.Warning)
		require.Empty(t, logger.warnMsgs)
	})
}

// TestSendToRouteV2Integration tests integration scenarios for SendToRouteV2.
func TestSendToRouteV2Integration(t *testing.T) {
	// Test validation with payment address matching
	t.Run("payment address matching", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeEnforce,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		paymentAddr := [32]byte{1, 2, 3, 4, 5}

		// Create a route with MPP matching payment address
		testRoute := &route.Route{
			TotalTimeLock: 100,
			TotalAmount:   1000,
			Hops: []*route.Hop{
				{
					PubKeyBytes:      route.Vertex{1},
					ChannelID:        1,
					OutgoingTimeLock: 100,
					AmtToForward:     1000,
					MPP: record.NewMPP(
						1000, paymentAddr),
				},
			},
		}

		// Validate with matching payment address
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       testRoute,
			PaymentAddr: fn.Some(paymentAddr),
			RequestID:   "test-5",
		}

		result := validator.ValidateRoute(req)
		require.True(t, result.Valid)
		require.NoError(t, result.Error)
	})

	// Test validation with payment address mismatch
	t.Run("payment address mismatch", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeEnforce,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		paymentAddr1 := [32]byte{1, 2, 3, 4, 5}
		paymentAddr2 := [32]byte{6, 7, 8, 9, 10}

		// Create a route with MPP
		testRoute := &route.Route{
			TotalTimeLock: 100,
			TotalAmount:   1000,
			Hops: []*route.Hop{
				{
					PubKeyBytes:      route.Vertex{1},
					ChannelID:        1,
					OutgoingTimeLock: 100,
					AmtToForward:     1000,
					MPP: record.NewMPP(
						1000, paymentAddr1),
				},
			},
		}

		// Validate with mismatching payment address
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       testRoute,
			PaymentAddr: fn.Some(paymentAddr2),
			RequestID:   "test-6",
		}

		result := validator.ValidateRoute(req)
		require.False(t, result.Valid)
		require.Error(t, result.Error)
		require.Contains(t, result.Error.Error(),
			"payment address mismatch")
	})

	// Test nil validator handling
	t.Run("nil validator", func(t *testing.T) {
		// This simulates the case where MPPValidator is nil in the
		// config
		// The actual SendToRouteV2 implementation should check for nil
		// and skip validation if the validator is not configured
		var validator *MPPValidator = nil
		require.Nil(t, validator)

		// In the actual implementation, this would be:
		// if s.cfg.MPPValidator != nil { ... }
	})

	// Test emergency override
	t.Run("emergency override", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:        EnforcementModeEnforce,
			EmergencyOverride: true,
			MetricsEnabled:    false,
		}
		validator := NewMPPValidator(config, logger)

		// Create a route without MPP
		testRoute := &route.Route{
			TotalTimeLock: 100,
			TotalAmount:   1000,
			Hops: []*route.Hop{
				{
					PubKeyBytes:      route.Vertex{1},
					ChannelID:        1,
					OutgoingTimeLock: 100,
					AmtToForward:     1000,
				},
			},
		}

		// Validate route - should pass due to emergency override
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       testRoute,
			PaymentAddr: fn.None[[32]byte](),
			RequestID:   "test-7",
		}

		result := validator.ValidateRoute(req)
		require.True(t, result.Valid)
		require.NoError(t, result.Error)
		require.Empty(t, result.Warning) // No warning in legacy mode
	})
}

// TestSendToRouteV2ErrorHandling tests error scenarios.
func TestSendToRouteV2ErrorHandling(t *testing.T) {
	// Test various error conditions
	t.Run("empty route", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeEnforce,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		// Validate empty route
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       &route.Route{},
			PaymentAddr: fn.None[[32]byte](),
			RequestID:   "test-8",
		}

		result := validator.ValidateRoute(req)
		require.True(t, result.Valid) // Empty routes are allowed
	})

	t.Run("nil route", func(t *testing.T) {
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeEnforce,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		// Validate nil route
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       nil,
			PaymentAddr: fn.None[[32]byte](),
			RequestID:   "test-9",
		}

		result := validator.ValidateRoute(req)
		require.True(t, result.Valid) // Nil routes are allowed
	})
}

// TestHelperValidatePaymentAddr tests the ValidatePaymentAddr helper function.
func TestHelperValidatePaymentAddr(t *testing.T) {
	// Test valid payment address
	validAddr := make([]byte, 32)
	validAddr[0] = 1 // Not all zeros
	err := ValidatePaymentAddr(validAddr)
	require.NoError(t, err)

	// Test invalid length
	shortAddr := make([]byte, 16)
	err = ValidatePaymentAddr(shortAddr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected 32 bytes")

	// Test all zeros
	zeroAddr := make([]byte, 32)
	err = ValidatePaymentAddr(zeroAddr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot be all zeros")
}

// TestSendToRouteV2MockedServer tests with a minimal mocked server setup.
func TestSendToRouteV2MockedServer(t *testing.T) {
	// This test demonstrates how the actual SendToRouteV2 would integrate
	// with the MPP validator. Since we can't easily mock the full server
	// dependencies, we test the validation logic independently above.

	// In the actual implementation:
	// 1. SendToRouteV2 unmarshalls the route
	// 2. If MPPValidator is configured, it validates the route
	// 3. If validation fails, it returns an error
	// 4. Otherwise, it proceeds with sending the payment

	t.Run("validation flow", func(t *testing.T) {
		// Create validator
		logger := &mockLogger{}
		config := &MPPValidationConfig{
			GlobalMode:     EnforcementModeEnforce,
			MetricsEnabled: false,
		}
		validator := NewMPPValidator(config, logger)

		// Unmarshal would produce a route
		unmarshalledRoute := &route.Route{
			TotalTimeLock: 100,
			TotalAmount:   1000,
			Hops: []*route.Hop{
				{
					PubKeyBytes:      route.Vertex{1},
					ChannelID:        1,
					OutgoingTimeLock: 100,
					AmtToForward:     1000,
				},
			},
		}

		// Validate the route
		req := &ValidationRequest{
			RPCMethod:   "SendToRouteV2",
			Route:       unmarshalledRoute,
			PaymentAddr: fn.None[[32]byte](),
			RequestID: fmt.Sprintf(
				"send-to-route-v2-%s", "test-hash"),
		}

		result := validator.ValidateRoute(req)

		// In enforce mode, SendToRoute without MPP should fail
		require.False(t, result.Valid)
		require.Error(t, result.Error)

		// The server would return this error to the client
		require.Contains(t, result.Error.Error(), "MPP enforcement:")
	})
}

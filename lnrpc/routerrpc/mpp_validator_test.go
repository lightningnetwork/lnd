package routerrpc

import (
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// mockLogger implements the Logger interface for testing.
type mockLogger struct {
	debugMsgs []string
	infoMsgs  []string
	warnMsgs  []string
	errorMsgs []string
}

func (m *mockLogger) Debugf(format string, params ...interface{}) {
	m.debugMsgs = append(m.debugMsgs, format)
}

func (m *mockLogger) Infof(format string, params ...interface{}) {
	m.infoMsgs = append(m.infoMsgs, format)
}

func (m *mockLogger) Warnf(format string, params ...interface{}) {
	m.warnMsgs = append(m.warnMsgs, format)
}

func (m *mockLogger) Errorf(format string, params ...interface{}) {
	m.errorMsgs = append(m.errorMsgs, format)
}

// Helper functions for creating test data.
func createTestRoute(withMPP bool) *route.Route {
	testRoute := &route.Route{
		TotalTimeLock: 100,
		TotalAmount:   1000,
		SourcePubKey:  route.Vertex{1, 2, 3},
		Hops: []*route.Hop{
			{
				PubKeyBytes:      route.Vertex{4, 5, 6},
				ChannelID:        12345,
				OutgoingTimeLock: 100,
				AmtToForward:     1000,
			},
		},
	}

	if withMPP {
		paymentAddr := createTestPaymentAddr()
		testRoute.Hops[0].MPP = record.NewMPP(1000, paymentAddr)
	}

	return testRoute
}

func createTestPaymentAddr() [32]byte {
	return [32]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
	}
}

func TestEnforcementModeString(t *testing.T) {
	tests := []struct {
		mode     EnforcementMode
		expected string
	}{
		{EnforcementModeLegacy, "legacy"},
		{EnforcementModeWarn, "warn"},
		{EnforcementModeEnforce, "enforce"},
		{EnforcementMode(999), "unknown"},
	}

	for _, test := range tests {
		t.Run(test.expected, func(t *testing.T) {
			result := test.mode.String()
			require.Equal(t, test.expected, result)
		})
	}
}

func TestParseEnforcementMode(t *testing.T) {
	tests := []struct {
		input       string
		expected    EnforcementMode
		expectError bool
	}{
		{"legacy", EnforcementModeLegacy, false},
		{"warn", EnforcementModeWarn, false},
		{"enforce", EnforcementModeEnforce, false},
		{"invalid", EnforcementModeLegacy, true},
		{"", EnforcementModeLegacy, true},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			result, err := ParseEnforcementMode(test.input)

			if test.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.expected, result)
			}
		})
	}
}

func TestValidatePaymentAddr(t *testing.T) {
	tests := []struct {
		name        string
		paymentAddr []byte
		expectError bool
	}{
		{
			name: "valid payment address",
			paymentAddr: func() []byte {
				addr := createTestPaymentAddr()
				return addr[:]
			}(),
			expectError: false,
		},
		{
			name:        "too short",
			paymentAddr: []byte{1, 2, 3},
			expectError: true,
		},
		{
			name:        "too long",
			paymentAddr: make([]byte, 40),
			expectError: true,
		},
		{
			name:        "all zeros",
			paymentAddr: make([]byte, 32),
			expectError: true,
		},
		{
			name:        "empty",
			paymentAddr: []byte{},
			expectError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePaymentAddr(test.paymentAddr)

			if test.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMPPValidatorLegacyMode(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeLegacy,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Test with route that has no MPP
	route := createTestRoute(false)
	paymentAddr := createTestPaymentAddr()

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-1",
	}

	result := validator.ValidateRoute(req)

	require.True(t, result.Valid)
	require.NoError(t, result.Error)
	require.Empty(t, result.Warning)
	require.Equal(t, EnforcementModeLegacy, result.Mode)
}

func TestMPPValidatorWarnMode(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeWarn,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Test with route that has no MPP but payment_addr is provided
	route := createTestRoute(false)
	paymentAddr := createTestPaymentAddr()

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-warn",
	}

	result := validator.ValidateRoute(req)

	require.True(t, result.Valid, "Should be valid in warn mode")
	require.NoError(t, result.Error)
	require.NotEmpty(t, result.Warning, "Should have warning message")
	require.Equal(t, EnforcementModeWarn, result.Mode)
	require.Len(t, logger.warnMsgs, 1, "Should have logged one warning")
}

func TestMPPValidatorEnforceMode(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeEnforce,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Test with route that has no MPP but payment_addr is provided
	route := createTestRoute(false)
	paymentAddr := createTestPaymentAddr()

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-enforce",
	}

	result := validator.ValidateRoute(req)

	require.False(t, result.Valid, "Should be invalid in enforce mode")
	require.Error(t, result.Error)
	require.Empty(t, result.Warning)
	require.Equal(t, EnforcementModeEnforce, result.Mode)
	require.Len(t, logger.errorMsgs, 1, "Should have logged one error")
}

func TestMPPValidatorValidRoute(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeEnforce,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Test with route that has matching MPP
	route := createTestRoute(true)
	paymentAddr := createTestPaymentAddr()

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-valid",
	}

	result := validator.ValidateRoute(req)

	require.True(t, result.Valid)
	require.NoError(t, result.Error)
	require.Empty(t, result.Warning)
	require.Equal(t, EnforcementModeEnforce, result.Mode)
}

func TestMPPValidatorPaymentAddrMismatch(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeEnforce,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Create route with different payment address
	route := createTestRoute(true)
	// Different from the one in route
	differentPaymentAddr := [32]byte{99, 98, 97}

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(differentPaymentAddr),
		RequestID:   "test-mismatch",
	}

	result := validator.ValidateRoute(req)

	require.False(t, result.Valid)
	require.Error(t, result.Error)
	require.ErrorIs(t, result.Error, ErrPaymentAddrMismatch)
	require.Equal(t, EnforcementModeEnforce, result.Mode)
}

func TestMPPValidatorPerRPCModes(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode: EnforcementModeWarn,
		PerRPCModes: map[string]EnforcementMode{
			"BuildRoute":  EnforcementModeEnforce,
			"QueryRoutes": EnforcementModeLegacy,
		},
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Test BuildRoute uses enforce mode (override)
	route := createTestRoute(false)
	paymentAddr := createTestPaymentAddr()

	buildReq := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-build",
	}

	result := validator.ValidateRoute(buildReq)
	require.False(t, result.Valid, "BuildRoute should enforce")
	require.Equal(t, EnforcementModeEnforce, result.Mode)

	// Test QueryRoutes uses legacy mode (override)
	queryReq := &ValidationRequest{
		RPCMethod:   "QueryRoutes",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-query",
	}

	result = validator.ValidateRoute(queryReq)
	require.True(t, result.Valid, "QueryRoutes should use legacy")
	require.Equal(t, EnforcementModeLegacy, result.Mode)

	// Test SendToRoute uses global mode (warn)
	sendReq := &ValidationRequest{
		RPCMethod:   "SendToRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-send",
	}

	result = validator.ValidateRoute(sendReq)
	require.True(t, result.Valid, "SendToRoute should warn")
	require.NotEmpty(t, result.Warning)
	require.Equal(t, EnforcementModeWarn, result.Mode)
}

func TestMPPValidatorRuntimeOverrides(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeWarn,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Set runtime override for BuildRoute
	err := validator.SetRuntimeOverride(
		"BuildRoute", EnforcementModeEnforce)
	require.NoError(t, err)

	// Test that BuildRoute now uses enforce mode
	route := createTestRoute(false)
	paymentAddr := createTestPaymentAddr()

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-runtime",
	}

	result := validator.ValidateRoute(req)
	require.False(t, result.Valid)
	require.Equal(t, EnforcementModeEnforce, result.Mode)

	// Test that other RPCs still use global mode
	req.RPCMethod = "QueryRoutes"
	result = validator.ValidateRoute(req)
	require.True(t, result.Valid)
	require.Equal(t, EnforcementModeWarn, result.Mode)
}

func TestMPPValidatorEmergencyOverride(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:        EnforcementModeEnforce,
		EmergencyOverride: false,
		MetricsEnabled:    false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	route := createTestRoute(false)
	paymentAddr := createTestPaymentAddr()

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-emergency",
	}

	// First test normal enforce mode
	result := validator.ValidateRoute(req)
	require.False(t, result.Valid,
		"Should reject without emergency override")

	// Enable emergency override
	validator.SetEmergencyOverride(true)

	// Now should pass
	result = validator.ValidateRoute(req)
	require.True(t, result.Valid,
		"Should pass with emergency override")
	require.Equal(t, EnforcementModeLegacy, result.Mode)

	// Disable emergency override
	validator.SetEmergencyOverride(false)

	// Should reject again
	result = validator.ValidateRoute(req)
	require.False(t, result.Valid,
		"Should reject after disabling emergency override")
}

func TestMPPValidatorDowngradeRestriction(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:        EnforcementModeEnforce,
		EmergencyOverride: false,
		MetricsEnabled:    false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Try to downgrade from enforce to warn (should fail)
	err := validator.SetRuntimeOverride("BuildRoute", EnforcementModeWarn)
	require.Error(t, err,
		"Should not allow downgrade without emergency override")

	// Enable emergency override and try again
	validator.SetEmergencyOverride(true)
	err = validator.SetRuntimeOverride("BuildRoute", EnforcementModeWarn)
	require.NoError(t, err,
		"Should allow downgrade with emergency override")
}

func TestMPPValidatorGetEnforcementStatus(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode: EnforcementModeWarn,
		PerRPCModes: map[string]EnforcementMode{
			"BuildRoute": EnforcementModeEnforce,
		},
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Add runtime override (emergency override needed to downgrade from
	// warn to legacy)
	validator.SetEmergencyOverride(true)
	err := validator.SetRuntimeOverride(
		"QueryRoutes", EnforcementModeLegacy)
	require.NoError(t, err)
	validator.SetEmergencyOverride(false)

	status := validator.GetEnforcementStatus()

	require.Equal(t, EnforcementModeEnforce, status["BuildRoute"],
		"BuildRoute should use per-RPC config")
	require.Equal(t, EnforcementModeLegacy, status["QueryRoutes"],
		"QueryRoutes should use runtime override")
	require.Equal(t, EnforcementModeWarn, status["SendToRoute"],
		"SendToRoute should use global config")
}

func TestMPPValidatorNoRouteOrHops(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeEnforce,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	paymentAddr := createTestPaymentAddr()

	// Test with nil route
	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       nil,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "test-nil-route",
	}

	result := validator.ValidateRoute(req)
	require.True(t, result.Valid, "Nil route should pass validation")

	// Test with route having no hops
	emptyRoute := &route.Route{
		TotalTimeLock: 100,
		TotalAmount:   1000,
		Hops:          []*route.Hop{},
	}

	req.Route = emptyRoute
	result = validator.ValidateRoute(req)
	require.True(t, result.Valid, "Empty route should pass validation")
}

func TestMPPValidatorNoPaymentAddrRequired(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeEnforce,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	// Test with route that has no MPP and no payment_addr requirement
	route := createTestRoute(false)

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.None[[32]byte](), // No payment address provided
		RequestID:   "test-no-addr",
	}

	result := validator.ValidateRoute(req)
	require.True(t, result.Valid,
		"Should pass when neither MPP nor payment_addr present")
}

func TestMPPValidatorFormatEnforcementError(t *testing.T) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeEnforce,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	tests := []struct {
		rpcMethod    string
		expectedText string
	}{
		{"BuildRoute", "BuildRouteRequest"},
		{"QueryRoutes", "QueryRoutesRequest"},
		{"SendToRoute", "MPP record"},
		{"UnknownRPC", "payment_addr required for UnknownRPC"},
	}

	for _, test := range tests {
		t.Run(test.rpcMethod, func(t *testing.T) {
			err := validator.formatEnforcementError(test.rpcMethod)
			require.Error(t, err)
			require.Contains(t, err.Error(), test.expectedText)
		})
	}
}

func TestCreateMPPRecord(t *testing.T) {
	paymentAddr := createTestPaymentAddr()
	totalMsat := lnwire.MilliSatoshi(50000)

	record := CreateMPPRecord(paymentAddr, totalMsat)

	require.NotNil(t, record)
	require.Equal(t, paymentAddr, record.PaymentAddr())
	require.Equal(t, totalMsat, record.TotalMsat())
}

func TestDefaultMPPValidationConfig(t *testing.T) {
	config := DefaultMPPValidationConfig()

	require.Equal(t, EnforcementModeWarn, config.GlobalMode)
	require.NotNil(t, config.PerRPCModes)
	require.True(t, config.MetricsEnabled)
	require.False(t, config.EmergencyOverride)
	require.Equal(t, 90, config.GracePeriodDays)
}

// Benchmark tests for performance validation.
func BenchmarkMPPValidatorLegacyMode(b *testing.B) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeLegacy,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	route := createTestRoute(false)
	paymentAddr := createTestPaymentAddr()

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "bench",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		validator.ValidateRoute(req)
	}
}

func BenchmarkMPPValidatorEnforceMode(b *testing.B) {
	config := &MPPValidationConfig{
		GlobalMode:     EnforcementModeEnforce,
		MetricsEnabled: false,
	}

	logger := &mockLogger{}
	validator := NewMPPValidator(config, logger)

	route := createTestRoute(true) // Valid route with MPP
	paymentAddr := createTestPaymentAddr()

	req := &ValidationRequest{
		RPCMethod:   "BuildRoute",
		Route:       route,
		PaymentAddr: fn.Some(paymentAddr),
		RequestID:   "bench",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		validator.ValidateRoute(req)
	}
}

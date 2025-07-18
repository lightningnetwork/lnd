package routerrpc

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// EnforcementMode defines how MPP validation should be handled.
type EnforcementMode int

const (
	// EnforcementModeLegacy maintains current behavior (no validation).
	EnforcementModeLegacy EnforcementMode = iota

	// EnforcementModeWarn logs warnings but allows payments.
	EnforcementModeWarn

	// EnforcementModeEnforce rejects payments without MPP.
	EnforcementModeEnforce
)

// String returns a string representation of the enforcement mode.
func (e EnforcementMode) String() string {
	switch e {
	case EnforcementModeLegacy:
		return "legacy"
	case EnforcementModeWarn:
		return "warn"
	case EnforcementModeEnforce:
		return "enforce"
	default:
		return "unknown"
	}
}

// ParseEnforcementMode parses a string into an EnforcementMode.
func ParseEnforcementMode(s string) (EnforcementMode, error) {
	switch s {
	case "legacy":
		return EnforcementModeLegacy, nil
	case "warn":
		return EnforcementModeWarn, nil
	case "enforce":
		return EnforcementModeEnforce, nil
	default:
		return EnforcementModeLegacy, fmt.Errorf(
			"invalid enforcement mode: %s", s)
	}
}

// MPPValidationConfig contains configuration for MPP validation.
type MPPValidationConfig struct {
	// GlobalMode is the default enforcement mode.
	GlobalMode EnforcementMode

	// PerRPCModes allows overriding enforcement mode per RPC method.
	PerRPCModes map[string]EnforcementMode

	// EmergencyOverride forces legacy mode when enabled.
	EmergencyOverride bool

	// MetricsEnabled controls whether metrics are collected.
	MetricsEnabled bool

	// GracePeriodDays allows a grace period before enforcement.
	GracePeriodDays int

	// AutoUpgradeDate automatically upgrades mode after this date.
	AutoUpgradeDate *time.Time
}

// DefaultMPPValidationConfig returns the default MPP validation configuration.
func DefaultMPPValidationConfig() *MPPValidationConfig {
	return &MPPValidationConfig{
		GlobalMode:        EnforcementModeWarn,
		PerRPCModes:       make(map[string]EnforcementMode),
		EmergencyOverride: false,
		MetricsEnabled:    true,
		GracePeriodDays:   90,
	}
}

// MPPMetrics contains metrics for MPP validation.
type MPPMetrics struct {
	validationAttempts    prometheus.Counter
	validationFailures    prometheus.Counter
	validationDuration    prometheus.Histogram
	enforcementRejections prometheus.Counter
	warningsIssued        prometheus.Counter
	modeChanges           prometheus.Counter
}

// NewMPPMetrics creates a new MPPMetrics instance.
func NewMPPMetrics() *MPPMetrics {
	return &MPPMetrics{
		validationAttempts: promauto.NewCounter(prometheus.CounterOpts{
			Name: "mpp_validation_attempts_total",
			Help: "Total number of MPP validation attempts",
		}),
		validationFailures: promauto.NewCounter(prometheus.CounterOpts{
			Name: "mpp_validation_failures_total",
			Help: "Total number of MPP validation failures",
		}),
		validationDuration: promauto.NewHistogram(
			prometheus.HistogramOpts{
				Name: "mpp_validation_duration_seconds",
				Help: "Duration of MPP validation operations",
				Buckets: prometheus.ExponentialBuckets(
					0.0001, 2, 10),
			}),
		enforcementRejections: promauto.NewCounter(
			prometheus.CounterOpts{
				Name: "mpp_enforcement_rejections_total",
				Help: "Total number of payments rejected " +
					"due to MPP enforcement",
			}),
		warningsIssued: promauto.NewCounter(prometheus.CounterOpts{
			Name: "mpp_warnings_issued_total",
			Help: "Total number of MPP warnings issued",
		}),
		modeChanges: promauto.NewCounter(prometheus.CounterOpts{
			Name: "mpp_mode_changes_total",
			Help: "Total number of MPP enforcement mode changes",
		}),
	}
}

// MPPValidator handles validation of MPP records across RPC methods.
type MPPValidator struct {
	config  *MPPValidationConfig
	metrics *MPPMetrics
	logger  Logger

	// mu protects runtime overrides and mode changes.
	mu sync.RWMutex

	// runtimeOverrides allows dynamic mode changes per RPC.
	runtimeOverrides map[string]EnforcementMode

	// startTime tracks when validation started for grace period
	// calculations.
	startTime time.Time
}

// Logger defines the logging interface used by MPPValidator.
type Logger interface {
	Debugf(format string, params ...interface{})
	Infof(format string, params ...interface{})
	Warnf(format string, params ...interface{})
	Errorf(format string, params ...interface{})
}

// NewMPPValidator creates a new MPP validator instance.
func NewMPPValidator(config *MPPValidationConfig, logger Logger) *MPPValidator {
	var metrics *MPPMetrics
	if config.MetricsEnabled {
		metrics = NewMPPMetrics()
	}

	return &MPPValidator{
		config:           config,
		metrics:          metrics,
		logger:           logger,
		runtimeOverrides: make(map[string]EnforcementMode),
		startTime:        time.Now(),
	}
}

// Common validation errors.
var (
	ErrMPPRequired = errors.New(
		"MPP record required for this payment")
	ErrPaymentAddrMismatch  = errors.New("payment address mismatch")
	ErrInvalidPaymentAddr   = errors.New("invalid payment address format")
	ErrMPPEnforcementActive = errors.New(
		"MPP enforcement: payment_addr required")
)

// ValidationRequest contains the parameters for MPP validation.
type ValidationRequest struct {
	RPCMethod   string
	Route       *route.Route
	PaymentAddr fn.Option[[32]byte]
	RequestID   string // For logging/tracing
}

// ValidationResult contains the result of MPP validation.
type ValidationResult struct {
	Valid   bool
	Error   error
	Warning string
	Mode    EnforcementMode
}

// ValidateRoute validates a route for MPP compliance.
func (v *MPPValidator) ValidateRoute(req *ValidationRequest) *ValidationResult {
	if v.metrics != nil {
		start := time.Now()
		defer func() {
			v.metrics.validationAttempts.Inc()
			v.metrics.validationDuration.Observe(
				time.Since(start).Seconds())
		}()
	}

	// Get effective enforcement mode for this RPC
	mode := v.getEnforcementMode(req.RPCMethod)

	v.logger.Debugf("Validating route for %s with mode %s (request: %s)",
		req.RPCMethod, mode, req.RequestID)

	result := &ValidationResult{
		Valid: true,
		Mode:  mode,
	}

	// Legacy mode - no validation
	if mode == EnforcementModeLegacy {
		v.logger.Debugf("Legacy mode - skipping MPP validation")
		return result
	}

	// If route is nil or has no hops, skip validation
	if req.Route == nil || len(req.Route.Hops) == 0 {
		v.logger.Debugf(
			"Skipping validation for nil route or route " +
				"with no hops")

		return result
	}

	// Check if route has MPP record
	hasMPP := v.routeHasMPP(req.Route)
	hasPaymentAddr := req.PaymentAddr.IsSome()

	// Special handling for SendToRoute - the route itself must have MPP
	if req.RPCMethod == "SendToRoute" || req.RPCMethod == "SendToRouteV2" {
		if !hasMPP {
			return v.handleMissingMPP(req, mode)
		}
		// If route has MPP and we have a payment address, validate
		// match
		if hasMPP && hasPaymentAddr {
			return v.validatePaymentAddrMatch(req, mode)
		}
		// Route has MPP but no payment address to validate - pass
		v.logger.Debugf(
			"Route validation passed for %s (route has MPP)",
			req.RequestID)

		return result
	}

	// For other RPCs (BuildRoute, QueryRoutes), validate based on
	// payment address
	if !hasMPP && hasPaymentAddr {
		return v.handleMissingMPP(req, mode)
	}

	// Validate payment address match if both present
	if hasMPP && hasPaymentAddr {
		return v.validatePaymentAddrMatch(req, mode)
	}

	// No validation needed if neither has MPP requirements
	v.logger.Debugf("Route validation passed for %s", req.RequestID)

	return result
}

// routeHasMPP checks if the route contains an MPP record in the final hop.
func (v *MPPValidator) routeHasMPP(r *route.Route) bool {
	if r == nil || len(r.Hops) == 0 {
		return false
	}

	finalHop := r.Hops[len(r.Hops)-1]

	// Check if final hop has MPP record
	if finalHop.MPP != nil {
		return true
	}

	// Check custom records for MPP TLV (Type 8) as backup
	if finalHop.CustomRecords != nil {
		if _, exists := finalHop.CustomRecords[8]; exists {
			return true
		}
	}

	return false
}

// handleMissingMPP processes the case where MPP is expected but missing.
func (v *MPPValidator) handleMissingMPP(
	req *ValidationRequest, mode EnforcementMode,
) *ValidationResult {

	message := fmt.Sprintf(
		"Route missing required MPP record for %s (request: %s)",
		req.RPCMethod, req.RequestID)

	result := &ValidationResult{
		Mode: mode,
	}

	switch mode {
	case EnforcementModeWarn:
		result.Valid = true
		result.Warning = message
		v.logger.Warnf(message)
		if v.metrics != nil {
			v.metrics.warningsIssued.Inc()
		}

	case EnforcementModeEnforce:
		result.Valid = false
		result.Error = v.formatEnforcementError(req.RPCMethod)
		v.logger.Errorf("MPP enforcement rejection: %s", message)
		if v.metrics != nil {
			v.metrics.enforcementRejections.Inc()
			v.metrics.validationFailures.Inc()
		}
	}

	return result
}

// validatePaymentAddrMatch validates that the payment address in the route
// matches the expected one.
func (v *MPPValidator) validatePaymentAddrMatch(
	req *ValidationRequest, mode EnforcementMode,
) *ValidationResult {

	if req.Route == nil || len(req.Route.Hops) == 0 {
		return &ValidationResult{
			Valid: false,
			Error: errors.New("invalid route structure"),
			Mode:  mode,
		}
	}

	finalHop := req.Route.Hops[len(req.Route.Hops)-1]
	expectedAddr := req.PaymentAddr.UnwrapOr([32]byte{})

	// Extract payment address from route's MPP record
	var routePaymentAddr [32]byte
	found := false

	if finalHop.MPP != nil {
		routePaymentAddr = finalHop.MPP.PaymentAddr()
		found = true
	}

	if !found {
		// This shouldn't happen if routeHasMPP returned true, but be
		// defensive
		return &ValidationResult{
			Valid: false,
			Error: ErrMPPRequired,
			Mode:  mode,
		}
	}

	// Compare payment addresses
	if routePaymentAddr != expectedAddr {
		v.logger.Errorf(
			"Payment address mismatch in %s (request: %s): "+
				"expected %x, got %x",
			req.RPCMethod, req.RequestID,
			expectedAddr, routePaymentAddr)

		if v.metrics != nil {
			v.metrics.validationFailures.Inc()
		}

		return &ValidationResult{
			Valid: false,
			Error: ErrPaymentAddrMismatch,
			Mode:  mode,
		}
	}

	v.logger.Debugf(
		"Payment address validation passed for %s", req.RequestID)

	return &ValidationResult{
		Valid: true,
		Mode:  mode,
	}
}

// formatEnforcementError returns a user-friendly error message for
// enforcement rejections.
func (v *MPPValidator) formatEnforcementError(rpcMethod string) error {
	// Add helpful migration information
	switch rpcMethod {
	case "BuildRoute":
		return fmt.Errorf(
			"MPP enforcement: payment_addr required in " +
				"BuildRouteRequest")
	case "QueryRoutes":
		return fmt.Errorf(
			"MPP enforcement: payment_addr required in " +
				"QueryRoutesRequest")
	case "SendToRoute", "SendToRouteV2":
		return fmt.Errorf(
			"MPP enforcement: route must include MPP record " +
				"in final hop")
	default:
		return fmt.Errorf(
			"MPP enforcement: payment_addr required for %s",
			rpcMethod)
	}
}

// getEnforcementMode returns the effective enforcement mode for a given RPC
// method.
func (v *MPPValidator) getEnforcementMode(rpcMethod string) EnforcementMode {
	v.mu.RLock()
	defer v.mu.RUnlock()

	// Check emergency override first
	if v.config.EmergencyOverride {
		return EnforcementModeLegacy
	}

	// Check runtime override
	if mode, exists := v.runtimeOverrides[rpcMethod]; exists {
		return mode
	}

	// Check per-RPC configuration
	if mode, exists := v.config.PerRPCModes[rpcMethod]; exists {
		return mode
	}

	// Check auto-upgrade date
	if v.config.AutoUpgradeDate != nil &&
		time.Now().After(*v.config.AutoUpgradeDate) {

		return v.getUpgradedMode(v.config.GlobalMode)
	}

	// Check grace period (automatically upgrade from legacy to warn)
	if v.config.GlobalMode == EnforcementModeLegacy &&
		v.config.GracePeriodDays > 0 {

		graceEnd := v.startTime.AddDate(0, 0, v.config.GracePeriodDays)
		if time.Now().After(graceEnd) {
			return EnforcementModeWarn
		}
	}

	return v.config.GlobalMode
}

// getUpgradedMode returns the next enforcement level for auto-upgrade.
func (v *MPPValidator) getUpgradedMode(
	current EnforcementMode,
) EnforcementMode {

	switch current {
	case EnforcementModeLegacy:
		return EnforcementModeWarn
	case EnforcementModeWarn:
		return EnforcementModeEnforce
	default:
		return current
	}
}

// SetRuntimeOverride allows dynamic mode changes for specific RPC methods.
func (v *MPPValidator) SetRuntimeOverride(
	rpcMethod string, mode EnforcementMode,
) error {

	v.mu.Lock()
	defer v.mu.Unlock()

	currentMode := v.getEnforcementModeUnsafe(rpcMethod)

	// Validate downgrade restrictions (can't downgrade without emergency
	// override)
	if mode < currentMode && !v.config.EmergencyOverride {
		return fmt.Errorf(
			"cannot downgrade from %s to %s without "+
				"emergency override",
			currentMode, mode)
	}

	v.runtimeOverrides[rpcMethod] = mode

	if v.metrics != nil {
		v.metrics.modeChanges.Inc()
	}

	v.logger.Infof("MPP enforcement mode changed for %s: %s -> %s",
		rpcMethod, currentMode, mode)

	return nil
}

// getEnforcementModeUnsafe is like getEnforcementMode but assumes lock is
// held.
func (v *MPPValidator) getEnforcementModeUnsafe(
	rpcMethod string,
) EnforcementMode {

	if v.config.EmergencyOverride {
		return EnforcementModeLegacy
	}

	if mode, exists := v.runtimeOverrides[rpcMethod]; exists {
		return mode
	}

	if mode, exists := v.config.PerRPCModes[rpcMethod]; exists {
		return mode
	}

	return v.config.GlobalMode
}

// SetEmergencyOverride enables or disables emergency override mode.
func (v *MPPValidator) SetEmergencyOverride(enabled bool) {
	v.mu.Lock()
	v.config.EmergencyOverride = enabled
	v.mu.Unlock()

	if enabled {
		v.logger.Warnf(
			"MPP enforcement emergency override ENABLED - " +
				"all validation disabled")
	} else {
		v.logger.Infof("MPP enforcement emergency override disabled")
	}

	if v.metrics != nil {
		v.metrics.modeChanges.Inc()
	}
}

// GetEnforcementStatus returns the current enforcement status for all RPC
// methods.
func (v *MPPValidator) GetEnforcementStatus() map[string]EnforcementMode {
	v.mu.RLock()
	defer v.mu.RUnlock()

	status := make(map[string]EnforcementMode)

	// Standard RPC methods
	methods := []string{"BuildRoute", "QueryRoutes", "SendToRoute"}

	for _, method := range methods {
		status[method] = v.getEnforcementModeUnsafe(method)
	}

	return status
}

// ValidatePaymentAddr validates that a payment address has the correct format.
func ValidatePaymentAddr(paymentAddr []byte) error {
	if len(paymentAddr) != 32 {
		return fmt.Errorf("%w: expected 32 bytes, got %d",
			ErrInvalidPaymentAddr, len(paymentAddr))
	}

	// Check for all-zero address (usually invalid)
	allZero := true
	for _, b := range paymentAddr {
		if b != 0 {
			allZero = false
			break
		}
	}

	if allZero {
		return fmt.Errorf("%w: payment address cannot be all zeros",
			ErrInvalidPaymentAddr)
	}

	return nil
}

// CreateMPPRecord creates an MPP record for inclusion in a route.
func CreateMPPRecord(
	paymentAddr [32]byte, totalMsat lnwire.MilliSatoshi,
) *record.MPP {

	return record.NewMPP(totalMsat, paymentAddr)
}

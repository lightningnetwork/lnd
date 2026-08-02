package invoices

import (
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// HtlcResolution describes how an htlc should be resolved.
type HtlcResolution interface {
	// CircuitKey returns the circuit key for the htlc that we have a
	// resolution for.
	CircuitKey() CircuitKey
}

// HtlcFailResolution is an implementation of the HtlcResolution interface
// which is returned when a htlc is failed.
type HtlcFailResolution struct {
	// circuitKey is the key of the htlc for which we have a resolution.
	circuitKey CircuitKey

	// AcceptHeight is the original height at which the htlc was accepted.
	AcceptHeight int32

	// Outcome indicates the outcome of the invoice registry update.
	Outcome FailResolutionResult
}

// HtlcErrorResolution fails an HTLC with a specific authenticated wire
// failure selected by a subsystem other than the invoice registry.
type HtlcErrorResolution struct {
	// Failure is encrypted by the incoming link's error encrypter.
	Failure lnwire.FailureMessage

	// circuitKey identifies the HTLC resolved by this failure.
	circuitKey CircuitKey

	// AcceptHeight is the height at which the HTLC was accepted.
	AcceptHeight int32
}

// HtlcNestedErrorResolution forwards an opaque downstream trampoline failure
// after the downstream ordinary payment path has been fully peeled.
type HtlcNestedErrorResolution struct {
	// Reason is wrapped by the current trampoline and incoming outer error
	// secrets before it is returned to the previous hop.
	Reason lnwire.OpaqueReason

	// circuitKey identifies the HTLC resolved by this failure.
	circuitKey CircuitKey

	// AcceptHeight is the height at which the HTLC was accepted.
	AcceptHeight int32
}

// NewNestedErrorResolution constructs an opaque nested HTLC failure
// resolution.
func NewNestedErrorResolution(key CircuitKey, acceptHeight int32,
	reason lnwire.OpaqueReason) *HtlcNestedErrorResolution {

	return &HtlcNestedErrorResolution{
		Reason:       append(lnwire.OpaqueReason(nil), reason...),
		circuitKey:   key,
		AcceptHeight: acceptHeight,
	}
}

// CircuitKey returns the circuit key for the failed HTLC.
func (f *HtlcNestedErrorResolution) CircuitKey() CircuitKey {
	return f.circuitKey
}

// NewErrorResolution constructs a wire-specific HTLC failure resolution.
func NewErrorResolution(key CircuitKey, acceptHeight int32,
	failure lnwire.FailureMessage) *HtlcErrorResolution {

	return &HtlcErrorResolution{
		Failure:      failure,
		circuitKey:   key,
		AcceptHeight: acceptHeight,
	}
}

// CircuitKey returns the circuit key for the failed HTLC.
func (f *HtlcErrorResolution) CircuitKey() CircuitKey {
	return f.circuitKey
}

// NewFailResolution returns a htlc failure resolution.
func NewFailResolution(key CircuitKey, acceptHeight int32,
	outcome FailResolutionResult) *HtlcFailResolution {

	return &HtlcFailResolution{
		circuitKey:   key,
		AcceptHeight: acceptHeight,
		Outcome:      outcome,
	}
}

// CircuitKey returns the circuit key for the htlc that we have a
// resolution for.
//
// Note: it is part of the HtlcResolution interface.
func (f *HtlcFailResolution) CircuitKey() CircuitKey {
	return f.circuitKey
}

// HtlcSettleResolution is an implementation of the HtlcResolution interface
// which is returned when a htlc is settled.
type HtlcSettleResolution struct {
	// Preimage is the htlc preimage. Its value is nil in case of a cancel.
	Preimage lntypes.Preimage

	// circuitKey is the key of the htlc for which we have a resolution.
	circuitKey CircuitKey

	// acceptHeight is the original height at which the htlc was accepted.
	AcceptHeight int32

	// Outcome indicates the outcome of the invoice registry update.
	Outcome SettleResolutionResult
}

// NewSettleResolution returns a htlc resolution which is associated with a
// settle.
func NewSettleResolution(preimage lntypes.Preimage, key CircuitKey,
	acceptHeight int32,
	outcome SettleResolutionResult) *HtlcSettleResolution {

	return &HtlcSettleResolution{
		Preimage:     preimage,
		circuitKey:   key,
		AcceptHeight: acceptHeight,
		Outcome:      outcome,
	}
}

// CircuitKey returns the circuit key for the htlc that we have a
// resolution for.
//
// Note: it is part of the HtlcResolution interface.
func (s *HtlcSettleResolution) CircuitKey() CircuitKey {
	return s.circuitKey
}

// htlcAcceptResolution is an implementation of the HtlcResolution interface
// which is returned when a htlc is accepted. This struct is not exported
// because the codebase uses a nil resolution to indicate that a htlc was
// accepted. This struct is used internally in the invoice registry to
// surface accept resolution results. When an invoice update returns an
// acceptResolution, a nil resolution should be surfaced.
type htlcAcceptResolution struct {
	// circuitKey is the key of the htlc for which we have a resolution.
	circuitKey CircuitKey

	// autoRelease signals that the htlc should be automatically released
	// after a timeout.
	autoRelease bool

	// acceptTime is the time at which this htlc was accepted.
	acceptTime time.Time

	// outcome indicates the outcome of the invoice registry update.
	outcome acceptResolutionResult
}

// newAcceptResolution returns a htlc resolution which is associated with a
// htlc accept.
func newAcceptResolution(key CircuitKey,
	outcome acceptResolutionResult) *htlcAcceptResolution {

	return &htlcAcceptResolution{
		circuitKey: key,
		outcome:    outcome,
	}
}

// CircuitKey returns the circuit key for the htlc that we have a
// resolution for.
//
// Note: it is part of the HtlcResolution interface.
func (a *htlcAcceptResolution) CircuitKey() CircuitKey {
	return a.circuitKey
}

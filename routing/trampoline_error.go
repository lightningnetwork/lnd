package routing

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/trampoline"
)

// OpaqueTrampolineError carries a downstream failure after every ordinary
// payment-onion layer has been peeled without authentication. A trampoline
// relay must wrap these bytes with its inner and incoming outer error secrets.
type OpaqueTrampolineError struct {
	Reason lnwire.OpaqueReason
}

// Error returns a stable description without exposing encrypted failure data.
func (e *OpaqueTrampolineError) Error() string {
	return "opaque downstream trampoline failure"
}

// TrampolineFailureReason returns an independent copy of the fully
// outer-peeled failure reason.
func (e *OpaqueTrampolineError) TrampolineFailureReason() lnwire.OpaqueReason {
	return append(lnwire.OpaqueReason(nil), e.Reason...)
}

// TrampolineFailureFromPayment returns the last authenticated nested failure
// persisted for a trampoline relay payment. The returned packet is copied so
// callers cannot mutate payment database state.
func TrampolineFailureFromPayment(
	payment paymentsdb.DBMPPayment) *OpaqueTrampolineError {

	durable, ok := payment.(*paymentsdb.MPPayment)
	if !ok {
		return nil
	}

	htlcs := durable.GetHTLCs()
	for i := len(htlcs) - 1; i >= 0; i-- {
		failure := htlcs[i].Failure
		if failure == nil ||
			failure.Reason != paymentsdb.HTLCFailTrampoline {

			continue
		}

		return &OpaqueTrampolineError{
			Reason: append(
				lnwire.OpaqueReason(nil),
				failure.TrampolineReason[:]...,
			),
		}
	}

	return nil
}

type trampolineErrorDecrypter struct {
	outerSecrets      [][32]byte
	trampolineSecrets [][32]byte
	outerHopCount     int
}

// DecryptError first exhausts the ordinary payment path. Origin payments then
// continue through their persisted trampoline path, while relays return the
// fully outer-peeled reason for nested rewrapping.
func (d *trampolineErrorDecrypter) DecryptError(
	reason lnwire.OpaqueReason) (*htlcswitch.ForwardingError, error) {

	peeled, failure, err := trampoline.PeelErrorPath(
		reason, d.outerSecrets,
	)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		message, err := decodeTrampolineFailure(failure.Message)
		if err != nil {
			return htlcswitch.NewUnknownForwardingError(
				failure.SenderIndex + 1,
			), nil
		}

		return htlcswitch.NewForwardingError(
			message, failure.SenderIndex+1,
		), nil
	}

	if len(d.trampolineSecrets) == 0 {
		return nil, &OpaqueTrampolineError{
			Reason: append(lnwire.OpaqueReason(nil), peeled...),
		}
	}

	_, failure, err = trampoline.PeelErrorPath(
		peeled, d.trampolineSecrets,
	)
	if err != nil {
		return nil, err
	}
	if failure == nil {
		return nil, trampoline.ErrNestedErrorUndecryptable
	}

	message, err := decodeTrampolineFailure(failure.Message)
	if err != nil {
		return htlcswitch.NewUnknownForwardingError(
			d.outerHopCount,
		), nil
	}

	return htlcswitch.NewTrampolineForwardingError(
		message, d.outerHopCount, failure.SenderIndex,
	), nil
}

func decodeTrampolineFailure(
	message []byte) (lnwire.FailureMessage, error) {

	failure, err := lnwire.DecodeFailure(bytes.NewReader(message), 0)
	if err != nil {
		return nil, fmt.Errorf(
			"decode trampoline failure: %w", err,
		)
	}
	if failure == nil {
		return nil, errors.New("decoded trampoline failure is nil")
	}

	return failure, nil
}

var _ htlcswitch.ErrorDecrypter = (*trampolineErrorDecrypter)(nil)

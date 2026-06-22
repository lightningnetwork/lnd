package htlcswitch

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrCannotResumeOnChain is returned when an on-chain held HTLC is
	// resolved with a resume action.
	ErrCannotResumeOnChain = errors.New(
		"cannot resume held htlc in the on-chain flow",
	)

	// ErrCannotFailOnChain is returned when an on-chain held HTLC is
	// resolved with a fail action.
	ErrCannotFailOnChain = errors.New(
		"cannot fail held htlc in the on-chain flow",
	)

	// errNilHeldForward is returned when the held HTLC constructors or
	// add helpers are given a nil InterceptedForward.
	errNilHeldForward = errors.New("nil held htlc forward")

	// errInvalidHeldDeadline is returned when a held HTLC has an
	// interceptor deadline that is not a positive block height.
	errInvalidHeldDeadline = errors.New(
		"invalid held htlc interceptor deadline",
	)

	// errInvalidHeldDeadlineType is returned when a held HTLC entry is
	// created with the wrong deadline type for its source.
	errInvalidHeldDeadlineType = errors.New(
		"invalid held htlc interceptor deadline type",
	)
)

// heldEntry models the behavior of a held HTLC based on whether it is still
// controlled by the off-chain link flow or the on-chain contractcourt flow.
type heldEntry interface {
	// interceptedForward returns the forward that should be replayed to the
	// external interceptor.
	interceptedForward() InterceptedForward

	// resolve applies an interceptor resolution to the held entry.
	resolve(*FwdResolution) error

	// expire expires the held entry at the given block height. The boolean
	// return value indicates whether the entry should be removed.
	expire(height uint32) (bool, error)
}

// offChainHeld is a held HTLC that is still controlled by the off-chain link
// flow.
type offChainHeld struct {
	fwd InterceptedForward

	// autoFailHeight is the block height at which the held off-chain HTLC
	// must be failed back to avoid forcing the incoming channel closed.
	autoFailHeight uint32
}

// Assert that offChainHeld implements heldEntry.
var _ heldEntry = (*offChainHeld)(nil)

// newOffChainHeld creates a held off-chain HTLC entry and validates that it has
// a positive auto-fail height.
func newOffChainHeld(fwd InterceptedForward) (*offChainHeld, error) {
	if fwd == nil {
		return nil, errNilHeldForward
	}

	autoFailHeight, err := fwd.Packet().Deadline.LeftToSome().UnwrapOrErr(
		errInvalidHeldDeadlineType,
	)
	if err != nil {
		return nil, err
	}

	if autoFailHeight <= 0 {
		return nil, fmt.Errorf("%w: %v", errInvalidHeldDeadline,
			autoFailHeight)
	}

	return &offChainHeld{
		fwd:            fwd,
		autoFailHeight: uint32(autoFailHeight),
	}, nil
}

// interceptedForward returns the intercepted forward backing the off-chain
// entry.
func (h *offChainHeld) interceptedForward() InterceptedForward {
	return h.fwd
}

// release resumes the held off-chain HTLC into the normal link forwarding
// flow.
func (h *offChainHeld) release() error {
	return h.fwd.Resume()
}

// resolve applies an interceptor resolution to the held off-chain HTLC.
func (h *offChainHeld) resolve(res *FwdResolution) error {
	switch res.Action {
	case FwdActionResume:
		return h.fwd.Resume()

	case FwdActionResumeModified:
		return h.fwd.ResumeModified(
			res.InAmountMsat, res.OutAmountMsat,
			res.OutWireCustomRecords,
		)

	case FwdActionSettle:
		return h.fwd.Settle(res.Preimage)

	case FwdActionFail:
		if len(res.FailureMessage) > 0 {
			return h.fwd.Fail(res.FailureMessage)
		}

		return h.fwd.FailWithCode(res.FailureCode)

	default:
		return fmt.Errorf("unrecognized action %v", res.Action)
	}
}

// expire fails back the held off-chain HTLC once its auto-fail height has been
// reached.
func (h *offChainHeld) expire(height uint32) (bool, error) {
	if h.autoFailHeight > height {
		return false, nil
	}

	err := h.fwd.FailWithCode(lnwire.CodeTemporaryChannelFailure)
	if err != nil {
		return false, err
	}

	return true, nil
}

// onChainHeld is a held HTLC that is controlled by the on-chain contractcourt
// flow.
type onChainHeld struct {
	fwd InterceptedForward

	// settleDeadline is the on-chain HTLC expiry. Once this height is
	// reached, the remote party can also sweep the HTLC using the timeout
	// path, so any late preimage would race that spend. At that point the
	// interceptor entry is pruned locally instead of failed back through
	// the link.
	settleDeadline uint32
}

// Assert that onChainHeld implements heldEntry.
var _ heldEntry = (*onChainHeld)(nil)

// newOnChainHeld creates a held on-chain HTLC entry and validates that it has a
// positive settlement deadline.
func newOnChainHeld(fwd InterceptedForward) (*onChainHeld, error) {
	if fwd == nil {
		return nil, errNilHeldForward
	}

	settleDeadline, err := fwd.Packet().Deadline.RightToSome().UnwrapOrErr(
		errInvalidHeldDeadlineType,
	)
	if err != nil {
		return nil, err
	}

	if settleDeadline <= 0 {
		return nil, fmt.Errorf("%w: %v", errInvalidHeldDeadline,
			settleDeadline)
	}

	return &onChainHeld{
		fwd:            fwd,
		settleDeadline: uint32(settleDeadline),
	}, nil
}

// interceptedForward returns the intercepted forward backing the on-chain
// entry.
func (h *onChainHeld) interceptedForward() InterceptedForward {
	return h.fwd
}

// resolve applies an interceptor resolution to the held on-chain HTLC.
func (h *onChainHeld) resolve(res *FwdResolution) error {
	switch res.Action {
	case FwdActionSettle:
		return h.fwd.Settle(res.Preimage)

	case FwdActionFail:
		return ErrCannotFailOnChain

	case FwdActionResume:
		return ErrCannotResumeOnChain

	case FwdActionResumeModified:
		return ErrCannotResumeOnChain

	default:
		return fmt.Errorf("unrecognized action %v", res.Action)
	}
}

// expire reports whether the held on-chain HTLC should be pruned locally
// because its settlement deadline has been reached.
func (h *onChainHeld) expire(height uint32) (bool, error) {
	return h.settleDeadline <= height, nil
}

// heldHtlcExpireError records an error returned while expiring a held HTLC.
type heldHtlcExpireError struct {
	key models.CircuitKey
	err error
}

// heldHtlcReleaseError records an error returned while releasing a held HTLC.
type heldHtlcReleaseError struct {
	key models.CircuitKey
	err error
}

// heldHtlcSet keeps track of outstanding intercepted forwards. It models
// whether each forward is still controlled by the off-chain link flow or has
// moved to the on-chain contractcourt flow.
type heldHtlcSet struct {
	set map[models.CircuitKey]heldEntry
}

func newHeldHtlcSet() *heldHtlcSet {
	return &heldHtlcSet{
		set: make(map[models.CircuitKey]heldEntry),
	}
}

// forEach iterates over all held forwards and calls the given callback for each
// of them.
func (h *heldHtlcSet) forEach(cb func(InterceptedForward)) {
	for _, entry := range h.set {
		cb(entry.interceptedForward())
	}
}

// releaseAllOffChainHeld releases off-chain entries when the optional
// interceptor disconnects. On-chain entries are kept because there is no link
// flow to resume, preserving the replay/settle handle while contractcourt waits
// for the preimage or on-chain expiry.
func (h *heldHtlcSet) releaseAllOffChainHeld() []heldHtlcReleaseError {
	var errs []heldHtlcReleaseError

	for key, entry := range h.set {
		offChain, ok := entry.(*offChainHeld)
		if !ok {
			continue
		}

		if err := offChain.release(); err != nil {
			errs = append(errs, heldHtlcReleaseError{
				key: key,
				err: err,
			})

			// Keep the entry tracked so it can still be resolved or
			// failed back by the normal expiry path.
			continue
		}

		delete(h.set, key)
	}

	return errs
}

// removeOnChainHeld removes an on-chain held entry by circuit key. Off-chain
// entries are left untouched because their lifecycle is owned by the link flow,
// not contractcourt.
func (h *heldHtlcSet) removeOnChainHeld(key models.CircuitKey) bool {
	if _, ok := h.set[key].(*onChainHeld); !ok {
		return false
	}

	delete(h.set, key)

	return true
}

// expire expires held forwards whose deadline has passed.
func (h *heldHtlcSet) expire(height uint32) []heldHtlcExpireError {
	var errs []heldHtlcExpireError

	for key, entry := range h.set {
		remove, err := entry.expire(height)
		if err != nil {
			errs = append(errs, heldHtlcExpireError{
				key: key,
				err: err,
			})

			continue
		}

		if remove {
			delete(h.set, key)
		}
	}

	return errs
}

// resolve applies the given resolution and removes the forward from the set if
// the resolution succeeds.
func (h *heldHtlcSet) resolve(res *FwdResolution) error {
	entry, ok := h.set[res.Key]
	if !ok {
		return fmt.Errorf("%w: %v", ErrFwdNotExists, res.Key)
	}

	if err := entry.resolve(res); err != nil {
		return err
	}

	delete(h.set, res.Key)

	return nil
}

// exists tests whether the specified forward is part of the set.
func (h *heldHtlcSet) exists(key models.CircuitKey) bool {
	_, ok := h.set[key]

	return ok
}

// addOffChain adds an off-chain forward to the set. If the forward already
// exists, the duplicate is ignored because callers should have handled it
// before insertion.
func (h *heldHtlcSet) addOffChain(fwd InterceptedForward) error {
	if fwd == nil {
		return errNilHeldForward
	}

	key := fwd.Packet().IncomingCircuit
	if h.exists(key) {
		log.Warnf("Ignoring duplicate off-chain held htlc %v", key)

		return nil
	}

	entry, err := newOffChainHeld(fwd)
	if err != nil {
		return err
	}

	h.set[key] = entry

	return nil
}

// addOnChain adds an on-chain forward to the set. If the same HTLC is currently
// held off-chain, it is replaced so future resolutions go to the witness beacon
// instead of the old link mailbox path.
func (h *heldHtlcSet) addOnChain(fwd InterceptedForward) error {
	if fwd == nil {
		return errNilHeldForward
	}

	key := fwd.Packet().IncomingCircuit

	if _, ok := h.set[key].(*onChainHeld); ok {
		return nil
	}

	if _, ok := h.set[key].(*offChainHeld); ok {
		log.Infof("Promoting held htlc %v from off-chain to "+
			"on-chain resolution", key)
	}

	entry, err := newOnChainHeld(fwd)
	if err != nil {
		return err
	}

	h.set[key] = entry

	return nil
}

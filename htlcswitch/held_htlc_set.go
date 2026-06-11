package htlcswitch

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// errNilHeldForward is returned when a nil forward is used to create a
	// held HTLC entry.
	errNilHeldForward = errors.New("nil held htlc forward")

	// errInvalidHeldDeadline is returned when a held HTLC has an
	// interceptor deadline that is not a positive block height.
	errInvalidHeldDeadline = errors.New(
		"invalid held htlc interceptor deadline",
	)

	// errInvalidOnChainResolution is returned when an interceptor attempts
	// to resolve an on-chain held HTLC with anything other than settle.
	errInvalidOnChainResolution = errors.New(
		"invalid on-chain htlc resolution",
	)
)

// heldEntry models the behavior of a held HTLC based on whether it is still
// controlled by the off-chain link flow or the on-chain contractcourt flow.
type heldEntry interface {
	// interceptedForward returns the forward that should be replayed to the
	// external interceptor.
	interceptedForward() InterceptedForward

	// release handles an interceptor disconnect when interception is no
	// longer required.
	release() error

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

	autoFailHeight := fwd.Packet().AutoFailHeight
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

	if err := h.fwd.FailWithCode(
		lnwire.CodeTemporaryChannelFailure,
	); err != nil {
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

	settleDeadline := fwd.Packet().AutoFailHeight
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

// release prunes the held on-chain HTLC locally. There is no live link flow to
// resume; the contest resolver remains responsible for resolving the HTLC on
// chain.
func (h *onChainHeld) release() error {
	return nil
}

// resolve applies an interceptor resolution to the held on-chain HTLC.
func (h *onChainHeld) resolve(res *FwdResolution) error {
	if res.Action != FwdActionSettle {
		return errInvalidOnChainResolution
	}

	return h.fwd.Settle(res.Preimage)
}

// expire prunes the held on-chain HTLC once its settlement deadline has been
// reached.
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

// releaseAll releases all held entries and removes them from the set. Off-chain
// entries are resumed, while on-chain entries are only pruned locally.
func (h *heldHtlcSet) releaseAll() []heldHtlcReleaseError {
	var errs []heldHtlcReleaseError

	for key, entry := range h.set {
		if err := entry.release(); err != nil {
			errs = append(errs, heldHtlcReleaseError{
				key: key,
				err: err,
			})
		}
	}

	h.set = make(map[models.CircuitKey]heldEntry)

	return errs
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

// addOffChain adds an off-chain forward to the set. Existing forwards are kept
// because replayed packets may already be held.
func (h *heldHtlcSet) addOffChain(fwd InterceptedForward) error {
	if fwd == nil {
		return errNilHeldForward
	}

	key := fwd.Packet().IncomingCircuit
	if h.exists(key) {
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

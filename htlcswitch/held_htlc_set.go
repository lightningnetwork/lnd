package htlcswitch

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/graph/db/models"
)

// heldHtlcSet keeps track of outstanding intercepted forwards. It exposes
// several methods to manipulate the underlying map structure in a consistent
// way.
type heldHtlcSet struct {
	set map[models.CircuitKey]InterceptedForward
}

func newHeldHtlcSet() *heldHtlcSet {
	return &heldHtlcSet{
		set: make(map[models.CircuitKey]InterceptedForward),
	}
}

// forEach iterates over all held forwards and calls the given callback for each
// of them.
func (h *heldHtlcSet) forEach(cb func(InterceptedForward)) {
	for _, fwd := range h.set {
		cb(fwd)
	}
}

// popAll calls the callback for each forward and removes them from the set.
func (h *heldHtlcSet) popAll(cb func(InterceptedForward)) {
	for _, fwd := range h.set {
		cb(fwd)
	}

	h.set = make(map[models.CircuitKey]InterceptedForward)
}

// popAutoFails calls the callback for each forward that has an auto-fail height
// equal or less then the specified pop height and removes them from the set.
func (h *heldHtlcSet) popAutoFails(height uint32, cb func(InterceptedForward)) {
	for key, fwd := range h.set {
		if uint32(fwd.Packet().AutoFailHeight) > height {
			continue
		}

		cb(fwd)

		delete(h.set, key)
	}
}

// pop returns the specified forward and removes it from the set.
func (h *heldHtlcSet) pop(key models.CircuitKey) (InterceptedForward, error) {
	intercepted, ok := h.set[key]
	if !ok {
		return nil, fmt.Errorf("fwd %v not found", key)
	}

	delete(h.set, key)

	return intercepted, nil
}

// exists tests whether the specified forward is part of the set.
func (h *heldHtlcSet) exists(key models.CircuitKey) bool {
	_, ok := h.set[key]

	return ok
}

// push adds the specified forward to the set. An error is returned if the
// forward exists already.
func (h *heldHtlcSet) push(key models.CircuitKey,
	fwd InterceptedForward) error {

	if fwd == nil {
		return errors.New("nil fwd pushed")
	}

	if h.exists(key) {
		return errors.New("htlc already exists in set")
	}

	h.set[key] = fwd

	return nil
}

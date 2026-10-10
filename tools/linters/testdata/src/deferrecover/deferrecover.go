// Package deferrecover contains valid and invalid RecoverPanic uses for the
// deferrecover analyzer.
package deferrecover

import (
	. "github.com/lightningnetwork/lnd/fn/v2"
	panicfn "github.com/lightningnetwork/lnd/fn/v2"
)

// handlePanic is the callback shared by the analyzer test cases.
func handlePanic(panicfn.Panic) {}

// valid exercises the supported selector and dot-import forms.
func valid() {
	defer panicfn.RecoverPanic(handlePanic)
	defer RecoverPanic(handlePanic)
}

// invalid exercises calls that are not deferred directly.
func invalid() {
	panicfn.RecoverPanic(handlePanic) // want "deferred directly"

	defer func() {
		panicfn.RecoverPanic(handlePanic) // want "deferred directly"
	}()

	go panicfn.RecoverPanic(handlePanic) // want "deferred directly"
	RecoverPanic(handlePanic)            // want "deferred directly"
}

// unrelated provides a same-named method from a different type.
type unrelated struct{}

// RecoverPanic is intentionally unrelated to fn.RecoverPanic.
func (unrelated) RecoverPanic(func(panicfn.Panic)) {}

// unrelatedMethod verifies that same-named methods are ignored.
func unrelatedMethod() {
	var value unrelated
	value.RecoverPanic(handlePanic)
}

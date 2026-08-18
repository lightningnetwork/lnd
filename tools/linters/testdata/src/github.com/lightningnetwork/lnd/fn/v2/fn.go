// Package fn provides the minimal API needed by the deferrecover analyzer
// fixture.
package fn

// Panic represents a recovered panic in the analyzer fixture.
type Panic struct{}

// RecoverPanic represents the function checked by the analyzer fixture.
func RecoverPanic(func(Panic)) {}

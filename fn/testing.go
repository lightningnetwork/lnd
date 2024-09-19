package fn

// TestingT abstracts *testing.T, *testing.B, assert.TestingT etc types
// that are passed to testing functions. It has only the methods needed
// by Option.UnwrapOrFail and Result.UnwrapOrFail.
type TestingT interface {
	// Errorf formats its arguments according to the format, analogous to
	// Printf, and records the text in the error log, then marks the
	// function as having failed.
	Errorf(format string, args ...any)
}

// TestingHelper abstracts *testing.T, *testing.B etc types that are passed to
// testing functions. It has only Helper method.
type TestingHelper interface {
	// Helper marks the calling function as a test helper function. When
	// printing file and line information, that function will be skipped.
	// Helper may be called simultaneously from multiple goroutines.
	Helper()
}

// TestingFailer abstracts *testing.T, *testing.B etc types that are passed to
// testing functions. It has only FailNow method.
type TestingFailer interface {
	// FailNow marks the function as having failed and stops its execution
	// by calling runtime.Goexit (which then runs all deferred calls in the
	// current goroutine). Execution will continue at the next test or
	// benchmark. FailNow must be called from the goroutine running the test
	// or benchmark function, not from other goroutines created during the
	// test. Calling FailNow does not stop those other goroutines.
	FailNow()
}

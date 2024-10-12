package lncfg

import "fmt"

// UsageError is an error type that signals a problem with the supplied flags.
type UsageError struct {
	Err error
}

// Error returns the error string.
//
// NOTE: This is part of the error interface.
func (u *UsageError) Error() string {
	return u.Err.Error()
}

// Unwrap returns the underlying error.
func (u *UsageError) Unwrap() error {
	return u.Err
}

// mkErr creates a new error from a string.
func mkErr(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}

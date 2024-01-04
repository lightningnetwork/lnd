package lnutils

import "errors"

// ErrorAs behaves the same as `errors.As` except there's no need to declare
// the target error as a variable first.
// Instead of writing:
//
//	var targetErr *TargetErr
//	errors.As(err, &targetErr)
//
// We can write:
//
//	lnutils.ErrorAs[*TargetErr](err)
//
// To save us from declaring the target error variable.
func ErrorAs[Target error](err error) bool {
	var targetErr Target

	return errors.As(err, &targetErr)
}

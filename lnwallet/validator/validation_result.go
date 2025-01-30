package validator

import "fmt"

// ValidationResult represents the result of a sign request validation.
type ValidationResult struct {
	// Type is the type of the validation result.
	Type ValidationResultType

	// FailureDetails provides unstructured, human-readable details
	// about why a validation failed.
	FailureDetails string
}

// ValidationFailureResult returns a validation failure result, including
// details on why the validation failed.
func ValidationFailureResult(format string, params ...any) ValidationResult {
	return NewValidationResult(ValidationFailure).withDetails(
		format, params,
	)
}

// ValidationSuccessResult returns a validation success result.
func ValidationSuccessResult() ValidationResult {
	return NewValidationResult(ValidationSuccess)
}

// NewValidationResult creates a new ValidationResult with the given type.
func NewValidationResult(t ValidationResultType) ValidationResult {
	return ValidationResult{
		Type: t,
	}
}

// withDetails sets the unstructured human-readable details about why the
// validation failed.
func (v ValidationResult) withDetails(format string,
	params ...any) ValidationResult {

	v.FailureDetails = fmt.Sprintf(format, params...)

	return v
}

// ValidationResultType is an enum that represents the result type of a
// validation.
type ValidationResultType uint8

const (
	// ValidationSuccess indicates that the validation was successful,
	// and the psbt request should be signed.
	ValidationSuccess ValidationResultType = 0

	// ValidationFailure indicates that the validation failed, and the psbt
	// request should be denied.
	ValidationFailure ValidationResultType = 1
)

// String returns a human-readable representation of the validation result type.
func (v ValidationResultType) String() string {
	switch v {
	case ValidationSuccess:
		return "validation_success"
	case ValidationFailure:
		return "validation_failure"
	default:
		return "unknown_validation_result_type"
	}
}

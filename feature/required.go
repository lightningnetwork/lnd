package feature

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrUnknownRequired signals that a feature vector requires certain features
// that our node is unaware of or does not implement.
type ErrUnknownRequired struct {
	unknown []lnwire.FeatureBit
}

// NewErrUnknownRequired initializes an ErrUnknownRequired with the unknown
// feature bits.
func NewErrUnknownRequired(unknown []lnwire.FeatureBit) ErrUnknownRequired {
	return ErrUnknownRequired{
		unknown: unknown,
	}
}

// Error returns a human-readable description of the error.
func (e ErrUnknownRequired) Error() string {
	return fmt.Sprintf("feature vector contains unknown required "+
		"features: %v", e.unknown)
}

// ValidateRequired returns an error if the feature vector contains a non-zero
// number of unknown, required feature bits.
func ValidateRequired(fv *lnwire.FeatureVector) error {
	unknown := fv.UnknownRequiredFeatures()
	if len(unknown) > 0 {
		return NewErrUnknownRequired(unknown)
	}
	return nil
}

package feature

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
)

type (
	// featureSet contains a set of feature bits.
	featureSet map[lnwire.FeatureBit]struct{}

	// supportedFeatures maps the feature bit from a feature vector to a
	// boolean indicating if this features dependencies have already been
	// verified. This allows us to short circuit verification if multiple
	// features have common dependencies, or map traversal starts verifying
	// from the bottom up.
	supportedFeatures map[lnwire.FeatureBit]bool

	// depDesc maps a features to its set of dependent features, which must
	// also be present for the vector to be valid. This can be used to
	// recursively check the dependency chain for features in a feature
	// vector.
	depDesc map[lnwire.FeatureBit]featureSet
)

// ErrMissingFeatureDep is an error signaling that a transitive dependency in a
// feature vector is not set properly.
type ErrMissingFeatureDep struct {
	dep lnwire.FeatureBit
}

// NewErrMissingFeatureDep creates a new ErrMissingFeatureDep error.
func NewErrMissingFeatureDep(dep lnwire.FeatureBit) ErrMissingFeatureDep {
	return ErrMissingFeatureDep{dep: dep}
}

// Error returns a human-readable description of the missing dep error.
func (e ErrMissingFeatureDep) Error() string {
	return fmt.Sprintf("missing feature dependency: %v", e.dep)
}

// deps is the default set of dependencies for assigned feature bits. If a
// feature is not present in the depDesc it is assumed to have no dependencies.
//
// NOTE: For proper functioning, only the optional variant of feature bits
// should be used in the following descriptor. In the future it may be necessary
// to distinguish the dependencies for optional and required bits, but for now
// the validation code maps required bits to optional ones since it simplifies
// the number of constraints.
var deps = depDesc{
	lnwire.PaymentAddrOptional: {
		lnwire.TLVOnionPayloadOptional: {},
	},
	lnwire.MPPOptional: {
		lnwire.PaymentAddrOptional: {},
	},
	lnwire.AnchorsOptional: {
		lnwire.StaticRemoteKeyOptional: {},
	},
	lnwire.AnchorsZeroFeeHtlcTxOptional: {
		lnwire.StaticRemoteKeyOptional: {},
	},
	lnwire.AMPOptional: {
		lnwire.PaymentAddrOptional: {},
	},
	lnwire.ExplicitChannelTypeOptional: {},
	lnwire.ScriptEnforcedLeaseOptional: {
		lnwire.ExplicitChannelTypeOptional:  {},
		lnwire.AnchorsZeroFeeHtlcTxOptional: {},
	},
	lnwire.KeysendOptional: {
		lnwire.TLVOnionPayloadOptional: {},
	},
	lnwire.ZeroConfOptional: {
		lnwire.ScidAliasOptional: {},
	},
	lnwire.SimpleTaprootChannelsOptionalStaging: {
		lnwire.AnchorsZeroFeeHtlcTxOptional: {},
		lnwire.ExplicitChannelTypeOptional:  {},
	},
	lnwire.SimpleTaprootOverlayChansOptional: {
		lnwire.SimpleTaprootChannelsOptionalStaging: {},
		lnwire.TLVOnionPayloadOptional:              {},
		lnwire.ScidAliasOptional:                    {},
	},
	lnwire.RouteBlindingOptional: {
		lnwire.TLVOnionPayloadOptional: {},
	},
	lnwire.Bolt11BlindedPathsOptional: {
		lnwire.RouteBlindingOptional: {},
	},
}

// ValidateDeps asserts that a feature vector sets all features and their
// transitive dependencies properly. It assumes that the dependencies between
// optional and required features are identical, e.g. if a feature is required
// but its dependency is optional, that is sufficient.
func ValidateDeps(fv *lnwire.FeatureVector) error {
	features := fv.Features()
	supported := initSupported(features)

	return validateDeps(features, supported)
}

// SetBit sets the given feature bit on the given feature bit vector along with
// any of its dependencies. If the bit is required, then all the dependencies
// are also set to required, otherwise, the optional dependency bits are set.
// Existing bits are only upgraded from optional to required but never
// downgraded from required to optional.
func SetBit(vector *lnwire.FeatureVector,
	bit lnwire.FeatureBit) *lnwire.FeatureVector {

	fv := vector.Clone()

	// Get the optional version of the bit since that is what the deps map
	// uses.
	optBit := mapToOptional(bit)

	// If the bit we are setting is optional, then we set it (in its
	// optional form) and also set all its dependents as optional if they
	// are not already set (they may already be set in a required form in
	// which case they should not be overridden).
	if !bit.IsRequired() {
		// Set the bit itself if it does not already exist. We use
		// SafeSet here so that if the bit already exists in the
		// required form, then this is not overwritten.
		_ = fv.SafeSet(bit)

		// Do the same for all the dependent bits.
		for depBit := range deps[optBit] {
			fv = SetBit(fv, depBit)
		}

		return fv
	}

	// The bit is required. In this case, we do want to override any
	// existing optional bit for both the bit itself and for the dependent
	// bits.
	fv.Unset(optBit)
	fv.Set(bit)

	// Do the same for all the dependent bits.
	for depBit := range deps[optBit] {
		// The deps map only contains the optional versions of bits, so
		// there is no need to first map the bit to the optional
		// version.
		fv.Unset(depBit)

		// Set the required version of the bit instead.
		fv = SetBit(fv, mapToRequired(depBit))
	}

	return fv
}

// validateDeps is a subroutine that recursively checks that the passed features
// have all of their associated dependencies in the supported map.
func validateDeps(features featureSet, supported supportedFeatures) error {
	for bit := range features {
		// Convert any required bits to optional.
		bit = mapToOptional(bit)

		// If the supported features doesn't contain the dependency, this
		// vector is invalid.
		checked, ok := supported[bit]
		if !ok {
			return NewErrMissingFeatureDep(bit)
		}

		// Alternatively, if we know that this dependency is valid, we
		// can short circuit and continue verifying other bits.
		if checked {
			continue
		}

		// Recursively validate dependencies, since this method ranges
		// over the subDeps. This method will return true even if
		// subDeps is nil.
		subDeps := deps[bit]
		if err := validateDeps(subDeps, supported); err != nil {
			return err
		}

		// Once we've confirmed that this feature's dependencies, if
		// any, are sound, we record this so other paths taken through
		// `bit` return early when inspecting the supported map.
		supported[bit] = true
	}

	return nil
}

// initSupported sets all bits from the feature vector as supported but not
// checked. This signals that the validity of their dependencies has not been
// verified. All required bits are mapped to optional to simplify the DAG.
func initSupported(features featureSet) supportedFeatures {
	supported := make(supportedFeatures)
	for bit := range features {
		bit = mapToOptional(bit)
		supported[bit] = false
	}

	return supported
}

// mapToOptional returns the optional variant of a given feature bit pair. Our
// dependency graph is described using only optional feature bits, which
// reduces the number of constraints we need to express in the descriptor.
func mapToOptional(bit lnwire.FeatureBit) lnwire.FeatureBit {
	if bit.IsRequired() {
		bit ^= 0x01
	}
	return bit
}

// mapToRequired returns the required variant of a given feature bit pair.
func mapToRequired(bit lnwire.FeatureBit) lnwire.FeatureBit {
	if bit.IsRequired() {
		return bit
	}
	bit ^= 0x01

	return bit
}

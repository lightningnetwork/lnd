package feature

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrUnknownSet is returned if a proposed feature vector contains a
	// set that is unknown to LND.
	ErrUnknownSet = errors.New("unknown feature bit set")

	// ErrFeatureConfigured is returned if an attempt is made to unset a
	// feature that was configured at startup.
	ErrFeatureConfigured = errors.New("can't unset configured feature")
)

// Config houses any runtime modifications to the default set descriptors. For
// our purposes, this typically means disabling certain features to test legacy
// protocol interoperability or functionality.
type Config struct {
	// NoTLVOnion unsets any optional or required TLVOnionPaylod bits from
	// all feature sets.
	NoTLVOnion bool

	// NoStaticRemoteKey unsets any optional or required StaticRemoteKey
	// bits from all feature sets.
	NoStaticRemoteKey bool

	// NoAnchors unsets any bits signaling support for anchor outputs.
	NoAnchors bool

	// NoWumbo unsets any bits signalling support for wumbo channels.
	NoWumbo bool

	// NoTaprootChans unsets any bits signaling support for taproot
	// channels.
	NoTaprootChans bool

	// NoScriptEnforcementLease unsets any bits signaling support for script
	// enforced leases.
	NoScriptEnforcementLease bool

	// NoKeysend unsets any bits signaling support for accepting keysend
	// payments.
	NoKeysend bool

	// NoOptionScidAlias unsets any bits signalling support for
	// option_scid_alias. This also implicitly disables zero-conf channels.
	NoOptionScidAlias bool

	// NoZeroConf unsets any bits signalling support for zero-conf
	// channels. This should be used instead of NoOptionScidAlias to still
	// keep option-scid-alias support.
	NoZeroConf bool

	// NoAnySegwit unsets any bits that signal support for using other
	// segwit witness versions for co-op closes.
	NoAnySegwit bool

	// NoRouteBlinding unsets route blinding feature bits.
	NoRouteBlinding bool

	// NoQuiescence unsets quiescence feature bits.
	NoQuiescence bool

	// NoTaprootOverlay unsets the taproot overlay channel feature bits.
	NoTaprootOverlay bool

	// NoExperimentalEndorsement unsets any bits that signal support for
	// forwarding experimental endorsement.
	NoExperimentalEndorsement bool

	// NoRbfCoopClose unsets any bits that signal support for using RBF for
	// coop close.
	NoRbfCoopClose bool

	// CustomFeatures is a set of custom features to advertise in each
	// set.
	CustomFeatures map[Set][]lnwire.FeatureBit
}

// Manager is responsible for generating feature vectors for different requested
// feature sets.
type Manager struct {
	// fsets is a static map of feature set to raw feature vectors. Requests
	// are fulfilled by cloning these internal feature vectors.
	fsets map[Set]*lnwire.RawFeatureVector

	// configFeatures is a set of custom features that were "hard set" in
	// lnd's config that cannot be updated at runtime (as is the case with
	// our "standard" features that are defined in LND).
	configFeatures map[Set]*lnwire.FeatureVector
}

// NewManager creates a new feature Manager, applying any custom modifications
// to its feature sets before returning.
func NewManager(cfg Config) (*Manager, error) {
	return newManager(cfg, defaultSetDesc)
}

// newManager creates a new feature Manager, applying any custom modifications
// to its feature sets before returning. This method accepts the setDesc as its
// own parameter so that it can be unit tested.
func newManager(cfg Config, desc setDesc) (*Manager, error) {
	// First build the default feature vector for all known sets.
	fsets := make(map[Set]*lnwire.RawFeatureVector)
	for bit, sets := range desc {
		for set := range sets {
			// Fetch the feature vector for this set, allocating a
			// new one if it doesn't exist.
			fv, ok := fsets[set]
			if !ok {
				fv = lnwire.NewRawFeatureVector()
			}

			// Set the configured bit on the feature vector,
			// ensuring that we don't set two feature bits for the
			// same pair.
			err := fv.SafeSet(bit)
			if err != nil {
				return nil, fmt.Errorf("unable to set "+
					"%v in %v: %v", bit, set, err)
			}

			// Write the updated feature vector under its set.
			fsets[set] = fv
		}
	}

	// Now, remove any features as directed by the config.
	configFeatures := make(map[Set]*lnwire.FeatureVector)
	for set, raw := range fsets {
		if cfg.NoTLVOnion {
			raw.Unset(lnwire.TLVOnionPayloadOptional)
			raw.Unset(lnwire.TLVOnionPayloadRequired)
			raw.Unset(lnwire.PaymentAddrOptional)
			raw.Unset(lnwire.PaymentAddrRequired)
			raw.Unset(lnwire.MPPOptional)
			raw.Unset(lnwire.MPPRequired)
			raw.Unset(lnwire.RouteBlindingOptional)
			raw.Unset(lnwire.RouteBlindingRequired)
			raw.Unset(lnwire.Bolt11BlindedPathsOptional)
			raw.Unset(lnwire.Bolt11BlindedPathsRequired)
			raw.Unset(lnwire.AMPOptional)
			raw.Unset(lnwire.AMPRequired)
			raw.Unset(lnwire.KeysendOptional)
			raw.Unset(lnwire.KeysendRequired)
		}
		if cfg.NoStaticRemoteKey {
			raw.Unset(lnwire.StaticRemoteKeyOptional)
			raw.Unset(lnwire.StaticRemoteKeyRequired)
		}
		if cfg.NoAnchors {
			raw.Unset(lnwire.AnchorsZeroFeeHtlcTxOptional)
			raw.Unset(lnwire.AnchorsZeroFeeHtlcTxRequired)

			// If anchors are disabled, then we also need to
			// disable all other features that depend on it as
			// well, as otherwise we may create an invalid feature
			// bit set.
			for bit, depFeatures := range deps {
				for depFeature := range depFeatures {
					switch {
					case depFeature == lnwire.AnchorsZeroFeeHtlcTxRequired:
						fallthrough
					case depFeature == lnwire.AnchorsZeroFeeHtlcTxOptional:
						raw.Unset(bit)
					}
				}
			}
		}
		if cfg.NoWumbo {
			raw.Unset(lnwire.WumboChannelsOptional)
			raw.Unset(lnwire.WumboChannelsRequired)
		}
		if cfg.NoScriptEnforcementLease {
			raw.Unset(lnwire.ScriptEnforcedLeaseOptional)
			raw.Unset(lnwire.ScriptEnforcedLeaseRequired)
		}
		if cfg.NoKeysend {
			raw.Unset(lnwire.KeysendOptional)
			raw.Unset(lnwire.KeysendRequired)
		}
		if cfg.NoOptionScidAlias {
			raw.Unset(lnwire.ScidAliasOptional)
			raw.Unset(lnwire.ScidAliasRequired)
		}
		if cfg.NoZeroConf {
			raw.Unset(lnwire.ZeroConfOptional)
			raw.Unset(lnwire.ZeroConfRequired)
		}
		if cfg.NoAnySegwit {
			raw.Unset(lnwire.ShutdownAnySegwitOptional)
			raw.Unset(lnwire.ShutdownAnySegwitRequired)
		}
		if cfg.NoTaprootChans {
			raw.Unset(lnwire.SimpleTaprootChannelsOptionalStaging)
			raw.Unset(lnwire.SimpleTaprootChannelsRequiredStaging)
		}
		if cfg.NoRouteBlinding {
			raw.Unset(lnwire.RouteBlindingOptional)
			raw.Unset(lnwire.RouteBlindingRequired)
			raw.Unset(lnwire.Bolt11BlindedPathsOptional)
			raw.Unset(lnwire.Bolt11BlindedPathsRequired)
		}
		if cfg.NoQuiescence {
			raw.Unset(lnwire.QuiescenceOptional)
		}
		if cfg.NoTaprootOverlay {
			raw.Unset(lnwire.SimpleTaprootOverlayChansOptional)
			raw.Unset(lnwire.SimpleTaprootOverlayChansRequired)
		}
		if cfg.NoExperimentalEndorsement {
			raw.Unset(lnwire.ExperimentalEndorsementOptional)
			raw.Unset(lnwire.ExperimentalEndorsementRequired)
		}
		if cfg.NoRbfCoopClose {
			raw.Unset(lnwire.RbfCoopCloseOptionalStaging)
			raw.Unset(lnwire.RbfCoopCloseOptional)
		}

		for _, custom := range cfg.CustomFeatures[set] {
			if custom > set.Maximum() {
				return nil, fmt.Errorf("feature bit: %v "+
					"exceeds set: %v maximum: %v", custom,
					set, set.Maximum())
			}

			if raw.IsSet(custom) {
				return nil, fmt.Errorf("feature bit: %v "+
					"already set", custom)
			}

			if err := raw.SafeSet(custom); err != nil {
				return nil, fmt.Errorf("%w: could not set "+
					"feature: %d", err, custom)
			}
		}

		// Track custom features separately so that we can check that
		// they aren't unset in subsequent updates. If there is no
		// entry for the set, the vector will just be empty.
		configFeatures[set] = lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(cfg.CustomFeatures[set]...),
			lnwire.Features,
		)

		// Ensure that all of our feature sets properly set any
		// dependent features.
		fv := lnwire.NewFeatureVector(raw, lnwire.Features)
		err := ValidateDeps(fv)
		if err != nil {
			return nil, fmt.Errorf("invalid feature set %v: %w",
				set, err)
		}
	}

	return &Manager{
		fsets:          fsets,
		configFeatures: configFeatures,
	}, nil
}

// GetRaw returns a raw feature vector for the passed set. If no set is known,
// an empty raw feature vector is returned.
func (m *Manager) GetRaw(set Set) *lnwire.RawFeatureVector {
	if fv, ok := m.fsets[set]; ok {
		return fv.Clone()
	}

	return lnwire.NewRawFeatureVector()
}

// setRaw sets a new raw feature vector for the given set.
func (m *Manager) setRaw(set Set, raw *lnwire.RawFeatureVector) {
	m.fsets[set] = raw
}

// Get returns a feature vector for the passed set. If no set is known, an empty
// feature vector is returned.
func (m *Manager) Get(set Set) *lnwire.FeatureVector {
	raw := m.GetRaw(set)
	return lnwire.NewFeatureVector(raw, lnwire.Features)
}

// ListSets returns a list of the feature sets that our node supports.
func (m *Manager) ListSets() []Set {
	var sets []Set

	for set := range m.fsets {
		sets = append(sets, set)
	}

	return sets
}

// UpdateFeatureSets accepts a map of new feature vectors for each of the
// manager's known sets, validates that the update can be applied and modifies
// the feature manager's internal state. If a set is not included in the update
// map, it is left unchanged. The feature vectors provided are expected to
// include the current set of features, updated with desired bits added/removed.
func (m *Manager) UpdateFeatureSets(
	updates map[Set]*lnwire.RawFeatureVector) error {

	for set, newFeatures := range updates {
		if !set.valid() {
			return fmt.Errorf("%w: set: %d", ErrUnknownSet, set)
		}

		if err := newFeatures.ValidatePairs(); err != nil {
			return err
		}

		if err := m.Get(set).ValidateUpdate(
			newFeatures, set.Maximum(),
		); err != nil {
			return err
		}

		// If any features were configured for this set, ensure that
		// they are still set in the new feature vector.
		if cfgFeat, haveCfgFeat := m.configFeatures[set]; haveCfgFeat {
			for feature := range cfgFeat.Features() {
				if !newFeatures.IsSet(feature) {
					return fmt.Errorf("%w: can't unset: "+
						"%d", ErrFeatureConfigured,
						feature)
				}
			}
		}

		fv := lnwire.NewFeatureVector(newFeatures, lnwire.Features)
		if err := ValidateDeps(fv); err != nil {
			return err
		}
	}

	// Only update the current feature sets once every proposed set has
	// passed validation so that we don't partially update any sets then
	// fail out on a later set's validation.
	for set, features := range updates {
		m.setRaw(set, features.Clone())
	}

	return nil
}

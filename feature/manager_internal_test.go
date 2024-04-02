package feature

import (
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

type managerTest struct {
	name string
	cfg  Config
}

const unknownFeature lnwire.FeatureBit = 30

var testSetDesc = setDesc{
	lnwire.DataLossProtectRequired: {
		SetNodeAnn: {}, // I
	},
	lnwire.TLVOnionPayloadRequired: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.StaticRemoteKeyRequired: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
}

var managerTests = []managerTest{
	{
		name: "default",
		cfg:  Config{},
	},
	{
		name: "no tlv",
		cfg: Config{
			NoTLVOnion: true,
		},
	},
	{
		name: "no static remote key",
		cfg: Config{
			NoStaticRemoteKey: true,
		},
	},
	{
		name: "no tlv or static remote key",
		cfg: Config{
			NoTLVOnion:        true,
			NoStaticRemoteKey: true,
		},
	},
	{
		name: "anchors should disable anything dependent on it",
		cfg: Config{
			NoAnchors: true,
		},
	},
}

// TestManager asserts basic initialazation and operation of a feature manager,
// including that the proper features are removed in response to config changes.
func TestManager(t *testing.T) {
	for _, test := range managerTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testManager(t, test)
		})
	}
}

func testManager(t *testing.T, test managerTest) {
	m, err := newManager(test.cfg, testSetDesc)
	require.NoError(t, err, "unable to create feature manager")

	sets := []Set{
		SetInit,
		SetLegacyGlobal,
		SetNodeAnn,
		SetInvoice,
	}

	for _, set := range sets {
		raw := m.GetRaw(set)
		fv := m.Get(set)

		fv2 := lnwire.NewFeatureVector(raw, lnwire.Features)

		if !reflect.DeepEqual(fv, fv2) {
			t.Fatalf("mismatch Get vs GetRaw, raw: %v vs fv: %v",
				fv2, fv)
		}

		assertUnset := func(bit lnwire.FeatureBit) {
			hasBit := fv.HasFeature(bit) || fv.HasFeature(bit^1)
			if hasBit {
				t.Fatalf("bit %v or %v is set", bit, bit^1)
			}
		}

		// Assert that the manager properly unset the configured feature
		// bits from all sets.
		if test.cfg.NoTLVOnion {
			assertUnset(lnwire.TLVOnionPayloadRequired)
			assertUnset(lnwire.TLVOnionPayloadOptional)
		}
		if test.cfg.NoStaticRemoteKey {
			assertUnset(lnwire.StaticRemoteKeyRequired)
			assertUnset(lnwire.StaticRemoteKeyOptional)
		}
		if test.cfg.NoAnchors {
			assertUnset(lnwire.ScriptEnforcedLeaseRequired)
			assertUnset(lnwire.ScriptEnforcedLeaseOptional)
		}

		assertUnset(unknownFeature)
	}

	// Do same basic sanity checks on features that are always present.
	nodeFeatures := m.Get(SetNodeAnn)

	assertSet := func(bit lnwire.FeatureBit) {
		has := nodeFeatures.HasFeature(bit)
		if !has {
			t.Fatalf("node features don't advertised %v", bit)
		}
	}

	assertSet(lnwire.DataLossProtectRequired)
	if !test.cfg.NoTLVOnion {
		assertSet(lnwire.TLVOnionPayloadRequired)
	}
	if !test.cfg.NoStaticRemoteKey {
		assertSet(lnwire.StaticRemoteKeyRequired)
	}
}

// TestUpdateFeatureSets tests validation of the update of various features in
// each of our sets, asserting that the feature set is not partially modified
// if one set in incorrectly specified.
func TestUpdateFeatureSets(t *testing.T) {
	t.Parallel()

	// Use a reduced set description to make reasoning about our sets
	// easier.
	setDesc := setDesc{
		lnwire.DataLossProtectRequired: {
			SetInit:    {}, // I
			SetNodeAnn: {}, // N
		},
		lnwire.GossipQueriesOptional: {
			SetNodeAnn: {}, // N
		},
	}

	testCases := []struct {
		name     string
		features map[Set]*lnwire.RawFeatureVector
		config   Config
		err      error
	}{
		{
			name: "unknown set",
			features: map[Set]*lnwire.RawFeatureVector{
				setSentinel + 1: lnwire.NewRawFeatureVector(),
			},
			err: ErrUnknownSet,
		},
		{
			name: "invalid pairwise feature",
			features: map[Set]*lnwire.RawFeatureVector{
				SetNodeAnn: lnwire.NewRawFeatureVector(
					lnwire.FeatureBit(1000),
					lnwire.FeatureBit(1001),
				),
			},
			err: lnwire.ErrFeaturePairExists,
		},
		{
			name: "error in one set",
			features: map[Set]*lnwire.RawFeatureVector{
				SetNodeAnn: lnwire.NewRawFeatureVector(
					lnwire.FeatureBit(1000),
					lnwire.FeatureBit(1001),
				),
				SetInit: lnwire.NewRawFeatureVector(
					lnwire.DataLossProtectRequired,
				),
			},
			err: lnwire.ErrFeaturePairExists,
		},
		{
			name: "update existing sets ok",
			features: map[Set]*lnwire.RawFeatureVector{
				SetInit: lnwire.NewRawFeatureVector(
					lnwire.DataLossProtectRequired,
					lnwire.FeatureBit(1001),
				),
				SetNodeAnn: lnwire.NewRawFeatureVector(
					lnwire.DataLossProtectRequired,
					lnwire.GossipQueriesOptional,
					lnwire.FeatureBit(1000),
				),
			},
		},
		{
			name: "update new, valid set ok",
			features: map[Set]*lnwire.RawFeatureVector{
				SetInvoice: lnwire.NewRawFeatureVector(
					lnwire.FeatureBit(1001),
				),
			},
		},
		{
			name: "missing configured feature",
			features: map[Set]*lnwire.RawFeatureVector{
				SetInit: lnwire.NewRawFeatureVector(
					lnwire.DataLossProtectRequired,
				),
				SetNodeAnn: lnwire.NewRawFeatureVector(
					lnwire.DataLossProtectRequired,
					lnwire.GossipQueriesOptional,
				),
			},
			config: Config{
				CustomFeatures: map[Set][]lnwire.FeatureBit{
					SetInit: {
						lnwire.FeatureBit(333),
					},
				},
			},
			err: ErrFeatureConfigured,
		},
		{
			name: "valid",
			features: map[Set]*lnwire.RawFeatureVector{
				SetInit: lnwire.NewRawFeatureVector(
					lnwire.DataLossProtectRequired,
				),
				SetNodeAnn: lnwire.NewRawFeatureVector(
					lnwire.DataLossProtectRequired,
					lnwire.GossipQueriesOptional,
					lnwire.FeatureBit(500),
				),
				SetInvoice: lnwire.NewRawFeatureVector(
					lnwire.FeatureBit(333),
				),
			},
			config: Config{
				CustomFeatures: map[Set][]lnwire.FeatureBit{
					SetInvoice: {
						lnwire.FeatureBit(333),
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			featureMgr, err := newManager(testCase.config, setDesc)
			require.NoError(t, err)

			err = featureMgr.UpdateFeatureSets(testCase.features)
			require.ErrorIs(t, err, testCase.err)

			// Compare the feature manager's sets to the updated
			// set if no error was hit, otherwise assert that it
			// is unchanged.
			expected := testCase.features
			actual := featureMgr
			if err != nil {
				originalMgr, err := newManager(
					testCase.config, setDesc,
				)
				require.NoError(t, err)
				expected = originalMgr.fsets
			}

			for set, expectedFeatures := range expected {
				actualSet := actual.GetRaw(set)
				require.True(t,
					actualSet.Equals(expectedFeatures))
			}
		})
	}
}

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
	lnwire.TLVOnionPayloadOptional: {
		SetInit:    {}, // I
		SetNodeAnn: {}, // N
	},
	lnwire.StaticRemoteKeyOptional: {
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
			assertUnset(lnwire.TLVOnionPayloadOptional)
		}
		if test.cfg.NoStaticRemoteKey {
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

	assertSet(lnwire.DataLossProtectOptional)
	if !test.cfg.NoTLVOnion {
		assertSet(lnwire.TLVOnionPayloadRequired)
	}
	if !test.cfg.NoStaticRemoteKey {
		assertSet(lnwire.StaticRemoteKeyOptional)
	}
}

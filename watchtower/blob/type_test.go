package blob_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/watchtower/blob"
)

var unknownFlag = blob.Flag(16)

type typeStringTest struct {
	name   string
	typ    blob.Type
	expStr string
}

var typeStringTests = []typeStringTest{
	{
		name: "commit no-reward",
		typ:  blob.TypeAltruistCommit,
		expStr: "[No-FlagTaprootChannel|" +
			"No-FlagAnchorChannel|" +
			"FlagCommitOutputs|" +
			"No-FlagReward]",
	},
	{
		name: "commit reward",
		typ:  blob.TypeRewardCommit,
		expStr: "[No-FlagTaprootChannel|" +
			"No-FlagAnchorChannel|" +
			"FlagCommitOutputs|" +
			"FlagReward]",
	},
	{
		name: "unknown flag",
		typ:  unknownFlag.Type(),
		expStr: "0000000000010000[No-FlagTaprootChannel|" +
			"No-FlagAnchorChannel|" +
			"No-FlagCommitOutputs|" +
			"No-FlagReward]",
	},
}

// TestTypeStrings asserts that the proper human-readable string is returned for
// various blob.Types
func TestTypeStrings(t *testing.T) {
	for _, test := range typeStringTests {
		t.Run(test.name, func(t *testing.T) {
			typeStr := test.typ.String()
			if typeStr != test.expStr {
				t.Fatalf("mismatched type string, want: %v, "+
					"got %v", test.expStr, typeStr)
			}
		})
	}
}

// TestUnknownFlagString asserts that the proper string is returned from
// unallocated flags.
func TestUnknownFlagString(t *testing.T) {
	if unknownFlag.String() != "FlagUnknown" {
		t.Fatalf("unknown flags should return FlagUnknown, instead "+
			"got: %v", unknownFlag.String())
	}
}

type typeFromFlagTest struct {
	name    string
	flags   []blob.Flag
	expType blob.Type
}

var typeFromFlagTests = []typeFromFlagTest{
	{
		name:    "no flags",
		flags:   nil,
		expType: blob.Type(0),
	},
	{
		name:    "single flag",
		flags:   []blob.Flag{blob.FlagReward},
		expType: blob.Type(blob.FlagReward),
	},
	{
		name:    "multiple flags",
		flags:   []blob.Flag{blob.FlagReward, blob.FlagCommitOutputs},
		expType: blob.TypeRewardCommit,
	},
	{
		name:    "duplicate flag",
		flags:   []blob.Flag{blob.FlagReward, blob.FlagReward},
		expType: blob.Type(blob.FlagReward),
	},
}

// TestTypeFromFlags asserts that blob.Types constructed using
// blob.TypeFromFlags are correct, and properly deduplicate flags. We also
// assert that Has returns true for the generated blob.Type for all of the flags
// that were used to create it.
func TestTypeFromFlags(t *testing.T) {
	for _, test := range typeFromFlagTests {
		t.Run(test.name, func(t *testing.T) {
			blobType := blob.TypeFromFlags(test.flags...)

			// Assert that the constructed type matches our
			// expectation.
			if blobType != test.expType {
				t.Fatalf("mismatch, expected blob type %s, "+
					"got %s", test.expType, blobType)
			}

			// Assert that Has returns true for all flags used to
			// construct the type.
			for _, flag := range test.flags {
				if blobType.Has(flag) {
					continue
				}

				t.Fatalf("expected type to have flag %s, "+
					"but didn't", flag)
			}
		})
	}
}

// TestSupportedTypes verifies that blob.IsSupported returns true for all
// blob.Types returned from blob.SupportedTypes. It also asserts that the
// blob.DefaultType returns true.
func TestSupportedTypes(t *testing.T) {
	// Assert that the package's default type is supported.
	if !blob.IsSupportedType(blob.TypeAltruistCommit) {
		t.Fatalf("default type %s is not supported", blob.TypeAltruistCommit)
	}

	// Assert that the altruist anchor commit types are supported.
	if !blob.IsSupportedType(blob.TypeAltruistAnchorCommit) {
		t.Fatalf("default type %s is not supported",
			blob.TypeAltruistAnchorCommit)
	}

	// Assert that all claimed supported types are actually supported.
	for _, supType := range blob.SupportedTypes() {
		if blob.IsSupportedType(supType) {
			continue
		}

		t.Fatalf("supposedly supported type %s is not supported",
			supType)
	}
}

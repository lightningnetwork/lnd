package hodl_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/htlcswitch/hodl"
)

var hodlMaskTests = []struct {
	mask  hodl.Mask
	flags map[hodl.Flag]struct{}
}{
	{
		// Check that the empty mask has no active flags.
		mask:  hodl.MaskNone,
		flags: map[hodl.Flag]struct{}{},
	},
	{
		// Check that passing no arguments to MaskFromFlags is
		// equivalent to MaskNone.
		mask:  hodl.MaskFromFlags(),
		flags: map[hodl.Flag]struct{}{},
	},

	{
		// Check using Mask to convert a single flag into a Mask only
		// reports that flag active.
		mask: hodl.ExitSettle.Mask(),
		flags: map[hodl.Flag]struct{}{
			hodl.ExitSettle: {},
		},
	},
	{
		// Check that using MaskFromFlags on a single flag only reports
		// that flag active.
		mask: hodl.MaskFromFlags(hodl.Commit),
		flags: map[hodl.Flag]struct{}{
			hodl.Commit: {},
		},
	},

	{
		// Check that using MaskFromFlags on some-but-not-all flags
		// reports the correct subset of flags as active.
		mask: hodl.MaskFromFlags(
			hodl.ExitSettle,
			hodl.Commit,
			hodl.AddIncoming,
			hodl.SettleOutgoing,
		),
		flags: map[hodl.Flag]struct{}{
			hodl.ExitSettle:     {},
			hodl.Commit:         {},
			hodl.AddIncoming:    {},
			hodl.SettleOutgoing: {},
		},
	},
	{
		// Check that using MaskFromFlags on all known flags reports
		// those an no other flags.
		mask: hodl.MaskFromFlags(
			hodl.ExitSettle,
			hodl.AddIncoming,
			hodl.SettleIncoming,
			hodl.FailIncoming,
			hodl.AddOutgoing,
			hodl.SettleOutgoing,
			hodl.FailOutgoing,
			hodl.Commit,
			hodl.BogusSettle,
		),
		flags: map[hodl.Flag]struct{}{
			hodl.ExitSettle:     {},
			hodl.AddIncoming:    {},
			hodl.SettleIncoming: {},
			hodl.FailIncoming:   {},
			hodl.AddOutgoing:    {},
			hodl.SettleOutgoing: {},
			hodl.FailOutgoing:   {},
			hodl.Commit:         {},
			hodl.BogusSettle:    {},
		},
	},
}

// TestMask iterates through all of the hodlMaskTests, checking that the mask
// correctly reports active for flags in the tests' expected flags, and inactive
// for all others.
func TestMask(t *testing.T) {
	if !build.IsDevBuild() {
		t.Fatalf("htlcswitch tests must be run with '-tags=dev'")
	}

	for i, test := range hodlMaskTests {
		for j := uint32(0); i < 32; i++ {
			flag := hodl.Flag(1 << j)
			_, shouldBeActive := test.flags[flag]

			switch {
			case shouldBeActive && !test.mask.Active(flag):
				t.Fatalf("hodl mask test #%d -- "+
					"expected flag %s to be active",
					i, flag)

			case !shouldBeActive && test.mask.Active(flag):
				t.Fatalf("hodl mask test #%d -- "+
					"expected flag %s to be inactive",
					i, flag)
			}
		}
	}
}

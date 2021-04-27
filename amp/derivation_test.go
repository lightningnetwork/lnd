package amp_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/amp"
	"github.com/stretchr/testify/require"
)

type sharerTest struct {
	name      string
	numShares int
	merge     bool
}

var sharerTests = []sharerTest{
	{
		name:      "root only",
		numShares: 1,
	},
	{
		name:      "two shares",
		numShares: 2,
	},
	{
		name:      "many shares",
		numShares: 10,
	},
	{
		name:      "merge 4 shares",
		numShares: 4,
		merge:     true,
	},
	{
		name:      "merge many shares",
		numShares: 20,
		merge:     true,
	},
}

// TestSharer executes the end-to-end derivation between sender and receiver,
// asserting that shares are properly computed and, when reconstructed by the
// receiver, produce identical child hashes and preimages as the sender.
func TestSharer(t *testing.T) {
	for _, test := range sharerTests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			testSharer(t, test)
		})
	}
}

func testSharer(t *testing.T, test sharerTest) {
	// Construct a new sharer with a random seed.
	var (
		sharer amp.Sharer
		err    error
	)
	sharer, err = amp.NewSeedSharer()
	require.NoError(t, err)

	// Assert that we can instantiate an equivalent root sharer using the
	// root share.
	root := sharer.Root()
	sharerFromRoot := amp.SeedSharerFromRoot(&root)
	require.Equal(t, sharer, sharerFromRoot)

	// Generate numShares-1 randomized shares.
	children := make([]*amp.Child, 0, test.numShares)
	for i := 0; i < test.numShares-1; i++ {
		var left amp.Sharer
		left, sharer, err = sharer.Split()
		require.NoError(t, err)

		child := left.Child(0)

		assertChildShare(t, child, 0)
		children = append(children, child)
	}

	// Compute the final share and finalize the sharing.
	child := sharer.Child(0)
	sharer = sharer.Zero()

	assertChildShare(t, child, 0)
	children = append(children, child)

	// If we are testing merging, merge half of the created children back
	// into the sharer.
	if test.merge {
		for i := len(children) / 2; i < len(children); i++ {
			sharer = sharer.Merge(children[i])
		}
		children = children[:len(children)/2]

		// We must create a new last child from what we just merged
		// back.
		child := sharer.Child(0)

		assertChildShare(t, child, 0)
		children = append(children, child)
	}

	assertReconstruction(t, children...)
}

// assertChildShare checks that the child has the expected child index, and that
// the child's preimage is valid for the its hash.
func assertChildShare(t *testing.T, child *amp.Child, expIndex int) {
	t.Helper()

	require.Equal(t, uint32(expIndex), child.Index)
	require.True(t, child.Preimage.Matches(child.Hash))
}

// assertReconstruction takes a list of children and simulates the receiver
// recombining the shares, and then deriving the child preimage and hash for
// each HTLC. This asserts that the receiver can always rederive the full set of
// children knowing only the shares and child indexes for each.
func assertReconstruction(t *testing.T, children ...*amp.Child) {
	t.Helper()

	// Reconstruct a child descriptor for each of the provided children.
	// In practice, the receiver will only know the share and the child
	// index it learns for each HTLC.
	descs := make([]amp.ChildDesc, 0, len(children))
	for _, child := range children {
		descs = append(descs, amp.ChildDesc{
			Share: child.Share,
			Index: child.Index,
		})
	}

	// Now, recombine the shares and rederive a child for each of the
	// descriptors above. The resulting set of children should exactly match
	// the set provided.
	children2 := amp.ReconstructChildren(descs...)
	require.Equal(t, children, children2)
}

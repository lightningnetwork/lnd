package amp

import (
	"crypto/rand"
	"fmt"
)

// zeroShare is the all-zero 32-byte share.
var zeroShare = Share{}

// Sharer facilitates dynamic splitting of a root share value and derivation of
// child preimage and hashes for individual HTLCs in an AMP payment. A sharer
// represents a specific node in an abstract binary tree that can generate up to
// 2^32-1 unique child preimage-hash pairs for the same share value. A node can
// also be split into it's left and right child in the tree. The Sharer
// guarantees that the share value of the left and right child XOR to the share
// value of the parent. This allows larger HTLCs to split into smaller
// subpayments, while ensuring that the reconstructed secret will exactly match
// the root seed.
type Sharer interface {
	// Root returns the root share of the derivation tree. This is the value
	// that will be reconstructed when combining the set of all child
	// shares.
	Root() Share

	// Child derives a child preimage and child hash given a 32-bit index.
	// Passing a different index will generate a unique preimage-hash pair
	// with high probability, allowing the payment hash carried on HTLCs to
	// be refreshed without needing to modify the share value. This would
	// typically be used when an partial payment needs to be retried if it
	// encounters routine network failures.
	Child(index uint32) *Child

	// Split returns a Sharer for the left and right child of the parent
	// Sharer. XORing the share values of both sharers always yields the
	// share value of the parent. The sender should use this to recursively
	// divide payments that are too large into smaller subpayments, knowing
	// that the shares of all nodes descending from the parent will XOR to
	// the parent's share.
	Split() (Sharer, Sharer, error)

	// Merge takes the given Child and "merges" it into the Sharer by
	// XOR-ing its share with the Sharer's current share.
	Merge(*Child) Sharer

	// Zero returns a a new "zero Sharer" that has its current share set to
	// zero, while keeping the root share. Merging a Child from the
	// original Sharer into this zero-Sharer gives back the original
	// Sharer.
	Zero() Sharer
}

// SeedSharer orchestrates the sharing of the root AMP seed along multiple
// paths. It also supports derivation of the child payment hashes that get
// attached to HTLCs, and the child preimages used by the receiver to settle
// individual HTLCs in the set.
type SeedSharer struct {
	root Share
	curr Share
}

// NewSeedSharer generates a new SeedSharer instance with a seed drawn at
// random.
func NewSeedSharer() (*SeedSharer, error) {
	var root Share
	if _, err := rand.Read(root[:]); err != nil {
		return nil, err
	}

	return SeedSharerFromRoot(&root), nil
}

// SeedSharerFromRoot instantiates a SeedSharer with an externally provided
// seed.
func SeedSharerFromRoot(root *Share) *SeedSharer {
	return initSeedSharer(root, root)
}

func initSeedSharer(root, curr *Share) *SeedSharer {
	return &SeedSharer{
		root: *root,
		curr: *curr,
	}
}

// Seed returns the sharer's seed, the primary source of entropy for deriving
// shares of the root.
func (s *SeedSharer) Root() Share {
	return s.root
}

// Split constructs two child Sharers whose shares sum to the parent Sharer.
// This allows an HTLC whose payment amount could not be routed to be
// recursively split into smaller subpayments. After splitting a sharer the
// parent share should no longer be used, and the caller should use the Child
// method on each to derive preimage/hash pairs for the HTLCs.
func (s *SeedSharer) Split() (Sharer, Sharer, error) {
	// We cannot split the zero-Sharer.
	if s.curr == zeroShare {
		return nil, nil, fmt.Errorf("cannot split zero-Sharer")
	}

	shareLeft, shareRight, err := split(&s.curr)
	if err != nil {
		return nil, nil, err
	}

	left := initSeedSharer(&s.root, &shareLeft)
	right := initSeedSharer(&s.root, &shareRight)

	return left, right, nil
}

// Merge takes the given Child and "merges" it into the Sharer by XOR-ing its
// share with the Sharer's current share.
func (s *SeedSharer) Merge(child *Child) Sharer {
	var curr Share
	curr.Xor(&s.curr, &child.Share)

	sharer := initSeedSharer(&s.root, &curr)
	return sharer
}

// Zero returns a a new "zero Sharer" that has its current share set to zero,
// while keeping the root share. Merging a Child from the original Sharer into
// this zero-Sharer gives back the original Sharer.
func (s *SeedSharer) Zero() Sharer {
	return initSeedSharer(&s.root, &zeroShare)
}

// Child derives a preimage/hash pair to be used for an AMP HTLC.
// All children of s will use the same underlying share, but have unique
// preimage and hash. This can be used to rerandomize the preimage/hash pair for
// a given HTLC if a new route is needed.
func (s *SeedSharer) Child(index uint32) *Child {
	desc := ChildDesc{
		Share: s.curr,
		Index: index,
	}

	return DeriveChild(s.root, desc)
}

// ReconstructChildren derives the set of children hashes and preimages from the
// provided descriptors. The shares from each child descriptor are first used to
// compute the root, afterwards the child hashes and preimages are
// deterministically computed. For child descriptor at index i in the input,
// it's derived child will occupy index i of the returned children.
func ReconstructChildren(descs ...ChildDesc) []*Child {
	// Recompute the root by XORing the provided shares.
	var root Share
	for _, desc := range descs {
		root.Xor(&root, &desc.Share)
	}

	// With the root computed, derive the child hashes and preimages from
	// the child descriptors.
	children := make([]*Child, len(descs))
	for i, desc := range descs {
		children[i] = DeriveChild(root, desc)
	}

	return children
}

// split splits a share into two random values, that when XOR'd reproduce the
// original share. Given a share s, the two shares are derived as:
//
//	left <-$- random
//	right = parent ^ left.
//
// When reconstructed, we have that:
//
//	left ^ right = left ^ parent ^ left
//	             = parent.
func split(parent *Share) (Share, Share, error) {
	// Generate a random share for the left child.
	var left Share
	if _, err := rand.Read(left[:]); err != nil {
		return Share{}, Share{}, err
	}

	// Compute right = parent ^ left.
	var right Share
	right.Xor(parent, &left)

	return left, right, nil
}

var _ Sharer = (*SeedSharer)(nil)

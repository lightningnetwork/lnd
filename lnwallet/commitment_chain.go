package lnwallet

import (
	"github.com/lightningnetwork/lnd/fn/v2"
)

// commitmentChain represents a chain of unrevoked commitments. The tail of the
// chain is the latest fully signed, yet unrevoked commitment. Two chains are
// tracked, one for the local node, and another for the remote node. New
// commitments we create locally extend the remote node's chain, and vice
// versa. Commitment chains are allowed to grow to a bounded length, after
// which the tail needs to be "dropped" before new commitments can be received.
// The tail is "dropped" when the owner of the chain sends a revocation for the
// previous tail.
type commitmentChain struct {
	// commitments is a linked list of commitments to new states. New
	// commitments are added to the end of the chain with increase height.
	// Once a commitment transaction is revoked, the tail is incremented,
	// freeing up the revocation window for new commitments.
	commitments *fn.List[*commitment]
}

// newCommitmentChain creates a new commitment chain.
func newCommitmentChain() *commitmentChain {
	return &commitmentChain{
		commitments: fn.NewList[*commitment](),
	}
}

// addCommitment extends the commitment chain by a single commitment. This
// added commitment represents a state update proposed by either party. Once
// the commitment prior to this commitment is revoked, the commitment becomes
// the new defacto state within the channel.
func (s *commitmentChain) addCommitment(c *commitment) {
	s.commitments.PushBack(c)
}

// advanceTail reduces the length of the commitment chain by one. The tail of
// the chain should be advanced once a revocation for the lowest unrevoked
// commitment in the chain is received.
func (s *commitmentChain) advanceTail() {
	s.commitments.Remove(s.commitments.Front())
}

// tip returns the latest commitment added to the chain.
func (s *commitmentChain) tip() *commitment {
	return s.commitments.Back().Value
}

// tail returns the lowest unrevoked commitment transaction in the chain.
func (s *commitmentChain) tail() *commitment {
	return s.commitments.Front().Value
}

// hasUnackedCommitment returns true if the commitment chain has more than one
// entry. The tail of the commitment chain has been ACKed by revoking all prior
// commitments, but any subsequent commitments have not yet been ACKed.
func (s *commitmentChain) hasUnackedCommitment() bool {
	return s.commitments.Front() != s.commitments.Back()
}

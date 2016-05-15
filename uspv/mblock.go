package uspv

import (
	"fmt"

	"github.com/roasbeef/btcd/wire"
)

func MakeMerkleParent(left *wire.ShaHash, right *wire.ShaHash) *wire.ShaHash {
	// dupes can screw things up; CVE-2012-2459. check for them
	if left != nil && right != nil && left.IsEqual(right) {
		fmt.Printf("DUP HASH CRASH")
		return nil
	}
	// if left child is nil, output nil.  Need this for hard mode.
	if left == nil {
		return nil
	}
	// if right is nil, hash left with itself
	if right == nil {
		right = left
	}

	// Concatenate the left and right nodes
	var sha [64]byte
	copy(sha[:32], left[:])
	copy(sha[32:], right[:])

	newSha := wire.DoubleSha256SH(sha[:])
	return &newSha
}

type merkleNode struct {
	p uint32        // position in the binary tree
	h *wire.ShaHash // hash
}

// given n merkle leaves, how deep is the tree?
// iterate shifting left until greater than n
func treeDepth(n uint32) (e uint8) {
	for ; (1 << e) < n; e++ {
	}
	return
}

// smallest power of 2 that can contain n
func nextPowerOfTwo(n uint32) uint32 {
	return 1 << treeDepth(n) // 2^exponent
}

// check if a node is populated based on node position and size of tree
func inDeadZone(pos, size uint32) bool {
	msb := nextPowerOfTwo(size)
	last := size - 1      // last valid position is 1 less than size
	if pos > (msb<<1)-2 { // greater than root; not even in the tree
		fmt.Printf(" ?? greater than root ")
		return true
	}
	h := msb
	for pos >= h {
		h = h>>1 | msb
		last = last>>1 | msb
	}
	return pos > last
}

// take in a merkle block, parse through it, and return txids indicated
// If there's any problem return an error.  Checks self-consistency only.
// doing it with a stack instead of recursion.  Because...
// OK I don't know why I'm just not in to recursion OK?
func checkMBlock(m *wire.MsgMerkleBlock) ([]*wire.ShaHash, error) {
	if m.Transactions == 0 {
		return nil, fmt.Errorf("No transactions in merkleblock")
	}
	if len(m.Flags) == 0 {
		return nil, fmt.Errorf("No flag bits")
	}
	var s []merkleNode    // the stack
	var r []*wire.ShaHash // slice to return; txids we care about

	// set initial position to root of merkle tree
	msb := nextPowerOfTwo(m.Transactions) // most significant bit possible
	pos := (msb << 1) - 2                 // current position in tree

	var i uint8 // position in the current flag byte
	var tip int
	// main loop
	for {
		tip = len(s) - 1 // slice position of stack tip
		// First check if stack operations can be performed
		// is stack one filled item?  that's complete.
		if tip == 0 && s[0].h != nil {
			if s[0].h.IsEqual(&m.Header.MerkleRoot) {
				return r, nil
			}
			return nil, fmt.Errorf("computed root %s but expect %s\n",
				s[0].h.String(), m.Header.MerkleRoot.String())
		}
		// is current position in the tree's dead zone? partial parent
		if inDeadZone(pos, m.Transactions) {
			// create merkle parent from single side (left)
			s[tip-1].h = MakeMerkleParent(s[tip].h, nil)
			s = s[:tip]          // remove 1 from stack
			pos = s[tip-1].p | 1 // move position to parent's sibling
			continue
		}
		// does stack have 3+ items? and are last 2 items filled?
		if tip > 1 && s[tip-1].h != nil && s[tip].h != nil {
			//fmt.Printf("nodes %d and %d combine into %d\n",
			//	s[tip-1].p, s[tip].p, s[tip-2].p)
			// combine two filled nodes into parent node
			s[tip-2].h = MakeMerkleParent(s[tip-1].h, s[tip].h)
			// remove children
			s = s[:tip-1]
			// move position to parent's sibling
			pos = s[tip-2].p | 1
			continue
		}

		// no stack ops to perform, so make new node from message hashes
		if len(m.Hashes) == 0 {
			return nil, fmt.Errorf("Ran out of hashes at position %d.", pos)
		}
		if len(m.Flags) == 0 {
			return nil, fmt.Errorf("Ran out of flag bits.")
		}
		var n merkleNode // make new node
		n.p = pos        // set current position for new node

		if pos&msb != 0 { // upper non-txid hash
			if m.Flags[0]&(1<<i) == 0 { // flag bit says fill node
				n.h = m.Hashes[0]       // copy hash from message
				m.Hashes = m.Hashes[1:] // pop off message
				if pos&1 != 0 {         // right side; ascend
					pos = pos>>1 | msb
				} else { // left side, go to sibling
					pos |= 1
				}
			} else { // flag bit says skip; put empty on stack and descend
				pos = (pos ^ msb) << 1 // descend to left
			}
			s = append(s, n) // push new node on stack
		} else { // bottom row txid; flag bit indicates tx of interest
			if pos >= m.Transactions {
				// this can't happen because we check deadzone above...
				return nil, fmt.Errorf("got into an invalid txid node")
			}
			n.h = m.Hashes[0]           // copy hash from message
			m.Hashes = m.Hashes[1:]     // pop off message
			if m.Flags[0]&(1<<i) != 0 { //txid of interest
				r = append(r, n.h)
			}
			if pos&1 == 0 { // left side, go to sibling
				pos |= 1
			} // if on right side we don't move; stack ops will move next
			s = append(s, n) // push new node onto the stack
		}

		// done with pushing onto stack; advance flag bit
		i++
		if i == 8 { // move to next byte
			i = 0
			m.Flags = m.Flags[1:]
		}
	}
	return nil, fmt.Errorf("ran out of things to do?")
}

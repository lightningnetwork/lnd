package elkrem

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
)

/* elkrem is a simpler alternative to the 64 dimensional sha-chain.
it's basically a reverse merkle tree.  If we want to provide 2**64 possible
hashes, this requires a worst case computation of 63 hashes for the
sender, and worst-case storage of 64 hashes for the receiver.

The operations are left hash L() and right hash R(), which are
hash(parent)  and  hash(parent, 1)  respectively.  (concatenate one byte)

Here is a shorter example of a tree with 8 leaves and 15 total nodes.

The sender first computes the bottom left leaf 0b0000.  This is
L(L(L(L(root)))).  The receiver stores leaf 0.

Next the sender computes 0b0001.  R(L(L(L(root)))).  Receiver stores.
Next sender computes 0b1000 (8).  L(L(L(root))).  Receiver stores this, and
discards leaves 0b0000 and 0b0001, as they have the parent node 8.

For total hashes (2**h)-1 requires a tree of height h.

Sender:
as state, must store 1 hash (root) and current index (h bits)
to move to the next index, compute at most h hashes.

Receiver:
as state, must store at most h+1 hashes and the index of each hash (h*(h+1)) bits
to compute a previous index, compute at most h hashes.
*/

// You can calculate h from i but I can't figure out how without taking
// O(i) ops.  Feels like there should be a clever O(h) way.  1 byte, whatever.
type ElkremNode struct {
	h   uint8         // height of this node
	i   uint64        // index (ith node)
	sha *wire.ShaHash // hash
}
type ElkremSender struct {
	treeHeight uint8         // height of tree (size is 2**height -1 )
	current    uint64        // last sent hash index
	maxIndex   uint64        // top of the tree
	root       *wire.ShaHash // root hash of the tree
}
type ElkremReceiver struct {
	current    uint64       // last received index (actually don't need it?)
	treeHeight uint8        // height of tree (size is 2**height -1 )
	s          []ElkremNode // store of received hashes, max size = height
}

func LeftSha(in wire.ShaHash) wire.ShaHash {
	return wire.DoubleSha256SH(in.Bytes()) // left is sha(sha(in))
}
func RightSha(in wire.ShaHash) wire.ShaHash {
	return wire.DoubleSha256SH(append(in.Bytes(), 0x01)) // sha(sha(in, 1))
}

// iterative descent of sub-tree. w = hash number you want. i = input index
// h = height of input index. sha = input hash
func descend(w, i uint64, h uint8, sha wire.ShaHash) (wire.ShaHash, error) {
	for w < i {
		if w <= i-(1<<h) { // left
			sha = LeftSha(sha)
			i = i - (1 << h) // left descent reduces index by 2**h
		} else { // right
			sha = RightSha(sha)
			i-- // right descent reduces index by 1
		}
		if h == 0 { // avoid underflowing h
			break
		}
		h-- // either descent reduces height by 1
	}
	if w != i { // somehow couldn't / didn't end up where we wanted to go
		return sha, fmt.Errorf("can't generate index %d from %d", w, i)
	}
	return sha, nil
}

// Creates an Elkrem Sender from a root hash and tree height
func NewElkremSender(th uint8, r wire.ShaHash) ElkremSender {
	var e ElkremSender
	e.root = &r
	e.treeHeight = th
	// set max index based on tree height
	for j := uint8(0); j <= e.treeHeight; j++ {
		e.maxIndex = e.maxIndex<<1 | 1
	}
	e.maxIndex-- // 1 less than 2**h
	return e
}

// Creates an Elkrem Receiver from a tree height
func NewElkremReceiver(th uint8) ElkremReceiver {
	var e ElkremReceiver
	e.treeHeight = th
	return e
}

// Next() increments the index to the next hash and outputs it
func (e *ElkremSender) Next() (*wire.ShaHash, error) {
	sha, err := e.AtIndex(e.current)
	if err != nil {
		return nil, err
	}
	// increment index for next time
	e.current++
	return sha, nil
}

// w is the wanted index, i is the root index
func (e *ElkremSender) AtIndex(w uint64) (*wire.ShaHash, error) {
	out, err := descend(w, e.maxIndex, e.treeHeight, *e.root)
	return &out, err
}

func (e *ElkremReceiver) AddNext(sha *wire.ShaHash) error {
	// note: careful about atomicity / disk writes here
	var n ElkremNode
	n.sha = sha
	t := len(e.s) - 1                    // top of stack
	if t > 0 && e.s[t-1].h == e.s[t].h { // top 2 elements are equal height
		// next node must be parent; verify and remove children
		n.h = e.s[t].h + 1             // assign height
		l := LeftSha(*sha)             // calc l child
		r := RightSha(*sha)            // calc r child
		if !e.s[t-1].sha.IsEqual(&l) { // test l child
			return fmt.Errorf("left child doesn't match, expect %s got %s",
				e.s[t-1].sha.String(), l.String())
		}
		if !e.s[t].sha.IsEqual(&r) { // test r child
			return fmt.Errorf("right child doesn't match, expect %s got %s",
				e.s[t].sha.String(), r.String())
		}
		e.s = e.s[:len(e.s)-2] // l and r children OK, remove them
	} // if that didn't happen, height defaults to 0
	e.current++          // increment current index
	n.i = e.current      // set new node to that incremented index
	e.s = append(e.s, n) // append new node to stack
	return nil
}
func (e *ElkremReceiver) AtIndex(w uint64) (*wire.ShaHash, error) {
	var out ElkremNode      // node we will eventually return
	for _, n := range e.s { // go through stack
		if w <= n.i { // found one bigger than or equal to what we want
			out = n
			break
		}
	}
	if out.sha == nil { // didn't find anything
		return nil, fmt.Errorf("receiver has max %d, less than requested %d",
			e.s[len(e.s)-1].i, w)
	}
	sha, err := descend(w, out.i, out.h, *out.sha)
	return &sha, err
}

package shachain

import (
	"crypto/sha256"
	"errors"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
)

// element represent the entity which contains the hash and
// corresponding to it index. By comparing two indexes we may change hash in
// such way to derive another element.
type element struct {
	index index
	hash  chainhash.Hash
}

// newElementFromStr creates new element by the given hash string.
func newElementFromStr(s string, index index) (*element, error) {
	hash, err := hashFromString(s)
	if err != nil {
		return nil, err
	}

	return &element{
		index: index,
		hash:  *hash,
	}, nil
}

// derive used to get one shachain element from another by applying a series of
// bit flipping and hashing operations based on an index.
func (e *element) derive(toIndex index) (*element, error) {
	fromIndex := e.index

	positions, err := fromIndex.deriveBitTransformations(toIndex)
	if err != nil {
		return nil, err
	}

	buf := e.hash.CloneBytes()
	for _, position := range positions {
		// Flip the bit and then hash the current state.
		byteNumber := position / 8
		bitNumber := position % 8

		buf[byteNumber] ^= (1 << bitNumber)

		h := sha256.Sum256(buf)
		buf = h[:]
	}

	hash, err := chainhash.NewHash(buf)
	if err != nil {
		return nil, err
	}

	return &element{
		index: toIndex,
		hash:  *hash,
	}, nil
}

// isEqual checks elements equality.
func (first *element) isEqual(second *element) bool {
	return (first.index == second.index) &&
		(&first.hash).IsEqual(&second.hash)
}

const (
	// maxHeight is used to determine the the maximum allowable index and
	// the length of array which should be stored in order to derive all
	// previous hashes by index, this array also known as buckets.
	maxHeight uint8 = 48

	// rootIndex is an index which corresponds to the root hash.
	rootIndex index = 0
)

// startIndex is an index of first element.
var startIndex index = (1 << maxHeight) - 1

// index is an number which identifies the hash number and serve as the way to
// determine which operation under hash we should made in order to derive one
// hash from another. index initialized with start index value and than
// decreases down to zero.
type index uint64

// newIndex is used to create index instance. The inner operations with
// index implies that index decreasing from some max number to zero, but for
// simplicity and backward compatibility with previous logic it was transformed
// to work in opposite way.
func newIndex(v uint64) index {
	return startIndex - index(v)
}

// deriveBitTransformations function checks that 'to' index is derivable from
// 'from' index by checking the indexes prefixes and then returns the bit
// positions where the zeroes should be changed to ones in order for the indexes
// to become the same. This set of bits is needed in order to derive one hash
// from another.
//
// NOTE: The index 'to' is derivable from index 'from' iff index 'from' lies
// left and above index 'to' on graph below, for example:
// 1. 7(0b111) -> 7 
// 2. 6(0b110) -> 6,7
// 3. 5(0b101) -> 5
// 4. 4(0b100) -> 4,5,6,7
// 5. 3(0b011) -> 3
// 6. 2(0b010) -> 2, 3
// 7. 1(0b001) -> 1
//
//    ^ bucket number
//    |
//  3 |   x
//    |   |
//  2 |   |               x
//    |   |               |
//  1 |   |       x       |       x
//    |   |       |       |       |
//  0 |   |   x   |   x   |   x   |   x
//    |   |   |   |   |   |   |   |   |
//    +---|---|---|---|---|---|---|---|---> index
//        0   1   2   3   4   5   6   7
//
func (from index) deriveBitTransformations(to index) ([]uint8, error) {
	var positions []uint8

	if from == to {
		return positions, nil
	}

	//	+ --------------- +
	// 	| №  | from | to  |
	//	+ -- + ---- + --- +
	//	| 48 |	 1  |  1  |
	//	| 47 |	 0  |  0  | [48-5] - same part of 'from' and 'to'
	//	| 46 |   0  |  0  |	    indexes which also is called prefix.
	//		....
	//	|  5 |	 1  |  1  |
	//	|  4 |	 0  |  1  | <--- position after which indexes becomes
	//	|  3 |   0  |  0  |	 different, after this position
	//	|  2 |   0  |  1  |	 bits in 'from' index all should be
	//	|  1 |   0  |  0  |	 zeros or such indexes considered to be
	//	|  0 |   0  |  1  |	 not derivable.
	//	+ -- + ---- + --- +
	zeros := countTrailingZeros(from)
	if uint64(from) != getPrefix(to, zeros) {
		return nil, errors.New("prefixes are different - indexes " +
			"aren't derivable")
	}

	// The remaining part of 'to' index represents the positions which we
	// will use then in order to derive one element from another.
	for position := zeros - 1; ; position-- {
		if getBit(to, position) == 1 {
			positions = append(positions, position)
		}

		if position == 0 {
			break
		}
	}

	return positions, nil
}

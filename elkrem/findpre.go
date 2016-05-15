package elkrem

import (
	"bytes"
	"fmt"

	"github.com/roasbeef/btcutil"
)

/* findpre - find the pre-image for a given hash

The worst (?) has happened and your channel counterparty has broadcast an old,
invalid state.  That's bad.  But what, that means you get to take all the money.
This is the fun part of the channel enforcement mechanism.

The old transaction they broadcast has an nLockTime field which provides a hint
about which state number, and which hash index, was used.  Bitcoin's nLockTime
field counts as blocks from 0 to 499,999,999 (which would happen somtime after
10,000 CE) and as a unix time from 500,000,000 up to 4,294,967,296 (which is
much sooner, in 2106).  There is some extra space we can use here in both cases,
though the unix time gives us much more.  nLockTimes from 500,000,000 to
1,036,870,912 are safe, representing dates from 1985 to 2002, which are before
any possible bitcoin transaction.  1,036,870,912 is 500,000,000 plus
536,870,912, and 536,870,912 is 2^29, so we have 29 bits of free space.

After subtracting 500,000,000, the remaning lowest 29 bits are the bits of the
index within the elkrem receiver used in that transaction.  That way, even with
a trillion previous channel states (2^40) we will only need to search through
2048 possible branches to find the right pre-image.  In most cases, there will
be fewer than 536,870,912 previous states and we can seek directly to the
correct pre-image.
*/

func (e *ElkremReceiver) FindPre(
	target [20]byte, timeHint uint32) (*[20]byte, error) {

	maxUint32 := uint32((1 << 32) - 1)
	minTime := uint32(500000000)
	hintRange := uint32((1 << 29) - 1)

	// a timeHint of 2^32 (4294967296) means we don't have a timeHint.
	if timeHint == maxUint32 {
		return nil, fmt.Errorf("no timeHint")
	}
	// valid timeHint range is 500M to 500M + 2^29
	if timeHint < minTime || timeHint > minTime+hintRange {
		return nil, fmt.Errorf("timeHint %d out of range (500M - ~1G)", timeHint)
	}
	indexHint := uint64(timeHint - minTime)
	maxIndex := e.s[len(e.s)-1].i // highest index we have

	if indexHint > maxIndex { // we can't derive needed index
		return nil, fmt.Errorf("hint index %d greater than max index %d",
			indexHint, maxIndex)
	}

	// iterate though, adding 2^29 each time.
	// there is some redundancy here when you have a large number of guesses
	// to go through, so this could be optimized later.
	for guess := indexHint; guess < maxIndex; guess += uint64(hintRange) {
		sha, err := e.AtIndex(guess) // generate preimage
		if err != nil {
			return nil, err
		}
		var truncatedSha [20]byte
		copy(truncatedSha[:], sha.Bytes()) // copy into 20 byte array

		checkHash := btcutil.Hash160(truncatedSha[:]) // hash and compare
		if bytes.Equal(target[:], checkHash) {        // matches hash, return
			return &truncatedSha, nil
		}
	}
	// got through the loop without finding anything.
	return nil, fmt.Errorf("Couldn't find preimage of %x. timeHint %d bad?",
		target, timeHint)
}

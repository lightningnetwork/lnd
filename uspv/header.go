/* this is blockchain technology.  Well, except without the blocks.
Really it's header chain technology.
The blocks themselves don't really make a chain.  Just the headers do.
*/

package uspv

import (
	"io"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/roasbeef/btcd/blockchain"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/wire"
)

// blockchain settings.  These are kindof bitcoin specific, but not contained in
// chaincfg.Params so they'll go here.  If you're into the [ANN]altcoin scene,
// you may want to paramaterize these constants.
const (
	targetTimespan      = time.Hour * 24 * 14
	targetSpacing       = time.Minute * 10
	epochLength         = int32(targetTimespan / targetSpacing) // 2016
	maxDiffAdjust       = 4
	minRetargetTimespan = int64(targetTimespan / maxDiffAdjust)
	maxRetargetTimespan = int64(targetTimespan * maxDiffAdjust)
)

/* checkProofOfWork verifies the header hashes into something
lower than specified by the 4-byte bits field. */
func checkProofOfWork(header wire.BlockHeader, p *chaincfg.Params) bool {
	target := blockchain.CompactToBig(header.Bits)

	// The target must more than 0.  Why can you even encode negative...
	if target.Sign() <= 0 {
		log.Printf("block target %064x is neagtive(??)\n", target.Bytes())
		return false
	}
	// The target must be less than the maximum allowed (difficulty 1)
	if target.Cmp(p.PowLimit) > 0 {
		log.Printf("block target %064x is "+
			"higher than max of %064x", target, p.PowLimit.Bytes())
		return false
	}
	// The header hash must be less than the claimed target in the header.
	blockHash := header.BlockSha()
	hashNum := blockchain.ShaHashToBig(&blockHash)
	if hashNum.Cmp(target) > 0 {
		log.Printf("block hash %064x is higher than "+
			"required target of %064x", hashNum, target)
		return false
	}
	return true
}

/* calcDiff returns a bool given two block headers.  This bool is
true if the correct dificulty adjustment is seen in the "next" header.
Only feed it headers n-2016 and n-1, otherwise it will calculate a difficulty
when no adjustment should take place, and return false.
Note that the epoch is actually 2015 blocks long, which is confusing. */
func calcDiffAdjust(start, end wire.BlockHeader, p *chaincfg.Params) uint32 {
	duration := end.Timestamp.UnixNano() - start.Timestamp.UnixNano()
	if duration < minRetargetTimespan {
		log.Printf("whoa there, block %s off-scale high 4X diff adjustment!",
			end.BlockSha().String())
		duration = minRetargetTimespan
	} else if duration > maxRetargetTimespan {
		log.Printf("Uh-oh! block %s off-scale low 0.25X diff adjustment!\n",
			end.BlockSha().String())
		duration = maxRetargetTimespan
	}

	// calculation of new 32-byte difficulty target
	// first turn the previous target into a big int
	prevTarget := blockchain.CompactToBig(start.Bits)
	// new target is old * duration...
	newTarget := new(big.Int).Mul(prevTarget, big.NewInt(duration))
	// divided by 2 weeks
	newTarget.Div(newTarget, big.NewInt(int64(targetTimespan)))

	// clip again if above minimum target (too easy)
	if newTarget.Cmp(p.PowLimit) > 0 {
		newTarget.Set(p.PowLimit)
	}

	// calculate and return 4-byte 'bits' difficulty from 32-byte target
	return blockchain.BigToCompact(newTarget)
}

func CheckHeader(r io.ReadSeeker, height int32, p *chaincfg.Params) bool {
	var err error
	var cur, prev, epochStart wire.BlockHeader
	// don't try to verfy the genesis block.  That way madness lies.
	if height == 0 {
		return true
	}
	// initial load of headers
	// load epochstart, previous and current.
	// get the header from the epoch start, up to 2016 blocks ago
	_, err = r.Seek(int64(80*(height-(height%epochLength))), os.SEEK_SET)
	if err != nil {
		log.Printf(err.Error())
		return false
	}
	err = epochStart.Deserialize(r)
	if err != nil {
		log.Printf(err.Error())
		return false
	}
	//	log.Printf("start epoch at height %d ", height-(height%epochLength))

	// seek to n-1 header
	_, err = r.Seek(int64(80*(height-1)), os.SEEK_SET)
	if err != nil {
		log.Printf(err.Error())
		return false
	}
	// read in n-1
	err = prev.Deserialize(r)
	if err != nil {
		log.Printf(err.Error())
		return false
	}
	// seek to curHeight header and read in
	_, err = r.Seek(int64(80*(height)), os.SEEK_SET)
	if err != nil {
		log.Printf(err.Error())
		return false
	}
	err = cur.Deserialize(r)
	if err != nil {
		log.Printf(err.Error())
		return false
	}

	// get hash of n-1 header
	prevHash := prev.BlockSha()
	// check if headers link together.  That whole 'blockchain' thing.
	if prevHash.IsEqual(&cur.PrevBlock) == false {
		log.Printf("Headers %d and %d don't link.\n",
			height-1, height)
		log.Printf("%s - %s",
			prev.BlockSha().String(), cur.BlockSha().String())
		return false
	}
	rightBits := epochStart.Bits // normal, no adjustment; Dn = Dn-1
	// see if we're on a difficulty adjustment block
	if (height)%epochLength == 0 {
		// if so, check if difficulty adjustment is valid.
		// That whole "controlled supply" thing.
		// calculate diff n based on n-2016 ... n-1
		rightBits = calcDiffAdjust(epochStart, prev, p)
		// done with adjustment, save new ephochStart header
		epochStart = cur
		log.Printf("Update epoch at height %d", height)
	} else { // not a new epoch
		// if on testnet, check for difficulty nerfing
		if p.ResetMinDifficulty && cur.Timestamp.After(
			prev.Timestamp.Add(targetSpacing*2)) {
			//	fmt.Printf("nerf %d ", curHeight)
			rightBits = p.PowLimitBits // difficulty 1
		}
		if cur.Bits != rightBits {
			log.Printf("Block %d %s incorrect difficuly.  Read %x, expect %x\n",
				height, cur.BlockSha().String(), cur.Bits, rightBits)
			return false
		}
	}

	// check if there's a valid proof of work.  That whole "Bitcoin" thing.
	if !checkProofOfWork(cur, p) {
		log.Printf("Block %d Bad proof of work.\n", height)
		return false
	}

	return true // it must have worked if there's no errors and got to the end.
}

/* checkrange verifies a range of headers.  it checks their proof of work,
difficulty adjustments, and that they all link in to each other properly.
This is the only blockchain technology in the whole code base.
Returns false if anything bad happens.  Returns true if the range checks
out with no errors. */
func CheckRange(r io.ReadSeeker, first, last int32, p *chaincfg.Params) bool {
	for i := first; i <= last; i++ {
		if !CheckHeader(r, i, p) {
			return false
		}
	}
	return true // all good.
}

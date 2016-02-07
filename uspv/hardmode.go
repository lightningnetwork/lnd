package uspv

import (
	"fmt"
	"log"
	"os"

	"github.com/btcsuite/btcd/wire"
)

// BlockRootOK checks that all the txs in the block match the merkle root.
// Only checks merkle root; it doesn't look at txs themselves.
func BlockRootOK(blk wire.MsgBlock) bool {
	fmt.Printf("BlockRootOK for block %s\n", blk.BlockSha().String())

	var shas []*wire.ShaHash
	for _, tx := range blk.Transactions { // make slice of txids
		nSha := tx.TxSha()
		shas = append(shas, &nSha)
	}

	// pad out tx slice to get the full tree base
	neededLen := int(nextPowerOfTwo(uint32(len(shas)))) // kindof ugly
	for len(shas) < neededLen {
		shas = append(shas, nil)
	}
	fmt.Printf("Padded %d txs in block to %d\n",
		len(blk.Transactions), len(shas))

	// calculate merkle root. Terse, eh?
	for len(shas) > 1 {
		shas = append(shas[2:], MakeMerkleParent(shas[0], shas[1]))
	}

	fmt.Printf("calc'd mroot %s, %s in header\n",
		shas[0].String(), blk.Header.MerkleRoot.String())

	if blk.Header.MerkleRoot.IsEqual(shas[0]) {
		return true
	}
	return false
}

func (s *SPVCon) IngestBlock(m *wire.MsgBlock) {
	ok := BlockRootOK(*m) // check self-consistency
	if !ok {
		fmt.Printf("block %s not OK!!11\n", m.BlockSha().String())
		return
	}

	return
}

func (s *SPVCon) AskForOneBlock(h int32) error {
	var hdr wire.BlockHeader
	var err error

	dbTip := int32(h)
	s.headerMutex.Lock() // seek to header we need
	_, err = s.headerFile.Seek(int64((dbTip)*80), os.SEEK_SET)
	if err != nil {
		return err
	}
	err = hdr.Deserialize(s.headerFile) // read header, done w/ file for now
	s.headerMutex.Unlock()              // unlock after reading 1 header
	if err != nil {
		log.Printf("header deserialize error!\n")
		return err
	}

	bHash := hdr.BlockSha()
	// create inventory we're asking for
	iv1 := wire.NewInvVect(wire.InvTypeBlock, &bHash)
	gdataMsg := wire.NewMsgGetData()
	// add inventory
	err = gdataMsg.AddInvVect(iv1)
	if err != nil {
		return err
	}

	s.outMsgQueue <- gdataMsg

	return nil
}

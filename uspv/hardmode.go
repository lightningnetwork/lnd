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
	var shas []*wire.ShaHash
	for _, tx := range blk.Transactions { // make slice of txids
		nSha := tx.TxSha()
		shas = append(shas, &nSha)
	}
	neededLen := int(nextPowerOfTwo(uint32(len(shas)))) // kindof ugly
	for len(shas) < neededLen {
		shas = append(shas, nil) // pad out tx slice to get the full tree base
	}
	for len(shas) > 1 { // calculate merkle root. Terse, eh?
		shas = append(shas[2:], MakeMerkleParent(shas[0], shas[1]))
	}
	return blk.Header.MerkleRoot.IsEqual(shas[0])
}

// IngestBlock is like IngestMerkleBlock but aralphic
// different enough that it's better to have 2 separate functions
func (s *SPVCon) IngestBlock(m *wire.MsgBlock) {
	var err error

	ok := BlockRootOK(*m) // check block self-consistency
	if !ok {
		fmt.Printf("block %s not OK!!11\n", m.BlockSha().String())
		return
	}

	var hah HashAndHeight
	select { // select here so we don't block on an unrequested mblock
	case hah = <-s.blockQueue: // pop height off mblock queue
		break
	default:
		log.Printf("Unrequested full block")
		return
	}

	newBlockSha := m.Header.BlockSha()
	if !hah.blockhash.IsEqual(&newBlockSha) {
		log.Printf("full block out of order error")
		return
	}

	// iterate through all txs in the block, looking for matches.
	// this is slow and can be sped up by doing in-ram filters client side.
	// kindof a pain to implement though and it's fast enough for now.
	for i, tx := range m.Transactions {
		hits, err := s.TS.Ingest(tx, hah.height)
		if err != nil {
			log.Printf("Incoming Tx error: %s\n", err.Error())
			return
		}
		if hits > 0 {
			log.Printf("block %d tx %d %s ingested and matches %d utxo/adrs.",
				hah.height, i, tx.TxSha().String(), hits)
		}
	}

	// write to db that we've sync'd to the height indicated in the
	// merkle block.  This isn't QUITE true since we haven't actually gotten
	// the txs yet but if there are problems with the txs we should backtrack.
	err = s.TS.SetDBSyncHeight(hah.height)
	if err != nil {
		log.Printf("full block sync error: %s\n", err.Error())
		return
	}
	fmt.Printf("ingested full block %s height %d OK\n",
		m.Header.BlockSha().String(), hah.height)

	if hah.final { // check sync end
		// don't set waitstate; instead, ask for headers again!
		// this way the only thing that triggers waitstate is asking for headers,
		// getting 0, calling AskForMerkBlocks(), and seeing you don't need any.
		// that way you are pretty sure you're synced up.
		err = s.AskForHeaders()
		if err != nil {
			log.Printf("Merkle block error: %s\n", err.Error())
			return
		}
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

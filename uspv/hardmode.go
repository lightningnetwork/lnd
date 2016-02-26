package uspv

import (
	"bytes"
	"fmt"
	"log"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/bloom"
)

var (
	WitMagicBytes = []byte{0x6a, 0x24, 0xaa, 0x21, 0xa9, 0xed}
)

// BlockRootOK checks for block self-consistency.
// If the block has no wintess txs, and no coinbase witness commitment,
// it only checks the tx merkle root.  If either a witness commitment or
// any witnesses are detected, it also checks that as well.
// Returns false if anything goes wrong, true if everything is fine.
func BlockOK(blk wire.MsgBlock) bool {
	var txids, wtxids []*wire.ShaHash // txids and wtxids
	// witMode true if any tx has a wintess OR coinbase has wit commit
	var witMode bool

	for _, tx := range blk.Transactions { // make slice of (w)/txids
		txid := tx.TxSha()
		wtxid := tx.WTxSha()
		if !witMode && !txid.IsEqual(&wtxid) {
			witMode = true
		}
		txids = append(txids, &txid)
		wtxids = append(wtxids, &wtxid)
	}

	var commitBytes []byte
	// try to extract coinbase witness commitment (even if !witMode)
	cb := blk.Transactions[0]                 // get coinbase tx
	for i := len(cb.TxOut) - 1; i >= 0; i-- { // start at the last txout
		if bytes.HasPrefix(cb.TxOut[i].PkScript, WitMagicBytes) &&
			len(cb.TxOut[i].PkScript) > 37 {
			// 38 bytes or more, and starts with WitMagicBytes is a hit
			commitBytes = cb.TxOut[i].PkScript[6:38]
			witMode = true // it there is a wit commit it must be valid
		}
	}

	if witMode { // witmode, so check witness tree
		// first find ways witMode can be disqualified
		if len(commitBytes) != 32 {
			// witness in block but didn't find a wintess commitment; fail
			log.Printf("block %s has witness but no witcommit",
				blk.BlockSha().String())
			return false
		}
		if len(cb.TxIn) != 1 {
			log.Printf("block %s coinbase tx has %d txins (must be 1)",
				blk.BlockSha().String(), len(cb.TxIn))
			return false
		}
		if len(cb.TxIn[0].Witness) != 1 {
			log.Printf("block %s coinbase has %d witnesses (must be 1)",
				blk.BlockSha().String(), len(cb.TxIn[0].Witness))
			return false
		}

		if len(cb.TxIn[0].Witness[0]) != 32 {
			log.Printf("block %s coinbase has %d byte witness nonce (not 32)",
				blk.BlockSha().String(), len(cb.TxIn[0].Witness[0]))
			return false
		}
		// witness nonce is the cb's witness, subject to above constraints
		witNonce, err := wire.NewShaHash(cb.TxIn[0].Witness[0])
		if err != nil {
			log.Printf("Witness nonce error: %s", err.Error())
			return false // not sure why that'd happen but fail
		}

		var empty [32]byte
		wtxids[0].SetBytes(empty[:]) // coinbase wtxid is 0x00...00

		// witness root calculated from wtixds
		witRoot := calcRoot(wtxids)

		calcWitCommit := wire.DoubleSha256SH(
			append(witRoot.Bytes(), witNonce.Bytes()...))

		// witness root given in coinbase op_return
		givenWitCommit, err := wire.NewShaHash(commitBytes)
		if err != nil {
			log.Printf("Witness root error: %s", err.Error())
			return false // not sure why that'd happen but fail
		}
		// they should be the same.  If not, fail.
		if !calcWitCommit.IsEqual(givenWitCommit) {
			log.Printf("Block %s witRoot error: calc %s given %s",
				blk.BlockSha().String(),
				calcWitCommit.String(), givenWitCommit.String())
			return false
		}
	}

	// got through witMode check so that should be OK;
	// check regular txid merkleroot.  Which is, like, trivial.
	return blk.Header.MerkleRoot.IsEqual(calcRoot(txids))
}

// calcRoot calculates the merkle root of a slice of hashes.
func calcRoot(hashes []*wire.ShaHash) *wire.ShaHash {
	for len(hashes) < int(nextPowerOfTwo(uint32(len(hashes)))) {
		hashes = append(hashes, nil) // pad out hash slice to get the full base
	}
	for len(hashes) > 1 { // calculate merkle root. Terse, eh?
		hashes = append(hashes[2:], MakeMerkleParent(hashes[0], hashes[1]))
	}
	return hashes[0]
}

func (ts *TxStore) Refilter() error {
	allUtxos, err := ts.GetAllUtxos()
	if err != nil {
		return err
	}
	filterElements := uint32(len(allUtxos) + len(ts.Adrs))

	ts.localFilter = bloom.NewFilter(filterElements, 0, 0, wire.BloomUpdateAll)

	for _, u := range allUtxos {
		ts.localFilter.AddOutPoint(&u.Op)
	}
	for _, a := range ts.Adrs {
		ts.localFilter.Add(a.PkhAdr.ScriptAddress())
	}

	msg := ts.localFilter.MsgFilterLoad()
	fmt.Printf("made %d element filter: %x\n", filterElements, msg.Filter)
	return nil
}

// IngestBlock is like IngestMerkleBlock but aralphic
// different enough that it's better to have 2 separate functions
func (s *SPVCon) IngestBlock(m *wire.MsgBlock) {
	var err error
	//	var buf bytes.Buffer
	//	m.SerializeWitness(&buf)
	//	fmt.Printf("block hex %x\n", buf.Bytes())
	//	for _, tx := range m.Transactions {
	//		fmt.Printf("wtxid: %s\n", tx.WTxSha())
	//		fmt.Printf(" txid: %s\n", tx.TxSha())
	//		fmt.Printf("%d %s", i, TxToString(tx))
	//	}
	ok := BlockOK(*m) // check block self-consistency
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

	fPositive := 0 // local filter false positives
	reFilter := 10 // after that many false positives, regenerate filter.
	// 10?  Making it up.  False positives have disk i/o cost, and regenning
	// the filter also has costs.  With a large local filter, false positives
	// should be rare.

	// iterate through all txs in the block, looking for matches.
	// use a local bloom filter to ignore txs that don't affect us
	for _, tx := range m.Transactions {
		utilTx := btcutil.NewTx(tx)
		if s.TS.localFilter.MatchTxAndUpdate(utilTx) {
			hits, err := s.TS.Ingest(tx, hah.height)
			if err != nil {
				log.Printf("Incoming Tx error: %s\n", err.Error())
				return
			}
			if hits > 0 {
				// log.Printf("block %d tx %d %s ingested and matches %d utxo/adrs.",
				//	hah.height, i, tx.TxSha().String(), hits)
			} else {
				fPositive++ // matched filter but no hits
			}
		}
	}

	if fPositive > reFilter {
		fmt.Printf("%d filter false positives in this block\n", fPositive)
		err = s.TS.Refilter()
		if err != nil {
			log.Printf("Refilter error: %s\n", err.Error())
			return
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

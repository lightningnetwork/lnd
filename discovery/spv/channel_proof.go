package spv

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

var (
	// ErrNoChannels is an error returned when a channel proof request
	// doesn't include any channels.
	ErrNoChannels = errors.New("no channels")

	// ErrChannelInvalidTxIndex is an error returned when we come across an
	// invalid transaction index when processing a channel proof request.
	ErrChannelInvalidTxIndex = errors.New("invalid transaction index")

	// ErrProofMissingTx is an error returned when we attempt to extract a
	// proof from one we've previously cached, but the cached one doesn't
	// contain enough information to do so.
	ErrProofMissingTx = errors.New("proof does not contain transaction")

	// ErrNoProof is an error returned we we attempt to locate a proof
	// within the cache for a block that hasn't had one computed yet.
	ErrNoProof = errors.New("proof not found")
)

// Chain provides information about the chain in order to contruct/verify a
// channel proof.
type Chain interface {
	// GetBlockHash returns the hash of the block with the given height.
	GetBlockHash(int32) (*chainhash.Hash, error)

	// GetBlockHeader returns the header of the block with the given hash.
	GetBlockHeader(*chainhash.Hash) (*wire.BlockHeader, error)

	// GetBlock returns the block with the given hash.
	GetBlock(*chainhash.Hash) (*wire.MsgBlock, error)
}

// ChannelProofRequest encapsulates a request to receive an aggregatable channel
// proof for a set of channels dictated by their block height and the
// transaction indices of each channel.
type ChannelProofRequest struct {
	// Transactions is the list of transaction indices that correspond to
	// channels within the same block to request a merkle proof for.
	Transactions []uint32

	// BlockHeight is the height of the block in which the transactions
	// being requested should exist.
	BlockHeight uint32
}

// String returns a human readable description of ChannelProofRequest.
func (r *ChannelProofRequest) String() string {
	return fmt.Sprintf("(height=%v, txs=%v)", r.BlockHeight, r.Transactions)
}

// ChannelProof is an efficient aggregated format for merkle proofs that span
// over multiple transactions.
type ChannelProof struct {
	// MerkleProof contains the list of hashes required to verify the proof
	// and arrive at the merkle root.
	MerkleProof []*chainhash.Hash

	// Transactions are the raw transactions for which the proof was
	// requested for.
	Transactions []*wire.MsgTx

	// NumTransactions is the number of transactions in the block that
	// corresponds to the proof.
	NumTransactions uint16
}

// CachedChannelProof is a wrapper over ChannelProof that also includes the
// corresponding indices for each transaction the proof commits to, which is
// helpful when merging a new proof with a cached one.
type CachedChannelProof struct {
	*ChannelProof

	// TxIdxs is the set of corresponding indices for each transaction the
	// proof commits to.
	TxIdxs []uint32
}

// ProofCache caches aggregatable channel proofs over blocks.
type ProofCache interface {
	// PutProof inserts a channel proof for a block.
	PutProof(uint32, *CachedChannelProof) error

	// GetProof retrieves a channel proof for a block. If a proof does not
	// exist, then ErrNoProof is returned.
	GetProof(uint32) (*CachedChannelProof, error)
}

// proofInfo is a helper struct to gather the parameters required to merge two
// channel proofs.
type proofInfo struct {
	// txs is the set of transactions a proof commits to.
	txs []*wire.MsgTx

	// idxs contains the corresponding index in a block of each transaction
	// the proof commits to.
	idxs []uint32
}

// mergeProofInfo merges two lists of transactions together sorted in increasing
// order of their index in a block.
func mergeProofInfo(a, b proofInfo) proofInfo {
	var merged proofInfo
	aIdx, bIdx := 0, 0

	// We'll compare transactions by their index in a block until we no
	// longer have enough transactions.
	for aIdx < len(a.idxs) && bIdx < len(b.idxs) {
		switch {
		// A contains the next lowest index, so advance it.
		case a.idxs[aIdx] < b.idxs[bIdx]:
			merged.idxs = append(merged.idxs, a.idxs[aIdx])
			merged.txs = append(merged.txs, a.txs[aIdx])
			aIdx++

		// B contains the next lowest index, so advance it.
		case a.idxs[aIdx] > b.idxs[bIdx]:
			merged.idxs = append(merged.idxs, b.idxs[bIdx])
			merged.txs = append(merged.txs, b.txs[bIdx])
			bIdx++

		// A and B contain the same index, so add it once and advance
		// them.
		default:
			merged.idxs = append(merged.idxs, a.idxs[aIdx])
			merged.txs = append(merged.txs, a.txs[aIdx])
			aIdx++
			bIdx++
		}
	}

	// Add any remaining indices.
	for aIdx < len(a.idxs) {
		merged.idxs = append(merged.idxs, a.idxs[aIdx])
		merged.txs = append(merged.txs, a.txs[aIdx])
		aIdx++
	}
	for bIdx < len(b.idxs) {
		merged.idxs = append(merged.idxs, b.idxs[bIdx])
		merged.txs = append(merged.txs, b.txs[bIdx])
		bIdx++
	}

	return merged
}

// calcTreeHeight calculates the height of a tree with N elements.
func calcTreeHeight(n uint) uint {
	height := uint(0)
	for (1 << height) < n {
		height++
	}
	return height
}

// extractMerkleProof extracts a merkle proof for the transactions with the
// given indices from the merkle tree.
func extractMerkleProof(txIdxs []uint32, numTxsInBlock uint16,
	merkleTree []*chainhash.Hash) []*chainhash.Hash {

	// We'll visit the relevant indices of each level of the tree starting
	// from the leaves until we arrive at the root, adding any intermediate
	// hashes required.
	visit := make([]uint32, len(txIdxs))
	copy(visit, txIdxs)

	var proof []*chainhash.Hash
	treeHeight := calcTreeHeight(uint(numTxsInBlock))
	for height := uint(0); height < treeHeight; height++ {
		// We'll be reusing the same slice, so make sure we don't visit
		// indices that belong to the next level.
		visitLen := len(visit)
		for i, hashIdx := range visit[:visitLen] {
			// To determine which nodes of the tree we need to visit
			// at the next level, i.e., our parent, we'll need to
			// determine our sibling first.
			siblingIdx := hashIdx ^ 1
			switch {
			// We'll need to visit our sibling, so don't include its
			// hash in the proof.
			case i < len(visit)-1 && visit[i+1] == siblingIdx:

			// If we already visited this hash as a sibling of a
			// previous one, then we can skip it.
			case i > 0 && visit[i-1] == siblingIdx:
				continue

			// The sibling is the same as the current hash we're
			// visiting if it corresponds to the last transaction of
			// a block with an odd number of transactions, so
			// there's no need to include it in the proof.
			case height == 0 && numTxsInBlock%2 != 0 &&
				hashIdx == uint32(numTxsInBlock-1):

			// Our sibling needs to be included in the proof.
			default:
				proof = append(proof, merkleTree[siblingIdx])
			}

			// With the sibling found, we'll visit their parent
			// during the next level iteration.
			parentIdx := (hashIdx >> 1) | (1 << treeHeight)
			visit = append(visit, parentIdx)
		}

		// Now that we've visited all of the hashes at the current
		// level, we can move on to the next.
		visit = visit[visitLen:]
	}

	return proof
}

// constructMerkleTree constructs a partial merkle tree from a merkle proof for
// the given transaction indices.
func constructMerkleTree(proof *ChannelProof,
	txIdxs []uint32) ([]*chainhash.Hash, error) {

	// Construct a merkle tree that can hold up to the number of
	// transactions in the block the proof corresponds to.
	treeHeight := calcTreeHeight(uint(proof.NumTransactions))
	tree := make([]*chainhash.Hash, (1<<treeHeight)*2-1)

	// We'll visit the relevant indices of each level of the tree starting
	// from the leaves until we arrive at the root, adding any intermediate
	// hashes required. The relevant indices at the leaf level correspond to
	// the transactions the proof commits to.
	visit := make([]uint32, 0, len(txIdxs))
	for i, txIdx := range txIdxs {
		txHash := proof.Transactions[i].TxHash()
		tree[txIdx] = &txHash
		visit = append(visit, txIdx)
	}

	// We'll use an index to keep track of the next merkle proof hash.
	nextProofIdx := 0
	for height := uint(0); height < treeHeight; height++ {
		// We'll be reusing the same slice, so make sure we don't visit
		// indices that belong to the next level.
		visitLen := len(visit)
		for i, hashIdx := range visit[:visitLen] {
			// To determine which nodes of the tree we need to visit
			// at the next level, i.e., our parent, we'll need to
			// determine our sibling first.
			siblingIdx := hashIdx ^ 1
			var sibling *chainhash.Hash
			switch {
			// We'll need to visit the sibling next, so we should
			// already have its hash.
			case i < len(visit)-1 && visit[i+1] == siblingIdx:
				sibling = tree[siblingIdx]

			// We've already visited this hash as the sibling of
			// another and explored their parent, so we can skip it.
			case i > 0 && visit[i-1] == siblingIdx:
				continue

			// The sibling is the same as the current hash we're
			// visiting if it corresponds to the last transaction of
			// a block with an odd number of transactions.
			case height == 0 && proof.NumTransactions%2 != 0 &&
				hashIdx == uint32(proof.NumTransactions-1):
				sibling = tree[hashIdx]

			// Our sibling is not known, so we'll assume it's the
			// next hash in the merkle proof.
			default:
				sibling = proof.MerkleProof[nextProofIdx]
				nextProofIdx++

				// Populate the sibling in the tree in case we
				// need to extract a merkle proof from it that
				// relies on this hash.
				tree[siblingIdx] = sibling
			}

			// With the sibling found, we'll determine our parent by
			// hashing the siblings according to their index in the
			// tree.
			hash := tree[hashIdx]
			var parent *chainhash.Hash
			if hashIdx < siblingIdx {
				parent = blockchain.HashMerkleBranches(
					hash, sibling,
				)
			} else {
				parent = blockchain.HashMerkleBranches(
					sibling, hash,
				)
			}

			// We'll visit the parent during the next level
			// iteration.
			parentIdx := (hashIdx >> 1) | (1 << treeHeight)
			tree[parentIdx] = parent
			visit = append(visit, parentIdx)
		}

		// Now that we've visited all of the hashes at the current
		// level, we can move on to the next.
		visit = visit[visitLen:]
	}

	// Arriving at the root should have consumed all of the hashes in the
	// merkle proof, otherwise it is an invalid proof.
	if nextProofIdx != len(proof.MerkleProof) {
		return nil, fmt.Errorf("consumed %v out of %v hashes",
			nextProofIdx, len(proof.MerkleProof))
	}

	return tree, nil
}

// extractFromCachedProof attempts to extract a channel proof from one that's
// already cached. This requires that the cached proof contains all transactions
// the extract proof should contain. If it doesn't, then ErrProofMissingTx is
// returned.
func extractFromCachedProof(txIdxs []uint32,
	cachedProof *CachedChannelProof) (*ChannelProof, error) {

	// Construct the set of transactions the cache proof commits to in order
	// to determine if we can extract a proof for the transaction indices
	// requested.
	proofTxs := make(map[uint32]*wire.MsgTx, len(txIdxs))
	for i, txIdx := range cachedProof.TxIdxs {
		proofTxs[txIdx] = cachedProof.Transactions[i]
	}

	txs := make([]*wire.MsgTx, 0, len(txIdxs))
	for _, txIdx := range txIdxs {
		// The cached proof doesn't commit to the current transaction,
		// so we're unable to extract a proof for it.
		tx, ok := proofTxs[txIdx]
		if !ok {
			return nil, ErrProofMissingTx
		}

		// Along the way, we'll coalesce the corresponding raw
		// transactions.
		txs = append(txs, tx)
	}

	// Then, construct a partial merkle tree that commits to all of the
	// transactions requested, which we'll use to extract their merkle
	// proof.
	merkleTree, err := constructMerkleTree(
		cachedProof.ChannelProof, cachedProof.TxIdxs,
	)
	if err != nil {
		return nil, err
	}
	merkleProof := extractMerkleProof(
		txIdxs, cachedProof.NumTransactions, merkleTree,
	)

	return &ChannelProof{
		MerkleProof:     merkleProof,
		Transactions:    txs,
		NumTransactions: cachedProof.NumTransactions,
	}, nil
}

// mergeWithCachedProof merges a newly computed channel proof with a cached one
// that share the same merkle tree.
func mergeWithCachedProof(newInfo, cachedInfo proofInfo,
	merkleTree []*chainhash.Hash, numTxsInBlock uint16) *CachedChannelProof {

	// Since both proofs should share the same merkle tree, we can combine
	// the transaction indices of both and extract a merkle proof that
	// commits to each.
	mergedInfo := mergeProofInfo(newInfo, cachedInfo)
	merkleProof := extractMerkleProof(
		mergedInfo.idxs, numTxsInBlock, merkleTree,
	)

	return &CachedChannelProof{
		ChannelProof: &ChannelProof{
			MerkleProof:     merkleProof,
			Transactions:    mergedInfo.txs,
			NumTransactions: numTxsInBlock,
		},
		TxIdxs: mergedInfo.idxs,
	}
}

// ConstructChannelProof constructs an aggregatable channel proof for the
// requested transactions. If a cache is provided, then it will be used to
// extract and/or merge proofs from/with an already cached proof.
func ConstructChannelProof(chain Chain, cache ProofCache,
	req *ChannelProofRequest) (*ChannelProof, error) {

	// The request should include at least one transaction.
	if len(req.Transactions) == 0 {
		return nil, ErrNoChannels
	}

	// If we have a cache and there already exists a proof for the block
	// containing the transactions requested, then we can just extract the
	// proof from it.
	var cachedProof *CachedChannelProof
	if cache != nil {
		var err error
		cachedProof, err = cache.GetProof(req.BlockHeight)
		switch err {
		// We haven't constructed a channel proof for this block yet, so
		// proceed with the request.
		case ErrNoProof:

		// We've constructed a channel proof for this block already, so
		// we'll attempt to extract this request's proof from it.
		case nil:
			proof, err := extractFromCachedProof(
				req.Transactions, cachedProof,
			)
			switch err {
			// We were successfully able to extract a channel proof
			// from our cached one, so we can return immediately.
			case nil:
				log.Debugf("Extracted channel proof for %v "+
					"from cache", req)
				return proof, nil

			// Our cached channel proof does not commit to one of
			// the transactions this request is for, so proceed to
			// construct it from the block.
			case ErrProofMissingTx:

			// For whatever reason, we were unable to extract a
			// channel proof for this request from the cached one
			// even though we should have been able to.
			//
			// TODO(wilmer): Remove proof? This could indicate that
			// we either constructed an invalid proof or the
			// persisted state became inconsistent.
			default:
				log.Errorf("Unable to extract channel proof "+
					"for %v from cache: %v", req, err)
			}

		// We were unable to determine if we had a cached channel proof
		// for this block, but this doesn't imply we can't proceed with
		// the request anyway.
		default:
			log.Errorf("Unable to determine if a cached channel "+
				"proof exists for %v: %v", req, err)
		}
	}

	// If we're here, then we need to construct a new channel proof. We'll
	// start by retrieving the block that corresponds to the transactions in
	// the request.
	blockHash, err := chain.GetBlockHash(int32(req.BlockHeight))
	if err != nil {
		return nil, err
	}
	block, err := chain.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	// We'll confirm each transaction has a valid index within the block and
	// that the transactions requested are in sorted order according to
	// their index. This is required as part of the proof format.
	txs := make([]*wire.MsgTx, 0, len(req.Transactions))
	for i, txIdx := range req.Transactions {
		if txIdx >= uint32(len(block.Transactions)) {
			return nil, ErrChannelInvalidTxIndex
		}
		if i > 0 && req.Transactions[i-1] >= txIdx {
			return nil, ErrChannelInvalidTxIndex
		}
		txs = append(txs, block.Transactions[txIdx])
	}

	// Extract the merkle proof from the block's merkle tree.
	//
	// TODO(wilmer): BuildMerkleTreeStore requires a btcutil.Block rather
	// than a wire.MsgBlock, making us copy all the transactions of the
	// block again.
	merkleTree := blockchain.BuildMerkleTreeStore(
		btcutil.NewBlock(block).Transactions(), false,
	)
	merkleProof := extractMerkleProof(
		req.Transactions, uint16(len(block.Transactions)), merkleTree,
	)
	proof := &ChannelProof{
		MerkleProof:     merkleProof,
		Transactions:    txs,
		NumTransactions: uint16(len(block.Transactions)),
	}

	// If a cache exists, we'll attempt to cache our newly constructed
	// proof.
	if cache != nil {
		switch {
		// If we didn't have a cached proof for this block, we'll just
		// go ahead and cache this one.
		case cachedProof == nil:
			cachedProof = &CachedChannelProof{
				ChannelProof: proof,
				TxIdxs:       req.Transactions,
			}

		// Otherwise, we'll merge and cache the one we just constructed
		// with our previously cached one.
		default:
			newInfo := proofInfo{
				idxs: req.Transactions,
				txs:  proof.Transactions,
			}
			cachedInfo := proofInfo{
				idxs: cachedProof.TxIdxs,
				txs:  cachedProof.Transactions,
			}
			cachedProof = mergeWithCachedProof(
				newInfo, cachedInfo, merkleTree,
				proof.NumTransactions,
			)
		}

		err := cache.PutProof(req.BlockHeight, cachedProof)
		if err != nil {
			log.Errorf("Unable to cache proof for %v: %v", req, err)
		}
	}

	return proof, nil
}

// VerifyChannelProof verifies a channel proof commits to all of the
// transactions requested.
func VerifyChannelProof(chain Chain, req *ChannelProofRequest,
	proof *ChannelProof) error {

	// Before attempting to verify the proof, make sure we know of the block
	// first.
	blockHash, err := chain.GetBlockHash(int32(req.BlockHeight))
	if err != nil {
		return err
	}
	blockHeader, err := chain.GetBlockHeader(blockHash)
	if err != nil {
		return err
	}

	// Then, construct a partial merkle tree that commits to all of the
	// transactions requested.
	tree, err := constructMerkleTree(proof, req.Transactions)
	if err != nil {
		return err
	}

	// The proof is invalid if the root doesn't match what we believe it
	// should be.
	if !tree[len(tree)-1].IsEqual(&blockHeader.MerkleRoot) {
		return fmt.Errorf("merkle root mismatch: expected %v, got %v",
			blockHeader.MerkleRoot, tree[len(tree)-1])
	}

	return nil
}

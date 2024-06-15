package spv

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func init() {
	testBlock.Deserialize(bytes.NewReader(testRawBlock))
}

var (
	// Block 1571140 on testnet3. We use a block with an odd number of
	// transactions to ensure we handle CVE-2012-2459.
	testRawBlock, _ = hex.DecodeString("00000020360c8a8317b9938461f3b2de2e4fd62b6c44bd5b06e5bd7b6ee0690a00000000fccdc113d1fb099f5a81d224e59a34b4e13fb47a7916c82e322e6abf23b3da9c4a7c3b5df421031a2984de0907020000000001010000000000000000000000000000000000000000000000000000000000000000ffffffff220344f91704377c3b5d44434578706c6f726174696f6e09000fd65b04000000000000ffffffff02830f54020000000017a914e0725ef08b0046fd2df3c58d5fefc5580e1f59de870000000000000000266a24aa21a9edcd8dec7531686d48eb9db4e6fcf2a5af150bb7b960ce9528a2fac8a402ba58b8012000000000000000000000000000000000000000000000000000000000000000000000000002000000000101465fbf3771ce6f1cffd68101cde8cf77495242ab2979c8e84a84e34cbbaedd45000000000090000000015fcf03000000000016001467ea9916339c8255030d92ca73cb53013f1e80130347304402202133b3c10a0c21332b46cd3f468253b17776076b3de0eb730cdecdd95f366dbb02206e368803323dc2f61fbf7d1b3c93536b90770b34399b48ac65e0db0c5b3fb03401004d632103a882b9cd240e0168d540380491c68a4a8d27896088490761af340c3a519f995567029000b27521023366488095fee4f943006609c3734b354837ff0f52e864dc22d0d24d977e7caa68ac43f9170002000000000101293cccdf59ba385113af74ba1d8005be57aa4fd47633c89ca5ec9ee80e758ea30000000017160014e6e6b1a83ff2c111753194aa579dd43510e71fb6feffffff0240420f000000000017a91474188b0cc920be9f822585e15153bed3b43ce08887eec610000000000017a914d6aeea9497a74dd54c216f48c9f51c441ca73ea1870247304402206c96b3bc568f1f02e89ffc2a03286bb4bf1bde61dfb7ee53f1ee091ac2f7617002206782cb1f629ce6e38514a9649ddd384b653f8c5f66bef0289a72c7d81cf3e978012102a5b42cac6d21d119a9119554443d1cd8d5cf7acba54172e29e7b56fe8bd3d38b43f91700020000000001011c39067f24381df69c6bf06537adc442fca8620efd5fac24696d41a99cd539cd01000000171600141be26cfcb860cc29aa073b4d7eed4e25d08c6c7bfeffffff02ecc610000000000017a9147162c121652342f80ae890c5d4af11d6a04cb3da8740420f000000000017a91474188b0cc920be9f822585e15153bed3b43ce088870247304402204902003f2234dba6be98c07ed80a4d72b582c35816062593eb2f14f3b9106c0f0220185dc9b920581d9648860ac6054642d5942faeb54344e68b55d74151ad2a2b9e012103adba184c01dc5db0bb24111675388f6f10fdaee49d798dfb2508b999b2a229f243f91700020000000001013e199b69635dd08a2f124a8fad9b8314ec583d0fac37dffef5be7733bcc9dcec0000000017160014e07ab851334a0f2d5b33080e99cedb043b1e9205feffffff02eec610000000000017a914108e9bbb667ad51682950d8964f791d1ae7b25db8740420f000000000017a91474188b0cc920be9f822585e15153bed3b43ce088870247304402203694716f6e7feb86592f7a1925a7cd3060573411bbac00160ebea756670e219802207e05de13b59c3538606e71d257bbde768fb023ac400fd6bc893631292d54c736012102058472ab0c729c966a12b8e51d6f5c20de80cb1ff67d12b3a8f1ed6a591b507143f9170002000000000101202403821d24e734ed49cb25de69e19c32f5e66b03de93c3c47f03e32110f1c90100000017160014fee294750c7a1730bb4f11c98cc061eb5b602946feffffff02eec610000000000017a914d46dfbe1d919ec343081eaa7a5b8540e339f5d0f8740420f000000000017a91474188b0cc920be9f822585e15153bed3b43ce088870247304402205f5c2badbb17f63f38db5ad44e077b9ddb99789d7b528b5dcd56eaa97a272cbc02200496a59d464f5da7ffd1598a333b430582c640969ee6c4bc6512864edea011e0012102abb3cbe909aba83e9d23b05caf99db7e417cb28ab633b94e750bad2c70bcd15b43f9170002000000000101f9f2f0fb425bfba86097645281993b2285caae0cd6530217c0097742a05118eb0100000000fdffffff02a0860100000000001600148f1b9ec9a37a3888be009f78e797cca0ec934537d11f0c0000000000160014e7b6d09219b4a511c8c03b461c15f219183bbe8d0247304402202e9d8deac064f521135f35c4121780ea19a844f637f1f12626b978cd6c12b6e8022003331599e402776854dee901ea6abdd8d9a3e9086e5ddecc484aa512920196e601210279e63dd4ba6ae3cf61fd3ec9d4e6e347b9745ff24aa55b6570a5c2224c71375d43f91700")
	testBlock       wire.MsgBlock
)

type mockChain struct {
	hashes map[int32]chainhash.Hash
	blocks map[chainhash.Hash]wire.MsgBlock
}

var _ Chain = (*mockChain)(nil)

func newMockChain(b ...*wire.MsgBlock) *mockChain {
	hashes := make(map[int32]chainhash.Hash)
	blocks := make(map[chainhash.Hash]wire.MsgBlock)
	for i, block := range b {
		hash := block.BlockHash()
		hashes[int32(i)] = hash
		blocks[hash] = *block
	}
	return &mockChain{hashes: hashes, blocks: blocks}
}

func (c *mockChain) GetBlockHash(height int32) (*chainhash.Hash, error) {
	hash, ok := c.hashes[height]
	if !ok {
		return nil, fmt.Errorf("height %v not found", height)
	}
	return &hash, nil
}

func (c *mockChain) GetBlockHeader(hash *chainhash.Hash) (*wire.BlockHeader, error) {
	block, ok := c.blocks[*hash]
	if !ok {
		return nil, fmt.Errorf("header for %v not found", hash)
	}
	return &block.Header, nil
}

func (c *mockChain) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	block, ok := c.blocks[*hash]
	if !ok {
		return nil, fmt.Errorf("block for %v not found", hash)
	}
	return &block, nil
}

type mockProofCache struct {
	sync.Mutex
	proofs map[uint32]CachedChannelProof

	read  chan uint32
	write chan uint32
}

var _ ProofCache = (*mockProofCache)(nil)

func newMockProofCache() *mockProofCache {
	return &mockProofCache{
		proofs: make(map[uint32]CachedChannelProof),
		read:   make(chan uint32, 1),
		write:  make(chan uint32, 1),
	}
}

func (c *mockProofCache) PutProof(height uint32, proof *CachedChannelProof) error {
	c.Lock()
	defer c.Unlock()

	c.proofs[height] = *proof
	c.write <- height
	return nil
}

func (c *mockProofCache) GetProof(height uint32) (*CachedChannelProof, error) {
	c.Lock()
	defer c.Unlock()

	proof, ok := c.proofs[height]
	if !ok {
		return nil, ErrNoProof
	}
	c.read <- height
	return &proof, nil
}

func (c *mockProofCache) assertRead(t *testing.T, expHeight uint32) {
	t.Helper()

	select {
	case height := <-c.read:
		if height != height {
			t.Fatalf("expected cache read for height %v, got %v",
				expHeight, height)
		}
	default:
		t.Fatalf("expected cache read for height %v", expHeight)
	}
}

func (c *mockProofCache) assertNoRead(t *testing.T) {
	t.Helper()

	select {
	case height := <-c.read:
		t.Fatalf("received unexpected cache read for height %v", height)
	default:
	}
}

func (c *mockProofCache) assertWrite(t *testing.T, expHeight uint32) {
	t.Helper()

	select {
	case height := <-c.write:
		if height != height {
			t.Fatalf("expected cache write for height %v, got %v",
				expHeight, height)
		}
	default:
		t.Fatalf("expected cache write for height %v", expHeight)
	}
}

func (c *mockProofCache) assertNoWrite(t *testing.T) {
	t.Helper()

	select {
	case height := <-c.write:
		t.Fatalf("received unexpected cache write for height %v", height)
	default:
	}
}

// TestChannelProofs ensures channel proofs are constructed and verified
// properly.
func TestChannelProofs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		set   []uint32
		block *wire.MsgBlock
	}{
		{
			name:  "one transaction",
			set:   []uint32{1},
			block: &testBlock,
		},
		{
			name:  "last transaction in block",
			set:   []uint32{6},
			block: &testBlock,
		},
		{
			name:  "two transactions on same root branch",
			set:   []uint32{1, 3},
			block: &testBlock,
		},
		{
			name:  "two transactions on different root branches",
			set:   []uint32{3, 4},
			block: &testBlock,
		},
		{
			name:  "multiple transactions including last",
			set:   []uint32{0, 3, 6},
			block: &testBlock,
		},
		{
			name:  "all transactions",
			set:   []uint32{0, 1, 2, 3, 4, 5, 6},
			block: &testBlock,
		},
	}

	for _, testCase := range testCases {
		success := t.Run(testCase.name, func(t *testing.T) {
			chain := newMockChain(testCase.block)
			req := &ChannelProofRequest{
				BlockHeight:  0,
				Transactions: testCase.set,
			}
			proof, err := ConstructChannelProof(chain, nil, req)
			if err != nil {
				t.Fatalf("unable to construct channel proof: "+
					"%v", err)
			}
			err = VerifyChannelProof(chain, req, proof)
			if err != nil {
				t.Fatalf("unable to verify channel proof: %v",
					err)
			}
		})
		if !success {
			return
		}
	}
}

// TestExtractChannelProofs ensures we can extract a valid proof from a cached
// one, as long as the cached one provides all of the information required to do
// so.
func TestExtractChannelProofs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		cacheSet   []uint32
		extractSet []uint32
		block      *wire.MsgBlock
	}{
		{
			name:       "1 of n",
			cacheSet:   []uint32{3, 4, 5},
			extractSet: []uint32{5},
			block:      &testBlock,
		},
		{
			name:       "1 of n including last transaction",
			cacheSet:   []uint32{0, 1, 2, 6},
			extractSet: []uint32{6},
			block:      &testBlock,
		},
		{
			name:       "m of n",
			cacheSet:   []uint32{0, 2, 3, 5},
			extractSet: []uint32{3, 5},
			block:      &testBlock,
		},
		{
			name:       "m of n including last transaction",
			cacheSet:   []uint32{1, 2, 4, 5, 6},
			extractSet: []uint32{2, 4, 6},
			block:      &testBlock,
		},
		{
			name:       "n of n",
			cacheSet:   []uint32{0, 1, 2, 3, 4, 5, 6},
			extractSet: []uint32{0, 1, 2, 3, 4, 5, 6},
			block:      &testBlock,
		},
	}

	for _, testCase := range testCases {
		success := t.Run(testCase.name, func(t *testing.T) {
			chain := newMockChain(testCase.block)
			cache := newMockProofCache()

			const blockHeight = 0

			// Construct and verify the request we should cache
			// first.
			cacheReq := &ChannelProofRequest{
				BlockHeight:  blockHeight,
				Transactions: testCase.cacheSet,
			}
			proof, err := ConstructChannelProof(
				chain, cache, cacheReq,
			)
			if err != nil {
				t.Fatalf("unable to construct channel proof: "+
					"%v", err)
			}
			cache.assertWrite(t, blockHeight)
			err = VerifyChannelProof(chain, cacheReq, proof)
			if err != nil {
				t.Fatalf("unable to verify channel proof: %v",
					err)
			}

			// Construct the extract proof and verify there was a
			// cache hit without a write. A write would indicate
			// that we attempted to merge two proofs, which
			// shouldn't happen.
			extractReq := &ChannelProofRequest{
				BlockHeight:  blockHeight,
				Transactions: testCase.extractSet,
			}
			extractedProof, err := ConstructChannelProof(
				chain, cache, extractReq,
			)
			if err != nil {
				t.Fatalf("unable to construct extracted "+
					"channel proof: %v", err)
			}

			cache.assertRead(t, blockHeight)
			cache.assertNoWrite(t)

			// Verify the extracted proof is valid.
			err = VerifyChannelProof(
				chain, extractReq, extractedProof,
			)
			if err != nil {
				t.Fatalf("unable to verify extracted channel "+
					"proof: %v", err)
			}
		})
		if !success {
			return
		}
	}
}

// TestMergeChannelProofs ensures we can properly merge two proofs together for
// the same block.
func TestMergeChannelProofs(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		set1   []uint32
		set2   []uint32
		merged []uint32
		block  *wire.MsgBlock
	}{
		{
			name:   "disjoint sets",
			set1:   []uint32{0, 2, 4},
			set2:   []uint32{1, 3, 5},
			merged: []uint32{0, 1, 2, 3, 4, 5},
			block:  &testBlock,
		},
		{
			name:   "disjoint sets including last transaction",
			set1:   []uint32{0, 6},
			set2:   []uint32{0, 3},
			merged: []uint32{0, 3, 6},
			block:  &testBlock,
		},
		{
			name:   "intersecting sets",
			set1:   []uint32{0, 2, 4, 5},
			set2:   []uint32{0, 3},
			merged: []uint32{0, 2, 3, 4, 5},
			block:  &testBlock,
		},
		{
			name:   "intersecting sets including last transaction",
			set1:   []uint32{0, 6},
			set2:   []uint32{0, 3},
			merged: []uint32{0, 3, 6},
			block:  &testBlock,
		},
	}

	for _, testCase := range testCases {
		success := t.Run(testCase.name, func(t *testing.T) {
			chain := newMockChain(testCase.block)
			cache := newMockProofCache()

			const blockHeight = 0

			// Construct a proof for the first set.
			req1 := &ChannelProofRequest{
				BlockHeight:  blockHeight,
				Transactions: testCase.set1,
			}
			_, err := ConstructChannelProof(chain, cache, req1)
			if err != nil {
				t.Fatalf("unable to construct channel proof: "+
					"%v", err)
			}

			// Since there isn't a proof already cached, we'll just
			// write this one.
			cache.assertNoRead(t)
			cache.assertWrite(t, blockHeight)

			// Then, construct the proof for the second set.
			req2 := &ChannelProofRequest{
				BlockHeight:  blockHeight,
				Transactions: testCase.set2,
			}
			_, err = ConstructChannelProof(chain, cache, req2)
			if err != nil {
				t.Fatalf("unable to construct channel proof: "+
					"%v", err)
			}

			// The proof for the first set was previously cached, so
			// we should expect both to be merged.
			cache.assertRead(t, blockHeight)
			cache.assertWrite(t, blockHeight)

			// Verify that the proof was merged correctly.
			cachedProof, err := cache.GetProof(blockHeight)
			if err != nil {
				t.Fatalf("unable to get merged proof: %v", err)
			}
			cache.assertRead(t, blockHeight)

			mergedReq := &ChannelProofRequest{
				BlockHeight:  blockHeight,
				Transactions: testCase.merged,
			}
			err = VerifyChannelProof(
				chain, mergedReq, cachedProof.ChannelProof,
			)
			if err != nil {
				t.Fatalf("unable to verify merged channel "+
					"proof: %v", err)
			}
		})
		if !success {
			return
		}
	}
}

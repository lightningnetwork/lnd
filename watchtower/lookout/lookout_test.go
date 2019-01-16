// +build dev

package lookout_test

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/lookout"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
)

type mockPunisher struct {
	matches chan *lookout.JusticeDescriptor
}

func (p *mockPunisher) Punish(
	info *lookout.JusticeDescriptor, quit <-chan struct{}) error {

	p.matches <- info
	return nil
}

func makeArray32(i uint64) [32]byte {
	var arr [32]byte
	binary.BigEndian.PutUint64(arr[:], i)
	return arr
}

func makeArray33(i uint64) [33]byte {
	var arr [33]byte
	binary.BigEndian.PutUint64(arr[:], i)
	return arr
}

func makePubKey(i uint64) [33]byte {
	var arr [33]byte
	arr[0] = 0x02
	if i%2 == 1 {
		arr[0] |= 0x01
	}
	binary.BigEndian.PutUint64(arr[1:], i)
	return arr
}

func makeArray64(i uint64) [64]byte {
	var arr [64]byte
	binary.BigEndian.PutUint64(arr[:], i)
	return arr
}

func makeAddrSlice(size int) []byte {
	addr := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, addr); err != nil {
		panic("cannot make addr")
	}
	return addr
}

func TestLookoutBreachMatching(t *testing.T) {
	db := wtdb.NewMockDB()

	// Initialize an mock backend to feed the lookout blocks.
	backend := lookout.NewMockBackend()

	// Initialize a punisher that will feed any successfully constructed
	// justice descriptors across the matches channel.
	matches := make(chan *lookout.JusticeDescriptor)
	punisher := &mockPunisher{matches: matches}

	// With the resources in place, initialize and start our watcher.
	watcher := lookout.New(&lookout.Config{
		BlockFetcher:   backend,
		DB:             db,
		EpochRegistrar: backend,
		Punisher:       punisher,
	})
	if err := watcher.Start(); err != nil {
		t.Fatalf("unable to start watcher: %v", err)
	}

	rewardAndCommitType := blob.TypeFromFlags(
		blob.FlagReward, blob.FlagCommitOutputs,
	)

	// Create two sessions, representing two distinct clients.
	sessionInfo1 := &wtdb.SessionInfo{
		ID: makeArray33(1),
		Policy: wtpolicy.Policy{
			BlobType:   rewardAndCommitType,
			MaxUpdates: 10,
		},
		RewardAddress: makeAddrSlice(22),
	}
	sessionInfo2 := &wtdb.SessionInfo{
		ID: makeArray33(2),
		Policy: wtpolicy.Policy{
			BlobType:   rewardAndCommitType,
			MaxUpdates: 10,
		},
		RewardAddress: makeAddrSlice(22),
	}

	// Insert both sessions into the watchtower's database.
	err := db.InsertSessionInfo(sessionInfo1)
	if err != nil {
		t.Fatalf("unable to insert session info: %v", err)
	}
	err = db.InsertSessionInfo(sessionInfo2)
	if err != nil {
		t.Fatalf("unable to insert session info: %v", err)
	}

	// Construct two distinct transactions, that will be used to test the
	// breach hint matching.
	tx := wire.NewMsgTx(wire.TxVersion)
	hash1 := tx.TxHash()

	tx2 := wire.NewMsgTx(wire.TxVersion + 1)
	hash2 := tx2.TxHash()

	if bytes.Equal(hash1[:], hash2[:]) {
		t.Fatalf("breach txids should be different")
	}

	// Construct a justice kit for each possible breach transaction.
	blob1 := &blob.JusticeKit{
		SweepAddress:     makeAddrSlice(22),
		RevocationPubKey: makePubKey(1),
		LocalDelayPubKey: makePubKey(1),
		CSVDelay:         144,
		CommitToLocalSig: makeArray64(1),
	}
	blob2 := &blob.JusticeKit{
		SweepAddress:     makeAddrSlice(22),
		RevocationPubKey: makePubKey(2),
		LocalDelayPubKey: makePubKey(2),
		CSVDelay:         144,
		CommitToLocalSig: makeArray64(2),
	}

	// Encrypt the first justice kit under the txid of the first txn.
	encBlob1, err := blob1.Encrypt(hash1[:], blob.FlagCommitOutputs.Type())
	if err != nil {
		t.Fatalf("unable to encrypt sweep detail 1: %v", err)
	}

	// Encrypt the second justice kit under the txid of the second txn.
	encBlob2, err := blob2.Encrypt(hash2[:], blob.FlagCommitOutputs.Type())
	if err != nil {
		t.Fatalf("unable to encrypt sweep detail 2: %v", err)
	}

	// Add both state updates to the tower's database.
	txBlob1 := &wtdb.SessionStateUpdate{
		ID:            makeArray33(1),
		Hint:          wtdb.NewBreachHintFromHash(&hash1),
		EncryptedBlob: encBlob1,
		SeqNum:        1,
	}
	txBlob2 := &wtdb.SessionStateUpdate{
		ID:            makeArray33(2),
		Hint:          wtdb.NewBreachHintFromHash(&hash2),
		EncryptedBlob: encBlob2,
		SeqNum:        1,
	}
	if _, err := db.InsertStateUpdate(txBlob1); err != nil {
		t.Fatalf("unable to add tx to db: %v", err)
	}
	if _, err := db.InsertStateUpdate(txBlob2); err != nil {
		t.Fatalf("unable to add tx to db: %v", err)
	}

	// Create a block containing the first transaction, connecting this
	// block should match the first state update's breach hint.
	block := &wire.MsgBlock{
		Header: wire.BlockHeader{
			Nonce: 1,
		},
		Transactions: []*wire.MsgTx{tx},
	}
	blockHash := block.BlockHash()
	epoch := &chainntnfs.BlockEpoch{
		Hash:   &blockHash,
		Height: 1,
	}

	// Connect the block via our mock backend.
	backend.ConnectEpoch(epoch, block)

	// This should trigger dispatch of the justice kit for the first tx.
	select {
	case match := <-matches:
		txid := match.BreachedCommitTx.TxHash()
		if !bytes.Equal(txid[:], hash1[:]) {
			t.Fatalf("matched breach did not match tx1's txid")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("breach tx1 was not matched")
	}

	// Ensure that at most one txn was matched as a result of connecting the
	// first block.
	select {
	case <-matches:
		t.Fatalf("only one txn should have been matched")
	case <-time.After(50 * time.Millisecond):
	}

	// Now, construct a second block containing the second breach
	// transaction.
	block2 := &wire.MsgBlock{
		Header: wire.BlockHeader{
			Nonce: 2,
		},
		Transactions: []*wire.MsgTx{tx2},
	}
	blockHash2 := block2.BlockHash()
	epoch2 := &chainntnfs.BlockEpoch{
		Hash:   &blockHash2,
		Height: 2,
	}

	// Verify that the block hashes do no collide, otherwise the mock
	// backend may not function properly.
	if bytes.Equal(blockHash[:], blockHash2[:]) {
		t.Fatalf("block hashes should be different")
	}

	// Connect the second block, such that the block is delivered via the
	// epoch stream.
	backend.ConnectEpoch(epoch2, block2)

	// This should trigger dispatch of the justice kit for the second txn.
	select {
	case match := <-matches:
		txid := match.BreachedCommitTx.TxHash()
		if !bytes.Equal(txid[:], hash2[:]) {
			t.Fatalf("received breach did not match tx2's txid")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("tx was not matched")
	}

	// Ensure that at most one txn was matched as a result of connecting the
	// second block.
	select {
	case <-matches:
		t.Fatalf("only one txn should have been matched")
	case <-time.After(50 * time.Millisecond):
	}
}

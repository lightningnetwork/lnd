package lookout_test

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/lookout"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtmock"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
	"github.com/stretchr/testify/require"
)

type mockPunisher struct {
	matches chan *lookout.JusticeDescriptor
}

func (p *mockPunisher) Punish(
	info *lookout.JusticeDescriptor, quit <-chan struct{}) error {

	p.matches <- info
	return nil
}

func makeRandomPK() *btcec.PublicKey {
	pk, _ := btcec.NewPrivateKey()
	return pk.PubKey()
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

func makeTestSig(i uint64) lnwire.Sig {
	sigBytes := makeArray64(i)
	sig, _ := lnwire.NewSigFromWireECDSA(sigBytes[:])
	return sig
}

func makeAddrSlice(size int) []byte {
	addr := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, addr); err != nil {
		panic("cannot make addr")
	}
	return addr
}

func TestLookoutBreachMatching(t *testing.T) {
	db := wtmock.NewTowerDB()

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
		MinBackoff:     time.Second,
		MaxBackoff:     time.Minute,
		MaxNumRetries:  1,
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
			TxPolicy: wtpolicy.TxPolicy{
				BlobType:     rewardAndCommitType,
				SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
			},
			MaxUpdates: 10,
		},
		RewardAddress: makeAddrSlice(22),
	}
	sessionInfo2 := &wtdb.SessionInfo{
		ID: makeArray33(2),
		Policy: wtpolicy.Policy{
			TxPolicy: wtpolicy.TxPolicy{
				BlobType:     rewardAndCommitType,
				SweepFeeRate: wtpolicy.DefaultSweepFeeRate,
			},
			MaxUpdates: 10,
		},
		RewardAddress: makeAddrSlice(22),
	}

	// Insert both sessions into the watchtower's database.
	err := db.InsertSessionInfo(sessionInfo1)
	require.NoError(t, err, "unable to insert session info")
	err = db.InsertSessionInfo(sessionInfo2)
	require.NoError(t, err, "unable to insert session info")

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
	blobType := blob.FlagCommitOutputs.Type()
	breachInfo1 := &lnwallet.BreachRetribution{
		RemoteDelay: 144,
		KeyRing: &lnwallet.CommitmentKeyRing{
			ToLocalKey:    makeRandomPK(),
			RevocationKey: makeRandomPK(),
		},
	}
	commitment1, err := blobType.CommitmentType(nil)
	require.NoError(t, err)

	blob1, err := commitment1.NewJusticeKit(
		makeAddrSlice(22), breachInfo1, false,
	)
	require.NoError(t, err)

	blob1.AddToLocalSig(makeTestSig(1))

	breachInfo2 := &lnwallet.BreachRetribution{
		RemoteDelay: 144,
		KeyRing: &lnwallet.CommitmentKeyRing{
			ToLocalKey:    makeRandomPK(),
			RevocationKey: makeRandomPK(),
		},
	}
	commitment2, err := blobType.CommitmentType(nil)
	require.NoError(t, err)

	blob2, err := commitment2.NewJusticeKit(
		makeAddrSlice(22), breachInfo2, false,
	)
	require.NoError(t, err)

	blob2.AddToLocalSig(makeTestSig(1))

	key1 := blob.NewBreachKeyFromHash(&hash1)
	key2 := blob.NewBreachKeyFromHash(&hash2)

	// Encrypt the first justice kit under breach key one.
	encBlob1, err := blob.Encrypt(blob1, key1)
	require.NoError(t, err, "unable to encrypt sweep detail 1")

	// Encrypt the second justice kit under breach key two.
	encBlob2, err := blob.Encrypt(blob2, key2)
	require.NoError(t, err, "unable to encrypt sweep detail 2")

	// Add both state updates to the tower's database.
	txBlob1 := &wtdb.SessionStateUpdate{
		ID:            makeArray33(1),
		Hint:          blob.NewBreachHintFromHash(&hash1),
		EncryptedBlob: encBlob1,
		SeqNum:        1,
	}
	txBlob2 := &wtdb.SessionStateUpdate{
		ID:            makeArray33(2),
		Hint:          blob.NewBreachHintFromHash(&hash2),
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

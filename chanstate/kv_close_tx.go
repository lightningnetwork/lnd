package chanstate

import (
	"bytes"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// forceCloseTxKey points to a the unilateral closing tx that we
	// broadcasted when moving the channel to state CommitBroadcasted.
	forceCloseTxKey = []byte("closing-tx-key")

	// coopCloseTxKey points to a the cooperative closing tx that we
	// broadcasted when moving the channel to state CoopBroadcasted.
	coopCloseTxKey = []byte("coop-closing-tx-key")
)

// ForceCloseTxKey returns the key used to store the unilateral closing
// transaction in a channel bucket.
func ForceCloseTxKey() []byte {
	return forceCloseTxKey
}

// CoopCloseTxKey returns the key used to store the cooperative closing
// transaction in a channel bucket.
func CoopCloseTxKey() []byte {
	return coopCloseTxKey
}

// PutChannelCloseTx stores the closing transaction under the requested key in
// the target channel bucket.
func PutChannelCloseTx(chanBucket kvdb.RwBucket, key []byte,
	closeTx *wire.MsgTx) error {

	var b bytes.Buffer
	if err := closeTx.Serialize(&b); err != nil {
		return err
	}

	return chanBucket.Put(key, b.Bytes())
}

// FetchChannelCloseTx retrieves the closing transaction stored under the
// requested key in the target channel bucket.
func FetchChannelCloseTx(chanBucket kvdb.RBucket,
	key []byte) (*wire.MsgTx, error) {

	bs := chanBucket.Get(key)
	if bs == nil {
		return nil, ErrNoCloseTx
	}

	closeTx := wire.NewMsgTx(2)
	r := bytes.NewReader(bs)
	if err := closeTx.Deserialize(r); err != nil {
		return nil, err
	}

	return closeTx, nil
}

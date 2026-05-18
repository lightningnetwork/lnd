package chanstate

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// dataLossCommitPointKey stores the commitment point received from the
	// remote peer during a channel sync in case we have lost channel state.
	dataLossCommitPointKey = []byte("data-loss-commit-point-key")
)

// DataLossCommitPointKey returns the key used to store the data-loss commit
// point in a channel bucket.
func DataLossCommitPointKey() []byte {
	return dataLossCommitPointKey
}

// PutChannelDataLossCommitPoint stores the data-loss commit point in the
// target channel bucket.
func PutChannelDataLossCommitPoint(chanBucket kvdb.RwBucket,
	commitPoint *btcec.PublicKey) error {

	return chanBucket.Put(
		dataLossCommitPointKey, commitPoint.SerializeCompressed(),
	)
}

// FetchChannelDataLossCommitPoint retrieves the data-loss commit point from the
// target channel bucket.
func FetchChannelDataLossCommitPoint(
	chanBucket kvdb.RBucket) (*btcec.PublicKey, error) {

	bs := chanBucket.Get(dataLossCommitPointKey)
	if bs == nil {
		return nil, ErrNoCommitPoint
	}

	var b [btcec.PubKeyBytesLenCompressed]byte
	r := bytes.NewReader(bs)
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return nil, err
	}

	return btcec.ParsePubKey(b[:])
}

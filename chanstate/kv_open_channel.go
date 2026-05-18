package chanstate

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
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

const (
	// A tlv type definition used to serialize an outpoint's indexStatus
	// for use in the outpoint index.
	indexStatusType tlv.Type = 0

	// IndexStatusType is the TLV type used to serialize an outpoint's
	// indexStatus for use in the outpoint index.
	IndexStatusType = indexStatusType
)

// indexStatus is an enum-like type that describes what state the outpoint is
// in. Currently only two possible values.
type indexStatus uint8

// IndexStatus is an enum-like type that describes what state the outpoint is
// in. Currently only two possible values.
type IndexStatus = indexStatus

const (
	// outpointOpen represents an outpoint that is open in the outpoint
	// index.
	outpointOpen indexStatus = 0

	// OutpointOpen represents an outpoint that is open in the outpoint
	// index.
	OutpointOpen = outpointOpen

	// outpointClosed represents an outpoint that is closed in the outpoint
	// index.
	outpointClosed indexStatus = 1

	// OutpointClosed represents an outpoint that is closed in the outpoint
	// index.
	OutpointClosed = outpointClosed
)

func putOutpointIndexStatus(opBucket kvdb.RwBucket, chanKey []byte,
	status indexStatus) error {

	statusByte := uint8(status)
	statusRecord := tlv.MakePrimitiveRecord(indexStatusType, &statusByte)
	opStream, err := tlv.NewStream(statusRecord)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := opStream.Encode(&b); err != nil {
		return err
	}

	return opBucket.Put(chanKey, b.Bytes())
}

// PutOpenOutpointIndex stores chanKey in the outpoint index as an open
// outpoint.
func PutOpenOutpointIndex(opBucket kvdb.RwBucket, chanKey []byte) error {
	return putOutpointIndexStatus(opBucket, chanKey, outpointOpen)
}

// UpdateClosedOutpointIndex flips the outpoint index entry for chanKey from
// open to closed. The index entry must already exist; it was placed there when
// the channel was opened.
func UpdateClosedOutpointIndex(tx kvdb.RwTx, chanKey []byte) error {
	opBucket := tx.ReadWriteBucket(outpointBucket)
	if opBucket == nil {
		return ErrNoChanDBExists
	}
	if opBucket.Get(chanKey) == nil {
		return ErrMissingIndexEntry
	}

	return putOutpointIndexStatus(opBucket, chanKey, outpointClosed)
}

// IsOutpointClosed reports whether the supplied chanKey has been flipped to
// outpointClosed in the supplied outpointBucket. The flip is performed in the
// same transaction as the rest of CloseChannel (sync and tombstone paths
// alike), so a true result is the authoritative "this channel went through
// CloseChannel" signal. On tombstone-enabled backends the chanBucket may still
// exist on disk; readers consult this helper to skip those entries. Callers
// fetch outpointBucket once and pass it in, which lets loop-style readers
// hoist the bucket lookup out of the inner loop.
func IsOutpointClosed(opBucket kvdb.RBucket, chanKey []byte) (bool, error) {
	if opBucket == nil {
		return false, nil
	}
	raw := opBucket.Get(chanKey)
	if raw == nil {
		return false, nil
	}

	var status uint8
	statusRecord := tlv.MakePrimitiveRecord(indexStatusType, &status)
	stream, err := tlv.NewStream(statusRecord)
	if err != nil {
		return false, err
	}
	if err := stream.Decode(bytes.NewReader(raw)); err != nil {
		return false, fmt.Errorf("decode outpoint status for "+
			"chan_key=%x: %w", chanKey, err)
	}

	return indexStatus(status) == outpointClosed, nil
}

// FetchChanBucket is a helper function that returns the bucket where a
// channel's data resides in given: the public key for the node, the outpoint,
// and the chainhash that the channel resides on.
func FetchChanBucket(tx kvdb.RTx, nodeKey *btcec.PublicKey,
	outPoint *wire.OutPoint, chainHash chainhash.Hash) (
	kvdb.RBucket, error) {

	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket := tx.ReadBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, ErrNoChanDBExists
	}

	// TODO(roasbeef): CreateTopLevelBucket on the interface isn't like
	// CreateIfNotExists, will return error.

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := nodeKey.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket := nodeChanBucket.NestedReadBucket(chainHash[:])
	if chainBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for this channel itself.
	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}
	chanKey := chanPointBuf.Bytes()

	// Treat already-closed channels as gone. The chanBucket may still
	// exist on tombstone-enabled backends; the outpoint flip is the
	// source of truth.
	closed, err := IsOutpointClosed(tx.ReadBucket(outpointBucket), chanKey)
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrChannelNotFound
	}

	chanBucket := chainBucket.NestedReadBucket(chanKey)
	if chanBucket == nil {
		return nil, ErrChannelNotFound
	}

	return chanBucket, nil
}

// FetchChanBucketRw is a helper function that returns the bucket where a
// channel's data resides in given: the public key for the node, the outpoint,
// and the chainhash that the channel resides on. This differs from
// FetchChanBucket in that it returns a writeable bucket.
func FetchChanBucketRw(tx kvdb.RwTx, nodeKey *btcec.PublicKey,
	outPoint *wire.OutPoint, chainHash chainhash.Hash) (kvdb.RwBucket,
	error) {

	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket := tx.ReadWriteBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, ErrNoChanDBExists
	}

	// TODO(roasbeef): CreateTopLevelBucket on the interface isn't like
	// CreateIfNotExists, will return error.

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := nodeKey.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadWriteBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket := nodeChanBucket.NestedReadWriteBucket(chainHash[:])
	if chainBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for this channel itself.
	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}
	chanKey := chanPointBuf.Bytes()

	// Treat already-closed channels as gone. The chanBucket may still
	// exist on tombstone-enabled backends; the outpoint flip is the
	// source of truth.
	closed, err := IsOutpointClosed(tx.ReadBucket(outpointBucket), chanKey)
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrChannelNotFound
	}

	chanBucket := chainBucket.NestedReadWriteBucket(chanKey)
	if chanBucket == nil {
		return nil, ErrChannelNotFound
	}

	return chanBucket, nil
}

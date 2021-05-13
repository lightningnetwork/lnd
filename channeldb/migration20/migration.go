package migration20

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// openChanBucket stores all the open channel information.
	openChanBucket = []byte("open-chan-bucket")

	// closedChannelBucket stores all the closed channel information.
	closedChannelBucket = []byte("closed-chan-bucket")

	// outpointBucket is an index mapping outpoints to a tlv
	// stream of channel data.
	outpointBucket = []byte("outpoint-bucket")
)

const (
	// A tlv type definition used to serialize an outpoint's indexStatus for
	// use in the outpoint index.
	indexStatusType tlv.Type = 0
)

// indexStatus is an enum-like type that describes what state the
// outpoint is in. Currently only two possible values.
type indexStatus uint8

const (
	// outpointOpen represents an outpoint that is open in the outpoint index.
	outpointOpen indexStatus = 0

	// outpointClosed represents an outpoint that is closed in the outpoint
	// index.
	outpointClosed indexStatus = 1
)

// MigrateOutpointIndex populates the outpoint index with outpoints that
// the node already has. This takes every outpoint in the open channel
// bucket and every outpoint in the closed channel bucket and stores them
// in this index.
func MigrateOutpointIndex(tx kvdb.RwTx) error {
	log.Info("Migrating to the outpoint index")

	// First get the set of open outpoints.
	openList, err := getOpenOutpoints(tx)
	if err != nil {
		return err
	}

	// Then get the set of closed outpoints.
	closedList, err := getClosedOutpoints(tx)
	if err != nil {
		return err
	}

	// Get the outpoint bucket which was created in migration 19.
	bucket := tx.ReadWriteBucket(outpointBucket)

	// Store the set of open outpoints in the outpoint bucket.
	if err := putOutpoints(bucket, openList, false); err != nil {
		return err
	}

	// Store the set of closed outpoints in the outpoint bucket.
	return putOutpoints(bucket, closedList, true)
}

// getOpenOutpoints traverses through the openChanBucket and returns the
// list of these channels' outpoints.
func getOpenOutpoints(tx kvdb.RwTx) ([]*wire.OutPoint, error) {
	var ops []*wire.OutPoint

	openBucket := tx.ReadBucket(openChanBucket)
	if openBucket == nil {
		return ops, nil
	}

	// Iterate through every node and chain bucket to get every open
	// outpoint.
	//
	// The bucket tree:
	// openChanBucket -> nodePub -> chainHash -> chanPoint
	err := openBucket.ForEach(func(k, v []byte) error {
		// Ensure that the key is the same size as a pubkey and the
		// value is nil.
		if len(k) != 33 || v != nil {
			return nil
		}

		nodeBucket := openBucket.NestedReadBucket(k)
		if nodeBucket == nil {
			return nil
		}

		return nodeBucket.ForEach(func(k, v []byte) error {
			// If there's a value it's not a bucket.
			if v != nil {
				return nil
			}

			chainBucket := nodeBucket.NestedReadBucket(k)
			if chainBucket == nil {
				return fmt.Errorf("unable to read "+
					"bucket for chain: %x", k)
			}

			return chainBucket.ForEach(func(k, v []byte) error {
				// If there's a value it's not a bucket.
				if v != nil {
					return nil
				}

				var op wire.OutPoint
				r := bytes.NewReader(k)
				if err := readOutpoint(r, &op); err != nil {
					return err
				}

				ops = append(ops, &op)

				return nil
			})
		})
	})
	if err != nil {
		return nil, err
	}
	return ops, nil
}

// getClosedOutpoints traverses through the closedChanBucket and returns
// a list of closed outpoints.
func getClosedOutpoints(tx kvdb.RwTx) ([]*wire.OutPoint, error) {
	var ops []*wire.OutPoint
	closedBucket := tx.ReadBucket(closedChannelBucket)
	if closedBucket == nil {
		return ops, nil
	}

	// Iterate through every key-value pair to gather all outpoints.
	err := closedBucket.ForEach(func(k, v []byte) error {
		var op wire.OutPoint
		r := bytes.NewReader(k)
		if err := readOutpoint(r, &op); err != nil {
			return err
		}

		ops = append(ops, &op)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return ops, nil
}

// putOutpoints puts the set of outpoints into the outpoint bucket.
func putOutpoints(bucket kvdb.RwBucket, ops []*wire.OutPoint, isClosed bool) error {
	status := uint8(outpointOpen)
	if isClosed {
		status = uint8(outpointClosed)
	}

	record := tlv.MakePrimitiveRecord(indexStatusType, &status)
	stream, err := tlv.NewStream(record)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		return err
	}

	// Store the set of outpoints with the encoded tlv stream.
	for _, op := range ops {
		var opBuf bytes.Buffer
		if err := writeOutpoint(&opBuf, op); err != nil {
			return err
		}

		if err := bucket.Put(opBuf.Bytes(), b.Bytes()); err != nil {
			return err
		}
	}

	return nil
}

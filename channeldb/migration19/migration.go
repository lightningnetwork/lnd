package migration19

import (
	"bytes"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// channelOpeningStateBucket stores channelOpeningState for
	// pending channels.
	channelOpeningStateBucket = []byte("channelOpeningState")
)

// channelOpeningState represents different states of a channel on-disk.
type channelOpeningState uint8

// openingInfo describes the data stored in the channelOpeningState bucket.
type openingInfo struct {
	op    wire.OutPoint
	state channelOpeningState
	sid   lnwire.ShortChannelID
}

// MigrateOutpoint migrates the outpoints stored in the funding open process
// to use a 32-byte encoding rather than a 33-byte encoding. This allows the
// fundingmanager to use the channeldb versions of the functions instead.
func MigrateOutpoint(tx kvdb.RwTx) error {
	log.Infof("Migrating outpoints in channelOpeningState bucket")

	// For every entry in the openingStateBucket, grab the key and value.
	// This tuple will be stored in a map.
	infos, err := getChannelOpeningState(tx)
	if err != nil {
		return err
	}

	// Now we delete everything in the channelOpeningState bucket.
	if err := deleteChannelOpeningState(tx, infos); err != nil {
		return err
	}

	// Then put all of the entries back under a new key.
	if err := putChannelOpeningState(tx, infos); err != nil {
		return err
	}

	return nil
}

// putChannelOpeningState puts all of the entries back under a new key.
func putChannelOpeningState(tx kvdb.RwTx, infos []openingInfo) error {
	opBucket := tx.ReadWriteBucket(channelOpeningStateBucket)
	if opBucket == nil {
		return nil
	}

	// For every openingInfo, put the associated data into the bucket
	// under the channeldb outpoint encoding.
	for _, info := range infos {
		var opBytes bytes.Buffer
		if err := channeldbWriteOutpoint(&opBytes, &info.op); err != nil {
			return err
		}

		scratch := make([]byte, 10)
		byteOrder.PutUint16(scratch[:2], uint16(info.state))
		byteOrder.PutUint64(scratch[2:], info.sid.ToUint64())

		if err := opBucket.Put(opBytes.Bytes(), scratch); err != nil {
			return err
		}
	}

	return nil
}

// deleteChannelOpeningState deletes all channelOpeningState from the
// bucket.
func deleteChannelOpeningState(tx kvdb.RwTx, infos []openingInfo) error {
	opBucket := tx.ReadWriteBucket(channelOpeningStateBucket)
	if opBucket == nil {
		return nil
	}

	// For every openingInfo, delete the associated data from the bucket.
	for _, info := range infos {
		var opBytes bytes.Buffer
		if err := legacyWriteOutpoint(&opBytes, &info.op); err != nil {
			return err
		}

		if err := opBucket.Delete(opBytes.Bytes()); err != nil {
			return err
		}
	}

	return nil
}

// getChannelOpeningState retrieves all the channelOpeningState data from
// the bucket and returns it in a slice.
func getChannelOpeningState(tx kvdb.RwTx) ([]openingInfo, error) {
	var infos []openingInfo

	openingStateBucket := tx.ReadBucket(channelOpeningStateBucket)
	if openingStateBucket == nil {
		return infos, nil
	}

	err := openingStateBucket.ForEach(func(k, v []byte) error {
		var op wire.OutPoint
		var state channelOpeningState
		var shortChanID lnwire.ShortChannelID

		r := bytes.NewReader(k)
		if err := legacyReadOutpoint(r, &op); err != nil {
			return err
		}

		state = channelOpeningState(byteOrder.Uint16(v[:2]))
		shortChanID = lnwire.NewShortChanIDFromInt(byteOrder.Uint64(v[2:]))

		info := openingInfo{
			op:    op,
			state: state,
			sid:   shortChanID,
		}

		infos = append(infos, info)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return infos, nil
}

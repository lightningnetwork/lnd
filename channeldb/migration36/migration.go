package migration36

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	outpointBucket = []byte("outpoint-bucket")
	chanIDBucket   = []byte("chan-id-bucket")
)

// MigrateChanIDOutpoints upgrades the existing ChannelID membership index to
// store the serialized funding outpoint as each value. Closed outpoints remain
// indexed so ChannelID uniqueness and tombstone semantics stay unchanged.
func MigrateChanIDOutpoints(tx kvdb.RwTx) error {
	log.Info("Populating ChannelID outpoint index")

	outpoints := tx.ReadBucket(outpointBucket)
	if outpoints == nil {
		return fmt.Errorf("outpoint index bucket missing")
	}
	channelIDs := tx.ReadWriteBucket(chanIDBucket)
	if channelIDs == nil {
		return fmt.Errorf("ChannelID index bucket missing")
	}

	return outpoints.ForEach(func(key, value []byte) error {
		if value == nil {
			return fmt.Errorf("unexpected nested outpoint bucket %x", key)
		}
		if len(key) != 36 {
			return fmt.Errorf("invalid outpoint key length %d", len(key))
		}

		var outPoint wire.OutPoint
		reader := bytes.NewReader(key)
		if err := readOutpoint(reader, &outPoint); err != nil {
			return fmt.Errorf("decode outpoint %x: %w", key, err)
		}
		if reader.Len() != 0 {
			return fmt.Errorf("outpoint %x has trailing bytes", key)
		}

		channelID := lnwire.NewChanIDFromOutPoint(outPoint)
		existing := channelIDs.Get(channelID[:])
		if existing != nil && len(existing) > 0 &&
			!bytes.Equal(existing, key) {

			return fmt.Errorf(
				"ChannelID collision %x maps to %x and %x",
				channelID, existing, key,
			)
		}

		if err := channelIDs.Put(channelID[:], key); err != nil {
			return fmt.Errorf(
				"store ChannelID outpoint %x: %w", channelID, err,
			)
		}

		return nil
	})
}

func readOutpoint(reader io.Reader, outPoint *wire.OutPoint) error {
	if _, err := io.ReadFull(reader, outPoint.Hash[:]); err != nil {
		return err
	}
	return binary.Read(reader, binary.BigEndian, &outPoint.Index)
}

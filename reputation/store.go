package reputation

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/kvdb"
)

// storeVersion is the on-disk schema version for channel snapshots.
const storeVersion uint8 = 1

// reputationBucket is the top-level kvdb bucket for reputation state.
var reputationBucket = []byte("reputation-state")

// avgSnapshot is the serialized form of a decaying average.
type avgSnapshot struct {
	value       int64
	lastUpdated uint64
	windowSecs  float64
}

// revenueSnapshot is the serialized form of the aggregated window average.
type revenueSnapshot struct {
	start          uint64
	windowCount    uint8
	windowDuration uint64 // seconds
	inner          avgSnapshot
}

// ChannelSnapshot is the persisted, restart-surviving state for one channel.
// Pending HTLCs and live bucket occupancy are deliberately NOT persisted — they
// are reconstructed at startup (P9).
type ChannelSnapshot struct {
	SCID             uint64
	MaxInFlightMsat  uint64
	MaxAcceptedHTLCs uint16

	reputation avgSnapshot
	revenue    revenueSnapshot

	// salts maps an outgoing scid to the general-bucket salt used to derive
	// its slot assignment, so slots can be regenerated deterministically.
	salts map[uint64][32]byte

	// misuse maps an outgoing scid to the unix-seconds time it last misused
	// the congestion bucket.
	misuse map[uint64]uint64
}

// Store is the persistence backend for reputation state. It performs no
// historical read — it only round-trips the live decaying averages and salts.
type Store interface {
	// LoadChannels returns all persisted channel snapshots.
	LoadChannels() ([]ChannelSnapshot, error)

	// PersistChannels writes the given channel snapshots.
	PersistChannels(snaps []ChannelSnapshot) error
}

// kvdbStore is a kvdb-backed Store.
type kvdbStore struct {
	db kvdb.Backend
}

// NewKVDBStore returns a Store backed by the given kvdb backend.
func NewKVDBStore(db kvdb.Backend) Store {
	return &kvdbStore{db: db}
}

// PersistChannels writes the snapshots into the reputation bucket.
func (s *kvdbStore) PersistChannels(snaps []ChannelSnapshot) error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(reputationBucket)
		if err != nil {
			return err
		}

		for _, snap := range snaps {
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], snap.SCID)

			var buf bytes.Buffer
			if err := encodeSnapshot(&buf, &snap); err != nil {
				return err
			}

			if err := bucket.Put(key[:], buf.Bytes()); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// LoadChannels reads all persisted snapshots.
func (s *kvdbStore) LoadChannels() ([]ChannelSnapshot, error) {
	var snaps []ChannelSnapshot

	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(reputationBucket)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(_, v []byte) error {
			snap, err := decodeSnapshot(bytes.NewReader(v))
			if err != nil {
				return err
			}
			snaps = append(snaps, *snap)

			return nil
		})
	}, func() { snaps = nil })
	if err != nil {
		return nil, err
	}

	return snaps, nil
}

// encodeSnapshot serializes a channel snapshot.
func encodeSnapshot(w io.Writer, snap *ChannelSnapshot) error {
	if err := writeUint8(w, storeVersion); err != nil {
		return err
	}
	if err := writeUint64(w, snap.SCID); err != nil {
		return err
	}
	if err := writeUint64(w, snap.MaxInFlightMsat); err != nil {
		return err
	}
	if err := writeUint16(w, snap.MaxAcceptedHTLCs); err != nil {
		return err
	}

	if err := encodeAvg(w, snap.reputation); err != nil {
		return err
	}

	// Revenue.
	if err := writeUint64(w, snap.revenue.start); err != nil {
		return err
	}
	if err := writeUint8(w, snap.revenue.windowCount); err != nil {
		return err
	}
	if err := writeUint64(w, snap.revenue.windowDuration); err != nil {
		return err
	}
	if err := encodeAvg(w, snap.revenue.inner); err != nil {
		return err
	}

	// Salts.
	if err := writeUint32(w, uint32(len(snap.salts))); err != nil {
		return err
	}
	for scid, salt := range snap.salts {
		if err := writeUint64(w, scid); err != nil {
			return err
		}
		if _, err := w.Write(salt[:]); err != nil {
			return err
		}
	}

	// Misuse.
	if err := writeUint32(w, uint32(len(snap.misuse))); err != nil {
		return err
	}
	for scid, ts := range snap.misuse {
		if err := writeUint64(w, scid); err != nil {
			return err
		}
		if err := writeUint64(w, ts); err != nil {
			return err
		}
	}

	return nil
}

// decodeSnapshot deserializes a channel snapshot.
func decodeSnapshot(r io.Reader) (*ChannelSnapshot, error) {
	version, err := readUint8(r)
	if err != nil {
		return nil, err
	}
	if version != storeVersion {
		return nil, fmt.Errorf("unknown reputation store version %d",
			version)
	}

	snap := &ChannelSnapshot{
		salts:  make(map[uint64][32]byte),
		misuse: make(map[uint64]uint64),
	}

	if snap.SCID, err = readUint64(r); err != nil {
		return nil, err
	}
	if snap.MaxInFlightMsat, err = readUint64(r); err != nil {
		return nil, err
	}
	if snap.MaxAcceptedHTLCs, err = readUint16(r); err != nil {
		return nil, err
	}

	if snap.reputation, err = decodeAvg(r); err != nil {
		return nil, err
	}

	if snap.revenue.start, err = readUint64(r); err != nil {
		return nil, err
	}
	if snap.revenue.windowCount, err = readUint8(r); err != nil {
		return nil, err
	}
	if snap.revenue.windowDuration, err = readUint64(r); err != nil {
		return nil, err
	}
	if snap.revenue.inner, err = decodeAvg(r); err != nil {
		return nil, err
	}

	nSalts, err := readUint32(r)
	if err != nil {
		return nil, err
	}
	for i := uint32(0); i < nSalts; i++ {
		scid, err := readUint64(r)
		if err != nil {
			return nil, err
		}
		var salt [32]byte
		if _, err := io.ReadFull(r, salt[:]); err != nil {
			return nil, err
		}
		snap.salts[scid] = salt
	}

	nMisuse, err := readUint32(r)
	if err != nil {
		return nil, err
	}
	for i := uint32(0); i < nMisuse; i++ {
		scid, err := readUint64(r)
		if err != nil {
			return nil, err
		}
		ts, err := readUint64(r)
		if err != nil {
			return nil, err
		}
		snap.misuse[scid] = ts
	}

	return snap, nil
}

// encodeAvg / decodeAvg serialize a decaying-average snapshot. The decay rate
// is not stored; it is recomputed from the window on load.
func encodeAvg(w io.Writer, a avgSnapshot) error {
	if err := writeUint64(w, uint64(a.value)); err != nil {
		return err
	}
	if err := writeUint64(w, a.lastUpdated); err != nil {
		return err
	}

	return writeUint64(w, uint64(a.windowSecs))
}

func decodeAvg(r io.Reader) (avgSnapshot, error) {
	var a avgSnapshot
	v, err := readUint64(r)
	if err != nil {
		return a, err
	}
	a.value = int64(v)

	if a.lastUpdated, err = readUint64(r); err != nil {
		return a, err
	}

	ws, err := readUint64(r)
	if err != nil {
		return a, err
	}
	a.windowSecs = float64(ws)

	return a, nil
}

// --- small fixed-width helpers ---

func writeUint8(w io.Writer, v uint8) error {
	_, err := w.Write([]byte{v})

	return err
}

func writeUint16(w io.Writer, v uint16) error {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	_, err := w.Write(b[:])

	return err
}

func writeUint32(w io.Writer, v uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])

	return err
}

func writeUint64(w io.Writer, v uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, err := w.Write(b[:])

	return err
}

func readUint8(r io.Reader) (uint8, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}

	return b[0], nil
}

func readUint16(r io.Reader) (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}

	return binary.BigEndian.Uint16(b[:]), nil
}

func readUint32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}

	return binary.BigEndian.Uint32(b[:]), nil
}

func readUint64(r io.Reader) (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}

	return binary.BigEndian.Uint64(b[:]), nil
}

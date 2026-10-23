package trampolinerelay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/routing/trampoline"
)

const (
	storedGroupVersion = 1
	maxStoredGroupSize = 2 << 20
)

var trampolineRelayBucket = []byte("trampoline-relay-v1")

// ErrPaymentHashConflict is returned when a second relay group attempts to
// claim a payment hash already bound to a different outer payment address.
// The downstream router identifies payments by hash alone, so permitting two
// such groups would alias their cancellation and reconciliation state.
var ErrPaymentHashConflict = errors.New(
	"trampoline payment hash belongs to another relay group",
)

// StoredGroup is the durable representation of one trampoline aggregation
// group, including the exact authenticated onion data and relay snapshot.
type StoredGroup struct {
	Version      uint8  `json:"version"`
	OnionBytes   []byte `json:"onion_bytes"`
	PayloadBytes []byte `json:"payload_bytes"`
	NextOnion    []byte `json:"next_onion,omitempty"`
	NextPathKey  []byte `json:"next_path_key,omitempty"`
	NextNode     []byte `json:"next_node,omitempty"`
	PathID       []byte `json:"path_id,omitempty"`
	OuterPathKey []byte `json:"outer_path_key,omitempty"`
	OuterMPP     bool   `json:"outer_mpp,omitempty"`
	//nolint:ll // The persistence key is part of the durable format.
	CLTV          uint32                   `json:"required_outer_cltv,omitempty"`
	Role          trampoline.HopRole       `json:"role"`
	Snapshot      trampoline.RelaySnapshot `json:"snapshot"`
	Preimage      *[32]byte                `json:"preimage,omitempty"`
	FailureCode   uint16                   `json:"failure_code,omitempty"`
	FailureReason []byte                   `json:"failure_reason,omitempty"`
	// PruneAfterHeight is the first height at which every incoming HTLC is
	// beyond its expiry and the configured reorganization safety margin.
	PruneAfterHeight uint32 `json:"prune_after_height,omitempty"`
}

// StoredGroupRecord associates a durable group with its aggregation key.
type StoredGroupRecord struct {
	ID    trampoline.RelayID
	Group StoredGroup
}

// Store atomically persists relay groups across daemon and link restarts.
type Store interface {
	Put(trampoline.RelayID, StoredGroup) error
	Load(trampoline.RelayID) (*StoredGroup, error)
	List() ([]StoredGroupRecord, error)
	Delete(trampoline.RelayID) error
}

// MemoryStore is a concurrency-safe store used by focused tests.
type MemoryStore struct {
	mu       sync.Mutex
	groups   map[trampoline.RelayID][]byte
	hashToID map[[32]byte]trampoline.RelayID
}

// NewMemoryStore creates an empty relay store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		groups:   make(map[trampoline.RelayID][]byte),
		hashToID: make(map[[32]byte]trampoline.RelayID),
	}
}

// Put stores one canonical group value.
func (s *MemoryStore) Put(id trampoline.RelayID, group StoredGroup) error {
	encoded, err := encodeStoredGroup(group)
	if err != nil {
		return err
	}

	s.mu.Lock()
	existingID, ok := s.hashToID[id.PaymentHash]
	if ok && existingID != id {
		s.mu.Unlock()

		return ErrPaymentHashConflict
	}
	s.hashToID[id.PaymentHash] = id
	s.groups[id] = encoded
	s.mu.Unlock()

	return nil
}

// Load returns an independent decoded group value.
func (s *MemoryStore) Load(
	id trampoline.RelayID) (*StoredGroup, error) {

	s.mu.Lock()
	encoded, ok := s.groups[id]
	encoded = append([]byte(nil), encoded...)
	s.mu.Unlock()
	if !ok {
		return nil, nil
	}

	group, err := decodeStoredGroup(encoded)
	if err != nil {
		return nil, err
	}

	return &group, nil
}

// List returns independent decoded copies of every durable relay group.
func (s *MemoryStore) List() ([]StoredGroupRecord, error) {
	s.mu.Lock()
	encodedGroups := make(map[trampoline.RelayID][]byte, len(s.groups))
	for id, encoded := range s.groups {
		encodedGroups[id] = append([]byte(nil), encoded...)
	}
	s.mu.Unlock()

	records := make([]StoredGroupRecord, 0, len(encodedGroups))
	for id, encoded := range encodedGroups {
		group, err := decodeStoredGroup(encoded)
		if err != nil {
			return nil, err
		}
		records = append(records, StoredGroupRecord{
			ID:    id,
			Group: group,
		})
	}

	return records, nil
}

// Delete removes one group.
func (s *MemoryStore) Delete(id trampoline.RelayID) error {
	s.mu.Lock()
	delete(s.groups, id)
	if s.hashToID[id.PaymentHash] == id {
		delete(s.hashToID, id.PaymentHash)
	}
	s.mu.Unlock()

	return nil
}

// KVStore persists relay groups in lnd's channel database.
type KVStore struct {
	db kvdb.Backend
}

// NewKVStore creates a relay store backed by the provided database.
func NewKVStore(db kvdb.Backend) (*KVStore, error) {
	if db == nil {
		return nil, errors.New("trampoline relay database is nil")
	}

	store := &KVStore{db: db}
	err := kvdb.Update(store.db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(trampolineRelayBucket)
		return err
	}, func() {})
	if err != nil {
		return nil, fmt.Errorf(
			"create trampoline relay bucket: %w", err,
		)
	}

	return store, nil
}

// Put stores one relay group atomically.
func (s *KVStore) Put(id trampoline.RelayID, group StoredGroup) error {
	encoded, err := encodeStoredGroup(group)
	if err != nil {
		return err
	}
	key := relayKey(id)

	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(trampolineRelayBucket)
		if bucket == nil {
			return errors.New("trampoline relay bucket missing")
		}
		if err := claimPaymentHash(bucket, id, key); err != nil {
			return err
		}

		return bucket.Put(key, encoded)
	}, func() {})
}

// Load returns a validated independent relay group.
func (s *KVStore) Load(
	id trampoline.RelayID) (*StoredGroup, error) {

	var encoded []byte
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(trampolineRelayBucket)
		if bucket == nil {
			return nil
		}
		value := bucket.Get(relayKey(id))
		if value != nil {
			encoded = append([]byte(nil), value...)
		}

		return nil
	}, func() {
		encoded = nil
	})
	if err != nil || encoded == nil {
		return nil, err
	}

	group, err := decodeStoredGroup(encoded)
	if err != nil {
		return nil, err
	}

	return &group, nil
}

// List returns every validated relay group while ignoring hash-index keys.
func (s *KVStore) List() ([]StoredGroupRecord, error) {
	var records []StoredGroupRecord
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(trampolineRelayBucket)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(key, value []byte) error {
			if len(key) != 64 || value == nil {
				return nil
			}

			group, err := decodeStoredGroup(value)
			if err != nil {
				return err
			}
			var id trampoline.RelayID
			copy(id.PaymentHash[:], key[:32])
			copy(id.PaymentAddr[:], key[32:])
			records = append(records, StoredGroupRecord{
				ID:    id,
				Group: group,
			})

			return nil
		})
	}, func() {
		records = nil
	})

	return records, err
}

// Delete removes one relay group atomically.
func (s *KVStore) Delete(id trampoline.RelayID) error {
	key := relayKey(id)

	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(trampolineRelayBucket)
		if bucket == nil {
			return nil
		}

		claim := bucket.Get(id.PaymentHash[:])
		if bytes.Equal(claim, key) {
			if err := bucket.Delete(id.PaymentHash[:]); err != nil {
				return err
			}
		}

		return bucket.Delete(key)
	}, func() {})
}

func claimPaymentHash(bucket kvdb.RwBucket, id trampoline.RelayID,
	groupKey []byte) error {

	claim := bucket.Get(id.PaymentHash[:])
	if claim != nil {
		if bytes.Equal(claim, groupKey) {
			return nil
		}

		return ErrPaymentHashConflict
	}

	// Populate the index safely for any group written before the index was
	// introduced. Once claimed, normal updates use the constant-time lookup
	// above instead of scanning the bucket.
	err := bucket.ForEach(func(existingKey, _ []byte) error {
		sameKey := bytes.Equal(existingKey, groupKey)
		if len(existingKey) != 64 || sameKey ||
			!bytes.Equal(existingKey[:32], id.PaymentHash[:]) {

			return nil
		}

		return ErrPaymentHashConflict
	})
	if err != nil {
		return err
	}

	return bucket.Put(id.PaymentHash[:], groupKey)
}

func relayKey(id trampoline.RelayID) []byte {
	key := make([]byte, 64)
	copy(key[:32], id.PaymentHash[:])
	copy(key[32:], id.PaymentAddr[:])

	return key
}

func encodeStoredGroup(group StoredGroup) ([]byte, error) {
	group.Version = storedGroupVersion
	encoded, err := json.Marshal(group)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxStoredGroupSize {
		return nil, fmt.Errorf(
			"stored trampoline group exceeds %d bytes",
			maxStoredGroupSize,
		)
	}

	return encoded, nil
}

func decodeStoredGroup(encoded []byte) (StoredGroup, error) {
	if len(encoded) == 0 || len(encoded) > maxStoredGroupSize {
		return StoredGroup{}, errors.New(
			"invalid stored trampoline group size",
		)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()

	var group StoredGroup
	if err := decoder.Decode(&group); err != nil {
		return StoredGroup{}, err
	}
	if group.Version != storedGroupVersion {
		return StoredGroup{}, fmt.Errorf(
			"unknown stored trampoline group version %d",
			group.Version,
		)
	}
	if len(group.FailureReason) != 0 &&
		len(group.FailureReason) != trampoline.FailurePacketSize {

		return StoredGroup{}, errors.New(
			"invalid stored trampoline failure reason size",
		)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return StoredGroup{}, errors.New(
			"trailing stored trampoline group value",
		)
	}

	return group, nil
}

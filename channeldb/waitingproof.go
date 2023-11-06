package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// waitingProofsBucketKey byte string name of the waiting proofs store.
	waitingProofsBucketKey = []byte("waitingproofs")

	// ErrWaitingProofNotFound is returned if waiting proofs haven't been
	// found by db.
	ErrWaitingProofNotFound = errors.New("waiting proofs haven't been " +
		"found")
)

// WaitingProofStore is the bold db map-like storage for half announcement
// signatures. The one responsibility of this storage is to be able to
// retrieve waiting proofs after client restart.
type WaitingProofStore struct {
	// cache is used in order to reduce the number of redundant get
	// calls, when object isn't stored in it.
	cache map[WaitingProofKey]struct{}
	db    kvdb.Backend
	mu    sync.RWMutex
}

// NewWaitingProofStore creates new instance of proofs storage.
func NewWaitingProofStore(db kvdb.Backend) (*WaitingProofStore, error) {
	s := &WaitingProofStore{
		db: db,
	}

	if err := s.ForAll(func(proof *WaitingProof) error {
		s.cache[proof.Key()] = struct{}{}
		return nil
	}, func() {
		s.cache = make(map[WaitingProofKey]struct{})
	}); err != nil && err != ErrWaitingProofNotFound {
		return nil, err
	}

	return s, nil
}

// Add adds new waiting proof in the storage.
func (s *WaitingProofStore) Add(proof *WaitingProof) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		var err error
		var b bytes.Buffer

		// Get or create the bucket.
		bucket, err := tx.CreateTopLevelBucket(waitingProofsBucketKey)
		if err != nil {
			return err
		}

		// Encode the objects and place it in the bucket.
		if err := proof.Encode(&b); err != nil {
			return err
		}

		key := proof.Key()

		return bucket.Put(key[:], b.Bytes())
	}, func() {})
	if err != nil {
		return err
	}

	// Knowing that the write succeeded, we can now update the in-memory
	// cache with the proof's key.
	s.cache[proof.Key()] = struct{}{}

	return nil
}

// Remove removes the proof from storage by its key.
func (s *WaitingProofStore) Remove(key WaitingProofKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.cache[key]; !ok {
		return ErrWaitingProofNotFound
	}

	err := kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		// Get or create the top bucket.
		bucket := tx.ReadWriteBucket(waitingProofsBucketKey)
		if bucket == nil {
			return ErrWaitingProofNotFound
		}

		return bucket.Delete(key[:])
	}, func() {})
	if err != nil {
		return err
	}

	// Since the proof was successfully deleted from the store, we can now
	// remove it from the in-memory cache.
	delete(s.cache, key)

	return nil
}

// ForAll iterates thought all waiting proofs and passing the waiting proof
// in the given callback.
func (s *WaitingProofStore) ForAll(cb func(*WaitingProof) error,
	reset func()) error {

	return kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(waitingProofsBucketKey)
		if bucket == nil {
			return ErrWaitingProofNotFound
		}

		// Iterate over objects buckets.
		return bucket.ForEach(func(k, v []byte) error {
			// Skip buckets fields.
			if v == nil {
				return nil
			}

			r := bytes.NewReader(v)
			proof := &WaitingProof{}
			if err := proof.Decode(r); err != nil {
				return err
			}

			return cb(proof)
		})
	}, reset)
}

// Get returns the object which corresponds to the given index.
func (s *WaitingProofStore) Get(key WaitingProofKey) (*WaitingProof, error) {
	var proof *WaitingProof

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.cache[key]; !ok {
		return nil, ErrWaitingProofNotFound
	}

	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(waitingProofsBucketKey)
		if bucket == nil {
			return ErrWaitingProofNotFound
		}

		// Iterate over objects buckets.
		v := bucket.Get(key[:])
		if v == nil {
			return ErrWaitingProofNotFound
		}

		r := bytes.NewReader(v)
		return proof.Decode(r)
	}, func() {
		proof = &WaitingProof{}
	})

	return proof, err
}

// WaitingProofKey is the proof key which uniquely identifies the waiting
// proof object. The goal of this key is distinguish the local and remote
// proof for the same channel id.
type WaitingProofKey [9]byte

// WaitingProofType represents the type of the encoded waiting proof.
type WaitingProofType uint8

const (
	// WaitingProofTypeLegacy represents a waiting proof for legacy P2WSH
	// channels.
	WaitingProofTypeLegacy WaitingProofType = 0

	// WaitingProofTypeTaproot represents a waiting proof for taproot
	// channels.
	WaitingProofTypeTaproot WaitingProofType = 1
)

// typeToWaitingProofType is a map from WaitingProofType to an empty
// instantiation of the associated type.
func typeToWaitingProof(pt WaitingProofType) (WaitingProofInterface, bool) {
	switch pt {
	case WaitingProofTypeLegacy:
		return &LegacyWaitingProof{}, true
	case WaitingProofTypeTaproot:
		return &TaprootWaitingProof{}, true

	default:
		return nil, false
	}
}

// WaitingProofInterface is an interface that must be implemented by any waiting
// proof to be stored in the waiting proof DB.
type WaitingProofInterface interface {
	// SCID returns the short channel ID of the channel that the waiting
	// proof is for.
	SCID() lnwire.ShortChannelID

	// Encode encodes the waiting proof to the given buffer.
	Encode(w *bytes.Buffer, pver uint32) error

	// Decode parses the bytes from the given reader to reconstruct the
	// waiting proof.
	Decode(r io.Reader, pver uint32) error

	// Type returns the waiting proof type.
	Type() WaitingProofType
}

// LegacyWaitingProof is an implementation of the WaitingProofInterface to be
// used for legacy, P2WSH channels.
type LegacyWaitingProof struct {
	lnwire.AnnounceSignatures1
}

// SCID returns the short channel ID of the channel that the waiting
// proof is for.
//
// NOTE: this is part of the WaitingProofInterface.
func (l *LegacyWaitingProof) SCID() lnwire.ShortChannelID {
	return l.ShortChannelID
}

// Type returns the waiting proof type.
//
// NOTE: this is part of the WaitingProofInterface.
func (l *LegacyWaitingProof) Type() WaitingProofType {
	return WaitingProofTypeLegacy
}

var _ WaitingProofInterface = (*LegacyWaitingProof)(nil)

// TaprootWaitingProof is an implementation of the WaitingProofInterface to be
// used for taproot channels.
type TaprootWaitingProof struct {
	lnwire.AnnounceSignatures2

	// AggNonce is the aggregate nonce used to construct the partial
	// signatures. It will be used as the R value in the final signature.
	AggNonce *btcec.PublicKey
}

// SCID returns the short channel ID of the channel that the waiting
// proof is for.
//
// NOTE: this is part of the WaitingProofInterface.
func (t *TaprootWaitingProof) SCID() lnwire.ShortChannelID {
	return t.ShortChannelID
}

// Decode parses the bytes from the given reader to reconstruct the
// waiting proof.
//
// NOTE: this is part of the WaitingProofInterface.
func (t *TaprootWaitingProof) Decode(r io.Reader, pver uint32) error {
	// Read byte to see if agg nonce is present.
	var aggNoncePresent bool
	if err := binary.Read(r, byteOrder, &aggNoncePresent); err != nil {
		return err
	}

	// If agg nonce is present, read it in.
	if aggNoncePresent {
		var nonceBytes [btcec.PubKeyBytesLenCompressed]byte
		if err := binary.Read(r, byteOrder, &nonceBytes); err != nil {
			return err
		}

		nonce, err := btcec.ParsePubKey(nonceBytes[:])
		if err != nil {
			return err
		}

		t.AggNonce = nonce
	}

	return t.AnnounceSignatures2.Decode(r, pver)
}

// Encode encodes the waiting proof to the given buffer.
//
// NOTE: this is part of the WaitingProofInterface.
func (t *TaprootWaitingProof) Encode(w *bytes.Buffer, pver uint32) error {
	// If agg nonce is present, write a signaling byte for that.
	aggNoncePresent := t.AggNonce != nil
	if err := binary.Write(w, byteOrder, aggNoncePresent); err != nil {
		return err
	}

	// Now follow with the actual nonce if present.
	if aggNoncePresent {
		err := binary.Write(
			w, byteOrder, t.AggNonce.SerializeCompressed(),
		)
		if err != nil {
			return err
		}
	}

	return t.AnnounceSignatures2.Encode(w, pver)
}

// Type returns the waiting proof type.
//
// NOTE: this is part of the WaitingProofInterface.
func (t *TaprootWaitingProof) Type() WaitingProofType {
	return WaitingProofTypeTaproot
}

var _ WaitingProofInterface = (*TaprootWaitingProof)(nil)

// WaitingProof is the storable object, which encapsulate the half proof and
// the information about from which side this proof came. This structure is
// needed to make channel proof exchange persistent, so that after client
// restart we may receive remote/local half proof and process it.
type WaitingProof struct {
	WaitingProofInterface
	isRemote bool
}

// NewLegacyWaitingProof constructs a new waiting prof instance for a legacy,
// P2WSH channel.
func NewLegacyWaitingProof(isRemote bool,
	proof *lnwire.AnnounceSignatures1) *WaitingProof {

	return &WaitingProof{
		WaitingProofInterface: &LegacyWaitingProof{*proof},
		isRemote:              isRemote,
	}
}

// NewTaprootWaitingProof constructs a new waiting prof instance for a taproot
// channel.
func NewTaprootWaitingProof(isRemote bool, proof *lnwire.AnnounceSignatures2,
	aggNonce *btcec.PublicKey) *WaitingProof {

	return &WaitingProof{
		WaitingProofInterface: &TaprootWaitingProof{
			AnnounceSignatures2: *proof,
			AggNonce:            aggNonce,
		},
		isRemote: isRemote,
	}
}

// OppositeKey returns the key which uniquely identifies opposite waiting proof.
func (p *WaitingProof) OppositeKey() WaitingProofKey {
	var key [9]byte
	binary.BigEndian.PutUint64(key[:8], p.SCID().ToUint64())

	if !p.isRemote {
		key[8] = 1
	}
	return key
}

// Key returns the key which uniquely identifies waiting proof.
func (p *WaitingProof) Key() WaitingProofKey {
	var key [9]byte
	binary.BigEndian.PutUint64(key[:8], p.SCID().ToUint64())

	if p.isRemote {
		key[8] = 1
	}
	return key
}

// Encode writes the internal representation of waiting proof in byte stream.
func (p *WaitingProof) Encode(w io.Writer) error {
	// Write the type byte.
	if err := binary.Write(w, byteOrder, p.Type()); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, p.isRemote); err != nil {
		return err
	}

	// TODO(yy): remove the type assertion when we finished refactoring db
	// into using write buffer.
	buf, ok := w.(*bytes.Buffer)
	if !ok {
		return fmt.Errorf("expect io.Writer to be *bytes.Buffer")
	}

	return p.WaitingProofInterface.Encode(buf, 0)
}

// Decode reads the data from the byte stream and initializes the
// waiting proof object with it.
func (p *WaitingProof) Decode(r io.Reader) error {
	var proofType WaitingProofType
	if err := binary.Read(r, byteOrder, &proofType); err != nil {
		return err
	}

	if err := binary.Read(r, byteOrder, &p.isRemote); err != nil {
		return err
	}

	proof, ok := typeToWaitingProof(proofType)
	if !ok {
		return fmt.Errorf("unknown proof type")
	}

	if err := proof.Decode(r, 0); err != nil {
		return err
	}

	p.WaitingProofInterface = proof

	return nil
}

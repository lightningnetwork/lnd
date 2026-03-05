package channeldb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
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

	// ErrWaitingProofAlreadyExist is returned if waiting proofs haven't been
	// found by db.
	ErrWaitingProofAlreadyExist = errors.New("waiting proof with such " +
		"key already exist")
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

// WaitingProofKey is the proof key which uniquely identifies the waiting proof
// object. The key includes proof type, short channel ID, and side
// (local/remote) to avoid cross-version collisions.
type WaitingProofKey [10]byte

// WaitingProofType represents the type of proof encoded in a waiting proof
// record.
type WaitingProofType uint8

const (
	// WaitingProofTypeV1 represents a waiting proof containing an
	// AnnounceSignatures1 message (gossip v1, P2WSH channels).
	WaitingProofTypeV1 WaitingProofType = 0

	// WaitingProofTypeV2 represents a waiting proof containing an
	// AnnounceSignatures2 message (gossip v2, taproot channels).
	WaitingProofTypeV2 WaitingProofType = 1
)

// typeToWaitingProof returns an empty instance of the WaitingProofInner
// implementation corresponding to the given proof type.
func typeToWaitingProof(pt WaitingProofType) (WaitingProofInner, bool) {
	switch pt {
	case WaitingProofTypeV1:
		return &V1WaitingProof{}, true
	case WaitingProofTypeV2:
		return &V2WaitingProof{}, true
	default:
		return nil, false
	}
}

// WaitingProofInner is an interface that must be implemented by any waiting
// proof payload to be stored in the waiting proof store.
type WaitingProofInner interface {
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

// V1WaitingProof wraps an AnnounceSignatures1 message for storage as a
// waiting proof.
type V1WaitingProof struct {
	lnwire.AnnounceSignatures1
}

// SCID returns the short channel ID of the channel.
//
// NOTE: this is part of the WaitingProofInner interface.
func (p *V1WaitingProof) SCID() lnwire.ShortChannelID {
	return p.ShortChannelID
}

// Type returns the waiting proof type.
//
// NOTE: this is part of the WaitingProofInner interface.
func (p *V1WaitingProof) Type() WaitingProofType {
	return WaitingProofTypeV1
}

// A compile time check to ensure V1WaitingProof implements the
// WaitingProofInner interface.
var _ WaitingProofInner = (*V1WaitingProof)(nil)

// V2WaitingProof wraps an AnnounceSignatures2 message for storage as a
// waiting proof. It also stores the aggregate MuSig2 nonce needed to
// reconstruct the final signature.
type V2WaitingProof struct {
	lnwire.AnnounceSignatures2

	// AggNonce is the aggregate nonce used to construct the partial
	// signatures. It will be used as the R value in the final signature.
	AggNonce *btcec.PublicKey
}

// SCID returns the short channel ID of the channel.
//
// NOTE: this is part of the WaitingProofInner interface.
func (p *V2WaitingProof) SCID() lnwire.ShortChannelID {
	return p.ShortChannelID.Val
}

// Decode parses the bytes from the given reader to reconstruct the waiting
// proof.
//
// NOTE: this is part of the WaitingProofInner interface.
func (p *V2WaitingProof) Decode(r io.Reader, pver uint32) error {
	// Read the nonce-presence marker first.
	var aggNoncePresent bool
	if err := binary.Read(r, byteOrder, &aggNoncePresent); err != nil {
		return err
	}

	// If present, parse and store the aggregate nonce.
	if aggNoncePresent {
		var nonceBytes [btcec.PubKeyBytesLenCompressed]byte
		if err := binary.Read(r, byteOrder, &nonceBytes); err != nil {
			return err
		}

		nonce, err := btcec.ParsePubKey(nonceBytes[:])
		if err != nil {
			return err
		}

		p.AggNonce = nonce
	}

	// Decode the underlying AnnounceSignatures2 payload.
	return p.AnnounceSignatures2.Decode(r, pver)
}

// Encode encodes the waiting proof to the given buffer.
//
// NOTE: this is part of the WaitingProofInner interface.
func (p *V2WaitingProof) Encode(w *bytes.Buffer, pver uint32) error {
	// Write whether an aggregate nonce follows.
	aggNoncePresent := p.AggNonce != nil
	if err := binary.Write(w, byteOrder, aggNoncePresent); err != nil {
		return err
	}

	// If present, serialize and write the aggregate nonce.
	if aggNoncePresent {
		err := binary.Write(
			w, byteOrder, p.AggNonce.SerializeCompressed(),
		)
		if err != nil {
			return err
		}
	}

	// Encode the underlying AnnounceSignatures2 payload.
	return p.AnnounceSignatures2.Encode(w, pver)
}

// Type returns the waiting proof type.
//
// NOTE: this is part of the WaitingProofInner interface.
func (p *V2WaitingProof) Type() WaitingProofType {
	return WaitingProofTypeV2
}

// A compile time check to ensure V2WaitingProof implements the
// WaitingProofInner interface.
var _ WaitingProofInner = (*V2WaitingProof)(nil)

// WaitingProof is the storable object, which encapsulate the half proof and
// the information about from which side this proof came. This structure is
// needed to make channel proof exchange persistent, so that after client
// restart we may receive remote/local half proof and process it.
type WaitingProof struct {
	WaitingProofInner
	isRemote bool
}

// NewWaitingProof constructs a new waiting proof instance for an
// AnnounceSignatures1 message.
func NewWaitingProof(isRemote bool,
	proof *lnwire.AnnounceSignatures1) *WaitingProof {

	return &WaitingProof{
		WaitingProofInner: &V1WaitingProof{*proof},
		isRemote:          isRemote,
	}
}

// NewV2WaitingProof constructs a new waiting proof instance for an
// AnnounceSignatures2 message.
func NewV2WaitingProof(isRemote bool, proof *lnwire.AnnounceSignatures2,
	aggNonce *btcec.PublicKey) *WaitingProof {

	return &WaitingProof{
		WaitingProofInner: &V2WaitingProof{
			AnnounceSignatures2: *proof,
			AggNonce:            aggNonce,
		},
		isRemote: isRemote,
	}
}

// OppositeKey returns the key which uniquely identifies opposite waiting proof.
func (p *WaitingProof) OppositeKey() WaitingProofKey {
	var key WaitingProofKey
	key[0] = byte(p.Type())
	binary.BigEndian.PutUint64(key[1:9], p.SCID().ToUint64())

	if !p.isRemote {
		key[9] = 1
	}
	return key
}

// Key returns the key which uniquely identifies waiting proof.
func (p *WaitingProof) Key() WaitingProofKey {
	var key WaitingProofKey
	key[0] = byte(p.Type())
	binary.BigEndian.PutUint64(key[1:9], p.SCID().ToUint64())

	if p.isRemote {
		key[9] = 1
	}
	return key
}

// Encode writes the internal representation of waiting proof in byte stream.
func (p *WaitingProof) Encode(w io.Writer) error {
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

	return p.WaitingProofInner.Encode(buf, 0)
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
		return fmt.Errorf("unknown waiting proof type: %v", proofType)
	}

	if err := proof.Decode(r, 0); err != nil {
		return err
	}

	p.WaitingProofInner = proof

	return nil
}

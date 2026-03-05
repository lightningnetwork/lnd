package migration35

import (
	"bytes"
	"encoding/binary"
	"fmt"

	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// waitingProofsBucketKey is the top-level bucket that stores waiting
	// proofs.
	waitingProofsBucketKey = []byte("waitingproofs")

	// byteOrder is the preferred DB byte order.
	byteOrder = binary.BigEndian
)

// waitingProofType represents the type of a waiting proof record.
type waitingProofType uint8

const (
	// waitingProofTypeV1 represents AnnounceSignatures1 proofs (gossip v1).
	waitingProofTypeV1 waitingProofType = 0
)

// waitingProofKey is the key used in the waiting proof bucket.
type waitingProofKey [9]byte

// waitingProof is a migration-only representation of a waiting proof record.
type waitingProof struct {
	announceSignatures *lnwire.AnnounceSignatures
	isRemote           bool
}

// Key computes the waiting proof store key.
func (p *waitingProof) Key() waitingProofKey {
	var key [9]byte
	binary.BigEndian.PutUint64(
		key[:8], p.announceSignatures.ShortChannelID.ToUint64(),
	)

	if p.isRemote {
		key[8] = 1
	}

	return key
}

// decodeLegacyWaitingProof decodes a pre-migration waiting proof in the
// legacy format: isRemote + raw AnnounceSignatures payload.
func decodeLegacyWaitingProof(v []byte) (*waitingProof, error) {
	r := bytes.NewReader(v)

	var isRemote bool
	if err := binary.Read(r, byteOrder, &isRemote); err != nil {
		return nil, err
	}

	ann := &lnwire.AnnounceSignatures{}
	if err := ann.Decode(r, 0); err != nil {
		return nil, err
	}

	return &waitingProof{
		announceSignatures: ann,
		isRemote:           isRemote,
	}, nil
}

// encodeUpdatedWaitingProof encodes a waiting proof in the new format:
// type byte + isRemote + raw AnnounceSignatures payload.
func encodeUpdatedWaitingProof(p *waitingProof) ([]byte, error) {
	var b bytes.Buffer

	if err := binary.Write(&b, byteOrder, waitingProofTypeV1); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, p.isRemote); err != nil {
		return nil, err
	}

	if err := p.announceSignatures.Encode(&b, 0); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// MigrateWaitingProofStore migrates waiting proofs to include a leading proof
// type byte.
func MigrateWaitingProofStore(tx kvdb.RwTx) error {
	log.Info("Migrating waiting proof store")

	bucket := tx.ReadWriteBucket(waitingProofsBucketKey)

	// If the bucket doesn't exist there is no data to migrate.
	if bucket == nil {
		return nil
	}

	return bucket.ForEach(func(k, v []byte) error {
		// Skip nested bucket references.
		if v == nil {
			return nil
		}

		proof, err := decodeLegacyWaitingProof(v)
		if err != nil {
			return fmt.Errorf("decode waiting proof for key %x: %w",
				k, err)
		}

		// Sanity check: the key should match the proof content.
		proofKey := proof.Key()
		if !bytes.Equal(k, proofKey[:]) {
			return fmt.Errorf("proof key (%x) does not "+
				"match bucket key (%x)", proofKey, k)
		}

		updatedProof, err := encodeUpdatedWaitingProof(proof)
		if err != nil {
			return fmt.Errorf("encode updated waiting "+
				"proof for key %x: %w", k, err)
		}

		return bucket.Put(k, updatedProof)
	})
}

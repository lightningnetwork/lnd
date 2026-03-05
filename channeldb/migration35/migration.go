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

// legacyWaitingProofKey is the key format used by legacy waiting proof
// records: [scid(8) || isRemote(1)].
type legacyWaitingProofKey [9]byte

// waitingProofKey is the updated key format used by waiting proof records:
// [proofType(1) || scid(8) || isRemote(1)].
type waitingProofKey [10]byte

// waitingProof is a migration-only representation of a waiting proof record.
type waitingProof struct {
	announceSignatures *lnwire.AnnounceSignatures
	isRemote           bool
}

// LegacyKey computes the legacy waiting proof store key.
func (p *waitingProof) LegacyKey() legacyWaitingProofKey {
	var key legacyWaitingProofKey
	binary.BigEndian.PutUint64(
		key[:8], p.announceSignatures.ShortChannelID.ToUint64(),
	)

	if p.isRemote {
		key[8] = 1
	}

	return key
}

// Key computes the updated waiting proof store key.
func (p *waitingProof) Key() waitingProofKey {
	var key waitingProofKey
	key[0] = byte(waitingProofTypeV1)

	binary.BigEndian.PutUint64(
		key[1:9], p.announceSignatures.ShortChannelID.ToUint64(),
	)

	if p.isRemote {
		key[9] = 1
	}

	return key
}

// decodeLegacyWaitingProof decodes a pre-migration waiting proof in the
// legacy format: isRemote + raw AnnounceSignatures payload.
func decodeLegacyWaitingProof(v []byte) (*waitingProof, error) {
	r := bytes.NewReader(v)

	// Decode the legacy side bit first.
	var isRemote bool
	if err := binary.Read(r, byteOrder, &isRemote); err != nil {
		return nil, err
	}

	// Decode the legacy AnnounceSignatures payload.
	ann := &lnwire.AnnounceSignatures{}
	if err := ann.Decode(r, 0); err != nil {
		return nil, err
	}

	// Reconstruct the migration-local waiting proof representation.
	return &waitingProof{
		announceSignatures: ann,
		isRemote:           isRemote,
	}, nil
}

// encodeUpdatedWaitingProof encodes a waiting proof in the new format:
// type byte + isRemote + raw AnnounceSignatures payload.
func encodeUpdatedWaitingProof(p *waitingProof) ([]byte, error) {
	var b bytes.Buffer

	// Prefix the payload with the explicit waiting proof type.
	if err := binary.Write(&b, byteOrder, waitingProofTypeV1); err != nil {
		return nil, err
	}

	// Preserve the side bit after the type prefix.
	if err := binary.Write(&b, byteOrder, p.isRemote); err != nil {
		return nil, err
	}

	// Encode the existing AnnounceSignatures payload unchanged.
	if err := p.announceSignatures.Encode(&b, 0); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// MigrateWaitingProofStore migrates waiting proofs to include a leading proof
// type byte and rewrites record keys to include proof type as well.
func MigrateWaitingProofStore(tx kvdb.RwTx) error {
	log.Info("Migrating waiting proof store")

	bucket := tx.ReadWriteBucket(waitingProofsBucketKey)

	// If the bucket doesn't exist there is no data to migrate.
	if bucket == nil {
		return nil
	}

	type migratedProof struct {
		oldKey []byte
		newKey waitingProofKey
		value  []byte
	}

	var migratedProofs []migratedProof

	err := bucket.ForEach(func(k, v []byte) error {
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
		legacyKey := proof.LegacyKey()
		if !bytes.Equal(k, legacyKey[:]) {
			return fmt.Errorf("proof key (%x) does not "+
				"match bucket key (%x)", legacyKey, k)
		}

		updatedProofValue, err := encodeUpdatedWaitingProof(proof)
		if err != nil {
			return fmt.Errorf("encode updated waiting "+
				"proof for key %x: %w", k, err)
		}

		oldKey := make([]byte, len(k))
		copy(oldKey, k)

		migratedProofs = append(migratedProofs, migratedProof{
			oldKey: oldKey,
			newKey: proof.Key(),
			value:  updatedProofValue,
		})

		return nil
	})
	if err != nil {
		return err
	}

	for _, proof := range migratedProofs {
		if err := bucket.Delete(proof.oldKey); err != nil {
			return fmt.Errorf(
				"delete legacy waiting proof key %x: %w",
				proof.oldKey, err,
			)
		}

		if err := bucket.Put(proof.newKey[:], proof.value); err != nil {
			return fmt.Errorf(
				"put updated waiting proof key %x: %w",
				proof.newKey, err,
			)
		}
	}

	return nil
}

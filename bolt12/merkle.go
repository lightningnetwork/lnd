package bolt12

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// ErrEmptyMerkleInput is returned by merkleRoot when the input contains no
// TLVs. merkleRoot never returns the all-zero digest for an empty input. A
// verifier must reject a signature over an all-zero digest.
var ErrEmptyMerkleInput = errors.New("cannot compute Merkle root over " +
	"empty TLV set")

// ErrUnsortedMerkleInput is returned by merkleRoot when the input records are
// not in strictly ascending type order. The leaves must be processed in
// ascending TLV order per the spec, so an unsorted or duplicated type would
// otherwise produce an incorrect root silently.
var ErrUnsortedMerkleInput = errors.New("TLV records not in strictly " +
	"ascending type order")

// taggedHash computes SHA256(SHA256(tag) || SHA256(tag) || msg) per the BIP-340
// tagged hash convention.
func taggedHash(tag string, msg []byte) [32]byte {
	return *chainhash.TaggedHash([]byte(tag), msg)
}

// leafHash computes H("LnLeaf", fullTLVBytes) for a single TLV field.
func leafHash(fullTLVBytes []byte) [32]byte {
	return taggedHash("LnLeaf", fullTLVBytes)
}

// nonceHash computes H("LnNonce" || firstTLV, tlvTypeBigSize) for a single TLV
// field. The tag includes the raw bytes of the first TLV in the stream. The
// message is the BigSize-encoded type of the current TLV field.
//
// The tag is the literal byte concatenation of "LnNonce" and the first TLV. Go
// converts []byte to string as a byte-faithful copy. The spec defines the tag
// as byte concatenation, not UTF-8 joining. The caller builds the tag once per
// tree and passes it in, since it is invariant across the records.
func nonceHash(tag string, tlvType tlv.Type) [32]byte {
	var buf [8]byte
	var typeBuf bytes.Buffer

	// WriteVarInt only fails on a Writer error. bytes.Buffer.Write is
	// documented to never return one, so the discard is safe.
	_ = tlv.WriteVarInt(&typeBuf, uint64(tlvType), &buf)

	return taggedHash(tag, typeBuf.Bytes())
}

// branchHash computes H("LnBranch", lesser || greater) where the two child
// hashes are sorted lexicographically with the lesser hash first.
func branchHash(a, b [32]byte) [32]byte {
	if bytes.Compare(a[:], b[:]) > 0 {
		a, b = b, a
	}

	var msg [64]byte
	copy(msg[:32], a[:])
	copy(msg[32:], b[:])

	return taggedHash("LnBranch", msg[:])
}

// signableTLVs returns the subset of records that contribute to the signature's
// Merkle root. Everything outside the inclusive range [240, 1000] is included.
// Types 240-1000 are reserved by the BOLT 12 spec for the signature TLV (type
// 240) and similar non-content fields the signer must not commit to. The
// reserved range covers more than just signature, so the filter is symmetric on
// both ends rather than a single-type exclusion.
func signableTLVs(records []tlv.Record) []tlv.Record {
	out := make([]tlv.Record, 0, len(records))
	for _, r := range records {
		if !bolt12InUnsignedRange(r.Type()) {
			out = append(out, r)
		}
	}

	return out
}

// merkleRoot computes the Merkle root of the given TLV records. Each record is
// encoded in isolation via its TLV stream form to derive the per-leaf full
// type+length+value bytes that feed both the LnLeaf and LnNonce digests. The
// records must be in canonical order (ascending by type, no duplicates);
// merkleRoot enforces the precondition and returns ErrUnsortedMerkleInput.
//
// An empty input returns ErrEmptyMerkleInput. Signing or verifying an empty
// stream would collide with the all-zero digest.
func merkleRoot(records []tlv.Record) ([32]byte, error) {
	if len(records) == 0 {
		return [32]byte{}, ErrEmptyMerkleInput
	}

	// Encode each record on its own to recover the same per-field
	// type+length+value bytes the original wire stream contained. The
	// spec's nonce tag binds to the bytes of the first TLV, so the
	// per-record encoding must match what the producer signed.
	//
	// The re-encoding is byte-exact over the signed range for every
	// message the codec accepts: decode enforces minimal BigSize
	// prefixes, minimal truncated integers, canonical record order, and
	// minimal feature vectors, and preserves unknown TLV values verbatim.
	// signableTLVs strips types 240-1000 before merkleRoot runs, and
	// unknown records in that stripped range do not survive re-encode.
	encoded := make([][]byte, len(records))
	var prevType tlv.Type
	for i, r := range records {
		// Check for strictly ascending order, not just non-descending,
		// to avoid silently dropping duplicates.
		if i > 0 && r.Type() <= prevType {
			return [32]byte{}, fmt.Errorf("%w: type %d after %d",
				ErrUnsortedMerkleInput, r.Type(), prevType)
		}
		prevType = r.Type()

		buf, err := lnwire.EncodeRecords([]tlv.Record{r})
		if err != nil {
			return [32]byte{}, fmt.Errorf("encode record %d (type "+
				"%d): %w", i, r.Type(), err)
		}
		encoded[i] = buf
	}

	firstTLV := encoded[0]

	// The nonce tag binds the first TLV and is invariant across the tree,
	// so it is built once rather than per record. chainhash.TaggedHash
	// re-hashes the tag on each call; at BOLT 12 message sizes that cost
	// is negligible, and keeping the library construction avoids owning a
	// copy of the BIP-340 tagged hash.
	nonceTag := "LnNonce" + string(firstTLV)

	branches := make([][32]byte, len(records))
	for i, r := range records {
		leaf := leafHash(encoded[i])
		nonce := nonceHash(nonceTag, r.Type())
		branches[i] = branchHash(leaf, nonce)
	}

	// Combine branches pairwise until a single root remains.
	for len(branches) > 1 {
		var next [][32]byte
		for i := 0; i < len(branches); i += 2 {
			if i+1 >= len(branches) {
				// Odd element is promoted unchanged.
				next = append(next, branches[i])
				continue
			}

			combined := branchHash(branches[i], branches[i+1])
			next = append(next, combined)
		}
		branches = next
	}

	return branches[0], nil
}

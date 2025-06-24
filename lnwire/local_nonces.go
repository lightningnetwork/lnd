package lnwire

import (
	"bytes"
	// Added for direct binary operations
	"encoding/binary"
	"io"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// LocalNoncesRecordTypeDef is the concrete TLV record type for LocalNoncesData.
// We'll use TlvType22 as it's available and even.
type LocalNoncesRecordTypeDef = tlv.TlvType22

// LocalNonceEntry holds a single TXID -> Musig2Nonce mapping.
type LocalNonceEntry struct {
	TXID  chainhash.Hash
	// Musig2Nonce is [musig2.PubNonceSize]byte
	Nonce Musig2Nonce
}

// LocalNoncesData is the core data structure holding the map of nonces.
type LocalNoncesData struct {
	NoncesMap map[chainhash.Hash]Musig2Nonce
}

// NewLocalNoncesData creates a new LocalNoncesData with an initialized map.
func NewLocalNoncesData() *LocalNoncesData {
	return &LocalNoncesData{
		NoncesMap: make(map[chainhash.Hash]Musig2Nonce),
	}
}

// Record implements the tlv.RecordProducer interface.
func (lnd *LocalNoncesData) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		(LocalNoncesRecordTypeDef)(nil).TypeVal(),
		lnd,
		// Length function
		func() uint64 {
			if lnd.NoncesMap == nil || len(lnd.NoncesMap) == 0 {
				// Just space for numEntries (uint16)
				return 2
			}
			numEntries := len(lnd.NoncesMap)
			return uint64(2 + numEntries*(chainhash.HashSize+musig2.PubNonceSize))
		}(),
		encodeLocalNoncesData,
		decodeLocalNoncesData,
	)
}

// encodeLocalNoncesData implements the tlv.Encoder for LocalNoncesData.
func encodeLocalNoncesData(w io.Writer, val interface{}, _ *[8]byte) error {
	lnd, ok := val.(*LocalNoncesData)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "*lnwire.LocalNoncesData")
	}

	var numEntries uint16
	var sortedEntries []LocalNonceEntry

	if lnd.NoncesMap != nil && len(lnd.NoncesMap) > 0 {
		sortedEntries = make([]LocalNonceEntry, 0, len(lnd.NoncesMap))
		for txid, nonce := range lnd.NoncesMap {
			sortedEntries = append(sortedEntries, LocalNonceEntry{TXID: txid, Nonce: nonce})
		}

		sort.Slice(sortedEntries, func(i, j int) bool {
			return bytes.Compare(sortedEntries[i].TXID[:], sortedEntries[j].TXID[:]) < 0
		})
		numEntries = uint16(len(sortedEntries))
	}

	// Write numEntries
	var uint16Bytes [2]byte
	binary.BigEndian.PutUint16(uint16Bytes[:], numEntries)
	if _, err := w.Write(uint16Bytes[:]); err != nil {
		return err
	}

	// Write actual entries
	for _, entry := range sortedEntries {
		if _, err := w.Write(entry.TXID[:]); err != nil {
			return err
		}
		if _, err := w.Write(entry.Nonce[:]); err != nil {
			return err
		}
	}
	return nil
}

// decodeLocalNoncesData implements the tlv.Decoder for LocalNoncesData.
// decodeLocalNoncesData implements the tlv.Decoder for LocalNoncesData.
func decodeLocalNoncesData(r io.Reader, val interface{}, _ *[8]byte, recordLen uint64) error {
	lnd, ok := val.(*LocalNoncesData)
	if !ok {
		return tlv.NewTypeForDecodingErr(val, "*lnwire.LocalNoncesData", recordLen, 0)
	}

	// Ensure the map is initialized. This handles cases where an uninitialized
	// LocalNoncesData might be passed, or if we want to ensure it's fresh.
	// If NewLocalNoncesData was used, NoncesMap would already be non-nil.
	// For decoding, we want to populate the passed 'val'.
	if lnd.NoncesMap == nil {
		lnd.NoncesMap = make(map[chainhash.Hash]Musig2Nonce)
	}

	if recordLen < 2 {
		// If recordLen is 0, it means an empty TLV value, which is valid for 0 entries.
		// Ensure the map is empty in this case.
		if recordLen == 0 {
			// Clear if it had previous entries
		if len(lnd.NoncesMap) > 0 {
				lnd.NoncesMap = make(map[chainhash.Hash]Musig2Nonce)
			}
			return nil
		}
		// Otherwise, it's too short to even read numEntries.
		return tlv.NewTypeForDecodingErr(lnd, "lnwire.LocalNoncesData (record too short for numEntries)", recordLen, 2)
	}

	var numEntriesBytes [2]byte
	if _, err := io.ReadFull(r, numEntriesBytes[:]); err != nil {
		// This could be io.EOF if recordLen was exactly 0 or 1, which is handled by the check above.
		// If recordLen >= 2, io.EOF here would be unexpected.
		return err
	}
	numEntries := binary.BigEndian.Uint16(numEntriesBytes[:])

	// Validate overall length against what numEntries implies.
	// The total record length must be 2 (for numEntries) + numEntries * (size_of_entry).
	expectedTotalRecordLength := uint64(2) + (uint64(numEntries) * (chainhash.HashSize + musig2.PubNonceSize))
	if recordLen != expectedTotalRecordLength {
		return tlv.NewTypeForDecodingErr(
			lnd, "lnwire.LocalNoncesData (record length mismatch)", recordLen, expectedTotalRecordLength,
		)
	}

	// If numEntries is 0, the map should be empty.
	if numEntries == 0 {
		// Clear if it had previous entries
		if len(lnd.NoncesMap) > 0 {
			lnd.NoncesMap = make(map[chainhash.Hash]Musig2Nonce)
		}
		return nil
	}

	// Prepare the map for new entries. Using 'make' here also clears any
	// existing entries if the LocalNoncesData instance is being reused.
	lnd.NoncesMap = make(map[chainhash.Hash]Musig2Nonce, numEntries)

	for i := uint16(0); i < numEntries; i++ {
		var txid chainhash.Hash
		var nonce Musig2Nonce

		// Should be UnexpectedEOF if recordLen was miscalculated or stream ends early
		if _, err := io.ReadFull(r, txid[:]); err != nil {
			return err
		}

		// Similar to above
		if _, err := io.ReadFull(r, nonce[:]); err != nil {
			return err
		}
		lnd.NoncesMap[txid] = nonce
	}

	return nil
}

// Compile-time checks to ensure LocalNoncesData implements the tlv.RecordProducer interface.
var _ tlv.RecordProducer = (*LocalNoncesData)(nil)

// OptLocalNonces is a type alias for the optional TLV structure.
type OptLocalNonces = fn.Option[LocalNoncesData]

// SomeLocalNonces is a helper function to create an fn.Option[LocalNoncesData]
// with the given data.
func SomeLocalNonces(data LocalNoncesData) OptLocalNonces {
	return fn.Some(data)
}

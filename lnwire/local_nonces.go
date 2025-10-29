package lnwire

import (
	"bytes"
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

// localNonceEntry holds a single TXID -> Musig2Nonce mapping.
type localNonceEntry struct {
	txid chainhash.Hash

	nonce Musig2Nonce
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
		func() uint64 {
			if len(lnd.NoncesMap) == 0 {
				return 0
			}

			numEntries := len(lnd.NoncesMap)
			entrySize := chainhash.HashSize + musig2.PubNonceSize
			return uint64(numEntries * entrySize)
		}(),
		encodeLocalNoncesData,
		decodeLocalNoncesData,
	)
}

// encodeLocalNoncesData implements the tlv.Encoder for LocalNoncesData.
func encodeLocalNoncesData(w io.Writer, val any, _ *[8]byte) error {

	lnd, ok := val.(*LocalNoncesData)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "*lnwire.LocalNoncesData")
	}

	var sortedEntries []localNonceEntry

	if len(lnd.NoncesMap) > 0 {
		sortedEntries = make([]localNonceEntry, 0, len(lnd.NoncesMap))
		for txid, nonce := range lnd.NoncesMap {
			sortedEntries = append(
				sortedEntries, localNonceEntry{
					txid: txid, nonce: nonce,
				},
			)
		}

		sort.Slice(sortedEntries, func(i, j int) bool {
			return bytes.Compare(
				sortedEntries[i].txid[:],
				sortedEntries[j].txid[:],
			) < 0
		})
	}

	for _, entry := range sortedEntries {
		if _, err := w.Write(entry.txid[:]); err != nil {
			return err
		}
		if _, err := w.Write(entry.nonce[:]); err != nil {
			return err
		}
	}
	return nil
}

// decodeLocalNoncesData implements the tlv.Decoder for LocalNoncesData.
func decodeLocalNoncesData(r io.Reader, val any, _ *[8]byte,
	recordLen uint64) error {

	lnd, ok := val.(*LocalNoncesData)
	if !ok {
		return tlv.NewTypeForDecodingErr(
			val, "*lnwire.LocalNoncesData", recordLen, 0,
		)
	}

	if lnd.NoncesMap == nil {
		lnd.NoncesMap = make(map[chainhash.Hash]Musig2Nonce)
	}

	// If recordLen is 0, it means an empty TLV value, which is valid for
	// 0 entries. Ensure the map is empty in this case.
	if recordLen == 0 {
		// Clear if it had previous entries.
		if len(lnd.NoncesMap) > 0 {
			lnd.NoncesMap = make(map[chainhash.Hash]Musig2Nonce)
		}

		return nil
	}

	// Each entry is a fixed size: TXID (32 bytes) + Nonce (66 bytes). We
	// can use this to compute the number of expected entries and perform a
	// sanity check while we're at it.
	const entrySize = chainhash.HashSize + musig2.PubNonceSize
	if recordLen%entrySize != 0 {
		return tlv.NewTypeForDecodingErr(
			lnd, "lnwire.LocalNoncesData (record length not "+
				"evenly divisible by entry size)",
			recordLen, 0,
		)
	}

	numEntries := recordLen / entrySize

	// Prepare the map for new entries. Using 'make' here also clears any
	// existing entries if the LocalNoncesData instance is being reused.
	lnd.NoncesMap = make(map[chainhash.Hash]Musig2Nonce, numEntries)

	for i := uint64(0); i < numEntries; i++ {
		var (
			txid  chainhash.Hash
			nonce Musig2Nonce
		)

		if _, err := io.ReadFull(r, txid[:]); err != nil {
			return err
		}
		if _, err := io.ReadFull(r, nonce[:]); err != nil {
			return err
		}

		lnd.NoncesMap[txid] = nonce
	}

	return nil
}

var _ tlv.RecordProducer = (*LocalNoncesData)(nil)

// OptLocalNonces is a type alias for the optional TLV structure.
type OptLocalNonces = fn.Option[LocalNoncesData]

// SomeLocalNonces is a helper function to create an fn.Option[LocalNoncesData]
// with the given data.
func SomeLocalNonces(data LocalNoncesData) OptLocalNonces {
	return fn.Some(data)
}

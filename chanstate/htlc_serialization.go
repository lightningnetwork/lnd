package chanstate

import (
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// serializeHtlcExtraData encodes a TLV stream of extra data to be stored with a
// HTLC. It uses the update_add_htlc TLV types, because this is where extra
// data is passed with a HTLC. At present blinding points are the only extra
// data that we will store, and the function is a no-op if a nil blinding
// point is provided.
//
// This function MUST be called to persist all HTLC values when they are
// serialized.
func serializeHtlcExtraData(h *HTLC) error {
	var records []tlv.RecordProducer
	h.BlindingPoint.WhenSome(func(b tlv.RecordT[lnwire.BlindingPointTlvType,
		*btcec.PublicKey]) {

		records = append(records, &b)
	})

	records, err := h.CustomRecords.ExtendRecordProducers(records)
	if err != nil {
		return err
	}

	return h.ExtraData.PackRecords(records...)
}

// deserializeHtlcExtraData extracts TLVs from the extra data persisted for the
// HTLC and populates values in the struct accordingly.
//
// This function MUST be called to populate the struct properly when HTLCs
// are deserialized.
func deserializeHtlcExtraData(h *HTLC) error {
	if len(h.ExtraData) == 0 {
		return nil
	}

	blindingPoint := h.BlindingPoint.Zero()
	tlvMap, err := h.ExtraData.ExtractRecords(&blindingPoint)
	if err != nil {
		return err
	}

	if val, ok := tlvMap[h.BlindingPoint.TlvType()]; ok && val == nil {
		h.BlindingPoint = tlv.SomeRecordT(blindingPoint)

		// Remove the entry from the TLV map. Anything left in the map
		// will be included in the custom records field.
		delete(tlvMap, h.BlindingPoint.TlvType())
	}

	// Set the custom records field to the remaining TLV records.
	customRecords, err := lnwire.NewCustomRecords(tlvMap)
	if err != nil {
		return err
	}
	h.CustomRecords = customRecords

	return nil
}

// SerializeHtlcs writes out the passed set of HTLC's into the passed writer
// using the current default on-disk serialization format.
//
// This inline serialization has been extended to allow storage of extra data
// associated with a HTLC in the following way:
//   - The known-length onion blob (1366 bytes) is serialized as var bytes in
//     WriteElements (ie, the length 1366 was written, followed by the 1366
//     onion bytes).
//   - To include extra data, we append any extra data present to this one
//     variable length of data. Since we know that the onion is strictly 1366
//     bytes, any length after that should be considered to be extra data.
//
// NOTE: This API is NOT stable, the on-disk format will likely change in the
// future.
func SerializeHtlcs(b io.Writer, htlcs ...HTLC) error {
	numHtlcs := uint16(len(htlcs))
	if err := WriteElement(b, numHtlcs); err != nil {
		return err
	}

	for _, htlc := range htlcs {
		// Populate TLV stream for any additional fields contained
		// in the TLV.
		if err := serializeHtlcExtraData(&htlc); err != nil {
			return err
		}

		// The onion blob and hltc data are stored as a single var
		// bytes blob.
		onionAndExtraData := make(
			[]byte, lnwire.OnionPacketSize+len(htlc.ExtraData),
		)
		copy(onionAndExtraData, htlc.OnionBlob[:])
		copy(onionAndExtraData[lnwire.OnionPacketSize:], htlc.ExtraData)

		if err := WriteElements(b,
			//nolint:ll
			htlc.Signature, htlc.RHash, htlc.Amt, htlc.RefundTimeout,
			htlc.OutputIndex, htlc.Incoming, onionAndExtraData,
			htlc.HtlcIndex, htlc.LogIndex,
		); err != nil {
			return err
		}
	}

	return nil
}

// DeserializeHtlcs attempts to read out a slice of HTLC's from the passed
// io.Reader. The bytes within the passed reader MUST have been previously
// written to using the SerializeHtlcs function.
//
// This inline deserialization has been extended to allow storage of extra data
// associated with a HTLC in the following way:
//   - The known-length onion blob (1366 bytes) and any additional data present
//     are read out as a single blob of variable byte data.
//   - They are stored like this to take advantage of the variable space
//     available for extension without migration (see SerializeHtlcs).
//   - The first 1366 bytes are interpreted as the onion blob, and any remaining
//     bytes as extra HTLC data.
//   - This extra HTLC data is expected to be serialized as a TLV stream, and
//     its parsing is left to higher layers.
//
// NOTE: This API is NOT stable, the on-disk format will likely change in the
// future.
func DeserializeHtlcs(r io.Reader) ([]HTLC, error) {
	var numHtlcs uint16
	if err := ReadElement(r, &numHtlcs); err != nil {
		return nil, err
	}

	var htlcs []HTLC
	if numHtlcs == 0 {
		return htlcs, nil
	}

	htlcs = make([]HTLC, numHtlcs)
	for i := uint16(0); i < numHtlcs; i++ {
		var onionAndExtraData []byte
		if err := ReadElements(r,
			&htlcs[i].Signature, &htlcs[i].RHash, &htlcs[i].Amt,
			&htlcs[i].RefundTimeout, &htlcs[i].OutputIndex,
			&htlcs[i].Incoming, &onionAndExtraData,
			&htlcs[i].HtlcIndex, &htlcs[i].LogIndex,
		); err != nil {
			return htlcs, err
		}

		// Sanity check that we have at least the onion blob size we
		// expect.
		if len(onionAndExtraData) < lnwire.OnionPacketSize {
			return nil, ErrOnionBlobLength
		}

		// First OnionPacketSize bytes are our fixed length onion
		// packet.
		copy(
			htlcs[i].OnionBlob[:],
			onionAndExtraData[0:lnwire.OnionPacketSize],
		)

		// Any additional bytes belong to extra data. ExtraDataLen
		// will be >= 0, because we know that we always have a fixed
		// length onion packet.
		extraDataLen := len(onionAndExtraData) - lnwire.OnionPacketSize
		if extraDataLen > 0 {
			htlcs[i].ExtraData = make([]byte, extraDataLen)

			copy(
				htlcs[i].ExtraData,
				onionAndExtraData[lnwire.OnionPacketSize:],
			)
		}

		// Finally, deserialize any TLVs contained in that extra data
		// if they are present.
		if err := deserializeHtlcExtraData(&htlcs[i]); err != nil {
			return nil, err
		}
	}

	return htlcs, nil
}

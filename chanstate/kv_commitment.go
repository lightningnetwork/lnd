package chanstate

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
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

// SerializeChanCommit serializes the channel commitment.
func SerializeChanCommit(w io.Writer, c *ChannelCommitment) error {
	if err := WriteElements(w,
		c.CommitHeight, c.LocalLogIndex, c.LocalHtlcIndex,
		c.RemoteLogIndex, c.RemoteHtlcIndex, c.LocalBalance,
		c.RemoteBalance, c.CommitFee, c.FeePerKw, c.CommitTx,
		c.CommitSig,
	); err != nil {
		return err
	}

	return SerializeHtlcs(w, c.Htlcs...)
}

// DeserializeChanCommit deserializes the channel commitment.
func DeserializeChanCommit(r io.Reader) (ChannelCommitment, error) {
	var c ChannelCommitment

	err := ReadElements(r,
		&c.CommitHeight, &c.LocalLogIndex, &c.LocalHtlcIndex,
		&c.RemoteLogIndex, &c.RemoteHtlcIndex, &c.LocalBalance,
		&c.RemoteBalance, &c.CommitFee, &c.FeePerKw, &c.CommitTx,
		&c.CommitSig,
	)
	if err != nil {
		return c, err
	}

	c.Htlcs, err = DeserializeHtlcs(r)
	if err != nil {
		return c, err
	}

	return c, nil
}

// commitTlvData stores all the optional data that may be stored as a TLV stream
// at the _end_ of the normal serialized commit on disk.
type commitTlvData struct {
	// customBlob is a custom blob that may store extra data for custom
	// channels.
	customBlob tlv.OptionalRecordT[tlv.TlvType1, tlv.Blob]
}

// encode encodes the aux data into the passed io.Writer.
func (c *commitTlvData) encode(w io.Writer) error {
	var tlvRecords []tlv.Record
	c.customBlob.WhenSome(func(blob tlv.RecordT[tlv.TlvType1, tlv.Blob]) {
		tlvRecords = append(tlvRecords, blob.Record())
	})

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// decode attempts to decode the aux data from the passed io.Reader.
func (c *commitTlvData) decode(r io.Reader) error {
	blob := c.customBlob.Zero()

	tlvStream, err := tlv.NewStream(
		blob.Record(),
	)
	if err != nil {
		return err
	}

	tlvs, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := tlvs[c.customBlob.TlvType()]; ok {
		c.customBlob = tlv.SomeRecordT(blob)
	}

	return nil
}

// DecodeCommitTlvData decodes and applies auxiliary TLV data to a commitment.
func DecodeCommitTlvData(r io.Reader, c *ChannelCommitment) error {
	var auxData commitTlvData
	if err := auxData.decode(r); err != nil {
		return err
	}

	amendCommitTlvData(c, auxData)

	return nil
}

// EncodeCommitTlvData extracts and encodes auxiliary TLV data from a
// commitment.
func EncodeCommitTlvData(w io.Writer, c *ChannelCommitment) error {
	auxData := extractCommitTlvData(c)
	return auxData.encode(w)
}

// amendCommitTlvData updates the commitment with the given auxiliary TLV data.
func amendCommitTlvData(c *ChannelCommitment, auxData commitTlvData) {
	auxData.customBlob.WhenSomeV(func(blob tlv.Blob) {
		c.CustomBlob = fn.Some(blob)
	})
}

// extractCommitTlvData creates a new commitTlvData from the given commitment.
func extractCommitTlvData(c *ChannelCommitment) commitTlvData {
	var auxData commitTlvData

	c.CustomBlob.WhenSome(func(blob tlv.Blob) {
		auxData.customBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType1](blob),
		)
	})

	return auxData
}

// SerializeLogUpdates serializes provided list of updates to a stream.
func SerializeLogUpdates(w io.Writer, logUpdates []LogUpdate) error {
	numUpdates := uint16(len(logUpdates))
	if err := binary.Write(w, byteOrder, numUpdates); err != nil {
		return err
	}

	for _, diff := range logUpdates {
		err := WriteElements(w, diff.LogIndex, diff.UpdateMsg)
		if err != nil {
			return err
		}
	}

	return nil
}

// DeserializeLogUpdates deserializes a list of updates from a stream.
func DeserializeLogUpdates(r io.Reader) ([]LogUpdate, error) {
	var numUpdates uint16
	if err := binary.Read(r, byteOrder, &numUpdates); err != nil {
		return nil, err
	}

	logUpdates := make([]LogUpdate, numUpdates)
	for i := 0; i < int(numUpdates); i++ {
		err := ReadElements(r,
			&logUpdates[i].LogIndex, &logUpdates[i].UpdateMsg,
		)
		if err != nil {
			return nil, err
		}
	}

	return logUpdates, nil
}

// SerializeCommitDiff serializes the commit diff.
func SerializeCommitDiff(w io.Writer, diff *CommitDiff) error {
	if err := SerializeChanCommit(w, &diff.Commitment); err != nil {
		return err
	}

	if err := WriteElements(w, diff.CommitSig); err != nil {
		return err
	}

	if err := SerializeLogUpdates(w, diff.LogUpdates); err != nil {
		return err
	}

	numOpenRefs := uint16(len(diff.OpenedCircuitKeys))
	if err := binary.Write(w, byteOrder, numOpenRefs); err != nil {
		return err
	}

	for _, openRef := range diff.OpenedCircuitKeys {
		err := WriteElements(w, openRef.ChanID, openRef.HtlcID)
		if err != nil {
			return err
		}
	}

	numClosedRefs := uint16(len(diff.ClosedCircuitKeys))
	if err := binary.Write(w, byteOrder, numClosedRefs); err != nil {
		return err
	}

	for _, closedRef := range diff.ClosedCircuitKeys {
		err := WriteElements(w, closedRef.ChanID, closedRef.HtlcID)
		if err != nil {
			return err
		}
	}

	// We'll also encode the commit aux data stream here. We do this here
	// rather than above (at the call to serializeChanCommit), to ensure
	// backwards compat for reads to existing non-custom channels.
	if err := EncodeCommitTlvData(w, &diff.Commitment); err != nil {
		return fmt.Errorf("unable to write aux data: %w", err)
	}

	return nil
}

// DeserializeCommitDiff deserializes the commit diff.
func DeserializeCommitDiff(r io.Reader) (*CommitDiff, error) {
	var (
		d   CommitDiff
		err error
	)

	d.Commitment, err = DeserializeChanCommit(r)
	if err != nil {
		return nil, err
	}

	var msg lnwire.Message
	if err := ReadElements(r, &msg); err != nil {
		return nil, err
	}
	commitSig, ok := msg.(*lnwire.CommitSig)
	if !ok {
		return nil, fmt.Errorf("expected lnwire.CommitSig, instead "+
			"read: %T", msg)
	}
	d.CommitSig = commitSig

	d.LogUpdates, err = DeserializeLogUpdates(r)
	if err != nil {
		return nil, err
	}

	var numOpenRefs uint16
	if err := binary.Read(r, byteOrder, &numOpenRefs); err != nil {
		return nil, err
	}

	d.OpenedCircuitKeys = make([]models.CircuitKey, numOpenRefs)
	for i := 0; i < int(numOpenRefs); i++ {
		err := ReadElements(r,
			&d.OpenedCircuitKeys[i].ChanID,
			&d.OpenedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	var numClosedRefs uint16
	if err := binary.Read(r, byteOrder, &numClosedRefs); err != nil {
		return nil, err
	}

	d.ClosedCircuitKeys = make([]models.CircuitKey, numClosedRefs)
	for i := 0; i < int(numClosedRefs); i++ {
		err := ReadElements(r,
			&d.ClosedCircuitKeys[i].ChanID,
			&d.ClosedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	// As a final step, we'll read out any aux commit data that we have at
	// the end of this byte stream. We do this here to ensure backward
	// compatibility, as otherwise we risk erroneously reading into the
	// wrong field.
	if err := DecodeCommitTlvData(r, &d.Commitment); err != nil {
		return nil, fmt.Errorf("unable to decode aux data: %w", err)
	}

	return &d, nil
}

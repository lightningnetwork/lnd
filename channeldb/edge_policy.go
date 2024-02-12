package channeldb

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	EdgePolicy2MsgType = tlv.Type(0)
	EdgePolicy2ToNode  = tlv.Type(1)

	// chanEdgePolicyNewEncodingPrefix is a byte used in the channel edge
	// policy encoding to signal that the new style encoding which is
	// prefixed with a type byte is being used instead of the legacy
	// encoding which would start with 0x02 due to the fact that the
	// encoding would start with a DER encoded ecdsa signature.
	chanEdgePolicyNewEncodingPrefix = 0xff
)

// edgePolicyEncoding indicates how the bytes for a channel edge policy have
// been serialised.
type edgePolicyEncodingType uint8

const (
	// edgePolicy2EncodingType will be used as a prefix for edge policies
	// advertised using the ChannelUpdate2 message. The type indicates how
	// the bytes following should be deserialized.
	edgePolicy2EncodingType edgePolicyEncodingType = 0
)

func putChanEdgePolicy(edges kvdb.RwBucket, edge *models.ChannelEdgePolicy1,
	from, to []byte) error {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], from)
	byteOrder.PutUint64(edgeKey[33:], edge.ChannelID)

	var b bytes.Buffer
	if err := serializeChanEdgePolicy(&b, edge, to); err != nil {
		return err
	}

	// Before we write out the new edge, we'll create a new entry in the
	// update index in order to keep it fresh.
	updateUnix := uint64(edge.LastUpdate.Unix())
	var indexKey [8 + 8]byte
	byteOrder.PutUint64(indexKey[:8], updateUnix)
	byteOrder.PutUint64(indexKey[8:], edge.ChannelID)

	updateIndex, err := edges.CreateBucketIfNotExists(edgeUpdateIndexBucket)
	if err != nil {
		return err
	}

	// If there was already an entry for this edge, then we'll need to
	// delete the old one to ensure we don't leave around any after-images.
	// An unknown policy value does not have a update time recorded, so
	// it also does not need to be removed.
	if edgeBytes := edges.Get(edgeKey[:]); edgeBytes != nil &&
		!bytes.Equal(edgeBytes, unknownPolicy) {

		// In order to delete the old entry, we'll need to obtain the
		// *prior* update time in order to delete it. To do this, we'll
		// need to deserialize the existing policy within the database
		// (now outdated by the new one), and delete its corresponding
		// entry within the update index. We'll ignore any
		// ErrEdgePolicyOptionalFieldNotFound error, as we only need
		// the channel ID and update time to delete the entry.
		// TODO(halseth): get rid of these invalid policies in a
		// migration.
		oldEdgePolicy, err := deserializeChanEdgePolicy(
			bytes.NewReader(edgeBytes),
		)
		if err != nil &&
			!errors.Is(err, ErrEdgePolicyOptionalFieldNotFound) {

			return err
		}

		oldPol, ok := oldEdgePolicy.(*models.ChannelEdgePolicy1)
		if !ok {
			return fmt.Errorf("expected "+
				"*models.ChannelEdgePolicy1, got: %T",
				oldEdgePolicy)
		}

		oldUpdateTime := uint64(oldPol.LastUpdate.Unix())

		var oldIndexKey [8 + 8]byte
		byteOrder.PutUint64(oldIndexKey[:8], oldUpdateTime)
		byteOrder.PutUint64(oldIndexKey[8:], edge.ChannelID)

		if err := updateIndex.Delete(oldIndexKey[:]); err != nil {
			return err
		}
	}

	if err := updateIndex.Put(indexKey[:], nil); err != nil {
		return err
	}

	err = updateEdgePolicyDisabledIndex(
		edges, edge.ChannelID,
		edge.ChannelFlags&lnwire.ChanUpdateDirection > 0,
		edge.IsDisabled(),
	)
	if err != nil {
		return err
	}

	return edges.Put(edgeKey[:], b.Bytes())
}

// updateEdgePolicyDisabledIndex is used to update the disabledEdgePolicyIndex
// bucket by either add a new disabled ChannelEdgePolicy1 or remove an existing
// one.
// The direction represents the direction of the edge and disabled is used for
// deciding whether to remove or add an entry to the bucket.
// In general a channel is disabled if two entries for the same chanID exist
// in this bucket.
// Maintaining the bucket this way allows a fast retrieval of disabled
// channels, for example when prune is needed.
func updateEdgePolicyDisabledIndex(edges kvdb.RwBucket, chanID uint64,
	direction bool, disabled bool) error {

	var disabledEdgeKey [8 + 1]byte
	byteOrder.PutUint64(disabledEdgeKey[0:], chanID)
	if direction {
		disabledEdgeKey[8] = 1
	}

	disabledEdgePolicyIndex, err := edges.CreateBucketIfNotExists(
		disabledEdgePolicyBucket,
	)
	if err != nil {
		return err
	}

	if disabled {
		return disabledEdgePolicyIndex.Put(disabledEdgeKey[:], []byte{})
	}

	return disabledEdgePolicyIndex.Delete(disabledEdgeKey[:])
}

// putChanEdgePolicyUnknown marks the edge policy as unknown
// in the edges bucket.
func putChanEdgePolicyUnknown(edges kvdb.RwBucket, channelID uint64,
	from []byte) error {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], from)
	byteOrder.PutUint64(edgeKey[33:], channelID)

	if edges.Get(edgeKey[:]) != nil {
		return fmt.Errorf("cannot write unknown policy for channel %v "+
			" when there is already a policy present", channelID)
	}

	return edges.Put(edgeKey[:], unknownPolicy)
}

func fetchChanEdgePolicy(edges kvdb.RBucket, chanID []byte, nodePub []byte) (
	*models.ChannelEdgePolicy1, error) {

	var edgeKey [33 + 8]byte
	copy(edgeKey[:], nodePub)
	copy(edgeKey[33:], chanID)

	edgeBytes := edges.Get(edgeKey[:])
	if edgeBytes == nil {
		return nil, ErrEdgeNotFound
	}

	// No need to deserialize unknown policy.
	if bytes.Equal(edgeBytes, unknownPolicy) {
		return nil, nil
	}

	edgeReader := bytes.NewReader(edgeBytes)

	ep, err := deserializeChanEdgePolicy(edgeReader)
	switch {
	// If the db policy was missing an expected optional field, we return
	// nil as if the policy was unknown.
	case errors.Is(err, ErrEdgePolicyOptionalFieldNotFound):
		return nil, nil

	case err != nil:
		return nil, err
	}

	pol, ok := ep.(*models.ChannelEdgePolicy1)
	if !ok {
		return nil, fmt.Errorf("expected *models.ChannelEdgePolicy1, "+
			"got: %T", ep)
	}

	return pol, nil
}

func fetchChanEdgePolicies(edgeIndex kvdb.RBucket, edges kvdb.RBucket,
	chanID []byte) (*models.ChannelEdgePolicy1, *models.ChannelEdgePolicy1,
	error) {

	edgeInfoBytes := edgeIndex.Get(chanID)
	if edgeInfoBytes == nil {
		return nil, nil, ErrEdgeNotFound
	}

	edgeInfo, err := deserializeChanEdgeInfo(bytes.NewReader(edgeInfoBytes))
	if err != nil {
		return nil, nil, err
	}

	node1Pub := edgeInfo.Node1Bytes()
	edge1, err := fetchChanEdgePolicy(edges, chanID, node1Pub[:])
	if err != nil {
		return nil, nil, err
	}

	node2Pub := edgeInfo.Node2Bytes()
	edge2, err := fetchChanEdgePolicy(edges, chanID, node2Pub[:])
	if err != nil {
		return nil, nil, err
	}

	return edge1, edge2, nil
}

func serializeChanEdgePolicy(w io.Writer,
	edgePolicy models.ChannelEdgePolicy, toNode []byte) error {

	var (
		withTypeByte bool
		typeByte     edgePolicyEncodingType
		serialize    func(w io.Writer) error
	)

	switch policy := edgePolicy.(type) {
	case *models.ChannelEdgePolicy1:
		serialize = func(w io.Writer) error {
			copy(policy.ToNode[:], toNode)

			return serializeChanEdgePolicy1(w, policy)
		}
	case *models.ChannelEdgePolicy2:
		withTypeByte = true
		typeByte = edgePolicy2EncodingType

		serialize = func(w io.Writer) error {
			copy(policy.ToNode[:], toNode)

			return serializeChanEdgePolicy2(w, policy)
		}
	default:
		return fmt.Errorf("unhandled implementation of "+
			"ChannelEdgePolicy: %T", edgePolicy)
	}

	if withTypeByte {
		// First, write the identifying encoding byte to signal that
		// this is not using the legacy encoding.
		_, err := w.Write([]byte{chanEdgePolicyNewEncodingPrefix})
		if err != nil {
			return err
		}

		// Now, write the encoding type.
		_, err = w.Write([]byte{byte(typeByte)})
		if err != nil {
			return err
		}
	}

	return serialize(w)
}

func serializeChanEdgePolicy1(w io.Writer,
	edge *models.ChannelEdgePolicy1) error {

	err := wire.WriteVarBytes(w, 0, edge.SigBytes)
	if err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, edge.ChannelID); err != nil {
		return err
	}

	var scratch [8]byte
	updateUnix := uint64(edge.LastUpdate.Unix())
	byteOrder.PutUint64(scratch[:], updateUnix)
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, edge.MessageFlags); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, edge.ChannelFlags); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, edge.TimeLockDelta); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, uint64(edge.MinHTLC)); err != nil {
		return err
	}
	err = binary.Write(w, byteOrder, uint64(edge.FeeBaseMSat))
	if err != nil {
		return err
	}
	err = binary.Write(w, byteOrder, uint64(edge.FeeProportionalMillionths))
	if err != nil {
		return err
	}

	if _, err := w.Write(edge.ToNode[:]); err != nil {
		return err
	}

	// If the max_htlc field is present, we write it. To be compatible with
	// older versions that wasn't aware of this field, we write it as part
	// of the opaque data.
	// TODO(halseth): clean up when moving to TLV.
	var opaqueBuf bytes.Buffer
	if edge.MessageFlags.HasMaxHtlc() {
		err := binary.Write(&opaqueBuf, byteOrder, uint64(edge.MaxHTLC))
		if err != nil {
			return err
		}
	}

	if len(edge.ExtraOpaqueData) > MaxAllowedExtraOpaqueBytes {
		return ErrTooManyExtraOpaqueBytes(len(edge.ExtraOpaqueData))
	}
	if _, err := opaqueBuf.Write(edge.ExtraOpaqueData); err != nil {
		return err
	}

	return wire.WriteVarBytes(w, 0, opaqueBuf.Bytes())
}

func serializeChanEdgePolicy2(w io.Writer,
	edge *models.ChannelEdgePolicy2) error {

	if len(edge.ExtraOpaqueData) > MaxAllowedExtraOpaqueBytes {
		return ErrTooManyExtraOpaqueBytes(len(edge.ExtraOpaqueData))
	}

	var b bytes.Buffer
	if err := edge.Encode(&b, 0); err != nil {
		return err
	}

	msg := b.Bytes()

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(EdgePolicy2MsgType, &msg),
		tlv.MakePrimitiveRecord(EdgePolicy2ToNode, &edge.ToNode),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

func deserializeChanEdgePolicy(r io.Reader) (models.ChannelEdgePolicy,
	error) {

	// Deserialize the policy. Note that in case an optional field is not
	// found, both an error and a populated policy object are returned.
	edge, deserializeErr := deserializeChanEdgePolicyRaw(r)
	if deserializeErr != nil &&
		!errors.Is(deserializeErr, ErrEdgePolicyOptionalFieldNotFound) {

		return nil, deserializeErr
	}

	return edge, deserializeErr
}

func deserializeChanEdgePolicy1Raw(r io.Reader) (*models.ChannelEdgePolicy1,
	error) {

	var edge models.ChannelEdgePolicy1

	var err error
	edge.SigBytes, err = wire.ReadVarBytes(r, 0, 80, "sig")
	if err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &edge.ChannelID); err != nil {
		return nil, err
	}

	var scratch [8]byte
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	unix := int64(byteOrder.Uint64(scratch[:]))
	edge.LastUpdate = time.Unix(unix, 0)

	if err := binary.Read(r, byteOrder, &edge.MessageFlags); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edge.ChannelFlags); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &edge.TimeLockDelta); err != nil {
		return nil, err
	}

	var n uint64
	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.MinHTLC = lnwire.MilliSatoshi(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.FeeBaseMSat = lnwire.MilliSatoshi(n)

	if err := binary.Read(r, byteOrder, &n); err != nil {
		return nil, err
	}
	edge.FeeProportionalMillionths = lnwire.MilliSatoshi(n)

	if _, err := r.Read(edge.ToNode[:]); err != nil {
		return nil, err
	}

	// We'll try and see if there are any opaque bytes left, if not, then
	// we'll ignore the EOF error and return the edge as is.
	edge.ExtraOpaqueData, err = wire.ReadVarBytes(
		r, 0, MaxAllowedExtraOpaqueBytes, "blob",
	)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, err
	}

	// See if optional fields are present.
	if edge.MessageFlags.HasMaxHtlc() {
		// The max_htlc field should be at the beginning of the opaque
		// bytes.
		opq := edge.ExtraOpaqueData

		// If the max_htlc field is not present, it might be old data
		// stored before this field was validated. We'll return the
		// edge along with an error.
		if len(opq) < 8 {
			return &edge, ErrEdgePolicyOptionalFieldNotFound
		}

		maxHtlc := byteOrder.Uint64(opq[:8])
		edge.MaxHTLC = lnwire.MilliSatoshi(maxHtlc)

		// Exclude the parsed field from the rest of the opaque data.
		edge.ExtraOpaqueData = opq[8:]
	}

	return &edge, nil
}

func deserializeChanEdgePolicyRaw(reader io.Reader) (models.ChannelEdgePolicy,
	error) {

	// Wrap the io.Reader in a bufio.Reader so that we can peak the first
	// byte of the stream without actually consuming from the stream.
	r := bufio.NewReader(reader)

	firstByte, err := r.Peek(1)
	if err != nil {
		return nil, err
	}

	if firstByte[0] != chanEdgePolicyNewEncodingPrefix {
		return deserializeChanEdgePolicy1Raw(r)
	}

	// Pop the encoding type byte.
	var scratch [1]byte
	if _, err = r.Read(scratch[:]); err != nil {
		return nil, err
	}

	// Now, read the encoding type byte.
	if _, err = r.Read(scratch[:]); err != nil {
		return nil, err
	}

	encoding := edgePolicyEncodingType(scratch[0])
	switch encoding {
	case edgePolicy2EncodingType:
		return deserializeChanEdgePolicy2Raw(r)

	default:
		return nil, fmt.Errorf("unknown edge policy encoding type: %d",
			encoding)
	}
}

func deserializeChanEdgePolicy2Raw(r io.Reader) (*models.ChannelEdgePolicy2,
	error) {

	var (
		msgBytes []byte
		toNode   [33]byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(EdgePolicy2MsgType, &msgBytes),
		tlv.MakePrimitiveRecord(EdgePolicy2ToNode, &toNode),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	err = stream.Decode(r)
	if err != nil {
		return nil, err
	}

	var (
		chanUpdate lnwire.ChannelUpdate2
		reader     = bytes.NewReader(msgBytes)
	)
	err = chanUpdate.Decode(reader, 0)
	if err != nil {
		return nil, err
	}

	return &models.ChannelEdgePolicy2{
		ChannelUpdate2: chanUpdate,
		ToNode:         toNode,
	}, nil
}

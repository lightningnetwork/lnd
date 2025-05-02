package migration32

import (
	"bytes"
	"io"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// unknownFailureSourceIdx is the database encoding of an unknown error
	// source.
	unknownFailureSourceIdx = -1
)

var (
	// resultsKey is the fixed key under which the attempt results are
	// stored.
	resultsKey = []byte("missioncontrol-results")
)

// paymentResultOld is the information that becomes available when a payment
// attempt completes.
type paymentResultOld struct {
	id                 uint64
	timeFwd, timeReply time.Time
	route              *Route
	success            bool
	failureSourceIdx   *int
	failure            lnwire.FailureMessage
}

// deserializeOldResult deserializes a payment result using the old encoding.
func deserializeOldResult(k, v []byte) (*paymentResultOld, error) {
	// Parse payment id.
	result := paymentResultOld{
		id: byteOrder.Uint64(k[8:]),
	}

	r := bytes.NewReader(v)

	// Read timestamps, success status and failure source index.
	var (
		timeFwd, timeReply uint64
		dbFailureSourceIdx int32
	)

	err := ReadElements(
		r, &timeFwd, &timeReply, &result.success, &dbFailureSourceIdx,
	)
	if err != nil {
		return nil, err
	}

	// Convert time stamps to local time zone for consistent logging.
	result.timeFwd = time.Unix(0, int64(timeFwd)).Local()
	result.timeReply = time.Unix(0, int64(timeReply)).Local()

	// Convert from unknown index magic number to nil value.
	if dbFailureSourceIdx != unknownFailureSourceIdx {
		failureSourceIdx := int(dbFailureSourceIdx)
		result.failureSourceIdx = &failureSourceIdx
	}

	// Read route.
	route, err := DeserializeRoute(r)
	if err != nil {
		return nil, err
	}
	result.route = &route

	// Read failure.
	failureBytes, err := wire.ReadVarBytes(r, 0, math.MaxUint16, "failure")
	if err != nil {
		return nil, err
	}
	if len(failureBytes) > 0 {
		result.failure, err = lnwire.DecodeFailureMessage(
			bytes.NewReader(failureBytes), 0,
		)
		if err != nil {
			return nil, err
		}
	}

	return &result, nil
}

// convertPaymentResult converts a paymentResultOld to a paymentResultNew.
func convertPaymentResult(old *paymentResultOld) *paymentResultNew {
	var failure fn.Option[paymentFailure]
	if !old.success {
		failure = fn.Some(newPaymentFailure(
			old.failureSourceIdx,
			old.failure,
		))
	}

	return newPaymentResult(
		old.id, extractMCRoute(old.route), old.timeFwd, old.timeReply,
		failure,
	)
}

// newPaymentResult constructs a new paymentResult.
func newPaymentResult(id uint64, rt *mcRoute, timeFwd, timeReply time.Time,
	failure fn.Option[paymentFailure]) *paymentResultNew {

	result := &paymentResultNew{
		id: id,
		timeFwd: tlv.NewPrimitiveRecord[tlv.TlvType0](
			uint64(timeFwd.UnixNano()),
		),
		timeReply: tlv.NewPrimitiveRecord[tlv.TlvType1](
			uint64(timeReply.UnixNano()),
		),
		route: tlv.NewRecordT[tlv.TlvType2](*rt),
	}

	failure.WhenSome(func(f paymentFailure) {
		result.failure = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType3](f),
		)
	})

	return result
}

// paymentResultNew is the information that becomes available when a payment
// attempt completes.
type paymentResultNew struct {
	id        uint64
	timeFwd   tlv.RecordT[tlv.TlvType0, uint64]
	timeReply tlv.RecordT[tlv.TlvType1, uint64]
	route     tlv.RecordT[tlv.TlvType2, mcRoute]

	// failure holds information related to the failure of a payment. The
	// presence of this record indicates a payment failure. The absence of
	// this record indicates a successful payment.
	failure tlv.OptionalRecordT[tlv.TlvType3, paymentFailure]
}

// newPaymentFailure constructs a new paymentFailure struct. If the source
// index is nil, then an empty paymentFailure is returned. This represents a
// failure with unknown details. Otherwise, the index and failure message are
// used to populate the info field of the paymentFailure.
func newPaymentFailure(sourceIdx *int,
	failureMsg lnwire.FailureMessage) paymentFailure {

	// If we can't identify a failure source, we also won't have a decrypted
	// failure message. In this case we return an empty payment failure.
	if sourceIdx == nil {
		return paymentFailure{}
	}

	info := paymentFailure{
		sourceIdx: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](
				uint8(*sourceIdx),
			),
		),
	}

	if failureMsg != nil {
		info.msg = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType1](
				failureMessage{failureMsg},
			),
		)
	}

	return info
}

// paymentFailure holds additional information about a payment failure.
type paymentFailure struct {
	// sourceIdx is the hop the error was reported from. In order to be able
	// to decrypt the error message, we need to know the source, which is
	// why an error message can only be present if the source is known.
	sourceIdx tlv.OptionalRecordT[tlv.TlvType0, uint8]

	// msg is the error why a payment failed. If we identify the failure of
	// a certain hop at the above index, but aren't able to decode the
	// failure message we indicate this by not setting this field.
	msg tlv.OptionalRecordT[tlv.TlvType1, failureMessage]
}

// Record returns a TLV record that can be used to encode/decode a
// paymentFailure to/from a TLV stream.
func (r *paymentFailure) Record() tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := encodePaymentFailure(&b, r, &buf); err != nil {
			panic(err)
		}

		return uint64(len(b.Bytes()))
	}

	return tlv.MakeDynamicRecord(
		0, r, recordSize, encodePaymentFailure, decodePaymentFailure,
	)
}

func encodePaymentFailure(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*paymentFailure); ok {
		var recordProducers []tlv.RecordProducer

		v.sourceIdx.WhenSome(
			func(r tlv.RecordT[tlv.TlvType0, uint8]) {
				recordProducers = append(
					recordProducers, &r,
				)
			},
		)

		v.msg.WhenSome(
			func(r tlv.RecordT[tlv.TlvType1, failureMessage]) {
				recordProducers = append(
					recordProducers, &r,
				)
			},
		)

		return lnwire.EncodeRecordsTo(
			w, lnwire.ProduceRecordsSorted(
				recordProducers...,
			),
		)
	}

	return tlv.NewTypeForEncodingErr(val, "routing.paymentFailure")
}

func decodePaymentFailure(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {
	if v, ok := val.(*paymentFailure); ok {
		var h paymentFailure

		sourceIdx := tlv.ZeroRecordT[tlv.TlvType0, uint8]()
		msg := tlv.ZeroRecordT[tlv.TlvType1, failureMessage]()

		typeMap, err := lnwire.DecodeRecords(
			r,
			lnwire.ProduceRecordsSorted(&sourceIdx, &msg)...,
		)

		if err != nil {
			return err
		}

		if _, ok := typeMap[h.sourceIdx.TlvType()]; ok {
			h.sourceIdx = tlv.SomeRecordT(sourceIdx)
		}

		if _, ok := typeMap[h.msg.TlvType()]; ok {
			h.msg = tlv.SomeRecordT(msg)
		}

		*v = h

		return nil
	}

	return tlv.NewTypeForDecodingErr(
		val, "routing.paymentFailure", l, l,
	)
}

type failureMessage struct {
	lnwire.FailureMessage
}

// Record returns a TLV record that can be used to encode/decode a list of
// failureMessage to/from a TLV stream.
func (r *failureMessage) Record() tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := encodeFailureMessage(&b, r, &buf); err != nil {
			panic(err)
		}

		return uint64(len(b.Bytes()))
	}

	return tlv.MakeDynamicRecord(
		0, r, recordSize, encodeFailureMessage, decodeFailureMessage,
	)
}

func encodeFailureMessage(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*failureMessage); ok {
		var b bytes.Buffer
		err := lnwire.EncodeFailureMessage(&b, v.FailureMessage, 0)
		if err != nil {
			return err
		}

		_, err = w.Write(b.Bytes())

		return err
	}

	return tlv.NewTypeForEncodingErr(val, "routing.failureMessage")
}

func decodeFailureMessage(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*failureMessage); ok {
		msg, err := lnwire.DecodeFailureMessage(r, 0)
		if err != nil {
			return err
		}

		*v = failureMessage{
			FailureMessage: msg,
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "routing.failureMessage", l, l)
}

// extractMCRoute extracts the fields required by MC from the Route struct to
// create the more minimal mcRoute struct.
func extractMCRoute(r *Route) *mcRoute {
	return &mcRoute{
		sourcePubKey: tlv.NewRecordT[tlv.TlvType0](r.SourcePubKey),
		totalAmount:  tlv.NewRecordT[tlv.TlvType1](r.TotalAmount),
		hops: tlv.NewRecordT[tlv.TlvType2](
			extractMCHops(r.Hops),
		),
	}
}

// extractMCHops extracts the Hop fields that MC actually uses from a slice of
// Hops.
func extractMCHops(hops []*Hop) mcHops {
	return fn.Map(hops, extractMCHop)
}

// extractMCHop extracts the Hop fields that MC actually uses from a Hop.
func extractMCHop(hop *Hop) *mcHop {
	h := mcHop{
		channelID: tlv.NewPrimitiveRecord[tlv.TlvType0, uint64](
			hop.ChannelID,
		),
		pubKeyBytes: tlv.NewRecordT[tlv.TlvType1, Vertex](
			hop.PubKeyBytes,
		),
		amtToFwd: tlv.NewRecordT[tlv.TlvType2, lnwire.MilliSatoshi](
			hop.AmtToForward,
		),
	}

	if hop.BlindingPoint != nil {
		h.hasBlindingPoint = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType3, lnwire.TrueBoolean](
				lnwire.TrueBoolean{},
			),
		)
	}

	if len(hop.CustomRecords) != 0 {
		h.hasCustomRecords = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType4, lnwire.TrueBoolean](
				lnwire.TrueBoolean{},
			),
		)
	}

	return &h
}

// mcRoute holds the bare minimum info about a payment attempt route that MC
// requires.
type mcRoute struct {
	sourcePubKey tlv.RecordT[tlv.TlvType0, Vertex]
	totalAmount  tlv.RecordT[tlv.TlvType1, lnwire.MilliSatoshi]
	hops         tlv.RecordT[tlv.TlvType2, mcHops]
}

// Record returns a TLV record that can be used to encode/decode an mcRoute
// to/from a TLV stream.
func (r *mcRoute) Record() tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := encodeMCRoute(&b, r, &buf); err != nil {
			panic(err)
		}

		return uint64(len(b.Bytes()))
	}

	return tlv.MakeDynamicRecord(
		0, r, recordSize, encodeMCRoute, decodeMCRoute,
	)
}

func encodeMCRoute(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*mcRoute); ok {
		return serializeRoute(w, v)
	}

	return tlv.NewTypeForEncodingErr(val, "routing.mcRoute")
}

func decodeMCRoute(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if v, ok := val.(*mcRoute); ok {
		route, err := deserializeRoute(io.LimitReader(r, int64(l)))
		if err != nil {
			return err
		}

		*v = *route

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "routing.mcRoute", l, l)
}

// mcHops is a list of mcHop records.
type mcHops []*mcHop

// Record returns a TLV record that can be used to encode/decode a list of
// mcHop to/from a TLV stream.
func (h *mcHops) Record() tlv.Record {
	recordSize := func() uint64 {
		var (
			b   bytes.Buffer
			buf [8]byte
		)
		if err := encodeMCHops(&b, h, &buf); err != nil {
			panic(err)
		}

		return uint64(len(b.Bytes()))
	}

	return tlv.MakeDynamicRecord(
		0, h, recordSize, encodeMCHops, decodeMCHops,
	)
}

func encodeMCHops(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*mcHops); ok {
		// Encode the number of hops as a var int.
		if err := tlv.WriteVarInt(w, uint64(len(*v)), buf); err != nil {
			return err
		}

		// With that written out, we'll now encode the entries
		// themselves as a sub-TLV record, which includes its _own_
		// inner length prefix.
		for _, hop := range *v {
			var hopBytes bytes.Buffer
			if err := serializeNewHop(&hopBytes, hop); err != nil {
				return err
			}

			// We encode the record with a varint length followed by
			// the _raw_ TLV bytes.
			tlvLen := uint64(len(hopBytes.Bytes()))
			if err := tlv.WriteVarInt(w, tlvLen, buf); err != nil {
				return err
			}

			if _, err := w.Write(hopBytes.Bytes()); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "routing.mcHops")
}

func decodeMCHops(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*mcHops); ok {
		// First, we'll decode the varint that encodes how many hops
		// are encoded in the stream.
		numHops, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		// Now that we know how many records we'll need to read, we can
		// iterate and read them all out in series.
		for i := uint64(0); i < numHops; i++ {
			// Read out the varint that encodes the size of this
			// inner TLV record.
			hopSize, err := tlv.ReadVarInt(r, buf)
			if err != nil {
				return err
			}

			// Using this information, we'll create a new limited
			// reader that'll return an EOF once the end has been
			// reached so the stream stops consuming bytes.
			innerTlvReader := &io.LimitedReader{
				R: r,
				N: int64(hopSize),
			}

			hop, err := deserializeNewHop(innerTlvReader)
			if err != nil {
				return err
			}

			*v = append(*v, hop)
		}

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "routing.mcHops", l, l)
}

// serializeRoute serializes a mcRoute and writes the resulting bytes to the
// given io.Writer.
func serializeRoute(w io.Writer, r *mcRoute) error {
	records := lnwire.ProduceRecordsSorted(
		&r.sourcePubKey,
		&r.totalAmount,
		&r.hops,
	)

	return lnwire.EncodeRecordsTo(w, records)
}

// deserializeRoute deserializes the mcRoute from the given io.Reader.
func deserializeRoute(r io.Reader) (*mcRoute, error) {
	var rt mcRoute
	records := lnwire.ProduceRecordsSorted(
		&rt.sourcePubKey,
		&rt.totalAmount,
		&rt.hops,
	)

	_, err := lnwire.DecodeRecords(r, records...)
	if err != nil {
		return nil, err
	}

	return &rt, nil
}

// deserializeNewHop deserializes the mcHop from the given io.Reader.
func deserializeNewHop(r io.Reader) (*mcHop, error) {
	var (
		h        mcHop
		blinding = tlv.ZeroRecordT[tlv.TlvType3, lnwire.TrueBoolean]()
		custom   = tlv.ZeroRecordT[tlv.TlvType4, lnwire.TrueBoolean]()
	)
	records := lnwire.ProduceRecordsSorted(
		&h.channelID,
		&h.pubKeyBytes,
		&h.amtToFwd,
		&blinding,
		&custom,
	)

	typeMap, err := lnwire.DecodeRecords(r, records...)
	if err != nil {
		return nil, err
	}

	if _, ok := typeMap[h.hasBlindingPoint.TlvType()]; ok {
		h.hasBlindingPoint = tlv.SomeRecordT(blinding)
	}

	if _, ok := typeMap[h.hasCustomRecords.TlvType()]; ok {
		h.hasCustomRecords = tlv.SomeRecordT(custom)
	}

	return &h, nil
}

// serializeNewHop serializes a mcHop and writes the resulting bytes to the
// given io.Writer.
func serializeNewHop(w io.Writer, h *mcHop) error {
	recordProducers := []tlv.RecordProducer{
		&h.channelID,
		&h.pubKeyBytes,
		&h.amtToFwd,
	}

	h.hasBlindingPoint.WhenSome(func(
		hasBlinding tlv.RecordT[tlv.TlvType3, lnwire.TrueBoolean]) {

		recordProducers = append(recordProducers, &hasBlinding)
	})

	h.hasCustomRecords.WhenSome(func(
		hasCustom tlv.RecordT[tlv.TlvType4, lnwire.TrueBoolean]) {

		recordProducers = append(recordProducers, &hasCustom)
	})

	return lnwire.EncodeRecordsTo(
		w, lnwire.ProduceRecordsSorted(recordProducers...),
	)
}

// mcHop holds the bare minimum info about a payment attempt route hop that MC
// requires.
type mcHop struct {
	channelID        tlv.RecordT[tlv.TlvType0, uint64]
	pubKeyBytes      tlv.RecordT[tlv.TlvType1, Vertex]
	amtToFwd         tlv.RecordT[tlv.TlvType2, lnwire.MilliSatoshi]
	hasBlindingPoint tlv.OptionalRecordT[tlv.TlvType3, lnwire.TrueBoolean]
	hasCustomRecords tlv.OptionalRecordT[tlv.TlvType4, lnwire.TrueBoolean]
}

// serializeOldResult serializes a payment result and returns a key and value
// byte slice to insert into the bucket.
func serializeOldResult(rp *paymentResultOld) ([]byte, []byte, error) {
	// Write timestamps, success status, failure source index and route.
	var b bytes.Buffer
	var dbFailureSourceIdx int32
	if rp.failureSourceIdx == nil {
		dbFailureSourceIdx = unknownFailureSourceIdx
	} else {
		dbFailureSourceIdx = int32(*rp.failureSourceIdx)
	}
	err := WriteElements(
		&b,
		uint64(rp.timeFwd.UnixNano()),
		uint64(rp.timeReply.UnixNano()),
		rp.success, dbFailureSourceIdx,
	)
	if err != nil {
		return nil, nil, err
	}

	if err := SerializeRoute(&b, *rp.route); err != nil {
		return nil, nil, err
	}

	// Write failure. If there is no failure message, write an empty
	// byte slice.
	var failureBytes bytes.Buffer
	if rp.failure != nil {
		err := lnwire.EncodeFailureMessage(&failureBytes, rp.failure, 0)
		if err != nil {
			return nil, nil, err
		}
	}
	err = wire.WriteVarBytes(&b, 0, failureBytes.Bytes())
	if err != nil {
		return nil, nil, err
	}
	// Compose key that identifies this result.
	key := getResultKeyOld(rp)

	return key, b.Bytes(), nil
}

// getResultKeyOld returns a byte slice representing a unique key for this
// payment result.
func getResultKeyOld(rp *paymentResultOld) []byte {
	var keyBytes [8 + 8 + 33]byte

	// Identify records by a combination of time, payment id and sender pub
	// key. This allows importing mission control data from an external
	// source without key collisions and keeps the records sorted
	// chronologically.
	byteOrder.PutUint64(keyBytes[:], uint64(rp.timeReply.UnixNano()))
	byteOrder.PutUint64(keyBytes[8:], rp.id)
	copy(keyBytes[16:], rp.route.SourcePubKey[:])

	return keyBytes[:]
}

// serializeNewResult serializes a payment result and returns a key and value
// byte slice to insert into the bucket.
func serializeNewResult(rp *paymentResultNew) ([]byte, []byte, error) {
	recordProducers := []tlv.RecordProducer{
		&rp.timeFwd,
		&rp.timeReply,
		&rp.route,
	}

	rp.failure.WhenSome(
		func(failure tlv.RecordT[tlv.TlvType3, paymentFailure]) {
			recordProducers = append(recordProducers, &failure)
		},
	)

	// Compose key that identifies this result.
	key := getResultKeyNew(rp)

	var buff bytes.Buffer
	err := lnwire.EncodeRecordsTo(
		&buff, lnwire.ProduceRecordsSorted(recordProducers...),
	)
	if err != nil {
		return nil, nil, err
	}

	return key, buff.Bytes(), nil
}

// getResultKeyNew returns a byte slice representing a unique key for this
// payment result.
func getResultKeyNew(rp *paymentResultNew) []byte {
	var keyBytes [8 + 8 + 33]byte

	// Identify records by a combination of time, payment id and sender pub
	// key. This allows importing mission control data from an external
	// source without key collisions and keeps the records sorted
	// chronologically.
	byteOrder.PutUint64(keyBytes[:], rp.timeReply.Val)
	byteOrder.PutUint64(keyBytes[8:], rp.id)
	copy(keyBytes[16:], rp.route.Val.sourcePubKey.Val[:])

	return keyBytes[:]
}

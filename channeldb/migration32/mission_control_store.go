package migration32

import (
	"bytes"
	"io"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
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

// paymentResultCommon holds the fields that are shared by the old and new
// payment result encoding.
type paymentResultCommon struct {
	id                 uint64
	timeFwd, timeReply time.Time
	success            bool
	failureSourceIdx   *int
	failure            lnwire.FailureMessage
}

// paymentResultOld is the information that becomes available when a payment
// attempt completes.
type paymentResultOld struct {
	paymentResultCommon
	route *Route
}

// deserializeOldResult deserializes a payment result using the old encoding.
func deserializeOldResult(k, v []byte) (*paymentResultOld, error) {
	// Parse payment id.
	result := paymentResultOld{
		paymentResultCommon: paymentResultCommon{
			id: byteOrder.Uint64(k[8:]),
		},
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
	return &paymentResultNew{
		paymentResultCommon: old.paymentResultCommon,
		route:               extractMCRoute(old.route),
	}
}

// paymentResultNew is the information that becomes available when a payment
// attempt completes.
type paymentResultNew struct {
	paymentResultCommon
	route *mcRoute
}

// extractMCRoute extracts the fields required by MC from the Route struct to
// create the more minima mcRoute struct.
func extractMCRoute(route *Route) *mcRoute {
	return &mcRoute{
		sourcePubKey: route.SourcePubKey,
		totalAmount:  route.TotalAmount,
		hops:         extractMCHops(route.Hops),
	}
}

// extractMCHops extracts the Hop fields that MC actually uses from a slice of
// Hops.
func extractMCHops(hops []*Hop) []*mcHop {
	mcHops := make([]*mcHop, len(hops))
	for i, hop := range hops {
		mcHops[i] = extractMCHop(hop)
	}

	return mcHops
}

// extractMCHop extracts the Hop fields that MC actually uses from a Hop.
func extractMCHop(hop *Hop) *mcHop {
	return &mcHop{
		channelID:        hop.ChannelID,
		pubKeyBytes:      hop.PubKeyBytes,
		amtToFwd:         hop.AmtToForward,
		hasBlindingPoint: hop.BlindingPoint != nil,
	}
}

// mcRoute holds the bare minimum info about a payment attempt route that MC
// requires.
type mcRoute struct {
	sourcePubKey Vertex
	totalAmount  lnwire.MilliSatoshi
	hops         []*mcHop
}

// mcHop holds the bare minimum info about a payment attempt route hop that MC
// requires.
type mcHop struct {
	channelID        uint64
	pubKeyBytes      Vertex
	amtToFwd         lnwire.MilliSatoshi
	hasBlindingPoint bool
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

	if err := serializeMCRoute(&b, rp.route); err != nil {
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
	key := getResultKeyNew(rp)

	return key, b.Bytes(), nil
}

// getResultKeyNew returns a byte slice representing a unique key for this
// payment result.
func getResultKeyNew(rp *paymentResultNew) []byte {
	var keyBytes [8 + 8 + 33]byte

	// Identify records by a combination of time, payment id and sender pub
	// key. This allows importing mission control data from an external
	// source without key collisions and keeps the records sorted
	// chronologically.
	byteOrder.PutUint64(keyBytes[:], uint64(rp.timeReply.UnixNano()))
	byteOrder.PutUint64(keyBytes[8:], rp.id)
	copy(keyBytes[16:], rp.route.sourcePubKey[:])

	return keyBytes[:]
}

// serializeMCRoute serializes an mcRoute and writes the bytes to the given
// io.Writer.
func serializeMCRoute(w io.Writer, r *mcRoute) error {
	if err := WriteElements(
		w, r.totalAmount, r.sourcePubKey[:],
	); err != nil {
		return err
	}

	if err := WriteElements(w, uint32(len(r.hops))); err != nil {
		return err
	}

	for _, h := range r.hops {
		if err := serializeNewHop(w, h); err != nil {
			return err
		}
	}

	return nil
}

// serializeMCRoute serializes an mcHop and writes the bytes to the given
// io.Writer.
func serializeNewHop(w io.Writer, h *mcHop) error {
	return WriteElements(w,
		h.pubKeyBytes[:],
		h.channelID,
		h.amtToFwd,
		h.hasBlindingPoint,
	)
}

package paymentsdb

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/lightningnetwork/lnd/tlv"
)

// unmarshalHtlcAttempt converts a sqlc.PaymentHtlcAttempt and its hops to
// an HTLCAttempt.
func unmarshalHtlcAttempt(dbAttempt sqlc.PaymentHtlcAttempt,
	hops []sqlc.PaymentRouteHop) (*HTLCAttempt, error) {

	if len(dbAttempt.SessionKey) != btcec.PrivKeyBytesLen {
		return nil, fmt.Errorf("invalid session key length: %d",
			len(dbAttempt.SessionKey))
	}

	// Convert session key of the sphinx packet.
	var sessionKey [btcec.PrivKeyBytesLen]byte
	copy(sessionKey[:], dbAttempt.SessionKey)

	// Reconstruct the route from hops
	route, err := unmarshalRoute(dbAttempt, hops)
	if err != nil {
		return nil, fmt.Errorf("unable to reconstruct route: %w", err)
	}

	var hash lntypes.Hash
	copy(hash[:], dbAttempt.PaymentHash)

	// Create HTLCAttemptInfo
	htlcInfo := HTLCAttemptInfo{
		AttemptID:   uint64(dbAttempt.AttemptIndex),
		sessionKey:  sessionKey,
		AttemptTime: dbAttempt.AttemptTime.Local(),
		Route:       *route,
		Hash:        &hash,
	}

	// Create the HTLCAttempt
	htlcAttempt := &HTLCAttempt{
		HTLCAttemptInfo: htlcInfo,
	}

	// Handle settle info.
	if dbAttempt.SettlePreimage != nil {
		var preimage lntypes.Preimage
		if len(dbAttempt.SettlePreimage) != 32 {
			return nil, fmt.Errorf("invalid preimage length: %d",
				len(dbAttempt.SettlePreimage))
		}
		copy(preimage[:], dbAttempt.SettlePreimage)

		var settleTime time.Time
		if dbAttempt.SettleTime.Valid {
			settleTime = dbAttempt.SettleTime.Time.Local()
		}

		htlcAttempt.Settle = &HTLCSettleInfo{
			Preimage:   preimage,
			SettleTime: settleTime,
		}
	}

	// Handle failure info.
	if dbAttempt.HtlcFailReason.Valid {
		htlcAttempt.Failure = &HTLCFailInfo{
			FailTime: dbAttempt.FailTime.Time.Local(),
			Reason: HTLCFailReason(
				dbAttempt.HtlcFailReason.Int32,
			),
			FailureSourceIndex: uint32(
				dbAttempt.FailureSourceIndex.Int32),
		}

		// If we have a failure message, we could parse it here
		if dbAttempt.FailureMsg != nil {
			failureMsg, err := lnwire.DecodeFailureMessage(
				bytes.NewReader(dbAttempt.FailureMsg), 0)
			if err != nil {
				return nil, fmt.Errorf(
					"unable to decode failure message: %w",
					err)
			}

			htlcAttempt.Failure.Message = failureMsg
		}
	}

	return htlcAttempt, nil
}

// unmarshalRoute converts a sqlc.PaymentHtlcAttempt and its hops to a
// route.Route.
func unmarshalRoute(dbAttempt sqlc.PaymentHtlcAttempt,
	hops []sqlc.PaymentRouteHop) (*route.Route, error) {

	// Unmarshal the hops.
	routeHops, err := unmarshalRouteHops(hops)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal route hops: %w",
			err)
	}

	// Add the additional data to the route.
	sourcePubKey, err := route.NewVertexFromBytes(dbAttempt.RouteSourceKey)
	if err != nil {
		return nil, fmt.Errorf("unable to convert source "+
			"pubkey: %w", err)
	}

	route := &route.Route{
		TotalTimeLock: uint32(dbAttempt.RouteTotalTimelock),
		TotalAmount:   lnwire.MilliSatoshi(dbAttempt.RouteTotalAmount),
		SourcePubKey:  sourcePubKey,
		Hops:          routeHops,
	}

	if dbAttempt.RouteFirstHopAmount.Valid {
		route.FirstHopAmount = tlv.NewRecordT[tlv.TlvType0](
			tlv.NewBigSizeT(lnwire.MilliSatoshi(
				dbAttempt.RouteFirstHopAmount.Int64),
			),
		)
	}

	return route, nil
}

// unmarshalHops converts a slice of sqlc.PaymentRouteHop to a slice of
// *route.Hop.
func unmarshalRouteHops(dbHops []sqlc.PaymentRouteHop) ([]*route.Hop, error) {
	if len(dbHops) == 0 {
		return nil, fmt.Errorf("no hops provided")
	}

	routeHops := make([]*route.Hop, len(dbHops))
	for i, dbHop := range dbHops {
		routeHop, err := unmarshalHop(dbHop)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal "+
				"hop %d: %w", i, err)
		}
		routeHops[i] = routeHop
	}

	return routeHops, nil
}

// unmarshalHop converts a sqlc.PaymentRouteHop to a route.Hop.
func unmarshalHop(dbHop sqlc.PaymentRouteHop) (*route.Hop, error) {
	// Parse channel ID which is represented as a string in the sql db and
	// uses the integer representation of the channel id.
	chanID, err := strconv.ParseUint(dbHop.ChanID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid channel ID: %w", err)
	}

	var pubKey [33]byte
	if len(dbHop.PubKey) != 33 {
		return nil, fmt.Errorf("invalid public key length: %d",
			len(dbHop.PubKey))
	}
	copy(pubKey[:], dbHop.PubKey)

	hop := &route.Hop{
		ChannelID:        chanID,
		PubKeyBytes:      pubKey,
		OutgoingTimeLock: uint32(dbHop.OutgoingTimeLock),
		AmtToForward:     lnwire.MilliSatoshi(dbHop.AmtToForward),
		Metadata:         dbHop.MetaData,
		LegacyPayload:    dbHop.LegacyPayload,
	}

	// Handle MPP fields if available. This should only be present for
	// the last hop of the route in MPP payments.
	if len(dbHop.MppPaymentAddr) > 0 {
		var paymentAddr [32]byte
		copy(paymentAddr[:], dbHop.MppPaymentAddr)
		totalMsat := lnwire.MilliSatoshi(0)
		if dbHop.MppTotalMsat.Valid {
			totalMsat = lnwire.MilliSatoshi(
				dbHop.MppTotalMsat.Int64,
			)
		}
		hop.MPP = record.NewMPP(totalMsat, paymentAddr)
	}

	// Handle AMP fields if available. This should only be present for
	// the last hop of the route in AMP payments.
	if len(dbHop.AmpRootShare) > 0 && len(dbHop.AmpSetID) > 0 {
		var rootShare, setID [32]byte
		copy(rootShare[:], dbHop.AmpRootShare)
		copy(setID[:], dbHop.AmpSetID)

		childIndex := uint32(0)
		if dbHop.AmpChildIndex.Valid {
			childIndex = uint32(dbHop.AmpChildIndex.Int32)
		}

		hop.AMP = record.NewAMP(rootShare, setID, childIndex)
	}

	// Handle blinded path fields
	if len(dbHop.EncryptedData) > 0 {
		hop.EncryptedData = dbHop.EncryptedData
	}

	if len(dbHop.BlindingPoint) > 0 {
		pubKey, err := btcec.ParsePubKey(dbHop.BlindingPoint)
		if err != nil {
			return nil, fmt.Errorf("invalid blinding "+
				"point: %w", err)
		}
		hop.BlindingPoint = pubKey
	}

	if dbHop.BlindedPathTotalAmt.Valid {
		hop.TotalAmtMsat = lnwire.MilliSatoshi(
			dbHop.BlindedPathTotalAmt.Int64)
	}

	return hop, nil
}

func unmarshalFirstHopCustomRecords(
	dbFirstHopCustomRecords []sqlc.PaymentFirstHopCustomRecord) (
	lnwire.CustomRecords, error) {

	tlvMap := make(tlv.TypeMap)
	for _, dbRecord := range dbFirstHopCustomRecords {
		// We need to make a copy to prevent nil/empty value comparison
		// issues for empty values.
		value := make([]byte, len(dbRecord.Value))
		copy(value, dbRecord.Value)
		tlvMap[tlv.Type(dbRecord.Key)] = value
	}
	firstHopCustomRecordsMap, err := lnwire.NewCustomRecords(tlvMap)
	if err != nil {
		return nil, fmt.Errorf("unable to convert first "+
			"hop custom records to tlv map: %w", err)
	}

	return firstHopCustomRecordsMap, nil
}

func unmarshalHopCustomRecords(
	dbHopCustomRecords []sqlc.PaymentRouteHopCustomRecord) (
	record.CustomSet, error) {

	tlvMap := make(tlv.TypeMap)
	for _, dbRecord := range dbHopCustomRecords {
		// We need to make a copy to prevent nil/empty value comparison
		// issues for empty values.
		value := make([]byte, len(dbRecord.Value))
		copy(value, dbRecord.Value)
		tlvMap[tlv.Type(dbRecord.Key)] = value
	}
	hopCustomRecordsMap, err := lnwire.NewCustomRecords(tlvMap)
	if err != nil {
		return nil, fmt.Errorf("unable to convert hop "+
			"custom records to tlv map: %w", err)
	}

	return record.CustomSet(hopCustomRecordsMap), nil
}

/*
Duplicate payment conversion functions.
*/

// unmarshalDuplicateHtlcAttempt converts a sqlc.DuplicatePaymentHtlcAttempt
// and its hops to an HTLCAttempt.
func unmarshalDuplicateHtlcAttempt(
	dbAttempt sqlc.DuplicatePaymentHtlcAttempt,
	hops []sqlc.DuplicatePaymentRouteHop) (*HTLCAttempt, error) {

	return nil, fmt.Errorf("not implemented")

}

// unmarshalDuplicateRoute converts a sqlc.DuplicatePaymentHtlcAttempt and its
// hops to a route.Route.
func unmarshalDuplicateRoute(
	dbAttempt sqlc.DuplicatePaymentHtlcAttempt,
	dbHops []sqlc.DuplicatePaymentRouteHop) (*route.Route, error) {

	return nil, fmt.Errorf("not implemented")
}

// unmarshalDuplicateHops converts a slice of sqlc.DuplicatePaymentRouteHop to
// a slice of *route.Hop.
func unmarshalDuplicateHops(
	dbHops []sqlc.DuplicatePaymentRouteHop) ([]*route.Hop, error) {

	return nil, fmt.Errorf("not implemented")
}

// unmarshalDuplicateHop converts a sqlc.DuplicatePaymentRouteHop to a
// route.Hop.
func unmarshalDuplicateHop(
	dbHop sqlc.DuplicatePaymentRouteHop) (*route.Hop, error) {

	return nil, fmt.Errorf("not implemented")
}

// unmarshalDuplicateHopCustomRecords converts a slice of
// sqlc.DuplicatePaymentRouteHopCustomRecord to a record.CustomSet.
func unmarshalDuplicateHopCustomRecords(
	dbHopCustomRecords []sqlc.DuplicatePaymentRouteHopCustomRecord) (
	record.CustomSet, error) {

	return nil, fmt.Errorf("not implemented")
}

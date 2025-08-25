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
	hops []sqlc.PaymentRouteHop,
	dbRouteCustomRecords []sqlc.PaymentHtlcAttemptCustomRecord) (
	*HTLCAttempt, error) {

	if len(dbAttempt.SessionKey) != btcec.PrivKeyBytesLen {
		return nil, fmt.Errorf("invalid session key length: %d",
			len(dbAttempt.SessionKey))
	}

	// Convert session key of the sphinx packet.
	var sessionKey [btcec.PrivKeyBytesLen]byte
	copy(sessionKey[:], dbAttempt.SessionKey)

	// Reconstruct the route from hops
	route, err := unmarshalRoute(dbAttempt, hops, dbRouteCustomRecords)
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
	hops []sqlc.PaymentRouteHop,
	dbRouteCustomRecords []sqlc.PaymentHtlcAttemptCustomRecord) (*route.Route, error) {

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
		TotalTimeLock: uint32(dbAttempt.RouteTotalTimeLock),
		TotalAmount:   lnwire.MilliSatoshi(dbAttempt.RouteTotalAmount),
		SourcePubKey:  sourcePubKey,
		Hops:          routeHops,
		FirstHopAmount: tlv.NewRecordT[tlv.TlvType0](
			tlv.NewBigSizeT(
				lnwire.MilliSatoshi(dbAttempt.FirstHopAmountMsat),
			),
		),
	}

	// Attach the custom records to the route.
	customRecords, err := unmarshalRouteCustomRecords(
		dbRouteCustomRecords,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal route "+
			"custom records: %w", err)
	}

	route.FirstHopWireCustomRecords = customRecords

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
	chanID, err := strconv.ParseUint(dbHop.Scid, 10, 64)
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

// unmarshalFirstHopCustomRecords converts a slice of
// sqlc.PaymentFirstHopCustomRecord to a lnwire.CustomRecords.
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

// unmarshalRouteCustomRecords converts a slice of
// sqlc.PaymentHtlcAttemptCustomRecord to a lnwire.CustomRecords.
func unmarshalRouteCustomRecords(
	dbRouteCustomRecords []sqlc.PaymentHtlcAttemptCustomRecord) (
	lnwire.CustomRecords, error) {

	tlvMap := make(tlv.TypeMap)
	for _, dbRecord := range dbRouteCustomRecords {
		// We need to make a copy to prevent nil/empty value comparison
		// issues for empty values.
		value := make([]byte, len(dbRecord.Value))
		copy(value, dbRecord.Value)
		tlvMap[tlv.Type(dbRecord.Key)] = value
	}
	routeCustomRecordsMap, err := lnwire.NewCustomRecords(tlvMap)
	if err != nil {
		return nil, fmt.Errorf("unable to convert route "+
			"custom records to tlv map: %w", err)
	}

	return routeCustomRecordsMap, nil
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

// unmarshalPaymentData unmarshals the payment data into the MPPayment object.
func unmarshalPaymentData(dbPayment sqlc.Payment,
	dbHtlcAttempts []sqlc.PaymentHtlcAttempt, dbHops []sqlc.PaymentRouteHop,
	dbRouteCustomRecords []sqlc.PaymentHtlcAttemptCustomRecord,
	dbHopCustomRecords []sqlc.PaymentRouteHopCustomRecord,
	dbFirstHopCustomRecords []sqlc.PaymentFirstHopCustomRecord) (*MPPayment,
	error) {

	// Create maps for efficient lookups
	hopsByAttemptIndex := make(map[int64][]sqlc.PaymentRouteHop)
	for _, hop := range dbHops {
		hopsByAttemptIndex[hop.HtlcAttemptIndex] = append(
			hopsByAttemptIndex[hop.HtlcAttemptIndex], hop,
		)
	}

	customRecordsByHopID := make(
		map[int64][]sqlc.PaymentRouteHopCustomRecord,
	)
	for _, record := range dbHopCustomRecords {
		customRecordsByHopID[record.HopID] = append(
			customRecordsByHopID[record.HopID], record,
		)
	}

	// Convert the db htlc attempts to our internal htlc attempts data
	// structure.
	var htlcAttempts []HTLCAttempt
	for _, dbAttempt := range dbHtlcAttempts {
		// Get hops for this attempt from our pre-fetched data
		dbHops := hopsByAttemptIndex[dbAttempt.AttemptIndex]

		attempt, err := unmarshalHtlcAttempt(
			dbAttempt, dbHops, dbRouteCustomRecords,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to convert htlc "+
				"attempt(id=%d): %w", dbAttempt.AttemptIndex,
				err)
		}

		hops := attempt.Route.Hops

		// Attach the custom records to the hop which are in a separate
		// table.
		for i, dbHop := range dbHops {
			// Get custom records for this hop from our pre-fetched
			// data.
			dbHopCustomRecords := customRecordsByHopID[dbHop.ID]

			hops[i].CustomRecords, err = unmarshalHopCustomRecords(
				dbHopCustomRecords,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to unmarshal "+
					"hop custom records: %w", err)
			}
		}

		htlcAttempts = append(htlcAttempts, *attempt)
	}

	// Convert first hop custom records
	firstHopCustomRecords, err := unmarshalFirstHopCustomRecords(
		dbFirstHopCustomRecords,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal first hop "+
			"custom records: %w", err)
	}

	// Convert payment hash from bytes to lntypes.Hash.
	var paymentHash lntypes.Hash
	copy(paymentHash[:], dbPayment.PaymentHash)

	// Create PaymentCreationInfo from the payment data.
	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentHash,
		Value: lnwire.MilliSatoshi(
			dbPayment.AmountMsat,
		),
		CreationTime:          dbPayment.CreatedAt.Local(),
		PaymentRequest:        dbPayment.PaymentRequest,
		FirstHopCustomRecords: firstHopCustomRecords,
	}

	var failureReason *FailureReason
	if dbPayment.FailReason.Valid {
		reason := FailureReason(dbPayment.FailReason.Int32)
		failureReason = &reason
	}

	// Create MPPayment in memory object.
	payment := &MPPayment{
		SequenceNum:   uint64(dbPayment.ID),
		FailureReason: failureReason,
		Info:          creationInfo,
		HTLCs:         htlcAttempts,
	}

	return payment, nil
}

// unmarshalPaymentWithoutHTLCs unmarshals the payment data into the MPPayment
// object without HTLCs.
func unmarshalPaymentWithoutHTLCs(dbPayment sqlc.Payment) (*MPPayment, error) {
	// Convert payment hash from bytes to lntypes.Hash
	var paymentHash lntypes.Hash
	copy(paymentHash[:], dbPayment.PaymentHash)

	// Create PaymentCreationInfo from the payment data
	creationInfo := &PaymentCreationInfo{
		PaymentIdentifier: paymentHash,
		Value:             lnwire.MilliSatoshi(dbPayment.AmountMsat),
		CreationTime:      dbPayment.CreatedAt.Local(),
		PaymentRequest:    dbPayment.PaymentRequest,
		// No first hop custom records for payments without HTLCs
	}

	var failureReason *FailureReason
	if dbPayment.FailReason.Valid {
		reason := FailureReason(dbPayment.FailReason.Int32)
		failureReason = &reason
	}

	// Create MPPayment object
	payment := &MPPayment{
		SequenceNum:   uint64(dbPayment.ID),
		FailureReason: failureReason,
		Info:          creationInfo,
		HTLCs:         []HTLCAttempt{},
	}

	return payment, nil
}

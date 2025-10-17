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

// dbPaymentToCreationInfo converts database payment data to the
// PaymentCreationInfo struct.
func dbPaymentToCreationInfo(paymentIdentifier []byte, amountMsat int64,
	createdAt time.Time, intentPayload []byte,
	firstHopCustomRecords lnwire.CustomRecords) *PaymentCreationInfo {

	// This is the payment hash for non-AMP payments and the SetID for AMP
	// payments.
	var identifier lntypes.Hash
	copy(identifier[:], paymentIdentifier)

	return &PaymentCreationInfo{
		PaymentIdentifier: identifier,
		Value:             lnwire.MilliSatoshi(amountMsat),
		// The creation time is stored in the database as UTC but here
		// we convert it to local time.
		CreationTime:          createdAt.Local(),
		PaymentRequest:        intentPayload,
		FirstHopCustomRecords: firstHopCustomRecords,
	}
}

// dbAttemptToHTLCAttempt converts a database HTLC attempt to an HTLCAttempt.
func dbAttemptToHTLCAttempt(dbAttempt sqlc.FetchHtlcAttemptsForPaymentsRow,
	hops []sqlc.FetchHopsForAttemptsRow,
	hopCustomRecords map[int64][]sqlc.PaymentHopCustomRecord,
	routeCustomRecords []sqlc.PaymentAttemptFirstHopCustomRecord) (
	*HTLCAttempt, error) {

	// Convert route-level first hop custom records to CustomRecords map.
	var firstHopWireCustomRecords lnwire.CustomRecords
	if len(routeCustomRecords) > 0 {
		firstHopWireCustomRecords = make(lnwire.CustomRecords)
		for _, record := range routeCustomRecords {
			firstHopWireCustomRecords[uint64(record.Key)] =
				record.Value
		}
	}

	// Build the route from the database data.
	route, err := dbDataToRoute(
		hops, hopCustomRecords, dbAttempt.FirstHopAmountMsat,
		dbAttempt.RouteTotalTimeLock, dbAttempt.RouteTotalAmount,
		dbAttempt.RouteSourceKey, firstHopWireCustomRecords,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to convert to route: %w",
			err)
	}

	hash, err := lntypes.MakeHash(dbAttempt.PaymentHash)
	if err != nil {
		return nil, fmt.Errorf("failed to parse payment "+
			"hash: %w", err)
	}

	// Create the attempt info.
	var sessionKey [32]byte
	copy(sessionKey[:], dbAttempt.SessionKey)

	info := HTLCAttemptInfo{
		AttemptID:   uint64(dbAttempt.AttemptIndex),
		sessionKey:  sessionKey,
		Route:       *route,
		AttemptTime: dbAttempt.AttemptTime,
		Hash:        &hash,
	}

	attempt := &HTLCAttempt{
		HTLCAttemptInfo: info,
	}

	// If there's no resolution type, the attempt is still in-flight.
	// Return early without processing settlement or failure info.
	if !dbAttempt.ResolutionType.Valid {
		return attempt, nil
	}

	// Add settlement info if present.
	if HTLCAttemptResolutionType(dbAttempt.ResolutionType.Int32) ==
		HTLCAttemptResolutionSettled {

		var preimage lntypes.Preimage
		copy(preimage[:], dbAttempt.SettlePreimage)

		attempt.Settle = &HTLCSettleInfo{
			Preimage:   preimage,
			SettleTime: dbAttempt.ResolutionTime.Time,
		}
	}

	// Add failure info if present.
	if HTLCAttemptResolutionType(dbAttempt.ResolutionType.Int32) ==
		HTLCAttemptResolutionFailed {

		failure := &HTLCFailInfo{
			FailTime: dbAttempt.ResolutionTime.Time,
		}

		if dbAttempt.HtlcFailReason.Valid {
			failure.Reason = HTLCFailReason(
				dbAttempt.HtlcFailReason.Int32,
			)
		}

		if dbAttempt.FailureSourceIndex.Valid {
			failure.FailureSourceIndex = uint32(
				dbAttempt.FailureSourceIndex.Int32,
			)
		}

		// Decode the failure message if present.
		if len(dbAttempt.FailureMsg) > 0 {
			msg, err := lnwire.DecodeFailureMessage(
				bytes.NewReader(dbAttempt.FailureMsg), 0,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to decode "+
					"failure message: %w", err)
			}
			failure.Message = msg
		}

		attempt.Failure = failure
	}

	return attempt, nil
}

// dbDataToRoute converts database route data to a route.Route.
func dbDataToRoute(hops []sqlc.FetchHopsForAttemptsRow,
	hopCustomRecords map[int64][]sqlc.PaymentHopCustomRecord,
	firstHopAmountMsat int64, totalTimeLock int32, totalAmount int64,
	sourceKey []byte, firstHopWireCustomRecords lnwire.CustomRecords) (
	*route.Route, error) {

	if len(hops) == 0 {
		return nil, fmt.Errorf("no hops provided")
	}

	// Hops are already sorted by hop_index from the SQL query.
	routeHops := make([]*route.Hop, len(hops))

	for i, hop := range hops {
		pubKey, err := route.NewVertexFromBytes(hop.PubKey)
		if err != nil {
			return nil, fmt.Errorf("failed to parse pub key: %w",
				err)
		}

		var channelID uint64
		if hop.Scid != "" {
			// The SCID is stored as a string representation
			// of the uint64.
			var err error
			channelID, err = strconv.ParseUint(hop.Scid, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("failed to parse "+
					"scid: %w", err)
			}
		}

		routeHop := &route.Hop{
			PubKeyBytes:      pubKey,
			ChannelID:        channelID,
			OutgoingTimeLock: uint32(hop.OutgoingTimeLock),
			AmtToForward:     lnwire.MilliSatoshi(hop.AmtToForward),
		}

		// Add MPP record if present.
		if len(hop.MppPaymentAddr) > 0 {
			var paymentAddr [32]byte
			copy(paymentAddr[:], hop.MppPaymentAddr)
			routeHop.MPP = record.NewMPP(
				lnwire.MilliSatoshi(hop.MppTotalMsat.Int64),
				paymentAddr,
			)
		}

		// Add AMP record if present.
		if len(hop.AmpRootShare) > 0 {
			var rootShare [32]byte
			copy(rootShare[:], hop.AmpRootShare)
			var setID [32]byte
			copy(setID[:], hop.AmpSetID)

			routeHop.AMP = record.NewAMP(
				rootShare, setID,
				uint32(hop.AmpChildIndex.Int32),
			)
		}

		// Add blinding point if present (only for introduction node
		// in blinded route).
		if len(hop.BlindingPoint) > 0 {
			pubKey, err := btcec.ParsePubKey(hop.BlindingPoint)
			if err != nil {
				return nil, fmt.Errorf("failed to parse "+
					"blinding point: %w", err)
			}
			routeHop.BlindingPoint = pubKey
		}

		// Add encrypted data if present (for all blinded hops).
		if len(hop.EncryptedData) > 0 {
			routeHop.EncryptedData = hop.EncryptedData
		}

		// Add total amount if present (only for final hop in blinded
		// route).
		if hop.BlindedPathTotalAmt.Valid {
			routeHop.TotalAmtMsat = lnwire.MilliSatoshi(
				hop.BlindedPathTotalAmt.Int64,
			)
		}

		// Add hop-level custom records.
		if records, ok := hopCustomRecords[hop.ID]; ok {
			routeHop.CustomRecords = make(
				record.CustomSet,
			)
			for _, rec := range records {
				routeHop.CustomRecords[uint64(rec.Key)] =
					rec.Value
			}
		}

		// Add metadata if present.
		if len(hop.MetaData) > 0 {
			routeHop.Metadata = hop.MetaData
		}

		routeHops[i] = routeHop
	}

	// Parse the source node public key.
	var sourceNode route.Vertex
	copy(sourceNode[:], sourceKey)

	route := &route.Route{
		TotalTimeLock:             uint32(totalTimeLock),
		TotalAmount:               lnwire.MilliSatoshi(totalAmount),
		SourcePubKey:              sourceNode,
		Hops:                      routeHops,
		FirstHopWireCustomRecords: firstHopWireCustomRecords,
	}

	// Set the first hop amount if it is set.
	if firstHopAmountMsat != 0 {
		route.FirstHopAmount = tlv.NewRecordT[tlv.TlvType0](
			tlv.NewBigSizeT(lnwire.MilliSatoshi(
				firstHopAmountMsat,
			)),
		)
	}

	return route, nil
}

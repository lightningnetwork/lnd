package router

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
)

// rpcHtlcEvent returns a rpc htlc event from a htlcswitch event.
func rpcHtlcEvent(htlcEvent interface{}) (*routerrpc.HtlcEvent, error) {
	var (
		key       htlcswitch.HtlcKey
		timestamp time.Time
		eventType htlcswitch.HtlcEventType
		event     routerrpc.IsHtlcEventEvent
	)

	switch e := htlcEvent.(type) {
	case *htlcswitch.ForwardingEvent:
		event = &routerrpc.HtlcEvent_ForwardEvent{
			ForwardEvent: &routerrpc.ForwardEvent{
				Info: rpcInfo(e.HtlcInfo),
			},
		}

		key = e.HtlcKey
		eventType = e.HtlcEventType
		timestamp = e.Timestamp

	case *htlcswitch.ForwardingFailEvent:
		event = &routerrpc.HtlcEvent_ForwardFailEvent{
			ForwardFailEvent: &routerrpc.ForwardFailEvent{},
		}

		key = e.HtlcKey
		eventType = e.HtlcEventType
		timestamp = e.Timestamp

	case *htlcswitch.LinkFailEvent:
		failureCode, failReason, err := rpcFailReason(
			e.LinkError,
		)
		if err != nil {
			return nil, err
		}

		event = &routerrpc.HtlcEvent_LinkFailEvent{
			LinkFailEvent: &routerrpc.LinkFailEvent{
				Info:          rpcInfo(e.HtlcInfo),
				WireFailure:   failureCode,
				FailureDetail: failReason,
				FailureString: e.LinkError.Error(),
			},
		}

		key = e.HtlcKey
		eventType = e.HtlcEventType
		timestamp = e.Timestamp

	case *htlcswitch.SettleEvent:
		event = &routerrpc.HtlcEvent_SettleEvent{
			SettleEvent: &routerrpc.SettleEvent{
				Preimage: e.Preimage[:],
			},
		}

		key = e.HtlcKey
		eventType = e.HtlcEventType
		timestamp = e.Timestamp

	default:
		return nil, fmt.Errorf("unknown event type: %T", e)
	}

	rpcEvent := &routerrpc.HtlcEvent{
		IncomingChannelId: key.IncomingCircuit.ChanID.ToUint64(),
		OutgoingChannelId: key.OutgoingCircuit.ChanID.ToUint64(),
		IncomingHtlcId:    key.IncomingCircuit.HtlcID,
		OutgoingHtlcId:    key.OutgoingCircuit.HtlcID,
		TimestampNs:       uint64(timestamp.UnixNano()),
		Event:             event,
	}

	// Convert the htlc event type to a rpc event.
	switch eventType {
	case htlcswitch.HtlcEventTypeSend:
		rpcEvent.EventType = routerrpc.HtlcEvent_SEND

	case htlcswitch.HtlcEventTypeReceive:
		rpcEvent.EventType = routerrpc.HtlcEvent_RECEIVE

	case htlcswitch.HtlcEventTypeForward:
		rpcEvent.EventType = routerrpc.HtlcEvent_FORWARD

	default:
		return nil, fmt.Errorf("unknown event type: %v", eventType)
	}

	return rpcEvent, nil
}

// rpcInfo returns a rpc struct containing the htlc information from the
// switch's htlc info struct.
func rpcInfo(info htlcswitch.HtlcInfo) *routerrpc.HtlcInfo {
	return &routerrpc.HtlcInfo{
		IncomingTimelock: info.IncomingTimeLock,
		OutgoingTimelock: info.OutgoingTimeLock,
		IncomingAmtMsat:  uint64(info.IncomingAmt),
		OutgoingAmtMsat:  uint64(info.OutgoingAmt),
	}
}

// rpcFailReason maps a lnwire failure message and failure detail to a rpc
// failure code and detail.
func rpcFailReason(linkErr *htlcswitch.LinkError) (lnrpc.Failure_FailureCode,
	routerrpc.FailureDetail, error) {

	wireErr, err := marshallError(linkErr)
	if err != nil {
		return 0, 0, err
	}
	wireCode := wireErr.GetCode()

	// If the link has no failure detail, return with failure detail none.
	if linkErr.FailureDetail == nil {
		return wireCode, routerrpc.FailureDetail_NO_DETAIL, nil
	}

	switch failureDetail := linkErr.FailureDetail.(type) {
	case invoices.FailResolutionResult:
		fd, err := rpcFailureResolution(failureDetail)
		return wireCode, fd, err

	case htlcswitch.OutgoingFailure:
		fd, err := rpcOutgoingFailure(failureDetail)
		return wireCode, fd, err

	default:
		return 0, 0, fmt.Errorf("unknown failure "+
			"detail type: %T", linkErr.FailureDetail)
	}
}

// rpcFailureResolution maps an invoice failure resolution to a rpc failure
// detail. Invoice failures have no zero resolution results (every failure
// is accompanied with a result), so we error if we fail to match the result
// type.
func rpcFailureResolution(invoiceFailure invoices.FailResolutionResult) (
	routerrpc.FailureDetail, error) {

	switch invoiceFailure {
	case invoices.ResultReplayToCanceled:
		return routerrpc.FailureDetail_INVOICE_CANCELED, nil

	case invoices.ResultInvoiceAlreadyCanceled:
		return routerrpc.FailureDetail_INVOICE_CANCELED, nil

	case invoices.ResultAmountTooLow:
		return routerrpc.FailureDetail_INVOICE_UNDERPAID, nil

	case invoices.ResultExpiryTooSoon:
		return routerrpc.FailureDetail_INVOICE_EXPIRY_TOO_SOON, nil

	case invoices.ResultCanceled:
		return routerrpc.FailureDetail_INVOICE_CANCELED, nil

	case invoices.ResultInvoiceNotOpen:
		return routerrpc.FailureDetail_INVOICE_NOT_OPEN, nil

	case invoices.ResultMppTimeout:
		return routerrpc.FailureDetail_MPP_INVOICE_TIMEOUT, nil

	case invoices.ResultAddressMismatch:
		return routerrpc.FailureDetail_ADDRESS_MISMATCH, nil

	case invoices.ResultHtlcSetTotalMismatch:
		return routerrpc.FailureDetail_SET_TOTAL_MISMATCH, nil

	case invoices.ResultHtlcSetTotalTooLow:
		return routerrpc.FailureDetail_SET_TOTAL_TOO_LOW, nil

	case invoices.ResultHtlcSetOverpayment:
		return routerrpc.FailureDetail_SET_OVERPAID, nil

	case invoices.ResultInvoiceNotFound:
		return routerrpc.FailureDetail_UNKNOWN_INVOICE, nil

	case invoices.ResultKeySendError:
		return routerrpc.FailureDetail_INVALID_KEYSEND, nil

	case invoices.ResultMppInProgress:
		return routerrpc.FailureDetail_MPP_IN_PROGRESS, nil

	default:
		return 0, fmt.Errorf("unknown fail resolution: %v",
			invoiceFailure.FailureString())
	}
}

// rpcOutgoingFailure maps an outgoing failure to a rpc FailureDetail. If the
// failure detail is FailureDetailNone, which indicates that the failure was
// a wire message which required no further failure detail, we return a no
// detail failure detail to indicate that there was no additional information.
func rpcOutgoingFailure(failureDetail htlcswitch.OutgoingFailure) (
	routerrpc.FailureDetail, error) {

	switch failureDetail {
	case htlcswitch.OutgoingFailureNone:
		return routerrpc.FailureDetail_NO_DETAIL, nil

	case htlcswitch.OutgoingFailureDecodeError:
		return routerrpc.FailureDetail_ONION_DECODE, nil

	case htlcswitch.OutgoingFailureLinkNotEligible:
		return routerrpc.FailureDetail_LINK_NOT_ELIGIBLE, nil

	case htlcswitch.OutgoingFailureOnChainTimeout:
		return routerrpc.FailureDetail_ON_CHAIN_TIMEOUT, nil

	case htlcswitch.OutgoingFailureHTLCExceedsMax:
		return routerrpc.FailureDetail_HTLC_EXCEEDS_MAX, nil

	case htlcswitch.OutgoingFailureInsufficientBalance:
		return routerrpc.FailureDetail_INSUFFICIENT_BALANCE, nil

	case htlcswitch.OutgoingFailureCircularRoute:
		return routerrpc.FailureDetail_CIRCULAR_ROUTE, nil

	case htlcswitch.OutgoingFailureIncompleteForward:
		return routerrpc.FailureDetail_INCOMPLETE_FORWARD, nil

	case htlcswitch.OutgoingFailureDownstreamHtlcAdd:
		return routerrpc.FailureDetail_HTLC_ADD_FAILED, nil

	case htlcswitch.OutgoingFailureForwardsDisabled:
		return routerrpc.FailureDetail_FORWARDS_DISABLED, nil

	default:
		return 0, fmt.Errorf("unknown outgoing failure "+
			"detail: %v", failureDetail.FailureString())
	}
}

package routerrpc

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// rpcHtlcEvent returns a rpc htlc event from a htlcswitch event.
func rpcHtlcEvent(htlcEvent interface{}) (*HtlcEvent, error) {
	var (
		key       htlcswitch.HtlcKey
		timestamp time.Time
		eventType *htlcswitch.HtlcEventType
		event     isHtlcEvent_Event
	)

	switch e := htlcEvent.(type) {
	case *htlcswitch.ForwardingEvent:
		event = &HtlcEvent_ForwardEvent{
			ForwardEvent: &ForwardEvent{
				Info: rpcInfo(e.HtlcInfo),
			},
		}

		key = e.HtlcKey
		eventType = &e.HtlcEventType
		timestamp = e.Timestamp

	case *htlcswitch.ForwardingFailEvent:
		event = &HtlcEvent_ForwardFailEvent{
			ForwardFailEvent: &ForwardFailEvent{},
		}

		key = e.HtlcKey
		eventType = &e.HtlcEventType
		timestamp = e.Timestamp

	case *htlcswitch.LinkFailEvent:
		failureCode, failReason, err := rpcFailReason(
			e.LinkError,
		)
		if err != nil {
			return nil, err
		}

		event = &HtlcEvent_LinkFailEvent{
			LinkFailEvent: &LinkFailEvent{
				Info:          rpcInfo(e.HtlcInfo),
				WireFailure:   failureCode,
				FailureDetail: failReason,
				FailureString: e.LinkError.Error(),
			},
		}

		key = e.HtlcKey
		eventType = &e.HtlcEventType
		timestamp = e.Timestamp

	case *htlcswitch.SettleEvent:
		event = &HtlcEvent_SettleEvent{
			SettleEvent: &SettleEvent{
				Preimage: e.Preimage[:],
			},
		}

		key = e.HtlcKey
		eventType = &e.HtlcEventType
		timestamp = e.Timestamp

	case *htlcswitch.FinalHtlcEvent:
		event = &HtlcEvent_FinalHtlcEvent{
			FinalHtlcEvent: &FinalHtlcEvent{
				Settled:  e.Settled,
				Offchain: e.Offchain,
			},
		}

		key = htlcswitch.HtlcKey{
			IncomingCircuit: e.CircuitKey,
		}
		timestamp = e.Timestamp

	default:
		return nil, fmt.Errorf("unknown event type: %T", e)
	}

	rpcEvent := &HtlcEvent{
		IncomingChannelId: key.IncomingCircuit.ChanID.ToUint64(),
		OutgoingChannelId: key.OutgoingCircuit.ChanID.ToUint64(),
		IncomingHtlcId:    key.IncomingCircuit.HtlcID,
		OutgoingHtlcId:    key.OutgoingCircuit.HtlcID,
		TimestampNs:       uint64(timestamp.UnixNano()),
		Event:             event,
	}

	// Convert the htlc event type to a rpc event.
	if eventType != nil {
		switch *eventType {
		case htlcswitch.HtlcEventTypeSend:
			rpcEvent.EventType = HtlcEvent_SEND

		case htlcswitch.HtlcEventTypeReceive:
			rpcEvent.EventType = HtlcEvent_RECEIVE

		case htlcswitch.HtlcEventTypeForward:
			rpcEvent.EventType = HtlcEvent_FORWARD

		default:
			return nil, fmt.Errorf("unknown event type: %v",
				eventType)
		}
	}

	return rpcEvent, nil
}

// rpcInfo returns a rpc struct containing the htlc information from the
// switch's htlc info struct.
func rpcInfo(info htlcswitch.HtlcInfo) *HtlcInfo {
	return &HtlcInfo{
		IncomingTimelock: info.IncomingTimeLock,
		OutgoingTimelock: info.OutgoingTimeLock,
		IncomingAmtMsat:  uint64(info.IncomingAmt),
		OutgoingAmtMsat:  uint64(info.OutgoingAmt),
	}
}

// rpcFailReason maps a lnwire failure message and failure detail to a rpc
// failure code and detail.
func rpcFailReason(linkErr *htlcswitch.LinkError) (lnrpc.Failure_FailureCode,
	FailureDetail, error) {

	wireErr, err := marshallError(linkErr)
	if err != nil {
		return 0, 0, err
	}
	wireCode := wireErr.GetCode()

	// If the link has no failure detail, return with failure detail none.
	if linkErr.FailureDetail == nil {
		return wireCode, FailureDetail_NO_DETAIL, nil
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
	FailureDetail, error) {

	switch invoiceFailure {
	case invoices.ResultReplayToCanceled:
		return FailureDetail_INVOICE_CANCELED, nil

	case invoices.ResultInvoiceAlreadyCanceled:
		return FailureDetail_INVOICE_CANCELED, nil

	case invoices.ResultAmountTooLow:
		return FailureDetail_INVOICE_UNDERPAID, nil

	case invoices.ResultExpiryTooSoon:
		return FailureDetail_INVOICE_EXPIRY_TOO_SOON, nil

	case invoices.ResultCanceled:
		return FailureDetail_INVOICE_CANCELED, nil

	case invoices.ResultInvoiceNotOpen:
		return FailureDetail_INVOICE_NOT_OPEN, nil

	case invoices.ResultMppTimeout:
		return FailureDetail_MPP_INVOICE_TIMEOUT, nil

	case invoices.ResultAddressMismatch:
		return FailureDetail_ADDRESS_MISMATCH, nil

	case invoices.ResultHtlcSetTotalMismatch:
		return FailureDetail_SET_TOTAL_MISMATCH, nil

	case invoices.ResultHtlcSetTotalTooLow:
		return FailureDetail_SET_TOTAL_TOO_LOW, nil

	case invoices.ResultHtlcSetOverpayment:
		return FailureDetail_SET_OVERPAID, nil

	case invoices.ResultInvoiceNotFound:
		return FailureDetail_UNKNOWN_INVOICE, nil

	case invoices.ResultKeySendError:
		return FailureDetail_INVALID_KEYSEND, nil

	case invoices.ResultMppInProgress:
		return FailureDetail_MPP_IN_PROGRESS, nil

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
	FailureDetail, error) {

	switch failureDetail {
	case htlcswitch.OutgoingFailureNone:
		return FailureDetail_NO_DETAIL, nil

	case htlcswitch.OutgoingFailureDecodeError:
		return FailureDetail_ONION_DECODE, nil

	case htlcswitch.OutgoingFailureLinkNotEligible:
		return FailureDetail_LINK_NOT_ELIGIBLE, nil

	case htlcswitch.OutgoingFailureOnChainTimeout:
		return FailureDetail_ON_CHAIN_TIMEOUT, nil

	case htlcswitch.OutgoingFailureHTLCExceedsMax:
		return FailureDetail_HTLC_EXCEEDS_MAX, nil

	case htlcswitch.OutgoingFailureInsufficientBalance:
		return FailureDetail_INSUFFICIENT_BALANCE, nil

	case htlcswitch.OutgoingFailureCircularRoute:
		return FailureDetail_CIRCULAR_ROUTE, nil

	case htlcswitch.OutgoingFailureIncompleteForward:
		return FailureDetail_INCOMPLETE_FORWARD, nil

	case htlcswitch.OutgoingFailureDownstreamHtlcAdd:
		return FailureDetail_HTLC_ADD_FAILED, nil

	case htlcswitch.OutgoingFailureForwardsDisabled:
		return FailureDetail_FORWARDS_DISABLED, nil

	default:
		return 0, fmt.Errorf("unknown outgoing failure "+
			"detail: %v", failureDetail.FailureString())
	}
}

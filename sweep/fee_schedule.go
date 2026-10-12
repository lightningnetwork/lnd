package sweep

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// FeeSchedule records the origin of an input's fee policy. Its starting rate
// is resolved once, before submission to the publisher, and is not capped by
// an individual batch's budget. The publisher applies that cap when evaluating
// the schedule. Neither elapsed time nor failed attempts mutate this origin.
//
// Schedules survive regrouping within this process. They are not persisted;
// after restart, a publication recovered from the mempool and sweeper store
// anchors a continuation at the recovery height. Without one, reoffered inputs
// establish fresh schedules.
type FeeSchedule struct {
	// StartHeight is the first eligible attempt height, or the recovery
	// height when continuing from a restored publication.
	StartHeight int32

	// StartingFeeRate is requested, estimated once, or recovered from a
	// publication. None means the schedule carries no baseline: either it
	// has not yet been attempted, or its deadline was already within one
	// block at its origin, so the ramp is constant at whatever cap applies.
	StartingFeeRate fn.Option[chainfee.SatPerKWeight]
}

// InputSchedule is an input's fee schedule within a sweep request, together
// with the input's contribution to the request's budget. The contribution
// relates the input to a particular batch, so it belongs to the request rather
// than to the schedule's origin. Its share of the batch's total is the share
// of the batch's fee headroom that the schedule's elapsed urgency can commit.
type InputSchedule struct {
	FeeSchedule

	// Budget is the input's own budget plus any extra budget that the aux
	// sweeper attributes to it.
	Budget btcutil.Amount
}

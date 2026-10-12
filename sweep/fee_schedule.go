package sweep

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
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

// startFeeSchedule fixes the first eligible attempt height at registration.
// Mature deferred requests wait for the next block; immature inputs can be
// attempted on the block that makes them eligible, without another deferral.
func (s *UtxoSweeper) startFeeSchedule(pi *SweeperInput) {
	if pi.feeSchedule != nil {
		return
	}

	height := s.currentHeight
	if !pi.params.Immediate {
		height++
	}
	locktime, _ := pi.RequiredLockTime()
	// CSV spends can enter the mempool one block before the expiry. This
	// is the same eligibility rule used by SweeperInput.isMature.
	csvHeight := int32(pi.HeightHint()) + int32(pi.BlocksToMaturity()) - 1
	height = max(height, int32(locktime), csvHeight)
	pi.feeSchedule = &FeeSchedule{
		StartHeight:     height,
		StartingFeeRate: pi.params.StartingFeeRate,
	}
}

// updateFeeSchedulePolicy restarts the schedule for an explicit policy change.
// Reoffering the same policy or merely requesting immediate publication does
// not restart its clock.
func (pi *SweeperInput) updateFeeSchedulePolicy(params Params) {
	deadline := params.DeadlineHeight.UnwrapOr(pi.DeadlineHeight)
	if pi.params.StartingFeeRate != params.StartingFeeRate ||
		pi.params.Budget != params.Budget ||
		deadline != pi.DeadlineHeight {

		pi.feeSchedule = nil
	}
}

// attachFeeContext takes snapshots from the canonical pending inputs, rather
// than the aggregator's copies. Wallet inputs have no independent sweep policy.
func (s *UtxoSweeper) attachFeeContext(req *BumpRequest) error {
	seen := make(map[chainhash.Hash]struct{})
	for _, inp := range req.Inputs {
		pi, ok := s.inputs[inp.OutPoint()]
		if !ok {
			continue
		}
		s.startFeeSchedule(pi)
		schedule := pi.feeSchedule
		confTarget := calcCurrentConfTarget(
			schedule.StartHeight, pi.DeadlineHeight,
		)
		if schedule.StartingFeeRate.IsNone() && confTarget > 1 {
			rate, err := estimateStartingFeeRate(
				s.cfg.FeeEstimator, confTarget, 0,
			)
			if err != nil {
				return err
			}
			schedule.StartingFeeRate = fn.Some(rate)
		}

		// Weight the schedule by the input's whole contribution to the
		// batch's budget. That budget already counts the contribution,
		// so an unknown share cannot be apportioned. Fail instead, and
		// leave the inputs eligible for the next block.
		extra, err := inputExtraBudget(s.cfg.AuxSweeper, inp)
		if err != nil {
			return fmt.Errorf("extra budget for input %v: %w",
				inp.OutPoint(), err)
		}
		req.FeeSchedules = append(req.FeeSchedules, InputSchedule{
			FeeSchedule: *schedule,
			Budget:      pi.params.Budget + extra,
		})

		pi.rbf.WhenSome(func(info RBFInfo) {
			if _, exists := seen[info.Txid]; exists {
				return
			}
			seen[info.Txid] = struct{}{}
			req.PreviousTxs = append(req.PreviousTxs, info)
		})
	}

	return nil
}

// invalidateFeeContext forgets replacement hints for transactions whose inputs
// have been confirmed spent. Survivors retain their policy and historical
// lastFeeRate, but must not inherit the invalidated transaction's fee floor.
func (s *UtxoSweeper) invalidateFeeContext(tx *wire.MsgTx) {
	invalid := make(map[chainhash.Hash]struct{})
	for _, txIn := range tx.TxIn {
		pi, ok := s.inputs[txIn.PreviousOutPoint]
		if !ok {
			continue
		}
		pi.rbf.WhenSome(func(info RBFInfo) {
			invalid[info.Txid] = struct{}{}
		})
	}
	for _, pi := range s.inputs {
		pi.rbf.WhenSome(func(info RBFInfo) {
			if _, ok := invalid[info.Txid]; ok {
				pi.rbf = fn.None[RBFInfo]()
			}
		})
	}
}

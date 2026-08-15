package chainntnfs

import "fmt"

// SpendFinality validates and applies a confirmation requirement.
type SpendFinality struct {
	numConfs uint32
}

// NewSpendFinality constructs a validated spend finality policy.
func NewSpendFinality(numConfs uint32) (*SpendFinality, error) {
	if numConfs == 0 || numConfs > MaxNumConfs {
		return nil, ErrNumConfsOutOfRange
	}

	return &SpendFinality{numConfs: numConfs}, nil
}

// NumConfs returns the confirmation requirement.
func (s *SpendFinality) NumConfs() uint32 {
	return s.numConfs
}

// IsFinal reports whether a spend has reached the policy's confirmation
// requirement at currentHeight.
func (s *SpendFinality) IsFinal(spendHeight, currentHeight int32) bool {
	if currentHeight < spendHeight {
		return false
	}

	confirmations := uint64(int64(currentHeight)-int64(spendHeight)) + 1

	return confirmations >= uint64(s.numConfs)
}

// WaitForSpendConfirmations waits for a spend to reach numConfs. Reorgs reset
// the candidate, and all subscriptions are canceled before returning.
func WaitForSpendConfirmations(spendEvent *SpendEvent,
	notifier ChainNotifier, pkScript []byte, finality *SpendFinality,
	quit <-chan struct{}) (*SpendDetail, error) {

	defer spendEvent.Cancel()
	if finality == nil {
		return nil, ErrNumConfsOutOfRange
	}

	var candidate *SpendDetail
	for {
		if candidate == nil {
			select {
			case spend, ok := <-spendEvent.Spend:
				if !ok {
					return nil, ErrChainNotifierShuttingDown
				}
				candidate = spend
			case <-quit:
				return nil, ErrChainNotifierShuttingDown
			}
		}

		// Drain a stale reorg left by a prior candidate. The active
		// confirmation subscription remains authoritative for this one.
		select {
		case _, ok := <-spendEvent.Reorg:
			if !ok {
				return nil, ErrChainNotifierShuttingDown
			}
		default:
		}

		confEvent, err := notifier.RegisterConfirmationsNtfn(
			candidate.SpenderTxHash, pkScript, finality.NumConfs(),
			uint32(candidate.SpendingHeight),
			WithTxIDOnlyMatch(),
		)
		if err != nil {
			return nil, fmt.Errorf("register: %w", err)
		}

		// Prefer an already-delivered authoritative confirmation over
		// later candidate events that may also be buffered.
		select {
		case confirmation, ok := <-confEvent.Confirmed:
			confEvent.Cancel()
			if !ok {
				return nil, ErrChainNotifierShuttingDown
			}

			return confirmedSpend(candidate, confirmation), nil
		default:
		}

		select {
		case next, ok := <-spendEvent.Spend:
			confEvent.Cancel()
			if !ok {
				return nil, ErrChainNotifierShuttingDown
			}
			candidate = next

		case _, ok := <-spendEvent.Reorg:
			confEvent.Cancel()
			if !ok {
				return nil, ErrChainNotifierShuttingDown
			}
			candidate = nil

		case _, ok := <-confEvent.NegativeConf:
			confEvent.Cancel()
			if !ok {
				return nil, ErrChainNotifierShuttingDown
			}
			candidate = nil

		case confirmation, ok := <-confEvent.Confirmed:
			confEvent.Cancel()
			if !ok {
				return nil, ErrChainNotifierShuttingDown
			}

			return confirmedSpend(candidate, confirmation), nil

		case <-quit:
			confEvent.Cancel()
			return nil, ErrChainNotifierShuttingDown
		}
	}
}

// confirmedSpend copies authoritative inclusion details onto a spend.
func confirmedSpend(spend *SpendDetail,
	confirmation *TxConfirmation) *SpendDetail {

	if confirmation == nil ||
		(confirmation.BlockHeight == 0 && confirmation.Tx == nil) {

		return spend
	}

	confirmed := *spend
	if confirmation.BlockHeight != 0 {
		confirmed.SpendingHeight = int32(confirmation.BlockHeight)
	}
	if confirmation.Tx != nil {
		confirmed.SpendingTx = confirmation.Tx
		hash := confirmation.Tx.TxHash()
		confirmed.SpenderTxHash = &hash
	}

	return &confirmed
}

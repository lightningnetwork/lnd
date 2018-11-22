package autopilot

import (
	"github.com/btcsuite/btcutil"
)

// HeuristicConstraints is a struct that indicate the constraints an autopilot
// heuristic must adhere to when opening channels.
type HeuristicConstraints struct {
	// MinChanSize is the smallest channel that the autopilot agent should
	// create.
	MinChanSize btcutil.Amount

	// MaxChanSize the largest channel that the autopilot agent should
	// create.
	MaxChanSize btcutil.Amount

	// ChanLimit the maximum number of channels that should be created.
	ChanLimit uint16

	// Allocation the percentage of total funds that should be committed to
	// automatic channel establishment.
	Allocation float64

	// MaxPendingOpens is the maximum number of pending channel
	// establishment goroutines that can be lingering. We cap this value in
	// order to control the level of parallelism caused by the autopilot
	// agent.
	MaxPendingOpens uint16
}

// availableChans returns the funds and number of channels slots the autopilot
// has available towards new channels, and still be within the set constraints.
func (h *HeuristicConstraints) availableChans(channels []Channel,
	funds btcutil.Amount) (btcutil.Amount, uint32) {

	// If we're already over our maximum allowed number of channels, then
	// we'll instruct the controller not to create any more channels.
	if len(channels) >= int(h.ChanLimit) {
		return 0, 0
	}

	// The number of additional channels that should be opened is the
	// difference between the channel limit, and the number of channels we
	// already have open.
	numAdditionalChans := uint32(h.ChanLimit) - uint32(len(channels))

	// First, we'll tally up the total amount of funds that are currently
	// present within the set of active channels.
	var totalChanAllocation btcutil.Amount
	for _, channel := range channels {
		totalChanAllocation += channel.Capacity
	}

	// With this value known, we'll now compute the total amount of fund
	// allocated across regular utxo's and channel utxo's.
	totalFunds := funds + totalChanAllocation

	// Once the total amount has been computed, we then calculate the
	// fraction of funds currently allocated to channels.
	fundsFraction := float64(totalChanAllocation) / float64(totalFunds)

	// If this fraction is below our threshold, then we'll return true, to
	// indicate the controller should call Select to obtain a candidate set
	// of channels to attempt to open.
	needMore := fundsFraction < h.Allocation
	if !needMore {
		return 0, 0
	}

	// Now that we know we need more funds, we'll compute the amount of
	// additional funds we should allocate towards channels.
	targetAllocation := btcutil.Amount(float64(totalFunds) * h.Allocation)
	fundsAvailable := targetAllocation - totalChanAllocation
	return fundsAvailable, numAdditionalChans
}

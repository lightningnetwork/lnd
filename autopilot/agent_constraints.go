package autopilot

import (
	"github.com/btcsuite/btcd/btcutil"
)

// AgentConstraints is an interface the agent will query to determine what
// limits it will need to stay inside when opening channels.
type AgentConstraints interface {
	// ChannelBudget should, given the passed parameters, return whether
	// more channels can be opened while still staying within the set
	// constraints. If the constraints allow us to open more channels, then
	// the first return value will represent the amount of additional funds
	// available towards creating channels. The second return value is the
	// exact *number* of additional channels available.
	ChannelBudget(chans []LocalChannel, balance btcutil.Amount) (
		btcutil.Amount, uint32)

	// MaxPendingOpens returns the maximum number of pending channel
	// establishment goroutines that can be lingering. We cap this value in
	// order to control the level of parallelism caused by the autopilot
	// agent.
	MaxPendingOpens() uint16

	// MinChanSize returns the smallest channel that the autopilot agent
	// should create.
	MinChanSize() btcutil.Amount

	// MaxChanSize returns largest channel that the autopilot agent should
	// create.
	MaxChanSize() btcutil.Amount
}

// agentConstraints is an implementation of the AgentConstraints interface that
// indicate the constraints the autopilot agent must adhere to when opening
// channels.
type agentConstraints struct {
	// minChanSize is the smallest channel that the autopilot agent should
	// create.
	minChanSize btcutil.Amount

	// maxChanSize is the largest channel that the autopilot agent should
	// create.
	maxChanSize btcutil.Amount

	// chanLimit is the maximum number of channels that should be created.
	chanLimit uint16

	// allocation is the percentage of total funds that should be committed
	// to automatic channel establishment.
	allocation float64

	// maxPendingOpens is the maximum number of pending channel
	// establishment goroutines that can be lingering. We cap this value in
	// order to control the level of parallelism caused by the autopilot
	// agent.
	maxPendingOpens uint16
}

// A compile time assertion to ensure agentConstraints satisfies the
// AgentConstraints interface.
var _ AgentConstraints = (*agentConstraints)(nil)

// NewConstraints returns a new AgentConstraints with the given limits.
func NewConstraints(minChanSize, maxChanSize btcutil.Amount, chanLimit,
	maxPendingOpens uint16, allocation float64) AgentConstraints {

	return &agentConstraints{
		minChanSize:     minChanSize,
		maxChanSize:     maxChanSize,
		chanLimit:       chanLimit,
		allocation:      allocation,
		maxPendingOpens: maxPendingOpens,
	}
}

// ChannelBudget should, given the passed parameters, return whether more
// channels can be be opened while still staying within the set constraints.
// If the constraints allow us to open more channels, then the first return
// value will represent the amount of additional funds available towards
// creating channels. The second return value is the exact *number* of
// additional channels available.
//
// Note: part of the AgentConstraints interface.
func (h *agentConstraints) ChannelBudget(channels []LocalChannel,
	funds btcutil.Amount) (btcutil.Amount, uint32) {

	// If we're already over our maximum allowed number of channels, then
	// we'll instruct the controller not to create any more channels.
	if len(channels) >= int(h.chanLimit) {
		return 0, 0
	}

	// The number of additional channels that should be opened is the
	// difference between the channel limit, and the number of channels we
	// already have open.
	numAdditionalChans := uint32(h.chanLimit) - uint32(len(channels))

	// First, we'll tally up the total amount of funds that are currently
	// present within the set of active channels.
	var totalChanAllocation btcutil.Amount
	for _, channel := range channels {
		totalChanAllocation += channel.Balance
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
	needMore := fundsFraction < h.allocation
	if !needMore {
		return 0, 0
	}

	// Now that we know we need more funds, we'll compute the amount of
	// additional funds we should allocate towards channels.
	targetAllocation := btcutil.Amount(float64(totalFunds) * h.allocation)
	fundsAvailable := targetAllocation - totalChanAllocation
	return fundsAvailable, numAdditionalChans
}

// MaxPendingOpens returns the maximum number of pending channel establishment
// goroutines that can be lingering. We cap this value in order to control the
// level of parallelism caused by the autopilot agent.
//
// Note: part of the AgentConstraints interface.
func (h *agentConstraints) MaxPendingOpens() uint16 {
	return h.maxPendingOpens
}

// MinChanSize returns the smallest channel that the autopilot agent should
// create.
//
// Note: part of the AgentConstraints interface.
func (h *agentConstraints) MinChanSize() btcutil.Amount {
	return h.minChanSize
}

// MaxChanSize returns largest channel that the autopilot agent should create.
//
// Note: part of the AgentConstraints interface.
func (h *agentConstraints) MaxChanSize() btcutil.Amount {
	return h.maxChanSize
}

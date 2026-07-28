package routing

import (
	"fmt"
)

// simArrivalWindow keeps at most max_in_flight payments live and starts the
// next one as soon as a slot frees. It is the only arrival process this stage
// implements.
const simArrivalWindow = "window"

// simArrivalPoisson is the arrival process the design spec names and defers.
// An arrival rate interacts with the background traffic prorating carry, which
// is the one piece of the simulator whose draw order every sealed tier depends
// on, so it needs a calibration run of its own rather than a default.
const simArrivalPoisson = "poisson"

// SimConcurrencyParams lets the sender run several of its OWN payments at
// once, which is the one kind of contention the simulator has never had.
//
// Three others already exist and none of them is this one. Sibling shards of a
// single payment contend under atomic_mpp (exp-010b), other people's payments
// move liquidity in the gaps (exp-014), and time passes between attempts. What
// none of them provides is the vantage node racing itself for its own outbound
// liquidity with results interleaved, which is what this section turns on.
//
// Absent, the batch runs one payment at a time, which is what every scenario
// file written before stage D says by omission.
type SimConcurrencyParams struct {
	// MaxInFlight is how many of the sender's payments may be live at
	// once. One is the sequential batch every prior tier ran, and the
	// scheduler is required to reproduce it exactly.
	MaxInFlight int `json:"max_in_flight"`

	// Arrival names the process that starts payments. Empty means
	// "window", the only one implemented: keep MaxInFlight payments live
	// and admit the next as soon as one resolves.
	Arrival string `json:"arrival,omitempty"`

	// InterArrivalSec is how much virtual time passes between a slot
	// freeing and the payment that takes it starting, in seconds. Zero
	// means the clock section's payment_gap_sec, which is the gap the
	// sequential batch has always left between its payments and is what
	// makes max_in_flight=1 reduce to that batch exactly.
	InterArrivalSec float64 `json:"inter_arrival_sec,omitempty"`
}

// validate rejects a concurrency section the scheduler cannot honor, naming
// the deferral where one applies rather than silently running something else.
func (p *SimConcurrencyParams) validate() error {
	if p == nil {
		return nil
	}

	if p.MaxInFlight <= 0 {
		return fmt.Errorf("concurrency: max_in_flight must be "+
			"positive, got %d; omit the section for the "+
			"sequential batch", p.MaxInFlight)
	}

	switch p.Arrival {
	case "", simArrivalWindow:

	case simArrivalPoisson:
		return fmt.Errorf("concurrency: arrival %q is specified but "+
			"DEFERRED; an arrival rate interacts with the "+
			"background traffic prorating carry and needs a "+
			"calibration run of its own, so use %q",
			simArrivalPoisson, simArrivalWindow)

	default:
		return fmt.Errorf("concurrency: unknown arrival %q (want %q)",
			p.Arrival, simArrivalWindow)
	}

	if p.InterArrivalSec < 0 {
		return fmt.Errorf("concurrency: inter_arrival_sec must not "+
			"be negative, got %v", p.InterArrivalSec)
	}

	return nil
}

// maxInFlight returns the window size, treating an absent section as the
// sequential batch.
func (p *SimConcurrencyParams) maxInFlight() int {
	if p == nil {
		return 1
	}

	return p.MaxInFlight
}

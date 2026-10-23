package trampolinerelay

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/routing/trampoline"
)

// ErrDispatchNotScheduled is returned when an attempt starts without first
// winning the ready-notification scheduling gate.
var ErrDispatchNotScheduled = errors.New(
	"trampoline downstream dispatch is not scheduled",
)

// DispatchGuard serializes ready notifications into one downstream attempt.
// The scheduled bit is intentionally volatile: a restored Ready relay may be
// scheduled again, while a restored Paying relay reconciles its durable
// attempt instead of dispatching another one.
type DispatchGuard struct {
	mu        sync.Mutex
	scheduled bool
}

// ScheduleReady claims the single dispatch slot for a Ready relay.
func (d *DispatchGuard) ScheduleReady(ready bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !ready || d.scheduled {
		return false
	}
	d.scheduled = true

	return true
}

// Begin atomically consumes the dispatch slot and starts the relay attempt.
func (d *DispatchGuard) Begin(
	machine *trampoline.RelayMachine) ([]trampoline.Action, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.scheduled {
		return nil, ErrDispatchNotScheduled
	}
	d.scheduled = false

	return machine.BeginAttempt()
}

// Finish records whether a committed retry transition needs another attempt.
func (d *DispatchGuard) Finish(retry bool) {
	d.mu.Lock()
	d.scheduled = retry
	d.mu.Unlock()
}

// Scheduled reports whether a downstream dispatch is currently claimed.
func (d *DispatchGuard) Scheduled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.scheduled
}

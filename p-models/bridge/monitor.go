package bridge

import (
	"fmt"

	"github.com/lightningnetwork/lnd/routing/trampoline"
)

// Monitor applies the P safety invariants to production observations.
type Monitor struct {
	config   trampoline.RelayConfig
	resolved map[trampoline.PartID]struct{}
	started  map[uint16]struct{}
	terminal bool
}

// NewMonitor constructs an empty runtime monitor.
func NewMonitor(config trampoline.RelayConfig) *Monitor {
	return &Monitor{
		config:   config,
		resolved: make(map[trampoline.PartID]struct{}),
		started:  make(map[uint16]struct{}),
	}
}

// Observe validates one committed snapshot and its emitted side effects.
func (m *Monitor) Observe(snapshot trampoline.RelaySnapshot,
	actions []trampoline.Action) error {

	var collected uint64
	parts := make(map[trampoline.PartID]trampoline.RelayPart)
	for _, part := range snapshot.Parts {
		if _, ok := parts[part.ID]; ok {
			return fmt.Errorf("monitor: duplicate part in snapshot")
		}
		parts[part.ID] = part
		if collected > ^uint64(0)-part.AmountMsat {
			return fmt.Errorf("monitor: collected amount overflow")
		}
		collected += part.AmountMsat
	}

	if collected != snapshot.CollectedMsat {
		return fmt.Errorf(
			"monitor: collected=%d, parts=%d",
			snapshot.CollectedMsat, collected,
		)
	}
	if snapshot.DownstreamMsat > snapshot.CollectedMsat &&
		snapshot.State != trampoline.RelayCollecting {

		return fmt.Errorf("monitor: downstream payment created value")
	}
	if snapshot.Attempts > m.config.MaxAttempts {
		return fmt.Errorf("monitor: retry bound exceeded")
	}
	if snapshot.State == trampoline.RelayRecovering &&
		(snapshot.RecoveryDeadline <= snapshot.ObservedHeight ||
			snapshot.RecoveryDeadline > snapshot.HoldDeadline) {

		return fmt.Errorf("monitor: invalid recovery deadline")
	}
	if snapshot.State.IsTerminal() {
		m.terminal = true
		for _, part := range snapshot.Parts {
			if part.Resolution == trampoline.PartUnresolved {
				return fmt.Errorf(
					"monitor: terminal part is unresolved",
				)
			}
		}
	}

	for _, action := range actions {
		switch action.Kind {
		case trampoline.ActionPaymentReady:
			if snapshot.State != trampoline.RelayReady {
				return fmt.Errorf(
					"monitor: ready action in state %d",
					snapshot.State,
				)
			}

		case trampoline.ActionStartDownstream:
			if m.terminal {
				return fmt.Errorf(
					"monitor: forward after terminal",
				)
			}
			if snapshot.State != trampoline.RelayPaying {
				return fmt.Errorf(
					"monitor: forward in state %d",
					snapshot.State,
				)
			}
			if action.AmountMsat != snapshot.DownstreamMsat ||
				action.AmountMsat > snapshot.CollectedMsat {

				return fmt.Errorf(
					"monitor: invalid downstream amount",
				)
			}
			if action.AttemptID == 0 ||
				action.AttemptID > m.config.MaxAttempts {

				return fmt.Errorf(
					"monitor: invalid downstream attempt",
				)
			}
			if _, ok := m.started[action.AttemptID]; ok {
				return fmt.Errorf(
					"monitor: downstream attempt " +
						"started twice",
				)
			}
			m.started[action.AttemptID] = struct{}{}

		case trampoline.ActionRetryReady:
			if snapshot.State != trampoline.RelayReady {
				return fmt.Errorf(
					"monitor: retry action in state %d",
					snapshot.State,
				)
			}

		case trampoline.ActionQueryDownstream:
			if snapshot.State != trampoline.RelayRecovering &&
				snapshot.State != trampoline.RelayCanceling {

				return fmt.Errorf(
					"monitor: query action in state %d",
					snapshot.State,
				)
			}

		case trampoline.ActionCancelDownstream:
			if snapshot.State != trampoline.RelayCanceling ||
				snapshot.ActiveAttempt == 0 ||
				action.AttemptID != snapshot.ActiveAttempt {

				return fmt.Errorf(
					"monitor: cancellation without " +
						"active attempt",
				)
			}

		case trampoline.ActionFulfillPart,
			trampoline.ActionFailPart:

			if _, ok := m.resolved[action.PartID]; ok {
				return fmt.Errorf(
					"monitor: part resolved more than once",
				)
			}
			m.resolved[action.PartID] = struct{}{}

			part, ok := parts[action.PartID]
			if !ok || action.AmountMsat != part.AmountMsat {
				return fmt.Errorf(
					"monitor: resolution does not " +
						"match part",
				)
			}
			if action.Kind == trampoline.ActionFulfillPart &&
				snapshot.State != trampoline.RelaySucceeded {

				return fmt.Errorf(
					"monitor: fulfillment without success",
				)
			}
			if action.Kind == trampoline.ActionFailPart &&
				snapshot.State != trampoline.RelayFailed &&
				snapshot.State != trampoline.RelayCanceled &&
				snapshot.State != trampoline.RelayTimedOut {

				return fmt.Errorf(
					"monitor: failure without terminal " +
						"failure",
				)
			}

		default:
			return fmt.Errorf(
				"monitor: unknown action kind %d", action.Kind,
			)
		}
	}

	return nil
}

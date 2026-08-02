// Package bridge replays P model traces against the production trampoline
// relay state machine.
package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lightningnetwork/lnd/routing/trampoline"
)

const maxTraceSize = 1 << 20

// Trace is a versioned model trace and its production relay configuration.
type Trace struct {
	Version    uint8       `json:"version"`
	ID         string      `json:"id"`
	Config     TraceConfig `json:"config"`
	Relay      TraceRelay  `json:"relay"`
	Events     []Event     `json:"events"`
	Invariants []string    `json:"invariants"`
}

// TraceConfig is the JSON representation of trampoline.RelayConfig.
type TraceConfig struct {
	MaxParts           uint16 `json:"max_parts"`
	MaxAttempts        uint16 `json:"max_attempts"`
	HoldBlocks         uint32 `json:"hold_blocks"`
	RecoveryBlocks     uint32 `json:"recovery_blocks"`
	ExpirySafetyBlocks uint32 `json:"expiry_safety_blocks"`
	MaxReorgDepth      uint32 `json:"max_reorg_depth"`
}

// RelayConfig converts the trace configuration to the production type.
func (c TraceConfig) RelayConfig() trampoline.RelayConfig {
	return trampoline.RelayConfig{
		MaxParts:           c.MaxParts,
		MaxAttempts:        c.MaxAttempts,
		HoldBlocks:         c.HoldBlocks,
		RecoveryBlocks:     c.RecoveryBlocks,
		ExpirySafetyBlocks: c.ExpirySafetyBlocks,
		MaxReorgDepth:      c.MaxReorgDepth,
	}
}

// TraceRelay contains the immutable fields used to construct the relay.
type TraceRelay struct {
	ExpectedTotalMsat uint64 `json:"expected_total_msat"`
	DownstreamMsat    uint64 `json:"downstream_msat"`
	RequiredCLTV      uint32 `json:"required_cltv"`
	StartHeight       uint32 `json:"start_height"`
}

// Event is one model transition and its expected production observation.
type Event struct {
	Seq      uint32          `json:"seq"`
	Name     string          `json:"event"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Expected Expected        `json:"expected"`
}

// Expected describes the state, side effects, or error after an event.
type Expected struct {
	State             string           `json:"state,omitempty"`
	CollectedMsat     *uint64          `json:"collected_msat,omitempty"`
	Attempts          *uint16          `json:"attempts,omitempty"`
	ActiveAttempt     *uint16          `json:"active_attempt,omitempty"`
	DispatchClaimed   *bool            `json:"dispatch_claimed,omitempty"`
	DispatchScheduled *bool            `json:"dispatch_scheduled,omitempty"`
	Actions           []ExpectedAction `json:"actions,omitempty"`
	ErrorContains     string           `json:"error_contains,omitempty"`
}

// ExpectedAction is the stable JSON form of a production relay action.
type ExpectedAction struct {
	Kind       string `json:"kind"`
	ChannelID  uint64 `json:"channel_id,omitempty"`
	HTLCID     uint64 `json:"htlc_id,omitempty"`
	AttemptID  uint16 `json:"attempt_id,omitempty"`
	AmountMsat uint64 `json:"amount_msat,omitempty"`
}

// ParseTraceFile parses and validates one bounded JSON trace.
func ParseTraceFile(path string) (*Trace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}

	return ParseTrace(bytes.NewReader(data))
}

// ParseTrace parses and validates one bounded, strict JSON trace.
func ParseTrace(reader io.Reader) (*Trace, error) {
	limited := io.LimitReader(reader, maxTraceSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}
	if len(data) == 0 || len(data) > maxTraceSize {
		return nil, fmt.Errorf("invalid trace size: %d", len(data))
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var trace Trace
	if err := decoder.Decode(&trace); err != nil {
		return nil, fmt.Errorf("decode trace: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("decode trace: trailing JSON")
	}
	if err := trace.Validate(); err != nil {
		return nil, err
	}

	return &trace, nil
}

// Validate checks trace identity, configuration, sequencing, and event names.
func (t *Trace) Validate() error {
	if t.Version != 1 {
		return fmt.Errorf("unsupported trace version: %d", t.Version)
	}
	if t.ID == "" {
		return fmt.Errorf("trace ID is empty")
	}
	if err := t.Config.RelayConfig().Validate(); err != nil {
		return fmt.Errorf("trace config: %w", err)
	}
	if t.Relay.ExpectedTotalMsat == 0 {
		return fmt.Errorf("trace expected total is zero")
	}
	if len(t.Events) == 0 {
		return fmt.Errorf("trace has no events")
	}

	for i := range t.Events {
		event := &t.Events[i]
		if event.Seq != uint32(i) {
			return fmt.Errorf(
				"event %d has sequence %d", i, event.Seq,
			)
		}
		if !supportedEvent(event.Name) {
			return fmt.Errorf(
				"event %d has unsupported name %q", i,
				event.Name,
			)
		}
	}

	return nil
}

func supportedEvent(name string) bool {
	switch name {
	case "add_part", "notify_ready", "begin_attempt", "attempt_success",
		"attempt_failure", "cancel", "observe_height", "restart",
		"reconcile_success", "reconcile_failure",
		"reconcile_pending":

		return true

	default:
		return false
	}
}

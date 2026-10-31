package trampoline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
)

const (
	relaySnapshotVersion = 1
	maxRelaySnapshotSize = 1 << 20
)

var (
	// ErrInvalidRelayConfig is returned when a relay machine is created
	// with unsafe or internally inconsistent bounds.
	ErrInvalidRelayConfig = errors.New("invalid trampoline relay config")

	// ErrInvalidRelaySnapshot is returned when persisted relay state breaks
	// a state-machine invariant.
	ErrInvalidRelaySnapshot = errors.New(
		"invalid trampoline relay snapshot",
	)

	// ErrRelayTerminal is returned when an event cannot apply after a relay
	// payment has reached a terminal state.
	ErrRelayTerminal = errors.New("trampoline relay is terminal")

	// ErrRelayState is returned when an event is invalid for the current
	// non-terminal state.
	ErrRelayState = errors.New("invalid trampoline relay state transition")

	// ErrPartConflict is returned when an existing part ID is replayed with
	// different immutable fields.
	ErrPartConflict = errors.New("conflicting trampoline payment part")

	// ErrPartLimit is returned when an MPP group exceeds its local part
	// count bound.
	ErrPartLimit = errors.New("trampoline payment part limit exceeded")

	// ErrPartTotalExceeded is returned when another part would exceed the
	// outer onion's declared MPP total.
	ErrPartTotalExceeded = errors.New(
		"trampoline payment parts exceed declared total",
	)

	// ErrPartCLTVInsufficient is returned when a part cannot fund the
	// downstream CLTV budget.
	ErrPartCLTVInsufficient = errors.New(
		"trampoline payment part cltv insufficient",
	)

	// ErrAttemptMismatch is returned for a result that does not belong to
	// the active downstream attempt.
	ErrAttemptMismatch = errors.New(
		"trampoline downstream attempt mismatch",
	)

	// ErrReorgTooDeep is returned when an observed height rolls back beyond
	// the configured model and persistence safety bound.
	ErrReorgTooDeep = errors.New("trampoline relay reorg exceeds bound")
)

// RelayState is the durable lifecycle state of one incoming trampoline MPP
// group and its single logical downstream payment.
type RelayState uint8

const (
	// RelayCollecting waits for every part declared by the outer MPP total.
	RelayCollecting RelayState = iota

	// RelayReady has a complete incoming amount and may begin an attempt.
	RelayReady

	// RelayPaying has one active downstream attempt.
	RelayPaying

	// RelayRecovering reconciles an attempt that was active across restart.
	RelayRecovering

	// RelaySucceeded fulfilled every accepted upstream part.
	RelaySucceeded

	// RelayFailed failed every accepted upstream part.
	RelayFailed

	// RelayCanceled failed every accepted part because of cancellation.
	RelayCanceled

	// RelayTimedOut failed every accepted part after the hold deadline.
	RelayTimedOut

	// RelayCanceling waits for an active downstream attempt to become
	// terminal after cancellation was requested. Upstream parts remain held
	// because a concurrently settling downstream attempt must still win.
	RelayCanceling
)

// IsTerminal reports whether no further payment-lifecycle transition is
// allowed.
func (s RelayState) IsTerminal() bool {
	switch s {
	case RelaySucceeded, RelayFailed, RelayCanceled, RelayTimedOut:
		return true

	default:
		return false
	}
}

// PartResolution is the durable resolution of one accepted upstream part.
type PartResolution uint8

const (
	// PartUnresolved is held while collection or downstream payment is in
	// progress.
	PartUnresolved PartResolution = iota

	// PartFulfilled settles the upstream HTLC with the downstream preimage.
	PartFulfilled

	// PartFailed fails the upstream HTLC.
	PartFailed
)

// RelayConfig supplies explicit resource, retry, timeout, and reorg bounds.
type RelayConfig struct {
	// MaxParts is the maximum number of accepted incoming MPP parts.
	MaxParts uint16

	// MaxAttempts is the maximum number of logical downstream attempts.
	MaxAttempts uint16

	// HoldBlocks bounds collection and downstream execution.
	HoldBlocks uint32

	// RecoveryBlocks bounds reconciliation of an in-flight attempt after
	// restart.
	RecoveryBlocks uint32

	// ExpirySafetyBlocks keeps cancellation and upstream resolution ahead
	// of the earliest accepted HTLC expiry.
	ExpirySafetyBlocks uint32

	// MaxReorgDepth bounds backward height movement accepted by the
	// lifecycle model.
	MaxReorgDepth uint32
}

// Validate checks that every safety bound is non-zero and arithmetically safe.
func (c RelayConfig) Validate() error {
	switch {
	case c.MaxParts == 0:
		return fmt.Errorf(
			"%w: max parts is zero", ErrInvalidRelayConfig,
		)

	case c.MaxAttempts == 0:
		return fmt.Errorf("%w: max attempts is zero",
			ErrInvalidRelayConfig)

	case c.HoldBlocks == 0:
		return fmt.Errorf("%w: hold blocks is zero",
			ErrInvalidRelayConfig)

	case c.RecoveryBlocks == 0:
		return fmt.Errorf("%w: recovery blocks is zero",
			ErrInvalidRelayConfig)

	case c.ExpirySafetyBlocks == 0:
		return fmt.Errorf("%w: expiry safety blocks is zero",
			ErrInvalidRelayConfig)
	}

	return nil
}

// RelayID is the local aggregation key. The payment hash binds the HTLCs and
// the random outer payment address isolates independent trampoline legs.
type RelayID struct {
	PaymentHash [32]byte `json:"payment_hash"`
	PaymentAddr [32]byte `json:"payment_addr"`
}

// PartID identifies an upstream HTLC without depending on arrival order.
type PartID struct {
	ChannelID uint64 `json:"channel_id"`
	HTLCID    uint64 `json:"htlc_id"`
}

// RelayPart is the durable immutable input and mutable resolution of one part.
type RelayPart struct {
	ID         PartID         `json:"id"`
	AmountMsat uint64         `json:"amount_msat"`
	CLTV       uint32         `json:"cltv"`
	Resolution PartResolution `json:"resolution"`
}

// ActionKind identifies an external side effect emitted by a committed state
// transition.
type ActionKind uint8

const (
	// ActionPaymentReady reports that all declared incoming parts arrived.
	ActionPaymentReady ActionKind = iota

	// ActionStartDownstream requests one persisted downstream attempt.
	ActionStartDownstream

	// ActionRetryReady reports that a retryable attempt returned to ready.
	ActionRetryReady

	// ActionQueryDownstream requests reconciliation after restart.
	ActionQueryDownstream

	// ActionFulfillPart resolves one upstream part successfully.
	ActionFulfillPart

	// ActionFailPart resolves one upstream part unsuccessfully.
	ActionFailPart

	// ActionCancelDownstream requests durable cancellation of the active
	// downstream payment without resolving any upstream part.
	ActionCancelDownstream
)

// Action is a deterministic external effect. PartID is set for part
// resolutions and AttemptID is set for downstream actions.
type Action struct {
	Kind       ActionKind
	PartID     PartID
	AttemptID  uint16
	AmountMsat uint64
}

// RelaySnapshot is the canonical durable state of a relay machine.
type RelaySnapshot struct {
	Version           uint8       `json:"version"`
	ID                RelayID     `json:"id"`
	State             RelayState  `json:"state"`
	ExpectedTotalMsat uint64      `json:"expected_total_msat"`
	DownstreamMsat    uint64      `json:"downstream_msat"`
	CollectedMsat     uint64      `json:"collected_msat"`
	RequiredCLTV      uint32      `json:"required_cltv"`
	StartHeight       uint32      `json:"start_height"`
	ObservedHeight    uint32      `json:"observed_height"`
	PeakHeight        uint32      `json:"peak_height"`
	HoldDeadline      uint32      `json:"hold_deadline"`
	RecoveryDeadline  uint32      `json:"recovery_deadline"`
	Attempts          uint16      `json:"attempts"`
	ActiveAttempt     uint16      `json:"active_attempt"`
	Parts             []RelayPart `json:"parts"`
}

// RelayMachine applies lifecycle events atomically and emits side effects only
// after its in-memory transition has committed.
type RelayMachine struct {
	mu     sync.Mutex
	config RelayConfig
	state  RelaySnapshot
	parts  map[PartID]RelayPart
}

// NewRelayMachine creates an empty MPP aggregation group.
func NewRelayMachine(config RelayConfig, id RelayID, expectedTotalMsat,
	downstreamMsat uint64, requiredCLTV,
	startHeight uint32) (*RelayMachine, error) {

	if err := config.Validate(); err != nil {
		return nil, err
	}
	if expectedTotalMsat == 0 {
		return nil, fmt.Errorf("%w: expected total is zero",
			ErrInvalidRelayConfig)
	}
	if downstreamMsat == 0 || downstreamMsat > expectedTotalMsat {
		return nil, fmt.Errorf("%w: downstream=%d total=%d",
			ErrInvalidRelayConfig, downstreamMsat,
			expectedTotalMsat)
	}
	if startHeight > math.MaxUint32-config.HoldBlocks {
		return nil, fmt.Errorf("%w: hold deadline overflow",
			ErrInvalidRelayConfig)
	}

	snapshot := RelaySnapshot{
		Version:           relaySnapshotVersion,
		ID:                id,
		State:             RelayCollecting,
		ExpectedTotalMsat: expectedTotalMsat,
		DownstreamMsat:    downstreamMsat,
		RequiredCLTV:      requiredCLTV,
		StartHeight:       startHeight,
		ObservedHeight:    startHeight,
		PeakHeight:        startHeight,
		HoldDeadline:      startHeight + config.HoldBlocks,
	}

	return &RelayMachine{
		config: config,
		state:  snapshot,
		parts:  make(map[PartID]RelayPart),
	}, nil
}

// AddPart idempotently accepts one upstream MPP part.
func (m *RelayMachine) AddPart(part RelayPart) ([]Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.State.IsTerminal() {
		return nil, ErrRelayTerminal
	}

	if existing, ok := m.parts[part.ID]; ok {
		if existing.AmountMsat != part.AmountMsat ||
			existing.CLTV != part.CLTV {

			return nil, ErrPartConflict
		}

		return nil, nil
	}

	if m.state.State != RelayCollecting {
		return nil, fmt.Errorf("%w: add part in state %d",
			ErrRelayState, m.state.State)
	}
	if part.AmountMsat == 0 {
		return nil, fmt.Errorf("%w: zero amount", ErrPartConflict)
	}
	if part.CLTV < m.state.RequiredCLTV {
		return nil, ErrPartCLTVInsufficient
	}
	if part.CLTV <= m.config.ExpirySafetyBlocks {
		return nil, ErrPartCLTVInsufficient
	}
	safeDeadline := part.CLTV - m.config.ExpirySafetyBlocks
	if safeDeadline <= m.state.ObservedHeight {
		return nil, ErrPartCLTVInsufficient
	}
	if len(m.parts) >= int(m.config.MaxParts) {
		return nil, ErrPartLimit
	}
	if part.AmountMsat > m.state.ExpectedTotalMsat-m.state.CollectedMsat {
		return nil, ErrPartTotalExceeded
	}

	part.Resolution = PartUnresolved
	m.parts[part.ID] = part
	m.state.CollectedMsat += part.AmountMsat
	if safeDeadline < m.state.HoldDeadline {
		m.state.HoldDeadline = safeDeadline
	}
	if m.state.CollectedMsat != m.state.ExpectedTotalMsat {
		return nil, nil
	}

	m.state.State = RelayReady

	return []Action{{
		Kind:       ActionPaymentReady,
		AmountMsat: m.state.CollectedMsat,
	}}, nil
}

// BeginAttempt starts the only permitted downstream attempt.
func (m *RelayMachine) BeginAttempt() ([]Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.State.IsTerminal() {
		return nil, ErrRelayTerminal
	}
	if m.state.State != RelayReady {
		return nil, fmt.Errorf("%w: begin attempt in state %d",
			ErrRelayState, m.state.State)
	}
	if m.state.Attempts >= m.config.MaxAttempts {
		return m.failAllLocked(RelayFailed), nil
	}

	m.state.Attempts++
	m.state.ActiveAttempt = m.state.Attempts
	m.state.State = RelayPaying

	return []Action{{
		Kind:       ActionStartDownstream,
		AttemptID:  m.state.ActiveAttempt,
		AmountMsat: m.state.DownstreamMsat,
	}}, nil
}

// AttemptSucceeded fulfills every accepted part exactly once.
func (m *RelayMachine) AttemptSucceeded(attemptID uint16) ([]Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.State.IsTerminal() {
		return nil, nil
	}
	if m.state.State != RelayPaying &&
		m.state.State != RelayRecovering &&
		m.state.State != RelayCanceling {

		return nil, fmt.Errorf("%w: success in state %d",
			ErrRelayState, m.state.State)
	}
	if attemptID != m.state.ActiveAttempt {
		return nil, ErrAttemptMismatch
	}

	return m.resolveAllLocked(RelaySucceeded, PartFulfilled), nil
}

// AttemptFailed handles one authenticated downstream attempt result.
func (m *RelayMachine) AttemptFailed(attemptID uint16,
	retryable bool) ([]Action, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.State.IsTerminal() {
		return nil, nil
	}
	if m.state.State != RelayPaying &&
		m.state.State != RelayRecovering &&
		m.state.State != RelayCanceling {

		return nil, fmt.Errorf("%w: failure in state %d",
			ErrRelayState, m.state.State)
	}
	if attemptID != m.state.ActiveAttempt {
		return nil, ErrAttemptMismatch
	}
	if m.state.State == RelayCanceling {
		return m.failAllLocked(RelayTimedOut), nil
	}

	if retryable && m.state.Attempts < m.config.MaxAttempts &&
		m.state.ObservedHeight < m.state.HoldDeadline {

		m.state.State = RelayReady
		m.state.ActiveAttempt = 0
		m.state.RecoveryDeadline = 0

		return []Action{{
			Kind:      ActionRetryReady,
			AttemptID: attemptID,
		}}, nil
	}

	return m.failAllLocked(RelayFailed), nil
}

// Cancel terminates the group and fails every accepted part exactly once.
func (m *RelayMachine) Cancel() ([]Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.State.IsTerminal() {
		return nil, nil
	}
	if m.state.State == RelayCanceling {
		return nil, nil
	}
	if m.state.State == RelayPaying ||
		m.state.State == RelayRecovering {

		return m.cancelDownstreamLocked(), nil
	}

	return m.failAllLocked(RelayCanceled), nil
}

// ObserveHeight applies forward progress or a bounded reorg and enforces hold
// and recovery deadlines.
func (m *RelayMachine) ObserveHeight(height uint32) ([]Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.State.IsTerminal() {
		return nil, nil
	}
	if height < m.state.PeakHeight &&
		m.state.PeakHeight-height > m.config.MaxReorgDepth {

		return nil, ErrReorgTooDeep
	}

	m.state.ObservedHeight = height
	if height > m.state.PeakHeight {
		m.state.PeakHeight = height
	}

	recoveryExpired := m.state.State == RelayRecovering &&
		height >= m.state.RecoveryDeadline
	if height < m.state.HoldDeadline && !recoveryExpired {
		return nil, nil
	}

	switch m.state.State {
	case RelayPaying, RelayRecovering:
		return m.cancelDownstreamLocked(), nil

	case RelayCanceling:
		return nil, nil

	default:
		return m.failAllLocked(RelayTimedOut), nil
	}
}

// Snapshot returns a canonical, part-ID-sorted durable snapshot.
func (m *RelayMachine) Snapshot() RelaySnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.snapshotLocked()
}

// EncodeSnapshot encodes the current snapshot with a bounded versioned JSON
// format shared by persistence tests and the P trace bridge.
func (m *RelayMachine) EncodeSnapshot() ([]byte, error) {
	snapshot := m.Snapshot()
	return json.Marshal(snapshot)
}

// DecodeRelaySnapshot parses one bounded durable snapshot and rejects trailing
// values or unknown fields.
func DecodeRelaySnapshot(data []byte) (RelaySnapshot, error) {
	if len(data) == 0 || len(data) > maxRelaySnapshotSize {
		return RelaySnapshot{}, fmt.Errorf(
			"%w: size=%d", ErrInvalidRelaySnapshot, len(data),
		)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var snapshot RelaySnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return RelaySnapshot{}, fmt.Errorf(
			"%w: %w", ErrInvalidRelaySnapshot, err,
		)
	}

	var trailing any
	err := decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		return RelaySnapshot{}, fmt.Errorf(
			"%w: trailing json", ErrInvalidRelaySnapshot,
		)
	}

	return snapshot, nil
}

// RestoreRelayMachine validates a durable snapshot. An active attempt enters
// recovery and emits a query action before any new attempt may start.
func RestoreRelayMachine(config RelayConfig,
	snapshot RelaySnapshot) (*RelayMachine, []Action, error) {

	if err := config.Validate(); err != nil {
		return nil, nil, err
	}

	machine := &RelayMachine{
		config: config,
		state:  snapshot,
		parts:  make(map[PartID]RelayPart, len(snapshot.Parts)),
	}
	for _, part := range snapshot.Parts {
		if _, ok := machine.parts[part.ID]; ok {
			return nil, nil, fmt.Errorf(
				"%w: duplicate part", ErrInvalidRelaySnapshot,
			)
		}
		machine.parts[part.ID] = part
	}

	if err := machine.validateLocked(); err != nil {
		return nil, nil, err
	}

	var actions []Action
	switch machine.state.State {
	case RelayPaying:
		if machine.state.ObservedHeight >= machine.state.HoldDeadline {
			machine.state.State = RelayCanceling
			machine.state.RecoveryDeadline = 0
			actions = []Action{
				{
					Kind: ActionCancelDownstream,
					AttemptID: machine.state.
						ActiveAttempt,
					AmountMsat: machine.state.
						DownstreamMsat,
				},
				{
					Kind: ActionQueryDownstream,
					AttemptID: machine.state.
						ActiveAttempt,
					AmountMsat: machine.state.
						DownstreamMsat,
				},
			}

			break
		}
		if machine.state.ObservedHeight >
			math.MaxUint32-config.RecoveryBlocks {

			return nil, nil, fmt.Errorf(
				"%w: recovery deadline overflow",
				ErrInvalidRelaySnapshot,
			)
		}

		machine.state.State = RelayRecovering
		recoveryDeadline := machine.state.ObservedHeight +
			config.RecoveryBlocks
		if recoveryDeadline > machine.state.HoldDeadline {
			recoveryDeadline = machine.state.HoldDeadline
		}
		machine.state.RecoveryDeadline = recoveryDeadline
		actions = []Action{{
			Kind:       ActionQueryDownstream,
			AttemptID:  machine.state.ActiveAttempt,
			AmountMsat: machine.state.DownstreamMsat,
		}}

	case RelayRecovering:
		actions = []Action{{
			Kind:       ActionQueryDownstream,
			AttemptID:  machine.state.ActiveAttempt,
			AmountMsat: machine.state.DownstreamMsat,
		}}

	case RelayCanceling:
		actions = []Action{
			{
				Kind:       ActionCancelDownstream,
				AttemptID:  machine.state.ActiveAttempt,
				AmountMsat: machine.state.DownstreamMsat,
			},
			{
				Kind:       ActionQueryDownstream,
				AttemptID:  machine.state.ActiveAttempt,
				AmountMsat: machine.state.DownstreamMsat,
			},
		}
	}

	return machine, actions, nil
}

func (m *RelayMachine) cancelDownstreamLocked() []Action {
	m.state.State = RelayCanceling
	m.state.RecoveryDeadline = 0

	return []Action{{
		Kind:       ActionCancelDownstream,
		AttemptID:  m.state.ActiveAttempt,
		AmountMsat: m.state.DownstreamMsat,
	}}
}

func (m *RelayMachine) failAllLocked(state RelayState) []Action {
	return m.resolveAllLocked(state, PartFailed)
}

func (m *RelayMachine) resolveAllLocked(state RelayState,
	resolution PartResolution) []Action {

	m.state.State = state
	m.state.ActiveAttempt = 0
	m.state.RecoveryDeadline = 0

	partIDs := make([]PartID, 0, len(m.parts))
	for id := range m.parts {
		partIDs = append(partIDs, id)
	}
	sort.Slice(partIDs, func(i, j int) bool {
		if partIDs[i].ChannelID == partIDs[j].ChannelID {
			return partIDs[i].HTLCID < partIDs[j].HTLCID
		}

		return partIDs[i].ChannelID < partIDs[j].ChannelID
	})

	actions := make([]Action, 0, len(partIDs))
	for _, id := range partIDs {
		part := m.parts[id]
		if part.Resolution != PartUnresolved {
			continue
		}

		part.Resolution = resolution
		m.parts[id] = part

		kind := ActionFailPart
		if resolution == PartFulfilled {
			kind = ActionFulfillPart
		}
		actions = append(actions, Action{
			Kind:       kind,
			PartID:     id,
			AmountMsat: part.AmountMsat,
		})
	}

	return actions
}

func (m *RelayMachine) snapshotLocked() RelaySnapshot {
	snapshot := m.state
	snapshot.Parts = make([]RelayPart, 0, len(m.parts))
	for _, part := range m.parts {
		snapshot.Parts = append(snapshot.Parts, part)
	}
	sort.Slice(snapshot.Parts, func(i, j int) bool {
		if snapshot.Parts[i].ID.ChannelID ==
			snapshot.Parts[j].ID.ChannelID {

			return snapshot.Parts[i].ID.HTLCID <
				snapshot.Parts[j].ID.HTLCID
		}

		return snapshot.Parts[i].ID.ChannelID <
			snapshot.Parts[j].ID.ChannelID
	})

	return snapshot
}

func (m *RelayMachine) validateLocked() error {
	s := &m.state
	if s.Version != relaySnapshotVersion {
		return fmt.Errorf("%w: version=%d",
			ErrInvalidRelaySnapshot, s.Version)
	}
	if s.ExpectedTotalMsat == 0 || s.DownstreamMsat == 0 ||
		s.DownstreamMsat > s.ExpectedTotalMsat {

		return fmt.Errorf("%w: amount domain",
			ErrInvalidRelaySnapshot)
	}
	if s.StartHeight > math.MaxUint32-m.config.HoldBlocks ||
		s.HoldDeadline <= s.StartHeight ||
		s.HoldDeadline > s.StartHeight+m.config.HoldBlocks {

		return fmt.Errorf("%w: hold deadline",
			ErrInvalidRelaySnapshot)
	}
	if s.ObservedHeight > s.PeakHeight ||
		s.Attempts > m.config.MaxAttempts ||
		len(m.parts) > int(m.config.MaxParts) {

		return fmt.Errorf("%w: resource bound",
			ErrInvalidRelaySnapshot)
	}

	var collected uint64
	for _, part := range m.parts {
		if part.AmountMsat == 0 || part.CLTV < s.RequiredCLTV ||
			part.CLTV <= m.config.ExpirySafetyBlocks ||
			s.HoldDeadline >
				part.CLTV-m.config.ExpirySafetyBlocks {

			return fmt.Errorf("%w: invalid part",
				ErrInvalidRelaySnapshot)
		}
		if collected > math.MaxUint64-part.AmountMsat {
			return fmt.Errorf("%w: part amount overflow",
				ErrInvalidRelaySnapshot)
		}
		collected += part.AmountMsat

		if s.State.IsTerminal() {
			if s.State == RelaySucceeded &&
				part.Resolution != PartFulfilled {

				return fmt.Errorf(
					"%w: unresolved success",
					ErrInvalidRelaySnapshot,
				)
			}
			if s.State != RelaySucceeded &&
				part.Resolution != PartFailed {

				return fmt.Errorf(
					"%w: unresolved failure",
					ErrInvalidRelaySnapshot,
				)
			}
		} else if part.Resolution != PartUnresolved {
			return fmt.Errorf("%w: early resolution",
				ErrInvalidRelaySnapshot)
		}
	}
	if collected != s.CollectedMsat ||
		collected > s.ExpectedTotalMsat {

		return fmt.Errorf("%w: collected total",
			ErrInvalidRelaySnapshot)
	}

	switch s.State {
	case RelayCollecting:
		if collected >= s.ExpectedTotalMsat ||
			s.ActiveAttempt != 0 {

			return fmt.Errorf("%w: collecting state",
				ErrInvalidRelaySnapshot)
		}

	case RelayReady:
		if collected != s.ExpectedTotalMsat ||
			s.ActiveAttempt != 0 {

			return fmt.Errorf("%w: ready state",
				ErrInvalidRelaySnapshot)
		}

	case RelayPaying, RelayRecovering, RelayCanceling:
		if collected != s.ExpectedTotalMsat ||
			s.ActiveAttempt == 0 ||
			s.ActiveAttempt != s.Attempts {

			return fmt.Errorf("%w: active attempt",
				ErrInvalidRelaySnapshot)
		}

	case RelaySucceeded, RelayFailed, RelayCanceled, RelayTimedOut:
		if s.ActiveAttempt != 0 || s.RecoveryDeadline != 0 {
			return fmt.Errorf("%w: terminal attempt",
				ErrInvalidRelaySnapshot)
		}

	default:
		return fmt.Errorf("%w: state=%d",
			ErrInvalidRelaySnapshot, s.State)
	}

	if s.State == RelayRecovering {
		if s.RecoveryDeadline <= s.ObservedHeight {
			return fmt.Errorf("%w: recovery deadline",
				ErrInvalidRelaySnapshot)
		}
	} else if s.RecoveryDeadline != 0 {
		return fmt.Errorf("%w: unexpected recovery deadline",
			ErrInvalidRelaySnapshot)
	}

	return nil
}

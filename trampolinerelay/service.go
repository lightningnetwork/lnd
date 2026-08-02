package trampolinerelay

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	switchhop "github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrServiceStopped is returned after the relay service has stopped.
	ErrServiceStopped = errors.New("trampoline relay service stopped")

	// ErrGroupLimit is returned when a new relay group would exceed the
	// configured active-group resource bound.
	ErrGroupLimit = errors.New("trampoline relay group limit exceeded")

	errRelayDeadline     = errors.New("trampoline relay deadline reached")
	errBlindedDownstream = errors.New(
		"blinded trampoline downstream failure",
	)
)

// OnionProcessor peels an embedded trampoline onion with Sphinx replay
// protection.
type OnionProcessor interface {
	ProcessTrampolineOnion(*sphinx.OnionPacket, []byte,
		uint32, *btcec.PublicKey) (*sphinx.ProcessedPacket, error)
}

// BlindingProcessor decrypts recipient data and derives the next path key.
type BlindingProcessor interface {
	DecryptBlindedHopData(*btcec.PublicKey, []byte) ([]byte, error)
	NextEphemeral(*btcec.PublicKey) (*btcec.PublicKey, error)
}

// PaymentSender executes the downstream payment selected by a peeled
// trampoline hop.
type PaymentSender interface {
	SendPayment(context.Context, *routing.LightningPayment) (
		[32]byte, *route.Route, error)
}

// InvoiceRegistry handles a final trampoline recipient using the fields
// authenticated by the inner onion.
type InvoiceRegistry interface {
	NotifyExitHopHtlc(lntypes.Hash, lnwire.MilliSatoshi, uint32, int32,
		models.CircuitKey, chan<- interface{}, lnwire.CustomRecords,
		invoices.Payload) (invoices.HtlcResolution, error)

	HodlUnsubscribeAll(chan<- interface{})
}

// Config contains the bounded policy and dependencies of the relay service.
type Config struct {
	OnionProcessor    OnionProcessor
	BlindingProcessor BlindingProcessor
	PaymentSender     PaymentSender
	InvoiceRegistry   InvoiceRegistry

	// PaymentReconciler waits for the durable result of a downstream
	// payment that was active when the relay restarted.
	PaymentReconciler func(context.Context, lntypes.Hash) (
		[32]byte, error)

	// PaymentCanceler prevents new downstream attempts while preserving
	// reconciliation of an already in-flight attempt to its terminal
	// result.
	PaymentCanceler func(context.Context, lntypes.Hash) error

	// ResolveIntroductionNode resolves either introduction-node wire
	// variant to the public key used by pathfinding.
	ResolveIntroductionNode func(lnwire.IntroductionNode) (
		*btcec.PublicKey, error)

	// ResolveNextNode resolves a short channel ID from encrypted recipient
	// data to the other endpoint of the local node's channel.
	ResolveNextNode func(lnwire.ShortChannelID) (*btcec.PublicKey, error)

	RelayConfig trampoline.RelayConfig
	PayTimeout  time.Duration
	MaxParts    uint32
	MaxGroups   uint32

	FeeBaseMsat               uint32
	FeeProportionalMillionths uint32
	CLTVExpiryDelta           uint16
	RandomReader              io.Reader
	Store                     Store
}

// Service aggregates incoming outer MPP parts and executes each peeled
// trampoline instruction exactly once.
type Service struct {
	cfg Config

	mu            sync.Mutex
	groups        map[trampoline.RelayID]*relayGroup
	activeGroups  uint32
	durableGroups uint32
	terminalIDs   []terminalRecord
	terminalSet   map[trampoline.RelayID]struct{}
	quit          chan struct{}
	workers       sync.WaitGroup
	start         sync.Once
	stop          sync.Once
}

var _ chainio.Consumer = (*Service)(nil)

type relayGroup struct {
	onionBytes        []byte
	payloadBytes      []byte
	payload           *trampoline.Payload
	parsed            tlv.TypeMap
	role              trampoline.HopRole
	nextOnion         []byte
	nextPathKey       *btcec.PublicKey
	nextNode          *btcec.PublicKey
	pathID            *chainhash.Hash
	outerPathKey      *btcec.PublicKey
	outerMPP          bool
	requiredOuterCLTV uint32
	pruneAfterHeight  uint32
	machine           *trampoline.RelayMachine
	preimage          *[32]byte
	failure           lnwire.FailCode
	failureReason     lnwire.OpaqueReason
	recovering        bool
	dispatcher        DispatchGuard
	reconciling       bool
	cancelRunning     bool
	cancelConfirmed   bool
	counted           bool
	attemptCancel     context.CancelFunc
	reconcileCancel   context.CancelFunc
	cancelCancel      context.CancelFunc
	cancelAttempt     uint16

	subscribers map[trampoline.PartID]chan<- interface{}
	heights     map[trampoline.PartID]int32
}

type terminalRecord struct {
	id               trampoline.RelayID
	pruneAfterHeight uint32
}

// NewService creates a trampoline relay service.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.OnionProcessor == nil:
		return nil, errors.New(
			"trampoline onion processor is required",
		)

	case cfg.BlindingProcessor == nil:
		return nil, errors.New(
			"trampoline blinding processor is required",
		)

	case cfg.PaymentSender == nil:
		return nil, errors.New("trampoline payment sender is required")

	case cfg.InvoiceRegistry == nil:
		return nil, errors.New(
			"trampoline invoice registry is required",
		)

	case cfg.PaymentReconciler == nil:
		return nil, errors.New(
			"trampoline payment reconciler is required",
		)

	case cfg.PaymentCanceler == nil:
		return nil, errors.New(
			"trampoline payment canceler is required",
		)
	}

	if err := cfg.RelayConfig.Validate(); err != nil {
		return nil, err
	}
	if cfg.PayTimeout <= 0 {
		return nil, errors.New(
			"trampoline payment timeout must be positive",
		)
	}
	if cfg.MaxParts == 0 {
		return nil, errors.New(
			"trampoline downstream max parts is zero",
		)
	}
	if cfg.MaxGroups == 0 {
		return nil, errors.New("trampoline relay max groups is zero")
	}
	if cfg.RandomReader == nil {
		cfg.RandomReader = rand.Reader
	}
	if cfg.Store == nil {
		cfg.Store = NewMemoryStore()
	}

	records, err := cfg.Store.List()
	if err != nil {
		return nil, fmt.Errorf("list trampoline relay groups: %w", err)
	}
	service := &Service{
		cfg:           cfg,
		groups:        make(map[trampoline.RelayID]*relayGroup),
		durableGroups: uint32(len(records)),
		terminalIDs:   make([]terminalRecord, 0, len(records)),
		terminalSet: make(
			map[trampoline.RelayID]struct{}, len(records),
		),
		quit: make(chan struct{}),
	}
	for _, record := range records {
		if storedGroupTerminal(record.Group) {
			service.trackTerminalLocked(
				record.ID, record.Group.PruneAfterHeight,
			)

			continue
		}

		group, err := service.restoreGroup(record.Group)
		if err != nil {
			return nil, fmt.Errorf(
				"restore trampoline relay group %x: %w",
				record.ID.PaymentHash, err,
			)
		}
		group.counted = true
		service.groups[record.ID] = group
		service.activeGroups++
	}

	return service, nil
}

// Start resumes every durable relay group without waiting for an upstream
// channel link to replay its incoming HTLCs.
func (s *Service) Start() {
	s.start.Do(func() {
		if s.stopped() {
			return
		}

		var ready, recovering, canceling []trampoline.RelayID
		s.mu.Lock()
		for id, group := range s.groups {
			if group.machine == nil {
				continue
			}

			snapshot := group.machine.Snapshot()
			if group.dispatcher.ScheduleReady(
				snapshot.State == trampoline.RelayReady,
			) {

				ready = append(ready, id)
			}
			if snapshot.State == trampoline.RelayCanceling &&
				!group.cancelRunning && !group.cancelConfirmed {

				group.cancelRunning = true
				canceling = append(canceling, id)

				// Restored cancellation must run before result
				// lookup. Its worker then reconciles.
				continue
			}
			if group.recovering && !group.reconciling {
				group.recovering = false
				group.reconciling = true
				recovering = append(recovering, id)
			}
		}
		s.mu.Unlock()

		for _, id := range canceling {
			s.launchCancelDownstream(id)
		}
		for _, id := range recovering {
			s.launchReconcileDownstream(id)
		}
		for _, id := range ready {
			s.launchRunDownstream(id)
		}
	})
}

// RequestStop prevents new work and cancels all active downstream operations.
// It does not wait for workers because their payment dependencies may need to
// drain first.
func (s *Service) RequestStop() {
	s.stop.Do(func() {
		close(s.quit)

		s.mu.Lock()
		for _, group := range s.groups {
			if group.attemptCancel != nil {
				group.attemptCancel()
			}
			if group.reconcileCancel != nil {
				group.reconcileCancel()
			}
			if group.cancelCancel != nil {
				group.cancelCancel()
			}
		}
		s.mu.Unlock()
	})
}

// WaitForShutdown waits until every worker admitted before RequestStop has
// returned. The payment switch and router must be drained before this method
// if an in-flight SendPayment does not return on context cancellation alone.
func (s *Service) WaitForShutdown() {
	s.workers.Wait()
}

// Stop prevents new work, cancels active downstream operations, and waits for
// every relay worker to exit.
func (s *Service) Stop() {
	s.RequestStop()
	s.WaitForShutdown()
}

// Name returns the blockbeat consumer name.
func (s *Service) Name() string {
	return "trampoline-relay"
}

// ProcessBlock advances every live relay group to the blockbeat height.
func (s *Service) ProcessBlock(beat chainio.Blockbeat) error {
	height := beat.Height()
	if height < 0 {
		return errors.New("negative trampoline block height")
	}

	return s.ObserveHeight(uint32(height))
}

// ObserveHeight enforces live hold and recovery deadlines for every relay
// group. A reorg beyond the configured bound fails the affected group without
// stopping the daemon's block dispatcher.
func (s *Service) ObserveHeight(height uint32) error {
	s.mu.Lock()
	if err := s.pruneTerminalLocked(height); err != nil {
		s.mu.Unlock()

		return err
	}

	var (
		resolutions []queuedResolution
		cancelIDs   []trampoline.RelayID
	)
	for id, group := range s.groups {
		if group.machine == nil ||
			group.machine.Snapshot().State.IsTerminal() {

			continue
		}

		actions, err := group.machine.ObserveHeight(height)
		if errors.Is(err, trampoline.ErrReorgTooDeep) {
			actions, err = group.machine.Cancel()
		}
		if err != nil {
			s.mu.Unlock()
			return err
		}

		if hasAction(actions, trampoline.ActionFailPart) {
			group.failure = lnwire.CodeTemporaryTrampolineFailure
			resolutions = append(
				resolutions,
				s.resolutionsLocked(
					group, actions, [32]byte{},
					errRelayDeadline,
				)...,
			)
		}
		if group.machine.Snapshot().State ==
			trampoline.RelayCanceling &&
			!group.cancelRunning && !group.cancelConfirmed {

			group.cancelRunning = true
			cancelIDs = append(cancelIDs, id)
		}

		if err := s.persistGroup(id, group); err != nil {
			s.mu.Unlock()
			return err
		}
		if group.machine.Snapshot().State.IsTerminal() {
			s.evictGroupLocked(id, group)
		}
	}
	s.mu.Unlock()

	s.deliver(resolutions)
	for _, id := range cancelIDs {
		s.launchCancelDownstream(id)
	}

	return nil
}

// NotifyTrampolineHtlc validates and accepts an incoming outer HTLC.
func (s *Service) NotifyTrampolineHtlc(
	htlc htlcswitch.TrampolineHtlc) (invoices.HtlcResolution, error) {

	if htlc.Onion == nil {
		return s.failure(
			htlc, &lnwire.FailTemporaryTrampolineFailure{},
		), nil
	}
	if s.stopped() {
		return nil, ErrServiceStopped
	}

	id := trampoline.RelayID{PaymentHash: htlc.PaymentHash}
	if htlc.MPP != nil {
		id.PaymentAddr = htlc.MPP.PaymentAddr()
	}
	partID := trampoline.PartID{
		ChannelID: htlc.CircuitKey.ChanID.ToUint64(),
		HTLCID:    htlc.CircuitKey.HtlcID,
	}

	onionBytes, err := trampoline.EncodeOnionPacket(htlc.Onion)
	if err != nil {
		return s.failure(
			htlc, &lnwire.FailTemporaryTrampolineFailure{},
		), nil
	}

	s.mu.Lock()
	group, inMemory, loaded, err := s.loadRelayGroupLocked(
		htlc, id, onionBytes,
	)
	if err != nil {
		log.Debugf("Rejecting trampoline HTLC %v for group %x: %v",
			partID, id.PaymentHash, err)
		s.mu.Unlock()

		return s.failureForError(htlc, err), nil
	}

	resolution, terminal, err := s.terminalResolutionLocked(htlc, id, group)
	if err != nil || terminal {
		s.mu.Unlock()

		return resolution, err
	}

	if isFinalRole(group.role) {
		payload := newInvoicePayload(group.payload)
		payload.pathID = group.pathID
		s.mu.Unlock()

		return s.cfg.InvoiceRegistry.NotifyExitHopHtlc(
			htlc.PaymentHash, htlc.Amount, htlc.Expiry,
			htlc.CurrentHeight, htlc.CircuitKey, htlc.HodlChan, nil,
			payload,
		)
	}
	if !inMemory && !loaded && s.durableGroups >= s.cfg.MaxGroups {
		s.mu.Unlock()

		return s.failureForError(htlc, ErrGroupLimit), nil
	}
	if err := s.validateOuterShard(htlc, group); err != nil {
		log.Debugf("Rejecting trampoline HTLC %v for group %x: %v",
			partID, id.PaymentHash, err)
		s.mu.Unlock()

		return s.failureForError(htlc, err), nil
	}

	if existing, exists := group.subscribers[partID]; exists {
		if existing != htlc.HodlChan {
			s.mu.Unlock()
			return s.failure(
				htlc, &lnwire.FailTemporaryTrampolineFailure{},
			), nil
		}
	} else {
		group.subscribers[partID] = htlc.HodlChan
		group.heights[partID] = htlc.CurrentHeight
	}

	actions, err := group.machine.AddPart(trampoline.RelayPart{
		ID:         partID,
		AmountMsat: uint64(htlc.Amount),
		CLTV:       htlc.Expiry,
	})
	if err != nil {
		delete(group.subscribers, partID)
		delete(group.heights, partID)
		log.Debugf("Rejecting trampoline HTLC %v for group %x: %v",
			partID, id.PaymentHash, err)
		s.mu.Unlock()

		return s.failureForError(htlc, err), nil
	}
	if err := s.persistGroup(id, group); err != nil {
		delete(group.subscribers, partID)
		delete(group.heights, partID)
		s.mu.Unlock()
		if errors.Is(err, ErrPaymentHashConflict) {
			return s.failureForError(htlc, err), nil
		}

		return nil, err
	}
	if !inMemory {
		s.groups[id] = group
		group.counted = true
		if !loaded {
			s.activeGroups++
			s.durableGroups++
		}
	}

	snapshot := group.machine.Snapshot()
	ready := group.dispatcher.ScheduleReady(
		hasAction(actions, trampoline.ActionPaymentReady) ||
			snapshot.State == trampoline.RelayReady,
	)
	recovering := group.recovering && !group.reconciling
	canceling := snapshot.State == trampoline.RelayCanceling &&
		!group.cancelRunning && !group.cancelConfirmed
	if canceling {
		group.cancelRunning = true
		recovering = false
	} else if recovering {
		group.recovering = false
		group.reconciling = true
	}
	s.mu.Unlock()
	if canceling {
		s.launchCancelDownstream(id)
	}
	if recovering {
		s.launchReconcileDownstream(id)
	} else if ready {
		s.launchRunDownstream(id)
	}

	return nil, nil
}

// loadRelayGroupLocked loads or constructs a relay group and authenticates a
// byte-distinct onion against the first accepted instruction.
func (s *Service) loadRelayGroupLocked(htlc htlcswitch.TrampolineHtlc,
	id trampoline.RelayID, onionBytes []byte) (*relayGroup, bool, bool,
	error) {

	group, inMemory := s.groups[id]
	if inMemory {
		if bytes.Equal(group.onionBytes, onionBytes) {
			return group, true, false, nil
		}

		err := s.validateCompatibleOnion(
			htlc, id, onionBytes, group,
		)

		return group, true, false, err
	}

	stored, err := s.cfg.Store.Load(id)
	if err != nil {
		return nil, false, false, err
	}

	loaded := stored != nil
	if loaded {
		group, err = s.restoreGroup(*stored)
	} else {
		group, err = s.newGroup(htlc, id, onionBytes)
	}
	if err != nil || group.preimage != nil || group.failure != 0 ||
		bytes.Equal(group.onionBytes, onionBytes) {

		return group, false, loaded, err
	}

	err = s.validateCompatibleOnion(htlc, id, onionBytes, group)

	return group, false, loaded, err
}

func (s *Service) stopped() bool {
	select {
	case <-s.quit:
		return true

	default:
		return false
	}
}

// validateCompatibleOnion independently authenticates a distinct inner onion
// for an existing MPP group. Senders may use a fresh inner Sphinx session key
// for every outer shard, so serialized onion equality is not a valid group
// invariant. The first authenticated onion supplies the downstream remainder;
// later shards must commit to the same first-hop instruction and derived relay
// state before they can contribute value to that instruction.
func (s *Service) validateCompatibleOnion(htlc htlcswitch.TrampolineHtlc,
	id trampoline.RelayID, onionBytes []byte, group *relayGroup) error {

	candidate, err := s.newGroup(htlc, id, onionBytes)
	if err != nil {
		return err
	}
	if candidate.role != group.role ||
		!bytes.Equal(candidate.payloadBytes, group.payloadBytes) ||
		!equalPubKeys(candidate.nextPathKey, group.nextPathKey) ||
		!equalPubKeys(candidate.nextNode, group.nextNode) ||
		!equalHashes(candidate.pathID, group.pathID) ||
		candidate.outerMPP != group.outerMPP ||
		!equalPubKeys(candidate.outerPathKey, group.outerPathKey) {

		return errors.New("conflicting trampoline MPP instruction")
	}

	if candidate.machine == nil || group.machine == nil {
		if candidate.machine != group.machine {
			return errors.New("conflicting trampoline MPP role")
		}

		return nil
	}

	candidateState := candidate.machine.Snapshot()
	groupState := group.machine.Snapshot()
	if candidateState.ExpectedTotalMsat != groupState.ExpectedTotalMsat ||
		candidateState.DownstreamMsat != groupState.DownstreamMsat ||
		candidateState.RequiredCLTV != groupState.RequiredCLTV {

		return errors.New("conflicting trampoline MPP budget")
	}

	return nil
}

// terminalResolutionLocked extends a terminal replay's durable safety horizon
// before returning its stored resolution. The caller must hold s.mu.
func (s *Service) terminalResolutionLocked(htlc htlcswitch.TrampolineHtlc,
	id trampoline.RelayID,
	group *relayGroup) (invoices.HtlcResolution, bool, error) {

	if group.preimage == nil && group.failure == 0 {
		return nil, false, nil
	}

	pruneAfterHeight := terminalPruneHeight(
		htlc.Expiry, s.cfg.RelayConfig.MaxReorgDepth,
	)
	if group.pruneAfterHeight != 0 &&
		pruneAfterHeight > group.pruneAfterHeight {

		err := s.persistGroupWithPruneHorizon(
			id, group, pruneAfterHeight,
		)
		if err != nil {
			return nil, true, err
		}
	}

	if group.preimage != nil {
		preimage := lntypes.Preimage(*group.preimage)

		return invoices.NewSettleResolution(
			preimage, htlc.CircuitKey, htlc.CurrentHeight,
			invoices.ResultReplayToSettled,
		), true, nil
	}
	if len(group.failureReason) != 0 {
		reason := append(
			lnwire.OpaqueReason(nil), group.failureReason...,
		)

		return invoices.NewNestedErrorResolution(
			htlc.CircuitKey, htlc.CurrentHeight, reason,
		), true, nil
	}

	return s.failure(
		htlc, failureForCode(group.failure),
	), true, nil
}

func (s *Service) validateOuterShard(htlc htlcswitch.TrampolineHtlc,
	group *relayGroup) error {

	if (htlc.MPP != nil) != group.outerMPP ||
		!equalPubKeys(htlc.CurrentPathKey, group.outerPathKey) {

		return errors.New("conflicting trampoline outer payment data")
	}

	snapshot := group.machine.Snapshot()
	total := uint64(htlc.Amount)
	var outerTotal *uint64
	if htlc.MPP != nil {
		total = uint64(htlc.MPP.TotalMsat())
		outerTotal = &total
	}
	if total != snapshot.ExpectedTotalMsat {
		return errors.New("conflicting trampoline outer total")
	}

	outer := trampoline.OuterPayment{
		AmountMsat:      uint64(htlc.Amount),
		CLTV:            htlc.Expiry,
		TotalAmountMsat: outerTotal,
	}
	if group.role == trampoline.RoleClearIntermediate ||
		group.role == trampoline.RoleRecipientPaths {

		return trampoline.ValidateRelayBudget(
			outer, group.payload, s.cfg.FeeBaseMsat,
			s.cfg.FeeProportionalMillionths,
			s.cfg.CLTVExpiryDelta,
		)
	}
	if isBlindedRole(group.role) &&
		htlc.Expiry < group.requiredOuterCLTV {

		return trampoline.ErrOuterCLTVInsufficient
	}

	return nil
}

func equalPubKeys(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return a == b
	}

	return bytes.Equal(a.SerializeCompressed(), b.SerializeCompressed())
}

func equalHashes(a, b *chainhash.Hash) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}

func (s *Service) newGroup(htlc htlcswitch.TrampolineHtlc,
	id trampoline.RelayID, onionBytes []byte) (*relayGroup, error) {

	processed, err := s.cfg.OnionProcessor.ProcessTrampolineOnion(
		htlc.Onion, htlc.PaymentHash[:], htlc.Expiry,
		htlc.CurrentPathKey,
	)
	if err != nil {
		return nil, err
	}

	payload, parsed, err := trampoline.DecodePayload(
		bytes.NewReader(processed.Payload.Payload),
	)
	if err != nil {
		return nil, err
	}

	role, err := selectRole(
		processed, payload, htlc.CurrentPathKey,
	)
	if err != nil {
		return nil, err
	}
	if err := trampoline.ValidatePayload(parsed, role); err != nil {
		return nil, err
	}

	total := uint64(htlc.Amount)
	var outerTotal *uint64
	if htlc.MPP != nil {
		total = uint64(htlc.MPP.TotalMsat())
		outerTotal = &total
	}
	outer := trampoline.OuterPayment{
		AmountMsat:      uint64(htlc.Amount),
		CLTV:            htlc.Expiry,
		TotalAmountMsat: outerTotal,
	}
	requiredOuterCLTV := htlc.Expiry
	if role == trampoline.RoleClearIntermediate ||
		role == trampoline.RoleRecipientPaths {

		err = trampoline.ValidateRelayBudget(
			outer, payload, s.cfg.FeeBaseMsat,
			s.cfg.FeeProportionalMillionths,
			s.cfg.CLTVExpiryDelta,
		)
	} else if isFinalRole(role) {
		err = trampoline.ValidateFinalOuter(outer, payload)
	}
	if err != nil {
		return nil, err
	}

	var blinded *blindedHop
	if isBlindedRole(role) {
		blinded, err = s.processBlindedHop(
			payload, role, outer, htlc.CurrentPathKey,
		)
		if err != nil {
			return nil, err
		}
	}

	group := &relayGroup{
		onionBytes: append([]byte(nil), onionBytes...),
		payloadBytes: append(
			[]byte(nil), processed.Payload.Payload...,
		),
		payload:           payload,
		parsed:            parsed,
		role:              role,
		outerPathKey:      htlc.CurrentPathKey,
		outerMPP:          htlc.MPP != nil,
		requiredOuterCLTV: requiredOuterCLTV,
		subscribers: make(
			map[trampoline.PartID]chan<- interface{},
		),
		heights: make(map[trampoline.PartID]int32),
	}
	if blinded != nil {
		group.nextPathKey = blinded.nextPathKey
		group.nextNode = blinded.nextNode
		group.pathID = blinded.pathID
	}
	if role == trampoline.RoleClearIntermediate ||
		role == trampoline.RoleRecipientPaths {

		_, requiredOuterCLTV, err = trampoline.RequiredRelayBudget(
			payload, s.cfg.FeeBaseMsat,
			s.cfg.FeeProportionalMillionths,
			s.cfg.CLTVExpiryDelta,
		)
		if err != nil {
			return nil, err
		}
		group.requiredOuterCLTV = requiredOuterCLTV
	}
	if isFinalRole(role) {
		return group, nil
	}

	if role != trampoline.RoleRecipientPaths {
		if processed.NextPacket == nil {
			return nil, errors.New(
				"missing next trampoline onion packet",
			)
		}
		group.nextOnion, err = trampoline.EncodeOnionPacket(
			processed.NextPacket,
		)
		if err != nil {
			return nil, err
		}
	}
	group.machine, err = trampoline.NewRelayMachine(
		s.cfg.RelayConfig, id, total, *payload.AmountToForward,
		*payload.OutgoingCLTV, uint32(htlc.CurrentHeight),
	)
	if err != nil {
		return nil, err
	}

	return group, nil
}

func (s *Service) restoreGroup(stored StoredGroup) (*relayGroup, error) {
	group := &relayGroup{
		preimage:         stored.Preimage,
		failure:          lnwire.FailCode(stored.FailureCode),
		pruneAfterHeight: stored.PruneAfterHeight,
		failureReason: append(
			lnwire.OpaqueReason(nil), stored.FailureReason...,
		),
		subscribers: make(
			map[trampoline.PartID]chan<- interface{},
		),
		heights: make(map[trampoline.PartID]int32),
	}
	if group.preimage != nil || group.failure != 0 {
		return group, nil
	}

	payload, parsed, err := trampoline.DecodePayload(
		bytes.NewReader(stored.PayloadBytes),
	)
	if err != nil {
		return nil, err
	}
	if err := trampoline.ValidatePayload(parsed, stored.Role); err != nil {
		return nil, err
	}

	group.onionBytes = append([]byte(nil), stored.OnionBytes...)
	group.payloadBytes = append([]byte(nil), stored.PayloadBytes...)
	group.payload = payload
	group.parsed = parsed
	group.role = stored.Role
	group.nextOnion = append([]byte(nil), stored.NextOnion...)
	group.outerMPP = stored.OuterMPP
	group.requiredOuterCLTV = stored.CLTV
	if len(stored.OuterPathKey) != 0 {
		group.outerPathKey, err = btcec.ParsePubKey(stored.OuterPathKey)
		if err != nil {
			return nil, fmt.Errorf(
				"decode stored outer path key: %w", err,
			)
		}
	}
	if len(stored.NextPathKey) != 0 {
		group.nextPathKey, err = btcec.ParsePubKey(stored.NextPathKey)
		if err != nil {
			return nil, fmt.Errorf(
				"decode stored trampoline path key: %w", err,
			)
		}
	}
	if len(stored.NextNode) != 0 {
		group.nextNode, err = btcec.ParsePubKey(stored.NextNode)
		if err != nil {
			return nil, fmt.Errorf(
				"decode stored trampoline next node: %w", err,
			)
		}
	}
	if len(stored.PathID) != 0 {
		if len(stored.PathID) != chainhash.HashSize {
			return nil, errors.New(
				"invalid stored trampoline path ID",
			)
		}
		var pathID chainhash.Hash
		copy(pathID[:], stored.PathID)
		group.pathID = &pathID
	}
	if isFinalRole(stored.Role) {
		return group, nil
	}

	var actions []trampoline.Action
	group.machine, actions, err = trampoline.RestoreRelayMachine(
		s.cfg.RelayConfig, stored.Snapshot,
	)
	if err != nil {
		return nil, err
	}
	if isBlindedRole(stored.Role) {
		snapshot := group.machine.Snapshot()
		amount := snapshot.DownstreamMsat
		cltv := snapshot.RequiredCLTV
		group.payload.AmountToForward = &amount
		group.payload.OutgoingCLTV = &cltv
	}
	group.recovering = hasAction(
		actions, trampoline.ActionQueryDownstream,
	)

	return group, nil
}

func (s *Service) persistGroup(id trampoline.RelayID,
	group *relayGroup) error {

	return s.persistGroupWithPruneHorizon(id, group, 0)
}

func (s *Service) persistGroupWithPruneHorizon(id trampoline.RelayID,
	group *relayGroup, minimumPruneHeight uint32) error {

	if group.preimage != nil || group.failure != 0 {
		snapshot := trampoline.RelaySnapshot{ID: id}
		pruneAfterHeight := terminalPruneHeight(
			group.requiredOuterCLTV,
			s.cfg.RelayConfig.MaxReorgDepth,
		)
		if group.machine != nil {
			snapshot = group.machine.Snapshot()
			for _, part := range snapshot.Parts {
				partPruneHeight := terminalPruneHeight(
					part.CLTV,
					s.cfg.RelayConfig.MaxReorgDepth,
				)
				if partPruneHeight > pruneAfterHeight {
					pruneAfterHeight = partPruneHeight
				}
			}
			snapshot.Parts = nil
		}
		if group.pruneAfterHeight > pruneAfterHeight {
			pruneAfterHeight = group.pruneAfterHeight
		}
		if minimumPruneHeight > pruneAfterHeight {
			pruneAfterHeight = minimumPruneHeight
		}

		err := s.cfg.Store.Put(id, StoredGroup{
			Preimage:    group.preimage,
			FailureCode: uint16(group.failure),
			Snapshot:    snapshot,
			FailureReason: append(
				[]byte(nil), group.failureReason...,
			),
			PruneAfterHeight: pruneAfterHeight,
		})
		if err != nil {
			return err
		}

		group.pruneAfterHeight = pruneAfterHeight
		s.trackTerminalLocked(id, pruneAfterHeight)

		return nil
	}

	stored := StoredGroup{
		OnionBytes:   append([]byte(nil), group.onionBytes...),
		PayloadBytes: append([]byte(nil), group.payloadBytes...),
		NextOnion:    append([]byte(nil), group.nextOnion...),
		Role:         group.role,
		OuterMPP:     group.outerMPP,
		CLTV:         group.requiredOuterCLTV,
		Preimage:     group.preimage,
		FailureCode:  uint16(group.failure),
		FailureReason: append(
			[]byte(nil), group.failureReason...,
		),
	}
	if group.nextPathKey != nil {
		stored.NextPathKey = group.nextPathKey.SerializeCompressed()
	}
	if group.nextNode != nil {
		stored.NextNode = group.nextNode.SerializeCompressed()
	}
	if group.pathID != nil {
		stored.PathID = append([]byte(nil), group.pathID[:]...)
	}
	if group.outerPathKey != nil {
		stored.OuterPathKey = group.outerPathKey.SerializeCompressed()
	}
	if group.machine != nil {
		stored.Snapshot = group.machine.Snapshot()
	}

	return s.cfg.Store.Put(id, stored)
}

func storedGroupTerminal(group StoredGroup) bool {
	return group.Preimage != nil || group.FailureCode != 0 ||
		group.Snapshot.State.IsTerminal()
}

func terminalPruneHeight(expiry, reorgMargin uint32) uint32 {
	if expiry == 0 {
		return 0
	}
	if expiry > math.MaxUint32-reorgMargin {
		return math.MaxUint32
	}

	return expiry + reorgMargin
}

func (s *Service) trackTerminalLocked(id trampoline.RelayID,
	pruneAfterHeight uint32) {

	if _, ok := s.terminalSet[id]; ok {
		for i := range s.terminalIDs {
			if s.terminalIDs[i].id != id {
				continue
			}
			if pruneAfterHeight >
				s.terminalIDs[i].pruneAfterHeight {

				s.terminalIDs[i].pruneAfterHeight =
					pruneAfterHeight
			}

			return
		}

		return
	}

	s.terminalSet[id] = struct{}{}
	s.terminalIDs = append(s.terminalIDs, terminalRecord{
		id:               id,
		pruneAfterHeight: pruneAfterHeight,
	})
}

func (s *Service) pruneTerminalLocked(height uint32) error {
	retained := s.terminalIDs[:0]
	for i, record := range s.terminalIDs {
		// A zero height denotes a legacy tombstone without enough
		// durable information to prove that pruning is loss-safe.
		if record.pruneAfterHeight == 0 ||
			height < record.pruneAfterHeight {

			retained = append(retained, record)

			continue
		}
		if err := s.cfg.Store.Delete(record.id); err != nil {
			s.terminalIDs = append(retained, s.terminalIDs[i:]...)

			return err
		}
		delete(s.terminalSet, record.id)
		if s.durableGroups != 0 {
			s.durableGroups--
		}
	}
	s.terminalIDs = retained

	return nil
}

func selectRole(processed *sphinx.ProcessedPacket,
	payload *trampoline.Payload,
	outerPathKey *btcec.PublicKey) (trampoline.HopRole, error) {

	blinded := payload.EncryptedRecipientData != nil
	if !blinded {
		if payload.CurrentPathKey != nil || outerPathKey != nil {
			return 0, errors.New(
				"path key without encrypted recipient data",
			)
		}
	} else {
		if _, err := trampoline.SelectPathKey(
			payload.CurrentPathKey, outerPathKey,
		); err != nil {
			return 0, err
		}
	}

	switch processed.Action {
	case sphinx.MoreHops:
		if processed.NextPacket == nil {
			return 0, errors.New("missing next trampoline hop")
		}
		if blinded {
			if payload.CurrentPathKey != nil {
				return trampoline.RoleBlindedIntroduction, nil
			}

			return trampoline.RoleBlindedIntermediate, nil
		}
		if payload.OutgoingNodeID == nil {
			return 0, errors.New("missing next trampoline node id")
		}

		return trampoline.RoleClearIntermediate, nil

	case sphinx.ExitNode:
		if payload.RecipientBlindedPaths != nil {
			return trampoline.RoleRecipientPaths, nil
		}
		if blinded {
			if payload.CurrentPathKey != nil {
				role := trampoline.RoleBlindedIntroductionFinal

				return role, nil
			}

			return trampoline.RoleBlindedFinal, nil
		}

		return trampoline.RoleBolt11Final, nil

	default:
		return 0, fmt.Errorf(
			"unknown trampoline sphinx action %d", processed.Action,
		)
	}
}

type blindedHop struct {
	nextPathKey *btcec.PublicKey
	nextNode    *btcec.PublicKey
	pathID      *chainhash.Hash
}

func (s *Service) processBlindedHop(payload *trampoline.Payload,
	role trampoline.HopRole, outer trampoline.OuterPayment,
	outerPathKey *btcec.PublicKey) (*blindedHop, error) {

	pathKey, err := trampoline.SelectPathKey(
		payload.CurrentPathKey, outerPathKey,
	)
	if err != nil {
		return nil, err
	}

	decrypted, err := s.cfg.BlindingProcessor.DecryptBlindedHopData(
		pathKey, payload.EncryptedRecipientData,
	)
	if err != nil {
		return nil, fmt.Errorf("decrypt blinded trampoline data: %w",
			err)
	}
	routeData, err := record.DecodeBlindedRouteData(
		bytes.NewReader(decrypted),
	)
	if err != nil {
		return nil, fmt.Errorf("decode blinded trampoline data: %w",
			err)
	}

	incoming := outer.AmountMsat
	if outer.TotalAmountMsat != nil {
		incoming = *outer.TotalAmountMsat
	}
	if err := switchhop.ValidateBlindedRouteData(
		routeData, lnwire.MilliSatoshi(incoming), outer.CLTV,
	); err != nil {
		return nil, err
	}

	if isFinalRole(role) {
		var pathID *chainhash.Hash
		routeData.PathID.WhenSome(
			func(id tlv.RecordT[tlv.TlvType6, []byte]) {
				var hash chainhash.Hash
				copy(hash[:], id.Val)
				pathID = &hash
			},
		)
		if pathID == nil {
			return nil, errors.New(
				"final blinded trampoline path ID missing",
			)
		}

		return &blindedHop{pathID: pathID}, nil
	}

	hasSCID := routeData.ShortChannelID.IsSome()
	hasNodeID := routeData.NextNodeID.IsSome()
	if hasSCID == hasNodeID {
		return nil, errors.New(
			"blinded trampoline hop must set exactly one next hop",
		)
	}
	relay, err := routeData.RelayInfo.UnwrapOrErr(
		errors.New("blinded trampoline relay info missing"),
	)
	if err != nil {
		return nil, err
	}
	forwardAmount, err := trampoline.CalculateForwardingAmount(
		incoming, uint32(relay.Val.BaseFee), relay.Val.FeeRate,
	)
	if err != nil {
		return nil, err
	}
	if outer.CLTV < uint32(relay.Val.CltvExpiryDelta) {
		return nil, trampoline.ErrOuterCLTVInsufficient
	}
	forwardCLTV := outer.CLTV - uint32(relay.Val.CltvExpiryDelta)
	payload.AmountToForward = &forwardAmount
	payload.OutgoingCLTV = &forwardCLTV

	var nextNode *btcec.PublicKey
	routeData.NextNodeID.WhenSome(
		func(node tlv.RecordT[tlv.TlvType4, *btcec.PublicKey]) {
			nextNode = node.Val
		},
	)
	if nextNode == nil {
		if s.cfg.ResolveNextNode == nil {
			return nil, errors.New(
				"blinded trampoline SCID resolver unavailable",
			)
		}
		scid := routeData.ShortChannelID.UnwrapOr(
			routeData.ShortChannelID.Zero(),
		)
		nextNode, err = s.cfg.ResolveNextNode(scid.Val)
		if err != nil {
			return nil, fmt.Errorf(
				"resolve blinded trampoline next node: %w", err,
			)
		}
	}

	nextPathKey, err := routeData.NextBlindingOverride.UnwrapOrFuncErr(
		func() (tlv.RecordT[tlv.TlvType8, *btcec.PublicKey],
			error) {

			next, err := s.cfg.BlindingProcessor.NextEphemeral(
				pathKey,
			)
			if err != nil {
				return routeData.NextBlindingOverride.Zero(),
					err
			}

			return tlv.NewPrimitiveRecord[tlv.TlvType8](next), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"derive next trampoline path key: %w", err,
		)
	}

	return &blindedHop{
		nextPathKey: nextPathKey.Val,
		nextNode:    nextNode,
	}, nil
}

func isBlindedRole(role trampoline.HopRole) bool {
	switch role {
	case trampoline.RoleBlindedIntroduction,
		trampoline.RoleBlindedIntermediate,
		trampoline.RoleBlindedFinal,
		trampoline.RoleBlindedIntroductionFinal:

		return true

	default:
		return false
	}
}

func isFinalRole(role trampoline.HopRole) bool {
	switch role {
	case trampoline.RoleBolt11Final,
		trampoline.RoleBlindedFinal,
		trampoline.RoleBlindedIntroductionFinal:

		return true

	default:
		return false
	}
}

// launchWorker serializes worker admission with Stop. Once Stop closes quit,
// no new worker can increment the wait group. Stop therefore waits for every
// admitted worker without racing a subsequent Add.
func (s *Service) launchWorker(worker func()) {
	s.mu.Lock()
	if s.stopped() {
		s.mu.Unlock()

		return
	}
	s.workers.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.workers.Done()

		worker()
	}()
}

func (s *Service) launchRunDownstream(id trampoline.RelayID) {
	s.launchWorker(func() {
		s.runDownstream(id)
	})
}

func (s *Service) launchReconcileDownstream(id trampoline.RelayID) {
	s.launchWorker(func() {
		s.reconcileDownstream(id)
	})
}

func (s *Service) launchCancelDownstream(id trampoline.RelayID) {
	s.launchWorker(func() {
		s.cancelDownstream(id)
	})
}

func (s *Service) runDownstream(id trampoline.RelayID) {
	s.mu.Lock()
	if s.stopped() {
		s.mu.Unlock()

		return
	}
	group, ok := s.groups[id]
	if !ok {
		s.mu.Unlock()
		return
	}

	actions, err := group.dispatcher.Begin(group.machine)
	if err != nil {
		s.mu.Unlock()
		log.Errorf("Unable to begin trampoline attempt %x: %v",
			id.PaymentHash, err)
		if errors.Is(err, trampoline.ErrRelayState) ||
			errors.Is(err, trampoline.ErrRelayTerminal) {

			return
		}
		s.resolveFailure(
			id, &lnwire.FailTemporaryTrampolineFailure{},
		)

		return
	}
	var attemptID uint16
	for _, action := range actions {
		if action.Kind == trampoline.ActionStartDownstream {
			attemptID = action.AttemptID
			break
		}
	}
	if attemptID == 0 {
		group.dispatcher.Finish(false)
		group.failure = lnwire.CodeTemporaryTrampolineFailure
		if err := s.persistGroup(id, group); err != nil {
			s.mu.Unlock()

			return
		}
		resolutions := s.resolutionsLocked(
			group, actions, [32]byte{}, errRelayDeadline,
		)
		s.evictGroupLocked(id, group)
		s.mu.Unlock()
		s.deliver(resolutions)

		return
	}
	if err := s.persistGroup(id, group); err != nil {
		group.dispatcher.Finish(false)
		s.mu.Unlock()
		if attemptID != 0 {
			s.completeAttempt(
				id, attemptID, [32]byte{}, err, false, false,
			)
		}

		return
	}
	payment, err := s.makePayment(group)
	if err != nil {
		s.mu.Unlock()
		s.completeAttempt(
			id, attemptID, [32]byte{}, err, false, false,
		)

		return
	}

	background := context.Background()
	ctx, cancel := context.WithTimeout(background, s.cfg.PayTimeout)
	group.attemptCancel = cancel
	group.cancelAttempt = attemptID
	s.mu.Unlock()
	defer cancel()

	preimage, _, err := s.cfg.PaymentSender.SendPayment(ctx, payment)
	s.completeAttempt(
		id, attemptID, preimage, err, retryablePaymentError(err), true,
	)
}

func (s *Service) reconcileDownstream(id trampoline.RelayID) {
	s.mu.Lock()
	if s.stopped() {
		s.mu.Unlock()

		return
	}
	group, ok := s.groups[id]
	if !ok || group.machine == nil {
		s.mu.Unlock()
		return
	}
	attemptID := group.machine.Snapshot().ActiveAttempt

	ctx, cancel := context.WithCancel(context.Background())
	group.reconcileCancel = cancel
	s.mu.Unlock()
	defer cancel()

	preimage, err := s.cfg.PaymentReconciler(
		ctx, lntypes.Hash(id.PaymentHash),
	)
	s.completeAttempt(
		id, attemptID, preimage, err, retryablePaymentError(err), true,
	)
}

func (s *Service) cancelDownstream(id trampoline.RelayID) {
	s.mu.Lock()
	if s.stopped() {
		s.mu.Unlock()

		return
	}
	group, ok := s.groups[id]
	if !ok {
		s.mu.Unlock()

		return
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), s.cfg.PayTimeout,
	)
	group.cancelCancel = cancel
	s.mu.Unlock()

	err := s.cfg.PaymentCanceler(ctx, lntypes.Hash(id.PaymentHash))
	cancel()

	s.mu.Lock()
	group, ok = s.groups[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	group.cancelCancel = nil
	group.cancelRunning = false
	var attemptCancel context.CancelFunc
	if err == nil {
		group.cancelConfirmed = true
		attemptCancel = group.attemptCancel
	}
	reconcile := group.recovering && !group.reconciling
	if reconcile {
		group.recovering = false
		group.reconciling = true
	}
	s.mu.Unlock()
	if err != nil {
		log.Warnf("Unable to cancel trampoline payment %x: %v",
			id.PaymentHash, err)
	}

	// Canceling the sender context after FailPayment prevents another route
	// attempt. The payment lifecycle still waits for every in-flight HTLC
	// to become terminal before SendPayment returns.
	if attemptCancel != nil {
		attemptCancel()
	}
	if reconcile {
		s.launchReconcileDownstream(id)
	}
}

func (s *Service) makePayment(
	group *relayGroup) (*routing.LightningPayment, error) {

	payload := group.payload
	if payload.AmountToForward == nil ||
		payload.OutgoingCLTV == nil {

		return nil, errors.New(
			"incomplete downstream trampoline payload",
		)
	}

	snapshot := group.machine.Snapshot()
	if *payload.OutgoingCLTV <= snapshot.ObservedHeight {
		return nil, trampoline.ErrOuterCLTVInsufficient
	}
	delta := *payload.OutgoingCLTV - snapshot.ObservedHeight
	if delta > uint32(^uint16(0)) {
		return nil, trampoline.ErrOuterCLTVInsufficient
	}
	maxExpiry := ^uint32(0)
	for _, part := range snapshot.Parts {
		if part.CLTV < maxExpiry {
			maxExpiry = part.CLTV
		}
	}
	if maxExpiry <= snapshot.ObservedHeight {
		return nil, trampoline.ErrOuterCLTVInsufficient
	}

	var (
		target         route.Vertex
		destFeatures   *lnwire.FeatureVector
		paymentAddr    fn.Option[[32]byte]
		blindedPathSet *routing.BlindedPaymentPathSet
	)
	if group.role == trampoline.RoleRecipientPaths {
		var err error
		blindedPathSet, err = s.recipientPathSet(
			payload.RecipientBlindedPaths,
		)
		if err != nil {
			return nil, err
		}
		copy(
			target[:],
			blindedPathSet.TargetPubKey().SerializeCompressed(),
		)
		destFeatures = blindedPathSet.Features()
	} else {
		nextNode := payload.OutgoingNodeID
		if isBlindedRole(group.role) {
			nextNode = group.nextNode
		}
		if nextNode == nil {
			return nil, errors.New(
				"downstream trampoline node id missing",
			)
		}
		copy(target[:], nextNode.SerializeCompressed())

		var addr [32]byte
		if _, err := io.ReadFull(
			s.cfg.RandomReader, addr[:],
		); err != nil {
			return nil, fmt.Errorf(
				"derive trampoline payment address: %w", err,
			)
		}
		paymentAddr = fn.Some(addr)
		rawFeatures := lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.PaymentAddrOptional,
			lnwire.MPPOptional,
			lnwire.TrampolineRoutingOptional,
		)
		destFeatures = lnwire.NewFeatureVector(
			rawFeatures, lnwire.Features,
		)
	}

	payment := &routing.LightningPayment{
		Target: target,
		Amount: lnwire.MilliSatoshi(*payload.AmountToForward),
		FeeLimit: lnwire.MilliSatoshi(snapshot.ExpectedTotalMsat -
			*payload.AmountToForward),
		CltvLimit:         maxExpiry - snapshot.ObservedHeight,
		FinalCLTVDelta:    uint16(delta),
		PayAttemptTimeout: s.cfg.PayTimeout,
		DestFeatures:      destFeatures,
		PaymentAddr:       paymentAddr,
		MaxParts:          s.cfg.MaxParts,
		BlindedPathSet:    blindedPathSet,
		TrampolineOnion:   append([]byte(nil), group.nextOnion...),
		TrampolinePathKey: group.nextPathKey,
	}
	if blindedPathSet != nil {
		payment.FinalCLTVDelta = blindedPathSet.FinalCLTVDelta()
	}
	hash := lntypes.Hash(snapshot.ID.PaymentHash)
	if err := payment.SetPaymentHash(hash); err != nil {
		return nil, err
	}

	return payment, nil
}

func (s *Service) recipientPathSet(
	paths *trampoline.RecipientBlindedPaths) (
	*routing.BlindedPaymentPathSet, error) {

	if paths == nil || len(paths.Paths) == 0 {
		return nil, trampoline.ErrNoRecipientBlindedPaths
	}

	payments := make([]*routing.BlindedPayment, 0, len(paths.Paths))
	for i := range paths.Paths {
		path := &paths.Paths[i]
		introduction, err := s.resolveIntroductionNode(
			path.Path.IntroductionNode,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"resolve recipient path %d introduction: %w", i,
				err,
			)
		}

		blindedHops := make(
			[]*sphinx.BlindedHopInfo, len(path.Path.Hops),
		)
		for j := range path.Path.Hops {
			hop := &path.Path.Hops[j]
			blindedHops[j] = &sphinx.BlindedHopInfo{
				BlindedNodePub: hop.BlindedNodeID,
				CipherText: append(
					[]byte(nil), hop.EncryptedData...,
				),
			}
		}
		rawFeatures := path.PaymentInfo.Features.Clone()
		payment := &routing.BlindedPayment{
			BlindedPath: &sphinx.BlindedPath{
				IntroductionPoint: introduction,
				BlindingPoint:     path.Path.BlindingPoint,
				BlindedHops:       blindedHops,
			},
			BaseFee: path.PaymentInfo.FeeBaseMsat,
			ProportionalFeeRate: path.PaymentInfo.
				FeeProportionalMillionths,
			CltvExpiryDelta: path.PaymentInfo.CltvExpiryDelta,
			HtlcMinimum:     path.PaymentInfo.HtlcMinimumMsat,
			HtlcMaximum:     path.PaymentInfo.HtlcMaximumMsat,
			Features: lnwire.NewFeatureVector(
				rawFeatures, lnwire.Features,
			),
		}
		if err := payment.Validate(); err != nil {
			return nil, fmt.Errorf(
				"validate recipient path %d: %w", i, err,
			)
		}
		payments = append(payments, payment)
	}

	return routing.NewBlindedPaymentPathSet(payments)
}

func (s *Service) resolveIntroductionNode(
	introduction lnwire.IntroductionNode) (*btcec.PublicKey, error) {

	switch node := introduction.(type) {
	case lnwire.PubkeyIntro:
		return node.Pubkey, nil

	case *lnwire.PubkeyIntro:
		return node.Pubkey, nil

	default:
		if s.cfg.ResolveIntroductionNode == nil {
			return nil, errors.New(
				"sciddir introduction resolver unavailable",
			)
		}

		return s.cfg.ResolveIntroductionNode(introduction)
	}
}

func (s *Service) completeAttempt(id trampoline.RelayID, attemptID uint16,
	preimage [32]byte, paymentErr error, retryable,
	downstreamResult bool) {

	// Any downstream result observed after relay shutdown is not an
	// authoritative payment outcome. The underlying HTLC attempt remains
	// durable and is resumed by the router on startup. Preserve RelayPaying
	// so the next service instance reconciles that durable payment instead
	// of failing held upstream parts with a synthetic shutdown error.
	select {
	case <-s.quit:
		return
	default:
	}

	s.mu.Lock()
	group, ok := s.groups[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	if group.cancelAttempt == attemptID {
		group.attemptCancel = nil
		group.cancelAttempt = 0
	}
	group.dispatcher.Finish(false)
	group.reconciling = false
	group.reconcileCancel = nil
	if paymentErr != nil && downstreamResult &&
		isBlindedRole(group.role) {

		paymentErr = errBlindedDownstream
		retryable = false
	}

	var (
		actions []trampoline.Action
		err     error
	)
	if paymentErr == nil {
		actions, err = group.machine.AttemptSucceeded(attemptID)
	} else {
		actions, err = group.machine.AttemptFailed(attemptID, retryable)
	}
	if err != nil {
		s.mu.Unlock()
		return
	}

	resolutions := s.resolutionsLocked(group, actions, preimage, paymentErr)
	retry := hasAction(actions, trampoline.ActionRetryReady)
	group.dispatcher.Finish(retry)
	if paymentErr == nil {
		result := preimage
		group.preimage = &result
	} else if !retry {
		group.failure = failureFromPaymentError(paymentErr).Code()
		var opaque *routing.OpaqueTrampolineError
		if errors.As(paymentErr, &opaque) {
			group.failureReason = append(
				lnwire.OpaqueReason(nil), opaque.Reason...,
			)
		}
	}
	if err := s.persistGroup(id, group); err != nil {
		s.mu.Unlock()
		return
	}
	if group.machine.Snapshot().State.IsTerminal() {
		s.evictGroupLocked(id, group)
	}
	s.mu.Unlock()

	s.deliver(resolutions)
	if retry {
		s.launchRunDownstream(id)
	}
}

func retryablePaymentError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {

		return false
	}

	var opaque *routing.OpaqueTrampolineError
	if errors.As(err, &opaque) {
		return false
	}

	var reason paymentsdb.FailureReason
	if errors.As(err, &reason) {
		return reason == paymentsdb.FailureReasonError
	}

	return true
}

type queuedResolution struct {
	subscriber chan<- interface{}
	resolution invoices.HtlcResolution
}

func (s *Service) resolutionsLocked(group *relayGroup,
	actions []trampoline.Action, preimage [32]byte,
	paymentErr error) []queuedResolution {

	resolutions := make([]queuedResolution, 0, len(actions))
	for _, action := range actions {
		subscriber, ok := group.subscribers[action.PartID]
		if !ok {
			continue
		}
		key := models.CircuitKey{
			ChanID: lnwire.NewShortChanIDFromInt(
				action.PartID.ChannelID,
			),
			HtlcID: action.PartID.HTLCID,
		}
		height := group.heights[action.PartID]

		var resolution invoices.HtlcResolution
		switch action.Kind {
		case trampoline.ActionFulfillPart:
			resolution = invoices.NewSettleResolution(
				lntypes.Preimage(preimage), key, height,
				invoices.ResultSettled,
			)

		case trampoline.ActionFailPart:
			var opaque *routing.OpaqueTrampolineError
			if errors.As(paymentErr, &opaque) {
				resolution = invoices.NewNestedErrorResolution(
					key, height, opaque.Reason,
				)
			} else {
				resolution = invoices.NewErrorResolution(
					key, height,
					failureFromPaymentError(paymentErr),
				)
			}

		default:
			continue
		}
		resolutions = append(resolutions, queuedResolution{
			subscriber: subscriber,
			resolution: resolution,
		})
	}

	return resolutions
}

func (s *Service) resolveFailure(id trampoline.RelayID,
	failure lnwire.FailureMessage) {

	s.mu.Lock()
	group, ok := s.groups[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	actions, _ := group.machine.Cancel()
	canceling := hasAction(
		actions, trampoline.ActionCancelDownstream,
	) && !group.cancelRunning && !group.cancelConfirmed
	if canceling {
		group.cancelRunning = true
	}
	resolutions := make([]queuedResolution, 0, len(actions))
	for _, action := range actions {
		subscriber, ok := group.subscribers[action.PartID]
		if !ok {
			continue
		}
		key := models.CircuitKey{
			ChanID: lnwire.NewShortChanIDFromInt(
				action.PartID.ChannelID,
			),
			HtlcID: action.PartID.HTLCID,
		}
		resolutions = append(resolutions, queuedResolution{
			subscriber: subscriber,
			resolution: invoices.NewErrorResolution(
				key, group.heights[action.PartID], failure,
			),
		})
	}
	if hasAction(actions, trampoline.ActionFailPart) {
		group.failure = failure.Code()
	}
	if err := s.persistGroup(id, group); err != nil {
		s.mu.Unlock()
		return
	}
	if group.machine.Snapshot().State.IsTerminal() {
		s.evictGroupLocked(id, group)
	}
	s.mu.Unlock()

	s.deliver(resolutions)
	if canceling {
		s.launchCancelDownstream(id)
	}
}

func (s *Service) deliver(resolutions []queuedResolution) {
	for _, item := range resolutions {
		select {
		case item.subscriber <- item.resolution:
		case <-s.quit:
			return
		}
	}
}

func (s *Service) evictGroupLocked(id trampoline.RelayID,
	group *relayGroup) {

	if current, ok := s.groups[id]; !ok || current != group {
		return
	}
	delete(s.groups, id)
	if group.counted && s.activeGroups != 0 {
		s.activeGroups--
		group.counted = false
	}
}

func (s *Service) failure(htlc htlcswitch.TrampolineHtlc,
	failure lnwire.FailureMessage) invoices.HtlcResolution {

	return invoices.NewErrorResolution(
		htlc.CircuitKey, htlc.CurrentHeight, failure,
	)
}

func (s *Service) failureForError(htlc htlcswitch.TrampolineHtlc,
	err error) invoices.HtlcResolution {

	if htlc.CurrentPathKey != nil {
		return s.failure(htlc, &lnwire.FailInvalidBlinding{})
	}

	switch {
	case errors.Is(err, trampoline.ErrOuterAmountInsufficient),
		errors.Is(err, trampoline.ErrOuterTotalInsufficient),
		errors.Is(err, trampoline.ErrOuterCLTVInsufficient),
		errors.Is(err, trampoline.ErrPartCLTVInsufficient):

		return s.failure(htlc,
			&lnwire.FailTrampolineFeeOrExpiryInsufficient{
				FeeBaseMsat: s.cfg.FeeBaseMsat,
				FeeProportionalMillionths: s.cfg.
					FeeProportionalMillionths,
				CltvExpiryDelta: s.cfg.CLTVExpiryDelta,
			},
		)

	default:
		return s.failure(
			htlc, &lnwire.FailTemporaryTrampolineFailure{},
		)
	}
}

func failureFromPaymentError(err error) lnwire.FailureMessage {
	if errors.Is(err, errBlindedDownstream) {
		return &lnwire.FailInvalidBlinding{}
	}
	if errors.Is(err, paymentsdb.FailureReasonNoRoute) {
		return &lnwire.FailUnknownNextTrampoline{}
	}

	return &lnwire.FailTemporaryTrampolineFailure{}
}

func failureForCode(code lnwire.FailCode) lnwire.FailureMessage {
	switch code {
	case lnwire.CodeInvalidBlinding:
		return &lnwire.FailInvalidBlinding{}

	case lnwire.CodeUnknownNextTrampoline:
		return &lnwire.FailUnknownNextTrampoline{}

	case lnwire.CodeTrampolineFeeOrExpiryInsufficient:
		return &lnwire.FailTrampolineFeeOrExpiryInsufficient{}

	default:
		return &lnwire.FailTemporaryTrampolineFailure{}
	}
}

func hasAction(actions []trampoline.Action,
	kind trampoline.ActionKind) bool {

	for _, action := range actions {
		if action.Kind == kind {
			return true
		}
	}

	return false
}

// HodlUnsubscribeAll removes every pending delivery owned by a stopped link.
func (s *Service) HodlUnsubscribeAll(subscriber chan<- interface{}) {
	s.cfg.InvoiceRegistry.HodlUnsubscribeAll(subscriber)

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, group := range s.groups {
		for partID, candidate := range group.subscribers {
			if candidate != subscriber {
				continue
			}

			delete(group.subscribers, partID)
			delete(group.heights, partID)
		}
	}
}

type invoicePayload struct {
	mpp      *record.MPP
	metadata []byte
	total    lnwire.MilliSatoshi
	pathID   *chainhash.Hash
}

func newInvoicePayload(payload *trampoline.Payload) *invoicePayload {
	var total lnwire.MilliSatoshi
	switch {
	case payload.PaymentData != nil:
		total = payload.PaymentData.TotalMsat()

	case payload.TotalAmountMsat != nil:
		total = lnwire.MilliSatoshi(*payload.TotalAmountMsat)

	case payload.AmountToForward != nil:
		total = lnwire.MilliSatoshi(*payload.AmountToForward)
	}

	return &invoicePayload{
		mpp:      payload.PaymentData,
		metadata: append([]byte(nil), payload.PaymentMetadata...),
		total:    total,
	}
}

func (p *invoicePayload) MultiPath() *record.MPP {
	return p.mpp
}

func (p *invoicePayload) AMPRecord() *record.AMP {
	return nil
}

func (p *invoicePayload) CustomRecords() record.CustomSet {
	return nil
}

func (p *invoicePayload) Metadata() []byte {
	return append([]byte(nil), p.metadata...)
}

func (p *invoicePayload) PathID() *chainhash.Hash {
	if p.pathID == nil {
		return nil
	}

	pathID := *p.pathID

	return &pathID
}

func (p *invoicePayload) TotalAmtMsat() lnwire.MilliSatoshi {
	return p.total
}

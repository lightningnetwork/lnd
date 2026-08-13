package descriptorsweep

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/descriptors"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
)

var (
	// ErrNotFound is returned when a descriptor registration is unknown.
	ErrNotFound = errors.New("descriptor sweep registration not found")

	// ErrAlreadyExists is returned when a descriptor registration already
	// exists.
	ErrAlreadyExists = errors.New("descriptor sweep registration exists")
)

// RegistrationID is a stable identifier derived from a registration's
// canonical descriptor and key bindings.
type RegistrationID [32]byte

// String returns the hexadecimal registration identifier.
func (i RegistrationID) String() string {
	return hex.EncodeToString(i[:])
}

// ParseRegistrationID parses a hexadecimal registration identifier.
func ParseRegistrationID(id string) (RegistrationID, error) {
	var result RegistrationID

	raw, err := hex.DecodeString(id)
	if err != nil {
		return result, err
	}
	if len(raw) != len(result) {
		return result, fmt.Errorf("registration id must be %d bytes", len(result))
	}
	copy(result[:], raw)

	return result, nil
}

// RegistrationIDFromBytes parses a raw 32-byte registration identifier.
func RegistrationIDFromBytes(id []byte) (RegistrationID, error) {
	var result RegistrationID
	if len(id) != len(result) {
		return result, fmt.Errorf("registration id must be %d bytes", len(result))
	}
	copy(result[:], id)
	return result, nil
}

// Bytes returns a copy of the raw registration identifier.
func (i RegistrationID) Bytes() []byte {
	result := make([]byte, len(i))
	copy(result, i[:])
	return result
}

// Status is the durable lifecycle state of a descriptor sweep.
type Status uint8

const (
	StatusRegistered Status = iota
	StatusWatching
	StatusFound
	StatusWaiting
	StatusSweeping
	StatusSwept
	StatusFailed
)

// String returns a human-readable status name.
func (s Status) String() string {
	switch s {
	case StatusRegistered:
		return "registered"
	case StatusWatching:
		return "watching"
	case StatusFound:
		return "found"
	case StatusWaiting:
		return "waiting"
	case StatusSweeping:
		return "sweeping"
	case StatusSwept:
		return "swept"
	case StatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// KeyBinding binds one key expression in a descriptor to an lnd key locator.
// The binding is accepted only when the key ring derives the same public key.
type KeyBinding struct {
	DescriptorKey string
	KeyLocator    keychain.KeyLocator
}

// RegisterRequest describes one fixed-index native P2WSH output to watch and
// sweep. Ranged and multipath descriptors are deliberately excluded from the
// first version of the service.
type RegisterRequest struct {
	Descriptor      string
	DerivationIndex uint32
	KeyBindings     []KeyBinding
	ExpectedValue   btcutil.Amount
	HeightHint      uint32
	MinConfs        uint32
	Budget          btcutil.Amount
	DeadlineDelta   uint32
	Immediate       bool
	Label           string
	StartingFeeRate fn.Option[chainfee.SatPerKWeight]
}

// Record is the public, immutable snapshot of a registration.
type Record struct {
	ID                  RegistrationID
	Descriptor          string
	CanonicalDescriptor string
	DerivationIndex     uint32
	KeyBindings         []KeyBinding
	Address             string
	PkScript            []byte
	WitnessScript       []byte
	ExpectedValue       btcutil.Amount
	HeightHint          uint32
	MinConfs            uint32
	Budget              btcutil.Amount
	DeadlineDelta       uint32
	Immediate           bool
	Label               string
	Status              Status
	OutPoint            *wire.OutPoint
	Value               int64
	ConfirmationHeight  uint32
	SweepTxID           *chainhash.Hash
	Error               string
}

type storedRecord struct {
	Record
	WatchHeight        uint32
	BlockScan          bool
	Preimages          map[string][]byte
	PlanLocktime       *uint32
	PlanSequence       *uint32
	PlanDeadlineHeight *int32
	HasStartingFeeRate bool
	StartingFeeRate    chainfee.SatPerKWeight
}

func (r *storedRecord) snapshot() *Record {
	result := r.Record
	result.KeyBindings = append([]KeyBinding(nil), r.KeyBindings...)
	result.PkScript = append([]byte(nil), r.PkScript...)
	result.WitnessScript = append([]byte(nil), r.WitnessScript...)
	if r.OutPoint != nil {
		op := *r.OutPoint
		result.OutPoint = &op
	}
	if r.SweepTxID != nil {
		txid := *r.SweepTxID
		result.SweepTxID = &txid
	}
	return &result
}

// Sweeper is the subset of UtxoSweeper used by the service.
type Sweeper interface {
	SweepInput(input.Input, sweep.Params) (chan sweep.Result, error)
}

// BlockSource retrieves blocks by main-chain height. It is used after a
// wrong-value script match because lnd's script confirmation cache deliberately
// retains the first match and cannot be re-registered to discover address
// reuse safely.
type BlockSource interface {
	GetBlockHash(blockHeight int64) (*chainhash.Hash, error)
	GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error)
}

// Config contains the dependencies of the descriptor sweep service.
type Config struct {
	DB          kvdb.Backend
	Notifier    chainntnfs.ChainNotifier
	KeyRing     keychain.KeyRing
	Sweeper     Sweeper
	BlockSource BlockSource
	ChainParams *chaincfg.Params

	// Ready is closed after both the chain notifier and UTXO sweeper have
	// started. WalletKit itself starts before either dependency, so notifier
	// registrations must be deferred until this explicit lifecycle signal.
	Ready <-chan struct{}
}

// Service durably watches descriptor outputs and hands immutable, satisfiable
// inputs to the existing UTXO sweeper.
type Service struct {
	cfg   Config
	store recordStore

	mu      sync.RWMutex
	scanMu  sync.Mutex
	records map[RegistrationID]*storedRecord
	watches map[RegistrationID]func()
	pending map[RegistrationID]struct{}
	// retrying maps an active retry worker to whether another retry request
	// arrived while its task was running.
	retrying map[retryKey]bool

	bestHeight    uint32
	started       bool
	notifierReady bool
	quit          chan struct{}
	wg            sync.WaitGroup
	retryInitial  time.Duration
	retryMax      time.Duration
}

type retryKey struct {
	id   RegistrationID
	kind string
}

const (
	defaultRetryInitial = 100 * time.Millisecond
	defaultRetryMax     = 5 * time.Second
)

// New constructs a descriptor sweep service and initializes its bucket.
func New(cfg Config) (*Service, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("descriptor sweep DB is required")
	case cfg.Notifier == nil:
		return nil, errors.New("descriptor sweep notifier is required")
	case cfg.KeyRing == nil:
		return nil, errors.New("descriptor sweep key ring is required")
	case cfg.Sweeper == nil:
		return nil, errors.New("descriptor sweep sweeper is required")
	case cfg.BlockSource == nil:
		return nil, errors.New("descriptor sweep block source is required")
	case cfg.ChainParams == nil:
		return nil, errors.New("descriptor sweep chain params are required")
	case cfg.Ready == nil:
		return nil, errors.New("descriptor sweep ready signal is required")
	}

	storage := newStore(cfg.DB)
	if err := storage.init(); err != nil {
		return nil, err
	}

	return &Service{
		cfg:          cfg,
		store:        storage,
		records:      make(map[RegistrationID]*storedRecord),
		watches:      make(map[RegistrationID]func()),
		pending:      make(map[RegistrationID]struct{}),
		retrying:     make(map[retryKey]bool),
		quit:         make(chan struct{}),
		retryInitial: defaultRetryInitial,
		retryMax:     defaultRetryMax,
	}, nil
}

func (s *Service) storage() recordStore {
	if s.store != nil {
		return s.store
	}
	return newStore(s.cfg.DB)
}

// updateRecordLocked performs a durable copy-on-write transition. The caller
// must hold s.mu. The live record is replaced only after the next state has
// committed successfully.
func (s *Service) updateRecordLocked(id RegistrationID,
	mutate func(*storedRecord) error) (*storedRecord, error) {

	current, ok := s.records[id]
	if !ok {
		return nil, ErrNotFound
	}
	next := current.cloneForInput()
	if err := mutate(next); err != nil {
		return nil, err
	}
	if err := s.storage().put(next); err != nil {
		return nil, retryable(fmt.Errorf("persist descriptor sweep: %w", err))
	}
	s.records[id] = next
	return next, nil
}

func registrationID(descriptor string, bindings []KeyBinding,
	index uint32, expectedValue btcutil.Amount) RegistrationID {

	copyBindings := append([]KeyBinding(nil), bindings...)
	sort.Slice(copyBindings, func(i, j int) bool {
		return copyBindings[i].DescriptorKey < copyBindings[j].DescriptorKey
	})

	h := sha256.New()
	_, _ = h.Write([]byte(descriptor))
	_, _ = fmt.Fprintf(h, "|%d|value:%d", index, expectedValue)
	for _, binding := range copyBindings {
		_, _ = fmt.Fprintf(
			h, "|%s:%d:%d", binding.DescriptorKey,
			binding.KeyLocator.Family, binding.KeyLocator.Index,
		)
	}

	var id RegistrationID
	copy(id[:], h.Sum(nil))
	return id
}

func descriptorScripts(desc *descriptors.Descriptor, params *chaincfg.Params,
	index uint32) (string, []byte, []byte, error) {

	addressString, err := desc.AddressAt(params, 0, index)
	if err != nil {
		return "", nil, nil, err
	}
	addr, err := address.DecodeAddress(addressString, params)
	if err != nil {
		return "", nil, nil, err
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return "", nil, nil, err
	}
	witnessScript, err := desc.ScriptCodeAt(0, index)
	if err != nil {
		return "", nil, nil, err
	}

	return addressString, pkScript, witnessScript, nil
}

// Start restores durable registrations. Notifier registrations are installed
// asynchronously once both the chain notifier and UTXO sweeper are ready.
func (s *Service) Start() error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}

	records, err := s.storage().list()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	for _, record := range records {
		s.records[record.ID] = record
		s.pending[record.ID] = struct{}{}
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()

	go s.waitForReady()

	return nil
}

// Stop cancels all notifier registrations and waits for workers to exit.
func (s *Service) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	select {
	case <-s.quit:
	default:
		close(s.quit)
	}
	for _, cancel := range s.watches {
		cancel()
	}
	s.watches = make(map[RegistrationID]func())
	s.pending = make(map[RegistrationID]struct{})
	s.retrying = make(map[retryKey]bool)
	s.notifierReady = false
	s.mu.Unlock()

	s.wg.Wait()
	return nil
}

// Register validates and persists one descriptor watch.
func (s *Service) Register(_ context.Context,
	req RegisterRequest) (*Record, error) {

	if req.HeightHint == 0 {
		return nil, errors.New("height hint must be non-zero")
	}
	if req.ExpectedValue <= 0 {
		return nil, errors.New("expected output value must be positive")
	}
	if req.ExpectedValue > btcutil.MaxSatoshi {
		return nil, errors.New("expected output value exceeds maximum money")
	}
	if req.Budget <= 0 {
		return nil, errors.New("sweep budget must be positive")
	}
	if req.Budget > req.ExpectedValue {
		return nil, errors.New("sweep budget must not exceed expected output value")
	}
	if req.MinConfs == 0 {
		req.MinConfs = 1
	}
	if req.MinConfs > chainntnfs.MaxNumConfs {
		return nil, fmt.Errorf("min confirmations must not exceed %d",
			chainntnfs.MaxNumConfs)
	}
	if req.DeadlineDelta > uint32(math.MaxInt32) {
		return nil, errors.New("deadline delta exceeds maximum block height")
	}
	if len(req.Label) > 500 {
		return nil, errors.New("label must not exceed 500 bytes")
	}

	desc, err := descriptors.NewDescriptor(req.Descriptor)
	if err != nil {
		return nil, fmt.Errorf("parse descriptor: %w", err)
	}
	if desc.DescType() != descriptors.DescTypeWsh {
		return nil, fmt.Errorf("only native wsh descriptors are supported")
	}
	if desc.MultipathLen() != 1 {
		return nil, errors.New("multipath descriptors are not supported")
	}
	if req.DerivationIndex != 0 {
		return nil, errors.New("ranged descriptors are not supported")
	}

	if err := rejectTimeLocks(desc); err != nil {
		return nil, err
	}
	if err := s.verifyBindings(desc, req.KeyBindings); err != nil {
		return nil, err
	}

	canonical := desc.String()
	addr, pkScript, witnessScript, err := descriptorScripts(
		desc, s.cfg.ChainParams, req.DerivationIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("derive descriptor: %w", err)
	}

	id := registrationID(
		canonical, req.KeyBindings, req.DerivationIndex,
		req.ExpectedValue,
	)
	record := &storedRecord{
		Record: Record{
			ID:                  id,
			Descriptor:          req.Descriptor,
			CanonicalDescriptor: canonical,
			DerivationIndex:     req.DerivationIndex,
			KeyBindings:         append([]KeyBinding(nil), req.KeyBindings...),
			Address:             addr,
			PkScript:            pkScript,
			WitnessScript:       witnessScript,
			ExpectedValue:       req.ExpectedValue,
			HeightHint:          req.HeightHint,
			MinConfs:            req.MinConfs,
			Budget:              req.Budget,
			DeadlineDelta:       req.DeadlineDelta,
			Immediate:           req.Immediate,
			Label:               req.Label,
			Status:              StatusRegistered,
		},
		WatchHeight: req.HeightHint,
		Preimages:   make(map[string][]byte),
	}
	if req.StartingFeeRate.IsSome() {
		record.HasStartingFeeRate = true
		record.StartingFeeRate = req.StartingFeeRate.UnwrapOr(0)
	}

	s.mu.Lock()
	if _, ok := s.records[id]; ok {
		s.mu.Unlock()
		return nil, ErrAlreadyExists
	}
	if err := s.storage().put(record); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.records[id] = record
	notifierReady := s.notifierReady
	if !notifierReady {
		if s.pending == nil {
			s.pending = make(map[RegistrationID]struct{})
		}
		s.pending[id] = struct{}{}
	}
	s.mu.Unlock()

	if notifierReady {
		if err := s.watchOutput(id); err != nil {
			s.handleRegistrationError(id, err)
		}
	}

	return s.Get(id)
}

// AddPreimage persists a 32-byte late-bound preimage and retries planning.
func (s *Service) AddPreimage(_ context.Context, id RegistrationID,
	preimage []byte) (*Record, error) {

	if len(preimage) != 32 {
		return nil, errors.New("preimage must be 32 bytes")
	}

	s.mu.Lock()
	record, ok := s.records[id]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if record.Status == StatusSweeping || record.Status == StatusSwept ||
		record.Status == StatusFailed {
		s.mu.Unlock()
		return nil, errors.New("descriptor sweep branch is already frozen")
	}
	hash := sha256.Sum256(preimage)
	desc, err := descriptors.NewDescriptor(record.CanonicalDescriptor)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	policy, err := desc.Lift()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if !policyCommitsSHA256(policy, hash[:]) {
		s.mu.Unlock()
		return nil, errors.New("preimage does not match a descriptor sha256 commitment")
	}
	record, err = s.updateRecordLocked(id, func(next *storedRecord) error {
		if next.Preimages == nil {
			next.Preimages = make(map[string][]byte)
		}
		next.Preimages[preimageKey("sha256", hash[:])] =
			append([]byte(nil), preimage...)
		return nil
	})
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	notifierReady := s.notifierReady
	if !notifierReady {
		if s.pending == nil {
			s.pending = make(map[RegistrationID]struct{})
		}
		s.pending[id] = struct{}{}
	}
	s.mu.Unlock()

	if !notifierReady {
		return s.Get(id)
	}
	if err := s.trySweep(id); err != nil {
		s.handleRegistrationError(id, err)
		if !isRetryable(err) {
			return nil, err
		}
	}
	return s.Get(id)
}

func policyCommitsSHA256(policy *descriptors.SemanticPolicy,
	digest []byte) bool {

	if policy == nil {
		return false
	}
	if policy.Type == descriptors.SemanticPolicyTypeSha256 &&
		policy.Hash != nil {

		committed, err := hex.DecodeString(*policy.Hash)
		if err == nil && bytes.Equal(committed, digest) {
			return true
		}
	}
	for _, child := range policy.Policies {
		if policyCommitsSHA256(child, digest) {
			return true
		}
	}
	return false
}

// Get returns one registration snapshot.
func (s *Service) Get(id RegistrationID) (*Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.records[id]
	if !ok {
		return nil, ErrNotFound
	}
	return record.snapshot(), nil
}

// List returns all registration snapshots sorted by identifier.
func (s *Service) List() []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Record, 0, len(s.records))
	for _, record := range s.records {
		result = append(result, record.snapshot())
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].ID.String() < result[j].ID.String()
	})
	return result
}

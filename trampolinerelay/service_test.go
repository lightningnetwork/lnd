package trampolinerelay

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/stretchr/testify/require"
)

type mockOnionProcessor struct {
	mu             sync.Mutex
	processed      *sphinx.ProcessedPacket
	processedQueue []*sphinx.ProcessedPacket
	err            error
	calls          int
	pathKey        *btcec.PublicKey
}

type mockBlindingProcessor struct{}

type failPutStore struct {
	*MemoryStore

	mu        sync.Mutex
	failCount int
}

func (f *failPutStore) Put(id trampoline.RelayID, group StoredGroup) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failCount != 0 {
		f.failCount--

		return errors.New("injected relay store put failure")
	}

	return f.MemoryStore.Put(id, group)
}

func (f *failPutStore) failNextPut() {
	f.mu.Lock()
	f.failCount++
	f.mu.Unlock()
}

func terminalPruneAfterHeight(t *testing.T, service *Service,
	id trampoline.RelayID) uint32 {

	t.Helper()
	for _, record := range service.terminalIDs {
		if record.id == id {
			return record.pruneAfterHeight
		}
	}

	require.FailNow(t, "terminal relay record not found")

	return 0
}

func (*mockBlindingProcessor) DecryptBlindedHopData(
	_ *btcec.PublicKey, encrypted []byte) ([]byte, error) {

	return append([]byte(nil), encrypted...), nil
}

func (*mockBlindingProcessor) NextEphemeral(
	pathKey *btcec.PublicKey) (*btcec.PublicKey, error) {

	return pathKey, nil
}

func (m *mockOnionProcessor) ProcessTrampolineOnion(
	_ *sphinx.OnionPacket, _ []byte, _ uint32,
	pathKey *btcec.PublicKey) (*sphinx.ProcessedPacket, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	m.pathKey = pathKey
	if len(m.processedQueue) != 0 {
		processed := m.processedQueue[0]
		m.processedQueue = m.processedQueue[1:]

		return processed, m.err
	}

	return m.processed, m.err
}

func (m *mockOnionProcessor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

func (m *mockOnionProcessor) processedPathKey() *btcec.PublicKey {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.pathKey
}

type mockPaymentSender struct {
	payments chan *routing.LightningPayment
	preimage [32]byte
	err      error
}

type retryPaymentSender struct {
	mu       sync.Mutex
	payments chan *routing.LightningPayment
	errors   []error
	preimage [32]byte
}

func (r *retryPaymentSender) SendPayment(_ context.Context,
	payment *routing.LightningPayment) ([32]byte, *route.Route, error) {

	r.payments <- payment
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.errors) == 0 {
		return r.preimage, nil, nil
	}
	err := r.errors[0]
	r.errors = r.errors[1:]

	return [32]byte{}, nil, err
}

func (m *mockPaymentSender) SendPayment(_ context.Context,
	payment *routing.LightningPayment) ([32]byte, *route.Route, error) {

	m.payments <- payment
	return m.preimage, nil, m.err
}

type blockingPaymentSender struct {
	started  chan struct{}
	canceled chan struct{}
}

func (b *blockingPaymentSender) SendPayment(ctx context.Context,
	_ *routing.LightningPayment) ([32]byte, *route.Route, error) {

	close(b.started)
	<-ctx.Done()
	close(b.canceled)

	return [32]byte{}, nil, ctx.Err()
}

type countingPaymentSender struct {
	mu      sync.Mutex
	started chan struct{}
	calls   int
}

func (c *countingPaymentSender) SendPayment(ctx context.Context,
	_ *routing.LightningPayment) ([32]byte, *route.Route, error) {

	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	c.started <- struct{}{}
	<-ctx.Done()

	return [32]byte{}, nil, ctx.Err()
}

func (c *countingPaymentSender) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

type cancelSettlesPaymentSender struct {
	started  chan struct{}
	preimage [32]byte
}

func (c *cancelSettlesPaymentSender) SendPayment(ctx context.Context,
	_ *routing.LightningPayment) ([32]byte, *route.Route, error) {

	close(c.started)
	<-ctx.Done()

	return c.preimage, nil, nil
}

type invoiceCall struct {
	hash    lntypes.Hash
	amount  lnwire.MilliSatoshi
	payload invoices.Payload
}

type mockInvoiceRegistry struct {
	calls      chan invoiceCall
	resolution invoices.HtlcResolution
	err        error
}

type listErrorStore struct {
	Store
	err error
}

func (l *listErrorStore) List() ([]StoredGroupRecord, error) {
	return nil, l.err
}

func (m *mockInvoiceRegistry) NotifyExitHopHtlc(hash lntypes.Hash,
	amount lnwire.MilliSatoshi, _ uint32, _ int32, _ models.CircuitKey,
	_ chan<- interface{}, _ lnwire.CustomRecords,
	payload invoices.Payload) (invoices.HtlcResolution, error) {

	m.calls <- invoiceCall{
		hash:    hash,
		amount:  amount,
		payload: payload,
	}

	return m.resolution, m.err
}

func (*mockInvoiceRegistry) HodlUnsubscribeAll(chan<- interface{}) {
}

func testPacket(t *testing.T) *sphinx.OnionPacket {
	t.Helper()

	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x31}, 32))
	packet := &sphinx.OnionPacket{
		Version:      0,
		EphemeralKey: key.PubKey(),
		RoutingInfo:  bytes.Repeat([]byte{0x17}, 97),
	}
	copy(packet.HeaderMAC[:], bytes.Repeat([]byte{0x29}, sphinx.HMACSize))

	return packet
}

func distinctTestPacket(t *testing.T) *sphinx.OnionPacket {
	t.Helper()

	packet := testPacket(t)
	packet.RoutingInfo[0] ^= 1

	return packet
}

func encodedPayload(t *testing.T,
	payload *trampoline.Payload) []byte {

	t.Helper()

	var encoded bytes.Buffer
	require.NoError(t, payload.Encode(&encoded))

	return encoded.Bytes()
}

func testService(t *testing.T, processor *mockOnionProcessor,
	sender PaymentSender,
	registry *mockInvoiceRegistry) *Service {

	return testServiceWithStore(t, processor, sender, registry, nil)
}

func testServiceWithStore(t *testing.T, processor *mockOnionProcessor,
	sender PaymentSender, registry *mockInvoiceRegistry,
	store Store) *Service {

	t.Helper()

	service, err := NewService(Config{
		OnionProcessor:    processor,
		BlindingProcessor: &mockBlindingProcessor{},
		PaymentSender:     sender,
		InvoiceRegistry:   registry,
		PaymentReconciler: func(ctx context.Context,
			_ lntypes.Hash) ([32]byte, error) {

			<-ctx.Done()

			return [32]byte{}, ctx.Err()
		},
		PaymentCanceler: func(context.Context, lntypes.Hash) error {
			return nil
		},
		RelayConfig: trampoline.RelayConfig{
			MaxParts:           4,
			MaxAttempts:        1,
			HoldBlocks:         40,
			RecoveryBlocks:     6,
			ExpirySafetyBlocks: 1,
			MaxReorgDepth:      3,
		},
		PayTimeout:   time.Second,
		MaxParts:     3,
		MaxGroups:    100,
		RandomReader: bytes.NewReader(bytes.Repeat([]byte{0x44}, 64)),
		Store:        store,
	})
	require.NoError(t, err)
	t.Cleanup(service.Stop)

	return service
}

// TestNewServiceValidation verifies that every mandatory dependency and
// resource bound is rejected before durable relay recovery starts.
func TestNewServiceValidation(t *testing.T) {
	t.Parallel()

	processor := &mockOnionProcessor{}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	valid := Config{
		OnionProcessor:    processor,
		BlindingProcessor: &mockBlindingProcessor{},
		PaymentSender:     sender,
		InvoiceRegistry:   registry,
		PaymentReconciler: func(context.Context,
			lntypes.Hash) ([32]byte, error) {

			return [32]byte{}, nil
		},
		PaymentCanceler: func(context.Context, lntypes.Hash) error {
			return nil
		},
		RelayConfig: trampoline.RelayConfig{
			MaxParts:           4,
			MaxAttempts:        1,
			HoldBlocks:         40,
			RecoveryBlocks:     6,
			ExpirySafetyBlocks: 1,
			MaxReorgDepth:      3,
		},
		PayTimeout: time.Second,
		MaxParts:   3,
		MaxGroups:  100,
		Store:      NewMemoryStore(),
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "onion processor",
			mutate: func(cfg *Config) {
				cfg.OnionProcessor = nil
			},
		},
		{
			name: "blinding processor",
			mutate: func(cfg *Config) {
				cfg.BlindingProcessor = nil
			},
		},
		{
			name: "payment sender",
			mutate: func(cfg *Config) {
				cfg.PaymentSender = nil
			},
		},
		{
			name: "invoice registry",
			mutate: func(cfg *Config) {
				cfg.InvoiceRegistry = nil
			},
		},
		{
			name: "payment reconciler",
			mutate: func(cfg *Config) {
				cfg.PaymentReconciler = nil
			},
		},
		{
			name: "payment canceler",
			mutate: func(cfg *Config) {
				cfg.PaymentCanceler = nil
			},
		},
		{
			name: "relay config",
			mutate: func(cfg *Config) {
				cfg.RelayConfig.MaxParts = 0
			},
		},
		{
			name: "payment timeout",
			mutate: func(cfg *Config) {
				cfg.PayTimeout = 0
			},
		},
		{
			name: "downstream max parts",
			mutate: func(cfg *Config) {
				cfg.MaxParts = 0
			},
		},
		{
			name: "relay max groups",
			mutate: func(cfg *Config) {
				cfg.MaxGroups = 0
			},
		},
		{
			name: "durable list",
			mutate: func(cfg *Config) {
				cfg.Store = &listErrorStore{
					Store: NewMemoryStore(),
					err:   errors.New("list failure"),
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			test.mutate(&cfg)
			service, err := NewService(cfg)
			require.Error(t, err)
			require.Nil(t, service)
		})
	}

	defaults := valid
	defaults.RandomReader = nil
	defaults.Store = nil
	service, err := NewService(defaults)
	require.NoError(t, err)
	require.NotNil(t, service.cfg.RandomReader)
	require.NotNil(t, service.cfg.Store)
	service.Stop()
}

// TestSelectRoleMatrix verifies that peer-controlled path-key and Sphinx
// combinations map to exactly one relay role or fail closed.
func TestSelectRoleMatrix(t *testing.T) {
	t.Parallel()

	privateKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x52}, 32))
	pathKey := privateKey.PubKey()
	nextPacket := testPacket(t)

	tests := []struct {
		name         string
		processed    *sphinx.ProcessedPacket
		payload      *trampoline.Payload
		outerPathKey *btcec.PublicKey
		role         trampoline.HopRole
		err          bool
	}{
		{
			name: "path key without encrypted data",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.ExitNode,
			},
			payload: &trampoline.Payload{CurrentPathKey: pathKey},
			err:     true,
		},
		{
			name: "outer path key without encrypted data",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.ExitNode,
			},
			payload:      &trampoline.Payload{},
			outerPathKey: pathKey,
			err:          true,
		},
		{
			name: "blinded hop without path key",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.ExitNode,
			},
			payload: &trampoline.Payload{
				EncryptedRecipientData: []byte{1},
			},
			err: true,
		},
		{
			name: "missing next packet",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.MoreHops,
			},
			payload: &trampoline.Payload{},
			err:     true,
		},
		{
			name: "missing clear next node",
			processed: &sphinx.ProcessedPacket{
				Action:     sphinx.MoreHops,
				NextPacket: nextPacket,
			},
			payload: &trampoline.Payload{},
			err:     true,
		},
		{
			name: "clear intermediate",
			processed: &sphinx.ProcessedPacket{
				Action:     sphinx.MoreHops,
				NextPacket: nextPacket,
			},
			payload: &trampoline.Payload{OutgoingNodeID: pathKey},
			role:    trampoline.RoleClearIntermediate,
		},
		{
			name: "blinded introduction",
			processed: &sphinx.ProcessedPacket{
				Action:     sphinx.MoreHops,
				NextPacket: nextPacket,
			},
			payload: &trampoline.Payload{
				EncryptedRecipientData: []byte{1},
				CurrentPathKey:         pathKey,
			},
			role: trampoline.RoleBlindedIntroduction,
		},
		{
			name: "blinded intermediate",
			processed: &sphinx.ProcessedPacket{
				Action:     sphinx.MoreHops,
				NextPacket: nextPacket,
			},
			payload: &trampoline.Payload{
				EncryptedRecipientData: []byte{1},
			},
			outerPathKey: pathKey,
			role:         trampoline.RoleBlindedIntermediate,
		},
		{
			name: "recipient blinded paths",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.ExitNode,
			},
			payload: &trampoline.Payload{
				RecipientBlindedPaths: &trampoline.
					RecipientBlindedPaths{},
			},
			role: trampoline.RoleRecipientPaths,
		},
		{
			name: "blinded introduction final",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.ExitNode,
			},
			payload: &trampoline.Payload{
				EncryptedRecipientData: []byte{1},
				CurrentPathKey:         pathKey,
			},
			role: trampoline.RoleBlindedIntroductionFinal,
		},
		{
			name: "blinded final",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.ExitNode,
			},
			payload: &trampoline.Payload{
				EncryptedRecipientData: []byte{1},
			},
			outerPathKey: pathKey,
			role:         trampoline.RoleBlindedFinal,
		},
		{
			name: "bolt 11 final",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.ExitNode,
			},
			payload: &trampoline.Payload{},
			role:    trampoline.RoleBolt11Final,
		},
		{
			name: "unknown action",
			processed: &sphinx.ProcessedPacket{
				Action: sphinx.ProcessCode(99),
			},
			payload: &trampoline.Payload{},
			err:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			role, err := selectRole(
				test.processed, test.payload, test.outerPathKey,
			)
			if test.err {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, test.role, role)
		})
	}
}

// TestRestoreGroupRejectsCorruptDurableState verifies that every encoded key,
// path identifier, payload, and state snapshot is authenticated during relay
// recovery before the group can resume downstream work.
func TestRestoreGroupRejectsCorruptDurableState(t *testing.T) {
	t.Parallel()

	processor := &mockOnionProcessor{}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	privateKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x53}, 32))
	publicKey := privateKey.PubKey()
	publicKeyBytes := publicKey.SerializeCompressed()
	amount := uint64(1_000)
	total := uint64(1_100)
	cltv := uint32(110)
	id := trampoline.RelayID{
		PaymentHash: [32]byte{1},
		PaymentAddr: [32]byte{2},
	}

	base := func(t *testing.T) StoredGroup {
		t.Helper()

		machine, err := trampoline.NewRelayMachine(
			service.cfg.RelayConfig, id, total, amount, cltv, 100,
		)
		require.NoError(t, err)
		_, err = machine.AddPart(trampoline.RelayPart{
			ID: trampoline.PartID{
				ChannelID: 1,
				HTLCID:    2,
			},
			AmountMsat: total,
			CLTV:       150,
		})
		require.NoError(t, err)
		_, err = machine.BeginAttempt()
		require.NoError(t, err)

		return StoredGroup{
			PayloadBytes: encodedPayload(t, &trampoline.Payload{
				AmountToForward: &amount,
				OutgoingCLTV:    &cltv,
				OutgoingNodeID:  publicKey,
			}),
			NextPathKey: append([]byte(nil), publicKeyBytes...),
			NextNode:    append([]byte(nil), publicKeyBytes...),
			PathID: bytes.Repeat(
				[]byte{3}, chainhash.HashSize,
			),
			OuterPathKey: append([]byte(nil), publicKeyBytes...),
			Role:         trampoline.RoleClearIntermediate,
			Snapshot:     machine.Snapshot(),
		}
	}

	valid, err := service.restoreGroup(base(t))
	require.NoError(t, err)
	require.NotNil(t, valid.machine)
	require.True(t, valid.recovering)
	require.True(t, valid.nextNode.IsEqual(publicKey))
	require.NotNil(t, valid.pathID)

	terminal := base(t)
	terminal.PayloadBytes = []byte{0xff}
	terminal.Preimage = &[32]byte{4}
	restoredTerminal, err := service.restoreGroup(terminal)
	require.NoError(t, err)
	require.Equal(t, terminal.Preimage, restoredTerminal.preimage)
	require.Nil(t, restoredTerminal.machine)

	tests := []struct {
		name   string
		mutate func(*StoredGroup)
	}{
		{
			name: "payload encoding",
			mutate: func(group *StoredGroup) {
				group.PayloadBytes = []byte{0xff}
			},
		},
		{
			name: "payload role",
			mutate: func(group *StoredGroup) {
				group.Role = trampoline.RoleBolt11Final
			},
		},
		{
			name: "outer path key",
			mutate: func(group *StoredGroup) {
				group.OuterPathKey = []byte{1}
			},
		},
		{
			name: "next path key",
			mutate: func(group *StoredGroup) {
				group.NextPathKey = []byte{1}
			},
		},
		{
			name: "next node",
			mutate: func(group *StoredGroup) {
				group.NextNode = []byte{1}
			},
		},
		{
			name: "path id",
			mutate: func(group *StoredGroup) {
				group.PathID = []byte{1}
			},
		},
		{
			name: "snapshot",
			mutate: func(group *StoredGroup) {
				group.Snapshot.Version++
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			group := base(t)
			test.mutate(&group)
			_, err := service.restoreGroup(group)
			require.Error(t, err)
		})
	}
}

// TestServiceBoundaryHelpers verifies the blockbeat adapter, stored failure
// mapping, invoice payload copy semantics, subscriber cleanup, and direct
// failure resolution at the relay boundary.
func TestServiceBoundaryHelpers(t *testing.T) {
	t.Parallel()

	processor := &mockOnionProcessor{}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	require.Equal(t, "trampoline-relay", service.Name())
	require.Error(t, service.ProcessBlock(chainio.NewBeat(
		chainntnfs.BlockEpoch{Height: -1},
	)))
	require.NoError(t, service.ProcessBlock(chainio.NewBeat(
		chainntnfs.BlockEpoch{Height: 100},
	)))

	failures := []struct {
		code    lnwire.FailCode
		failure lnwire.FailureMessage
	}{
		{lnwire.CodeInvalidBlinding, &lnwire.FailInvalidBlinding{}},
		{
			lnwire.CodeUnknownNextTrampoline,
			&lnwire.FailUnknownNextTrampoline{},
		},
		{
			lnwire.CodeTrampolineFeeOrExpiryInsufficient,
			&lnwire.FailTrampolineFeeOrExpiryInsufficient{},
		},
		{
			lnwire.CodeTemporaryChannelFailure,
			&lnwire.FailTemporaryTrampolineFailure{},
		},
	}
	for _, test := range failures {
		require.IsType(t, test.failure, failureForCode(test.code))
	}

	metadata := []byte{1, 2, 3}
	address := [32]byte{9}
	mpp := record.NewMPP(1_500, address)
	payload := newInvoicePayload(&trampoline.Payload{
		PaymentData:     mpp,
		PaymentMetadata: metadata,
	})
	require.Same(t, mpp, payload.MultiPath())
	require.Nil(t, payload.AMPRecord())
	require.Nil(t, payload.CustomRecords())
	require.Equal(t, lnwire.MilliSatoshi(1_500), payload.TotalAmtMsat())
	metadata[0] = 0xff
	require.Equal(t, []byte{1, 2, 3}, payload.Metadata())
	returnedMetadata := payload.Metadata()
	returnedMetadata[0] = 0xee
	require.Equal(t, []byte{1, 2, 3}, payload.Metadata())
	require.Nil(t, payload.PathID())

	pathID := chainhash.Hash{7}
	payload.pathID = &pathID
	returnedPathID := payload.PathID()
	returnedPathID[0] = 0xff
	require.Equal(t, byte(7), payload.PathID()[0])

	total := uint64(2_000)
	require.Equal(t, lnwire.MilliSatoshi(total), newInvoicePayload(
		&trampoline.Payload{TotalAmountMsat: &total},
	).TotalAmtMsat())
	amount := uint64(3_000)
	require.Equal(t, lnwire.MilliSatoshi(amount), newInvoicePayload(
		&trampoline.Payload{AmountToForward: &amount},
	).TotalAmtMsat())
	require.Zero(t, newInvoicePayload(
		&trampoline.Payload{},
	).TotalAmtMsat())

	matching := make(chan interface{}, 1)
	other := make(chan interface{}, 1)
	partOne := trampoline.PartID{ChannelID: 1, HTLCID: 1}
	partTwo := trampoline.PartID{ChannelID: 2, HTLCID: 2}
	cleanupID := trampoline.RelayID{PaymentHash: [32]byte{5}}
	service.groups[cleanupID] = &relayGroup{
		subscribers: map[trampoline.PartID]chan<- interface{}{
			partOne: matching,
			partTwo: other,
		},
		heights: map[trampoline.PartID]int32{
			partOne: 100,
			partTwo: 100,
		},
	}
	service.HodlUnsubscribeAll(matching)
	require.NotContains(t, service.groups[cleanupID].subscribers, partOne)
	require.NotContains(t, service.groups[cleanupID].heights, partOne)
	require.Contains(t, service.groups[cleanupID].subscribers, partTwo)

	id := trampoline.RelayID{
		PaymentHash: [32]byte{6},
		PaymentAddr: [32]byte{7},
	}
	machine, err := trampoline.NewRelayMachine(
		service.cfg.RelayConfig, id, 1_000, 900, 110, 100,
	)
	require.NoError(t, err)
	part := trampoline.PartID{ChannelID: 8, HTLCID: 9}
	_, err = machine.AddPart(trampoline.RelayPart{
		ID:         part,
		AmountMsat: 500,
		CLTV:       150,
	})
	require.NoError(t, err)
	resolutionChan := make(chan interface{}, 1)
	service.groups[id] = &relayGroup{
		machine:    machine,
		dispatcher: DispatchGuard{},
		subscribers: map[trampoline.PartID]chan<- interface{}{
			part: resolutionChan,
		},
		heights: map[trampoline.PartID]int32{part: 100},
	}
	service.resolveFailure(id, &lnwire.FailUnknownNextTrampoline{})
	event := receive(t, resolutionChan)
	failure, ok := event.(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	require.Equal(t, lnwire.CodeUnknownNextTrampoline,
		failure.Failure.Code())
	require.NotContains(t, service.groups, id)

	introductionPrivateKey, _ := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{0x54}, 32),
	)
	pubkeyIntroduction := lnwire.PubkeyIntro{
		Pubkey: introductionPrivateKey.PubKey(),
	}
	resolved, err := service.resolveIntroductionNode(&pubkeyIntroduction)
	require.NoError(t, err)
	require.True(t, resolved.IsEqual(pubkeyIntroduction.Pubkey))

	sciddirIntroduction, err := lnwire.NewSciddirIntro(0, [8]byte{1})
	require.NoError(t, err)
	_, err = service.resolveIntroductionNode(sciddirIntroduction)
	require.Error(t, err)
	service.cfg.ResolveIntroductionNode = func(
		lnwire.IntroductionNode) (*btcec.PublicKey, error) {

		return pubkeyIntroduction.Pubkey, nil
	}
	resolved, err = service.resolveIntroductionNode(sciddirIntroduction)
	require.NoError(t, err)
	require.True(t, resolved.IsEqual(pubkeyIntroduction.Pubkey))

	missingID := trampoline.RelayID{PaymentHash: [32]byte{0xaa}}
	service.runDownstream(missingID)
	service.RequestStop()
	service.runDownstream(missingID)
}

// TestCollectingGroupSurvivesRestart verifies that replayed outer HTLCs
// reconstruct subscribers without peeling the inner onion twice, while a new
// shard with an independently randomized equivalent onion is authenticated.
func TestCollectingGroupSurvivesRestart(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processed := &sphinx.ProcessedPacket{
		Action: sphinx.MoreHops,
		Payload: sphinx.HopPayload{
			Payload: encodedPayload(t, &trampoline.Payload{
				AmountToForward: &amount,
				OutgoingCLTV:    &cltv,
				OutgoingNodeID:  nextKey.PubKey(),
			}),
		},
		NextPacket: packet,
	}
	store := NewMemoryStore()
	firstProcessor := &mockOnionProcessor{processed: processed}
	firstSender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	firstService := testServiceWithStore(
		t, firstProcessor, firstSender, registry, store,
	)

	firstHodl := make(chan interface{}, 1)
	first := testHtlc(packet, 400, 1_100, 1, firstHodl)
	resolution, err := firstService.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)
	require.Equal(t, 1, firstProcessor.callCount())
	firstService.Stop()

	secondProcessor := &mockOnionProcessor{processed: processed}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x55}, 32))
	secondSender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
		preimage: preimage,
	}
	secondService := testServiceWithStore(
		t, secondProcessor, secondSender, registry, store,
	)

	replayedHodl := make(chan interface{}, 1)
	first.HodlChan = replayedHodl
	resolution, err = secondService.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)

	secondHodl := make(chan interface{}, 1)
	second := testHtlc(
		distinctTestPacket(t), 700, 1_100, 2, secondHodl,
	)
	resolution, err = secondService.NotifyTrampolineHtlc(second)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, secondSender.payments)

	for _, hodl := range []chan interface{}{replayedHodl, secondHodl} {
		event := receive(t, hodl)
		settle, ok := event.(*invoices.HtlcSettleResolution)
		require.True(t, ok)
		require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
	}
	require.Equal(t, 1, secondProcessor.callCount())
}

// TestPayingGroupReconcilesAfterRestart verifies that a relay which restarts
// after dispatching its downstream payment subscribes to the durable payment
// result and resolves the replayed upstream part exactly once.
func TestPayingGroupReconcilesAfterRestart(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	payloadBytes := encodedPayload(t, &trampoline.Payload{
		AmountToForward: &amount,
		OutgoingCLTV:    &cltv,
		OutgoingNodeID:  nextKey.PubKey(),
	})
	onionBytes, err := trampoline.EncodeOnionPacket(packet)
	require.NoError(t, err)

	htlc := testHtlc(packet, 1_100, 1_100, 1, nil)
	id := trampoline.RelayID{
		PaymentHash: htlc.PaymentHash,
		PaymentAddr: htlc.MPP.PaymentAddr(),
	}
	config := trampoline.RelayConfig{
		MaxParts:           4,
		MaxAttempts:        1,
		HoldBlocks:         40,
		RecoveryBlocks:     6,
		ExpirySafetyBlocks: 1,
		MaxReorgDepth:      3,
	}
	machine, err := trampoline.NewRelayMachine(
		config, id, 1_100, amount, cltv, 100,
	)
	require.NoError(t, err)
	_, err = machine.AddPart(trampoline.RelayPart{
		ID: trampoline.PartID{
			ChannelID: htlc.CircuitKey.ChanID.ToUint64(),
			HTLCID:    htlc.CircuitKey.HtlcID,
		},
		AmountMsat: 1_100,
		CLTV:       htlc.Expiry,
	})
	require.NoError(t, err)
	_, err = machine.BeginAttempt()
	require.NoError(t, err)

	store := NewMemoryStore()
	require.NoError(t, store.Put(id, StoredGroup{
		OnionBytes:   onionBytes,
		PayloadBytes: payloadBytes,
		NextOnion:    onionBytes,
		Role:         trampoline.RoleClearIntermediate,
		OuterMPP:     true,
		Snapshot:     machine.Snapshot(),
	}))

	processor := &mockOnionProcessor{
		err: errors.New("recovery must not peel the inner onion"),
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x77}, 32))
	reconciled := make(chan lntypes.Hash, 1)
	base := testServiceWithStore(
		t, processor, sender, registry, nil,
	)
	base.Stop()
	cfg := base.cfg
	cfg.Store = store
	cfg.PayTimeout = 10 * time.Millisecond
	cfg.PaymentReconciler = func(ctx context.Context,
		hash lntypes.Hash) ([32]byte, error) {

		reconciled <- hash
		select {
		case <-time.After(50 * time.Millisecond):
			return preimage, nil

		case <-ctx.Done():
			return [32]byte{}, ctx.Err()
		}
	}
	service, err := NewService(cfg)
	require.NoError(t, err)
	t.Cleanup(service.Stop)
	service.Start()
	require.Equal(t, htlc.PaymentHash, receive(t, reconciled))
	require.Eventually(t, func() bool {
		stored, err := store.Load(id)

		return err == nil && stored != nil && stored.Preimage != nil
	}, time.Second, 10*time.Millisecond)

	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	settle, ok := resolution.(*invoices.HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
	require.Zero(t, processor.callCount())

	stored, err := store.Load(id)
	require.NoError(t, err)
	require.NotNil(t, stored.Preimage)
	require.Equal(t, preimage, *stored.Preimage)
	require.Equal(t, trampoline.RelaySucceeded, stored.Snapshot.State)
}

// TestReadyGroupConcurrentReplaysDispatchOnce verifies that startup resumes a
// durably ready group before an upstream replay, and concurrent replayed parts
// cannot launch duplicate downstream payments or fail the held upstream HTLC.
func TestReadyGroupConcurrentReplaysDispatchOnce(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	payloadBytes := encodedPayload(t, &trampoline.Payload{
		AmountToForward: &amount,
		OutgoingCLTV:    &cltv,
		OutgoingNodeID:  nextKey.PubKey(),
	})
	onionBytes, err := trampoline.EncodeOnionPacket(packet)
	require.NoError(t, err)

	htlc := testHtlc(packet, 1_100, 1_100, 1, nil)
	id := trampoline.RelayID{
		PaymentHash: htlc.PaymentHash,
		PaymentAddr: htlc.MPP.PaymentAddr(),
	}
	config := trampoline.RelayConfig{
		MaxParts:           4,
		MaxAttempts:        1,
		HoldBlocks:         40,
		RecoveryBlocks:     6,
		ExpirySafetyBlocks: 1,
		MaxReorgDepth:      3,
	}
	machine, err := trampoline.NewRelayMachine(
		config, id, 1_100, amount, cltv, 100,
	)
	require.NoError(t, err)
	_, err = machine.AddPart(trampoline.RelayPart{
		ID: trampoline.PartID{
			ChannelID: htlc.CircuitKey.ChanID.ToUint64(),
			HTLCID:    htlc.CircuitKey.HtlcID,
		},
		AmountMsat: 1_100,
		CLTV:       htlc.Expiry,
	})
	require.NoError(t, err)

	store := NewMemoryStore()
	require.NoError(t, store.Put(id, StoredGroup{
		OnionBytes:   onionBytes,
		PayloadBytes: payloadBytes,
		NextOnion:    onionBytes,
		Role:         trampoline.RoleClearIntermediate,
		OuterMPP:     true,
		Snapshot:     machine.Snapshot(),
	}))

	sender := &countingPaymentSender{
		started: make(chan struct{}, 20),
	}
	service := testServiceWithStore(
		t, &mockOnionProcessor{
			err: errors.New("ready replay must not peel the onion"),
		}, sender, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		}, store,
	)
	service.Start()
	receive(t, sender.started)

	hodl := make(chan interface{}, 1)
	htlc.HodlChan = hodl
	const replays = 20
	var wg sync.WaitGroup
	type notifyResult struct {
		resolution invoices.HtlcResolution
		err        error
	}
	results := make(chan notifyResult, replays)
	for range replays {
		wg.Add(1)
		go func() {
			defer wg.Done()

			notify := service.NotifyTrampolineHtlc
			resolution, notifyErr := notify(htlc)
			results <- notifyResult{
				resolution: resolution,
				err:        notifyErr,
			}
		}()
	}
	wg.Wait()
	close(results)
	for result := range results {
		require.NoError(t, result.err)
		require.Nil(t, result.resolution)
	}
	require.Never(t, func() bool {
		return sender.callCount() != 1
	}, 100*time.Millisecond, 5*time.Millisecond)
	select {
	case event := <-hodl:
		t.Fatalf("ready replay resolved upstream early: %T", event)
	default:
	}
}

// TestCancelingGroupReissuesCancelAndReconcile verifies that startup reissues
// both side effects without waiting for an upstream replay after the durable
// canceling transition commits.
func TestCancelingGroupReissuesCancelAndReconcile(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	payloadBytes := encodedPayload(t, &trampoline.Payload{
		AmountToForward: &amount,
		OutgoingCLTV:    &cltv,
		OutgoingNodeID:  nextKey.PubKey(),
	})
	onionBytes, err := trampoline.EncodeOnionPacket(packet)
	require.NoError(t, err)

	htlc := testHtlc(packet, 1_100, 1_100, 1, nil)
	id := trampoline.RelayID{
		PaymentHash: htlc.PaymentHash,
		PaymentAddr: htlc.MPP.PaymentAddr(),
	}
	config := trampoline.RelayConfig{
		MaxParts:           4,
		MaxAttempts:        1,
		HoldBlocks:         40,
		RecoveryBlocks:     6,
		ExpirySafetyBlocks: 1,
		MaxReorgDepth:      3,
	}
	machine, err := trampoline.NewRelayMachine(
		config, id, 1_100, amount, cltv, 100,
	)
	require.NoError(t, err)
	_, err = machine.AddPart(trampoline.RelayPart{
		ID: trampoline.PartID{
			ChannelID: htlc.CircuitKey.ChanID.ToUint64(),
			HTLCID:    htlc.CircuitKey.HtlcID,
		},
		AmountMsat: 1_100,
		CLTV:       htlc.Expiry,
	})
	require.NoError(t, err)
	_, err = machine.BeginAttempt()
	require.NoError(t, err)
	_, err = machine.ObserveHeight(140)
	require.NoError(t, err)
	require.Equal(t, trampoline.RelayCanceling, machine.Snapshot().State)

	store := NewMemoryStore()
	require.NoError(t, store.Put(id, StoredGroup{
		OnionBytes:   onionBytes,
		PayloadBytes: payloadBytes,
		NextOnion:    onionBytes,
		Role:         trampoline.RoleClearIntermediate,
		OuterMPP:     true,
		Snapshot:     machine.Snapshot(),
	}))
	peelErr := errors.New("canceling replay must not peel the onion")
	service := testServiceWithStore(
		t, &mockOnionProcessor{
			err: peelErr,
		}, &mockPaymentSender{
			payments: make(chan *routing.LightningPayment, 1),
		}, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		}, store,
	)

	canceled := make(chan lntypes.Hash, 1)
	service.cfg.PaymentCanceler = func(_ context.Context,
		hash lntypes.Hash) error {

		canceled <- hash

		return nil
	}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x72}, 32))
	reconciled := make(chan lntypes.Hash, 1)
	service.cfg.PaymentReconciler = func(_ context.Context,
		hash lntypes.Hash) ([32]byte, error) {

		reconciled <- hash

		return preimage, nil
	}
	service.Start()
	require.Equal(t, htlc.PaymentHash, receive(t, canceled))
	require.Equal(t, htlc.PaymentHash, receive(t, reconciled))
	require.Eventually(t, func() bool {
		stored, loadErr := store.Load(id)

		return loadErr == nil && stored != nil &&
			stored.Snapshot.State == trampoline.RelaySucceeded
	}, time.Second, 10*time.Millisecond)

	hodl := make(chan interface{}, 1)
	htlc.HodlChan = hodl
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	settle, ok := resolution.(*invoices.HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
	select {
	case event := <-hodl:
		t.Fatalf("terminal replay resolved asynchronously: %T", event)
	default:
	}
	stored, err := store.Load(id)
	require.NoError(t, err)
	require.Equal(t, trampoline.RelaySucceeded, stored.Snapshot.State)
}

// TestServiceStopPreservesPayingGroup verifies daemon shutdown only cancels
// the in-memory payment call. The durable relay remains reconcilable because
// the router resumes its underlying HTLC attempt on the next startup.
func TestServiceStopPreservesPayingGroup(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x46}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	store := NewMemoryStore()
	blocking := &blockingPaymentSender{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	firstService := testServiceWithStore(
		t, processor, blocking, registry, store,
	)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	id := trampoline.RelayID{
		PaymentHash: htlc.PaymentHash,
		PaymentAddr: htlc.MPP.PaymentAddr(),
	}
	resolution, err := firstService.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, blocking.started)

	firstService.Stop()
	receive(t, blocking.canceled)
	require.Never(t, func() bool {
		candidate, loadErr := store.Load(id)
		if loadErr != nil || candidate == nil {
			return true
		}

		return candidate.Snapshot.State != trampoline.RelayPaying ||
			candidate.FailureCode != 0
	}, 100*time.Millisecond, 5*time.Millisecond)
	stored, err := store.Load(id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, trampoline.RelayPaying, stored.Snapshot.State)
	require.Zero(t, stored.FailureCode)

	secondProcessor := &mockOnionProcessor{
		err: errors.New("recovery must not peel the inner onion"),
	}
	secondSender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	secondService := testServiceWithStore(
		t, secondProcessor, secondSender, registry, store,
	)
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x78}, 32))
	secondService.cfg.PaymentReconciler = func(
		_ context.Context, hash lntypes.Hash) ([32]byte, error) {

		require.Equal(t, htlc.PaymentHash, hash)

		return preimage, nil
	}

	replayedHodl := make(chan interface{}, 1)
	htlc.HodlChan = replayedHodl
	resolution, err = secondService.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)
	settle, ok := receive(t, replayedHodl).(*invoices.HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
	require.Zero(t, secondProcessor.callCount())
}

// TestWorkerAdmissionSerializedWithStop verifies that RequestStop rejects new
// work synchronously while WaitForShutdown can run after payment dependencies
// release admitted workers. This covers the launch boundary shared by
// dispatch, reconciliation, and cancellation workers.
func TestWorkerAdmissionSerializedWithStop(t *testing.T) {
	t.Parallel()

	service := testService(t, &mockOnionProcessor{},
		&mockPaymentSender{
			payments: make(chan *routing.LightningPayment, 1),
		}, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		})

	started := make(chan struct{})
	release := make(chan struct{})
	service.launchWorker(func() {
		close(started)
		<-release
	})
	receive(t, started)

	service.RequestStop()
	require.True(t, service.stopped())

	stopped := make(chan struct{})
	go func() {
		service.WaitForShutdown()
		close(stopped)
	}()
	require.Never(t, func() bool {
		select {
		case <-stopped:
			return true

		default:
			return false
		}
	}, 50*time.Millisecond, 5*time.Millisecond)
	close(release)
	receive(t, stopped)

	late := make(chan struct{}, 1)
	service.launchWorker(func() {
		late <- struct{}{}
	})
	select {
	case <-late:
		t.Fatal("relay worker started after Stop returned")

	case <-time.After(25 * time.Millisecond):
	}
}

func testHtlc(packet *sphinx.OnionPacket, amount,
	total lnwire.MilliSatoshi, htlcID uint64,
	hodl chan<- interface{}) htlcswitch.TrampolineHtlc {

	var hash lntypes.Hash
	copy(hash[:], bytes.Repeat([]byte{0x42}, 32))
	var addr [32]byte
	copy(addr[:], bytes.Repeat([]byte{0x2a}, 32))

	return htlcswitch.TrampolineHtlc{
		PaymentHash:   hash,
		Amount:        amount,
		Expiry:        120,
		CurrentHeight: 100,
		CircuitKey: models.CircuitKey{
			ChanID: lnwire.NewShortChanIDFromInt(9),
			HtlcID: htlcID,
		},
		HodlChan: hodl,
		Onion:    packet,
		MPP:      record.NewMPP(total, addr),
	}
}

// TestIntermediateMPPRelaysOnce verifies that the outer payment is held until
// complete, the inner onion is peeled once, and every part settles with the
// downstream preimage.
func TestIntermediateMPPRelaysOnce(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x55}, 32))
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
		preimage: preimage,
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	store := NewMemoryStore()
	service := testServiceWithStore(
		t, processor, sender, registry, store,
	)

	firstHodl := make(chan interface{}, 1)
	first := testHtlc(packet, 400, 1_100, 1, firstHodl)
	resolution, err := service.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)

	select {
	case <-sender.payments:
		t.Fatal("downstream payment started before MPP completion")
	default:
	}

	secondHodl := make(chan interface{}, 1)
	second := testHtlc(packet, 700, 1_100, 2, secondHodl)
	resolution, err = service.NotifyTrampolineHtlc(second)
	require.NoError(t, err)
	require.Nil(t, resolution)

	payment := receive(t, sender.payments)
	require.Equal(t, lnwire.MilliSatoshi(1_000), payment.Amount)
	require.Equal(t, lnwire.MilliSatoshi(100), payment.FeeLimit)
	require.Equal(t, uint32(20), payment.CltvLimit)
	require.Equal(t, uint16(10), payment.FinalCLTVDelta)
	require.Equal(t, uint32(3), payment.MaxParts)
	require.NotEmpty(t, payment.TrampolineOnion)

	for _, hodl := range []chan interface{}{firstHodl, secondHodl} {
		event := receive(t, hodl)
		settle, ok := event.(*invoices.HtlcSettleResolution)
		require.True(t, ok)
		require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
	}
	require.Equal(t, 1, processor.callCount())
}

// TestIntermediateSinglePartWithoutPaymentData verifies that the outer
// payment_data record is optional when the incoming trampoline payment is not
// multi-part.
func TestIntermediateSinglePartWithoutPaymentData(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x57}, 32))
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
		preimage: preimage,
	}
	service := testService(
		t, processor, sender, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		},
	)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	htlc.MPP = nil
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)
	payment := receive(t, sender.payments)
	require.Equal(t, lnwire.MilliSatoshi(amount), payment.Amount)
	settle, ok := receive(t, hodl).(*invoices.HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
}

// TestExactOnionReplayRevalidatesOuterShard verifies that byte-identical inner
// onions cannot bypass the outer MPP, path-key, fee, or CLTV invariants fixed
// by the first accepted shard.
func TestExactOnionReplayRevalidatesOuterShard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(*htlcswitch.TrampolineHtlc)
		failureType lnwire.FailureMessage
	}{
		{
			name: "mpp total",
			mutate: func(htlc *htlcswitch.TrampolineHtlc) {
				var addr [32]byte
				addr = htlc.MPP.PaymentAddr()
				htlc.MPP = record.NewMPP(1_200, addr)
			},
			failureType: &lnwire.FailTemporaryTrampolineFailure{},
		},
		{
			name: "outer path key",
			mutate: func(htlc *htlcswitch.TrampolineHtlc) {
				key, _ := btcec.PrivKeyFromBytes(
					bytes.Repeat([]byte{0x63}, 32),
				)
				htlc.CurrentPathKey = key.PubKey()
			},
			failureType: &lnwire.FailInvalidBlinding{},
		},
		{
			name: "relay cltv budget",
			mutate: func(htlc *htlcswitch.TrampolineHtlc) {
				htlc.Expiry = 119
			},
			failureType: &lnwire.
				FailTrampolineFeeOrExpiryInsufficient{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			packet := testPacket(t)
			nextKey, _ := btcec.PrivKeyFromBytes(
				bytes.Repeat([]byte{0x45}, 32),
			)
			amount := uint64(1_000)
			cltv := uint32(110)
			payload := &trampoline.Payload{
				AmountToForward: &amount,
				OutgoingCLTV:    &cltv,
				OutgoingNodeID:  nextKey.PubKey(),
			}
			encoded := encodedPayload(t, payload)
			hopPayload := sphinx.HopPayload{Payload: encoded}
			processor := &mockOnionProcessor{
				processed: &sphinx.ProcessedPacket{
					Action:     sphinx.MoreHops,
					Payload:    hopPayload,
					NextPacket: packet,
				},
			}
			sender := &countingPaymentSender{
				started: make(chan struct{}, 1),
			}
			service := testService(
				t, processor, sender, &mockInvoiceRegistry{
					calls: make(chan invoiceCall, 1),
				},
			)
			service.cfg.CLTVExpiryDelta = 10

			firstHodl := make(chan interface{}, 1)
			first := testHtlc(
				packet, 400, 1_100, 1, firstHodl,
			)
			resolution, err := service.NotifyTrampolineHtlc(first)
			require.NoError(t, err)
			require.Nil(t, resolution)

			secondHodl := make(chan interface{}, 1)
			second := testHtlc(packet, 700, 1_100, 2, secondHodl)
			test.mutate(&second)
			resolution, err = service.NotifyTrampolineHtlc(second)
			require.NoError(t, err)
			raw := resolution
			failure, ok := raw.(*invoices.HtlcErrorResolution)
			require.True(t, ok)
			require.IsType(t, test.failureType, failure.Failure)
			require.Zero(t, sender.callCount())
			require.Equal(t, 1, processor.callCount())
		})
	}
}

// TestPaymentHashCannotAliasRelayGroups verifies that distinct outer payment
// addresses cannot create two relay groups that share the downstream router's
// payment-hash identity.
func TestPaymentHashCannotAliasRelayGroups(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &countingPaymentSender{
		started: make(chan struct{}, 2),
	}
	service := testServiceWithStore(
		t, processor, sender, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		}, NewMemoryStore(),
	)

	firstHodl := make(chan interface{}, 1)
	first := testHtlc(packet, 1_100, 1_100, 1, firstHodl)
	resolution, err := service.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.started)

	secondHodl := make(chan interface{}, 1)
	second := testHtlc(packet, 1_100, 1_100, 2, secondHodl)
	var secondAddr [32]byte
	copy(secondAddr[:], bytes.Repeat([]byte{0x2b}, 32))
	second.MPP = record.NewMPP(1_100, secondAddr)
	resolution, err = service.NotifyTrampolineHtlc(second)
	require.NoError(t, err)
	failure, ok := resolution.(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	require.IsType(
		t, &lnwire.FailTemporaryTrampolineFailure{},
		failure.Failure,
	)
	require.Equal(t, 1, sender.callCount())
	select {
	case event := <-firstHodl:
		t.Fatalf("hash-alias attempt resolved first group: %T", event)
	default:
	}
}

// TestRejectedFirstPartLeavesNoGroup verifies that validation which fails
// after onion authentication does not consume memory, a payment-hash claim,
// or a durable group record.
func TestRejectedFirstPartLeavesNoGroup(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	store := NewMemoryStore()
	service := testServiceWithStore(
		t, processor, &mockPaymentSender{
			payments: make(chan *routing.LightningPayment, 1),
		}, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		}, store,
	)

	htlc := testHtlc(packet, 0, 1_100, 1, nil)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.IsType(t, &invoices.HtlcErrorResolution{}, resolution)
	require.Empty(t, service.groups)
	require.Zero(t, service.activeGroups)
	require.Empty(t, store.groups)
	require.Empty(t, store.hashToID)
}

// TestActiveRelayGroupBound verifies that unique remote hashes cannot create
// more concurrently held groups than the configured service bound.
func TestActiveRelayGroupBound(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	store := NewMemoryStore()
	service := testServiceWithStore(
		t, processor, &mockPaymentSender{
			payments: make(chan *routing.LightningPayment, 1),
		}, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		}, store,
	)
	service.cfg.MaxGroups = 1

	first := testHtlc(packet, 400, 1_100, 1, make(chan interface{}, 1))
	resolution, err := service.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)
	require.Equal(t, uint32(1), service.activeGroups)

	second := testHtlc(packet, 400, 1_100, 2, nil)
	second.PaymentHash[0] ^= 1
	var secondAddr [32]byte
	copy(secondAddr[:], bytes.Repeat([]byte{0x2c}, 32))
	second.MPP = record.NewMPP(1_100, secondAddr)
	resolution, err = service.NotifyTrampolineHtlc(second)
	require.NoError(t, err)
	require.IsType(t, &invoices.HtlcErrorResolution{}, resolution)
	require.Equal(t, uint32(1), service.activeGroups)
	require.Len(t, service.groups, 1)
	require.Len(t, store.groups, 1)
}

// TestActiveRelayGroupBoundSurvivesRestart verifies that durable collecting
// groups continue to consume admission capacity after a daemon restart.
func TestActiveRelayGroupBoundSurvivesRestart(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processed := &sphinx.ProcessedPacket{
		Action: sphinx.MoreHops,
		Payload: sphinx.HopPayload{
			Payload: encodedPayload(t, &trampoline.Payload{
				AmountToForward: &amount,
				OutgoingCLTV:    &cltv,
				OutgoingNodeID:  nextKey.PubKey(),
			}),
		},
		NextPacket: packet,
	}
	store := NewMemoryStore()
	firstService := testServiceWithStore(
		t, &mockOnionProcessor{processed: processed},
		&mockPaymentSender{
			payments: make(chan *routing.LightningPayment, 1),
		},
		&mockInvoiceRegistry{calls: make(chan invoiceCall, 1)}, store,
	)
	firstService.cfg.MaxGroups = 1
	first := testHtlc(packet, 400, 1_100, 1, make(chan interface{}, 1))
	resolution, err := firstService.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)
	firstService.Stop()

	cfg := firstService.cfg
	cfg.OnionProcessor = &mockOnionProcessor{processed: processed}
	secondService, err := NewService(cfg)
	require.NoError(t, err)
	t.Cleanup(secondService.Stop)
	require.Equal(t, uint32(1), secondService.activeGroups)

	second := testHtlc(packet, 400, 1_100, 2, nil)
	second.PaymentHash[0] ^= 1
	var secondAddr [32]byte
	copy(secondAddr[:], bytes.Repeat([]byte{0x2c}, 32))
	second.MPP = record.NewMPP(1_100, secondAddr)
	resolution, err = secondService.NotifyTrampolineHtlc(second)
	require.NoError(t, err)
	require.IsType(t, &invoices.HtlcErrorResolution{}, resolution)
	require.Equal(t, uint32(1), secondService.activeGroups)

	resolution, err = secondService.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)
	require.Equal(t, uint32(1), secondService.activeGroups)
}

// TestTerminalRelayTombstonesPrunedAtSafeHeight verifies that terminal replay
// records retain their payment-hash claims through the reorganization margin.
func TestTerminalRelayTombstonesPrunedAtSafeHeight(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	store := &failPutStore{MemoryStore: NewMemoryStore()}
	replay := testHtlc(packet, 400, 1_100, 1, nil)
	replay.Expiry = 500
	ids := make([]trampoline.RelayID, 3)
	ids[0] = trampoline.RelayID{
		PaymentHash: replay.PaymentHash,
		PaymentAddr: replay.MPP.PaymentAddr(),
	}
	for i := 1; i < len(ids); i++ {
		ids[i].PaymentHash[0] = byte(i + 1)
		ids[i].PaymentAddr[0] = byte(i + 11)
	}
	for i := range ids {
		require.NoError(t, store.Put(ids[i], StoredGroup{
			FailureCode: uint16(
				lnwire.CodeTemporaryTrampolineFailure,
			),
			Snapshot: trampoline.RelaySnapshot{
				ID:             ids[i],
				State:          trampoline.RelayFailed,
				ObservedHeight: uint32(100 + i),
			},
			PruneAfterHeight: uint32(116 + i),
		}))
	}

	base := testServiceWithStore(
		t, processor,
		&mockPaymentSender{
			payments: make(chan *routing.LightningPayment, 1),
		},
		&mockInvoiceRegistry{calls: make(chan invoiceCall, 1)}, nil,
	)
	base.Stop()
	cfg := base.cfg
	cfg.Store = store
	cfg.MaxGroups = 2
	service, err := NewService(cfg)
	require.NoError(t, err)
	t.Cleanup(service.Stop)

	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 3)
	oldest, err := store.Load(ids[0])
	require.NoError(t, err)
	require.NotNil(t, oldest)
	require.Equal(t, uint32(3), service.durableGroups)
	store.failNextPut()
	resolution, err := service.NotifyTrampolineHtlc(replay)
	require.ErrorContains(t, err, "injected relay store put failure")
	require.Nil(t, resolution)
	oldest, err = store.Load(ids[0])
	require.NoError(t, err)
	require.Equal(t, uint32(116), oldest.PruneAfterHeight)
	require.Equal(
		t, uint32(116), terminalPruneAfterHeight(t, service, ids[0]),
	)

	resolution, err = service.NotifyTrampolineHtlc(replay)
	require.NoError(t, err)
	require.IsType(t, &invoices.HtlcErrorResolution{}, resolution)
	require.Zero(t, service.activeGroups)
	oldest, err = store.Load(ids[0])
	require.NoError(t, err)
	require.Equal(t, uint32(503), oldest.PruneAfterHeight)
	require.Equal(
		t, uint32(503), terminalPruneAfterHeight(t, service, ids[0]),
	)

	fresh := testHtlc(packet, 400, 1_100, 2, nil)
	fresh.PaymentHash[0]++
	addr := fresh.MPP.PaymentAddr()
	addr[0]++
	fresh.MPP = record.NewMPP(fresh.MPP.TotalMsat(), addr)
	resolution, err = service.NotifyTrampolineHtlc(fresh)
	require.NoError(t, err)
	require.IsType(t, &invoices.HtlcErrorResolution{}, resolution)
	require.Zero(t, service.activeGroups)

	require.NoError(t, service.ObserveHeight(116))
	records, err = store.List()
	require.NoError(t, err)
	require.Len(t, records, 3)
	require.NoError(t, service.ObserveHeight(117))
	records, err = store.List()
	require.NoError(t, err)
	require.Len(t, records, 2)
	oldest, err = store.Load(ids[0])
	require.NoError(t, err)
	require.NotNil(t, oldest)
	require.Len(t, service.terminalIDs, 2)
	require.Len(t, service.terminalSet, 2)
	require.Equal(t, uint32(2), service.durableGroups)
	require.NoError(t, service.ObserveHeight(503))
	records, err = store.List()
	require.NoError(t, err)
	require.Empty(t, records)
	require.Empty(t, service.terminalIDs)
	require.Empty(t, service.terminalSet)
	require.Zero(t, service.durableGroups)
}

// TestLegacyTerminalTombstoneNeverPruned verifies that a record without a
// proven original CLTV horizon retains its payment-hash claim after replay.
func TestLegacyTerminalTombstoneNeverPruned(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	replay := testHtlc(packet, 400, 1_100, 1, nil)
	replay.Expiry = 500
	id := trampoline.RelayID{
		PaymentHash: replay.PaymentHash,
		PaymentAddr: replay.MPP.PaymentAddr(),
	}
	store := NewMemoryStore()
	require.NoError(t, store.Put(id, StoredGroup{
		FailureCode: uint16(lnwire.CodeTemporaryTrampolineFailure),
		Snapshot: trampoline.RelaySnapshot{
			ID:    id,
			State: trampoline.RelayFailed,
		},
	}))

	base := testServiceWithStore(
		t, &mockOnionProcessor{},
		&mockPaymentSender{
			payments: make(chan *routing.LightningPayment, 1),
		},
		&mockInvoiceRegistry{calls: make(chan invoiceCall, 1)}, nil,
	)
	base.Stop()
	cfg := base.cfg
	cfg.Store = store
	service, err := NewService(cfg)
	require.NoError(t, err)
	t.Cleanup(service.Stop)

	resolution, err := service.NotifyTrampolineHtlc(replay)
	require.NoError(t, err)
	require.IsType(t, &invoices.HtlcErrorResolution{}, resolution)
	stored, err := store.Load(id)
	require.NoError(t, err)
	require.Zero(t, stored.PruneAfterHeight)
	require.Zero(t, terminalPruneAfterHeight(t, service, id))

	require.NoError(t, service.ObserveHeight(^uint32(0)))
	stored, err = store.Load(id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, uint32(1), service.durableGroups)
}

// TestIntermediateMPPAcceptsDistinctEquivalentOnions verifies that MPP parts
// may use independently randomized inner onions when their authenticated
// first-hop instructions are identical.
func TestIntermediateMPPAcceptsDistinctEquivalentOnions(t *testing.T) {
	t.Parallel()

	firstPacket := testPacket(t)
	secondPacket := distinctTestPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processed := &sphinx.ProcessedPacket{
		Action: sphinx.MoreHops,
		Payload: sphinx.HopPayload{
			Payload: encodedPayload(t, &trampoline.Payload{
				AmountToForward: &amount,
				OutgoingCLTV:    &cltv,
				OutgoingNodeID:  nextKey.PubKey(),
			}),
		},
		NextPacket: firstPacket,
	}
	processor := &mockOnionProcessor{processed: processed}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x55}, 32))
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
		preimage: preimage,
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	store := NewMemoryStore()
	service := testServiceWithStore(
		t, processor, sender, registry, store,
	)

	firstHodl := make(chan interface{}, 1)
	first := testHtlc(firstPacket, 400, 1_100, 1, firstHodl)
	resolution, err := service.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)

	secondHodl := make(chan interface{}, 1)
	second := testHtlc(secondPacket, 700, 1_100, 2, secondHodl)
	resolution, err = service.NotifyTrampolineHtlc(second)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.payments)

	for _, hodl := range []chan interface{}{firstHodl, secondHodl} {
		settle, ok := receive(t, hodl).(*invoices.HtlcSettleResolution)
		require.True(t, ok)
		require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
	}
	require.Equal(t, 2, processor.callCount())
	require.Zero(t, service.activeGroups)
	require.Empty(t, service.groups)

	id := trampoline.RelayID{
		PaymentHash: first.PaymentHash,
		PaymentAddr: first.MPP.PaymentAddr(),
	}
	stored, err := store.Load(id)
	require.NoError(t, err)
	require.Equal(t, trampoline.RelaySucceeded, stored.Snapshot.State)
	require.Empty(t, stored.Snapshot.Parts)
	require.NotNil(t, stored.Preimage)
	require.Equal(t, uint32(123), stored.PruneAfterHeight)
	require.Equal(t, uint32(1), service.durableGroups)
}

// TestIntermediateMPPRejectsDistinctConflictingOnion verifies that a shard
// cannot change the authenticated trampoline instruction for an existing
// payment group.
func TestIntermediateMPPRejectsDistinctConflictingOnion(t *testing.T) {
	t.Parallel()

	firstPacket := testPacket(t)
	conflictingPacket := distinctTestPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	conflictingAmount := uint64(999)
	cltv := uint32(110)
	processed := func(forward uint64) *sphinx.ProcessedPacket {
		return &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &forward,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: firstPacket,
		}
	}
	processor := &mockOnionProcessor{
		processedQueue: []*sphinx.ProcessedPacket{
			processed(amount), processed(conflictingAmount),
		},
	}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x55}, 32))
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
		preimage: preimage,
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	firstHodl := make(chan interface{}, 1)
	first := testHtlc(firstPacket, 400, 1_100, 1, firstHodl)
	resolution, err := service.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)

	conflicting := testHtlc(
		conflictingPacket, 700, 1_100, 2, make(chan interface{}, 1),
	)
	resolution, err = service.NotifyTrampolineHtlc(conflicting)
	require.NoError(t, err)
	failure, ok := resolution.(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	require.IsType(
		t, &lnwire.FailTemporaryTrampolineFailure{}, failure.Failure,
	)

	secondHodl := make(chan interface{}, 1)
	second := testHtlc(firstPacket, 700, 1_100, 3, secondHodl)
	resolution, err = service.NotifyTrampolineHtlc(second)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.payments)

	for _, hodl := range []chan interface{}{firstHodl, secondHodl} {
		settle, ok := receive(t, hodl).(*invoices.HtlcSettleResolution)
		require.True(t, ok)
		require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
	}
	require.Equal(t, 2, processor.callCount())
}

// TestIntermediateForwardsOpaqueNestedFailure verifies a relay persists and
// returns the downstream reason after only the downstream outer path was
// peeled.
func TestIntermediateForwardsOpaqueNestedFailure(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x46}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	reason := bytes.Repeat(
		[]byte{0x71}, trampoline.FailurePacketSize,
	)
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
		err: &routing.OpaqueTrampolineError{
			Reason: reason,
		},
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	store := NewMemoryStore()
	service := testServiceWithStore(
		t, processor, sender, registry, store,
	)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.payments)

	nested, ok := receive(t, hodl).(*invoices.HtlcNestedErrorResolution)
	require.True(t, ok)
	require.Equal(t, lnwire.OpaqueReason(reason), nested.Reason)

	id := trampoline.RelayID{
		PaymentHash: htlc.PaymentHash,
		PaymentAddr: htlc.MPP.PaymentAddr(),
	}
	stored, err := store.Load(id)
	require.NoError(t, err)
	require.Equal(t, reason, stored.FailureReason)

	replayHodl := make(chan interface{}, 1)
	htlc.HodlChan = replayHodl
	resolution, err = service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	replayed, ok := resolution.(*invoices.HtlcNestedErrorResolution)
	require.True(t, ok)
	require.Equal(t, nested.Reason, replayed.Reason)
}

// TestObserveHeightEnforcesRelayDeadline verifies that live block progress
// persists the observed height and resolves every held part at the hold
// deadline.
func TestObserveHeightEnforcesRelayDeadline(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	store := NewMemoryStore()
	service := testServiceWithStore(
		t, processor, sender, registry, store,
	)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 400, 1_100, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)

	require.NoError(t, service.ObserveHeight(118))
	select {
	case <-hodl:
		t.Fatal("relay resolved before its hold deadline")
	default:
	}

	id := trampoline.RelayID{
		PaymentHash: htlc.PaymentHash,
		PaymentAddr: htlc.MPP.PaymentAddr(),
	}
	stored, err := store.Load(id)
	require.NoError(t, err)
	require.Equal(t, uint32(118), stored.Snapshot.ObservedHeight)
	require.Equal(t, trampoline.RelayCollecting, stored.Snapshot.State)

	require.NoError(t, service.ObserveHeight(119))
	failure, ok := receive(t, hodl).(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	require.IsType(
		t, &lnwire.FailTemporaryTrampolineFailure{},
		failure.Failure,
	)

	stored, err = store.Load(id)
	require.NoError(t, err)
	require.Equal(t, trampoline.RelayTimedOut, stored.Snapshot.State)
	require.Equal(
		t, uint16(lnwire.CodeTemporaryTrampolineFailure),
		stored.FailureCode,
	)

	require.NoError(t, service.ObserveHeight(120))
	select {
	case <-hodl:
		t.Fatal("terminal relay part resolved more than once")
	default:
	}
}

// TestObserveHeightRejectsDeepReorg verifies that a reorg beyond the local
// safety bound fails the group instead of aborting the block dispatcher.
func TestObserveHeightRejectsDeepReorg(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 400, 1_100, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)

	require.NoError(t, service.ObserveHeight(102))
	require.NoError(t, service.ObserveHeight(98))

	failure, ok := receive(t, hodl).(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	require.IsType(
		t, &lnwire.FailTemporaryTrampolineFailure{},
		failure.Failure,
	)
}

// TestRelayDeadlineCancelsDownstream verifies that a terminal block deadline
// cancels an in-flight downstream payment before resolving the upstream HTLC.
func TestRelayDeadlineCancelsDownstream(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &blockingPaymentSender{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.started)

	require.NoError(t, service.ObserveHeight(140))
	receive(t, sender.canceled)

	failure, ok := receive(t, hodl).(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	require.IsType(
		t, &lnwire.FailTemporaryTrampolineFailure{},
		failure.Failure,
	)
}

// TestRelayDeadlineAllowsConcurrentSettlement verifies that cancellation does
// not fail upstream parts when the in-flight downstream HTLC settles while
// the cancellation request is being applied.
func TestRelayDeadlineAllowsConcurrentSettlement(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x71}, 32))
	sender := &cancelSettlesPaymentSender{
		started:  make(chan struct{}),
		preimage: preimage,
	}
	store := NewMemoryStore()
	service := testServiceWithStore(
		t, processor, sender, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		}, store,
	)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	id := trampoline.RelayID{
		PaymentHash: htlc.PaymentHash,
		PaymentAddr: htlc.MPP.PaymentAddr(),
	}
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.started)

	require.NoError(t, service.ObserveHeight(140))
	settle, ok := receive(t, hodl).(*invoices.HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)

	stored, err := store.Load(id)
	require.NoError(t, err)
	require.Equal(t, trampoline.RelaySucceeded, stored.Snapshot.State)
}

// TestRelayCancellationFailureRetries verifies that cancellation plumbing
// errors keep upstream parts held and are retried on later block progress.
func TestRelayCancellationFailureRetries(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &blockingPaymentSender{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	service := testService(
		t, processor, sender, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		},
	)
	cancelCalls := make(chan struct{}, 2)
	var cancelMu sync.Mutex
	cancelAttempts := 0
	service.cfg.PaymentCanceler = func(context.Context,
		lntypes.Hash) error {

		cancelMu.Lock()
		defer cancelMu.Unlock()
		cancelAttempts++
		cancelCalls <- struct{}{}
		if cancelAttempts == 1 {
			return errors.New("injected cancellation failure")
		}

		return nil
	}

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.started)

	require.NoError(t, service.ObserveHeight(140))
	receive(t, cancelCalls)
	select {
	case event := <-hodl:
		t.Fatalf("failed cancellation resolved upstream: %T", event)
	case <-sender.canceled:
		t.Fatal("failed cancellation stopped downstream sender")
	default:
	}

	require.Eventually(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()

		for _, group := range service.groups {
			return !group.cancelRunning
		}

		return false
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, service.ObserveHeight(141))
	receive(t, cancelCalls)
	receive(t, sender.canceled)
	failure, ok := receive(t, hodl).(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	require.IsType(
		t, &lnwire.FailTemporaryTrampolineFailure{},
		failure.Failure,
	)
}

// TestFinalRecipientUsesInnerPaymentData verifies that a final inner onion is
// handed to the invoice registry with the inner secret, total, and metadata.
func TestFinalRecipientUsesInnerPaymentData(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	amount := uint64(1_000)
	cltv := uint32(110)
	var innerAddr [32]byte
	copy(innerAddr[:], bytes.Repeat([]byte{0x33}, 32))
	metadata := []byte("trampoline metadata")
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.ExitNode,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					PaymentData: record.NewMPP(
						lnwire.MilliSatoshi(amount),
						innerAddr,
					),
					PaymentMetadata: metadata,
				}),
			},
		},
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_000, 1_000, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)

	call := receive(t, registry.calls)
	require.Equal(t, htlc.PaymentHash, call.hash)
	require.Equal(t, htlc.Amount, call.amount)
	require.Equal(t, innerAddr, call.payload.MultiPath().PaymentAddr())
	require.Equal(
		t, lnwire.MilliSatoshi(amount),
		call.payload.MultiPath().TotalMsat(),
	)
	require.Equal(t, metadata, call.payload.Metadata())
	require.Equal(t, 1, processor.callCount())
}

// TestRecipientBlindedPathsAreForwarded verifies that a last trampoline node
// converts the authenticated type-22 paths into the router's native blinded
// path set without forwarding a peeled trampoline onion.
func TestRecipientBlindedPathsAreForwarded(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	introKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x61}, 32))
	blindingKey, _ := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{0x62}, 32),
	)
	introduction, err := lnwire.NewPubkeyIntro(introKey.PubKey())
	require.NoError(t, err)
	amount := uint64(1_000)
	cltv := uint32(110)
	paths := &trampoline.RecipientBlindedPaths{
		Paths: []trampoline.RecipientBlindedPath{{
			Path: lnwire.BlindedPath{
				IntroductionNode: introduction,
				BlindingPoint:    blindingKey.PubKey(),
				Hops: []lnwire.BlindedHop{{
					BlindedNodeID: introKey.PubKey(),
					EncryptedData: []byte{1, 2, 3},
				}},
			},
			PaymentInfo: bolt12.BlindedPayInfo{
				CltvExpiryDelta: 40,
				HtlcMaximumMsat: 10_000,
				Features: *lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadOptional,
				),
			},
		}},
	}
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.ExitNode,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward:       &amount,
					OutgoingCLTV:          &cltv,
					RecipientBlindedPaths: paths,
				}),
			},
		},
	}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x71}, 32))
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
		preimage: preimage,
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)

	payment := receive(t, sender.payments)
	require.NotNil(t, payment.BlindedPathSet)
	require.Equal(
		t, route.NewVertex(introKey.PubKey()), payment.Target,
	)
	require.True(t, payment.PaymentAddr.IsNone())
	require.Empty(t, payment.TrampolineOnion)
	require.Equal(t, uint16(40), payment.FinalCLTVDelta)

	settle, ok := receive(t, hodl).(*invoices.HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)
}

// TestBlindedIntroductionForwardsDerivedValues verifies that an introduction
// node derives the downstream amount, CLTV, node, and next path key.
func TestBlindedIntroductionForwardsDerivedValues(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	pathKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x72}, 32))
	nextNode, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x73}, 32))
	encrypted := encodeBlindedData(t, record.NewDummyHopRouteData(
		nextNode.PubKey(),
		record.PaymentRelayInfo{
			CltvExpiryDelta: 10,
			BaseFee:         100,
		},
		record.PaymentConstraints{
			MaxCltvExpiry:   200,
			HtlcMinimumMsat: 1,
		},
	))
	currentPathKey := pathKey.PubKey()
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					EncryptedRecipientData: encrypted,
					CurrentPathKey:         currentPathKey,
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)

	payment := receive(t, sender.payments)
	require.Equal(t, lnwire.MilliSatoshi(1_000), payment.Amount)
	require.Equal(t, lnwire.MilliSatoshi(100), payment.FeeLimit)
	require.Equal(t, uint16(10), payment.FinalCLTVDelta)
	require.Equal(
		t, route.NewVertex(nextNode.PubKey()), payment.Target,
	)
	require.Equal(t, pathKey.PubKey(), payment.TrampolinePathKey)
	require.NotEmpty(t, payment.TrampolineOnion)
	require.Nil(t, processor.processedPathKey())
}

// TestBlindedIntermediateUsesOuterPathKey verifies that an intermediate
// blinded node uses the path key carried in the outer onion.
func TestBlindedIntermediateUsesOuterPathKey(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	outerPathKey, _ := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{0x74}, 32),
	)
	nextNode, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x75}, 32))
	encrypted := encodeBlindedData(t, record.NewDummyHopRouteData(
		nextNode.PubKey(),
		record.PaymentRelayInfo{CltvExpiryDelta: 5},
		record.PaymentConstraints{
			MaxCltvExpiry:   200,
			HtlcMinimumMsat: 1,
		},
	))
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					EncryptedRecipientData: encrypted,
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_000, 1_000, 1, hodl)
	htlc.CurrentPathKey = outerPathKey.PubKey()
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)

	payment := receive(t, sender.payments)
	require.Equal(t, outerPathKey.PubKey(), processor.processedPathKey())
	require.Equal(t, outerPathKey.PubKey(), payment.TrampolinePathKey)
	require.Equal(
		t, route.NewVertex(nextNode.PubKey()), payment.Target,
	)
}

// TestBlindedDownstreamFailureIsReplaced verifies that every error received
// from the next node inside a blinded path is discarded and replaced with
// invalid_onion_blinding at both introduction and intermediate nodes.
func TestBlindedDownstreamFailureIsReplaced(t *testing.T) {
	t.Parallel()

	for _, introduction := range []bool{true, false} {
		name := "intermediate"
		if introduction {
			name = "introduction"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			packet := testPacket(t)
			pathKey, _ := btcec.PrivKeyFromBytes(
				bytes.Repeat([]byte{0x74}, 32),
			)
			nextNode, _ := btcec.PrivKeyFromBytes(
				bytes.Repeat([]byte{0x75}, 32),
			)
			relayInfo := record.PaymentRelayInfo{
				CltvExpiryDelta: 5,
			}
			encrypted := encodeBlindedData(
				t, record.NewDummyHopRouteData(
					nextNode.PubKey(), relayInfo,
					record.PaymentConstraints{
						MaxCltvExpiry:   200,
						HtlcMinimumMsat: 1,
					},
				),
			)
			payload := &trampoline.Payload{
				EncryptedRecipientData: encrypted,
			}
			if introduction {
				payload.CurrentPathKey = pathKey.PubKey()
			}
			encoded := encodedPayload(t, payload)
			hopPayload := sphinx.HopPayload{Payload: encoded}
			processor := &mockOnionProcessor{
				processed: &sphinx.ProcessedPacket{
					Action:     sphinx.MoreHops,
					Payload:    hopPayload,
					NextPacket: packet,
				},
			}
			reason := bytes.Repeat(
				[]byte{0x91}, trampoline.FailurePacketSize,
			)
			payments := make(
				chan *routing.LightningPayment, 1,
			)
			sender := &mockPaymentSender{
				payments: payments,
				err: &routing.OpaqueTrampolineError{
					Reason: reason,
				},
			}
			store := NewMemoryStore()
			service := testServiceWithStore(
				t, processor, sender, &mockInvoiceRegistry{
					calls: make(chan invoiceCall, 1),
				}, store,
			)

			hodl := make(chan interface{}, 1)
			htlc := testHtlc(packet, 1_000, 1_000, 1, hodl)
			if !introduction {
				htlc.CurrentPathKey = pathKey.PubKey()
			}
			resolution, err := service.NotifyTrampolineHtlc(htlc)
			require.NoError(t, err)
			require.Nil(t, resolution)
			receive(t, sender.payments)
			event := receive(t, hodl)
			failure, ok := event.(*invoices.HtlcErrorResolution)
			require.True(t, ok)
			expected := &lnwire.FailInvalidBlinding{}
			require.IsType(
				t, expected, failure.Failure,
			)

			id := trampoline.RelayID{
				PaymentHash: htlc.PaymentHash,
				PaymentAddr: htlc.MPP.PaymentAddr(),
			}
			stored, err := store.Load(id)
			require.NoError(t, err)
			require.Equal(
				t, uint16(lnwire.CodeInvalidBlinding),
				stored.FailureCode,
			)
			require.Empty(t, stored.FailureReason)
		})
	}
}

// TestBlindedIntermediateLocalFailureUsesInvalidBlinding verifies that a
// non-introduction node never reveals its local blinded-path parse failure.
func TestBlindedIntermediateLocalFailureUsesInvalidBlinding(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	pathKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x74}, 32))
	peelErr := errors.New("injected peel failure")
	service := testService(
		t, &mockOnionProcessor{err: peelErr},
		&mockPaymentSender{
			payments: make(chan *routing.LightningPayment, 1),
		}, &mockInvoiceRegistry{
			calls: make(chan invoiceCall, 1),
		},
	)
	htlc := testHtlc(packet, 1_000, 1_000, 1, nil)
	htlc.CurrentPathKey = pathKey.PubKey()
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	failure, ok := resolution.(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	require.IsType(
		t, &lnwire.FailInvalidBlinding{}, failure.Failure,
	)
}

// TestBlindedFinalSuppliesPathID verifies that the final blinded payload is
// delivered with its authenticated path ID and total amount.
func TestBlindedFinalSuppliesPathID(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	pathKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x76}, 32))
	pathID := bytes.Repeat([]byte{0x77}, chainhash.HashSize)
	encrypted := encodeBlindedData(
		t, record.NewFinalHopBlindedRouteData(nil, pathID),
	)
	amount := uint64(1_000)
	cltv := uint32(110)
	total := uint64(1_500)
	currentPathKey := pathKey.PubKey()
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.ExitNode,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward:        &amount,
					OutgoingCLTV:           &cltv,
					EncryptedRecipientData: encrypted,
					CurrentPathKey:         currentPathKey,
					TotalAmountMsat:        &total,
				}),
			},
		},
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_000, 1_000, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)

	call := receive(t, registry.calls)
	expectedTotal := lnwire.MilliSatoshi(total)
	require.Equal(t, expectedTotal, call.payload.TotalAmtMsat())
	require.Equal(t, pathID, call.payload.PathID()[:])
}

func encodeBlindedData(t *testing.T,
	data *record.BlindedRouteData) []byte {

	t.Helper()

	encoded, err := record.EncodeBlindedRouteData(data)
	require.NoError(t, err)

	return encoded
}

// TestOuterBudgetFailureReturnsQuote verifies that an insufficient outer
// amount is translated into the trampoline fee-or-expiry failure.
func TestOuterBudgetFailureReturnsQuote(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_001)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)
	service.cfg.FeeBaseMsat = 100
	service.cfg.FeeProportionalMillionths = 500
	service.cfg.CLTVExpiryDelta = 18

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_000, 1_000, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)

	failure, ok := resolution.(*invoices.HtlcErrorResolution)
	require.True(t, ok)
	rawFailure := failure.Failure
	quote, ok := rawFailure.(*lnwire.FailTrampolineFeeOrExpiryInsufficient)
	require.True(t, ok)
	require.Equal(t, uint32(100), quote.FeeBaseMsat)
	require.Equal(t, uint32(500), quote.FeeProportionalMillionths)
	require.Equal(t, uint16(18), quote.CltvExpiryDelta)
}

// TestDownstreamFailureResolvesEveryPart verifies that a failed downstream
// payment emits one terminal failure for each accepted incoming part.
func TestDownstreamFailureResolvesEveryPart(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	sender := &mockPaymentSender{
		payments: make(chan *routing.LightningPayment, 1),
		err:      errors.New("no route"),
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	service := testService(t, processor, sender, registry)

	hodl := make(chan interface{}, 2)
	first := testHtlc(packet, 400, 1_100, 1, hodl)
	second := testHtlc(packet, 700, 1_100, 2, hodl)
	resolution, err := service.NotifyTrampolineHtlc(first)
	require.NoError(t, err)
	require.Nil(t, resolution)
	resolution, err = service.NotifyTrampolineHtlc(second)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.payments)

	for range 2 {
		event := receive(t, hodl)
		failure, ok := event.(*invoices.HtlcErrorResolution)
		require.True(t, ok)
		require.IsType(
			t, &lnwire.FailTemporaryTrampolineFailure{},
			failure.Failure,
		)
	}
}

// TestDownstreamNoRouteReturnsUnknownNext verifies that a terminal pathfinding
// failure is translated into the trampoline-specific unknown-next failure.
func TestDownstreamNoRouteReturnsUnknownNext(t *testing.T) {
	t.Parallel()

	require.IsType(
		t, &lnwire.FailUnknownNextTrampoline{},
		failureFromPaymentError(paymentsdb.FailureReasonNoRoute),
	)
	require.IsType(
		t, &lnwire.FailTemporaryTrampolineFailure{},
		failureFromPaymentError(paymentsdb.FailureReasonTimeout),
	)
}

// TestDownstreamRetryResolvesOnce verifies a transient downstream execution
// error follows the durable retry transition and settles the held part only
// after the bounded replacement attempt succeeds.
func TestDownstreamRetryResolvesOnce(t *testing.T) {
	t.Parallel()

	packet := testPacket(t)
	nextKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x49}, 32))
	amount := uint64(1_000)
	cltv := uint32(110)
	processor := &mockOnionProcessor{
		processed: &sphinx.ProcessedPacket{
			Action: sphinx.MoreHops,
			Payload: sphinx.HopPayload{
				Payload: encodedPayload(t, &trampoline.Payload{
					AmountToForward: &amount,
					OutgoingCLTV:    &cltv,
					OutgoingNodeID:  nextKey.PubKey(),
				}),
			},
			NextPacket: packet,
		},
	}
	var preimage [32]byte
	copy(preimage[:], bytes.Repeat([]byte{0x5a}, 32))
	sender := &retryPaymentSender{
		payments: make(chan *routing.LightningPayment, 2),
		errors:   []error{errors.New("transient router failure")},
		preimage: preimage,
	}
	registry := &mockInvoiceRegistry{
		calls: make(chan invoiceCall, 1),
	}
	store := NewMemoryStore()
	service := testServiceWithStore(
		t, processor, sender, registry, store,
	)
	service.cfg.RelayConfig.MaxAttempts = 2

	hodl := make(chan interface{}, 1)
	htlc := testHtlc(packet, 1_100, 1_100, 1, hodl)
	resolution, err := service.NotifyTrampolineHtlc(htlc)
	require.NoError(t, err)
	require.Nil(t, resolution)
	receive(t, sender.payments)
	receive(t, sender.payments)

	settle, ok := receive(t, hodl).(*invoices.HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, lntypes.Preimage(preimage), settle.Preimage)

	id := trampoline.RelayID{
		PaymentHash: htlc.PaymentHash,
		PaymentAddr: htlc.MPP.PaymentAddr(),
	}
	stored, err := store.Load(id)
	require.NoError(t, err)
	require.Equal(t, uint16(2), stored.Snapshot.Attempts)
	require.Equal(t, trampoline.RelaySucceeded, stored.Snapshot.State)
}

// TestRetryablePaymentError classifies only bounded transient execution
// failures for relay-level retry.
func TestRetryablePaymentError(t *testing.T) {
	t.Parallel()

	reason := bytes.Repeat([]byte{0x42}, trampoline.FailurePacketSize)
	tests := []struct {
		err       error
		retryable bool
	}{
		{err: errors.New("transient"), retryable: true},
		{err: paymentsdb.FailureReasonError, retryable: true},
		{err: paymentsdb.FailureReasonNoRoute},
		{err: paymentsdb.FailureReasonPaymentDetails},
		{err: context.Canceled},
		{err: context.DeadlineExceeded},
		{err: &routing.OpaqueTrampolineError{Reason: reason}},
		{},
	}
	for _, test := range tests {
		require.Equal(t, test.retryable,
			retryablePaymentError(test.err))
	}
}

func receive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()

	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for trampoline event")
		var zero T
		return zero
	}
}

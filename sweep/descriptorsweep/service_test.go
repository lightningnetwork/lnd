package descriptorsweep

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
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
	"github.com/stretchr/testify/require"
)

type testKeyRing struct {
	keys map[keychain.KeyLocator]*btcec.PublicKey
}

func (k *testKeyRing) DeriveNextKey(keychain.KeyFamily) (
	keychain.KeyDescriptor, error) {

	return keychain.KeyDescriptor{}, fmt.Errorf("not implemented")
}

func (k *testKeyRing) DeriveKey(locator keychain.KeyLocator) (
	keychain.KeyDescriptor, error) {

	key, ok := k.keys[locator]
	if !ok {
		return keychain.KeyDescriptor{}, fmt.Errorf("key not found")
	}
	return keychain.KeyDescriptor{KeyLocator: locator, PubKey: key}, nil
}

type testSweeper struct {
	inputs chan input.Input
}

func (s *testSweeper) SweepInput(inp input.Input, _ sweep.Params) (
	chan sweep.Result, error) {

	s.inputs <- inp
	return make(chan sweep.Result, 1), nil
}

type restartSweeper struct {
	input  input.Input
	params sweep.Params
}

type failOnceStore struct {
	mu       sync.Mutex
	delegate recordStore
	failures int
}

func (s *failOnceStore) init() error {
	return s.delegate.init()
}

func (s *failOnceStore) put(record *storedRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures > 0 {
		s.failures--
		return errors.New("transient store failure")
	}
	return s.delegate.put(record)
}

func (s *failOnceStore) list() ([]*storedRecord, error) {
	return s.delegate.list()
}

type testBlockSource struct {
	mu sync.Mutex

	hashes       map[int64]chainhash.Hash
	blocks       map[chainhash.Hash]*wire.MsgBlock
	hashFailures map[int64]int
}

func newTestBlockSource() *testBlockSource {
	return &testBlockSource{
		hashes:       make(map[int64]chainhash.Hash),
		blocks:       make(map[chainhash.Hash]*wire.MsgBlock),
		hashFailures: make(map[int64]int),
	}
}

func (s *testBlockSource) failHash(height int64, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashFailures[height] = count
}

func (s *testBlockSource) add(height int64, block *wire.MsgBlock) {
	s.mu.Lock()
	defer s.mu.Unlock()

	hash := chainhash.Hash{byte(height), byte(height >> 8)}
	s.hashes[height] = hash
	s.blocks[hash] = block
}

func (s *testBlockSource) GetBlockHash(height int64) (*chainhash.Hash, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hashFailures[height] > 0 {
		s.hashFailures[height]--
		return nil, errors.New("transient block hash failure")
	}

	hash, ok := s.hashes[height]
	if !ok {
		return nil, fmt.Errorf("block %d not found", height)
	}
	return &hash, nil
}

func (s *testBlockSource) GetBlock(
	hash *chainhash.Hash) (*wire.MsgBlock, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	block, ok := s.blocks[*hash]
	if !ok {
		return nil, fmt.Errorf("block %v not found", hash)
	}
	return block.Copy(), nil
}

type readyTestNotifier struct {
	mu sync.Mutex

	epochCalls  int
	confCalls   map[string]int
	confTotal   int
	confCancel  int
	epochCancel int

	epochRegistered chan struct{}
	confRegistered  chan []byte
	confEvents      chan *chainntnfs.ConfirmationEvent
	confHeightHints []uint32
	blockFirstConf  <-chan struct{}
	blockEpochs     chan *chainntnfs.BlockEpoch
	confFailures    int
}

func (n *readyTestNotifier) failConfirmations(count int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.confFailures = count
}

func newReadyTestNotifier() *readyTestNotifier {
	notifier := &readyTestNotifier{
		confCalls:       make(map[string]int),
		epochRegistered: make(chan struct{}, 1),
		confRegistered:  make(chan []byte, 10),
		confEvents:      make(chan *chainntnfs.ConfirmationEvent, 10),
		blockEpochs:     make(chan *chainntnfs.BlockEpoch, 1),
	}
	notifier.blockEpochs <- &chainntnfs.BlockEpoch{Height: 100}
	return notifier
}

func (n *readyTestNotifier) RegisterConfirmationsNtfn(_ *chainhash.Hash,
	pkScript []byte, numConfs, heightHint uint32,
	_ ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	n.mu.Lock()
	n.confTotal++
	call := n.confTotal
	n.confCalls[string(pkScript)]++
	n.confHeightHints = append(n.confHeightHints, heightHint)
	if n.confFailures > 0 {
		n.confFailures--
		n.mu.Unlock()
		return nil, errors.New("transient confirmation registration failure")
	}
	n.mu.Unlock()

	n.confRegistered <- append([]byte(nil), pkScript...)
	if call == 1 && n.blockFirstConf != nil {
		<-n.blockFirstConf
	}

	event := chainntnfs.NewConfirmationEvent(numConfs, func() {
		n.mu.Lock()
		n.confCancel++
		n.mu.Unlock()
	})
	n.confEvents <- event

	return event, nil
}

func (n *readyTestNotifier) RegisterSpendNtfn(*wire.OutPoint, []byte,
	uint32) (*chainntnfs.SpendEvent, error) {

	return nil, errors.New("not implemented")
}

func (n *readyTestNotifier) RegisterBlockEpochNtfn(
	*chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	n.mu.Lock()
	n.epochCalls++
	n.mu.Unlock()
	n.epochRegistered <- struct{}{}

	return &chainntnfs.BlockEpochEvent{
		Epochs: n.blockEpochs,
		Cancel: func() {
			n.mu.Lock()
			n.epochCancel++
			n.mu.Unlock()
		},
	}, nil
}

func (n *readyTestNotifier) Start() error  { return nil }
func (n *readyTestNotifier) Started() bool { return true }
func (n *readyTestNotifier) Stop() error   { return nil }

func (n *readyTestNotifier) counts() (int, int, int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.epochCalls, n.confTotal, n.epochCancel, n.confCancel
}

func (n *readyTestNotifier) scriptCalls(pkScript []byte) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.confCalls[string(pkScript)]
}

func (n *readyTestNotifier) heightHints() []uint32 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]uint32(nil), n.confHeightHints...)
}

func (s *restartSweeper) SweepInput(inp input.Input, params sweep.Params) (
	chan sweep.Result, error) {

	s.input = inp
	s.params = params
	return make(chan sweep.Result, 1), nil
}

func newTestSigner(keys []*btcec.PrivateKey) input.Signer {
	return input.NewMockSigner(keys, &chaincfg.RegressionNetParams)
}

func testBackend(t *testing.T) kvdb.Backend {
	t.Helper()

	db, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "descriptor.db")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return db
}

func requireReceive[T any](t *testing.T, channel <-chan T) T {
	t.Helper()

	select {
	case value := <-channel:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel value")
		var zero T
		return zero
	}
}

func testDescriptor(t *testing.T, timeout uint32) (string, []byte,
	[]*btcec.PrivateKey) {

	t.Helper()
	keyA, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	keyB, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	preimage := bytes.Repeat([]byte{0x2a}, 32)
	hash := sha256.Sum256(preimage)
	desc := fmt.Sprintf(
		"wsh(or_i(and_v(v:pk(%x),sha256(%x)),"+
			"and_v(v:pk(%x),after(%d))))",
		keyA.PubKey().SerializeCompressed(), hash,
		keyB.PubKey().SerializeCompressed(), timeout,
	)

	return desc, preimage, []*btcec.PrivateKey{keyA, keyB}
}

func TestRegisterValidatesAndPersists(t *testing.T) {
	t.Parallel()

	descriptor, _, keys := testDescriptor(t, 500)
	locA := keychain.KeyLocator{Family: 1, Index: 2}
	locB := keychain.KeyLocator{Family: 1, Index: 3}
	keyRing := &testKeyRing{keys: map[keychain.KeyLocator]*btcec.PublicKey{
		locA: keys[0].PubKey(), locB: keys[1].PubKey(),
	}}
	db := testBackend(t)
	service, err := New(Config{
		DB:          db,
		Notifier:    &chainntnfs.MockChainNotifier{},
		KeyRing:     keyRing,
		Sweeper:     &testSweeper{inputs: make(chan input.Input, 1)},
		BlockSource: newTestBlockSource(),
		ChainParams: &chaincfg.RegressionNetParams,
		Ready:       make(chan struct{}),
	})
	require.NoError(t, err)

	record, err := service.Register(context.Background(), RegisterRequest{
		Descriptor: descriptor,
		KeyBindings: []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
			KeyLocator:    locA,
		}, {
			DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
			KeyLocator:    locB,
		}},
		ExpectedValue:   50_000,
		HeightHint:      100,
		Budget:          10_000,
		StartingFeeRate: fn.Some(chainfee.SatPerKWeight(1000)),
	})
	require.NoError(t, err)
	require.Equal(t, StatusRegistered, record.Status)
	require.NotEmpty(t, record.PkScript)
	require.NotEmpty(t, record.WitnessScript)
	require.Equal(t, btcutil.Amount(50_000), record.ExpectedValue)

	loaded, err := newStore(db).list()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, record.CanonicalDescriptor,
		loaded[0].CanonicalDescriptor)
	require.True(t, loaded[0].HasStartingFeeRate)
	require.Equal(t, chainfee.SatPerKWeight(1000),
		loaded[0].StartingFeeRate)

	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bad := other.PubKey().SerializeCompressed()
	_, err = service.Register(context.Background(), RegisterRequest{
		Descriptor: fmt.Sprintf("wsh(pk(%x))", bad),
		KeyBindings: []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", bad), KeyLocator: locA,
		}},
		ExpectedValue: 50_000,
		HeightHint:    100,
		Budget:        10_000,
	})
	require.ErrorContains(t, err, "does not match locator")
}

func TestRegisterRejectsZeroBudget(t *testing.T) {
	t.Parallel()

	descriptor, _, keys := testDescriptor(t, 500)
	locatorA := keychain.KeyLocator{Family: 1, Index: 1}
	locatorB := keychain.KeyLocator{Family: 1, Index: 2}
	service, err := New(Config{
		DB:       testBackend(t),
		Notifier: &chainntnfs.MockChainNotifier{},
		KeyRing: &testKeyRing{keys: map[keychain.KeyLocator]*btcec.PublicKey{
			locatorA: keys[0].PubKey(), locatorB: keys[1].PubKey(),
		}},
		Sweeper:     &testSweeper{inputs: make(chan input.Input, 1)},
		BlockSource: newTestBlockSource(),
		ChainParams: &chaincfg.RegressionNetParams,
		Ready:       make(chan struct{}),
	})
	require.NoError(t, err)

	_, err = service.Register(context.Background(), RegisterRequest{
		Descriptor: descriptor,
		KeyBindings: []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
			KeyLocator:    locatorA,
		}, {
			DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
			KeyLocator:    locatorB,
		}},
		ExpectedValue: 50_000,
		HeightHint:    100,
	})
	require.ErrorContains(t, err, "budget must be positive")
}

func TestRegisterValueAndLabelValidation(t *testing.T) {
	t.Parallel()

	service := &Service{}
	base := RegisterRequest{
		Descriptor:    "wsh(pk(02))",
		ExpectedValue: 10_000,
		HeightHint:    1,
		Budget:        1_000,
	}

	req := base
	req.ExpectedValue = 0
	_, err := service.Register(context.Background(), req)
	require.ErrorContains(t, err, "expected output value must be positive")

	req = base
	req.ExpectedValue = btcutil.MaxSatoshi + 1
	_, err = service.Register(context.Background(), req)
	require.ErrorContains(t, err, "maximum money")

	req = base
	req.Budget = req.ExpectedValue + 1
	_, err = service.Register(context.Background(), req)
	require.ErrorContains(t, err, "budget must not exceed expected")

	req = base
	req.Label = string(bytes.Repeat([]byte{'a'}, 501))
	_, err = service.Register(context.Background(), req)
	require.ErrorContains(t, err, "label must not exceed 500 bytes")

	idA := registrationID("wsh(pk(02))", nil, 0, 10_000)
	idB := registrationID("wsh(pk(02))", nil, 0, 10_001)
	require.NotEqual(t, idA, idB,
		"expected output value must be part of registration identity")
}

func TestRegisterRejectsTooManyConfirmations(t *testing.T) {
	t.Parallel()

	service := &Service{}
	_, err := service.Register(context.Background(), RegisterRequest{
		Descriptor:    "wsh(pk(02))",
		ExpectedValue: 1,
		HeightHint:    1,
		MinConfs:      chainntnfs.MaxNumConfs + 1,
		Budget:        1,
	})
	require.ErrorContains(t, err, "min confirmations must not exceed")
}

func TestRegisterBeforeNotifierReadiness(t *testing.T) {
	t.Parallel()

	descriptor, _, keys := testDescriptor(t, 500)
	locA := keychain.KeyLocator{Family: 11, Index: 1}
	locB := keychain.KeyLocator{Family: 11, Index: 2}
	notifier := newReadyTestNotifier()
	ready := make(chan struct{})
	service, err := New(Config{
		DB:       testBackend(t),
		Notifier: notifier,
		KeyRing: &testKeyRing{keys: map[keychain.KeyLocator]*btcec.PublicKey{
			locA: keys[0].PubKey(), locB: keys[1].PubKey(),
		}},
		Sweeper:     &testSweeper{inputs: make(chan input.Input, 1)},
		BlockSource: newTestBlockSource(),
		ChainParams: &chaincfg.RegressionNetParams,
		Ready:       ready,
	})
	require.NoError(t, err)
	require.NoError(t, service.Start())
	t.Cleanup(func() { require.NoError(t, service.Stop()) })

	record, err := service.Register(context.Background(), RegisterRequest{
		Descriptor: descriptor,
		KeyBindings: []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
			KeyLocator:    locA,
		}, {
			DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
			KeyLocator:    locB,
		}},
		ExpectedValue: 50_000,
		HeightHint:    100,
		Budget:        10_000,
	})
	require.NoError(t, err)
	require.Equal(t, StatusRegistered, record.Status)

	epochCalls, confCalls, _, _ := notifier.counts()
	require.Zero(t, epochCalls)
	require.Zero(t, confCalls)

	close(ready)
	requireReceive(t, notifier.epochRegistered)
	requireReceive(t, notifier.confRegistered)
	require.Eventually(t, func() bool {
		status, err := service.Get(record.ID)
		return err == nil && status.Status == StatusWatching
	}, time.Second, time.Millisecond)
	service.mu.RLock()
	require.Equal(t, uint32(100), service.bestHeight)
	service.mu.RUnlock()

	epochCalls, confCalls, _, _ = notifier.counts()
	require.Equal(t, 1, epochCalls)
	require.Equal(t, 1, confCalls)
	require.Equal(t, 1, notifier.scriptCalls(record.PkScript))
	require.NoError(t, service.Stop())
	_, _, epochCancel, confCancel := notifier.counts()
	require.Equal(t, 1, epochCancel)
	require.Equal(t, 1, confCancel)
}

func TestTransientWatchFailureRetries(t *testing.T) {
	t.Parallel()

	descriptor, _, keys := testDescriptor(t, 500)
	locA := keychain.KeyLocator{Family: 15, Index: 1}
	locB := keychain.KeyLocator{Family: 15, Index: 2}
	notifier := newReadyTestNotifier()
	notifier.failConfirmations(1)
	ready := make(chan struct{})
	service, err := New(Config{
		DB:       testBackend(t),
		Notifier: notifier,
		KeyRing: &testKeyRing{keys: map[keychain.KeyLocator]*btcec.PublicKey{
			locA: keys[0].PubKey(), locB: keys[1].PubKey(),
		}},
		Sweeper:     &testSweeper{inputs: make(chan input.Input, 1)},
		BlockSource: newTestBlockSource(),
		ChainParams: &chaincfg.RegressionNetParams,
		Ready:       ready,
	})
	require.NoError(t, err)
	service.retryInitial = time.Millisecond
	service.retryMax = 5 * time.Millisecond
	record, err := service.Register(context.Background(), RegisterRequest{
		Descriptor: descriptor,
		KeyBindings: []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
			KeyLocator:    locA,
		}, {
			DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
			KeyLocator:    locB,
		}},
		ExpectedValue: 50_000,
		HeightHint:    100,
		Budget:        10_000,
	})
	require.NoError(t, err)
	require.NoError(t, service.Start())
	t.Cleanup(func() { require.NoError(t, service.Stop()) })
	close(ready)
	requireReceive(t, notifier.epochRegistered)
	requireReceive(t, notifier.confRegistered)
	require.Eventually(t, func() bool {
		status, err := service.Get(record.ID)
		return err == nil && status.Status == StatusWatching &&
			notifier.scriptCalls(record.PkScript) == 2
	}, time.Second, time.Millisecond)
	status, err := service.Get(record.ID)
	require.NoError(t, err)
	require.NotEqual(t, StatusFailed, status.Status)
}

func TestReadinessDrainDoesNotDoubleWatch(t *testing.T) {
	t.Parallel()

	descriptorA, _, keysA := testDescriptor(t, 500)
	descriptorB, _, keysB := testDescriptor(t, 600)
	locA1 := keychain.KeyLocator{Family: 12, Index: 1}
	locA2 := keychain.KeyLocator{Family: 12, Index: 2}
	locB1 := keychain.KeyLocator{Family: 12, Index: 3}
	locB2 := keychain.KeyLocator{Family: 12, Index: 4}
	notifier := newReadyTestNotifier()
	releaseFirstWatch := make(chan struct{})
	notifier.blockFirstConf = releaseFirstWatch
	ready := make(chan struct{})
	service, err := New(Config{
		DB:       testBackend(t),
		Notifier: notifier,
		KeyRing: &testKeyRing{keys: map[keychain.KeyLocator]*btcec.PublicKey{
			locA1: keysA[0].PubKey(), locA2: keysA[1].PubKey(),
			locB1: keysB[0].PubKey(), locB2: keysB[1].PubKey(),
		}},
		Sweeper:     &testSweeper{inputs: make(chan input.Input, 1)},
		BlockSource: newTestBlockSource(),
		ChainParams: &chaincfg.RegressionNetParams,
		Ready:       ready,
	})
	require.NoError(t, err)

	binding := func(keys []*btcec.PrivateKey, first,
		second keychain.KeyLocator) []KeyBinding {

		return []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
			KeyLocator:    first,
		}, {
			DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
			KeyLocator:    second,
		}}
	}
	recordA, err := service.Register(context.Background(), RegisterRequest{
		Descriptor:    descriptorA,
		KeyBindings:   binding(keysA, locA1, locA2),
		ExpectedValue: 50_000,
		HeightHint:    100,
		Budget:        10_000,
	})
	require.NoError(t, err)
	require.NoError(t, service.Start())
	t.Cleanup(func() { require.NoError(t, service.Stop()) })
	close(ready)
	requireReceive(t, notifier.epochRegistered)
	requireReceive(t, notifier.confRegistered)

	type registerResult struct {
		record *Record
		err    error
	}
	registered := make(chan registerResult, 1)
	go func() {
		record, err := service.Register(
			context.Background(), RegisterRequest{
				Descriptor:    descriptorB,
				KeyBindings:   binding(keysB, locB1, locB2),
				ExpectedValue: 50_000,
				HeightHint:    100,
				Budget:        10_000,
			},
		)
		registered <- registerResult{record: record, err: err}
	}()

	close(releaseFirstWatch)
	result := requireReceive(t, registered)
	require.NoError(t, result.err)
	requireReceive(t, notifier.confRegistered)
	require.Eventually(t, func() bool {
		service.mu.RLock()
		defer service.mu.RUnlock()
		return service.notifierReady
	}, time.Second, time.Millisecond)

	require.Equal(t, 1, notifier.scriptCalls(recordA.PkScript))
	require.Equal(t, 1, notifier.scriptCalls(result.record.PkScript))
	_, confCalls, _, _ := notifier.counts()
	require.Equal(t, 2, confCalls)
	require.NoError(t, service.Stop())
	_, _, epochCancel, confCancel := notifier.counts()
	require.Equal(t, 1, epochCancel)
	require.Equal(t, 2, confCancel)
}

func TestAddPreimageWaitsForReadiness(t *testing.T) {
	t.Parallel()

	descriptor, preimage, keys := testDescriptor(t, 500)
	locA := keychain.KeyLocator{Family: 14, Index: 1}
	locB := keychain.KeyLocator{Family: 14, Index: 2}
	notifier := newReadyTestNotifier()
	ready := make(chan struct{})
	sweeper := &testSweeper{inputs: make(chan input.Input, 1)}
	service, err := New(Config{
		DB:       testBackend(t),
		Notifier: notifier,
		KeyRing: &testKeyRing{keys: map[keychain.KeyLocator]*btcec.PublicKey{
			locA: keys[0].PubKey(), locB: keys[1].PubKey(),
		}},
		Sweeper:     sweeper,
		BlockSource: newTestBlockSource(),
		ChainParams: &chaincfg.RegressionNetParams,
		Ready:       ready,
	})
	require.NoError(t, err)
	record, err := service.Register(context.Background(), RegisterRequest{
		Descriptor: descriptor,
		KeyBindings: []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
			KeyLocator:    locA,
		}, {
			DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
			KeyLocator:    locB,
		}},
		ExpectedValue: 50_000,
		HeightHint:    100,
		Budget:        10_000,
	})
	require.NoError(t, err)

	service.mu.Lock()
	stored := service.records[record.ID]
	stored.OutPoint = &wire.OutPoint{Index: 7}
	stored.Value = int64(stored.ExpectedValue)
	stored.ConfirmationHeight = 110
	stored.Status = StatusWaiting
	require.NoError(t, newStore(service.cfg.DB).put(stored))
	service.mu.Unlock()

	require.NoError(t, service.Start())
	t.Cleanup(func() { require.NoError(t, service.Stop()) })
	updated, err := service.AddPreimage(
		context.Background(), record.ID, preimage,
	)
	require.NoError(t, err)
	require.Equal(t, StatusWaiting, updated.Status)
	select {
	case <-sweeper.inputs:
		t.Fatal("input offered before notifier and sweeper readiness")
	default:
	}
	service.mu.RLock()
	_, pending := service.pending[record.ID]
	service.mu.RUnlock()
	require.True(t, pending)

	close(ready)
	requireReceive(t, notifier.epochRegistered)
	_ = requireReceive(t, sweeper.inputs)
	require.Eventually(t, func() bool {
		status, err := service.Get(record.ID)
		return err == nil && status.Status == StatusSweeping
	}, time.Second, time.Millisecond)
}

func TestAddPreimageRejectsFailedRegistration(t *testing.T) {
	t.Parallel()

	descriptor, preimage, _ := testDescriptor(t, 500)
	desc, err := descriptors.NewDescriptor(descriptor)
	require.NoError(t, err)
	id := registrationID(desc.String(), nil, 0, 50_000)
	record := &storedRecord{
		Record: Record{
			ID: id, CanonicalDescriptor: desc.String(),
			Status: StatusFailed,
		},
		Preimages: make(map[string][]byte),
	}
	service := &Service{
		records: map[RegistrationID]*storedRecord{id: record},
		quit:    make(chan struct{}),
	}

	_, err = service.AddPreimage(context.Background(), id, preimage)
	require.ErrorContains(t, err, "already frozen")
	require.Empty(t, record.Preimages)
	require.NoError(t, service.trySweep(id))
	require.Equal(t, StatusFailed, record.Status)
}

func TestWrongValueRewatchesFromNextBlock(t *testing.T) {
	t.Parallel()

	descriptor, _, keys := testDescriptor(t, 500)
	locA := keychain.KeyLocator{Family: 13, Index: 1}
	locB := keychain.KeyLocator{Family: 13, Index: 2}
	notifier := newReadyTestNotifier()
	ready := make(chan struct{})
	blockSource := newTestBlockSource()
	service, err := New(Config{
		DB:       testBackend(t),
		Notifier: notifier,
		KeyRing: &testKeyRing{keys: map[keychain.KeyLocator]*btcec.PublicKey{
			locA: keys[0].PubKey(), locB: keys[1].PubKey(),
		}},
		Sweeper:     &testSweeper{inputs: make(chan input.Input, 1)},
		BlockSource: blockSource,
		ChainParams: &chaincfg.RegressionNetParams,
		Ready:       ready,
	})
	require.NoError(t, err)
	service.retryInitial = time.Millisecond
	service.retryMax = 5 * time.Millisecond
	record, err := service.Register(context.Background(), RegisterRequest{
		Descriptor: descriptor,
		KeyBindings: []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
			KeyLocator:    locA,
		}, {
			DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
			KeyLocator:    locB,
		}},
		ExpectedValue: 50_000,
		HeightHint:    100,
		Budget:        10_000,
	})
	require.NoError(t, err)
	require.NoError(t, service.Start())
	t.Cleanup(func() { require.NoError(t, service.Stop()) })
	close(ready)
	requireReceive(t, notifier.epochRegistered)
	requireReceive(t, notifier.confRegistered)
	firstEvent := requireReceive(t, notifier.confEvents)

	wrongValueTx := wire.NewMsgTx(2)
	wrongValueTx.AddTxOut(&wire.TxOut{
		Value: 49_999, PkScript: record.PkScript,
	})
	validTx := wire.NewMsgTx(2)
	validTx.AddTxOut(&wire.TxOut{
		Value: int64(record.ExpectedValue), PkScript: record.PkScript,
	})
	firstEvent.Confirmed <- &chainntnfs.TxConfirmation{
		// The notification points at the wrong-value transaction. Full-block
		// scanning must still find the valid transaction in the same block.
		Tx:          wrongValueTx,
		BlockHeight: 120,
		Block: &wire.MsgBlock{Transactions: []*wire.MsgTx{
			wrongValueTx, validTx,
		}},
	}
	require.Eventually(t, func() bool {
		status, err := service.Get(record.ID)
		return err == nil && status.OutPoint != nil &&
			status.OutPoint.Hash == validTx.TxHash()
	}, time.Second, time.Millisecond)
	require.Equal(t, []uint32{100}, notifier.heightHints())

	// Exercise the wrong-value-only case directly with a fresh watchable
	// record. It must advance and persist the scan cursor without
	// re-registering the cached script request.
	secondID := RegistrationID{99}
	service.mu.Lock()
	service.records[secondID] = &storedRecord{
		Record: Record{
			ID: secondID, CanonicalDescriptor: record.CanonicalDescriptor,
			KeyBindings:   append([]KeyBinding(nil), record.KeyBindings...),
			PkScript:      append([]byte(nil), record.PkScript...),
			WitnessScript: append([]byte(nil), record.WitnessScript...),
			ExpectedValue: 25_000, HeightHint: 90, MinConfs: 1,
			Budget: 1_000, Status: StatusWatching,
		},
		WatchHeight: 90,
		Preimages:   make(map[string][]byte),
	}
	require.NoError(t, newStore(service.cfg.DB).put(service.records[secondID]))
	service.bestHeight = 132
	service.mu.Unlock()
	require.NoError(t, service.watchOutput(secondID))
	requireReceive(t, notifier.confRegistered)
	wrongEvent := requireReceive(t, notifier.confEvents)
	wrongOnlyTx := wire.NewMsgTx(2)
	wrongOnlyTx.AddTxOut(&wire.TxOut{
		Value: 24_999, PkScript: record.PkScript,
	})
	emptyBlock := &wire.MsgBlock{}
	blockSource.add(131, emptyBlock)
	blockSource.failHash(131, 1)
	scanTx := wire.NewMsgTx(2)
	scanTx.AddTxOut(&wire.TxOut{
		Value: 25_000, PkScript: record.PkScript,
	})
	blockSource.add(132, &wire.MsgBlock{Transactions: []*wire.MsgTx{scanTx}})
	wrongEvent.Confirmed <- &chainntnfs.TxConfirmation{
		Tx: wrongOnlyTx, BlockHeight: 130,
		Block: &wire.MsgBlock{Transactions: []*wire.MsgTx{wrongOnlyTx}},
	}
	require.Equal(t, []uint32{100, 90}, notifier.heightHints())

	// The callback can arrive after later blocks have already matured. Scan
	// through the current known tip immediately rather than waiting for yet
	// another epoch.
	require.Eventually(t, func() bool {
		second, err := service.Get(secondID)
		return err == nil && second.OutPoint != nil &&
			second.OutPoint.Hash == scanTx.TxHash() &&
			second.ConfirmationHeight == 132 &&
			second.Status == StatusWaiting
	}, time.Second, time.Millisecond)
	second, err := service.Get(secondID)
	require.NoError(t, err)
	require.NotEqual(t, StatusFailed, second.Status)
}

func TestMultipleExactOutputsAreAmbiguous(t *testing.T) {
	t.Parallel()

	pkScript := []byte{txscript.OP_TRUE}
	txA := wire.NewMsgTx(2)
	txA.AddTxOut(&wire.TxOut{Value: 10_000, PkScript: pkScript})
	txB := wire.NewMsgTx(2)
	txB.AddTxOut(&wire.TxOut{Value: 10_000, PkScript: pkScript})
	_, err := findExactOutput(&wire.MsgBlock{
		Transactions: []*wire.MsgTx{txA, txB},
	}, pkScript, 10_000)
	require.ErrorContains(t, err, "multiple exact descriptor outputs")
}

func TestResumeLeavesFailedRegistrationTerminal(t *testing.T) {
	t.Parallel()

	sweeper := &restartSweeper{}
	outpoint := wire.OutPoint{Index: 4}
	failedWithoutOutput := &storedRecord{
		Record: Record{ID: RegistrationID{1}, Status: StatusFailed},
	}
	failedWithOutput := &storedRecord{
		Record: Record{
			ID: RegistrationID{2}, Status: StatusFailed,
			OutPoint: &outpoint,
		},
	}
	service := &Service{
		cfg: Config{Sweeper: sweeper},
		records: map[RegistrationID]*storedRecord{
			failedWithoutOutput.ID: failedWithoutOutput,
			failedWithOutput.ID:    failedWithOutput,
		},
		quit: make(chan struct{}),
	}

	require.NoError(t, service.resume(failedWithoutOutput.ID))
	require.NoError(t, service.resume(failedWithOutput.ID))
	require.Nil(t, sweeper.input)
	require.Equal(t, StatusFailed, failedWithoutOutput.Status)
	require.Equal(t, StatusFailed, failedWithOutput.Status)
}

func TestAddPreimageRejectsUnrelatedData(t *testing.T) {
	t.Parallel()

	descriptor, _, _ := testDescriptor(t, 500)
	desc, err := descriptors.NewDescriptor(descriptor)
	require.NoError(t, err)
	record := &storedRecord{
		Record: Record{
			ID:                  registrationID(desc.String(), nil, 0, 50_000),
			CanonicalDescriptor: desc.String(),
			Status:              StatusWaiting,
		},
		Preimages: make(map[string][]byte),
	}
	service := &Service{
		cfg:     Config{DB: testBackend(t)},
		records: map[RegistrationID]*storedRecord{record.ID: record},
		quit:    make(chan struct{}),
	}

	_, err = service.AddPreimage(
		context.Background(), record.ID, bytes.Repeat([]byte{0x99}, 32),
	)
	require.ErrorContains(t, err, "does not match")
	require.Empty(t, record.Preimages)
}

func TestStoreRejectsUnknownVersion(t *testing.T) {
	t.Parallel()

	db := testBackend(t)
	storage := newStore(db)
	require.NoError(t, storage.init())
	id := RegistrationID{1}
	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(descriptorSweepBucket)
		return bucket.Put(id[:], []byte{descriptorSweepStoreVersion + 1})
	}, func() {})
	require.NoError(t, err)

	_, err = storage.list()
	require.ErrorContains(t, err, "unknown descriptor sweep store version")
}

func TestPersistTransitionIsCopyOnWrite(t *testing.T) {
	t.Parallel()

	db := testBackend(t)
	storage := newStore(db)
	require.NoError(t, storage.init())
	id := RegistrationID{42}
	record := &storedRecord{
		Record:    Record{ID: id, Status: StatusWaiting},
		Preimages: map[string][]byte{},
	}
	require.NoError(t, storage.put(record))

	service := &Service{
		cfg: Config{DB: db},
		store: &failOnceStore{
			delegate: storage,
			failures: 1,
		},
		records: map[RegistrationID]*storedRecord{id: record},
		quit:    make(chan struct{}),
	}
	err := service.persistTransition(id, func(next *storedRecord) error {
		next.Status = StatusSweeping
		return nil
	})
	require.ErrorContains(t, err, "transient store failure")
	require.True(t, isRetryable(err))
	require.Equal(t, StatusWaiting, service.records[id].Status)

	require.NoError(t, service.persistTransition(
		id, func(next *storedRecord) error {
			next.Status = StatusSweeping
			return nil
		},
	))
	require.Equal(t, StatusSweeping, service.records[id].Status)
	loaded, err := storage.list()
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, StatusSweeping, loaded[0].Status)
}

func TestRestoreSweepKeepsFrozenBranch(t *testing.T) {
	t.Parallel()

	descriptor, preimage, keys := testDescriptor(t, 500)
	desc, err := descriptors.NewDescriptor(descriptor)
	require.NoError(t, err)
	_, pkScript, witnessScript, err := descriptorScripts(
		desc, &chaincfg.RegressionNetParams, 0,
	)
	require.NoError(t, err)
	locA := keychain.KeyLocator{Family: 4, Index: 1}
	locB := keychain.KeyLocator{Family: 4, Index: 2}
	hash := sha256.Sum256(preimage)
	locktime := uint32(500)
	outpoint := wire.OutPoint{Index: 1}
	record := &storedRecord{
		Record: Record{
			ID:                  RegistrationID{3},
			CanonicalDescriptor: desc.String(),
			KeyBindings: []KeyBinding{{
				DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
				KeyLocator:    locA,
			}, {
				DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
				KeyLocator:    locB,
			}},
			PkScript: pkScript, WitnessScript: witnessScript,
			OutPoint: &outpoint, Value: 50_000,
			Status: StatusSweeping, Budget: 10_000,
		},
		// A success preimage arriving in storage must not switch an already
		// frozen timeout plan during restart.
		Preimages: map[string][]byte{
			preimageKey("sha256", hash[:]): preimage,
		},
		PlanLocktime: &locktime,
	}
	deadline := int32(520)
	record.PlanDeadlineHeight = &deadline
	record.DeadlineDelta = 99
	sweeper := &restartSweeper{}
	service := &Service{
		cfg:        Config{Sweeper: sweeper},
		records:    map[RegistrationID]*storedRecord{record.ID: record},
		bestHeight: 800,
		quit:       make(chan struct{}),
	}

	require.NoError(t, service.restoreSweep(record.ID))
	require.NotNil(t, sweeper.input)
	require.Equal(t, locktime,
		valueOrZero(sweeper.input.(*descriptorInput).locktime))
	require.Equal(t, deadline,
		sweeper.params.DeadlineHeight.UnwrapOr(0))
	_, err = service.AddPreimage(context.Background(), record.ID, preimage)
	require.ErrorContains(t, err, "already frozen")
}

func TestFrozenPlanUsesOnlySelectedBranch(t *testing.T) {
	t.Parallel()

	descriptor, preimage, keys := testDescriptor(t, 500)
	desc, err := descriptors.NewDescriptor(descriptor)
	require.NoError(t, err)
	witnessScript, err := desc.ScriptCodeAt(0, 0)
	require.NoError(t, err)
	locA := keychain.KeyLocator{Family: 2, Index: 1}
	locB := keychain.KeyLocator{Family: 2, Index: 2}
	record := &storedRecord{
		Record: Record{
			CanonicalDescriptor: desc.String(),
			KeyBindings: []KeyBinding{{
				DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
				KeyLocator:    locA,
			}, {
				DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
				KeyLocator:    locB,
			}},
			WitnessScript: witnessScript,
		},
		Preimages: map[string][]byte{},
	}
	hash := sha256.Sum256(preimage)
	record.Preimages[preimageKey("sha256", hash[:])] = preimage

	assets := makeAssets(record, 499)
	plan, err := desc.PlanAt(0, 0, assets)
	require.NoError(t, err)
	require.Nil(t, plan.TxConstraints().AbsoluteLocktime,
		"preimage branch must not inherit timeout branch CLTV")

	locktime := uint32(500)
	record.Preimages = map[string][]byte{}
	record.PlanLocktime = &locktime
	timeoutPlan, err := desc.PlanAt(0, 0, makeFrozenAssets(record))
	require.NoError(t, err)
	require.Equal(t, locktime,
		*timeoutPlan.TxConstraints().AbsoluteLocktime)
}

func TestDescriptorInputUsesSelectedCSV(t *testing.T) {
	t.Parallel()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	keyString := fmt.Sprintf("%x", key.PubKey().SerializeCompressed())
	desc, err := descriptors.NewDescriptor(fmt.Sprintf(
		"wsh(and_v(v:pk(%s),older(5)))", keyString,
	))
	require.NoError(t, err)
	witnessScript, err := desc.ScriptCodeAt(0, 0)
	require.NoError(t, err)

	sequence := uint32(5)
	plan, err := desc.PlanAt(0, 0, descriptors.Assets{
		LookupEcdsaSig:   func(string) bool { return true },
		RelativeLocktime: &sequence,
	})
	require.NoError(t, err)
	constraints := plan.TxConstraints()
	require.Equal(t, uint32(2), constraints.MinTxVersion)
	require.Equal(t, sequence, *constraints.RelativeLocktime)
	require.False(t, constraints.RequiresNonFinalSequence)

	outpoint := wire.OutPoint{Index: 1}
	record := &storedRecord{Record: Record{
		CanonicalDescriptor: desc.String(),
		DerivationIndex:     0,
		KeyBindings: []KeyBinding{{
			DescriptorKey: keyString,
			KeyLocator: keychain.KeyLocator{
				Family: 17, Index: 2,
			},
		}},
		PkScript:           []byte{txscript.OP_TRUE},
		WitnessScript:      witnessScript,
		OutPoint:           &outpoint,
		Value:              50_000,
		ConfirmationHeight: 100,
	}, PlanSequence: constraints.RelativeLocktime}

	inp, err := newDescriptorInput(desc, plan, record)
	require.NoError(t, err)
	require.Equal(t, sequence, inp.BlocksToMaturity())
	require.Equal(t, uint32(100), inp.HeightHint())
	_, hasLocktime := inp.RequiredLockTime()
	require.False(t, hasLocktime)
}

func TestImmediatePlanWinsOverMatureTimeout(t *testing.T) {
	t.Parallel()

	descriptor, preimage, keys := testDescriptor(t, 500)
	desc, err := descriptors.NewDescriptor(descriptor)
	require.NoError(t, err)
	record := &storedRecord{
		Record: Record{KeyBindings: []KeyBinding{{
			DescriptorKey: fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed()),
		}, {
			DescriptorKey: fmt.Sprintf("%x", keys[1].PubKey().SerializeCompressed()),
		}}},
		Preimages: make(map[string][]byte),
	}
	hash := sha256.Sum256(preimage)
	record.Preimages[preimageKey("sha256", hash[:])] = preimage

	plan, err := desc.PlanAt(0, 0, makeAssets(record, 0))
	require.NoError(t, err)
	require.Nil(t, plan.TxConstraints().AbsoluteLocktime)

	// Exposing the mature timeout simultaneously would let Miniscript pick
	// its smaller witness, demonstrating why the service uses two passes.
	plan, err = desc.PlanAt(0, 0, makeAssets(record, 600))
	require.NoError(t, err)
	require.Equal(t, uint32(500),
		*plan.TxConstraints().AbsoluteLocktime)
}

func TestDescriptorWitness(t *testing.T) {
	t.Parallel()

	descriptor, preimage, keys := testDescriptor(t, 500)
	desc, err := descriptors.NewDescriptor(descriptor)
	require.NoError(t, err)
	witnessScript, err := desc.ScriptCodeAt(0, 0)
	require.NoError(t, err)
	hash := sha256.Sum256(preimage)
	keyA := fmt.Sprintf("%x", keys[0].PubKey().SerializeCompressed())
	assets := descriptors.Assets{
		LookupEcdsaSig: func(key string) bool { return key == keyA },
		LookupPreimage: func(string, []byte) bool { return true },
	}
	plan, err := desc.PlanAt(0, 0, assets)
	require.NoError(t, err)

	record := &storedRecord{
		Record: Record{
			KeyBindings: []KeyBinding{{DescriptorKey: keyA,
				KeyLocator: keychain.KeyLocator{Family: 3, Index: 4}}},
			WitnessScript: witnessScript,
		},
		Preimages: map[string][]byte{
			preimageKey("sha256", hash[:]): preimage,
		},
	}
	witnessType, err := newDescriptorWitnessType(desc, plan, record)
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{Sequence: 0})
	tx.AddTxOut(&wire.TxOut{Value: 5000,
		PkScript: []byte{txscript.OP_TRUE}})
	output := &wire.TxOut{Value: 10_000, PkScript: []byte{txscript.OP_0}}
	fetcher := txscript.NewCannedPrevOutputFetcher(
		output.PkScript, output.Value,
	)
	hashes := txscript.NewTxSigHashes(tx, fetcher)
	signDesc := &input.SignDescriptor{
		WitnessScript:     witnessScript,
		Output:            output,
		HashType:          txscript.SigHashAll,
		PrevOutputFetcher: fetcher,
		SigHashes:         hashes,
	}
	script, err := witnessType.craft(
		newTestSigner(keys),
		signDesc, tx, 0,
	)
	require.NoError(t, err)
	require.Equal(t, witnessScript, script.Witness[len(script.Witness)-1])
	require.Contains(t, script.Witness, preimage)

	bound, _, err := witnessType.SizeUpperBound()
	require.NoError(t, err)
	var actual int
	actual += wire.VarIntSerializeSize(uint64(len(script.Witness)))
	for _, element := range script.Witness {
		actual += wire.VarIntSerializeSize(uint64(len(element))) + len(element)
	}
	require.LessOrEqual(t, actual, int(bound))
}

package sweep

import (
	"errors"
	"fmt"
	"os"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	lnmock "github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	testLog = build.NewSubLogger("SWPR_TEST", nil)

	testMaxSweepAttempts = 3

	testMaxInputsPerTx = uint32(3)

	defaultFeePref = Params{Fee: FeeEstimateInfo{ConfTarget: 1}}

	errDummy = errors.New("dummy error")
)

type sweeperTestContext struct {
	t *testing.T

	sweeper   *UtxoSweeper
	notifier  *MockNotifier
	estimator *mockFeeEstimator
	backend   *mockBackend
	store     SweeperStore
	publisher *MockBumper

	publishChan   chan wire.MsgTx
	currentHeight int32
}

var (
	spendableInputs []*input.BaseInput
	testInputCount  atomic.Uint64

	testPubKey, _ = btcec.ParsePubKey([]byte{
		0x04, 0x11, 0xdb, 0x93, 0xe1, 0xdc, 0xdb, 0x8a,
		0x01, 0x6b, 0x49, 0x84, 0x0f, 0x8c, 0x53, 0xbc, 0x1e,
		0xb6, 0x8a, 0x38, 0x2e, 0x97, 0xb1, 0x48, 0x2e, 0xca,
		0xd7, 0xb1, 0x48, 0xa6, 0x90, 0x9a, 0x5c, 0xb2, 0xe0,
		0xea, 0xdd, 0xfb, 0x84, 0xcc, 0xf9, 0x74, 0x44, 0x64,
		0xf8, 0x2e, 0x16, 0x0b, 0xfa, 0x9b, 0x8b, 0x64, 0xf9,
		0xd4, 0xc0, 0x3f, 0x99, 0x9b, 0x86, 0x43, 0xf6, 0x56,
		0xb4, 0x12, 0xa3,
	})
)

func createTestInput(value int64, witnessType input.WitnessType) input.BaseInput {
	hash := chainhash.Hash{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		byte(testInputCount.Add(1))}

	input := input.MakeBaseInput(
		&wire.OutPoint{
			Hash: hash,
		},
		witnessType,
		&input.SignDescriptor{
			Output: &wire.TxOut{
				Value: value,
			},
			KeyDesc: keychain.KeyDescriptor{
				PubKey: testPubKey,
			},
		},
		0,
		nil,
	)

	return input
}

func init() {
	// Create a set of test spendable inputs.
	for i := 0; i < 20; i++ {
		input := createTestInput(int64(10000+i*500),
			input.CommitmentTimeLock)

		spendableInputs = append(spendableInputs, &input)
	}
}

func createSweeperTestContext(t *testing.T) *sweeperTestContext {
	notifier := NewMockNotifier(t)

	// Create new store.
	cdb, err := channeldb.MakeTestDB(t)
	require.NoError(t, err)

	var chain chainhash.Hash
	store, err := NewSweeperStore(cdb, &chain)
	require.NoError(t, err)

	backend := newMockBackend(t, notifier)
	backend.walletUtxos = []*lnwallet.Utxo{
		{
			Value:       btcutil.Amount(1_000_000),
			AddressType: lnwallet.WitnessPubKey,
		},
	}

	estimator := newMockFeeEstimator(10000, chainfee.FeePerKwFloor)

	aggregator := NewSimpleUtxoAggregator(
		estimator, DefaultMaxFeeRate.FeePerKWeight(),
		testMaxInputsPerTx,
	)

	// Create a mock fee bumper.
	mockBumper := &MockBumper{}
	t.Cleanup(func() {
		mockBumper.AssertExpectations(t)
	})

	ctx := &sweeperTestContext{
		notifier:      notifier,
		publishChan:   backend.publishChan,
		t:             t,
		estimator:     estimator,
		backend:       backend,
		store:         store,
		currentHeight: mockChainHeight,
		publisher:     mockBumper,
	}

	ctx.sweeper = New(&UtxoSweeperConfig{
		Notifier: notifier,
		Wallet:   backend,
		Store:    store,
		Signer:   &lnmock.DummySigner{},
		GenSweepScript: func() ([]byte, error) {
			script := make([]byte, input.P2WPKHSize)
			script[0] = 0
			script[1] = 20
			return script, nil
		},
		FeeEstimator:   estimator,
		MaxInputsPerTx: testMaxInputsPerTx,
		MaxFeeRate:     DefaultMaxFeeRate,
		Aggregator:     aggregator,
		Publisher:      mockBumper,
	})

	ctx.sweeper.Start()

	return ctx
}

func (ctx *sweeperTestContext) restartSweeper() {
	ctx.t.Helper()

	ctx.sweeper.Stop()
	ctx.sweeper = New(ctx.sweeper.cfg)
	ctx.sweeper.Start()
}

func (ctx *sweeperTestContext) finish(expectedGoroutineCount int) {
	// We assume that when finish is called, sweeper has finished all its
	// goroutines. This implies that the waitgroup is empty.
	signalChan := make(chan struct{})
	go func() {
		ctx.sweeper.wg.Wait()
		close(signalChan)
	}()

	// Simulate exits of the expected number of running goroutines.
	for i := 0; i < expectedGoroutineCount; i++ {
		ctx.sweeper.wg.Done()
	}

	// We now expect the Wait to succeed.
	select {
	case <-signalChan:
	case <-time.After(time.Second):
		pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)

		ctx.t.Fatalf("lingering goroutines detected after test " +
			"is finished")
	}

	// Restore waitgroup state to what it was before.
	ctx.sweeper.wg.Add(expectedGoroutineCount)

	// Stop sweeper.
	ctx.sweeper.Stop()

	// We should have consumed and asserted all published transactions in
	// our unit tests.
	ctx.assertNoTx()
	if !ctx.backend.isDone() {
		ctx.t.Fatal("unconfirmed txes remaining")
	}
}

func (ctx *sweeperTestContext) assertNoTx() {
	ctx.t.Helper()
	select {
	case <-ctx.publishChan:
		ctx.t.Fatalf("unexpected transactions published")
	default:
	}
}

func (ctx *sweeperTestContext) receiveTx() wire.MsgTx {
	ctx.t.Helper()

	// Every time we want to receive a tx, we send a new block epoch to the
	// sweeper to trigger a sweeping action.
	ctx.notifier.NotifyEpochNonBlocking(ctx.currentHeight + 1)

	var tx wire.MsgTx
	select {
	case tx = <-ctx.publishChan:
		return tx
	case <-time.After(5 * time.Second):
		pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)

		ctx.t.Fatalf("tx not published")
	}
	return tx
}

func (ctx *sweeperTestContext) expectResult(c chan Result, expected error) {
	ctx.t.Helper()
	select {
	case result := <-c:
		if result.Err != expected {
			ctx.t.Fatalf("expected %v result, but got %v",
				expected, result.Err,
			)
		}
	case <-time.After(defaultTestTimeout):
		ctx.t.Fatalf("no result received")
	}
}

func (ctx *sweeperTestContext) assertPendingInputs(inputs ...input.Input) {
	ctx.t.Helper()

	inputSet := make(map[wire.OutPoint]struct{}, len(inputs))
	for _, input := range inputs {
		inputSet[input.OutPoint()] = struct{}{}
	}

	inputsMap, err := ctx.sweeper.PendingInputs()
	if err != nil {
		ctx.t.Fatal(err)
	}
	if len(inputsMap) != len(inputSet) {
		ctx.t.Fatalf("expected %d pending inputs, got %d",
			len(inputSet), len(inputsMap))
	}
	for input := range inputsMap {
		if _, ok := inputSet[input]; !ok {
			ctx.t.Fatalf("found unexpected input %v", input)
		}
	}
}

// assertTxSweepsInputs ensures that the transaction returned within the value
// received from resultChan spends the given inputs.
func assertTxSweepsInputs(t *testing.T, sweepTx *wire.MsgTx,
	inputs ...input.Input) {

	t.Helper()

	if len(sweepTx.TxIn) != len(inputs) {
		t.Fatalf("expected sweep tx to contain %d inputs, got %d",
			len(inputs), len(sweepTx.TxIn))
	}
	m := make(map[wire.OutPoint]struct{}, len(inputs))
	for _, input := range inputs {
		m[input.OutPoint()] = struct{}{}
	}
	for _, txIn := range sweepTx.TxIn {
		if _, ok := m[txIn.PreviousOutPoint]; !ok {
			t.Fatalf("expected tx %v to spend input %v",
				txIn.PreviousOutPoint, sweepTx.TxHash())
		}
	}
}

// assertTxFeeRate asserts that the transaction was created with the given
// inputs and fee rate.
//
// NOTE: This assumes that transactions only have one output, as this is the
// only type of transaction the UtxoSweeper can create at the moment.
func assertTxFeeRate(t *testing.T, tx *wire.MsgTx,
	expectedFeeRate chainfee.SatPerKWeight, changePk []byte,
	inputs ...input.Input) {

	t.Helper()

	if len(tx.TxIn) != len(inputs) {
		t.Fatalf("expected %d inputs, got %d", len(tx.TxIn), len(inputs))
	}

	m := make(map[wire.OutPoint]input.Input, len(inputs))
	for _, input := range inputs {
		m[input.OutPoint()] = input
	}

	var inputAmt int64
	for _, txIn := range tx.TxIn {
		input, ok := m[txIn.PreviousOutPoint]
		if !ok {
			t.Fatalf("expected input %v to be provided",
				txIn.PreviousOutPoint)
		}
		inputAmt += input.SignDesc().Output.Value
	}
	outputAmt := tx.TxOut[0].Value

	fee := btcutil.Amount(inputAmt - outputAmt)
	_, estimator, err := getWeightEstimate(inputs, nil, 0, 0, changePk)
	require.NoError(t, err)

	txWeight := estimator.weight()

	expectedFee := expectedFeeRate.FeeForWeight(int64(txWeight))
	if fee != expectedFee {
		t.Fatalf("expected fee rate %v results in %v fee, got %v fee",
			expectedFeeRate, expectedFee, fee)
	}
}

// assertNumSweeps asserts that the expected number of sweeps has been found in
// the sweeper's store.
func assertNumSweeps(t *testing.T, sweeper *UtxoSweeper, num int) {
	err := wait.NoError(func() error {
		sweeps, err := sweeper.ListSweeps()
		if err != nil {
			return err
		}

		if len(sweeps) != num {
			return fmt.Errorf("want %d sweeps, got %d",
				num, len(sweeps))
		}

		return nil
	}, 5*time.Second)
	require.NoError(t, err, "timeout checking num of sweeps")
}

// TestSuccess tests the sweeper happy flow.
func TestSuccess(t *testing.T) {
	ctx := createSweeperTestContext(t)

	inp := spendableInputs[0]

	// Sweeping an input without a fee preference should result in an error.
	_, err := ctx.sweeper.SweepInput(inp, Params{
		Fee: &FeeEstimateInfo{},
	})
	require.ErrorIs(t, err, ErrNoFeePreference)

	// Mock the Broadcast method to succeed.
	bumpResultChan := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{{
				PreviousOutPoint: inp.OutPoint(),
			}},
		}

		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	})

	resultChan, err := ctx.sweeper.SweepInput(inp, defaultFeePref)
	require.NoError(t, err)

	sweepTx := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	// Mock a confirmed event.
	bumpResultChan <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx,
		FeeRate: 10,
		Fee:     100,
	}

	// Mine a block to confirm the sweep tx.
	ctx.backend.mine()

	select {
	case result := <-resultChan:
		if result.Err != nil {
			t.Fatalf("expected successful spend, but received "+
				"error %v instead", result.Err)
		}
		if result.Tx.TxHash() != sweepTx.TxHash() {
			t.Fatalf("expected sweep tx ")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no result received")
	}

	ctx.finish(1)
}

// TestDust asserts that inputs that are not big enough to raise above the dust
// limit, are held back until the total set does surpass the limit.
func TestDust(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweeping a single output produces a tx of 486 weight units. With the
	// test fee rate, the sweep tx will pay 4860 sat in fees.
	//
	// Create an input so that the output after paying fees is still
	// positive (400 sat), but less than the dust limit (537 sat) for the
	// sweep tx output script (P2WPKH).
	dustInput := createTestInput(5260, input.CommitmentTimeLock)

	_, err := ctx.sweeper.SweepInput(&dustInput, defaultFeePref)
	require.NoError(t, err)

	// No sweep transaction is expected now. The sweeper should recognize
	// that the sweep output will not be relayed and not generate the tx. It
	// isn't possible to attach a wallet utxo either, because the added
	// weight would create a negatively yielding transaction at this fee
	// rate.

	// Sweep another input that brings the tx output above the dust limit.
	largeInput := createTestInput(100000, input.CommitmentTimeLock)

	// Mock the Broadcast method to succeed.
	bumpResultChan := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: largeInput.OutPoint()},
				{PreviousOutPoint: dustInput.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	})

	_, err = ctx.sweeper.SweepInput(&largeInput, defaultFeePref)
	require.NoError(t, err)

	// The second input brings the sweep output above the dust limit. We
	// expect a sweep tx now.
	sweepTx := ctx.receiveTx()
	require.Len(t, sweepTx.TxIn, 2, "unexpected num of tx inputs")

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	// Mock a confirmed event.
	bumpResultChan <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.backend.mine()

	ctx.finish(1)
}

// TestWalletUtxo asserts that inputs that are not big enough to raise above the
// dust limit are accompanied by a wallet utxo to make them sweepable.
func TestWalletUtxo(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweeping a single output produces a tx of 439 weight units. At the
	// fee floor, the sweep tx will pay 439*253/1000 = 111 sat in fees.
	//
	// Create an input so that the output after paying fees is still
	// positive (183 sat), but less than the dust limit (537 sat) for the
	// sweep tx output script (P2WPKH).
	//
	// What we now expect is that the sweeper will attach a utxo from the
	// wallet. This increases the tx weight to 712 units with a fee of 180
	// sats. The tx yield becomes then 294-180 = 114 sats.
	dustInput := createTestInput(294, input.WitnessKeyHash)

	// Mock the Broadcast method to succeed.
	bumpResultChan := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: dustInput.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	})

	_, err := ctx.sweeper.SweepInput(
		&dustInput,
		Params{Fee: FeeEstimateInfo{FeeRate: chainfee.FeePerKwFloor}},
	)
	require.NoError(t, err)

	sweepTx := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.finish(1)
}

// TestNegativeInput asserts that no inputs with a negative yield are swept.
// Negative yield means that the value minus the added fee is negative.
func TestNegativeInput(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweep an input large enough to cover fees, so in any case the tx
	// output will be above the dust limit.
	largeInput := createTestInput(100000, input.CommitmentNoDelay)
	largeInputResult, err := ctx.sweeper.SweepInput(
		&largeInput, defaultFeePref,
	)
	require.NoError(t, err)

	// Sweep an additional input with a negative net yield. The weight of
	// the HtlcAcceptedRemoteSuccess input type adds more in fees than its
	// value at the current fee level.
	negInput := createTestInput(2900, input.HtlcOfferedRemoteTimeout)
	negInputResult, err := ctx.sweeper.SweepInput(&negInput, defaultFeePref)
	require.NoError(t, err)

	// Sweep a third input that has a smaller output than the previous one,
	// but yields positively because of its lower weight.
	positiveInput := createTestInput(2800, input.CommitmentNoDelay)

	// Mock the Broadcast method to succeed.
	bumpResultChan := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: largeInput.OutPoint()},
				{PreviousOutPoint: positiveInput.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	positiveInputResult, err := ctx.sweeper.SweepInput(
		&positiveInput, defaultFeePref,
	)
	require.NoError(t, err)

	// We expect that a sweep tx is published now, but it should only
	// contain the large input. The negative input should stay out of sweeps
	// until fees come down to get a positive net yield.
	sweepTx1 := ctx.receiveTx()
	assertTxSweepsInputs(t, &sweepTx1, &largeInput, &positiveInput)

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx1,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.expectResult(largeInputResult, nil)
	ctx.expectResult(positiveInputResult, nil)

	// Lower fee rate so that the negative input is no longer negative.
	ctx.estimator.updateFees(1000, 1000)

	// Create another large input.
	secondLargeInput := createTestInput(100000, input.CommitmentNoDelay)

	// Mock the Broadcast method to succeed.
	bumpResultChan = make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: negInput.OutPoint()},
				{PreviousOutPoint: secondLargeInput.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	secondLargeInputResult, err := ctx.sweeper.SweepInput(
		&secondLargeInput, defaultFeePref,
	)
	require.NoError(t, err)

	sweepTx2 := ctx.receiveTx()
	assertTxSweepsInputs(t, &sweepTx2, &secondLargeInput, &negInput)

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 2)

	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx2,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.expectResult(secondLargeInputResult, nil)
	ctx.expectResult(negInputResult, nil)

	ctx.finish(1)
}

// TestChunks asserts that large sets of inputs are split into multiple txes.
func TestChunks(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Mock the Broadcast method to succeed on the first chunk.
	bumpResultChan1 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan1, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		//nolint:lll
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: spendableInputs[0].OutPoint()},
				{PreviousOutPoint: spendableInputs[1].OutPoint()},
				{PreviousOutPoint: spendableInputs[2].OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan1 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Mock the Broadcast method to succeed on the second chunk.
	bumpResultChan2 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan2, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		//nolint:lll
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: spendableInputs[3].OutPoint()},
				{PreviousOutPoint: spendableInputs[4].OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan2 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Sweep five inputs.
	for _, input := range spendableInputs[:5] {
		_, err := ctx.sweeper.SweepInput(input, defaultFeePref)
		require.NoError(t, err)
	}

	// We expect two txes to be published because of the max input count of
	// three.
	sweepTx1 := ctx.receiveTx()
	require.Len(t, sweepTx1.TxIn, 3)

	sweepTx2 := ctx.receiveTx()
	require.Len(t, sweepTx2.TxIn, 2)

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 2)

	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan1 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx1,
		FeeRate: 10,
		Fee:     100,
	}
	bumpResultChan2 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx2,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.finish(1)
}

// TestRemoteSpend asserts that remote spends are properly detected and handled
// both before the sweep is published as well as after.
func TestRemoteSpend(t *testing.T) {
	t.Run("pre-sweep", func(t *testing.T) {
		testRemoteSpend(t, false)
	})
	t.Run("post-sweep", func(t *testing.T) {
		testRemoteSpend(t, true)
	})
}

func testRemoteSpend(t *testing.T, postSweep bool) {
	ctx := createSweeperTestContext(t)

	// Create a fake sweep tx that spends the second input as the first
	// will be spent by the remote.
	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: spendableInputs[1].OutPoint()},
		},
	}

	// Mock the Broadcast method to succeed.
	bumpResultChan := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	resultChan1, err := ctx.sweeper.SweepInput(
		spendableInputs[0], defaultFeePref,
	)
	require.NoError(t, err)

	resultChan2, err := ctx.sweeper.SweepInput(
		spendableInputs[1], defaultFeePref,
	)
	require.NoError(t, err)

	// Spend the input with an unknown tx.
	remoteTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: spendableInputs[0].OutPoint()},
		},
	}
	err = ctx.backend.publishTransaction(remoteTx)
	require.NoError(t, err)

	if postSweep {
		// Tx publication by sweeper returns ErrDoubleSpend. Sweeper
		// will retry the inputs without reporting a result. It could be
		// spent by the remote party.
		ctx.receiveTx()

		// Wait until the sweep tx has been saved to db.
		assertNumSweeps(t, ctx.sweeper, 1)
	}

	ctx.backend.mine()

	select {
	case result := <-resultChan1:
		if result.Err != ErrRemoteSpend {
			t.Fatalf("expected remote spend")
		}
		if result.Tx.TxHash() != remoteTx.TxHash() {
			t.Fatalf("expected remote spend tx")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no result received")
	}

	if !postSweep {
		// Assert that the sweeper sweeps the remaining input.
		sweepTx := ctx.receiveTx()
		require.Len(t, sweepTx.TxIn, 1)

		// Wait until the sweep tx has been saved to db.
		assertNumSweeps(t, ctx.sweeper, 1)

		ctx.backend.mine()

		// Mock a confirmed event.
		bumpResultChan <- &BumpResult{
			Event:   TxConfirmed,
			Tx:      &sweepTx,
			FeeRate: 10,
			Fee:     100,
		}

		ctx.expectResult(resultChan2, nil)

		ctx.finish(1)
	} else {
		// Expected sweeper to be still listening for spend of the
		// error input.
		ctx.finish(2)

		select {
		case r := <-resultChan2:
			require.NoError(t, r.Err)
			require.Equal(t, r.Tx.TxHash(), tx.TxHash())

		default:
		}
	}
}

// TestIdempotency asserts that offering the same input multiple times is
// handled correctly.
func TestIdempotency(t *testing.T) {
	ctx := createSweeperTestContext(t)

	input := spendableInputs[0]

	// Mock the Broadcast method to succeed.
	bumpResultChan := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	resultChan1, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	require.NoError(t, err)

	resultChan2, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	require.NoError(t, err)

	sweepTx := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	resultChan3, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	require.NoError(t, err)

	// Spend the input of the sweep tx.
	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.expectResult(resultChan1, nil)
	ctx.expectResult(resultChan2, nil)
	ctx.expectResult(resultChan3, nil)

	// Offer the same input again. The sweeper will register a spend ntfn
	// for this input. Because the input has already been spent, it will
	// immediately receive the spend notification with a spending tx hash.
	// Because the sweeper kept track of all of its sweep txes, it will
	// recognize the spend as its own.
	resultChan4, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	require.NoError(t, err)
	ctx.expectResult(resultChan4, nil)

	// Timer is still running, but spend notification was delivered before
	// it expired.
	ctx.finish(1)
}

// TestNoInputs asserts that nothing happens if nothing happens.
func TestNoInputs(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// No tx should appear. This is asserted in finish().
	ctx.finish(1)
}

// TestRestart asserts that the sweeper picks up sweeping properly after
// a restart.
func TestRestart(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweep input and expect sweep tx.
	input1 := spendableInputs[0]

	// Mock the Broadcast method to succeed.
	bumpResultChan1 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan1, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input1.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan1 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	_, err := ctx.sweeper.SweepInput(input1, defaultFeePref)
	require.NoError(t, err)

	sweepTx1 := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	// Restart sweeper.
	ctx.restartSweeper()

	// Simulate other subsystem (e.g. contract resolver) re-offering inputs.
	spendChan1, err := ctx.sweeper.SweepInput(input1, defaultFeePref)
	require.NoError(t, err)

	input2 := spendableInputs[1]

	// Mock the Broadcast method to succeed.
	bumpResultChan2 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan2, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input2.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan2 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	spendChan2, err := ctx.sweeper.SweepInput(input2, defaultFeePref)
	require.NoError(t, err)

	// Spend inputs of sweep txes and verify that spend channels signal
	// spends.
	ctx.backend.mine()

	// Sweeper should recognize that its sweep tx of the previous run is
	// spending the input.
	select {
	case result := <-spendChan1:
		if result.Err != nil {
			t.Fatalf("expected successful sweep")
		}
	case <-time.After(defaultTestTimeout):
		t.Fatalf("no result received")
	}

	// Timer tick should trigger republishing a sweep for the remaining
	// input.
	sweepTx2 := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 2)

	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan1 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx1,
		FeeRate: 10,
		Fee:     100,
	}
	bumpResultChan2 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx2,
		FeeRate: 10,
		Fee:     100,
	}

	select {
	case result := <-spendChan2:
		if result.Err != nil {
			t.Fatalf("expected successful sweep")
		}
	case <-time.After(defaultTestTimeout):
		t.Fatalf("no result received")
	}

	// Restart sweeper again. No action is expected.
	ctx.restartSweeper()

	ctx.finish(1)
}

// TestRestartRemoteSpend asserts that the sweeper picks up sweeping properly
// after a restart with remote spend.
func TestRestartRemoteSpend(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Get testing inputs.
	input1 := spendableInputs[0]
	input2 := spendableInputs[1]

	// Create a fake sweep tx that spends the second input as the first
	// will be spent by the remote.
	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input2.OutPoint()},
		},
	}

	// Mock the Broadcast method to succeed.
	bumpResultChan := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	_, err := ctx.sweeper.SweepInput(input1, defaultFeePref)
	require.NoError(t, err)

	// Sweep another input.
	_, err = ctx.sweeper.SweepInput(input2, defaultFeePref)
	require.NoError(t, err)

	sweepTx := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	// Restart sweeper.
	ctx.restartSweeper()

	// Replace the sweep tx with a remote tx spending input 2.
	ctx.backend.deleteUnconfirmed(sweepTx.TxHash())

	remoteTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: input1.OutPoint()},
		},
	}
	err = ctx.backend.publishTransaction(remoteTx)
	require.NoError(t, err)

	// Mine remote spending tx.
	ctx.backend.mine()

	// Mock the Broadcast method to succeed.
	bumpResultChan = make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Simulate other subsystem (e.g. contract resolver) re-offering input
	// 2.
	spendChan, err := ctx.sweeper.SweepInput(input2, defaultFeePref)
	require.NoError(t, err)

	// Expect sweeper to construct a new tx, because input 1 was spend
	// remotely.
	sweepTx = ctx.receiveTx()

	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.expectResult(spendChan, nil)

	ctx.finish(1)
}

// TestRestartConfirmed asserts that the sweeper picks up sweeping properly
// after a restart with a confirm of our own sweep tx.
func TestRestartConfirmed(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweep input.
	input := spendableInputs[0]

	// Mock the Broadcast method to succeed.
	bumpResultChan := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	_, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	require.NoError(t, err)

	sweepTx := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	// Restart sweeper.
	ctx.restartSweeper()

	// Mine the sweep tx.
	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx,
		FeeRate: 10,
		Fee:     100,
	}

	// Simulate other subsystem (e.g. contract resolver) re-offering input
	// 0.
	spendChan, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	require.NoError(t, err)
	if err != nil {
		t.Fatal(err)
	}

	// Here we expect again a successful sweep.
	ctx.expectResult(spendChan, nil)

	ctx.finish(1)
}

// TestRetry tests the sweeper retry flow.
func TestRetry(t *testing.T) {
	ctx := createSweeperTestContext(t)

	inp0 := spendableInputs[0]
	inp1 := spendableInputs[1]

	// Mock the Broadcast method to succeed.
	bumpResultChan1 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan1, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: inp0.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan1 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	resultChan0, err := ctx.sweeper.SweepInput(inp0, defaultFeePref)
	require.NoError(t, err)

	// We expect a sweep to be published.
	sweepTx1 := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 1)

	// Mock the Broadcast method to succeed on the second sweep.
	bumpResultChan2 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan2, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: inp1.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan2 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Offer a fresh input.
	resultChan1, err := ctx.sweeper.SweepInput(inp1, defaultFeePref)
	require.NoError(t, err)

	// A single tx is expected to be published.
	sweepTx2 := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 2)

	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan1 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx1,
		FeeRate: 10,
		Fee:     100,
	}
	bumpResultChan2 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx2,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.expectResult(resultChan0, nil)
	ctx.expectResult(resultChan1, nil)

	ctx.finish(1)
}

// TestDifferentFeePreferences ensures that the sweeper can have different
// transactions for different fee preferences. These transactions should be
// broadcast from highest to lowest fee rate.
func TestDifferentFeePreferences(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Throughout this test, we'll be attempting to sweep three inputs, two
	// with the higher fee preference, and the last with the lower. We do
	// this to ensure the sweeper can broadcast distinct transactions for
	// each sweep with a different fee preference.
	lowFeePref := FeeEstimateInfo{ConfTarget: 12}
	lowFeeRate := chainfee.SatPerKWeight(5000)
	ctx.estimator.blocksToFee[lowFeePref.ConfTarget] = lowFeeRate

	highFeePref := FeeEstimateInfo{ConfTarget: 6}
	highFeeRate := chainfee.SatPerKWeight(10000)
	ctx.estimator.blocksToFee[highFeePref.ConfTarget] = highFeeRate

	input1 := spendableInputs[0]
	input2 := spendableInputs[1]
	input3 := spendableInputs[2]

	// Mock the Broadcast method to succeed on the first sweep.
	bumpResultChan1 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan1, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input1.OutPoint()},
				{PreviousOutPoint: input2.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan1 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Mock the Broadcast method to succeed on the second sweep.
	bumpResultChan2 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan2, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input3.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan2 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	resultChan1, err := ctx.sweeper.SweepInput(
		input1, Params{Fee: highFeePref},
	)
	require.NoError(t, err)

	resultChan2, err := ctx.sweeper.SweepInput(
		input2, Params{Fee: highFeePref},
	)
	require.NoError(t, err)

	resultChan3, err := ctx.sweeper.SweepInput(
		input3, Params{Fee: lowFeePref},
	)
	require.NoError(t, err)

	// The first transaction broadcast should be the one spending the
	// higher fee rate inputs.
	sweepTx1 := ctx.receiveTx()

	// The second should be the one spending the lower fee rate inputs.
	sweepTx2 := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 2)

	// With the transactions broadcast, we'll mine a block to so that the
	// result is delivered to each respective client.
	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan1 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx1,
		FeeRate: 10,
		Fee:     100,
	}
	bumpResultChan2 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx2,
		FeeRate: 10,
		Fee:     100,
	}

	resultChans := []chan Result{resultChan1, resultChan2, resultChan3}
	for _, resultChan := range resultChans {
		ctx.expectResult(resultChan, nil)
	}

	ctx.finish(1)
}

// TestPendingInputs ensures that the sweeper correctly determines the inputs
// pending to be swept.
func TestPendingInputs(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Throughout this test, we'll be attempting to sweep three inputs, two
	// with the higher fee preference, and the last with the lower. We do
	// this to ensure the sweeper can return all pending inputs, even those
	// with different fee preferences.
	const (
		lowFeeRate  = 5000
		highFeeRate = 10000
	)

	lowFeePref := FeeEstimateInfo{
		ConfTarget: 12,
	}
	ctx.estimator.blocksToFee[lowFeePref.ConfTarget] = lowFeeRate

	highFeePref := FeeEstimateInfo{
		ConfTarget: 6,
	}
	ctx.estimator.blocksToFee[highFeePref.ConfTarget] = highFeeRate

	input1 := spendableInputs[0]
	input2 := spendableInputs[1]
	input3 := spendableInputs[2]

	// Mock the Broadcast method to succeed on the first sweep.
	bumpResultChan1 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan1, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input1.OutPoint()},
				{PreviousOutPoint: input2.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan1 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Mock the Broadcast method to succeed on the second sweep.
	bumpResultChan2 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan2, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input3.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan2 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	resultChan1, err := ctx.sweeper.SweepInput(
		input1, Params{Fee: highFeePref},
	)
	require.NoError(t, err)

	_, err = ctx.sweeper.SweepInput(
		input2, Params{Fee: highFeePref},
	)
	require.NoError(t, err)

	resultChan3, err := ctx.sweeper.SweepInput(
		input3, Params{Fee: lowFeePref},
	)
	require.NoError(t, err)

	// We should expect to see all inputs pending.
	ctx.assertPendingInputs(input1, input2, input3)

	// We should expect to see both sweep transactions broadcast - one for
	// the higher feerate, the other for the lower.
	sweepTx1 := ctx.receiveTx()
	sweepTx2 := ctx.receiveTx()

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 2)

	// Mine these txns, and we should expect to see the results delivered.
	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan1 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx1,
		FeeRate: 10,
		Fee:     100,
	}
	bumpResultChan2 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx2,
		FeeRate: 10,
		Fee:     100,
	}

	ctx.expectResult(resultChan1, nil)
	ctx.expectResult(resultChan3, nil)
	ctx.assertPendingInputs()

	ctx.finish(1)
}

// TestExclusiveGroup tests the sweeper exclusive group functionality.
func TestExclusiveGroup(t *testing.T) {
	ctx := createSweeperTestContext(t)

	input1 := spendableInputs[0]
	input2 := spendableInputs[1]
	input3 := spendableInputs[2]

	// Mock the Broadcast method to succeed on the first sweep.
	bumpResultChan1 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan1, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input1.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan1 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Mock the Broadcast method to succeed on the second sweep.
	bumpResultChan2 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan2, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input2.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan2 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Mock the Broadcast method to succeed on the third sweep.
	bumpResultChan3 := make(chan *BumpResult, 1)
	ctx.publisher.On("Broadcast", mock.Anything).Return(
		bumpResultChan3, nil).Run(func(args mock.Arguments) {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn: []*wire.TxIn{
				{PreviousOutPoint: input3.OutPoint()},
			},
		}

		// Send the first event.
		bumpResultChan3 <- &BumpResult{
			Event: TxPublished,
			Tx:    tx,
		}

		// Due to a mix of new and old test frameworks, we need to
		// manually call the method to get the test to pass.
		//
		// TODO(yy): remove the test context and replace them will
		// mocks.
		err := ctx.backend.PublishTransaction(tx, "")
		require.NoError(t, err)
	}).Once()

	// Sweep three inputs in the same exclusive group.
	var results []chan Result
	for i := 0; i < 3; i++ {
		exclusiveGroup := uint64(1)
		result, err := ctx.sweeper.SweepInput(
			spendableInputs[i], Params{
				Fee:            FeeEstimateInfo{ConfTarget: 6},
				ExclusiveGroup: &exclusiveGroup,
			},
		)
		require.NoError(t, err)
		results = append(results, result)
	}

	// We expect all inputs to be published in separate transactions, even
	// though they share the same fee preference.
	sweepTx1 := ctx.receiveTx()
	require.Len(t, sweepTx1.TxIn, 1)

	sweepTx2 := ctx.receiveTx()
	sweepTx3 := ctx.receiveTx()

	// Remove all txes except for the one that sweeps the first
	// input. This simulates the sweeps being conflicting.
	ctx.backend.deleteUnconfirmed(sweepTx2.TxHash())
	ctx.backend.deleteUnconfirmed(sweepTx3.TxHash())

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 3)

	// Mine the first sweep tx.
	ctx.backend.mine()

	// Mock a confirmed event.
	bumpResultChan1 <- &BumpResult{
		Event:   TxConfirmed,
		Tx:      &sweepTx1,
		FeeRate: 10,
		Fee:     100,
	}
	bumpResultChan2 <- &BumpResult{
		Event: TxFailed,
		Tx:    &sweepTx2,
	}
	bumpResultChan2 <- &BumpResult{
		Event: TxFailed,
		Tx:    &sweepTx3,
	}

	// Expect the first input to be swept by the confirmed sweep tx.
	result0 := <-results[0]
	if result0.Err != nil {
		t.Fatal("expected first input to be swept")
	}

	// Expect the other two inputs to return an error. They have no chance
	// of confirming.
	result1 := <-results[1]
	if result1.Err != ErrExclusiveGroupSpend {
		t.Fatal("expected second input to be canceled")
	}

	result2 := <-results[2]
	if result2.Err != ErrExclusiveGroupSpend {
		t.Fatal("expected third input to be canceled")
	}
}

type testInput struct {
	*input.BaseInput

	locktime *uint32
	reqTxOut *wire.TxOut
}

func (i *testInput) RequiredLockTime() (uint32, bool) {
	if i.locktime != nil {
		return *i.locktime, true
	}

	return 0, false
}

func (i *testInput) RequiredTxOut() *wire.TxOut {
	return i.reqTxOut
}

// CraftInputScript is a custom sign method for the testInput type that will
// encode the spending outpoint and the tx input index as part of the returned
// witness.
func (i *testInput) CraftInputScript(_ input.Signer, txn *wire.MsgTx,
	hashCache *txscript.TxSigHashes,
	prevOutputFetcher txscript.PrevOutputFetcher,
	txinIdx int) (*input.Script, error) {

	// We'll encode the outpoint in the witness, so we can assert that the
	// expected input was signed at the correct index.
	op := i.OutPoint()
	return &input.Script{
		Witness: [][]byte{
			// We encode the hash of the outpoint...
			op.Hash[:],
			// ..the outpoint index...
			{byte(op.Index)},
			// ..and finally the tx input index.
			{byte(txinIdx)},
		},
	}, nil
}

// assertSignedIndex goes through all inputs to the tx and checks that all
// testInputs have witnesses corresponding to the outpoints they are spending,
// and are signed at the correct tx input index. All found testInputs are
// returned such that we can sum up and sanity check that all testInputs were
// part of the sweep.
func assertSignedIndex(t *testing.T, tx *wire.MsgTx,
	testInputs map[wire.OutPoint]*testInput) map[wire.OutPoint]struct{} {

	found := make(map[wire.OutPoint]struct{})
	for idx, txIn := range tx.TxIn {
		op := txIn.PreviousOutPoint

		// Not a testInput, it won't have the test encoding we require
		// to check outpoint and index.
		if _, ok := testInputs[op]; !ok {
			continue
		}

		if _, ok := found[op]; ok {
			t.Fatalf("input already used")
		}

		// Check it was signes spending the correct outpoint, and at
		// the expected tx input index.
		require.Equal(t, txIn.Witness[0], op.Hash[:])
		require.Equal(t, txIn.Witness[1], []byte{byte(op.Index)})
		require.Equal(t, txIn.Witness[2], []byte{byte(idx)})
		found[op] = struct{}{}
	}

	return found
}

// TestLockTimes checks that the sweeper properly groups inputs requiring the
// same locktime together into sweep transactions.
func TestLockTimes(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// We increase the number of max inputs to a tx so that won't
	// impact our test.
	ctx.sweeper.cfg.MaxInputsPerTx = 100

	// We also need to update the aggregator about this new config.
	ctx.sweeper.cfg.Aggregator = NewSimpleUtxoAggregator(
		ctx.estimator, DefaultMaxFeeRate.FeePerKWeight(), 100,
	)

	// We will set up the lock times in such a way that we expect the
	// sweeper to divide the inputs into 4 diffeerent transactions.
	const numSweeps = 4

	// Sweep 8 inputs, using 4 different lock times.
	var (
		results         []chan Result
		inputs          = make(map[wire.OutPoint]input.Input)
		clusters        = make(map[uint32][]input.Input)
		bumpResultChans = make([]chan *BumpResult, 0, 4)
	)
	for i := 0; i < numSweeps*2; i++ {
		lt := uint32(10 + (i % numSweeps))
		inp := &testInput{
			BaseInput: spendableInputs[i],
			locktime:  &lt,
		}

		op := inp.OutPoint()
		inputs[op] = inp

		cluster, ok := clusters[lt]
		if !ok {
			cluster = make([]input.Input, 0)
		}
		cluster = append(cluster, inp)
		clusters[lt] = cluster
	}

	for i := 0; i < 3; i++ {
		inp := spendableInputs[i+numSweeps*2]
		inputs[inp.OutPoint()] = inp

		lt := uint32(10 + (i % numSweeps))
		clusters[lt] = append(clusters[lt], inp)
	}

	for lt, cluster := range clusters {
		// Create a fake sweep tx.
		tx := &wire.MsgTx{
			TxIn:     []*wire.TxIn{},
			LockTime: lt,
		}

		// Append the inputs.
		for _, inp := range cluster {
			txIn := &wire.TxIn{
				PreviousOutPoint: inp.OutPoint(),
			}
			tx.TxIn = append(tx.TxIn, txIn)
		}

		// Mock the Broadcast method to succeed on current sweep.
		bumpResultChan := make(chan *BumpResult, 1)
		bumpResultChans = append(bumpResultChans, bumpResultChan)
		ctx.publisher.On("Broadcast", mock.Anything).Return(
			bumpResultChan, nil).Run(func(args mock.Arguments) {
			// Send the first event.
			bumpResultChan <- &BumpResult{
				Event: TxPublished,
				Tx:    tx,
			}

			// Due to a mix of new and old test frameworks, we need
			// to manually call the method to get the test to pass.
			//
			// TODO(yy): remove the test context and replace them
			// will mocks.
			err := ctx.backend.PublishTransaction(tx, "")
			require.NoError(t, err)
		}).Once()
	}

	// Make all the sweeps.
	for _, inp := range inputs {
		result, err := ctx.sweeper.SweepInput(
			inp, Params{
				Fee: FeeEstimateInfo{ConfTarget: 6},
			},
		)
		require.NoError(t, err)

		results = append(results, result)
	}

	// Check the sweeps transactions, ensuring all inputs are there, and
	// all the locktimes are satisfied.
	sweepTxes := make([]wire.MsgTx, 0, numSweeps)
	for i := 0; i < numSweeps; i++ {
		sweepTx := ctx.receiveTx()
		sweepTxes = append(sweepTxes, sweepTx)

		for _, txIn := range sweepTx.TxIn {
			op := txIn.PreviousOutPoint
			inp, ok := inputs[op]
			require.True(t, ok)

			delete(inputs, op)

			// If this input had a required locktime, ensure the tx
			// has that set correctly.
			lt, ok := inp.RequiredLockTime()
			if !ok {
				continue
			}

			require.EqualValues(t, lt, sweepTx.LockTime)
		}
	}

	// Wait until the sweep tx has been saved to db.
	assertNumSweeps(t, ctx.sweeper, 4)

	// Mine the sweeps.
	ctx.backend.mine()

	for i, bumpResultChan := range bumpResultChans {
		// Mock a confirmed event.
		bumpResultChan <- &BumpResult{
			Event:   TxConfirmed,
			Tx:      &sweepTxes[i],
			FeeRate: 10,
			Fee:     100,
		}
	}

	// The should be no inputs not foud in any of the sweeps.
	require.Empty(t, inputs)

	// Results should all come back.
	for i, resultChan := range results {
		select {
		case result := <-resultChan:
			require.NoError(t, result.Err)
		case <-time.After(1 * time.Second):
			t.Fatalf("result %v did not come back", i)
		}
	}
}

// TestSweeperShutdownHandling tests that we notify callers when the sweeper
// cannot handle requests since it's in the process of shutting down.
func TestSweeperShutdownHandling(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Make the backing notifier break down. This is what happens during
	// lnd shut down, since the notifier is stopped before the sweeper.
	require.Len(t, ctx.notifier.epochChan, 1)
	for epochChan := range ctx.notifier.epochChan {
		close(epochChan)
	}

	// Give the collector some time to exit.
	time.Sleep(50 * time.Millisecond)

	// Now trying to sweep inputs should return an error on the error
	// channel.
	resultChan, err := ctx.sweeper.SweepInput(
		spendableInputs[0], defaultFeePref,
	)
	require.NoError(t, err)

	select {
	case res := <-resultChan:
		require.Equal(t, ErrSweeperShuttingDown, res.Err)

	case <-time.After(defaultTestTimeout):
		t.Fatalf("no result arrived")
	}

	// Stop the sweeper properly.
	err = ctx.sweeper.Stop()
	require.NoError(t, err)

	// Now attempting to sweep an input should error out immediately.
	_, err = ctx.sweeper.SweepInput(
		spendableInputs[0], defaultFeePref,
	)
	require.Error(t, err)
}

// TestMarkInputsPendingPublish checks that given a list of inputs with
// different states, only the non-terminal state will be marked as `Published`.
func TestMarkInputsPendingPublish(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create three testing inputs.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `pendingInputs` map.
	inputNotExist := &input.MockInput{}
	defer inputNotExist.AssertExpectations(t)

	inputNotExist.On("OutPoint").Return(wire.OutPoint{Index: 0})

	// inputInit specifies a newly created input.
	inputInit := &input.MockInput{}
	defer inputInit.AssertExpectations(t)

	inputInit.On("OutPoint").Return(wire.OutPoint{Index: 1})

	s.inputs[inputInit.OutPoint()] = &SweeperInput{
		state: Init,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &input.MockInput{}
	defer inputPendingPublish.AssertExpectations(t)

	inputPendingPublish.On("OutPoint").Return(wire.OutPoint{Index: 2})

	s.inputs[inputPendingPublish.OutPoint()] = &SweeperInput{
		state: PendingPublish,
	}

	// inputTerminated specifies an input that's terminated.
	inputTerminated := &input.MockInput{}
	defer inputTerminated.AssertExpectations(t)

	inputTerminated.On("OutPoint").Return(wire.OutPoint{Index: 3})

	s.inputs[inputTerminated.OutPoint()] = &SweeperInput{
		state: Excluded,
	}

	// Mark the test inputs. We expect the non-exist input and the
	// inputTerminated to be skipped, and the rest to be marked as pending
	// publish.
	set.On("Inputs").Return([]input.Input{
		inputNotExist, inputInit, inputPendingPublish, inputTerminated,
	})
	s.markInputsPendingPublish(set)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 3)

	// We expect the init input's state to become pending publish.
	require.Equal(PendingPublish, s.inputs[inputInit.OutPoint()].state)

	// We expect the pending-publish to stay unchanged.
	require.Equal(PendingPublish,
		s.inputs[inputPendingPublish.OutPoint()].state)

	// We expect the terminated to stay unchanged.
	require.Equal(Excluded, s.inputs[inputTerminated.OutPoint()].state)
}

// TestMarkInputsPublished checks that given a list of inputs with different
// states, only the state `PendingPublish` will be marked as `Published`.
func TestMarkInputsPublished(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a mock sweeper store.
	mockStore := NewMockSweeperStore()

	// Create a test TxRecord and a dummy error.
	dummyTR := &TxRecord{}
	dummyErr := errors.New("dummy error")

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: mockStore,
	})

	// Create three testing inputs.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `inputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input. When marking this as
	// published, we should see an error log as this input hasn't been
	// published yet.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.inputs[inputInit.PreviousOutPoint] = &SweeperInput{
		state: Init,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.inputs[inputPendingPublish.PreviousOutPoint] = &SweeperInput{
		state: PendingPublish,
	}

	// First, check that when an error is returned from db, it's properly
	// returned here.
	mockStore.On("StoreTx", dummyTR).Return(dummyErr).Once()
	err := s.markInputsPublished(dummyTR, nil)
	require.ErrorIs(err, dummyErr)

	// We also expect the record has been marked as published.
	require.True(dummyTR.Published)

	// Then, check that the target input has will be correctly marked as
	// published.
	//
	// Mock the store to return nil
	mockStore.On("StoreTx", dummyTR).Return(nil).Once()

	// Mark the test inputs. We expect the non-exist input and the
	// inputInit to be skipped, and the final input to be marked as
	// published.
	err = s.markInputsPublished(dummyTR, []*wire.TxIn{
		inputNotExist, inputInit, inputPendingPublish,
	})
	require.NoError(err)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 2)

	// We expect the init input's state to stay unchanged.
	require.Equal(Init,
		s.inputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish input's is now marked as published.
	require.Equal(Published,
		s.inputs[inputPendingPublish.PreviousOutPoint].state)

	// Assert mocked statements are executed as expected.
	mockStore.AssertExpectations(t)
}

// TestMarkInputsPublishFailed checks that given a list of inputs with
// different states, only the state `PendingPublish` and `Published` will be
// marked as `PublishFailed`.
func TestMarkInputsPublishFailed(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a mock sweeper store.
	mockStore := NewMockSweeperStore()

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: mockStore,
	})

	// Create three testing inputs.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `inputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input. When marking this as
	// published, we should see an error log as this input hasn't been
	// published yet.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.inputs[inputInit.PreviousOutPoint] = &SweeperInput{
		state: Init,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.inputs[inputPendingPublish.PreviousOutPoint] = &SweeperInput{
		state: PendingPublish,
	}

	// inputPublished specifies an input that's published.
	inputPublished := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 4},
	}
	s.inputs[inputPublished.PreviousOutPoint] = &SweeperInput{
		state: Published,
	}

	// Mark the test inputs. We expect the non-exist input and the
	// inputInit to be skipped, and the final input to be marked as
	// published.
	s.markInputsPublishFailed([]wire.OutPoint{
		inputNotExist.PreviousOutPoint,
		inputInit.PreviousOutPoint,
		inputPendingPublish.PreviousOutPoint,
		inputPublished.PreviousOutPoint,
	})

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 3)

	// We expect the init input's state to stay unchanged.
	require.Equal(Init,
		s.inputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish input's is now marked as publish
	// failed.
	require.Equal(PublishFailed,
		s.inputs[inputPendingPublish.PreviousOutPoint].state)

	// We expect the published input's is now marked as publish failed.
	require.Equal(PublishFailed,
		s.inputs[inputPublished.PreviousOutPoint].state)

	// Assert mocked statements are executed as expected.
	mockStore.AssertExpectations(t)
}

// TestMarkInputsSwept checks that given a list of inputs with different
// states, only the non-terminal state will be marked as `Swept`.
func TestMarkInputsSwept(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a mock input.
	mockInput := &input.MockInput{}
	defer mockInput.AssertExpectations(t)

	// Mock the `OutPoint` to return a dummy outpoint.
	mockInput.On("OutPoint").Return(wire.OutPoint{Hash: chainhash.Hash{1}})

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create three testing inputs.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `inputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.inputs[inputInit.PreviousOutPoint] = &SweeperInput{
		state: Init,
		Input: mockInput,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.inputs[inputPendingPublish.PreviousOutPoint] = &SweeperInput{
		state: PendingPublish,
		Input: mockInput,
	}

	// inputTerminated specifies an input that's terminated.
	inputTerminated := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 4},
	}
	s.inputs[inputTerminated.PreviousOutPoint] = &SweeperInput{
		state: Excluded,
		Input: mockInput,
	}

	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			inputNotExist, inputInit,
			inputPendingPublish, inputTerminated,
		},
	}

	// Mark the test inputs. We expect the inputTerminated to be skipped,
	// and the rest to be marked as swept.
	s.markInputsSwept(tx, true)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 3)

	// We expect the init input's state to become swept.
	require.Equal(Swept,
		s.inputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish becomes swept.
	require.Equal(Swept,
		s.inputs[inputPendingPublish.PreviousOutPoint].state)

	// We expect the terminated to stay unchanged.
	require.Equal(Excluded,
		s.inputs[inputTerminated.PreviousOutPoint].state)
}

// TestMempoolLookup checks that the method `mempoolLookup` works as expected.
func TestMempoolLookup(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a test outpoint.
	op := wire.OutPoint{Index: 1}

	// Create a mock mempool watcher.
	mockMempool := chainntnfs.NewMockMempoolWatcher()
	defer mockMempool.AssertExpectations(t)

	// Create a test sweeper without a mempool.
	s := New(&UtxoSweeperConfig{})

	// Since we don't have a mempool, we expect the call to return a
	// fn.None indicating it's not found.
	tx := s.mempoolLookup(op)
	require.True(tx.IsNone())

	// Re-create the sweeper with the mocked mempool watcher.
	s = New(&UtxoSweeperConfig{
		Mempool: mockMempool,
	})

	// Mock the mempool watcher to return not found.
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.None[wire.MsgTx]()).Once()

	// We expect a fn.None tx to be returned.
	tx = s.mempoolLookup(op)
	require.True(tx.IsNone())

	// Mock the mempool to return a spending tx.
	dummyTx := wire.MsgTx{}
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.Some(dummyTx)).Once()

	// Calling the loopup again, we expect the dummyTx to be returned.
	tx = s.mempoolLookup(op)
	require.False(tx.IsNone())
	require.Equal(dummyTx, tx.UnsafeFromSome())
}

// TestUpdateSweeperInputs checks that the method `updateSweeperInputs` will
// properly update the inputs based on their states.
func TestUpdateSweeperInputs(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a test sweeper.
	s := New(nil)

	// Create mock inputs.
	inp1 := &input.MockInput{}
	defer inp1.AssertExpectations(t)
	inp2 := &input.MockInput{}
	defer inp2.AssertExpectations(t)
	inp3 := &input.MockInput{}
	defer inp3.AssertExpectations(t)

	// Create a list of inputs using all the states.
	//
	// Mock the input to have a locktime that's matured so it will be
	// returned.
	inp1.On("RequiredLockTime").Return(
		uint32(s.currentHeight), false).Once()
	inp1.On("BlocksToMaturity").Return(uint32(0)).Once()
	inp1.On("HeightHint").Return(uint32(s.currentHeight)).Once()
	input0 := &SweeperInput{state: Init, Input: inp1}

	// These inputs won't hit RequiredLockTime so we won't mock.
	input1 := &SweeperInput{state: PendingPublish, Input: inp1}
	input2 := &SweeperInput{state: Published, Input: inp1}

	// Mock the input to have a locktime that's matured so it will be
	// returned.
	inp1.On("RequiredLockTime").Return(
		uint32(s.currentHeight), false).Once()
	inp1.On("BlocksToMaturity").Return(uint32(0)).Once()
	inp1.On("HeightHint").Return(uint32(s.currentHeight)).Once()
	input3 := &SweeperInput{state: PublishFailed, Input: inp1}

	// These inputs won't hit RequiredLockTime so we won't mock.
	input4 := &SweeperInput{state: Swept, Input: inp1}
	input5 := &SweeperInput{state: Excluded, Input: inp1}
	input6 := &SweeperInput{state: Failed, Input: inp1}

	// Mock the input to have a locktime in the future so it will NOT be
	// returned.
	inp2.On("RequiredLockTime").Return(
		uint32(s.currentHeight+1), true).Once()
	input7 := &SweeperInput{state: Init, Input: inp2}

	// Mock the input to have a CSV expiry in the future so it will NOT be
	// returned.
	inp3.On("RequiredLockTime").Return(
		uint32(s.currentHeight), false).Once()
	inp3.On("BlocksToMaturity").Return(uint32(2)).Once()
	inp3.On("HeightHint").Return(uint32(s.currentHeight)).Once()
	input8 := &SweeperInput{state: Init, Input: inp3}

	// Add the inputs to the sweeper. After the update, we should see the
	// terminated inputs being removed.
	s.inputs = map[wire.OutPoint]*SweeperInput{
		{Index: 0}: input0,
		{Index: 1}: input1,
		{Index: 2}: input2,
		{Index: 3}: input3,
		{Index: 4}: input4,
		{Index: 5}: input5,
		{Index: 6}: input6,
		{Index: 7}: input7,
		{Index: 8}: input8,
	}

	// We expect the inputs with `Swept`, `Excluded`, and `Failed` to be
	// removed.
	expectedInputs := map[wire.OutPoint]*SweeperInput{
		{Index: 0}: input0,
		{Index: 1}: input1,
		{Index: 2}: input2,
		{Index: 3}: input3,
		{Index: 7}: input7,
		{Index: 8}: input8,
	}

	// We expect only the inputs with `Init` and `PublishFailed` to be
	// returned.
	expectedReturn := map[wire.OutPoint]*SweeperInput{
		{Index: 0}: input0,
		{Index: 3}: input3,
	}

	// Update the sweeper inputs.
	inputs := s.updateSweeperInputs()

	// Assert the returned inputs are as expected.
	require.Equal(expectedReturn, inputs)

	// Assert the sweeper inputs are as expected.
	require.Equal(expectedInputs, s.inputs)
}

// TestDecideStateAndRBFInfo checks that the expected state and RBFInfo are
// returned based on whether this input can be found both in mempool and the
// sweeper store.
func TestDecideStateAndRBFInfo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a test outpoint.
	op := wire.OutPoint{Index: 1}

	// Create a mock mempool watcher and a mock sweeper store.
	mockMempool := chainntnfs.NewMockMempoolWatcher()
	defer mockMempool.AssertExpectations(t)
	mockStore := NewMockSweeperStore()
	defer mockStore.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store:   mockStore,
		Mempool: mockMempool,
	})

	// First, mock the mempool to return false.
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.None[wire.MsgTx]()).Once()

	// Since the mempool lookup failed, we exepect state Init and no
	// RBFInfo.
	state, rbf := s.decideStateAndRBFInfo(op)
	require.True(rbf.IsNone())
	require.Equal(Init, state)

	// Mock the mempool lookup to return a tx three times as we are calling
	// attachAvailableRBFInfo three times.
	tx := wire.MsgTx{}
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.Some(tx)).Times(3)

	// Mock the store to return an error saying the tx cannot be found.
	mockStore.On("GetTx", tx.TxHash()).Return(nil, ErrTxNotFound).Once()

	// Although the db lookup failed, we expect the state to be Published.
	state, rbf = s.decideStateAndRBFInfo(op)
	require.True(rbf.IsNone())
	require.Equal(Published, state)

	// Mock the store to return a db error.
	dummyErr := errors.New("dummy error")
	mockStore.On("GetTx", tx.TxHash()).Return(nil, dummyErr).Once()

	// Although the db lookup failed, we expect the state to be Published.
	state, rbf = s.decideStateAndRBFInfo(op)
	require.True(rbf.IsNone())
	require.Equal(Published, state)

	// Mock the store to return a record.
	tr := &TxRecord{
		Fee:     100,
		FeeRate: 100,
	}
	mockStore.On("GetTx", tx.TxHash()).Return(tr, nil).Once()

	// Call the method again.
	state, rbf = s.decideStateAndRBFInfo(op)

	// Assert that the RBF info is returned.
	rbfInfo := fn.Some(RBFInfo{
		Txid:    tx.TxHash(),
		Fee:     btcutil.Amount(tr.Fee),
		FeeRate: chainfee.SatPerKWeight(tr.FeeRate),
	})
	require.Equal(rbfInfo, rbf)

	// Assert the state is updated.
	require.Equal(Published, state)
}

// TestMarkInputFailed checks that the input is marked as failed as expected.
func TestMarkInputFailed(t *testing.T) {
	t.Parallel()

	// Create a mock input.
	mockInput := &input.MockInput{}
	defer mockInput.AssertExpectations(t)

	// Mock the `OutPoint` to return a dummy outpoint.
	mockInput.On("OutPoint").Return(wire.OutPoint{Hash: chainhash.Hash{1}})

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create a testing pending input.
	pi := &SweeperInput{
		state: Init,
		Input: mockInput,
	}

	// Call the method under test.
	s.markInputFailed(pi, errors.New("dummy error"))

	// Assert the state is updated.
	require.Equal(t, Failed, pi.state)
}

// TestSweepPendingInputs checks that `sweepPendingInputs` correctly executes
// its workflow based on the returned values from the interfaces.
func TestSweepPendingInputs(t *testing.T) {
	t.Parallel()

	// Create a mock wallet and aggregator.
	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	aggregator := &mockUtxoAggregator{}
	defer aggregator.AssertExpectations(t)

	publisher := &MockBumper{}
	defer publisher.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Wallet:     wallet,
		Aggregator: aggregator,
		Publisher:  publisher,
		GenSweepScript: func() ([]byte, error) {
			return testPubKey.SerializeCompressed(), nil
		},
		NoDeadlineConfTarget: uint32(DefaultDeadlineDelta),
	})

	// Set a current height to test the deadline override.
	s.currentHeight = testHeight

	// Create an input set that needs wallet inputs.
	setNeedWallet := &MockInputSet{}
	defer setNeedWallet.AssertExpectations(t)

	// Mock this set to ask for wallet input.
	setNeedWallet.On("NeedWalletInput").Return(true).Once()
	setNeedWallet.On("AddWalletInputs", wallet).Return(nil).Once()

	// Mock the wallet to require the lock once.
	wallet.On("WithCoinSelectLock", mock.Anything).Return(nil).Once()

	// Create an input set that doesn't need wallet inputs.
	normalSet := &MockInputSet{}
	defer normalSet.AssertExpectations(t)

	normalSet.On("NeedWalletInput").Return(false).Once()

	// Mock the methods used in `sweep`. This is not important for this
	// unit test.
	setNeedWallet.On("Inputs").Return(nil).Times(4)
	setNeedWallet.On("DeadlineHeight").Return(testHeight).Once()
	setNeedWallet.On("Budget").Return(btcutil.Amount(1)).Once()
	setNeedWallet.On("StartingFeeRate").Return(
		fn.None[chainfee.SatPerKWeight]()).Once()
	normalSet.On("Inputs").Return(nil).Times(4)
	normalSet.On("DeadlineHeight").Return(testHeight).Once()
	normalSet.On("Budget").Return(btcutil.Amount(1)).Once()
	normalSet.On("StartingFeeRate").Return(
		fn.None[chainfee.SatPerKWeight]()).Once()

	// Make pending inputs for testing. We don't need real values here as
	// the returned clusters are mocked.
	pis := make(InputsMap)

	// Mock the aggregator to return the mocked input sets.
	expectedDeadlineUsed := testHeight + DefaultDeadlineDelta
	aggregator.On("ClusterInputs", pis,
		expectedDeadlineUsed).Return([]InputSet{
		setNeedWallet, normalSet,
	})

	// Mock `Broadcast` to return an error. This should cause the
	// `createSweepTx` inside `sweep` to fail. This is done so we can
	// terminate the method early as we are only interested in testing the
	// workflow in `sweepPendingInputs`. We don't need to test `sweep` here
	// as it should be tested in its own unit test.
	dummyErr := errors.New("dummy error")
	publisher.On("Broadcast", mock.Anything).Return(nil, dummyErr).Twice()

	// Call the method under test.
	s.sweepPendingInputs(pis)
}

// TestHandleBumpEventTxFailed checks that the sweeper correctly handles the
// case where the bump event tx fails to be published.
func TestHandleBumpEventTxFailed(t *testing.T) {
	t.Parallel()

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	var (
		// Create four testing outpoints.
		op1        = wire.OutPoint{Hash: chainhash.Hash{1}}
		op2        = wire.OutPoint{Hash: chainhash.Hash{2}}
		op3        = wire.OutPoint{Hash: chainhash.Hash{3}}
		opNotExist = wire.OutPoint{Hash: chainhash.Hash{4}}
	)

	// Create three mock inputs.
	input1 := &input.MockInput{}
	defer input1.AssertExpectations(t)

	input2 := &input.MockInput{}
	defer input2.AssertExpectations(t)

	input3 := &input.MockInput{}
	defer input3.AssertExpectations(t)

	// Construct the initial state for the sweeper.
	s.inputs = InputsMap{
		op1: &SweeperInput{Input: input1, state: PendingPublish},
		op2: &SweeperInput{Input: input2, state: PendingPublish},
		op3: &SweeperInput{Input: input3, state: PendingPublish},
	}

	// Create a testing tx that spends the first two inputs.
	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op1},
			{PreviousOutPoint: op2},
			{PreviousOutPoint: opNotExist},
		},
	}

	// Create a testing bump result.
	br := &BumpResult{
		Tx:    tx,
		Event: TxFailed,
		Err:   errDummy,
	}

	// Call the method under test.
	err := s.handleBumpEvent(br)
	require.ErrorIs(t, err, errDummy)

	// Assert the states of the first two inputs are updated.
	require.Equal(t, PublishFailed, s.inputs[op1].state)
	require.Equal(t, PublishFailed, s.inputs[op2].state)

	// Assert the state of the third input is not updated.
	require.Equal(t, PendingPublish, s.inputs[op3].state)

	// Assert the non-existing input is not added to the pending inputs.
	require.NotContains(t, s.inputs, opNotExist)
}

// TestHandleBumpEventTxReplaced checks that the sweeper correctly handles the
// case where the bump event tx is replaced.
func TestHandleBumpEventTxReplaced(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a mock wallet.
	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store:  store,
		Wallet: wallet,
	})

	// Create a testing outpoint.
	op := wire.OutPoint{Hash: chainhash.Hash{1}}

	// Create a mock input.
	inp := &input.MockInput{}
	defer inp.AssertExpectations(t)

	// Construct the initial state for the sweeper.
	s.inputs = InputsMap{
		op: &SweeperInput{Input: inp, state: PendingPublish},
	}

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}

	// Create a replacement tx.
	replacementTx := &wire.MsgTx{
		LockTime: 2,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}

	// Create a testing bump result.
	br := &BumpResult{
		Tx:         replacementTx,
		ReplacedTx: tx,
		Event:      TxReplaced,
	}

	// Mock the store to return an error.
	dummyErr := errors.New("dummy error")
	store.On("GetTx", tx.TxHash()).Return(nil, dummyErr).Once()

	// Call the method under test and assert the error is returned.
	err := s.handleBumpEventTxReplaced(br)
	require.ErrorIs(t, err, dummyErr)

	// Mock the store to return the old tx record.
	store.On("GetTx", tx.TxHash()).Return(&TxRecord{
		Txid: tx.TxHash(),
	}, nil).Once()

	// We expect to cancel rebroadcasting the replaced tx.
	wallet.On("CancelRebroadcast", tx.TxHash()).Once()

	// Mock an error returned when deleting the old tx record.
	store.On("DeleteTx", tx.TxHash()).Return(dummyErr).Once()

	// Call the method under test and assert the error is returned.
	err = s.handleBumpEventTxReplaced(br)
	require.ErrorIs(t, err, dummyErr)

	// Mock the store to return the old tx record and delete it without
	// error.
	store.On("GetTx", tx.TxHash()).Return(&TxRecord{
		Txid: tx.TxHash(),
	}, nil).Once()
	store.On("DeleteTx", tx.TxHash()).Return(nil).Once()

	// Mock the store to save the new tx record.
	store.On("StoreTx", &TxRecord{
		Txid:      replacementTx.TxHash(),
		Published: true,
	}).Return(nil).Once()

	// We expect to cancel rebroadcasting the replaced tx.
	wallet.On("CancelRebroadcast", tx.TxHash()).Once()

	// Call the method under test.
	err = s.handleBumpEventTxReplaced(br)
	require.NoError(t, err)

	// Assert the state of the input is updated.
	require.Equal(t, Published, s.inputs[op].state)
}

// TestHandleBumpEventTxPublished checks that the sweeper correctly handles the
// case where the bump event tx is published.
func TestHandleBumpEventTxPublished(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: store,
	})

	// Create a testing outpoint.
	op := wire.OutPoint{Hash: chainhash.Hash{1}}

	// Create a mock input.
	inp := &input.MockInput{}
	defer inp.AssertExpectations(t)

	// Construct the initial state for the sweeper.
	s.inputs = InputsMap{
		op: &SweeperInput{Input: inp, state: PendingPublish},
	}

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}

	// Create a testing bump result.
	br := &BumpResult{
		Tx:    tx,
		Event: TxPublished,
	}

	// Mock the store to save the new tx record.
	store.On("StoreTx", &TxRecord{
		Txid:      tx.TxHash(),
		Published: true,
	}).Return(nil).Once()

	// Call the method under test.
	err := s.handleBumpEventTxPublished(br)
	require.NoError(t, err)

	// Assert the state of the input is updated.
	require.Equal(t, Published, s.inputs[op].state)
}

// TestMonitorFeeBumpResult checks that the fee bump monitor loop correctly
// exits when the sweeper is stopped, the tx is confirmed or failed.
func TestMonitorFeeBumpResult(t *testing.T) {
	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a mock wallet.
	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store:  store,
		Wallet: wallet,
	})

	// Create a testing outpoint.
	op := wire.OutPoint{Hash: chainhash.Hash{1}}

	// Create a mock input.
	inp := &input.MockInput{}
	defer inp.AssertExpectations(t)

	// Construct the initial state for the sweeper.
	s.inputs = InputsMap{
		op: &SweeperInput{Input: inp, state: PendingPublish},
	}

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}

	testCases := []struct {
		name            string
		setupResultChan func() <-chan *BumpResult
		shouldExit      bool
	}{
		{
			// When a tx confirmed event is received, we expect to
			// exit the monitor loop.
			name: "tx confirmed",
			// We send a result with TxConfirmed event to the
			// result channel.
			setupResultChan: func() <-chan *BumpResult {
				// Create a result chan.
				resultChan := make(chan *BumpResult, 1)
				resultChan <- &BumpResult{
					Tx:      tx,
					Event:   TxConfirmed,
					Fee:     10000,
					FeeRate: 100,
				}

				// We expect to cancel rebroadcasting the tx
				// once confirmed.
				wallet.On("CancelRebroadcast",
					tx.TxHash()).Once()

				return resultChan
			},
			shouldExit: true,
		},
		{
			// When a tx failed event is received, we expect to
			// exit the monitor loop.
			name: "tx failed",
			// We send a result with TxConfirmed event to the
			// result channel.
			setupResultChan: func() <-chan *BumpResult {
				// Create a result chan.
				resultChan := make(chan *BumpResult, 1)
				resultChan <- &BumpResult{
					Tx:    tx,
					Event: TxFailed,
					Err:   errDummy,
				}

				// We expect to cancel rebroadcasting the tx
				// once failed.
				wallet.On("CancelRebroadcast",
					tx.TxHash()).Once()

				return resultChan
			},
			shouldExit: true,
		},
		{
			// When processing non-confirmed events, the monitor
			// should not exit.
			name: "no exit on normal event",
			// We send a result with TxPublished and mock the
			// method `StoreTx` to return nil.
			setupResultChan: func() <-chan *BumpResult {
				// Create a result chan.
				resultChan := make(chan *BumpResult, 1)
				resultChan <- &BumpResult{
					Tx:    tx,
					Event: TxPublished,
				}

				return resultChan
			},
			shouldExit: false,
		}, {
			// When the sweeper is shutting down, the monitor loop
			// should exit.
			name: "exit on sweeper shutdown",
			// We don't send anything but quit the sweeper.
			setupResultChan: func() <-chan *BumpResult {
				close(s.quit)

				return nil
			},
			shouldExit: true,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Setup the testing result channel.
			resultChan := tc.setupResultChan()

			// Create a done chan that's used to signal the monitor
			// has exited.
			done := make(chan struct{})

			s.wg.Add(1)
			go func() {
				s.monitorFeeBumpResult(resultChan)
				close(done)
			}()

			// The monitor is expected to exit, we check it's done
			// in one second or fail.
			if tc.shouldExit {
				select {
				case <-done:
				case <-time.After(1 * time.Second):
					require.Fail(t, "monitor not exited")
				}

				return
			}

			// The monitor should not exit, check it doesn't close
			// the `done` channel within one second.
			select {
			case <-done:
				require.Fail(t, "monitor exited")
			case <-time.After(1 * time.Second):
			}
		})
	}
}

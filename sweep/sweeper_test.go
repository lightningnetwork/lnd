package sweep

import (
	"errors"
	"os"
	"runtime/pprof"
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
)

type sweeperTestContext struct {
	t *testing.T

	sweeper   *UtxoSweeper
	notifier  *MockNotifier
	estimator *mockFeeEstimator
	backend   *mockBackend
	store     SweeperStore

	publishChan   chan wire.MsgTx
	currentHeight int32
}

var (
	spendableInputs []*input.BaseInput
	testInputCount  int

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
		byte(testInputCount + 1)}

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

	testInputCount++

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

	ctx := &sweeperTestContext{
		notifier:      notifier,
		publishChan:   backend.publishChan,
		t:             t,
		estimator:     estimator,
		backend:       backend,
		store:         store,
		currentHeight: mockChainHeight,
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
		FeeEstimator:     estimator,
		MaxInputsPerTx:   testMaxInputsPerTx,
		MaxSweepAttempts: testMaxSweepAttempts,
		MaxFeeRate:       DefaultMaxFeeRate,
		Aggregator:       aggregator,
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
		inputSet[*input.OutPoint()] = struct{}{}
	}

	pendingInputs, err := ctx.sweeper.PendingInputs()
	if err != nil {
		ctx.t.Fatal(err)
	}
	if len(pendingInputs) != len(inputSet) {
		ctx.t.Fatalf("expected %d pending inputs, got %d",
			len(inputSet), len(pendingInputs))
	}
	for input := range pendingInputs {
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
		m[*input.OutPoint()] = struct{}{}
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
		m[*input.OutPoint()] = input
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

// TestSuccess tests the sweeper happy flow.
func TestSuccess(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweeping an input without a fee preference should result in an error.
	_, err := ctx.sweeper.SweepInput(spendableInputs[0], Params{
		Fee: &FeeEstimateInfo{},
	})
	if err != ErrNoFeePreference {
		t.Fatalf("expected ErrNoFeePreference, got %v", err)
	}

	resultChan, err := ctx.sweeper.SweepInput(
		spendableInputs[0], defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	sweepTx := ctx.receiveTx()

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

	_, err = ctx.sweeper.SweepInput(&largeInput, defaultFeePref)
	require.NoError(t, err)

	// The second input brings the sweep output above the dust limit. We
	// expect a sweep tx now.

	sweepTx := ctx.receiveTx()
	require.Len(t, sweepTx.TxIn, 2, "unexpected num of tx inputs")

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

	_, err := ctx.sweeper.SweepInput(
		&dustInput,
		Params{Fee: FeeEstimateInfo{FeeRate: chainfee.FeePerKwFloor}},
	)
	if err != nil {
		t.Fatal(err)
	}

	sweepTx := ctx.receiveTx()
	if len(sweepTx.TxIn) != 2 {
		t.Fatalf("Expected tx to sweep 2 inputs, but contains %v "+
			"inputs instead", len(sweepTx.TxIn))
	}

	// Calculate expected output value based on wallet utxo of 1_000_000
	// sats.
	expectedOutputValue := int64(294 + 1_000_000 - 180)
	if sweepTx.TxOut[0].Value != expectedOutputValue {
		t.Fatalf("Expected output value of %v, but got %v",
			expectedOutputValue, sweepTx.TxOut[0].Value)
	}

	ctx.backend.mine()
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
	if err != nil {
		t.Fatal(err)
	}

	// Sweep an additional input with a negative net yield. The weight of
	// the HtlcAcceptedRemoteSuccess input type adds more in fees than its
	// value at the current fee level.
	negInput := createTestInput(2900, input.HtlcOfferedRemoteTimeout)
	negInputResult, err := ctx.sweeper.SweepInput(&negInput, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	// Sweep a third input that has a smaller output than the previous one,
	// but yields positively because of its lower weight.
	positiveInput := createTestInput(2800, input.CommitmentNoDelay)
	positiveInputResult, err := ctx.sweeper.SweepInput(
		&positiveInput, defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	// We expect that a sweep tx is published now, but it should only
	// contain the large input. The negative input should stay out of sweeps
	// until fees come down to get a positive net yield.
	sweepTx1 := ctx.receiveTx()
	assertTxSweepsInputs(t, &sweepTx1, &largeInput, &positiveInput)

	ctx.backend.mine()

	ctx.expectResult(largeInputResult, nil)
	ctx.expectResult(positiveInputResult, nil)

	// Lower fee rate so that the negative input is no longer negative.
	ctx.estimator.updateFees(1000, 1000)

	// Create another large input.
	secondLargeInput := createTestInput(100000, input.CommitmentNoDelay)
	secondLargeInputResult, err := ctx.sweeper.SweepInput(
		&secondLargeInput, defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	sweepTx2 := ctx.receiveTx()
	assertTxSweepsInputs(t, &sweepTx2, &secondLargeInput, &negInput)

	ctx.backend.mine()

	ctx.expectResult(secondLargeInputResult, nil)
	ctx.expectResult(negInputResult, nil)

	ctx.finish(1)
}

// TestChunks asserts that large sets of inputs are split into multiple txes.
func TestChunks(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweep five inputs.
	for _, input := range spendableInputs[:5] {
		_, err := ctx.sweeper.SweepInput(input, defaultFeePref)
		if err != nil {
			t.Fatal(err)
		}
	}

	// We expect two txes to be published because of the max input count of
	// three.
	sweepTx1 := ctx.receiveTx()
	if len(sweepTx1.TxIn) != 3 {
		t.Fatalf("Expected first tx to sweep 3 inputs, but contains %v "+
			"inputs instead", len(sweepTx1.TxIn))
	}

	sweepTx2 := ctx.receiveTx()
	if len(sweepTx2.TxIn) != 2 {
		t.Fatalf("Expected first tx to sweep 2 inputs, but contains %v "+
			"inputs instead", len(sweepTx1.TxIn))
	}

	ctx.backend.mine()

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

	resultChan1, err := ctx.sweeper.SweepInput(
		spendableInputs[0], defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	resultChan2, err := ctx.sweeper.SweepInput(
		spendableInputs[1], defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Spend the input with an unknown tx.
	remoteTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: *(spendableInputs[0].OutPoint()),
			},
		},
	}
	err = ctx.backend.publishTransaction(remoteTx)
	if err != nil {
		t.Fatal(err)
	}

	if postSweep {

		// Tx publication by sweeper returns ErrDoubleSpend. Sweeper
		// will retry the inputs without reporting a result. It could be
		// spent by the remote party.
		ctx.receiveTx()
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

		if len(sweepTx.TxIn) != 1 {
			t.Fatal("expected sweep to only sweep the one remaining output")
		}

		ctx.backend.mine()

		ctx.expectResult(resultChan2, nil)

		ctx.finish(1)
	} else {
		// Expected sweeper to be still listening for spend of the
		// error input.
		ctx.finish(2)

		select {
		case <-resultChan2:
			t.Fatalf("no result expected for error input")
		default:
		}
	}
}

// TestIdempotency asserts that offering the same input multiple times is
// handled correctly.
func TestIdempotency(t *testing.T) {
	ctx := createSweeperTestContext(t)

	input := spendableInputs[0]
	resultChan1, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	resultChan2, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	ctx.receiveTx()

	resultChan3, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	// Spend the input of the sweep tx.
	ctx.backend.mine()

	ctx.expectResult(resultChan1, nil)
	ctx.expectResult(resultChan2, nil)
	ctx.expectResult(resultChan3, nil)

	// Offer the same input again. The sweeper will register a spend ntfn
	// for this input. Because the input has already been spent, it will
	// immediately receive the spend notification with a spending tx hash.
	// Because the sweeper kept track of all of its sweep txes, it will
	// recognize the spend as its own.
	resultChan4, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}
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
	_, err := ctx.sweeper.SweepInput(input1, defaultFeePref)
	require.NoError(t, err)

	ctx.receiveTx()

	// Restart sweeper.
	ctx.restartSweeper()

	// Simulate other subsystem (e.g. contract resolver) re-offering inputs.
	spendChan1, err := ctx.sweeper.SweepInput(input1, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	input2 := spendableInputs[1]
	spendChan2, err := ctx.sweeper.SweepInput(input2, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

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
	ctx.receiveTx()

	ctx.backend.mine()

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

	// Sweep input.
	input1 := spendableInputs[0]
	_, err := ctx.sweeper.SweepInput(input1, defaultFeePref)
	require.NoError(t, err)

	// Sweep another input.
	input2 := spendableInputs[1]
	_, err = ctx.sweeper.SweepInput(input2, defaultFeePref)
	require.NoError(t, err)

	sweepTx := ctx.receiveTx()

	// Restart sweeper.
	ctx.restartSweeper()

	// Replace the sweep tx with a remote tx spending input 1.
	ctx.backend.deleteUnconfirmed(sweepTx.TxHash())

	remoteTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: *(input2.OutPoint()),
			},
		},
	}
	if err := ctx.backend.publishTransaction(remoteTx); err != nil {
		t.Fatal(err)
	}

	// Mine remote spending tx.
	ctx.backend.mine()

	// Simulate other subsystem (e.g. contract resolver) re-offering input
	// 0.
	spendChan, err := ctx.sweeper.SweepInput(input1, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	// Expect sweeper to construct a new tx, because input 1 was spend
	// remotely.
	ctx.receiveTx()

	ctx.backend.mine()

	ctx.expectResult(spendChan, nil)

	ctx.finish(1)
}

// TestRestartConfirmed asserts that the sweeper picks up sweeping properly
// after a restart with a confirm of our own sweep tx.
func TestRestartConfirmed(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweep input.
	input := spendableInputs[0]
	if _, err := ctx.sweeper.SweepInput(input, defaultFeePref); err != nil {
		t.Fatal(err)
	}

	ctx.receiveTx()

	// Restart sweeper.
	ctx.restartSweeper()

	// Mine the sweep tx.
	ctx.backend.mine()

	// Simulate other subsystem (e.g. contract resolver) re-offering input
	// 0.
	spendChan, err := ctx.sweeper.SweepInput(input, defaultFeePref)
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

	resultChan0, err := ctx.sweeper.SweepInput(
		spendableInputs[0], defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	// We expect a sweep to be published.
	ctx.receiveTx()

	// Offer a fresh input.
	resultChan1, err := ctx.sweeper.SweepInput(
		spendableInputs[1], defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	// A single tx is expected to be published.
	ctx.receiveTx()

	ctx.backend.mine()

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
	resultChan1, err := ctx.sweeper.SweepInput(
		input1, Params{Fee: highFeePref},
	)
	if err != nil {
		t.Fatal(err)
	}
	input2 := spendableInputs[1]
	resultChan2, err := ctx.sweeper.SweepInput(
		input2, Params{Fee: highFeePref},
	)
	if err != nil {
		t.Fatal(err)
	}
	input3 := spendableInputs[2]
	resultChan3, err := ctx.sweeper.SweepInput(
		input3, Params{Fee: lowFeePref},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Generate the same type of sweep script that was used for weight
	// estimation.
	changePk, err := ctx.sweeper.cfg.GenSweepScript()
	require.NoError(t, err)

	// The first transaction broadcast should be the one spending the higher
	// fee rate inputs.
	sweepTx1 := ctx.receiveTx()
	assertTxFeeRate(t, &sweepTx1, highFeeRate, changePk, input1, input2)

	// The second should be the one spending the lower fee rate inputs.
	sweepTx2 := ctx.receiveTx()
	assertTxFeeRate(t, &sweepTx2, lowFeeRate, changePk, input3)

	// With the transactions broadcast, we'll mine a block to so that the
	// result is delivered to each respective client.
	ctx.backend.mine()
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
	resultChan1, err := ctx.sweeper.SweepInput(
		input1, Params{Fee: highFeePref},
	)
	if err != nil {
		t.Fatal(err)
	}
	input2 := spendableInputs[1]
	_, err = ctx.sweeper.SweepInput(
		input2, Params{Fee: highFeePref},
	)
	if err != nil {
		t.Fatal(err)
	}
	input3 := spendableInputs[2]
	resultChan3, err := ctx.sweeper.SweepInput(
		input3, Params{Fee: lowFeePref},
	)
	if err != nil {
		t.Fatal(err)
	}

	// We should expect to see all inputs pending.
	ctx.assertPendingInputs(input1, input2, input3)

	// We should expect to see both sweep transactions broadcast - one for
	// the higher feerate, the other for the lower.
	ctx.receiveTx()
	ctx.receiveTx()

	// Mine these txns, and we should expect to see the results delivered.
	ctx.backend.mine()
	ctx.expectResult(resultChan1, nil)
	ctx.expectResult(resultChan3, nil)
	ctx.assertPendingInputs()

	ctx.finish(1)
}

// TestBumpFeeRBF ensures that the UtxoSweeper can properly handle a fee bump
// request for an input it is currently attempting to sweep. When sweeping the
// input with the higher fee rate, a replacement transaction is created.
func TestBumpFeeRBF(t *testing.T) {
	ctx := createSweeperTestContext(t)

	lowFeePref := FeeEstimateInfo{ConfTarget: 144}
	lowFeeRate := chainfee.FeePerKwFloor
	ctx.estimator.blocksToFee[lowFeePref.ConfTarget] = lowFeeRate

	// We'll first try to bump the fee of an output currently unknown to the
	// UtxoSweeper. Doing so should result in a lnwallet.ErrNotMine error.
	_, err := ctx.sweeper.UpdateParams(
		wire.OutPoint{}, ParamsUpdate{Fee: lowFeePref},
	)
	if err != lnwallet.ErrNotMine {
		t.Fatalf("expected error lnwallet.ErrNotMine, got \"%v\"", err)
	}

	// We'll then attempt to sweep an input, which we'll use to bump its fee
	// later on.
	input := createTestInput(
		btcutil.SatoshiPerBitcoin, input.CommitmentTimeLock,
	)
	sweepResult, err := ctx.sweeper.SweepInput(
		&input, Params{Fee: lowFeePref},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Generate the same type of change script used so we can have accurate
	// weight estimation.
	changePk, err := ctx.sweeper.cfg.GenSweepScript()
	require.NoError(t, err)

	// Ensure that a transaction is broadcast with the lower fee preference.
	lowFeeTx := ctx.receiveTx()
	assertTxFeeRate(t, &lowFeeTx, lowFeeRate, changePk, &input)

	// We'll then attempt to bump its fee rate.
	highFeePref := FeeEstimateInfo{ConfTarget: 6}
	highFeeRate := DefaultMaxFeeRate.FeePerKWeight()
	ctx.estimator.blocksToFee[highFeePref.ConfTarget] = highFeeRate

	// We should expect to see an error if a fee preference isn't provided.
	_, err = ctx.sweeper.UpdateParams(*input.OutPoint(), ParamsUpdate{
		Fee: &FeeEstimateInfo{},
	})
	if err != ErrNoFeePreference {
		t.Fatalf("expected ErrNoFeePreference, got %v", err)
	}

	bumpResult, err := ctx.sweeper.UpdateParams(
		*input.OutPoint(), ParamsUpdate{Fee: highFeePref},
	)
	require.NoError(t, err, "unable to bump input's fee")

	// A higher fee rate transaction should be immediately broadcast.
	highFeeTx := ctx.receiveTx()
	assertTxFeeRate(t, &highFeeTx, highFeeRate, changePk, &input)

	// We'll finish our test by mining the sweep transaction.
	ctx.backend.mine()
	ctx.expectResult(sweepResult, nil)
	ctx.expectResult(bumpResult, nil)

	ctx.finish(1)
}

// TestExclusiveGroup tests the sweeper exclusive group functionality.
func TestExclusiveGroup(t *testing.T) {
	ctx := createSweeperTestContext(t)

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
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}

	// We expect all inputs to be published in separate transactions, even
	// though they share the same fee preference.
	for i := 0; i < 3; i++ {
		sweepTx := ctx.receiveTx()
		if len(sweepTx.TxOut) != 1 {
			t.Fatal("expected a single tx out in the sweep tx")
		}

		// Remove all txes except for the one that sweeps the first
		// input. This simulates the sweeps being conflicting.
		if sweepTx.TxIn[0].PreviousOutPoint !=
			*spendableInputs[0].OutPoint() {

			ctx.backend.deleteUnconfirmed(sweepTx.TxHash())
		}
	}

	// Mine the first sweep tx.
	ctx.backend.mine()

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

// TestCpfp tests that the sweeper spends cpfp inputs at a fee rate that exceeds
// the parent tx fee rate.
func TestCpfp(t *testing.T) {
	ctx := createSweeperTestContext(t)

	ctx.estimator.updateFees(1000, chainfee.FeePerKwFloor)

	// Offer an input with an unconfirmed parent tx to the sweeper. The
	// parent tx pays 3000 sat/kw.
	hash := chainhash.Hash{1}
	input := input.MakeBaseInput(
		&wire.OutPoint{Hash: hash},
		input.CommitmentTimeLock,
		&input.SignDescriptor{
			Output: &wire.TxOut{
				Value: 330,
			},
			KeyDesc: keychain.KeyDescriptor{
				PubKey: testPubKey,
			},
		},
		0,
		&input.TxInfo{
			Weight: 300,
			Fee:    900,
		},
	)

	feePref := FeeEstimateInfo{ConfTarget: 6}
	result, err := ctx.sweeper.SweepInput(
		&input, Params{Fee: feePref, Force: true},
	)
	require.NoError(t, err)

	// Increase the fee estimate to above the parent tx fee rate.
	ctx.estimator.updateFees(5000, chainfee.FeePerKwFloor)

	// Signal a new block. This is a trigger for the sweeper to refresh fee
	// estimates.
	ctx.notifier.NotifyEpoch(1000)

	// Now we do expect a sweep transaction to be published with our input
	// and an attached wallet utxo.
	tx := ctx.receiveTx()
	require.Len(t, tx.TxIn, 2)
	require.Len(t, tx.TxOut, 1)

	// As inputs we have 10000 sats from the wallet and 330 sats from the
	// cpfp input. The sweep tx is weight expected to be 759 units. There is
	// an additional 300 weight units from the parent to include in the
	// package, making a total of 1059. At 5000 sat/kw, the required fee for
	// the package is 5295 sats. The parent already paid 900 sats, so there
	// is 4395 sat remaining to be paid. The expected output value is
	// therefore: 1_000_000 + 330 - 4395 = 995 935.
	require.Equal(t, int64(995_935), tx.TxOut[0].Value)

	// Mine the tx and assert that the result is passed back.
	ctx.backend.mine()
	ctx.expectResult(result, nil)

	ctx.finish(1)
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
		results []chan Result
		inputs  = make(map[wire.OutPoint]input.Input)
	)
	for i := 0; i < numSweeps*2; i++ {
		lt := uint32(10 + (i % numSweeps))
		inp := &testInput{
			BaseInput: spendableInputs[i],
			locktime:  &lt,
		}

		result, err := ctx.sweeper.SweepInput(
			inp, Params{
				Fee: FeeEstimateInfo{ConfTarget: 6},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)

		op := inp.OutPoint()
		inputs[*op] = inp
	}

	// We also add 3 regular inputs that don't require any specific lock
	// time.
	for i := 0; i < 3; i++ {
		inp := spendableInputs[i+numSweeps*2]
		result, err := ctx.sweeper.SweepInput(
			inp, Params{
				Fee: FeeEstimateInfo{ConfTarget: 6},
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		results = append(results, result)

		op := inp.OutPoint()
		inputs[*op] = inp
	}

	// Check the sweeps transactions, ensuring all inputs are there, and
	// all the locktimes are satisfied.
	for i := 0; i < numSweeps; i++ {
		sweepTx := ctx.receiveTx()
		if len(sweepTx.TxOut) != 1 {
			t.Fatal("expected a single tx out in the sweep tx")
		}

		for _, txIn := range sweepTx.TxIn {
			op := txIn.PreviousOutPoint
			inp, ok := inputs[op]
			if !ok {
				t.Fatalf("Unexpected outpoint: %v", op)
			}

			delete(inputs, op)

			// If this input had a required locktime, ensure the tx
			// has that set correctly.
			lt, ok := inp.RequiredLockTime()
			if !ok {
				continue
			}

			if lt != sweepTx.LockTime {
				t.Fatalf("Input required locktime %v, sweep "+
					"tx had locktime %v", lt, sweepTx.LockTime)
			}
		}
	}

	// The should be no inputs not foud in any of the sweeps.
	if len(inputs) != 0 {
		t.Fatalf("had unsweeped inputs: %v", inputs)
	}

	// Mine the first sweeps
	ctx.backend.mine()

	// Results should all come back.
	for i := range results {
		select {
		case result := <-results[i]:
			require.NoError(t, result.Err)
		case <-time.After(1 * time.Second):
			t.Fatalf("result %v did not come back", i)
		}
	}
}

// TestRequiredTxOuts checks that inputs having a required TxOut gets swept with
// sweep transactions paying into these outputs.
func TestRequiredTxOuts(t *testing.T) {
	// Create some test inputs and locktime vars.
	var inputs []*input.BaseInput
	for i := 0; i < 20; i++ {
		input := createTestInput(
			int64(btcutil.SatoshiPerBitcoin+i*500),
			input.CommitmentTimeLock,
		)

		inputs = append(inputs, &input)
	}

	locktime1 := uint32(51)
	locktime2 := uint32(52)
	locktime3 := uint32(53)

	aPkScript := make([]byte, input.P2WPKHSize)
	aPkScript[0] = 'a'

	bPkScript := make([]byte, input.P2WSHSize)
	bPkScript[0] = 'b'

	cPkScript := make([]byte, input.P2PKHSize)
	cPkScript[0] = 'c'

	dPkScript := make([]byte, input.P2SHSize)
	dPkScript[0] = 'd'

	ePkScript := make([]byte, input.UnknownWitnessSize)
	ePkScript[0] = 'e'

	fPkScript := make([]byte, input.P2WSHSize)
	fPkScript[0] = 'f'

	testCases := []struct {
		name         string
		inputs       []*testInput
		assertSweeps func(*testing.T, map[wire.OutPoint]*testInput,
			[]*wire.MsgTx)
	}{
		{
			// Single input with a required TX out that is smaller.
			// We expect a change output to be added.
			name: "single input, leftover change",
			inputs: []*testInput{
				{
					BaseInput: inputs[0],
					reqTxOut: &wire.TxOut{
						PkScript: aPkScript,
						Value:    100000,
					},
				},
			},

			// Since the required output value is small, we expect
			// the rest after fees to go into a change output.
			assertSweeps: func(t *testing.T,
				_ map[wire.OutPoint]*testInput,
				txs []*wire.MsgTx) {

				require.Equal(t, 1, len(txs))

				tx := txs[0]
				require.Equal(t, 1, len(tx.TxIn))

				// We should have two outputs, the required
				// output must be the first one.
				require.Equal(t, 2, len(tx.TxOut))
				out := tx.TxOut[0]
				require.Equal(t, aPkScript, out.PkScript)
				require.Equal(t, int64(100000), out.Value)
			},
		},
		{
			// An input committing to a slightly smaller output, so
			// it will pay its own fees.
			name: "single input, no change",
			inputs: []*testInput{
				{
					BaseInput: inputs[0],
					reqTxOut: &wire.TxOut{
						PkScript: aPkScript,

						// Fee will be about 5340 sats.
						// Subtract a bit more to
						// ensure no dust change output
						// is manifested.
						Value: inputs[0].SignDesc().Output.Value - 6300,
					},
				},
			},

			// We expect this single input/output pair.
			assertSweeps: func(t *testing.T,
				_ map[wire.OutPoint]*testInput,
				txs []*wire.MsgTx) {

				require.Equal(t, 1, len(txs))

				tx := txs[0]
				require.Equal(t, 1, len(tx.TxIn))

				require.Equal(t, 1, len(tx.TxOut))
				out := tx.TxOut[0]
				require.Equal(t, aPkScript, out.PkScript)
				require.Equal(
					t,
					inputs[0].SignDesc().Output.Value-6300,
					out.Value,
				)
			},
		},
		{
			// Two inputs, where the first one required no tx out.
			name: "two inputs, one with required tx out",
			inputs: []*testInput{
				{

					// We add a normal, non-requiredTxOut
					// input. We use test input 10, to make
					// sure this has a higher yield than
					// the other input, and will be
					// attempted added first to the sweep
					// tx.
					BaseInput: inputs[10],
				},
				{
					// The second input requires a TxOut.
					BaseInput: inputs[0],
					reqTxOut: &wire.TxOut{
						PkScript: aPkScript,
						Value:    inputs[0].SignDesc().Output.Value,
					},
				},
			},

			// We expect the inputs to have been reordered.
			assertSweeps: func(t *testing.T,
				_ map[wire.OutPoint]*testInput,
				txs []*wire.MsgTx) {

				require.Equal(t, 1, len(txs))

				tx := txs[0]
				require.Equal(t, 2, len(tx.TxIn))
				require.Equal(t, 2, len(tx.TxOut))

				// The required TxOut should be the first one.
				out := tx.TxOut[0]
				require.Equal(t, aPkScript, out.PkScript)
				require.Equal(
					t, inputs[0].SignDesc().Output.Value,
					out.Value,
				)

				// The first input should be the one having the
				// required TxOut.
				require.Len(t, tx.TxIn, 2)
				require.Equal(
					t, inputs[0].OutPoint(),
					&tx.TxIn[0].PreviousOutPoint,
				)

				// Second one is the one without a required tx
				// out.
				require.Equal(
					t, inputs[10].OutPoint(),
					&tx.TxIn[1].PreviousOutPoint,
				)
			},
		},

		{
			// An input committing to an output of equal value, just
			// add input to pay fees.
			name: "single input, extra fee input",
			inputs: []*testInput{
				{
					BaseInput: inputs[0],
					reqTxOut: &wire.TxOut{
						PkScript: aPkScript,
						Value:    inputs[0].SignDesc().Output.Value,
					},
				},
			},

			// We expect an extra input and output.
			assertSweeps: func(t *testing.T,
				_ map[wire.OutPoint]*testInput,
				txs []*wire.MsgTx) {

				require.Equal(t, 1, len(txs))

				tx := txs[0]
				require.Equal(t, 2, len(tx.TxIn))

				require.Equal(t, 2, len(tx.TxOut))
				out := tx.TxOut[0]
				require.Equal(t, aPkScript, out.PkScript)
				require.Equal(
					t, inputs[0].SignDesc().Output.Value,
					out.Value,
				)
			},
		},
		{
			// Three inputs added, should be combined into a single
			// sweep.
			name: "three inputs",
			inputs: []*testInput{
				{
					BaseInput: inputs[0],
					reqTxOut: &wire.TxOut{
						PkScript: aPkScript,
						Value:    inputs[0].SignDesc().Output.Value,
					},
				},
				{
					BaseInput: inputs[1],
					reqTxOut: &wire.TxOut{
						PkScript: bPkScript,
						Value:    inputs[1].SignDesc().Output.Value,
					},
				},
				{
					BaseInput: inputs[2],
					reqTxOut: &wire.TxOut{
						PkScript: cPkScript,
						Value:    inputs[2].SignDesc().Output.Value,
					},
				},
			},

			// We expect an extra input and output to pay fees.
			assertSweeps: func(t *testing.T,
				testInputs map[wire.OutPoint]*testInput,
				txs []*wire.MsgTx) {

				require.Equal(t, 1, len(txs))

				tx := txs[0]
				require.Equal(t, 4, len(tx.TxIn))
				require.Equal(t, 4, len(tx.TxOut))

				// The inputs and outputs must be in the same
				// order.
				for i, in := range tx.TxIn {
					// Last one is the change input/output
					// pair, so we'll skip it.
					if i == 3 {
						continue
					}

					// Get this input to ensure the output
					// on index i coresponsd to this one.
					inp := testInputs[in.PreviousOutPoint]
					require.NotNil(t, inp)

					require.Equal(
						t, tx.TxOut[i].Value,
						inp.SignDesc().Output.Value,
					)
				}
			},
		},
		{
			// Six inputs added, which 3 different locktimes.
			// Should result in 3 sweeps.
			name: "six inputs",
			inputs: []*testInput{
				{
					BaseInput: inputs[0],
					locktime:  &locktime1,
					reqTxOut: &wire.TxOut{
						PkScript: aPkScript,
						Value:    inputs[0].SignDesc().Output.Value,
					},
				},
				{
					BaseInput: inputs[1],
					locktime:  &locktime1,
					reqTxOut: &wire.TxOut{
						PkScript: bPkScript,
						Value:    inputs[1].SignDesc().Output.Value,
					},
				},
				{
					BaseInput: inputs[2],
					locktime:  &locktime2,
					reqTxOut: &wire.TxOut{
						PkScript: cPkScript,
						Value:    inputs[2].SignDesc().Output.Value,
					},
				},
				{
					BaseInput: inputs[3],
					locktime:  &locktime2,
					reqTxOut: &wire.TxOut{
						PkScript: dPkScript,
						Value:    inputs[3].SignDesc().Output.Value,
					},
				},
				{
					BaseInput: inputs[4],
					locktime:  &locktime3,
					reqTxOut: &wire.TxOut{
						PkScript: ePkScript,
						Value:    inputs[4].SignDesc().Output.Value,
					},
				},
				{
					BaseInput: inputs[5],
					locktime:  &locktime3,
					reqTxOut: &wire.TxOut{
						PkScript: fPkScript,
						Value:    inputs[5].SignDesc().Output.Value,
					},
				},
			},

			// We expect three sweeps, each having two of our
			// inputs, one extra input and output to pay fees.
			assertSweeps: func(t *testing.T,
				testInputs map[wire.OutPoint]*testInput,
				txs []*wire.MsgTx) {

				require.Equal(t, 3, len(txs))

				for _, tx := range txs {
					require.Equal(t, 3, len(tx.TxIn))
					require.Equal(t, 3, len(tx.TxOut))

					// The inputs and outputs must be in
					// the same order.
					for i, in := range tx.TxIn {
						// Last one is the change
						// output, so we'll skip it.
						if i == 2 {
							continue
						}

						// Get this input to ensure the
						// output on index i coresponsd
						// to this one.
						inp := testInputs[in.PreviousOutPoint]
						require.NotNil(t, inp)

						require.Equal(
							t, tx.TxOut[i].Value,
							inp.SignDesc().Output.Value,
						)

						// Check that the locktimes are
						// kept intact.
						require.Equal(
							t, tx.LockTime,
							*inp.locktime,
						)
					}
				}
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			ctx := createSweeperTestContext(t)

			// We increase the number of max inputs to a tx so that
			// won't impact our test.
			ctx.sweeper.cfg.MaxInputsPerTx = 100

			// Sweep all test inputs.
			var (
				inputs  = make(map[wire.OutPoint]*testInput)
				results = make(map[wire.OutPoint]chan Result)
			)
			for _, inp := range testCase.inputs {
				result, err := ctx.sweeper.SweepInput(
					inp, Params{
						Fee: FeeEstimateInfo{
							ConfTarget: 6,
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}

				op := inp.OutPoint()
				results[*op] = result
				inputs[*op] = inp
			}

			// Send a new block epoch to trigger the sweeper to
			// sweep the inputs.
			ctx.notifier.NotifyEpoch(ctx.sweeper.currentHeight + 1)

			// Check the sweeps transactions, ensuring all inputs
			// are there, and all the locktimes are satisfied.
			var sweeps []*wire.MsgTx
		Loop:
			for {
				select {
				case tx := <-ctx.publishChan:
					sweeps = append(sweeps, &tx)
				case <-time.After(200 * time.Millisecond):
					break Loop
				}
			}

			// Mine the sweeps.
			ctx.backend.mine()

			// Results should all come back.
			for _, resultChan := range results {
				result := <-resultChan
				if result.Err != nil {
					t.Fatalf("expected input to be "+
						"swept: %v", result.Err)
				}
			}

			// Assert the transactions are what we expect.
			testCase.assertSweeps(t, inputs, sweeps)

			// Finally we assert that all our test inputs were part
			// of the sweeps, and that they were signed correctly.
			sweptInputs := make(map[wire.OutPoint]struct{})
			for _, sweep := range sweeps {
				swept := assertSignedIndex(t, sweep, inputs)
				for op := range swept {
					if _, ok := sweptInputs[op]; ok {
						t.Fatalf("outpoint %v part of "+
							"previous sweep", op)
					}

					sweptInputs[op] = struct{}{}
				}
			}

			require.Equal(t, len(inputs), len(sweptInputs))
			for op := range sweptInputs {
				_, ok := inputs[op]
				if !ok {
					t.Fatalf("swept input %v not part of "+
						"test inputs", op)
				}
			}
		})
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
	// `pendingInputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.pendingInputs[inputInit.PreviousOutPoint] = &pendingInput{
		state: StateInit,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.pendingInputs[inputPendingPublish.PreviousOutPoint] = &pendingInput{
		state: StatePendingPublish,
	}

	// inputTerminated specifies an input that's terminated.
	inputTerminated := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 4},
	}
	s.pendingInputs[inputTerminated.PreviousOutPoint] = &pendingInput{
		state: StateExcluded,
	}

	// First, check that when an error is returned from db, it's properly
	// returned here.
	mockStore.On("StoreTx", dummyTR).Return(dummyErr).Once()
	err := s.markInputsPendingPublish(dummyTR, nil)
	require.ErrorIs(err, dummyErr)

	// Then, check that the target input has will be correctly marked as
	// published.
	//
	// Mock the store to return nil
	mockStore.On("StoreTx", dummyTR).Return(nil).Once()

	// Mark the test inputs. We expect the non-exist input and the
	// inputTerminated to be skipped, and the rest to be marked as pending
	// publish.
	err = s.markInputsPendingPublish(dummyTR, []*wire.TxIn{
		inputNotExist, inputInit, inputPendingPublish, inputTerminated,
	})
	require.NoError(err)

	// We expect unchanged number of pending inputs.
	require.Len(s.pendingInputs, 3)

	// We expect the init input's state to become pending publish.
	require.Equal(StatePendingPublish,
		s.pendingInputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish to stay unchanged.
	require.Equal(StatePendingPublish,
		s.pendingInputs[inputPendingPublish.PreviousOutPoint].state)

	// We expect the terminated to stay unchanged.
	require.Equal(StateExcluded,
		s.pendingInputs[inputTerminated.PreviousOutPoint].state)

	// Assert mocked statements are executed as expected.
	mockStore.AssertExpectations(t)
}

// TestMarkInputsPublished checks that given a list of inputs with different
// states, only the state `StatePendingPublish` will be marked as `Published`.
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
	// `pendingInputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input. When marking this as
	// published, we should see an error log as this input hasn't been
	// published yet.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.pendingInputs[inputInit.PreviousOutPoint] = &pendingInput{
		state: StateInit,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.pendingInputs[inputPendingPublish.PreviousOutPoint] = &pendingInput{
		state: StatePendingPublish,
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
	require.Len(s.pendingInputs, 2)

	// We expect the init input's state to stay unchanged.
	require.Equal(StateInit,
		s.pendingInputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish input's is now marked as published.
	require.Equal(StatePublished,
		s.pendingInputs[inputPendingPublish.PreviousOutPoint].state)

	// Assert mocked statements are executed as expected.
	mockStore.AssertExpectations(t)
}

// TestMarkInputsPublishFailed checks that given a list of inputs with
// different states, only the state `StatePendingPublish` will be marked as
// `PublishFailed`.
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
	// `pendingInputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input. When marking this as
	// published, we should see an error log as this input hasn't been
	// published yet.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.pendingInputs[inputInit.PreviousOutPoint] = &pendingInput{
		state: StateInit,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.pendingInputs[inputPendingPublish.PreviousOutPoint] = &pendingInput{
		state: StatePendingPublish,
	}

	// Mark the test inputs. We expect the non-exist input and the
	// inputInit to be skipped, and the final input to be marked as
	// published.
	s.markInputsPublishFailed([]*wire.TxIn{
		inputNotExist, inputInit, inputPendingPublish,
	})

	// We expect unchanged number of pending inputs.
	require.Len(s.pendingInputs, 2)

	// We expect the init input's state to stay unchanged.
	require.Equal(StateInit,
		s.pendingInputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish input's is now marked as publish
	// failed.
	require.Equal(StatePublishFailed,
		s.pendingInputs[inputPendingPublish.PreviousOutPoint].state)

	// Assert mocked statements are executed as expected.
	mockStore.AssertExpectations(t)
}

// TestMarkInputsSwept checks that given a list of inputs with different
// states, only the non-terminal state will be marked as `StateSwept`.
func TestMarkInputsSwept(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a mock input.
	mockInput := &input.MockInput{}
	defer mockInput.AssertExpectations(t)

	// Mock the `OutPoint` to return a dummy outpoint.
	mockInput.On("OutPoint").Return(&wire.OutPoint{Hash: chainhash.Hash{1}})

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create three testing inputs.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `pendingInputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.pendingInputs[inputInit.PreviousOutPoint] = &pendingInput{
		state: StateInit,
		Input: mockInput,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.pendingInputs[inputPendingPublish.PreviousOutPoint] = &pendingInput{
		state: StatePendingPublish,
		Input: mockInput,
	}

	// inputTerminated specifies an input that's terminated.
	inputTerminated := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 4},
	}
	s.pendingInputs[inputTerminated.PreviousOutPoint] = &pendingInput{
		state: StateExcluded,
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
	require.Len(s.pendingInputs, 3)

	// We expect the init input's state to become swept.
	require.Equal(StateSwept,
		s.pendingInputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish becomes swept.
	require.Equal(StateSwept,
		s.pendingInputs[inputPendingPublish.PreviousOutPoint].state)

	// We expect the terminated to stay unchanged.
	require.Equal(StateExcluded,
		s.pendingInputs[inputTerminated.PreviousOutPoint].state)
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

	// Create a list of inputs using all the states.
	input0 := &pendingInput{state: StateInit}
	input1 := &pendingInput{state: StatePendingPublish}
	input2 := &pendingInput{state: StatePublished}
	input3 := &pendingInput{state: StatePublishFailed}
	input4 := &pendingInput{state: StateSwept}
	input5 := &pendingInput{state: StateExcluded}
	input6 := &pendingInput{state: StateFailed}

	// Add the inputs to the sweeper. After the update, we should see the
	// terminated inputs being removed.
	s.pendingInputs = map[wire.OutPoint]*pendingInput{
		{Index: 0}: input0,
		{Index: 1}: input1,
		{Index: 2}: input2,
		{Index: 3}: input3,
		{Index: 4}: input4,
		{Index: 5}: input5,
		{Index: 6}: input6,
	}

	// We expect the inputs with `StateSwept`, `StateExcluded`, and
	// `StateFailed` to be removed.
	expectedInputs := map[wire.OutPoint]*pendingInput{
		{Index: 0}: input0,
		{Index: 1}: input1,
		{Index: 2}: input2,
		{Index: 3}: input3,
	}

	// We expect only the inputs with `StateInit` and `StatePublishFailed`
	// to be returned.
	expectedReturn := map[wire.OutPoint]*pendingInput{
		{Index: 0}: input0,
		{Index: 3}: input3,
	}

	// Update the sweeper inputs.
	inputs := s.updateSweeperInputs()

	// Assert the returned inputs are as expected.
	require.Equal(expectedReturn, inputs)

	// Assert the sweeper inputs are as expected.
	require.Equal(expectedInputs, s.pendingInputs)
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
	require.Equal(StateInit, state)

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
	require.Equal(StatePublished, state)

	// Mock the store to return a db error.
	dummyErr := errors.New("dummy error")
	mockStore.On("GetTx", tx.TxHash()).Return(nil, dummyErr).Once()

	// Although the db lookup failed, we expect the state to be Published.
	state, rbf = s.decideStateAndRBFInfo(op)
	require.True(rbf.IsNone())
	require.Equal(StatePublished, state)

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
	require.Equal(StatePublished, state)
}

// TestMarkInputFailed checks that the input is marked as failed as expected.
func TestMarkInputFailed(t *testing.T) {
	t.Parallel()

	// Create a mock input.
	mockInput := &input.MockInput{}
	defer mockInput.AssertExpectations(t)

	// Mock the `OutPoint` to return a dummy outpoint.
	mockInput.On("OutPoint").Return(&wire.OutPoint{Hash: chainhash.Hash{1}})

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create a testing pending input.
	pi := &pendingInput{
		state: StateInit,
		Input: mockInput,
	}

	// Call the method under test.
	s.markInputFailed(pi, errors.New("dummy error"))

	// Assert the state is updated.
	require.Equal(t, StateFailed, pi.state)
}

// TestSweepPendingInputs checks that `sweepPendingInputs` correctly executes
// its workflow based on the returned values from the interfaces.
func TestSweepPendingInputs(t *testing.T) {
	t.Parallel()

	// Create a mock wallet and aggregator.
	wallet := &MockWallet{}
	aggregator := &mockUtxoAggregator{}

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Wallet:     wallet,
		Aggregator: aggregator,
	})

	// Create an input set that needs wallet inputs.
	setNeedWallet := &MockInputSet{}

	// Mock this set to ask for wallet input.
	setNeedWallet.On("NeedWalletInput").Return(true).Once()
	setNeedWallet.On("AddWalletInputs", wallet).Return(nil).Once()

	// Mock the wallet to require the lock once.
	wallet.On("WithCoinSelectLock", mock.Anything).Return(nil).Once()

	// Create an input set that doesn't need wallet inputs.
	normalSet := &MockInputSet{}
	normalSet.On("NeedWalletInput").Return(false).Once()

	// Mock the methods used in `sweep`. This is not important for this
	// unit test.
	feeRate := chainfee.SatPerKWeight(1000)
	setNeedWallet.On("Inputs").Return(nil).Once()
	setNeedWallet.On("FeeRate").Return(feeRate).Once()
	normalSet.On("Inputs").Return(nil).Once()
	normalSet.On("FeeRate").Return(feeRate).Once()

	// Make pending inputs for testing. We don't need real values here as
	// the returned clusters are mocked.
	pis := make(pendingInputs)

	// Mock the aggregator to return the mocked input sets.
	aggregator.On("ClusterInputs", pis).Return([]InputSet{
		setNeedWallet, normalSet,
	})

	// Set change output script to an invalid value. This should cause the
	// `createSweepTx` inside `sweep` to fail. This is done so we can
	// terminate the method early as we are only interested in testing the
	// workflow in `sweepPendingInputs`. We don't need to test `sweep` here
	// as it should be tested in its own unit test.
	s.currentOutputScript = []byte{1}

	// Call the method under test.
	s.sweepPendingInputs(pis)

	// Assert mocked methods are called as expected.
	wallet.AssertExpectations(t)
	aggregator.AssertExpectations(t)
	setNeedWallet.AssertExpectations(t)
	normalSet.AssertExpectations(t)
}

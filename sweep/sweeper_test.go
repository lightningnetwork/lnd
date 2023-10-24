package sweep

import (
	"os"
	"reflect"
	"runtime/pprof"
	"sort"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

var (
	testLog = build.NewSubLogger("SWPR_TEST", nil)

	testMaxSweepAttempts = 3

	testMaxInputsPerTx = 3

	defaultFeePref = Params{Fee: FeePreference{ConfTarget: 1}}
)

type sweeperTestContext struct {
	t *testing.T

	sweeper   *UtxoSweeper
	notifier  *MockNotifier
	estimator *mockFeeEstimator
	backend   *mockBackend
	store     SweeperStore

	publishChan chan wire.MsgTx
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

	ctx := &sweeperTestContext{
		notifier:    notifier,
		publishChan: backend.publishChan,
		t:           t,
		estimator:   estimator,
		backend:     backend,
		store:       store,
	}

	ctx.sweeper = New(&UtxoSweeperConfig{
		Notifier:       notifier,
		Wallet:         backend,
		TickerDuration: 100 * time.Millisecond,
		Store:          store,
		Signer:         &mock.DummySigner{},
		GenSweepScript: func() ([]byte, error) {
			script := make([]byte, input.P2WPKHSize)
			script[0] = 0
			script[1] = 20
			return script, nil
		},
		FeeEstimator:     estimator,
		MaxInputsPerTx:   testMaxInputsPerTx,
		MaxSweepAttempts: testMaxSweepAttempts,
		NextAttemptDeltaFunc: func(attempts int) int32 {
			// Use delta func without random factor.
			return 1 << uint(attempts-1)
		},
		MaxFeeRate:        DefaultMaxFeeRate,
		FeeRateBucketSize: DefaultFeeRateBucketSize,
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
	_, err := ctx.sweeper.SweepInput(spendableInputs[0], Params{})
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
	if err != nil {
		t.Fatal(err)
	}

	// No sweep transaction is expected now. The sweeper should recognize
	// that the sweep output will not be relayed and not generate the tx. It
	// isn't possible to attach a wallet utxo either, because the added
	// weight would create a negatively yielding transaction at this fee
	// rate.

	// Sweep another input that brings the tx output above the dust limit.
	largeInput := createTestInput(100000, input.CommitmentTimeLock)

	_, err = ctx.sweeper.SweepInput(&largeInput, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	// The second input brings the sweep output above the dust limit. We
	// expect a sweep tx now.

	sweepTx := ctx.receiveTx()
	if len(sweepTx.TxIn) != 2 {
		t.Fatalf("Expected tx to sweep 2 inputs, but contains %v "+
			"inputs instead", len(sweepTx.TxIn))
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

	_, err := ctx.sweeper.SweepInput(
		&dustInput,
		Params{Fee: FeePreference{FeeRate: chainfee.FeePerKwFloor}},
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

// TestGiveUp asserts that the sweeper gives up on an input if it can't be swept
// after a configured number of attempts.a
func TestGiveUp(t *testing.T) {
	ctx := createSweeperTestContext(t)

	resultChan0, err := ctx.sweeper.SweepInput(
		spendableInputs[0], defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	// We expect a sweep to be published at height 100 (mockChainIOHeight).
	ctx.receiveTx()

	// Because of MaxSweepAttemps, two more sweeps will be attempted. We
	// configured exponential back-off without randomness for the test. The
	// second attempt, we expect to happen at 101. The third attempt at 103.
	// At that point, the input is expected to be failed.

	// Second attempt
	ctx.notifier.NotifyEpoch(101)
	ctx.receiveTx()

	// Third attempt
	ctx.notifier.NotifyEpoch(103)
	ctx.receiveTx()

	ctx.expectResult(resultChan0, ErrTooManyAttempts)

	ctx.backend.mine()

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
	lowFeePref := FeePreference{ConfTarget: 12}
	lowFeeRate := chainfee.SatPerKWeight(5000)
	ctx.estimator.blocksToFee[lowFeePref.ConfTarget] = lowFeeRate

	highFeePref := FeePreference{ConfTarget: 6}
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

	lowFeePref := FeePreference{
		ConfTarget: 12,
	}
	ctx.estimator.blocksToFee[lowFeePref.ConfTarget] = lowFeeRate

	highFeePref := FeePreference{
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

	// We should expect to see both sweep transactions broadcast. The higher
	// fee rate sweep should be broadcast first. We'll remove the lower fee
	// rate sweep to ensure we can detect pending inputs after a sweep.
	// Once the higher fee rate sweep confirms, we should no longer see
	// those inputs pending.
	ctx.receiveTx()
	lowFeeRateTx := ctx.receiveTx()
	ctx.backend.deleteUnconfirmed(lowFeeRateTx.TxHash())
	ctx.backend.mine()
	ctx.expectResult(resultChan1, nil)
	ctx.assertPendingInputs(input3)

	// We'll then trigger a new block to rebroadcast the lower fee rate
	// sweep. Once again we'll ensure those inputs are no longer pending
	// once the sweep transaction confirms.
	ctx.backend.notifier.NotifyEpoch(101)
	ctx.receiveTx()
	ctx.backend.mine()
	ctx.expectResult(resultChan3, nil)
	ctx.assertPendingInputs()

	ctx.finish(1)
}

// TestBumpFeeRBF ensures that the UtxoSweeper can properly handle a fee bump
// request for an input it is currently attempting to sweep. When sweeping the
// input with the higher fee rate, a replacement transaction is created.
func TestBumpFeeRBF(t *testing.T) {
	ctx := createSweeperTestContext(t)

	lowFeePref := FeePreference{ConfTarget: 144}
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
	highFeePref := FeePreference{ConfTarget: 6}
	highFeeRate := DefaultMaxFeeRate.FeePerKWeight()
	ctx.estimator.blocksToFee[highFeePref.ConfTarget] = highFeeRate

	// We should expect to see an error if a fee preference isn't provided.
	_, err = ctx.sweeper.UpdateParams(*input.OutPoint(), ParamsUpdate{})
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
				Fee:            FeePreference{ConfTarget: 6},
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

	feePref := FeePreference{ConfTarget: 6}
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

var (
	testInputsA = pendingInputs{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 1}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 2}: &pendingInput{},
	}

	testInputsB = pendingInputs{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 10}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 11}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 12}: &pendingInput{},
	}

	testInputsC = pendingInputs{
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 0}:  &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 1}:  &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 2}:  &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 10}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 11}: &pendingInput{},
		wire.OutPoint{Hash: chainhash.Hash{}, Index: 12}: &pendingInput{},
	}
)

// TestMergeClusters check that we properly can merge clusters together,
// according to their required locktime.
func TestMergeClusters(t *testing.T) {
	t.Parallel()

	lockTime1 := uint32(100)
	lockTime2 := uint32(200)

	testCases := []struct {
		name string
		a    inputCluster
		b    inputCluster
		res  []inputCluster
	}{
		{
			name: "max fee rate",
			a: inputCluster{
				sweepFeeRate: 5000,
				inputs:       testInputsA,
			},
			b: inputCluster{
				sweepFeeRate: 7000,
				inputs:       testInputsB,
			},
			res: []inputCluster{
				{
					sweepFeeRate: 7000,
					inputs:       testInputsC,
				},
			},
		},
		{
			name: "same locktime",
			a: inputCluster{
				lockTime:     &lockTime1,
				sweepFeeRate: 5000,
				inputs:       testInputsA,
			},
			b: inputCluster{
				lockTime:     &lockTime1,
				sweepFeeRate: 7000,
				inputs:       testInputsB,
			},
			res: []inputCluster{
				{
					lockTime:     &lockTime1,
					sweepFeeRate: 7000,
					inputs:       testInputsC,
				},
			},
		},
		{
			name: "diff locktime",
			a: inputCluster{
				lockTime:     &lockTime1,
				sweepFeeRate: 5000,
				inputs:       testInputsA,
			},
			b: inputCluster{
				lockTime:     &lockTime2,
				sweepFeeRate: 7000,
				inputs:       testInputsB,
			},
			res: []inputCluster{
				{
					lockTime:     &lockTime1,
					sweepFeeRate: 5000,
					inputs:       testInputsA,
				},
				{
					lockTime:     &lockTime2,
					sweepFeeRate: 7000,
					inputs:       testInputsB,
				},
			},
		},
	}

	for _, test := range testCases {
		merged := mergeClusters(test.a, test.b)
		if !reflect.DeepEqual(merged, test.res) {
			t.Fatalf("[%s] unexpected result: %v",
				test.name, spew.Sdump(merged))
		}
	}
}

// TestZipClusters tests that we can merge lists of inputs clusters correctly.
func TestZipClusters(t *testing.T) {
	t.Parallel()

	createCluster := func(inp pendingInputs, f chainfee.SatPerKWeight) inputCluster {
		return inputCluster{
			sweepFeeRate: f,
			inputs:       inp,
		}
	}

	testCases := []struct {
		name string
		as   []inputCluster
		bs   []inputCluster
		res  []inputCluster
	}{
		{
			name: "merge A into B",
			as: []inputCluster{
				createCluster(testInputsA, 5000),
			},
			bs: []inputCluster{
				createCluster(testInputsB, 7000),
			},
			res: []inputCluster{
				createCluster(testInputsC, 7000),
			},
		},
		{
			name: "A can't merge with B",
			as: []inputCluster{
				createCluster(testInputsA, 7000),
			},
			bs: []inputCluster{
				createCluster(testInputsB, 5000),
			},
			res: []inputCluster{
				createCluster(testInputsA, 7000),
				createCluster(testInputsB, 5000),
			},
		},
		{
			name: "empty bs",
			as: []inputCluster{
				createCluster(testInputsA, 7000),
			},
			bs: []inputCluster{},
			res: []inputCluster{
				createCluster(testInputsA, 7000),
			},
		},
		{
			name: "empty as",
			as:   []inputCluster{},
			bs: []inputCluster{
				createCluster(testInputsB, 5000),
			},
			res: []inputCluster{
				createCluster(testInputsB, 5000),
			},
		},

		{
			name: "zip 3xA into 3xB",
			as: []inputCluster{
				createCluster(testInputsA, 5000),
				createCluster(testInputsA, 5000),
				createCluster(testInputsA, 5000),
			},
			bs: []inputCluster{
				createCluster(testInputsB, 7000),
				createCluster(testInputsB, 7000),
				createCluster(testInputsB, 7000),
			},
			res: []inputCluster{
				createCluster(testInputsC, 7000),
				createCluster(testInputsC, 7000),
				createCluster(testInputsC, 7000),
			},
		},
		{
			name: "zip A into 3xB",
			as: []inputCluster{
				createCluster(testInputsA, 2500),
			},
			bs: []inputCluster{
				createCluster(testInputsB, 3000),
				createCluster(testInputsB, 2000),
				createCluster(testInputsB, 1000),
			},
			res: []inputCluster{
				createCluster(testInputsC, 3000),
				createCluster(testInputsB, 2000),
				createCluster(testInputsB, 1000),
			},
		},
	}

	for _, test := range testCases {
		zipped := zipClusters(test.as, test.bs)
		if !reflect.DeepEqual(zipped, test.res) {
			t.Fatalf("[%s] unexpected result: %v",
				test.name, spew.Sdump(zipped))
		}
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
				Fee: FeePreference{ConfTarget: 6},
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
				Fee: FeePreference{ConfTarget: 6},
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
		t.Fatalf("had unsweeped inputs")
	}

	// Mine the first sweeps
	ctx.backend.mine()

	// Results should all come back.
	for i := range results {
		result := <-results[i]
		if result.Err != nil {
			t.Fatal("expected input to be swept")
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
						Fee: FeePreference{ConfTarget: 6},
					},
				)
				if err != nil {
					t.Fatal(err)
				}

				op := inp.OutPoint()
				results[*op] = result
				inputs[*op] = inp
			}

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

// TestClusterByLockTime tests the method clusterByLockTime works as expected.
func TestClusterByLockTime(t *testing.T) {
	t.Parallel()

	// Create a test param with a dummy fee preference. This is needed so
	// `feeRateForPreference` won't throw an error.
	param := Params{Fee: FeePreference{ConfTarget: 1}}

	// We begin the test by creating three clusters of inputs, the first
	// cluster has a locktime of 1, the second has a locktime of 2, and the
	// final has no locktime.
	lockTime1 := uint32(1)
	lockTime2 := uint32(2)

	// Create cluster one, which has a locktime of 1.
	input1LockTime1 := &input.MockInput{}
	input2LockTime1 := &input.MockInput{}
	input1LockTime1.On("RequiredLockTime").Return(lockTime1, true)
	input2LockTime1.On("RequiredLockTime").Return(lockTime1, true)

	// Create cluster two, which has a locktime of 2.
	input3LockTime2 := &input.MockInput{}
	input4LockTime2 := &input.MockInput{}
	input3LockTime2.On("RequiredLockTime").Return(lockTime2, true)
	input4LockTime2.On("RequiredLockTime").Return(lockTime2, true)

	// Create cluster three, which has no locktime.
	input5NoLockTime := &input.MockInput{}
	input6NoLockTime := &input.MockInput{}
	input5NoLockTime.On("RequiredLockTime").Return(uint32(0), false)
	input6NoLockTime.On("RequiredLockTime").Return(uint32(0), false)

	// With the inner Input being mocked, we can now create the pending
	// inputs.
	input1 := &pendingInput{Input: input1LockTime1, params: param}
	input2 := &pendingInput{Input: input2LockTime1, params: param}
	input3 := &pendingInput{Input: input3LockTime2, params: param}
	input4 := &pendingInput{Input: input4LockTime2, params: param}
	input5 := &pendingInput{Input: input5NoLockTime, params: param}
	input6 := &pendingInput{Input: input6NoLockTime, params: param}

	// Create the pending inputs map, which will be passed to the method
	// under test.
	//
	// NOTE: we don't care the actual outpoint values as long as they are
	// unique.
	inputs := pendingInputs{
		wire.OutPoint{Index: 1}: input1,
		wire.OutPoint{Index: 2}: input2,
		wire.OutPoint{Index: 3}: input3,
		wire.OutPoint{Index: 4}: input4,
		wire.OutPoint{Index: 5}: input5,
		wire.OutPoint{Index: 6}: input6,
	}

	// Create expected clusters so we can shorten the line length in the
	// test cases below.
	cluster1 := pendingInputs{
		wire.OutPoint{Index: 1}: input1,
		wire.OutPoint{Index: 2}: input2,
	}
	cluster2 := pendingInputs{
		wire.OutPoint{Index: 3}: input3,
		wire.OutPoint{Index: 4}: input4,
	}

	// cluster3 should be the remaining inputs since they don't have
	// locktime.
	cluster3 := pendingInputs{
		wire.OutPoint{Index: 5}: input5,
		wire.OutPoint{Index: 6}: input6,
	}

	// Set the min fee rate to be 1000 sat/kw.
	const minFeeRate = chainfee.SatPerKWeight(1000)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		MaxFeeRate: minFeeRate.FeePerVByte() * 10,
	})

	// Set the relay fee to be the minFeeRate. Any fee rate below the
	// minFeeRate will cause an error to be returned.
	s.relayFeeRate = minFeeRate

	// applyFeeRate takes a testing fee rate and makes a mocker over
	// DetermineFeePerKw that always return the testing fee rate. This
	// mocked method is then attached to the sweeper.
	applyFeeRate := func(feeRate chainfee.SatPerKWeight) {
		// TODO(yy): fix the test here.
	}

	testCases := []struct {
		name                    string
		testFeeRate             chainfee.SatPerKWeight
		expectedClusters        []inputCluster
		expectedRemainingInputs pendingInputs
	}{
		{
			// Test a successful case where the locktime clusters
			// are created and the no-locktime cluster is returned
			// as the remaining inputs.
			name: "successfully create clusters",
			// Use a fee rate above the min value so we don't hit
			// an error when performing fee estimation.
			//
			// TODO(yy): we should customize the returned fee rate
			// for each input to further test the averaging logic.
			// Or we can split the method into two, one for
			// grouping the clusters and the other for averaging
			// the fee rates so it's easier to be tested.
			testFeeRate: minFeeRate + 1,
			expectedClusters: []inputCluster{
				{
					lockTime:     &lockTime1,
					sweepFeeRate: minFeeRate + 1,
					inputs:       cluster1,
				},
				{
					lockTime:     &lockTime2,
					sweepFeeRate: minFeeRate + 1,
					inputs:       cluster2,
				},
			},
			expectedRemainingInputs: cluster3,
		},
		{
			// Test that when the input is skipped when the fee
			// estimation returns an error.
			name: "error from fee estimation",
			// Use a fee rate below the min value so we hit an
			// error when performing fee estimation.
			testFeeRate:      minFeeRate - 1,
			expectedClusters: []inputCluster{},
			// Remaining inputs should stay untouched.
			expectedRemainingInputs: cluster3,
		},
	}

	//nolint:paralleltest
	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Apply the test fee rate so `feeRateForPreference` is
			// mocked to return the specified value.
			applyFeeRate(tc.testFeeRate)

			// Call the method under test.
			clusters, remainingInputs := s.clusterByLockTime(inputs)

			// Sort by locktime as the order is not guaranteed.
			sort.Slice(clusters, func(i, j int) bool {
				return *clusters[i].lockTime <
					*clusters[j].lockTime
			})

			// Validate the values are returned as expected.
			require.Equal(t, tc.expectedClusters, clusters)
			require.Equal(t, tc.expectedRemainingInputs,
				remainingInputs,
			)

			// Assert the mocked methods are called as expected.
			input1LockTime1.AssertExpectations(t)
			input2LockTime1.AssertExpectations(t)
			input3LockTime2.AssertExpectations(t)
			input4LockTime2.AssertExpectations(t)
			input5NoLockTime.AssertExpectations(t)
			input6NoLockTime.AssertExpectations(t)
		})
	}
}

// TestGetInputLists checks that the expected input sets are returned based on
// whether there are retried inputs or not.
func TestGetInputLists(t *testing.T) {
	t.Parallel()

	// Create a test param with a dummy fee preference. This is needed so
	// `feeRateForPreference` won't throw an error.
	param := Params{Fee: FeePreference{ConfTarget: 1}}

	// Create a mock input and mock all the methods used in this test.
	testInput := &input.MockInput{}
	testInput.On("RequiredLockTime").Return(0, false)
	testInput.On("WitnessType").Return(input.CommitmentAnchor)
	testInput.On("OutPoint").Return(&wire.OutPoint{Index: 1})
	testInput.On("RequiredTxOut").Return(nil)
	testInput.On("UnconfParent").Return(nil)
	testInput.On("SignDesc").Return(&input.SignDescriptor{
		Output: &wire.TxOut{Value: 100_000},
	})

	// Create a new and a retried input.
	//
	// NOTE: we use the same input.Input for both pending inputs as we only
	// test the logic of returning the correct non-nil input sets, and not
	// the content the of sets. To validate the content of the sets, we
	// should test `generateInputPartitionings` instead.
	newInput := &pendingInput{
		Input:  testInput,
		params: param,
	}
	oldInput := &pendingInput{
		Input:           testInput,
		params:          param,
		publishAttempts: 1,
	}

	// clusterNew contains only new inputs.
	clusterNew := pendingInputs{
		wire.OutPoint{Index: 1}: newInput,
	}

	// clusterMixed contains a mixed of new and retried inputs.
	clusterMixed := pendingInputs{
		wire.OutPoint{Index: 1}: newInput,
		wire.OutPoint{Index: 2}: oldInput,
	}

	// clusterOld contains only retried inputs.
	clusterOld := pendingInputs{
		wire.OutPoint{Index: 2}: oldInput,
	}

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		MaxInputsPerTx: DefaultMaxInputsPerTx,
	})

	testCases := []struct {
		name              string
		cluster           inputCluster
		expectedNilAllSet bool
		expectNilNewSet   bool
	}{
		{
			// When there are only new inputs, we'd expect the
			// first returned set(allSets) to be empty.
			name:              "new inputs only",
			cluster:           inputCluster{inputs: clusterNew},
			expectedNilAllSet: true,
			expectNilNewSet:   false,
		},
		{
			// When there are only retried inputs, we'd expect the
			// second returned set(newSet) to be empty.
			name:              "retried inputs only",
			cluster:           inputCluster{inputs: clusterOld},
			expectedNilAllSet: false,
			expectNilNewSet:   true,
		},
		{
			// When there are mixed inputs, we'd expect two sets
			// are returned.
			name:              "mixed inputs",
			cluster:           inputCluster{inputs: clusterMixed},
			expectedNilAllSet: false,
			expectNilNewSet:   false,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			allSets, newSets, err := s.getInputLists(tc.cluster)
			require.NoError(t, err)

			if tc.expectNilNewSet {
				require.Nil(t, newSets)
			}

			if tc.expectedNilAllSet {
				require.Nil(t, allSets)
			}
		})
	}
}

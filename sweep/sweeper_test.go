package sweep

import (
	"os"
	"runtime/debug"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	testLog = build.NewSubLogger("SWPR_TEST", nil)

	testMaxSweepAttempts = 3

	testMaxInputsPerTx = 3

	defaultFeePref = FeePreference{ConfTarget: 1}
)

type sweeperTestContext struct {
	t *testing.T

	sweeper   *UtxoSweeper
	notifier  *MockNotifier
	estimator *mockFeeEstimator
	backend   *mockBackend
	store     *MockSweeperStore

	timeoutChan chan chan time.Time
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
	}, btcec.S256())
)

func createTestInput(value int64, witnessType input.WitnessType) input.BaseInput {
	hash := chainhash.Hash{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		byte(testInputCount)}

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
	)

	testInputCount++

	return input
}

func init() {
	// Create a set of test spendable inputs.
	for i := 0; i < 5; i++ {
		input := createTestInput(int64(10000+i*500),
			input.CommitmentTimeLock)

		spendableInputs = append(spendableInputs, &input)
	}
}

func createSweeperTestContext(t *testing.T) *sweeperTestContext {
	notifier := NewMockNotifier(t)

	store := NewMockSweeperStore()

	backend := newMockBackend(notifier)

	estimator := newMockFeeEstimator(10000, lnwallet.FeePerKwFloor)

	publishChan := make(chan wire.MsgTx, 2)
	ctx := &sweeperTestContext{
		notifier:    notifier,
		publishChan: publishChan,
		t:           t,
		estimator:   estimator,
		backend:     backend,
		store:       store,
		timeoutChan: make(chan chan time.Time, 1),
	}

	var outputScriptCount byte
	ctx.sweeper = New(&UtxoSweeperConfig{
		Notifier: notifier,
		PublishTransaction: func(tx *wire.MsgTx) error {
			log.Tracef("Publishing tx %v", tx.TxHash())
			err := backend.publishTransaction(tx)
			select {
			case publishChan <- *tx:
			case <-time.After(defaultTestTimeout):
				t.Fatalf("unexpected tx published")
			}
			return err
		},
		NewBatchTimer: func() <-chan time.Time {
			c := make(chan time.Time, 1)
			ctx.timeoutChan <- c
			return c
		},
		Store:   store,
		Signer:  &mockSigner{},
		ChainIO: &mockChainIO{},
		GenSweepScript: func() ([]byte, error) {
			script := []byte{outputScriptCount}
			outputScriptCount++
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

func (ctx *sweeperTestContext) tick() {
	testLog.Trace("Waiting for tick to be consumed")
	select {
	case c := <-ctx.timeoutChan:
		select {
		case c <- time.Time{}:
			testLog.Trace("Tick")
		case <-time.After(defaultTestTimeout):
			debug.PrintStack()
			ctx.t.Fatal("tick timeout - tick not consumed")
		}
	case <-time.After(defaultTestTimeout):
		debug.PrintStack()
		ctx.t.Fatal("tick timeout - no new timer created")
	}
}

func (ctx *sweeperTestContext) assertNoNewTimer() {
	select {
	case <-ctx.timeoutChan:
		ctx.t.Fatal("no new timer expected")
	default:
	}
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
	ctx.assertNoNewTimer()
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

// receiveSpendTx receives the transaction sent through the given resultChan.
func receiveSpendTx(t *testing.T, resultChan chan Result) *wire.MsgTx {
	t.Helper()

	var result Result
	select {
	case result = <-resultChan:
	case <-time.After(5 * time.Second):
		t.Fatal("no sweep result received")
	}

	if result.Err != nil {
		t.Fatalf("expected successful spend, but received error "+
			"\"%v\" instead", result.Err)
	}

	return result.Tx
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
	expectedFeeRate lnwallet.SatPerKWeight, inputs ...input.Input) {

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
	_, txWeight, _, _ := getWeightEstimate(inputs)

	expectedFee := expectedFeeRate.FeeForWeight(txWeight)
	if fee != expectedFee {
		t.Fatalf("expected fee rate %v results in %v fee, got %v fee",
			expectedFeeRate, expectedFee, fee)
	}
}

// TestSuccess tests the sweeper happy flow.
func TestSuccess(t *testing.T) {
	ctx := createSweeperTestContext(t)

	resultChan, err := ctx.sweeper.SweepInput(
		spendableInputs[0], defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx.tick()

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

	// Assert that last tx is stored in the database so we can republish
	// on restart.
	lastTx, err := ctx.store.GetLastPublishedTx()
	if err != nil {
		t.Fatal(err)
	}
	if lastTx == nil || sweepTx.TxHash() != lastTx.TxHash() {
		t.Fatalf("last tx not stored")
	}
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
	// that the sweep output will not be relayed and not generate the tx.

	// Sweep another input that brings the tx output above the dust limit.
	largeInput := createTestInput(100000, input.CommitmentTimeLock)

	_, err = ctx.sweeper.SweepInput(&largeInput, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	ctx.tick()

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

	ctx.tick()

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

	ctx.tick()

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

	ctx.tick()

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
		ctx.tick()

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
		ctx.tick()
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

	ctx.tick()

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
	ctx.tick()

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
	if _, err := ctx.sweeper.SweepInput(input1, defaultFeePref); err != nil {
		t.Fatal(err)
	}
	ctx.tick()

	ctx.receiveTx()

	// Restart sweeper.
	ctx.restartSweeper()

	// Expect last tx to be republished.
	ctx.receiveTx()

	// Simulate other subsystem (eg contract resolver) re-offering inputs.
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
	ctx.tick()

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

	// Expect last tx to be republished.
	ctx.receiveTx()

	ctx.finish(1)
}

// TestRestartRemoteSpend asserts that the sweeper picks up sweeping properly after
// a restart with remote spend.
func TestRestartRemoteSpend(t *testing.T) {

	ctx := createSweeperTestContext(t)

	// Sweep input.
	input1 := spendableInputs[0]
	if _, err := ctx.sweeper.SweepInput(input1, defaultFeePref); err != nil {
		t.Fatal(err)
	}

	// Sweep another input.
	input2 := spendableInputs[1]
	if _, err := ctx.sweeper.SweepInput(input2, defaultFeePref); err != nil {
		t.Fatal(err)
	}

	ctx.tick()

	sweepTx := ctx.receiveTx()

	// Restart sweeper.
	ctx.restartSweeper()

	// Expect last tx to be republished.
	ctx.receiveTx()

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

	// Simulate other subsystem (eg contract resolver) re-offering input 0.
	spendChan, err := ctx.sweeper.SweepInput(input1, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	// Expect sweeper to construct a new tx, because input 1 was spend
	// remotely.
	ctx.tick()

	ctx.receiveTx()

	ctx.backend.mine()

	ctx.expectResult(spendChan, nil)

	ctx.finish(1)
}

// TestRestartConfirmed asserts that the sweeper picks up sweeping properly after
// a restart with a confirm of our own sweep tx.
func TestRestartConfirmed(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Sweep input.
	input := spendableInputs[0]
	if _, err := ctx.sweeper.SweepInput(input, defaultFeePref); err != nil {
		t.Fatal(err)
	}

	ctx.tick()

	ctx.receiveTx()

	// Restart sweeper.
	ctx.restartSweeper()

	// Expect last tx to be republished.
	ctx.receiveTx()

	// Mine the sweep tx.
	ctx.backend.mine()

	// Simulate other subsystem (eg contract resolver) re-offering input 0.
	spendChan, err := ctx.sweeper.SweepInput(input, defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	// Here we expect again a successful sweep.
	ctx.expectResult(spendChan, nil)

	// Timer started but not needed because spend ntfn was sent.
	ctx.tick()

	ctx.finish(1)
}

// TestRestartRepublish asserts that sweeper republishes the last published
// tx on restart.
func TestRestartRepublish(t *testing.T) {
	ctx := createSweeperTestContext(t)

	_, err := ctx.sweeper.SweepInput(spendableInputs[0], defaultFeePref)
	if err != nil {
		t.Fatal(err)
	}

	ctx.tick()

	sweepTx := ctx.receiveTx()

	// Restart sweeper again. No action is expected.
	ctx.restartSweeper()

	republishedTx := ctx.receiveTx()

	if sweepTx.TxHash() != republishedTx.TxHash() {
		t.Fatalf("last tx not republished")
	}

	// Mine the tx to conclude the test properly.
	ctx.backend.mine()

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

	ctx.tick()

	// We expect a sweep to be published.
	ctx.receiveTx()

	// New block arrives. This should trigger a new sweep attempt timer
	// start.
	ctx.notifier.NotifyEpoch(1000)

	// Offer a fresh input.
	resultChan1, err := ctx.sweeper.SweepInput(
		spendableInputs[1], defaultFeePref,
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx.tick()

	// Two txes are expected to be published, because new and retry inputs
	// are separated.
	ctx.receiveTx()
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

	ctx.tick()

	// We expect a sweep to be published at height 100 (mockChainIOHeight).
	ctx.receiveTx()

	// Because of MaxSweepAttemps, two more sweeps will be attempted. We
	// configured exponential back-off without randomness for the test. The
	// second attempt, we expect to happen at 101. The third attempt at 103.
	// At that point, the input is expected to be failed.

	// Second attempt
	ctx.notifier.NotifyEpoch(101)
	ctx.tick()
	ctx.receiveTx()

	// Third attempt
	ctx.notifier.NotifyEpoch(103)
	ctx.tick()
	ctx.receiveTx()

	ctx.expectResult(resultChan0, ErrTooManyAttempts)

	ctx.backend.mine()

	ctx.finish(1)
}

// TestDifferentFeePreferences ensures that the sweeper can have different
// transactions for different fee preferences.
func TestDifferentFeePreferences(t *testing.T) {
	ctx := createSweeperTestContext(t)

	// Throughout this test, we'll be attempting to sweep three inputs, two
	// with the higher fee preference, and the last with the lower. We do
	// this to ensure the sweeper can broadcast distinct transactions for
	// each sweep with a different fee preference.
	lowFeePref := FeePreference{
		ConfTarget: 12,
	}
	ctx.estimator.blocksToFee[lowFeePref.ConfTarget] = 5000
	highFeePref := FeePreference{
		ConfTarget: 6,
	}
	ctx.estimator.blocksToFee[highFeePref.ConfTarget] = 10000

	input1 := spendableInputs[0]
	resultChan1, err := ctx.sweeper.SweepInput(input1, highFeePref)
	if err != nil {
		t.Fatal(err)
	}
	input2 := spendableInputs[1]
	resultChan2, err := ctx.sweeper.SweepInput(input2, highFeePref)
	if err != nil {
		t.Fatal(err)
	}
	input3 := spendableInputs[2]
	resultChan3, err := ctx.sweeper.SweepInput(input3, lowFeePref)
	if err != nil {
		t.Fatal(err)
	}

	// Start the sweeper's batch ticker, which should cause the sweep
	// transactions to be broadcast.
	ctx.tick()

	ctx.receiveTx()
	ctx.receiveTx()

	// With the transactions broadcast, we'll mine a block to so that the
	// result is delivered to each respective client.
	ctx.backend.mine()

	// We should expect to see a single transaction that sweeps the high fee
	// preference inputs.
	sweepTx1 := receiveSpendTx(t, resultChan1)
	assertTxSweepsInputs(t, sweepTx1, input1, input2)
	sweepTx2 := receiveSpendTx(t, resultChan2)
	assertTxSweepsInputs(t, sweepTx2, input1, input2)

	// We should expect to see a distinct transaction that sweeps the low
	// fee preference inputs.
	sweepTx3 := receiveSpendTx(t, resultChan3)
	assertTxSweepsInputs(t, sweepTx3, input3)

	ctx.finish(1)
}

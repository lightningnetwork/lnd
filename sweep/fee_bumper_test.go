package sweep

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	// Create  a taproot change script.
	changePkScript = []byte{
		0x51, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	testInputCount atomic.Uint64
)

func createTestInput(value int64,
	witnessType input.WitnessType) input.BaseInput {

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

// TestBumpResultValidate tests the validate method of the BumpResult struct.
func TestBumpResultValidate(t *testing.T) {
	t.Parallel()

	// An empty result will give an error.
	b := BumpResult{}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// Unknown event type will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: sentinalEvent,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A replacing event without a new tx will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxReplaced,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A failed event without a failure reason will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxFailed,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// A confirmed event without fee info will give an error.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxConfirmed,
	}
	require.ErrorIs(t, b.Validate(), ErrInvalidBumpResult)

	// Test a valid result.
	b = BumpResult{
		Tx:    &wire.MsgTx{},
		Event: TxPublished,
	}
	require.NoError(t, b.Validate())
}

// TestCalcSweepTxWeight checks that the weight of the sweep tx is calculated
// correctly.
func TestCalcSweepTxWeight(t *testing.T) {
	t.Parallel()

	// Create an input.
	inp := createTestInput(100, input.WitnessKeyHash)

	// Use a wrong change script to test the error case.
	weight, err := calcSweepTxWeight([]input.Input{&inp}, []byte{0})
	require.Error(t, err)
	require.Zero(t, weight)

	// Use a correct change script to test the success case.
	weight, err = calcSweepTxWeight([]input.Input{&inp}, changePkScript)
	require.NoError(t, err)

	// BaseTxSize 8 bytes
	// InputSize 1+41 bytes
	// One P2TROutputSize 1+43 bytes
	// One P2WKHWitnessSize 2+109 bytes
	// Total weight = (8+42+44) * 4 + 111 = 487
	require.EqualValuesf(t, 487, weight, "unexpected weight %v", weight)
}

// TestBumpRequestMaxFeeRateAllowed tests the max fee rate allowed for a bump
// request.
func TestBumpRequestMaxFeeRateAllowed(t *testing.T) {
	t.Parallel()

	// Create a test input.
	inp := createTestInput(100, input.WitnessKeyHash)

	// The weight is 487.
	weight, err := calcSweepTxWeight([]input.Input{&inp}, changePkScript)
	require.NoError(t, err)

	// Define a test budget and calculates its fee rate.
	budget := btcutil.Amount(1000)
	budgetFeeRate := chainfee.NewSatPerKWeight(budget, weight)

	testCases := []struct {
		name               string
		req                *BumpRequest
		expectedMaxFeeRate chainfee.SatPerKWeight
		expectedErr        bool
	}{
		{
			// Use a wrong change script to test the error case.
			name: "error calc weight",
			req: &BumpRequest{
				DeliveryAddress: []byte{1},
			},
			expectedMaxFeeRate: 0,
			expectedErr:        true,
		},
		{
			// When the budget cannot give a fee rate that matches
			// the supplied MaxFeeRate, the max allowed feerate is
			// capped by the budget.
			name: "use budget as max fee rate",
			req: &BumpRequest{
				DeliveryAddress: changePkScript,
				Inputs:          []input.Input{&inp},
				Budget:          budget,
				MaxFeeRate:      budgetFeeRate + 1,
			},
			expectedMaxFeeRate: budgetFeeRate,
		},
		{
			// When the budget can give a fee rate that matches the
			// supplied MaxFeeRate, the max allowed feerate is
			// capped by the MaxFeeRate.
			name: "use config as max fee rate",
			req: &BumpRequest{
				DeliveryAddress: changePkScript,
				Inputs:          []input.Input{&inp},
				Budget:          budget,
				MaxFeeRate:      budgetFeeRate - 1,
			},
			expectedMaxFeeRate: budgetFeeRate - 1,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Check the method under test.
			maxFeeRate, err := tc.req.MaxFeeRateAllowed()

			// If we expect an error, check the error is returned
			// and the feerate is empty.
			if tc.expectedErr {
				require.Error(t, err)
				require.Zero(t, maxFeeRate)

				return
			}

			// Otherwise, check the max fee rate is as expected.
			require.NoError(t, err)
			require.Equal(t, tc.expectedMaxFeeRate, maxFeeRate)
		})
	}
}

// TestCalcCurrentConfTarget checks that the current confirmation target is
// calculated correctly.
func TestCalcCurrentConfTarget(t *testing.T) {
	t.Parallel()

	// When the current block height is 100 and deadline height is 200, the
	// conf target should be 100.
	conf := calcCurrentConfTarget(int32(100), int32(200))
	require.EqualValues(t, 100, conf)

	// When the current block height is 200 and deadline height is 100, the
	// conf target should be 0 since the deadline has passed.
	conf = calcCurrentConfTarget(int32(200), int32(100))
	require.EqualValues(t, 0, conf)
}

// TestInitializeFeeFunction tests the initialization of the fee function.
func TestInitializeFeeFunction(t *testing.T) {
	t.Parallel()

	// Create a test input.
	inp := createTestInput(100, input.WitnessKeyHash)

	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}
	defer estimator.AssertExpectations(t)

	// Create a publisher using the mocks.
	tp := NewTxPublisher(TxPublisherConfig{
		Estimator: estimator,
	})

	// Create a test feerate.
	feerate := chainfee.SatPerKWeight(1000)

	// Create a testing bump request.
	req := &BumpRequest{
		DeliveryAddress: changePkScript,
		Inputs:          []input.Input{&inp},
		Budget:          btcutil.Amount(1000),
		MaxFeeRate:      feerate * 10,
		DeadlineHeight:  10,
	}

	// Mock the fee estimator to return an error.
	//
	// We are not testing `NewLinearFeeFunction` here, so the actual params
	// used are irrelevant.
	dummyErr := fmt.Errorf("dummy error")
	estimator.On("EstimateFeePerKW", mock.Anything).Return(
		chainfee.SatPerKWeight(0), dummyErr).Once()

	// Call the method under test and assert the error is returned.
	f, err := tp.initializeFeeFunction(req)
	require.ErrorIs(t, err, dummyErr)
	require.Nil(t, f)

	// Mock the fee estimator to return the testing fee rate.
	//
	// We are not testing `NewLinearFeeFunction` here, so the actual params
	// used are irrelevant.
	estimator.On("EstimateFeePerKW", mock.Anything).Return(
		feerate, nil).Once()
	estimator.On("RelayFeePerKW").Return(chainfee.FeePerKwFloor).Once()

	// Call the method under test.
	f, err = tp.initializeFeeFunction(req)
	require.NoError(t, err)
	require.Equal(t, feerate, f.FeeRate())
}

// TestStoreRecord correctly increases the request counter and saves the
// record.
func TestStoreRecord(t *testing.T) {
	t.Parallel()

	// Create a test input.
	inp := createTestInput(1000, input.WitnessKeyHash)

	// Create a bump request.
	req := &BumpRequest{
		DeliveryAddress: changePkScript,
		Inputs:          []input.Input{&inp},
		Budget:          btcutil.Amount(1000),
	}

	// Create a naive fee function.
	feeFunc := &LinearFeeFunction{}

	// Create a test fee and tx.
	fee := btcutil.Amount(1000)
	tx := &wire.MsgTx{}

	// Create a publisher using the mocks.
	tp := NewTxPublisher(TxPublisherConfig{})

	// Get the current counter and check it's increased later.
	initialCounter := tp.requestCounter.Load()

	// Call the method under test.
	requestID := tp.storeRecord(tx, req, feeFunc, fee)

	// Check the request ID is as expected.
	require.Equal(t, initialCounter+1, requestID)

	// Read the saved record and compare.
	record, ok := tp.records.Load(requestID)
	require.True(t, ok)
	require.Equal(t, tx, record.tx)
	require.Equal(t, feeFunc, record.feeFunction)
	require.Equal(t, fee, record.fee)
	require.Equal(t, req, record.req)
}

// mockers wraps a list of mocked interfaces used inside tx publisher.
type mockers struct {
	signer    *input.MockInputSigner
	wallet    *MockWallet
	estimator *chainfee.MockEstimator
	notifier  *chainntnfs.MockChainNotifier

	feeFunc *MockFeeFunction
}

// createTestPublisher creates a new tx publisher using the provided mockers.
func createTestPublisher(t *testing.T) (*TxPublisher, *mockers) {
	// Create a mock fee estimator.
	estimator := &chainfee.MockEstimator{}

	// Create a mock fee function.
	feeFunc := &MockFeeFunction{}

	// Create a mock signer.
	signer := &input.MockInputSigner{}

	// Create a mock wallet.
	wallet := &MockWallet{}

	// Create a mock chain notifier.
	notifier := &chainntnfs.MockChainNotifier{}

	t.Cleanup(func() {
		estimator.AssertExpectations(t)
		feeFunc.AssertExpectations(t)
		signer.AssertExpectations(t)
		wallet.AssertExpectations(t)
		notifier.AssertExpectations(t)
	})

	m := &mockers{
		signer:    signer,
		wallet:    wallet,
		estimator: estimator,
		notifier:  notifier,
		feeFunc:   feeFunc,
	}

	// Create a publisher using the mocks.
	tp := NewTxPublisher(TxPublisherConfig{
		Estimator: m.estimator,
		Signer:    m.signer,
		Wallet:    m.wallet,
		Notifier:  m.notifier,
	})

	return tp, m
}

// TestCreateAndCheckTx checks `createAndCheckTx` behaves as expected.
func TestCreateAndCheckTx(t *testing.T) {
	t.Parallel()

	// Create a test request.
	inp := createTestInput(1000, input.WitnessKeyHash)

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test feerate and return it from the mock fee function.
	feerate := chainfee.SatPerKWeight(1000)
	m.feeFunc.On("FeeRate").Return(feerate)

	// Mock the wallet to fail on testmempoolaccept on the first call, and
	// succeed on the second.
	m.wallet.On("CheckMempoolAcceptance",
		mock.Anything).Return(errDummy).Once()
	m.wallet.On("CheckMempoolAcceptance", mock.Anything).Return(nil).Once()

	// Mock the signer to always return a valid script.
	//
	// NOTE: we are not testing the utility of creating valid txes here, so
	// this is fine to be mocked. This behaves essentially as skipping the
	// Signer check and alaways assume the tx has a valid sig.
	script := &input.Script{}
	m.signer.On("ComputeInputScript", mock.Anything,
		mock.Anything).Return(script, nil)

	testCases := []struct {
		name        string
		req         *BumpRequest
		expectedErr error
	}{
		{
			// When the budget cannot cover the fee, an error
			// should be returned.
			name: "not enough budget",
			req: &BumpRequest{
				DeliveryAddress: changePkScript,
				Inputs:          []input.Input{&inp},
			},
			expectedErr: ErrNotEnoughBudget,
		},
		{
			// When the mempool rejects the transaction, an error
			// should be returned.
			name: "testmempoolaccept fail",
			req: &BumpRequest{
				DeliveryAddress: changePkScript,
				Inputs:          []input.Input{&inp},
				Budget:          btcutil.Amount(1000),
			},
			expectedErr: errDummy,
		},
		{
			// When the mempool accepts the transaction, no error
			// should be returned.
			name: "testmempoolaccept pass",
			req: &BumpRequest{
				DeliveryAddress: changePkScript,
				Inputs:          []input.Input{&inp},
				Budget:          btcutil.Amount(1000),
			},
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Call the method under test.
			_, _, err := tp.createAndCheckTx(tc.req, m.feeFunc)

			// Check the result is as expected.
			require.ErrorIs(t, err, tc.expectedErr)
		})
	}
}

// createTestBumpRequest creates a new bump request.
func createTestBumpRequest() *BumpRequest {
	// Create a test input.
	inp := createTestInput(1000, input.WitnessKeyHash)

	return &BumpRequest{
		DeliveryAddress: changePkScript,
		Inputs:          []input.Input{&inp},
		Budget:          btcutil.Amount(1000),
	}
}

// TestCreateRBFCompliantTx checks that `createRBFCompliantTx` behaves as
// expected.
func TestCreateRBFCompliantTx(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test bump request.
	req := createTestBumpRequest()

	// Create a test feerate and return it from the mock fee function.
	feerate := chainfee.SatPerKWeight(1000)
	m.feeFunc.On("FeeRate").Return(feerate)

	// Mock the signer to always return a valid script.
	//
	// NOTE: we are not testing the utility of creating valid txes here, so
	// this is fine to be mocked. This behaves essentially as skipping the
	// Signer check and alaways assume the tx has a valid sig.
	script := &input.Script{}
	m.signer.On("ComputeInputScript", mock.Anything,
		mock.Anything).Return(script, nil)

	testCases := []struct {
		name        string
		setupMock   func()
		expectedErr error
	}{
		{
			// When testmempoolaccept accepts the tx, no error
			// should be returned.
			name: "success case",
			setupMock: func() {
				// Mock the testmempoolaccept to pass.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(nil).Once()
			},
			expectedErr: nil,
		},
		{
			// When testmempoolaccept fails due to a non-fee
			// related error, an error should be returned.
			name: "non-fee related testmempoolaccept fail",
			setupMock: func() {
				// Mock the testmempoolaccept to fail.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(errDummy).Once()
			},
			expectedErr: errDummy,
		},
		{
			// When increase feerate gives an error, the error
			// should be returned.
			name: "fail on increase fee",
			setupMock: func() {
				// Mock the testmempoolaccept to fail on fee.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(
					lnwallet.ErrMempoolFee).Once()

				// Mock the fee function to return an error.
				m.feeFunc.On("Increment").Return(
					false, errDummy).Once()
			},
			expectedErr: errDummy,
		},
		{
			// Test that after one round of increasing the feerate
			// the tx passes testmempoolaccept.
			name: "increase fee and success on min mempool fee",
			setupMock: func() {
				// Mock the testmempoolaccept to fail on fee
				// for the first call.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(
					lnwallet.ErrMempoolFee).Once()

				// Mock the fee function to increase feerate.
				m.feeFunc.On("Increment").Return(
					true, nil).Once()

				// Mock the testmempoolaccept to pass on the
				// second call.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(nil).Once()
			},
			expectedErr: nil,
		},
		{
			// Test that after one round of increasing the feerate
			// the tx passes testmempoolaccept.
			name: "increase fee and success on insufficient fee",
			setupMock: func() {
				// Mock the testmempoolaccept to fail on fee
				// for the first call.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(
					chain.ErrInsufficientFee).Once()

				// Mock the fee function to increase feerate.
				m.feeFunc.On("Increment").Return(
					true, nil).Once()

				// Mock the testmempoolaccept to pass on the
				// second call.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(nil).Once()
			},
			expectedErr: nil,
		},
		{
			// Test that the fee function increases the fee rate
			// after one round.
			name: "increase fee on second round",
			setupMock: func() {
				// Mock the testmempoolaccept to fail on fee
				// for the first call.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(
					chain.ErrInsufficientFee).Once()

				// Mock the fee function to NOT increase
				// feerate on the first round.
				m.feeFunc.On("Increment").Return(
					false, nil).Once()

				// Mock the fee function to increase feerate.
				m.feeFunc.On("Increment").Return(
					true, nil).Once()

				// Mock the testmempoolaccept to pass on the
				// second call.
				m.wallet.On("CheckMempoolAcceptance",
					mock.Anything).Return(nil).Once()
			},
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			tc.setupMock()

			// Call the method under test.
			id, err := tp.createRBFCompliantTx(req, m.feeFunc)

			// Check the result is as expected.
			require.ErrorIs(t, err, tc.expectedErr)

			// If there's an error, expect the requestID to be
			// empty.
			if tc.expectedErr != nil {
				require.Zero(t, id)
			}
		})
	}
}

// TestTxPublisherBroadcast checks the internal `broadcast` method behaves as
// expected.
func TestTxPublisherBroadcast(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test bump request.
	req := createTestBumpRequest()

	// Create a test tx.
	tx := &wire.MsgTx{LockTime: 1}

	// Create a test feerate and return it from the mock fee function.
	feerate := chainfee.SatPerKWeight(1000)
	m.feeFunc.On("FeeRate").Return(feerate)

	// Create a testing record and put it in the map.
	fee := btcutil.Amount(1000)
	requestID := tp.storeRecord(tx, req, m.feeFunc, fee)

	// Quickly check when the requestID cannot be found, an error is
	// returned.
	result, err := tp.broadcast(uint64(1000))
	require.Error(t, err)
	require.Nil(t, result)

	testCases := []struct {
		name           string
		setupMock      func()
		expectedErr    error
		expectedResult *BumpResult
	}{
		{
			// When the wallet cannot publish this tx, the error
			// should be put inside the result.
			name: "fail to publish",
			setupMock: func() {
				// Mock the wallet to fail to publish.
				m.wallet.On("PublishTransaction",
					tx, mock.Anything).Return(
					errDummy).Once()
			},
			expectedErr: nil,
			expectedResult: &BumpResult{
				Event:     TxFailed,
				Tx:        tx,
				Fee:       fee,
				FeeRate:   feerate,
				Err:       errDummy,
				requestID: requestID,
			},
		},
		{
			// When nothing goes wrong, the result is returned.
			name: "publish success",
			setupMock: func() {
				// Mock the wallet to publish successfully.
				m.wallet.On("PublishTransaction",
					tx, mock.Anything).Return(nil).Once()
			},
			expectedErr: nil,
			expectedResult: &BumpResult{
				Event:     TxPublished,
				Tx:        tx,
				Fee:       fee,
				FeeRate:   feerate,
				Err:       nil,
				requestID: requestID,
			},
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			tc.setupMock()

			// Call the method under test.
			result, err := tp.broadcast(requestID)

			// Check the result is as expected.
			require.ErrorIs(t, err, tc.expectedErr)
			require.Equal(t, tc.expectedResult, result)
		})
	}
}

// TestRemoveResult checks the records and subscriptions are removed when a tx
// is confirmed or failed.
func TestRemoveResult(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test bump request.
	req := createTestBumpRequest()

	// Create a test tx.
	tx := &wire.MsgTx{LockTime: 1}

	// Create a testing record and put it in the map.
	fee := btcutil.Amount(1000)

	testCases := []struct {
		name        string
		setupRecord func() uint64
		result      *BumpResult
		removed     bool
	}{
		{
			// When the tx is confirmed, the records will be
			// removed.
			name: "remove on TxConfirmed",
			setupRecord: func() uint64 {
				id := tp.storeRecord(tx, req, m.feeFunc, fee)
				tp.subscriberChans.Store(id, nil)

				return id
			},
			result: &BumpResult{
				Event: TxConfirmed,
				Tx:    tx,
			},
			removed: true,
		},
		{
			// When the tx is failed, the records will be removed.
			name: "remove on TxFailed",
			setupRecord: func() uint64 {
				id := tp.storeRecord(tx, req, m.feeFunc, fee)
				tp.subscriberChans.Store(id, nil)

				return id
			},
			result: &BumpResult{
				Event: TxFailed,
				Err:   errDummy,
				Tx:    tx,
			},
			removed: true,
		},
		{
			// Noop when the tx is neither confirmed or failed.
			name: "noop when tx is not confirmed or failed",
			setupRecord: func() uint64 {
				id := tp.storeRecord(tx, req, m.feeFunc, fee)
				tp.subscriberChans.Store(id, nil)

				return id
			},
			result: &BumpResult{
				Event: TxPublished,
				Tx:    tx,
			},
			removed: false,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			requestID := tc.setupRecord()

			// Attach the requestID from the setup.
			tc.result.requestID = requestID

			// Remove the result.
			tp.removeResult(tc.result)

			// Check if the record is removed.
			_, found := tp.records.Load(requestID)
			require.Equal(t, !tc.removed, found)

			_, found = tp.subscriberChans.Load(requestID)
			require.Equal(t, !tc.removed, found)
		})
	}
}

// TestNotifyResult checks the subscribers are notified when a result is sent.
func TestNotifyResult(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test bump request.
	req := createTestBumpRequest()

	// Create a test tx.
	tx := &wire.MsgTx{LockTime: 1}

	// Create a testing record and put it in the map.
	fee := btcutil.Amount(1000)
	requestID := tp.storeRecord(tx, req, m.feeFunc, fee)

	// Create a subscription to the event.
	subscriber := make(chan *BumpResult, 1)
	tp.subscriberChans.Store(requestID, subscriber)

	// Create a test result.
	result := &BumpResult{
		requestID: requestID,
		Tx:        tx,
	}

	// Notify the result and expect the subscriber to receive it.
	//
	// NOTE: must be done inside a goroutine in case it blocks.
	go tp.notifyResult(result)

	select {
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriber to receive result")

	case received := <-subscriber:
		require.Equal(t, result, received)
	}

	// Notify two results. This time it should block because the channel is
	// full. We then shutdown TxPublisher to test the quit behavior.
	done := make(chan struct{})
	go func() {
		// Call notifyResult twice, which blocks at the second call.
		tp.notifyResult(result)
		tp.notifyResult(result)

		close(done)
	}()

	// Shutdown the publisher and expect notifyResult to exit.
	close(tp.quit)

	// We expect to done chan.
	select {
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for notifyResult to exit")

	case <-done:
	}
}

// TestBroadcastSuccess checks the public `Broadcast` method can successfully
// broadcast a tx based on the request.
func TestBroadcastSuccess(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test feerate.
	feerate := chainfee.SatPerKWeight(1000)

	// Mock the fee estimator to return the testing fee rate.
	//
	// We are not testing `NewLinearFeeFunction` here, so the actual params
	// used are irrelevant.
	m.estimator.On("EstimateFeePerKW", mock.Anything).Return(
		feerate, nil).Once()
	m.estimator.On("RelayFeePerKW").Return(chainfee.FeePerKwFloor).Once()

	// Mock the signer to always return a valid script.
	//
	// NOTE: we are not testing the utility of creating valid txes here, so
	// this is fine to be mocked. This behaves essentially as skipping the
	// Signer check and alaways assume the tx has a valid sig.
	script := &input.Script{}
	m.signer.On("ComputeInputScript", mock.Anything,
		mock.Anything).Return(script, nil)

	// Mock the testmempoolaccept to pass.
	m.wallet.On("CheckMempoolAcceptance", mock.Anything).Return(nil).Once()

	// Mock the wallet to publish successfully.
	m.wallet.On("PublishTransaction",
		mock.Anything, mock.Anything).Return(nil).Once()

	// Create a test request.
	inp := createTestInput(1000, input.WitnessKeyHash)

	// Create a testing bump request.
	req := &BumpRequest{
		DeliveryAddress: changePkScript,
		Inputs:          []input.Input{&inp},
		Budget:          btcutil.Amount(1000),
		MaxFeeRate:      feerate * 10,
		DeadlineHeight:  10,
	}

	// Send the req and expect no error.
	resultChan, err := tp.Broadcast(req)
	require.NoError(t, err)

	// Check the result is sent back.
	select {
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriber to receive result")

	case result := <-resultChan:
		// We expect the first result to be TxPublished.
		require.Equal(t, TxPublished, result.Event)
	}

	// Validate the record was stored.
	require.Equal(t, 1, tp.records.Len())
	require.Equal(t, 1, tp.subscriberChans.Len())
}

// TestBroadcastFail checks the public `Broadcast` returns the error or a
// failed result when the broadcast fails.
func TestBroadcastFail(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test feerate.
	feerate := chainfee.SatPerKWeight(1000)

	// Create a test request.
	inp := createTestInput(1000, input.WitnessKeyHash)

	// Create a testing bump request.
	req := &BumpRequest{
		DeliveryAddress: changePkScript,
		Inputs:          []input.Input{&inp},
		Budget:          btcutil.Amount(1000),
		MaxFeeRate:      feerate * 10,
		DeadlineHeight:  10,
	}

	// Mock the fee estimator to return the testing fee rate.
	//
	// We are not testing `NewLinearFeeFunction` here, so the actual params
	// used are irrelevant.
	m.estimator.On("EstimateFeePerKW", mock.Anything).Return(
		feerate, nil).Twice()
	m.estimator.On("RelayFeePerKW").Return(chainfee.FeePerKwFloor).Twice()

	// Mock the signer to always return a valid script.
	//
	// NOTE: we are not testing the utility of creating valid txes here, so
	// this is fine to be mocked. This behaves essentially as skipping the
	// Signer check and alaways assume the tx has a valid sig.
	script := &input.Script{}
	m.signer.On("ComputeInputScript", mock.Anything,
		mock.Anything).Return(script, nil)

	// Mock the testmempoolaccept to return an error.
	m.wallet.On("CheckMempoolAcceptance",
		mock.Anything).Return(errDummy).Once()

	// Send the req and expect an error returned.
	resultChan, err := tp.Broadcast(req)
	require.ErrorIs(t, err, errDummy)
	require.Nil(t, resultChan)

	// Validate the record was NOT stored.
	require.Equal(t, 0, tp.records.Len())
	require.Equal(t, 0, tp.subscriberChans.Len())

	// Mock the testmempoolaccept again, this time it passes.
	m.wallet.On("CheckMempoolAcceptance", mock.Anything).Return(nil).Once()

	// Mock the wallet to fail on publish.
	m.wallet.On("PublishTransaction",
		mock.Anything, mock.Anything).Return(errDummy).Once()

	// Send the req and expect no error returned.
	resultChan, err = tp.Broadcast(req)
	require.NoError(t, err)

	// Check the result is sent back.
	select {
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriber to receive result")

	case result := <-resultChan:
		// We expect the result to be TxFailed and the error is set in
		// the result.
		require.Equal(t, TxFailed, result.Event)
		require.ErrorIs(t, result.Err, errDummy)
	}

	// Validate the record was removed.
	require.Equal(t, 0, tp.records.Len())
	require.Equal(t, 0, tp.subscriberChans.Len())
}

// TestCreateAnPublishFail checks all the error cases are handled properly in
// the method createAndPublish.
func TestCreateAnPublishFail(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test requestID.
	requestID := uint64(1)

	// Create a test feerate and return it from the mock fee function.
	feerate := chainfee.SatPerKWeight(1000)
	m.feeFunc.On("FeeRate").Return(feerate)

	// Create a testing monitor record.
	req := createTestBumpRequest()

	// Overwrite the budget to make it smaller than the fee.
	req.Budget = 100
	record := &monitorRecord{
		req:         req,
		feeFunction: m.feeFunc,
		tx:          &wire.MsgTx{},
	}

	// Mock the signer to always return a valid script.
	//
	// NOTE: we are not testing the utility of creating valid txes here, so
	// this is fine to be mocked. This behaves essentially as skipping the
	// Signer check and alaways assume the tx has a valid sig.
	script := &input.Script{}
	m.signer.On("ComputeInputScript", mock.Anything,
		mock.Anything).Return(script, nil)

	// Call the createAndPublish method.
	resultOpt := tp.createAndPublishTx(requestID, record)
	result := resultOpt.UnwrapOrFail(t)

	// We expect the result to be TxFailed and the error is set in the
	// result.
	require.Equal(t, TxFailed, result.Event)
	require.ErrorIs(t, result.Err, ErrNotEnoughBudget)
	require.Equal(t, requestID, result.requestID)

	// Increase the budget and call it again. This time we will mock an
	// error to be returned from CheckMempoolAcceptance.
	req.Budget = 1000

	// Mock the testmempoolaccept to return a fee related error that should
	// be ignored.
	m.wallet.On("CheckMempoolAcceptance",
		mock.Anything).Return(lnwallet.ErrMempoolFee).Once()

	// Call the createAndPublish method and expect a none option.
	resultOpt = tp.createAndPublishTx(requestID, record)
	require.True(t, resultOpt.IsNone())

	// Mock the testmempoolaccept to return a fee related error that should
	// be ignored.
	m.wallet.On("CheckMempoolAcceptance",
		mock.Anything).Return(chain.ErrInsufficientFee).Once()

	// Call the createAndPublish method and expect a none option.
	resultOpt = tp.createAndPublishTx(requestID, record)
	require.True(t, resultOpt.IsNone())
}

// TestCreateAnPublishSuccess checks the expected result is returned from the
// method createAndPublish.
func TestCreateAnPublishSuccess(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test requestID.
	requestID := uint64(1)

	// Create a test feerate and return it from the mock fee function.
	feerate := chainfee.SatPerKWeight(1000)
	m.feeFunc.On("FeeRate").Return(feerate)

	// Create a testing monitor record.
	req := createTestBumpRequest()
	record := &monitorRecord{
		req:         req,
		feeFunction: m.feeFunc,
		tx:          &wire.MsgTx{},
	}

	// Mock the signer to always return a valid script.
	//
	// NOTE: we are not testing the utility of creating valid txes here, so
	// this is fine to be mocked. This behaves essentially as skipping the
	// Signer check and alaways assume the tx has a valid sig.
	script := &input.Script{}
	m.signer.On("ComputeInputScript", mock.Anything,
		mock.Anything).Return(script, nil)

	// Mock the testmempoolaccept to return nil.
	m.wallet.On("CheckMempoolAcceptance", mock.Anything).Return(nil)

	// Mock the wallet to publish and return an error.
	m.wallet.On("PublishTransaction",
		mock.Anything, mock.Anything).Return(errDummy).Once()

	// Call the createAndPublish method and expect a failure result.
	resultOpt := tp.createAndPublishTx(requestID, record)
	result := resultOpt.UnwrapOrFail(t)

	// We expect the result to be TxFailed and the error is set.
	require.Equal(t, TxFailed, result.Event)
	require.ErrorIs(t, result.Err, errDummy)

	// Although the replacement tx was failed to be published, the record
	// should be stored.
	require.NotNil(t, result.Tx)
	require.NotNil(t, result.ReplacedTx)
	_, found := tp.records.Load(requestID)
	require.True(t, found)

	// We now check a successful RBF.
	//
	// Mock the wallet to publish successfully.
	m.wallet.On("PublishTransaction",
		mock.Anything, mock.Anything).Return(nil).Once()

	// Call the createAndPublish method and expect a success result.
	resultOpt = tp.createAndPublishTx(requestID, record)
	result = resultOpt.UnwrapOrFail(t)
	require.True(t, resultOpt.IsSome())

	// We expect the result to be TxReplaced and the error is nil.
	require.Equal(t, TxReplaced, result.Event)
	require.Nil(t, result.Err)

	// Check the Tx and ReplacedTx are set.
	require.NotNil(t, result.Tx)
	require.NotNil(t, result.ReplacedTx)

	// Check the record is stored.
	_, found = tp.records.Load(requestID)
	require.True(t, found)
}

// TestHandleTxConfirmed checks the expected result is returned from the method
// handleTxConfirmed.
func TestHandleTxConfirmed(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test bump request.
	req := createTestBumpRequest()

	// Create a test tx.
	tx := &wire.MsgTx{LockTime: 1}

	// Create a testing record and put it in the map.
	fee := btcutil.Amount(1000)
	requestID := tp.storeRecord(tx, req, m.feeFunc, fee)
	record, ok := tp.records.Load(requestID)
	require.True(t, ok)

	// Create a subscription to the event.
	subscriber := make(chan *BumpResult, 1)
	tp.subscriberChans.Store(requestID, subscriber)

	// Mock the fee function to return a fee rate.
	feerate := chainfee.SatPerKWeight(1000)
	m.feeFunc.On("FeeRate").Return(feerate).Once()

	// Call the method and expect a result to be received.
	//
	// NOTE: must be called in a goroutine in case it blocks.
	tp.wg.Add(1)
	done := make(chan struct{})
	go func() {
		tp.handleTxConfirmed(record, requestID)
		close(done)
	}()

	select {
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriber to receive result")

	case result := <-subscriber:
		// We expect the result to be TxConfirmed and the tx is set.
		require.Equal(t, TxConfirmed, result.Event)
		require.Equal(t, tx, result.Tx)
		require.Nil(t, result.Err)
		require.Equal(t, requestID, result.requestID)
		require.Equal(t, record.fee, result.Fee)
		require.Equal(t, feerate, result.FeeRate)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for handleTxConfirmed to return")
	}

	// We expect the record to be removed from the maps.
	_, found := tp.records.Load(requestID)
	require.False(t, found)
	_, found = tp.subscriberChans.Load(requestID)
	require.False(t, found)
}

// TestHandleFeeBumpTx validates handleFeeBumpTx behaves as expected.
func TestHandleFeeBumpTx(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create a test tx.
	tx := &wire.MsgTx{LockTime: 1}

	// Create a test current height.
	testHeight := int32(800000)

	// Create a testing monitor record.
	req := createTestBumpRequest()
	record := &monitorRecord{
		req:         req,
		feeFunction: m.feeFunc,
		tx:          tx,
	}

	// Create a testing record and put it in the map.
	fee := btcutil.Amount(1000)
	requestID := tp.storeRecord(tx, req, m.feeFunc, fee)

	// Create a subscription to the event.
	subscriber := make(chan *BumpResult, 1)
	tp.subscriberChans.Store(requestID, subscriber)

	// Create a test feerate and return it from the mock fee function.
	feerate := chainfee.SatPerKWeight(1000)
	m.feeFunc.On("FeeRate").Return(feerate)

	// Mock the fee function to skip the bump due to error.
	m.feeFunc.On("IncreaseFeeRate", mock.Anything).Return(
		false, errDummy).Once()

	// Call the method and expect no result received.
	tp.wg.Add(1)
	go tp.handleFeeBumpTx(requestID, record, testHeight)

	// Check there's no result sent back.
	select {
	case <-time.After(time.Second):
	case result := <-subscriber:
		t.Fatalf("unexpected result received: %v", result)
	}

	// Mock the fee function to skip the bump.
	m.feeFunc.On("IncreaseFeeRate", mock.Anything).Return(false, nil).Once()

	// Call the method and expect no result received.
	tp.wg.Add(1)
	go tp.handleFeeBumpTx(requestID, record, testHeight)

	// Check there's no result sent back.
	select {
	case <-time.After(time.Second):
	case result := <-subscriber:
		t.Fatalf("unexpected result received: %v", result)
	}

	// Mock the fee function to perform the fee bump.
	m.feeFunc.On("IncreaseFeeRate", mock.Anything).Return(true, nil)

	// Mock the signer to always return a valid script.
	//
	// NOTE: we are not testing the utility of creating valid txes here, so
	// this is fine to be mocked. This behaves essentially as skipping the
	// Signer check and alaways assume the tx has a valid sig.
	script := &input.Script{}
	m.signer.On("ComputeInputScript", mock.Anything,
		mock.Anything).Return(script, nil)

	// Mock the testmempoolaccept to return nil.
	m.wallet.On("CheckMempoolAcceptance", mock.Anything).Return(nil)

	// Mock the wallet to publish successfully.
	m.wallet.On("PublishTransaction",
		mock.Anything, mock.Anything).Return(nil).Once()

	// Call the method and expect a result to be received.
	//
	// NOTE: must be called in a goroutine in case it blocks.
	tp.wg.Add(1)
	go tp.handleFeeBumpTx(requestID, record, testHeight)

	select {
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriber to receive result")

	case result := <-subscriber:
		// We expect the result to be TxReplaced.
		require.Equal(t, TxReplaced, result.Event)

		// The new tx and old tx should be properly set.
		require.NotEqual(t, tx, result.Tx)
		require.Equal(t, tx, result.ReplacedTx)

		// No error should be set.
		require.Nil(t, result.Err)
		require.Equal(t, requestID, result.requestID)
	}

	// We expect the record to NOT be removed from the maps.
	_, found := tp.records.Load(requestID)
	require.True(t, found)
	_, found = tp.subscriberChans.Load(requestID)
	require.True(t, found)
}

// TestProcessRecords validates processRecords behaves as expected.
func TestProcessRecords(t *testing.T) {
	t.Parallel()

	// Create a publisher using the mocks.
	tp, m := createTestPublisher(t)

	// Create testing objects.
	requestID1 := uint64(1)
	req1 := createTestBumpRequest()
	tx1 := &wire.MsgTx{LockTime: 1}
	txid1 := tx1.TxHash()

	requestID2 := uint64(2)
	req2 := createTestBumpRequest()
	tx2 := &wire.MsgTx{LockTime: 2}
	txid2 := tx2.TxHash()

	// Create a monitor record that's confirmed.
	recordConfirmed := &monitorRecord{
		req:         req1,
		feeFunction: m.feeFunc,
		tx:          tx1,
	}
	m.wallet.On("GetTransactionDetails", &txid1).Return(
		&lnwallet.TransactionDetail{
			NumConfirmations: 1,
		}, nil,
	).Once()

	// Create a monitor record that's not confirmed. We know it's not
	// confirmed because the num of confirms is zero.
	recordFeeBump := &monitorRecord{
		req:         req2,
		feeFunction: m.feeFunc,
		tx:          tx2,
	}
	m.wallet.On("GetTransactionDetails", &txid2).Return(
		&lnwallet.TransactionDetail{
			NumConfirmations: 0,
		}, nil,
	).Once()
	m.wallet.On("BackEnd").Return("test-backend").Once()

	// Setup the initial publisher state by adding the records to the maps.
	subscriberConfirmed := make(chan *BumpResult, 1)
	tp.subscriberChans.Store(requestID1, subscriberConfirmed)
	tp.records.Store(requestID1, recordConfirmed)

	subscriberReplaced := make(chan *BumpResult, 1)
	tp.subscriberChans.Store(requestID2, subscriberReplaced)
	tp.records.Store(requestID2, recordFeeBump)

	// Create a test feerate and return it from the mock fee function.
	feerate := chainfee.SatPerKWeight(1000)
	m.feeFunc.On("FeeRate").Return(feerate)

	// The following methods should only be called once when creating the
	// replacement tx.
	//
	// Mock the fee function to NOT skip the fee bump.
	m.feeFunc.On("IncreaseFeeRate", mock.Anything).Return(true, nil).Once()

	// Mock the signer to always return a valid script.
	m.signer.On("ComputeInputScript", mock.Anything,
		mock.Anything).Return(&input.Script{}, nil).Once()

	// Mock the testmempoolaccept to return nil.
	m.wallet.On("CheckMempoolAcceptance", mock.Anything).Return(nil).Once()

	// Mock the wallet to publish successfully.
	m.wallet.On("PublishTransaction",
		mock.Anything, mock.Anything).Return(nil).Once()

	// Call processRecords and expect the results are notified back.
	tp.processRecords()

	// We expect two results to be received. One for the confirmed tx and
	// one for the replaced tx.
	//
	// Check the confirmed tx result.
	select {
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriberConfirmed")

	case result := <-subscriberConfirmed:
		// We expect the result to be TxConfirmed.
		require.Equal(t, TxConfirmed, result.Event)
		require.Equal(t, tx1, result.Tx)

		// No error should be set.
		require.Nil(t, result.Err)
		require.Equal(t, requestID1, result.requestID)
	}

	// Now check the replaced tx result.
	select {
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for subscriberReplaced")

	case result := <-subscriberReplaced:
		// We expect the result to be TxReplaced.
		require.Equal(t, TxReplaced, result.Event)

		// The new tx and old tx should be properly set.
		require.NotEqual(t, tx2, result.Tx)
		require.Equal(t, tx2, result.ReplacedTx)

		// No error should be set.
		require.Nil(t, result.Err)
		require.Equal(t, requestID2, result.requestID)
	}
}

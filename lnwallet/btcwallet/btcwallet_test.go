package btcwallet

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// lockedOutpointWallet simulates an output held by btcwallet's memory locker.
type lockedOutpointWallet struct {
	wallet.Interface
	leaseCalled bool
}

// LockedOutpoint reports that the test output is already locked in memory.
func (w *lockedOutpointWallet) LockedOutpoint(wire.OutPoint) bool {
	return true
}

// LeaseOutput records an unexpected attempt to lease the locked output.
func (w *lockedOutpointWallet) LeaseOutput(wtxmgr.LockID, wire.OutPoint,
	time.Duration) (time.Time, error) {

	w.leaseCalled = true
	return time.Time{}, nil
}

// TestLeaseOutputRejectsInMemoryLock verifies that the legacy lease path
// preserves the in-memory double-lock guard.
func TestLeaseOutputRejectsInMemoryLock(t *testing.T) {
	t.Parallel()

	backend := &lockedOutpointWallet{}
	wallet := &BtcWallet{wallet: backend}

	_, err := wallet.LeaseOutput(
		wtxmgr.LockID{}, wire.OutPoint{}, time.Minute,
	)
	require.ErrorIs(t, err, wtxmgr.ErrOutputAlreadyLocked)
	require.False(t, backend.leaseCalled)
}

type previousOutpointsTest struct {
	name     string
	tx       *wire.MsgTx
	myInputs []wallet.TransactionSummaryInput
	expRes   []lnwallet.PreviousOutPoint
}

var previousOutpointsTests = []previousOutpointsTest{{
	name: "both outpoints are wallet controlled",
	tx: &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{Index: 0},
		}, {
			PreviousOutPoint: wire.OutPoint{Index: 1},
		}},
	},
	myInputs: []wallet.TransactionSummaryInput{{
		Index: 0,
	}, {
		Index: 1,
	}},
	expRes: []lnwallet.PreviousOutPoint{{
		OutPoint:    wire.OutPoint{Index: 0}.String(),
		IsOurOutput: true,
	}, {
		OutPoint:    wire.OutPoint{Index: 1}.String(),
		IsOurOutput: true,
	}},
}, {
	name: "only one outpoint is wallet controlled",
	tx: &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{Index: 0},
		}, {
			PreviousOutPoint: wire.OutPoint{Index: 1},
		}},
	},
	myInputs: []wallet.TransactionSummaryInput{{
		Index: 0,
	}, {
		Index: 2,
	}},
	expRes: []lnwallet.PreviousOutPoint{{
		OutPoint:    wire.OutPoint{Index: 0}.String(),
		IsOurOutput: true,
	}, {
		OutPoint:    wire.OutPoint{Index: 1}.String(),
		IsOurOutput: false,
	}},
}, {
	name: "no outpoint is wallet controlled",
	tx: &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{Index: 0},
		}, {
			PreviousOutPoint: wire.OutPoint{Index: 1},
		}},
	},
	myInputs: []wallet.TransactionSummaryInput{{
		Index: 2,
	}, {
		Index: 3,
	}},
	expRes: []lnwallet.PreviousOutPoint{{
		OutPoint:    wire.OutPoint{Index: 0}.String(),
		IsOurOutput: false,
	}, {
		OutPoint:    wire.OutPoint{Index: 1}.String(),
		IsOurOutput: false,
	}},
}, {
	name: "tx is empty",
	tx: &wire.MsgTx{
		TxIn: []*wire.TxIn{},
	},
	myInputs: []wallet.TransactionSummaryInput{{
		Index: 2,
	}, {
		Index: 3,
	}},
	expRes: []lnwallet.PreviousOutPoint{},
}, {
	name: "wallet controlled input set is empty",
	tx: &wire.MsgTx{
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{Index: 0},
		}, {
			PreviousOutPoint: wire.OutPoint{Index: 1},
		}},
	},
	myInputs: []wallet.TransactionSummaryInput{},
	expRes: []lnwallet.PreviousOutPoint{{
		OutPoint:    wire.OutPoint{Index: 0}.String(),
		IsOurOutput: false,
	}, {
		OutPoint:    wire.OutPoint{Index: 1}.String(),
		IsOurOutput: false,
	}},
}}

// TestPreviousOutpoints tests if we are able to get the previous
// outpoints correctly.
func TestPreviousOutpoints(t *testing.T) {
	for _, test := range previousOutpointsTests {
		t.Run(test.name, func(t *testing.T) {
			respOutpoints := getPreviousOutpoints(
				test.tx, test.myInputs,
			)

			for idx, respOutpoint := range respOutpoints {
				expRes := test.expRes[idx]
				require.Equal(
					t, expRes.OutPoint,
					respOutpoint.OutPoint,
				)
				require.Equal(
					t, expRes.IsOurOutput,
					respOutpoint.IsOurOutput,
				)
			}
		})
	}
}

// TestTransactionDetailsPage verifies the bounds returned for transaction
// pagination, including requests that would overflow with uint32 addition.
func TestTransactionDetailsPage(t *testing.T) {
	t.Parallel()

	txDetails := []*lnwallet.TransactionDetail{{}, {}, {}, {}}

	testCases := []struct {
		name          string
		offset        uint32
		limit         uint32
		expectedPage  []*lnwallet.TransactionDetail
		expectedFirst uint64
		expectedLast  uint64
	}{
		{
			name:          "zero limit returns remainder",
			offset:        1,
			expectedPage:  txDetails[1:],
			expectedFirst: 1,
			expectedLast:  3,
		},
		{
			name:          "limit selects page",
			offset:        1,
			limit:         2,
			expectedPage:  txDetails[1:3],
			expectedFirst: 1,
			expectedLast:  2,
		},
		{
			name:          "limit exceeds remainder",
			offset:        2,
			limit:         10,
			expectedPage:  txDetails[2:],
			expectedFirst: 2,
			expectedLast:  3,
		},
		{
			name:          "offset plus limit exceeds uint32",
			offset:        1,
			limit:         math.MaxUint32,
			expectedPage:  txDetails[1:],
			expectedFirst: 1,
			expectedLast:  3,
		},
		{
			name:         "offset equals transaction count",
			offset:       uint32(len(txDetails)),
			limit:        1,
			expectedPage: []*lnwallet.TransactionDetail{},
		},
		{
			name:         "maximum offset",
			offset:       math.MaxUint32,
			limit:        1,
			expectedPage: []*lnwallet.TransactionDetail{},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			page, first, last := transactionDetailsPage(
				txDetails, testCase.offset, testCase.limit,
			)

			require.Equal(t, testCase.expectedPage, page)
			require.Equal(t, testCase.expectedFirst, first)
			require.Equal(t, testCase.expectedLast, last)
		})
	}
}

// TestCheckMempoolAcceptance asserts the CheckMempoolAcceptance behaves as
// expected.
func TestCheckMempoolAcceptance(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Arrange backend results from unsupported RPC through accepted tx,
	// retaining typed errors from the existing Core mapper.
	mockChain := &lnmock.MockChain{}
	defer mockChain.AssertExpectations(t)

	// Create a test tx and a test max feerate.
	tx := wire.NewMsgTx(2)
	maxFeeRate := float64(0)

	// Create a test wallet.
	wallet := &BtcWallet{
		chain: mockChain,
	}

	// Assert that when the chain backend doesn't support
	// `TestMempoolAccept`, an error is returned.
	//
	// Mock the chain backend to not support `TestMempoolAccept`.
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		nil, rpcclient.ErrBackendVersion).Once()

	// Act by checking the transaction against the configured backend
	// verdict.
	err := wallet.CheckMempoolAcceptance(tx)
	// Assert that unsupported RPCs stay distinguishable from tx rejection.
	rt.ErrorIs(err, rpcclient.ErrBackendVersion)

	// Assert that when the chain backend doesn't implement
	// `TestMempoolAccept`, an error is returned.
	//
	// Mock the chain backend to not support `TestMempoolAccept`.
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		nil, chain.ErrUnimplemented).Once()

	// Now call the method under test.
	err = wallet.CheckMempoolAcceptance(tx)
	rt.ErrorIs(err, chain.ErrUnimplemented)

	// Assert that when the returned results are not as expected, an error
	// is returned.
	//
	// Mock the chain backend to return more than one result.
	results := []*btcjson.TestMempoolAcceptResult{
		{Txid: "txid1", Allowed: true},
		{Txid: "txid2", Allowed: false},
	}
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		results, nil).Once()

	// Now call the method under test.
	err = wallet.CheckMempoolAcceptance(tx)
	rt.ErrorContains(err, "expected 1 result from TestMempoolAccept")

	// Assert that when the tx is rejected, the reason is converted to an
	// RPC error and returned.
	//
	// Mock the chain backend to return one result.
	results = []*btcjson.TestMempoolAcceptResult{{
		Txid:         tx.TxHash().String(),
		Allowed:      false,
		RejectReason: "insufficient fee",
	}}
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		results, nil).Once()
	mockChain.On("MapRPCErr", mock.Anything).Return(
		chain.ErrInsufficientFee).Once()
	mockChain.On("BackEnd").Return("bitcoind").Once()

	// Now call the method under test.
	err = wallet.CheckMempoolAcceptance(tx)
	rt.ErrorIs(err, chain.ErrInsufficientFee)

	// Assert that when the tx is accepted, no error is returned.
	//
	// Mock the chain backend to return one result.
	results = []*btcjson.TestMempoolAcceptResult{
		{Txid: tx.TxHash().String(), Allowed: true},
	}
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, maxFeeRate).Return(
		results, nil).Once()

	// Now call the method under test.
	err = wallet.CheckMempoolAcceptance(tx)
	rt.NoError(err)
}

// TestNormalizeMempoolAcceptError checks the narrow raw btcd rejection forms
// that identify input failures, including negative controls for broad errors
// and output standardness.
func TestNormalizeMempoolAcceptError(t *testing.T) {
	t.Parallel()

	// Arrange the real btcd witness encodings and exact rejection forms,
	// along with existing backend, txid, mapping, and output-policy
	// controls.
	witnessCases := []struct {
		name    string
		witness wire.TxWitness
		encoded string
	}{
		{
			name:    "empty",
			encoded: "[]",
		},
		{
			name:    "single",
			witness: wire.TxWitness{{0x01, 0x02}},
			encoded: "[0102]",
		},
		{
			name:    "multiple",
			witness: wire.TxWitness{{0x01, 0x02}, {0x03}},
			encoded: "[0102 03]",
		},
		{
			name:    "empty elements",
			witness: wire.TxWitness{{}, {0x03}, {}},
			encoded: "[ 03 ]",
		},
	}
	for _, testCase := range witnessCases {
		actual := fmt.Sprintf("%x", testCase.witness)
		require.Equal(t, testCase.encoded, actual, testCase.name)
	}

	txHash := chainhash.Hash{1}
	otherTxHash := chainhash.Hash{2}
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{3},
		Index: 2,
	}.String()
	nonStandardReason := "transaction " + txHash.String() +
		" has a non-standard input: transaction input #0 has a " +
		"non-standard script form"
	inputReason := func(kind string, hash chainhash.Hash,
		witness string) string {

		return kind + " " + hash.String() +
			":0 which references output " + outpoint +
			" - false stack entry at end of script execution " +
			"(input witness " + witness +
			", input script bytes 00, " +
			"prev output script bytes 51)"
	}
	validateReason := inputReason(
		"failed to validate input", txHash, "[0102]",
	)
	parseReason := inputReason("failed to parse input", txHash, "[0102 03]")
	emptyWitnessReason := inputReason(
		"failed to validate input", txHash, "[]",
	)
	emptyElementsReason := inputReason(
		"failed to parse input", txHash, "[ 03 ]",
	)

	testCases := []struct {
		name           string
		backend        string
		rejectReason   string
		mappedErr      error
		expectedErr    error
		notExpectedErr error
	}{
		{
			name:         "non-standard input",
			backend:      "btcd",
			rejectReason: nonStandardReason,
			mappedErr:    chain.ErrNonStandardScript,
			expectedErr:  chain.ErrNonStandardInputs,
		},
		{
			name:         "failed to validate input",
			backend:      "btcd",
			rejectReason: validateReason,
			mappedErr:    chain.ErrUndefined,
			expectedErr:  chain.ErrScriptVerifyFlag,
		},
		{
			name:         "failed to parse input",
			backend:      "btcd",
			rejectReason: parseReason,
			mappedErr:    chain.ErrUndefined,
			expectedErr:  chain.ErrScriptVerifyFlag,
		},
		{
			name:         "empty witness",
			backend:      "btcd",
			rejectReason: emptyWitnessReason,
			mappedErr:    chain.ErrUndefined,
			expectedErr:  chain.ErrScriptVerifyFlag,
		},
		{
			name:         "empty witness elements",
			backend:      "btcd",
			rejectReason: emptyElementsReason,
			mappedErr:    chain.ErrUndefined,
			expectedErr:  chain.ErrScriptVerifyFlag,
		},
		{
			name:    "generic undefined",
			backend: "btcd",
			rejectReason: "transaction rejected for an " +
				"unknown reason",
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:    "output non-standard script",
			backend: "btcd",
			rejectReason: "transaction " + txHash.String() +
				" has a non-standard output: " +
				"transaction output #0 " +
				"has a non-standard script form",
			mappedErr:      chain.ErrNonStandardScript,
			expectedErr:    chain.ErrNonStandardScript,
			notExpectedErr: chain.ErrNonStandardInputs,
		},
		{
			name:           "other backend",
			backend:        "bitcoind",
			rejectReason:   nonStandardReason,
			mappedErr:      chain.ErrNonStandardScript,
			expectedErr:    chain.ErrNonStandardScript,
			notExpectedErr: chain.ErrNonStandardInputs,
		},
		{
			name:    "wrong transaction",
			backend: "btcd",
			rejectReason: inputReason(
				"failed to validate input", otherTxHash, "[]",
			),
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:    "wrong non-standard transaction",
			backend: "btcd",
			rejectReason: "transaction " + otherTxHash.String() +
				" has a non-standard input: " +
				"transaction input #0 " +
				"has a non-standard script form",
			mappedErr:      chain.ErrNonStandardScript,
			expectedErr:    chain.ErrNonStandardScript,
			notExpectedErr: chain.ErrNonStandardInputs,
		},
		{
			name:           "prefixed rejection",
			backend:        "btcd",
			rejectReason:   "prefix: " + validateReason,
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:           "suffixed rejection",
			backend:        "btcd",
			rejectReason:   validateReason + ": suffix",
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:           "malformed rejection",
			backend:        "btcd",
			rejectReason:   validateReason[:len(validateReason)-1],
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:    "unbracketed witness",
			backend: "btcd",
			rejectReason: inputReason(
				"failed to validate input", txHash, "0102",
			),
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:    "missing witness bracket",
			backend: "btcd",
			rejectReason: inputReason(
				"failed to validate input", txHash, "[0102",
			),
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:    "invalid witness separator",
			backend: "btcd",
			rejectReason: inputReason(
				"failed to validate input", txHash, "[0102,03]",
			),
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:           "script failure mapping mismatch",
			backend:        "btcd",
			rejectReason:   validateReason,
			mappedErr:      chain.ErrNonStandardScript,
			expectedErr:    chain.ErrNonStandardScript,
			notExpectedErr: chain.ErrScriptVerifyFlag,
		},
		{
			name:           "non-standard mapping mismatch",
			backend:        "btcd",
			rejectReason:   nonStandardReason,
			mappedErr:      chain.ErrUndefined,
			expectedErr:    chain.ErrUndefined,
			notExpectedErr: chain.ErrNonStandardInputs,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Act by refining only the supported backend rejection
			// evidence.
			err := normalizeMempoolAcceptError(
				testCase.backend, txHash, testCase.rejectReason,
				testCase.mappedErr,
			)

			// Assert exact input classification while every
			// unrelated or malformed rejection retains its original
			// error identity.
			require.ErrorIs(t, err, testCase.expectedErr)
			if testCase.notExpectedErr != nil {
				require.NotErrorIs(
					t, err, testCase.notExpectedErr,
				)
			}
		})
	}
}

// TestCheckMempoolAcceptanceBtcdInputFailure checks that raw btcd script
// failures are normalized on the wallet acceptance path.
func TestCheckMempoolAcceptanceBtcdInputFailure(t *testing.T) {
	t.Parallel()

	// Arrange the raw btcd script rejection and its coarse upstream mapping
	// with required single backend calls and one deferred mock
	// verification.
	mockChain := &lnmock.MockChain{}
	defer mockChain.AssertExpectations(t)

	tx := wire.NewMsgTx(2)
	rejectReason := "failed to validate input " + tx.TxHash().String() +
		":0 which references output " + wire.OutPoint{}.String() +
		" - false stack entry at end of script execution " +
		"(input witness [], input script bytes , " +
		"prev output script bytes 51)"
	results := []*btcjson.TestMempoolAcceptResult{{
		Txid:         tx.TxHash().String(),
		Allowed:      false,
		RejectReason: rejectReason,
	}}
	mockChain.On("TestMempoolAccept", []*wire.MsgTx{tx}, float64(0)).Return(
		results, nil,
	).Once()
	mockChain.On("MapRPCErr", mock.Anything).Return(
		chain.ErrUndefined,
	).Once()
	mockChain.On("BackEnd").Return("btcd").Once()
	wallet := &BtcWallet{chain: mockChain}

	// Act through the wallet acceptance entry point used by sweep probes.
	err := wallet.CheckMempoolAcceptance(tx)

	// Assert that the exact script failure is usable for input attribution
	// and is no longer merely an undefined backend rejection.
	require.ErrorIs(t, err, chain.ErrScriptVerifyFlag)
	require.NotErrorIs(t, err, chain.ErrUndefined)
}

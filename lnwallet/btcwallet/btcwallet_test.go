package btcwallet

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

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

// TestCheckMempoolAcceptance asserts the CheckMempoolAcceptance behaves as
// expected.
func TestCheckMempoolAcceptance(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock chain.Interface.
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

	err := wallet.CheckMempoolAcceptance(tx)
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

	witnessCases := []struct {
		name    string
		witness wire.TxWitness
		encoded string
	}{
		{"empty", nil, "[]"},
		{"single", wire.TxWitness{{0x01, 0x02}}, "[0102]"},
		{
			"multiple", wire.TxWitness{{0x01, 0x02}, {0x03}},
			"[0102 03]",
		},
		{
			"empty elements", wire.TxWitness{{}, {0x03}, {}},
			"[ 03 ]",
		},
	}
	for _, testCase := range witnessCases {
		actual := fmt.Sprintf("%x", testCase.witness)
		require.Equal(
			t, testCase.encoded, actual, testCase.name,
		)
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
	parseReason := inputReason(
		"failed to parse input", txHash, "[0102 03]",
	)
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
			err := normalizeMempoolAcceptError(
				testCase.backend, txHash,
				testCase.rejectReason, testCase.mappedErr,
			)

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

	err := wallet.CheckMempoolAcceptance(tx)

	require.ErrorIs(t, err, chain.ErrScriptVerifyFlag)
	require.NotErrorIs(t, err, chain.ErrUndefined)
}

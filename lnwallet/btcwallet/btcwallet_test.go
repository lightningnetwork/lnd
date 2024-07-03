package btcwallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
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

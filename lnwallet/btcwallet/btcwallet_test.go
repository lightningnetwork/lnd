package btcwallet

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lnwallet"
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

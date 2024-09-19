package invoices

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

type updateHTLCTest struct {
	name     string
	input    InvoiceHTLC
	invState ContractState
	setID    *[32]byte
	output   InvoiceHTLC
	expErr   error
}

// TestUpdateHTLC asserts the behavior of the updateHTLC method in various
// scenarios for MPP and AMP.
func TestUpdateHTLC(t *testing.T) {
	t.Parallel()

	testNow := time.Now()
	setID := [32]byte{0x01}
	ampRecord := record.NewAMP([32]byte{0x02}, setID, 3)
	preimage := lntypes.Preimage{0x04}
	hash := preimage.Hash()

	diffSetID := [32]byte{0x05}
	fakePreimage := lntypes.Preimage{0x06}
	testAlreadyNow := time.Now()

	tests := []updateHTLCTest{
		{
			name: "MPP accept",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			invState: ContractAccepted,
			setID:    nil,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			expErr: nil,
		},
		{
			name: "MPP accept, copy custom records",
			input: InvoiceHTLC{
				Amt:          5000,
				MppTotalAmt:  5000,
				AcceptHeight: 100,
				AcceptTime:   testNow,
				ResolveTime:  time.Time{},
				Expiry:       40,
				State:        HtlcStateAccepted,
				CustomRecords: record.CustomSet{
					0x01:   []byte{0x02},
					0xffff: []byte{0x04, 0x05, 0x06},
				},
				WireCustomRecords: lnwire.CustomRecords{
					0x010101: []byte{0x02, 0x03},
					0xffffff: []byte{0x44, 0x55, 0x66},
				},
				AMP: nil,
			},
			invState: ContractAccepted,
			setID:    nil,
			output: InvoiceHTLC{
				Amt:          5000,
				MppTotalAmt:  5000,
				AcceptHeight: 100,
				AcceptTime:   testNow,
				ResolveTime:  time.Time{},
				Expiry:       40,
				State:        HtlcStateAccepted,
				CustomRecords: record.CustomSet{
					0x01:   []byte{0x02},
					0xffff: []byte{0x04, 0x05, 0x06},
				},
				WireCustomRecords: lnwire.CustomRecords{
					0x010101: []byte{0x02, 0x03},
					0xffffff: []byte{0x44, 0x55, 0x66},
				},
				AMP: nil,
			},
			expErr: nil,
		},
		{
			name: "MPP settle",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			invState: ContractSettled,
			setID:    nil,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			expErr: nil,
		},
		{
			name: "MPP cancel",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			invState: ContractCanceled,
			setID:    nil,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP:               nil,
			},
			expErr: nil,
		},
		{
			name: "AMP accept missing preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: nil,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: nil,
				},
			},
			expErr: ErrHTLCPreimageMissing,
		},
		{
			name: "AMP accept invalid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &fakePreimage,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &fakePreimage,
				},
			},
			expErr: ErrHTLCPreimageMismatch,
		},
		{
			name: "AMP accept valid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "AMP accept valid preimage different htlc set",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractAccepted,
			setID:    &diffSetID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "AMP settle missing preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: nil,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: nil,
				},
			},
			expErr: ErrHTLCPreimageMissing,
		},
		{
			name: "AMP settle invalid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &fakePreimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &fakePreimage,
				},
			},
			expErr: ErrHTLCPreimageMismatch,
		},
		{
			name: "AMP settle valid preimage",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			// With the newer AMP logic, this is now valid, as we
			// want to be able to accept multiple settle attempts
			// to a given pay_addr. In this case, the HTLC should
			// remain in the accepted state.
			name: "AMP settle valid preimage different htlc set",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &diffSetID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "accept invoice htlc already settled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: ErrHTLCAlreadySettled,
		},
		{
			name: "cancel invoice htlc already settled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractCanceled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: ErrHTLCAlreadySettled,
		},
		{
			name: "settle invoice htlc already settled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateSettled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "cancel invoice",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       time.Time{},
				Expiry:            40,
				State:             HtlcStateAccepted,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractCanceled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "accept invoice htlc already canceled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractAccepted,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "cancel invoice htlc already canceled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractCanceled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
		{
			name: "settle invoice htlc already canceled",
			input: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			invState: ContractSettled,
			setID:    &setID,
			output: InvoiceHTLC{
				Amt:               5000,
				MppTotalAmt:       5000,
				AcceptHeight:      100,
				AcceptTime:        testNow,
				ResolveTime:       testAlreadyNow,
				Expiry:            40,
				State:             HtlcStateCanceled,
				CustomRecords:     make(record.CustomSet),
				WireCustomRecords: make(lnwire.CustomRecords),
				AMP: &InvoiceHtlcAMPData{
					Record:   *ampRecord,
					Hash:     hash,
					Preimage: &preimage,
				},
			},
			expErr: nil,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testUpdateHTLC(t, test, testNow)
		})
	}
}

func testUpdateHTLC(t *testing.T, test updateHTLCTest, now time.Time) {
	htlc := test.input.Copy()
	stateChanged, state, err := getUpdatedHtlcState(
		htlc, test.invState, test.setID,
	)
	if stateChanged {
		htlc.State = state
		htlc.ResolveTime = now
	}

	require.Equal(t, test.expErr, err)
	require.Equal(t, test.output, *htlc)
}

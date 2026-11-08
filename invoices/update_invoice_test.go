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

// TestResolveReplayedHtlcSettled checks preimage selection for settled HTLC
// replays.
func TestResolveReplayedHtlcSettled(t *testing.T) {
	t.Parallel()

	const missingPreimageErr = "settled invoice missing payment preimage"

	validPreimage := lntypes.Preimage{1}
	otherPreimage := lntypes.Preimage{2}
	validHash := validPreimage.Hash()
	otherHash := otherPreimage.Hash()
	setID := [32]byte{3}
	ampRecord := record.NewAMP([32]byte{4}, setID, 5)
	ampFeatures := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.AMPRequired),
		lnwire.Features,
	)

	tests := []struct {
		name             string
		invoicePreimage  *lntypes.Preimage
		invoiceFeatures  *lnwire.FeatureVector
		htlcAMP          *InvoiceHtlcAMPData
		paymentHash      lntypes.Hash
		expectedPreimage *lntypes.Preimage
		expectedErr      error
		expectedErrText  string
	}{
		{
			name:             "regular invoice",
			invoicePreimage:  &validPreimage,
			paymentHash:      validHash,
			expectedPreimage: &validPreimage,
		},
		{
			name:            "regular invoice missing preimage",
			paymentHash:     validHash,
			expectedErrText: missingPreimageErr,
		},
		{
			name:            "regular invoice preimage mismatch",
			invoicePreimage: &otherPreimage,
			paymentHash:     validHash,
			expectedErr:     ErrInvoicePreimageMismatch,
		},
		{
			name:            "AMP invoice",
			invoiceFeatures: ampFeatures,
			htlcAMP: &InvoiceHtlcAMPData{
				Record:   *ampRecord,
				Hash:     validHash,
				Preimage: &validPreimage,
			},
			paymentHash:      validHash,
			expectedPreimage: &validPreimage,
		},
		{
			name:            "AMP invoice missing HTLC data",
			invoiceFeatures: ampFeatures,
			paymentHash:     validHash,
			expectedErr:     ErrHTLCPreimageMissing,
		},
		{
			name:            "AMP invoice missing preimage",
			invoiceFeatures: ampFeatures,
			htlcAMP: &InvoiceHtlcAMPData{
				Record: *ampRecord,
				Hash:   validHash,
			},
			paymentHash: validHash,
			expectedErr: ErrHTLCPreimageMissing,
		},
		{
			name:            "AMP invoice preimage mismatch",
			invoiceFeatures: ampFeatures,
			htlcAMP: &InvoiceHtlcAMPData{
				Record:   *ampRecord,
				Hash:     validHash,
				Preimage: &otherPreimage,
			},
			paymentHash: validHash,
			expectedErr: ErrHTLCPreimageMismatch,
		},
		{
			name:            "AMP invoice hash mismatch",
			invoiceFeatures: ampFeatures,
			htlcAMP: &InvoiceHtlcAMPData{
				Record:   *ampRecord,
				Hash:     otherHash,
				Preimage: &otherPreimage,
			},
			paymentHash: validHash,
			expectedErr: ErrHTLCPreimageMismatch,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			circuitKey := CircuitKey{HtlcID: 1}
			ctx := &invoiceUpdateCtx{
				hash:       test.paymentHash,
				circuitKey: circuitKey,
			}
			invoice := &Invoice{
				Terms: ContractTerm{
					PaymentPreimage: test.invoicePreimage,
					Features:        test.invoiceFeatures,
				},
				Htlcs: map[CircuitKey]*InvoiceHTLC{
					circuitKey: {
						State: HtlcStateSettled,
						AMP:   test.htlcAMP,
					},
				},
			}

			replayed, resolution, err := resolveReplayedHtlc(
				ctx, invoice,
			)
			require.True(t, replayed)

			switch {
			case test.expectedErr != nil:
				require.ErrorIs(t, err, test.expectedErr)
				require.Nil(t, resolution)

			case test.expectedErrText != "":
				require.EqualError(t, err, test.expectedErrText)
				require.Nil(t, resolution)

			default:
				require.NoError(t, err)
				requireSettleResolution(
					t, resolution, ResultReplayToSettled,
				)
				settleResolution, ok :=
					resolution.(*HtlcSettleResolution)
				require.True(t, ok)
				require.Equal(
					t, *test.expectedPreimage,
					settleResolution.Preimage,
				)
			}
		})
	}
}

// TestUpdateInvoiceRejectsAmpWithoutMPP checks that AMP records follow the MPP
// update path.
func TestUpdateInvoiceRejectsAmpWithoutMPP(t *testing.T) {
	t.Parallel()

	ctx, invoice := newLegacyUpdateTestContext(t, ContractOpen)
	ctx.amp = record.NewAMP([32]byte{1}, [32]byte{2}, 3)

	update, resolution, err := updateInvoice(ctx, invoice)
	require.NoError(t, err)
	require.Nil(t, update)
	requireFailResolution(t, resolution, ResultAmpError)
}

// TestUpdateInvoiceRejectsAmpInvoiceInLegacyPath checks that AMP invoices are
// handled by the MPP update path.
func TestUpdateInvoiceRejectsAmpInvoiceInLegacyPath(t *testing.T) {
	t.Parallel()

	ctx, invoice := newLegacyUpdateTestContext(t, ContractOpen)
	invoice.Terms.PaymentPreimage = nil
	invoice.Terms.Features = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.PaymentAddrOptional,
			lnwire.AMPRequired,
		),
		lnwire.Features,
	)

	update, resolution, err := updateInvoice(ctx, invoice)
	require.NoError(t, err)
	require.Nil(t, update)
	requireFailResolution(t, resolution, ResultHtlcInvoiceTypeMismatch)
}

// TestUpdateMppRejectsStaleShard asserts that an MPP or AMP set cannot settle
// using a previously accepted shard whose expiry is now too close.
func TestUpdateMppRejectsStaleShard(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		isAMP            bool
		finalDelta       int32
		finalRejectDelta int32
	}{
		{
			name:             "mpp invoice delta",
			finalDelta:       4,
			finalRejectDelta: 2,
		},
		{
			name:             "amp invoice delta",
			isAMP:            true,
			finalDelta:       4,
			finalRejectDelta: 2,
		},
		{
			name:             "mpp reject delta",
			finalDelta:       2,
			finalRejectDelta: 4,
		},
		{
			name:             "amp reject delta",
			isAMP:            true,
			finalDelta:       2,
			finalRejectDelta: 4,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			const (
				currentHeight = int32(10)
				totalAmt      = lnwire.MilliSatoshi(100)
			)

			preimage := lntypes.Preimage{1}
			payAddr := [32]byte{2}
			setID := [32]byte{3}
			features := lnwire.NewFeatureVector(
				nil, lnwire.Features,
			)

			ctx := &invoiceUpdateCtx{
				hash:                 preimage.Hash(),
				circuitKey:           CircuitKey{HtlcID: 2},
				amtPaid:              40,
				expiry:               20,
				currentHeight:        currentHeight,
				finalCltvRejectDelta: testCase.finalRejectDelta,
				mpp: record.NewMPP(
					totalAmt, payAddr,
				),
				customRecords:     make(record.CustomSet),
				wireCustomRecords: make(lnwire.CustomRecords),
			}

			htlc := &InvoiceHTLC{
				Amt:         60,
				MppTotalAmt: totalAmt,
				Expiry:      13,
				State:       HtlcStateAccepted,
			}

			if testCase.isAMP {
				ampRecord := record.NewAMP(
					[32]byte{4}, setID, 0,
				)
				ctx.amp = record.NewAMP([32]byte{5}, setID, 1)
				htlc.AMP = &InvoiceHtlcAMPData{
					Record: *ampRecord,
					Hash:   lntypes.Hash{6},
				}
				features = lnwire.NewFeatureVector(
					lnwire.NewRawFeatureVector(
						lnwire.AMPRequired,
					),
					lnwire.Features,
				)
			}

			invoice := &Invoice{
				State: ContractOpen,
				Terms: ContractTerm{
					FinalCltvDelta:  testCase.finalDelta,
					PaymentPreimage: &preimage,
					PaymentAddr:     payAddr,
					Value:           totalAmt,
					Features:        features,
				},
				Htlcs: map[CircuitKey]*InvoiceHTLC{
					{HtlcID: 1}: htlc,
				},
			}
			if testCase.isAMP {
				otherSetID := [32]byte{9}
				otherRecord := record.NewAMP(
					[32]byte{8}, otherSetID, 0,
				)
				invoice.Htlcs[CircuitKey{HtlcID: 3}] =
					&InvoiceHTLC{
						Amt:         60,
						MppTotalAmt: totalAmt,
						Expiry:      20,
						State:       HtlcStateAccepted,
						AMP: &InvoiceHtlcAMPData{
							Record: *otherRecord,
							Hash:   lntypes.Hash{7},
						},
					}
			}

			update, resolution, err := updateMpp(ctx, invoice)
			require.NoError(t, err)
			require.NotNil(t, update)
			require.Equal(t, CancelHTLCsUpdate, update.UpdateType)
			require.Equal(
				t, map[CircuitKey]struct{}{{HtlcID: 1}: {}},
				update.CancelHtlcs,
			)
			require.Empty(t, update.AddHtlcs)
			if testCase.isAMP {
				require.Equal(t, (*SetID)(&setID), update.SetID)
			} else {
				require.Nil(t, update.SetID)
			}
			requireFailResolution(
				t, resolution, ResultHtlcSetExpiryTooSoon,
			)
		})
	}
}

// TestUpdateLegacyRejectsNilPreimageSettle checks the outcome when a legacy
// settlement has no invoice-level preimage.
func TestUpdateLegacyRejectsNilPreimageSettle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state ContractState
	}{
		{
			name:  "new settle",
			state: ContractOpen,
		},
		{
			name:  "duplicate settled",
			state: ContractSettled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx, invoice := newLegacyUpdateTestContext(
				t, test.state,
			)
			invoice.Terms.PaymentPreimage = nil

			update, resolution, err := updateLegacy(ctx, invoice)
			require.NoError(t, err)
			require.Nil(t, update)
			requireFailResolution(
				t, resolution, ResultHtlcInvoiceTypeMismatch,
			)
		})
	}
}

// TestUpdateLegacyValidatesKeysendRecord checks that the keysend record is
// well-formed and corresponds to the payment hash.
func TestUpdateLegacyValidatesKeysendRecord(t *testing.T) {
	t.Parallel()

	validPreimage := lntypes.Preimage{1}
	invalidPreimage := lntypes.Preimage{2}

	tests := []struct {
		name           string
		keysendRecord  []byte
		expectFail     bool
		expectedResult FailResolutionResult
	}{
		{
			name:           "missing keysend",
			expectFail:     true,
			expectedResult: ResultAddressMismatch,
		},
		{
			name:           "invalid keysend length",
			keysendRecord:  []byte{1, 2, 3},
			expectFail:     true,
			expectedResult: ResultAddressMismatch,
		},
		{
			name:           "wrong keysend preimage",
			keysendRecord:  invalidPreimage[:],
			expectFail:     true,
			expectedResult: ResultAddressMismatch,
		},
		{
			name:          "valid keysend",
			keysendRecord: validPreimage[:],
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx, invoice := newLegacyUpdateTestContext(
				t, ContractOpen,
			)
			ctx.hash = validPreimage.Hash()
			ctx.customRecords = make(record.CustomSet)
			invoice.Terms.PaymentPreimage = &validPreimage
			invoice.Terms.Features = lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadRequired,
					lnwire.PaymentAddrRequired,
				),
				lnwire.Features,
			)

			if test.keysendRecord != nil {
				ctx.customRecords[record.KeySendType] =
					test.keysendRecord
			}

			update, resolution, err := updateLegacy(ctx, invoice)
			require.NoError(t, err)

			if test.expectFail {
				require.Nil(t, update)
				requireFailResolution(
					t, resolution, test.expectedResult,
				)

				return
			}

			require.NotNil(t, update)
			requireSettleResolution(t, resolution, ResultSettled)
		})
	}
}

// TestUpdateMppRejectsSettledAmpSet checks that a fresh shard for a paid AMP
// set is rejected without an updater error that would fail the channel link.
func TestUpdateMppRejectsSettledAmpSet(t *testing.T) {
	t.Parallel()

	// Arrange a reusable AMP invoice with one paid set and a fresh circuit
	// for that same set. A partial amount avoids preimage reconstruction,
	// isolating the closed-set admission guard.
	settledSetID := [32]byte{1}
	ctx, invoice := newLegacyUpdateTestContext(t, ContractOpen)
	ctx.circuitKey = CircuitKey{HtlcID: 2}
	ctx.amtPaid = 400
	ctx.mpp = record.NewMPP(1000, invoice.Terms.PaymentAddr)
	ctx.amp = record.NewAMP([32]byte{3}, settledSetID, 1)
	invoice.Terms.PaymentPreimage = nil
	invoice.Terms.Features = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.AMPRequired), lnwire.Features,
	)
	invoice.AmtPaid = 1000
	invoice.Htlcs[CircuitKey{HtlcID: 1}] = &InvoiceHTLC{
		Amt:         1000,
		MppTotalAmt: 1000,
		State:       HtlcStateSettled,
		AMP: &InvoiceHtlcAMPData{
			Record: *record.NewAMP([32]byte{4}, settledSetID, 0),
		},
	}

	// Act through new-shard admission so rejection must be a normal HTLC
	// resolution rather than an updater error that fails its channel link.
	update, resolution, err := updateInvoice(ctx, invoice)

	// Assert rejection preserves the paid set and its accounting, while
	// returning no invoice update and no internal error to the link.
	require.NoError(t, err)
	require.Nil(t, update)
	requireFailResolution(t, resolution, ResultInvoiceNotOpen)
	require.Equal(t, ContractOpen, invoice.State)
	require.Equal(t, lnwire.MilliSatoshi(1000), invoice.AmtPaid)
	require.Zero(t, invoice.SettleIndex)
	require.Len(t, invoice.Htlcs, 1)
}

// newLegacyUpdateTestContext creates a minimal legacy invoice and update
// context for exercising update selection and settlement outcomes.
func newLegacyUpdateTestContext(t *testing.T,
	state ContractState) (*invoiceUpdateCtx, *Invoice) {

	t.Helper()

	preimage := lntypes.Preimage{1}
	payHash := preimage.Hash()

	ctx := &invoiceUpdateCtx{
		hash:                 payHash,
		circuitKey:           CircuitKey{HtlcID: 1},
		amtPaid:              lnwire.MilliSatoshi(1000),
		expiry:               40,
		currentHeight:        10,
		finalCltvRejectDelta: 10,
		customRecords:        make(record.CustomSet),
		wireCustomRecords:    make(lnwire.CustomRecords),
	}

	invoice := &Invoice{
		State: state,
		Terms: ContractTerm{
			FinalCltvDelta:  10,
			PaymentPreimage: &preimage,
			Value:           1000,
			Features: lnwire.NewFeatureVector(
				nil, lnwire.Features,
			),
		},
		Htlcs: make(map[CircuitKey]*InvoiceHTLC),
	}

	return ctx, invoice
}

// requireFailResolution checks the resolution type and its reported outcome.
func requireFailResolution(t *testing.T, resolution HtlcResolution,
	expected FailResolutionResult) {

	t.Helper()

	failResolution, ok := resolution.(*HtlcFailResolution)
	require.True(t, ok)
	require.Equal(t, expected, failResolution.Outcome)
}

// requireSettleResolution checks the resolution type and its reported outcome.
func requireSettleResolution(t *testing.T, resolution HtlcResolution,
	expected SettleResolutionResult) {

	t.Helper()

	settleResolution, ok := resolution.(*HtlcSettleResolution)
	require.True(t, ok)
	require.Equal(t, expected, settleResolution.Outcome)
}

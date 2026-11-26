package invoicesrpc

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestCreateRPCInvoiceBolt12 verifies that a BOLT 12 invoice converts to an
// RPC invoice. Its lni1 payment request is not a BOLT 11 string, so the
// conversion must take the payment hash from the preimage.
func TestCreateRPCInvoiceBolt12(t *testing.T) {
	t.Parallel()

	const payReq = "lni1qqgsqvgnwgcg35z6ee2h3yczraddm72xrfua9uve2rlrm9" +
		"deu7xyfzr"

	// The conversion would fail if it decoded the string as BOLT 11.
	_, err := zpay32.Decode(payReq, &chaincfg.MainNetParams)
	require.Error(t, err)

	var preimage lntypes.Preimage
	preimage[0] = 0x12

	invoice := &invoices.Invoice{
		PaymentRequest: []byte(payReq),
		IsBolt12:       true,
		Terms: invoices.ContractTerm{
			PaymentPreimage: &preimage,
			Value:           lnwire.MilliSatoshi(10000),
			Features: lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.MPPOptional,
				), lnwire.Features,
			),
		},
	}

	rpcInvoice, err := CreateRPCInvoice(invoice, &chaincfg.MainNetParams)
	require.NoError(t, err)

	hash := preimage.Hash()
	require.Equal(t, hash[:], rpcInvoice.RHash)
	require.Equal(t, payReq, rpcInvoice.PaymentRequest)
	require.EqualValues(t, 10000, rpcInvoice.ValueMsat)
}

// testPreimage is a deterministic preimage used across tests.
var testPreimage = lntypes.Preimage{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// TestCreateRPCInvoiceNoPayReq verifies that an invoice without a BOLT 11
// payment request takes its payment hash from the preimage, and has none
// without a preimage.
func TestCreateRPCInvoiceNoPayReq(t *testing.T) {
	t.Parallel()

	preimage := testPreimage
	hash := preimage.Hash()

	tests := []struct {
		name     string
		bolt12   bool
		preimage *lntypes.Preimage
		wantHash []byte
	}{
		{
			name:     "keysend",
			preimage: &preimage,
			wantHash: hash[:],
		},
		{
			name:   "bolt12 without preimage",
			bolt12: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			features := lnwire.EmptyFeatureVector()
			inv := &invoices.Invoice{
				IsBolt12: test.bolt12,
				Terms: invoices.ContractTerm{
					PaymentPreimage: test.preimage,
					Features:        features,
				},
			}

			rpcInv, err := CreateRPCInvoice(
				inv, &chaincfg.MainNetParams,
			)
			require.NoError(t, err)
			require.Equal(t, test.wantHash, rpcInv.RHash)
		})
	}
}

// TestCreateRPCInvoiceBolt12Detail verifies that a BOLT 12 invoice shows its
// offer hash, payer id and quantity, and that a BOLT 11 invoice shows none.
func TestCreateRPCInvoiceBolt12Detail(t *testing.T) {
	t.Parallel()

	offerHash := bytes.Repeat([]byte{0xab}, 32)
	payerID := []byte{0x03, 0x02}

	tests := []struct {
		name       string
		bolt12     bool
		wantDetail *lnrpc.OfferInvoiceDetail
	}{
		{
			name:   "bolt12",
			bolt12: true,
			wantDetail: &lnrpc.OfferInvoiceDetail{
				OfferHash:     offerHash,
				InvreqPayerId: payerID,
				Quantity:      3,
			},
		},
		{
			name: "bolt11",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			preimage := testPreimage
			features := lnwire.EmptyFeatureVector()
			inv := &invoices.Invoice{
				IsBolt12:       test.bolt12,
				OfferHash:      offerHash,
				InvreqPayerID:  payerID,
				InvreqQuantity: 3,
				Terms: invoices.ContractTerm{
					PaymentPreimage: &preimage,
					Features:        features,
				},
			}

			rpcInv, err := CreateRPCInvoice(
				inv, &chaincfg.MainNetParams,
			)
			require.NoError(t, err)

			if test.wantDetail == nil {
				require.Nil(t, rpcInv.Bolt12Detail)

				return
			}
			require.True(t, proto.Equal(
				test.wantDetail, rpcInv.Bolt12Detail,
			))
		})
	}
}

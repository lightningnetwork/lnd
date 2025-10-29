package itest

import (
	"bytes"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testInvoiceMetadata tests that metadata can be properly set and retrieved
// for invoices via AddInvoice, AddHoldInvoice, and DecodePayReq.
func testInvoiceMetadata(ht *lntest.HarnessTest) {
	// Open a channel with 100k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	chanAmt := btcutil.Amount(100000)
	chanPoints, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil}, lntest.OpenChannelParams{Amt: chanAmt},
	)
	cp := chanPoints[0]
	alice, bob := nodes[0], nodes[1]

	// assertAmountPaid is a helper closure that asserts the amount paid by
	// Alice and received by Bob are expected.
	assertAmountPaid := func(expected int64) {
		ht.AssertAmountPaid("alice -> bob", alice, cp, expected, 0)
		ht.AssertAmountPaid("bob <- alice", bob, cp, 0, expected)
	}

	const paymentAmt = 1000

	// Test 1: Create a regular invoice with metadata using AddInvoice.
	testMetadata := []byte{0x01, 0xfa, 0xfa, 0xf0}
	preimage := bytes.Repeat([]byte("B"), 32)
	invoice := &lnrpc.Invoice{
		Memo:      "metadata test",
		RPreimage: preimage,
		Value:     paymentAmt,
		Metadata:  testMetadata,
	}
	invoiceResp := bob.RPC.AddInvoice(invoice)

	// Decode the payment request and verify metadata is present.
	payReq := bob.RPC.DecodePayReq(invoiceResp.PaymentRequest)
	require.Equal(ht, testMetadata, payReq.Metadata,
		"decoded metadata should match")

	// Pay the invoice and verify it settles.
	ht.CompletePaymentRequests(alice, []string{invoiceResp.PaymentRequest})

	// Verify the invoice is marked as settled and metadata persists in DB.
	dbInvoice := bob.RPC.LookupInvoice(invoiceResp.RHash)
	require.Equal(ht, lnrpc.Invoice_SETTLED, dbInvoice.State,
		"bob's invoice should be marked as settled")
	require.Equal(ht, testMetadata, dbInvoice.Metadata,
		"metadata should persist when fetching invoice from DB")

	// Verify balances are updated.
	assertAmountPaid(paymentAmt)

	// Test 2: Create a hold invoice with metadata using AddHoldInvoice.
	testMetadata2 := []byte{0xde, 0xad, 0xbe, 0xef}
	preimage2, err := lntypes.MakePreimage(bytes.Repeat([]byte("C"), 32))
	require.NoError(ht, err, "unable to make preimage")
	hash := preimage2.Hash()

	holdInvoice := &invoicesrpc.AddHoldInvoiceRequest{
		Memo:     "hold invoice metadata test",
		Hash:     hash[:],
		Value:    paymentAmt,
		Metadata: testMetadata2,
	}
	holdInvoiceResp := bob.RPC.AddHoldInvoice(holdInvoice)

	// Decode the hold invoice payment request and verify metadata.
	holdPayReq := bob.RPC.DecodePayReq(holdInvoiceResp.PaymentRequest)
	require.Equal(ht, testMetadata2, holdPayReq.Metadata,
		"decoded hold invoice metadata should match")

	// Lookup the hold invoice from DB and verify metadata persists.
	dbHoldInvoice := bob.RPC.LookupInvoice(hash[:])
	require.Equal(ht, testMetadata2, dbHoldInvoice.Metadata,
		"hold invoice metadata should persist when fetching from DB")

	// Test 3: Create an invoice without metadata and verify it's empty.
	invoice3 := &lnrpc.Invoice{
		Memo:  "no metadata",
		Value: paymentAmt,
	}
	invoiceResp3 := bob.RPC.AddInvoice(invoice3)

	// Decode and verify metadata is nil or empty.
	payReq3 := bob.RPC.DecodePayReq(invoiceResp3.PaymentRequest)
	require.Empty(ht, payReq3.Metadata,
		"invoice without metadata should have empty metadata field")

	// Lookup from DB and verify metadata remains empty.
	dbInvoice3 := bob.RPC.LookupInvoice(invoiceResp3.RHash)
	require.Empty(ht, dbInvoice3.Metadata,
		"invoice without metadata should have empty metadata in DB")
}

package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testHoldInvoiceAutoGenerate tests that hold invoices can be created without
// providing a hash. The server auto-generates a preimage, derives the hash,
// and returns the preimage in the response without persisting it. It also
// verifies the hash-only flow still works unchanged.
func testHoldInvoiceAutoGenerate(ht *lntest.HarnessTest) {
	// Open a channel between Alice and Bob.
	_, nodes := ht.CreateSimpleNetwork(
		[][]string{nil, nil}, lntest.OpenChannelParams{
			Amt: btcutil.Amount(1_000_000),
		},
	)
	alice, bob := nodes[0], nodes[1]

	// Test 1: Auto-generate preimage and hash (no hash provided).
	autoReq := &invoicesrpc.AddHoldInvoiceRequest{
		Memo:  "auto-generated",
		Value: 10_000,
	}
	autoResp := bob.RPC.AddHoldInvoice(autoReq)

	// The response must contain both a preimage and hash.
	require.Len(ht, autoResp.PaymentPreimage, 32,
		"expected 32-byte preimage")
	require.Len(ht, autoResp.PaymentHash, 32,
		"expected 32-byte payment hash")

	// Verify the hash matches SHA256 of the preimage.
	var autoPreimage lntypes.Preimage
	copy(autoPreimage[:], autoResp.PaymentPreimage)
	autoHash := autoPreimage.Hash()
	require.Equal(ht, autoHash[:], autoResp.PaymentHash,
		"hash should be SHA256 of preimage")

	// Verify the generated preimage is not persisted: a LookupInvoice
	// before settlement must report an empty r_preimage. The server
	// only learns the preimage again when the caller passes it to
	// SettleInvoice.
	lookup := bob.RPC.LookupInvoice(autoResp.PaymentHash)
	require.Empty(ht, lookup.RPreimage,
		"auto-generated preimage must not be stored in the DB")

	// Subscribe, pay, and settle.
	autoStream := bob.RPC.SubscribeSingleInvoice(autoResp.PaymentHash)

	ht.SendPaymentAndAssertStatus(alice, &routerrpc.SendPaymentRequest{
		PaymentRequest: autoResp.PaymentRequest,
		FeeLimitSat:    1_000_000,
	}, lnrpc.Payment_IN_FLIGHT)

	ht.AssertInvoiceState(autoStream, lnrpc.Invoice_ACCEPTED)

	bob.RPC.SettleInvoice(autoResp.PaymentPreimage)
	ht.AssertInvoiceState(autoStream, lnrpc.Invoice_SETTLED)
	ht.AssertPaymentStatus(
		alice, autoHash, lnrpc.Payment_SUCCEEDED,
	)

	// Test 2: Traditional hash-only flow still works. The response
	// preimage must be empty because the server does not know it.
	var hashPreimage lntypes.Preimage
	copy(hashPreimage[:], ht.Random32Bytes())
	payHash := hashPreimage.Hash()

	hashResp := bob.RPC.AddHoldInvoice(
		&invoicesrpc.AddHoldInvoiceRequest{
			Memo:  "hash-only",
			Value: 10_000,
			Hash:  payHash[:],
		},
	)

	require.Empty(ht, hashResp.PaymentPreimage,
		"preimage should be empty for hash-only")
	require.Equal(ht, payHash[:], hashResp.PaymentHash,
		"returned hash should match provided hash")

	// Subscribe, pay, and settle.
	hashStream := bob.RPC.SubscribeSingleInvoice(payHash[:])

	ht.SendPaymentAndAssertStatus(alice, &routerrpc.SendPaymentRequest{
		PaymentRequest: hashResp.PaymentRequest,
		FeeLimitSat:    1_000_000,
	}, lnrpc.Payment_IN_FLIGHT)

	ht.AssertInvoiceState(hashStream, lnrpc.Invoice_ACCEPTED)

	bob.RPC.SettleInvoice(hashPreimage[:])
	ht.AssertInvoiceState(hashStream, lnrpc.Invoice_SETTLED)
	ht.AssertPaymentStatus(
		alice, payHash, lnrpc.Payment_SUCCEEDED,
	)
}

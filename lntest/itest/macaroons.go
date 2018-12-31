// +build rpctest

package itest

import (
	"context"
	"encoding/hex"
	"github.com/btcsuite/btcutil"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/macaroons"
	"gopkg.in/macaroon.v2"
)

// errContains is a helper function that returns true if a string is contained
// in the message of an error.
func errContains(err error, str string) bool {
	return strings.Contains(err.Error(), str)
}

// testMacaroonAuthentication makes sure that if macaroon authentication is
// enabled on the gRPC interface, no requests with missing or invalid
// macaroons are allowed. Further, the specific access rights (read/write,
// entity based) and first-party caveats are tested as well.
func testMacaroonAuthentication(net *lntest.NetworkHarness, t *harnessTest) {
	var (
		ctxb       = context.Background()
		infoReq    = &lnrpc.GetInfoRequest{}
		newAddrReq = &lnrpc.NewAddressRequest{
			Type: AddrTypeWitnessPubkeyHash,
		}
		testNode = net.Alice
	)

	// First test: Make sure we get an error if we use no macaroons but try
	// to connect to a node that has macaroon authentication enabled.
	conn, err := testNode.ConnectRPC(false)
	if err != nil {
		t.Fatalf("unable to connect to alice: %v", err)
	}
	defer conn.Close()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	noMacConnection := lnrpc.NewLightningClient(conn)
	_, err = noMacConnection.GetInfo(ctxt, infoReq)
	if err == nil || !errContains(err, "expected 1 macaroon") {
		t.Fatalf("expected to get an error when connecting without " +
			"macaroons")
	}

	// Second test: Ensure that an invalid macaroon also triggers an error.
	invalidMac, _ := macaroon.New(
		[]byte("dummy_root_key"), []byte("0"), "itest",
		macaroon.LatestVersion,
	)
	conn, err = testNode.ConnectRPCWithMacaroon(invalidMac)
	if err != nil {
		t.Fatalf("unable to connect to alice: %v", err)
	}
	defer conn.Close()
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	invalidMacConnection := lnrpc.NewLightningClient(conn)
	_, err = invalidMacConnection.GetInfo(ctxt, infoReq)
	if err == nil || !errContains(err, "cannot get macaroon") {
		t.Fatalf("expected to get an error when connecting with an " +
			"invalid macaroon")
	}

	// Third test: Try to access a write method with read-only macaroon.
	readonlyMac, err := testNode.ReadMacaroon(
		testNode.ReadMacPath(), defaultTimeout,
	)
	if err != nil {
		t.Fatalf("unable to read readonly.macaroon from node: %v", err)
	}
	conn, err = testNode.ConnectRPCWithMacaroon(readonlyMac)
	if err != nil {
		t.Fatalf("unable to connect to alice: %v", err)
	}
	defer conn.Close()
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	readonlyMacConnection := lnrpc.NewLightningClient(conn)
	_, err = readonlyMacConnection.NewAddress(ctxt, newAddrReq)
	if err == nil || !errContains(err, "permission denied") {
		t.Fatalf("expected to get an error when connecting to " +
			"write method with read-only macaroon")
	}

	// Fourth test: Check first-party caveat with timeout that expired
	// 30 seconds ago.
	timeoutMac, err := macaroons.AddConstraints(
		readonlyMac, macaroons.TimeoutConstraint(-30),
	)
	if err != nil {
		t.Fatalf("unable to add constraint to readonly macaroon: %v",
			err)
	}
	conn, err = testNode.ConnectRPCWithMacaroon(timeoutMac)
	if err != nil {
		t.Fatalf("unable to connect to alice: %v", err)
	}
	defer conn.Close()
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	timeoutMacConnection := lnrpc.NewLightningClient(conn)
	_, err = timeoutMacConnection.GetInfo(ctxt, infoReq)
	if err == nil || !errContains(err, "macaroon has expired") {
		t.Fatalf("expected to get an error when connecting with an " +
			"invalid macaroon")
	}

	// Fifth test: Check first-party caveat with invalid IP address.
	invalidIpAddrMac, err := macaroons.AddConstraints(
		readonlyMac, macaroons.IPLockConstraint("1.1.1.1"),
	)
	if err != nil {
		t.Fatalf("unable to add constraint to readonly macaroon: %v",
			err)
	}
	conn, err = testNode.ConnectRPCWithMacaroon(invalidIpAddrMac)
	if err != nil {
		t.Fatalf("unable to connect to alice: %v", err)
	}
	defer conn.Close()
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	invalidIpAddrMacConnection := lnrpc.NewLightningClient(conn)
	_, err = invalidIpAddrMacConnection.GetInfo(ctxt, infoReq)
	if err == nil || !errContains(err, "different IP address") {
		t.Fatalf("expected to get an error when connecting with an " +
			"invalid macaroon")
	}

	// Sixth test: Make sure that if we do everything correct and send
	// the admin macaroon with first-party caveats that we can satisfy,
	// we get a correct answer.
	adminMac, err := testNode.ReadMacaroon(
		testNode.AdminMacPath(), defaultTimeout,
	)
	if err != nil {
		t.Fatalf("unable to read admin.macaroon from node: %v", err)
	}
	adminMac, err = macaroons.AddConstraints(
		adminMac, macaroons.TimeoutConstraint(30),
		macaroons.IPLockConstraint("127.0.0.1"),
	)
	if err != nil {
		t.Fatalf("unable to add constraints to admin macaroon: %v", err)
	}
	conn, err = testNode.ConnectRPCWithMacaroon(adminMac)
	if err != nil {
		t.Fatalf("unable to connect to alice: %v", err)
	}
	defer conn.Close()
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	adminMacConnection := lnrpc.NewLightningClient(conn)
	res, err := adminMacConnection.NewAddress(ctxt, newAddrReq)
	if err != nil {
		t.Fatalf("unable to get new address with valid macaroon: %v",
			err)
	}
	if !strings.HasPrefix(res.Address, "bcrt1") {
		t.Fatalf("returned address was not a regtest address")
	}
}

// testMacaroonAccountBalance makes sure that if a macaroon is locked to an
// account, all off-chain payments spend from that account. The account balance
// must therefore always be sufficient for new payments to succeed.
func testMacaroonAccountBalance(net *lntest.NetworkHarness, t *harnessTest) {
	var (
		ctxb           = context.Background()
		testAccBalance = uint64(9735)
		newAccReq      = &lnrpc.CreateAccountRequest{
			AccountBalance: testAccBalance,
		}
		errContains = func(err error, str string) bool {
			return strings.Contains(err.Error(), str)
		}
	)

	// First of all, create a new account on Alice.
	adminMac, err := net.Alice.ReadMacaroon(
		net.Alice.AdminMacPath(), defaultTimeout,
	)
	if err != nil {
		t.Fatalf("unable to read admin.macaroon from node: %v", err)
	}
	conn, err := net.Alice.ConnectRPCWithMacaroon(adminMac)
	if err != nil {
		t.Fatalf("unable to connect to alice: %v", err)
	}
	defer conn.Close()
	ctxt, _ := context.WithTimeout(context.Background(), defaultTimeout)
	adminConnection := lnrpc.NewLightningClient(conn)
	accountRes, err := adminConnection.CreateAccount(ctxt, newAccReq)
	if err != nil {
		t.Fatalf("unable to create account: %v", err)
	}
	account := accountRes.Account
	if len(account.Id) != macaroons.AccountIDLen*2 {
		t.Fatalf("account %s has unexpected length", account.Id)
	}
	var accountID macaroons.AccountID
	accountIDBytes, err := hex.DecodeString(account.Id)
	if err != nil {
		t.Fatalf("unable to parse account ID as hex: %v", err)
	}
	copy(accountID[:], accountIDBytes)

	// Make sure we get the same account back when we list all accounts.
	ctxt, _ = context.WithTimeout(context.Background(), defaultTimeout)
	listAccReq := &lnrpc.ListAccountsRequest{}
	listRes, err := adminConnection.ListAccounts(ctxt, listAccReq)
	if err != nil {
		t.Fatalf("unable to list accounts: %v", err)
	}
	if len(listRes.Accounts) != 1 {
		t.Fatalf(
			"number of accounts don't match. expected %d, got %d",
			1, len(listRes.Accounts),
		)
	}
	if listRes.Accounts[0].Id != account.Id {
		t.Fatalf("created account is not in list")
	}

	// Add a caveat to the macaroon that locks it to the account we just
	// created and create a client connection to Alice with the new
	// macaroon.
	accountMac, err := macaroons.AddConstraints(
		adminMac, macaroons.AccountLockConstraint(accountID),
	)
	if err != nil {
		t.Fatalf("unable to add constraints to admin macaroon: %v", err)
	}
	conn, err = net.Alice.ConnectRPCWithMacaroon(accountMac)
	if err != nil {
		t.Fatalf("unable to connect to alice: %v", err)
	}
	defer conn.Close()
	ctxt, _ = context.WithTimeout(context.Background(), defaultTimeout)
	accountConnection := lnrpc.NewLightningClient(conn)

	// Now create a channel that we can use to test our account with.
	const chanAmt = btcutil.Amount(500000)

	// Open a channel with 500k satoshis between Alice and Bob with Alice
	// being the sole funder of the channel.
	ctxt, _ = context.WithTimeout(ctxb, channelOpenTimeout)
	_ = openChannelAndAssert(
		ctxt, t, net, net.Alice, net.Bob,
		lntest.OpenChannelParams{
			Amt: chanAmt,
		},
	)

	// The first payment we are going to try to pay is too large, exceeding
	// the account's balance (10'000 > 9'735).
	const paymentAmtTooLarge = 10000
	invoice := &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: makeFakePayHash(t),
		Value:     paymentAmtTooLarge,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err := net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Try to pay the invoice from Alice to Bob.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	payInvoiceReq := &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	payInvoiceRes, err := accountConnection.SendPaymentSync(
		ctxt, payInvoiceReq,
	)
	if err != nil {
		t.Fatalf("unexpected error when trying to pay invoice: %v", err)
	}
	if len(payInvoiceRes.PaymentError) == 0 ||
		payInvoiceRes.PaymentError != "account balance insufficient" {
		t.Fatalf("expected to get an error when paying an invoice "+
			"that exceeds the account balance, instead we got: %s",
			payInvoiceRes.PaymentError,
		)
	}

	// The second attempt is with an amount that we expect to succeed.
	const paymentAmtOk = 1000
	invoice = &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: makeFakePayHash(t),
		Value:     paymentAmtOk,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err = net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Try to pay the invoice from Alice to Bob.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	payInvoiceReq = &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	payInvoiceRes, err = accountConnection.SendPaymentSync(
		ctxt, payInvoiceReq,
	)
	if err != nil {
		t.Fatalf("unexpected error when trying to pay invoice: %v", err)
	}
	if len(payInvoiceRes.PaymentError) != 0 {
		t.Fatalf(
			"unexpected error when trying to pay invoice: %v",
			payInvoiceRes.PaymentError,
		)
	}

	// Now make sure our account's balance has been charged with the correct
	// amount.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	listAccRes, err := adminConnection.ListAccounts(ctxt, listAccReq)
	if err != nil {
		t.Fatalf("unable to list accounts: %v", err)
	}
	if len(listAccRes.Accounts) != 1 {
		t.Fatalf(
			"number of accounts don't match. expected %d, got %d",
			1, len(listRes.Accounts),
		)
	}
	if listAccRes.Accounts[0].CurrentBalance != 8735 {
		t.Fatalf(
			"invalid account balance. expected %d, got %d",
			8735, listAccRes.Accounts[0].CurrentBalance,
		)
	}

	// For the last try, we remove the account and then try to charge it
	// anyway.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	removeAccReq := &lnrpc.RemoveAccountRequest{
		Id: account.Id,
	}
	_, err = adminConnection.RemoveAccount(ctxt, removeAccReq)
	if err != nil {
		t.Fatalf("unable to remove account: %v", err)
	}
	invoice = &lnrpc.Invoice{
		Memo:      "testing",
		RPreimage: makeFakePayHash(t),
		Value:     paymentAmtOk,
	}
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	invoiceResp, err = net.Bob.AddInvoice(ctxt, invoice)
	if err != nil {
		t.Fatalf("unable to add invoice: %v", err)
	}

	// Try to pay the invoice from Alice to Bob with the deleted account.
	ctxt, _ = context.WithTimeout(ctxb, defaultTimeout)
	payInvoiceReq = &lnrpc.SendRequest{
		PaymentRequest: invoiceResp.PaymentRequest,
	}
	_, err = accountConnection.SendPaymentSync(
		ctxt, payInvoiceReq,
	)
	if err == nil || !errContains(err, "account not found") {
		t.Fatalf("expected to get an error when paying invoice")
	}
}

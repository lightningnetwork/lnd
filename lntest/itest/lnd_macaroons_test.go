// +build rpctest

package itest

import (
	"context"
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

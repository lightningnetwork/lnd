// +build rpctest

package itest

import (
	"context"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t.t, err)
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
	require.NoError(t.t, err)
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
	require.NoError(t.t, err)
	conn, err = testNode.ConnectRPCWithMacaroon(readonlyMac)
	require.NoError(t.t, err)
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
	require.NoError(t.t, err)
	conn, err = testNode.ConnectRPCWithMacaroon(timeoutMac)
	require.NoError(t.t, err)
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
	require.NoError(t.t, err)
	conn, err = testNode.ConnectRPCWithMacaroon(invalidIpAddrMac)
	require.NoError(t.t, err)
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
	require.NoError(t.t, err)
	adminMac, err = macaroons.AddConstraints(
		adminMac, macaroons.TimeoutConstraint(30),
		macaroons.IPLockConstraint("127.0.0.1"),
	)
	require.NoError(t.t, err)
	conn, err = testNode.ConnectRPCWithMacaroon(adminMac)
	require.NoError(t.t, err)
	defer conn.Close()
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	adminMacConnection := lnrpc.NewLightningClient(conn)
	res, err := adminMacConnection.NewAddress(ctxt, newAddrReq)
	require.NoError(t.t, err)
	assert.Contains(t.t, res.Address, "bcrt1")
}

// testBakeMacaroon checks that when creating macaroons, the permissions param
// in the request must be set correctly, and the baked macaroon has the intended
// permissions.
func testBakeMacaroon(net *lntest.NetworkHarness, t *harnessTest) {
	var (
		ctxb     = context.Background()
		req      = &lnrpc.BakeMacaroonRequest{}
		testNode = net.Alice
	)

	// First test: when the permission list is empty in the request, an error
	// should be returned.
	adminMac, err := testNode.ReadMacaroon(
		testNode.AdminMacPath(), defaultTimeout,
	)
	require.NoError(t.t, err)
	conn, err := testNode.ConnectRPCWithMacaroon(adminMac)
	require.NoError(t.t, err)
	defer conn.Close()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	adminMacConnection := lnrpc.NewLightningClient(conn)
	_, err = adminMacConnection.BakeMacaroon(ctxt, req)
	if err == nil || !errContains(err, "permission list cannot be empty") {
		t.Fatalf("expected an error, got %v", err)
	}

	// Second test: when the action in the permission list is not valid,
	// an error should be returned.
	req = &lnrpc.BakeMacaroonRequest{
		Permissions: []*lnrpc.MacaroonPermission{
			{
				Entity: "macaroon",
				Action: "invalid123",
			},
		},
	}
	_, err = adminMacConnection.BakeMacaroon(ctxt, req)
	if err == nil || !errContains(err, "invalid permission action") {
		t.Fatalf("expected an error, got %v", err)
	}

	// Third test: when the entity in the permission list is not valid,
	// an error should be returned.
	req = &lnrpc.BakeMacaroonRequest{
		Permissions: []*lnrpc.MacaroonPermission{
			{
				Entity: "invalid123",
				Action: "read",
			},
		},
	}
	_, err = adminMacConnection.BakeMacaroon(ctxt, req)
	if err == nil || !errContains(err, "invalid permission entity") {
		t.Fatalf("expected an error, got %v", err)
	}

	// Fourth test: check that when no root key ID is specified, the default
	// root key ID is used.
	req = &lnrpc.BakeMacaroonRequest{
		Permissions: []*lnrpc.MacaroonPermission{
			{
				Entity: "macaroon",
				Action: "read",
			},
		},
	}
	_, err = adminMacConnection.BakeMacaroon(ctxt, req)
	require.NoError(t.t, err)

	listReq := &lnrpc.ListMacaroonIDsRequest{}
	resp, err := adminMacConnection.ListMacaroonIDs(ctxt, listReq)
	require.NoError(t.t, err)
	if resp.RootKeyIds[0] != 0 {
		t.Fatalf("expected ID to be 0, found: %v", resp.RootKeyIds)
	}

	// Fifth test: create a macaroon use a non-default root key ID.
	rootKeyID := uint64(4200)
	req = &lnrpc.BakeMacaroonRequest{
		RootKeyId: rootKeyID,
		Permissions: []*lnrpc.MacaroonPermission{
			{
				Entity: "macaroon",
				Action: "read",
			},
		},
	}
	bakeResp, err := adminMacConnection.BakeMacaroon(ctxt, req)
	require.NoError(t.t, err)

	listReq = &lnrpc.ListMacaroonIDsRequest{}
	resp, err = adminMacConnection.ListMacaroonIDs(ctxt, listReq)
	require.NoError(t.t, err)

	// the ListMacaroonIDs should give a list of two IDs, the default ID 0, and
	// the newly created ID. The returned response is sorted to guarantee the
	// order so that we can compare them one by one.
	sort.Slice(resp.RootKeyIds, func(i, j int) bool {
		return resp.RootKeyIds[i] < resp.RootKeyIds[j]
	})
	if resp.RootKeyIds[0] != 0 {
		t.Fatalf("expected ID to be %v, found: %v", 0, resp.RootKeyIds[0])
	}
	if resp.RootKeyIds[1] != rootKeyID {
		t.Fatalf(
			"expected ID to be %v, found: %v",
			rootKeyID, resp.RootKeyIds[1],
		)
	}

	// Sixth test: check the baked macaroon has the intended permissions. It
	// should succeed in reading, and fail to write a macaroon.
	newMac, err := readMacaroonFromHex(bakeResp.Macaroon)
	require.NoError(t.t, err)
	conn, err = testNode.ConnectRPCWithMacaroon(newMac)
	require.NoError(t.t, err)
	defer conn.Close()
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	newMacConnection := lnrpc.NewLightningClient(conn)

	// BakeMacaroon requires a write permission, so this call should return an
	// error.
	_, err = newMacConnection.BakeMacaroon(ctxt, req)
	if err == nil || !errContains(err, "permission denied") {
		t.Fatalf("expected an error, got %v", err)
	}

	// ListMacaroon requires a read permission, so this call should succeed.
	listReq = &lnrpc.ListMacaroonIDsRequest{}
	resp, err = newMacConnection.ListMacaroonIDs(ctxt, listReq)
	require.NoError(t.t, err)

	// Current macaroon can only work on entity macaroon, so a GetInfo request
	// will fail.
	infoReq := &lnrpc.GetInfoRequest{}
	_, err = newMacConnection.GetInfo(ctxt, infoReq)
	if err == nil || !errContains(err, "permission denied") {
		t.Fatalf("expected error not returned, got %v", err)
	}
}

// testDeleteMacaroonID checks that when deleting a macaroon ID, it removes the
// specified ID and invalidates all macaroons derived from the key with that ID.
// Also, it checks deleting the reserved marcaroon ID, DefaultRootKeyID or is
// forbidden.
func testDeleteMacaroonID(net *lntest.NetworkHarness, t *harnessTest) {
	var (
		ctxb     = context.Background()
		testNode = net.Alice
	)

	// Use admin macaroon to create a connection.
	adminMac, err := testNode.ReadMacaroon(
		testNode.AdminMacPath(), defaultTimeout,
	)
	require.NoError(t.t, err)
	conn, err := testNode.ConnectRPCWithMacaroon(adminMac)
	require.NoError(t.t, err)
	defer conn.Close()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	adminMacConnection := lnrpc.NewLightningClient(conn)

	// Record the number of macaroon IDs before creation.
	listReq := &lnrpc.ListMacaroonIDsRequest{}
	listResp, err := adminMacConnection.ListMacaroonIDs(ctxt, listReq)
	require.NoError(t.t, err)
	numMacIDs := len(listResp.RootKeyIds)

	// Create macaroons for testing.
	rootKeyIDs := []uint64{1, 2, 3}
	macList := make([]string, 0, len(rootKeyIDs))
	for _, id := range rootKeyIDs {
		req := &lnrpc.BakeMacaroonRequest{
			RootKeyId: id,
			Permissions: []*lnrpc.MacaroonPermission{{
				Entity: "macaroon",
				Action: "read",
			}},
		}
		resp, err := adminMacConnection.BakeMacaroon(ctxt, req)
		require.NoError(t.t, err)
		macList = append(macList, resp.Macaroon)
	}

	// Check that the creation is successful.
	listReq = &lnrpc.ListMacaroonIDsRequest{}
	listResp, err = adminMacConnection.ListMacaroonIDs(ctxt, listReq)
	require.NoError(t.t, err)

	// The number of macaroon IDs should be increased by len(rootKeyIDs).
	require.Equal(t.t, numMacIDs+len(rootKeyIDs), len(listResp.RootKeyIds))

	// First test: check deleting the DefaultRootKeyID returns an error.
	defaultID, _ := strconv.ParseUint(
		string(macaroons.DefaultRootKeyID), 10, 64,
	)
	req := &lnrpc.DeleteMacaroonIDRequest{
		RootKeyId: defaultID,
	}
	_, err = adminMacConnection.DeleteMacaroonID(ctxt, req)
	require.Error(t.t, err)
	require.Contains(
		t.t, err.Error(), macaroons.ErrDeletionForbidden.Error(),
	)

	// Second test: check deleting the customized ID returns success.
	req = &lnrpc.DeleteMacaroonIDRequest{
		RootKeyId: rootKeyIDs[0],
	}
	resp, err := adminMacConnection.DeleteMacaroonID(ctxt, req)
	require.NoError(t.t, err)
	require.True(t.t, resp.Deleted)

	// Check that the deletion is successful.
	listReq = &lnrpc.ListMacaroonIDsRequest{}
	listResp, err = adminMacConnection.ListMacaroonIDs(ctxt, listReq)
	require.NoError(t.t, err)

	// The number of macaroon IDs should be decreased by 1.
	require.Equal(t.t, numMacIDs+len(rootKeyIDs)-1, len(listResp.RootKeyIds))

	// Check that the deleted macaroon can no longer access macaroon:read.
	deletedMac, err := readMacaroonFromHex(macList[0])
	require.NoError(t.t, err)
	conn, err = testNode.ConnectRPCWithMacaroon(deletedMac)
	require.NoError(t.t, err)
	defer conn.Close()
	ctxt, cancel = context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()
	deletedMacConnection := lnrpc.NewLightningClient(conn)

	// Because the macaroon is deleted, it will be treated as an invalid one.
	listReq = &lnrpc.ListMacaroonIDsRequest{}
	_, err = deletedMacConnection.ListMacaroonIDs(ctxt, listReq)
	require.Error(t.t, err)
	require.Contains(t.t, err.Error(), "cannot get macaroon")
}

// readMacaroonFromHex loads a macaroon from a hex string.
func readMacaroonFromHex(macHex string) (*macaroon.Macaroon, error) {
	macBytes, err := hex.DecodeString(macHex)
	if err != nil {
		return nil, err
	}

	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, err
	}
	return mac, nil
}

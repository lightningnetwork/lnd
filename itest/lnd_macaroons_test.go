package itest

import (
	"context"
	"encoding/hex"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"gopkg.in/macaroon.v2"
)

// testMacaroonAuthentication makes sure that if macaroon authentication is
// enabled on the gRPC interface, no requests with missing or invalid
// macaroons are allowed. Further, the specific access rights (read/write,
// entity based) and first-party caveats are tested as well.
func testMacaroonAuthentication(ht *lntest.HarnessTest) {
	var (
		infoReq    = &lnrpc.GetInfoRequest{}
		newAddrReq = &lnrpc.NewAddressRequest{
			Type: AddrTypeWitnessPubkeyHash,
		}
		testNode   = ht.NewNode("Alice", nil)
		testClient = testNode.RPC.LN
	)

	testCases := []struct {
		name string
		run  func(ctxt context.Context, t *testing.T)
	}{{
		// Make sure we get an error if we use no macaroons but try to
		// connect to a node that has macaroon authentication enabled.
		name: "no macaroon",
		run: func(ctxt context.Context, t *testing.T) {
			conn, err := testNode.ConnectRPCWithMacaroon(nil)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			client := lnrpc.NewLightningClient(conn)
			_, err = client.GetInfo(ctxt, infoReq)
			require.Error(t, err)
			require.Contains(t, err.Error(), "expected 1 macaroon")
		},
	}, {
		// Ensure that an invalid macaroon also triggers an error.
		name: "invalid macaroon",
		run: func(ctxt context.Context, t *testing.T) {
			invalidMac, _ := macaroon.New(
				[]byte("dummy_root_key"), []byte("0"), "itest",
				macaroon.LatestVersion,
			)
			cleanup, client := macaroonClient(
				t, testNode, invalidMac,
			)
			defer cleanup()
			_, err := client.GetInfo(ctxt, infoReq)
			require.Error(t, err)
			require.Contains(t, err.Error(), "invalid ID")
		},
	}, {
		// Try to access a write method with read-only macaroon.
		name: "read only macaroon",
		run: func(ctxt context.Context, t *testing.T) {
			readonlyMac, err := testNode.ReadMacaroon(
				testNode.Cfg.ReadMacPath, defaultTimeout,
			)
			require.NoError(t, err)
			cleanup, client := macaroonClient(
				t, testNode, readonlyMac,
			)
			defer cleanup()
			_, err = client.NewAddress(ctxt, newAddrReq)
			require.Error(t, err)
			require.Contains(t, err.Error(), "permission denied")
		},
	}, {
		// Check first-party caveat with timeout that expired 30 seconds
		// ago.
		name: "expired macaroon",
		run: func(ctxt context.Context, t *testing.T) {
			readonlyMac, err := testNode.ReadMacaroon(
				testNode.Cfg.ReadMacPath, defaultTimeout,
			)
			require.NoError(t, err)
			timeoutMac, err := macaroons.AddConstraints(
				readonlyMac, macaroons.TimeoutConstraint(-30),
			)
			require.NoError(t, err)
			cleanup, client := macaroonClient(
				t, testNode, timeoutMac,
			)
			defer cleanup()
			_, err = client.GetInfo(ctxt, infoReq)
			require.Error(t, err)
			require.Contains(t, err.Error(), "macaroon has expired")
		},
	}, {
		// Check first-party caveat with invalid IP address.
		name: "invalid IP macaroon",
		run: func(ctxt context.Context, t *testing.T) {
			readonlyMac, err := testNode.ReadMacaroon(
				testNode.Cfg.ReadMacPath, defaultTimeout,
			)
			require.NoError(t, err)
			invalidIPAddrMac, err := macaroons.AddConstraints(
				readonlyMac, macaroons.IPLockConstraint(
					"1.1.1.1",
				),
			)
			require.NoError(t, err)
			cleanup, client := macaroonClient(
				t, testNode, invalidIPAddrMac,
			)
			defer cleanup()
			_, err = client.GetInfo(ctxt, infoReq)
			require.Error(t, err)
			require.Contains(t, err.Error(), "different IP address")
		},
	}, {
		// Make sure that if we do everything correct and
		// send the admin macaroon with first-party caveats that we can
		// satisfy, we get a correct answer.
		name: "correct macaroon",
		run: func(ctxt context.Context, t *testing.T) {
			adminMac, err := testNode.ReadMacaroon(
				testNode.Cfg.AdminMacPath, defaultTimeout,
			)
			require.NoError(t, err)
			adminMac, err = macaroons.AddConstraints(
				adminMac, macaroons.TimeoutConstraint(30),
				macaroons.IPLockConstraint("127.0.0.1"),
			)
			require.NoError(t, err)
			cleanup, client := macaroonClient(t, testNode, adminMac)
			defer cleanup()
			res, err := client.NewAddress(ctxt, newAddrReq)
			require.NoError(t, err, "get new address")
			assert.Contains(t, res.Address, "bcrt1")
		},
	}, {
		// Check first-party caveat with invalid IP range.
		name: "invalid IP range macaroon",
		run: func(ctxt context.Context, t *testing.T) {
			readonlyMac, err := testNode.ReadMacaroon(
				testNode.Cfg.ReadMacPath, defaultTimeout,
			)
			require.NoError(t, err)
			invalidIPRangeMac, err := macaroons.AddConstraints(
				readonlyMac, macaroons.IPRangeLockConstraint(
					"1.1.1.1/32",
				),
			)
			require.NoError(t, err)
			cleanup, client := macaroonClient(
				t, testNode, invalidIPRangeMac,
			)
			defer cleanup()
			_, err = client.GetInfo(ctxt, infoReq)
			require.Error(t, err)
			require.Contains(t, err.Error(), "different IP range")
		},
	}, {
		// Make sure that if we do everything correct and send the admin
		// macaroon with first-party caveats that we can satisfy, we get
		// a correct answer.
		name: "correct macaroon",
		run: func(ctxt context.Context, t *testing.T) {
			adminMac, err := testNode.ReadMacaroon(
				testNode.Cfg.AdminMacPath, defaultTimeout,
			)
			require.NoError(t, err)
			adminMac, err = macaroons.AddConstraints(
				adminMac, macaroons.TimeoutConstraint(30),
				macaroons.IPRangeLockConstraint("127.0.0.0/8"),
			)
			require.NoError(t, err)
			cleanup, client := macaroonClient(t, testNode, adminMac)
			defer cleanup()
			res, err := client.NewAddress(ctxt, newAddrReq)
			require.NoError(t, err, "get new address")
			assert.Contains(t, res.Address, "bcrt1")
		},
	}, {
		// Bake a macaroon that can only access exactly two RPCs and
		// make sure it works as expected.
		name: "custom URI permissions",
		run: func(ctxt context.Context, t *testing.T) {
			entity := macaroons.PermissionEntityCustomURI
			req := &lnrpc.BakeMacaroonRequest{
				Permissions: []*lnrpc.MacaroonPermission{{
					Entity: entity,
					Action: "/lnrpc.Lightning/GetInfo",
				}, {
					Entity: entity,
					Action: "/lnrpc.Lightning/List" +
						"Permissions",
				}},
			}
			bakeRes, err := testClient.BakeMacaroon(ctxt, req)
			require.NoError(t, err)

			// Create a connection that uses the custom macaroon.
			customMacBytes, err := hex.DecodeString(
				bakeRes.Macaroon,
			)
			require.NoError(t, err)
			customMac := &macaroon.Macaroon{}
			err = customMac.UnmarshalBinary(customMacBytes)
			require.NoError(t, err)
			cleanup, client := macaroonClient(
				t, testNode, customMac,
			)
			defer cleanup()

			// Call GetInfo which should succeed.
			_, err = client.GetInfo(ctxt, infoReq)
			require.NoError(t, err)

			// Call ListPermissions which should also succeed.
			permReq := &lnrpc.ListPermissionsRequest{}
			permRes, err := client.ListPermissions(ctxt, permReq)
			require.NoError(t, err)
			require.Greater(
				t, len(permRes.MethodPermissions), 10,
				"permissions",
			)

			// Try NewAddress which should be denied.
			_, err = client.NewAddress(ctxt, newAddrReq)
			require.Error(t, err)
			require.Contains(t, err.Error(), "permission denied")
		},
	}, {
		// Check that with the CheckMacaroonPermissions RPC, we can
		// check that a macaroon follows (or doesn't) permissions and
		// constraints.
		name: "unknown permissions",
		run: func(ctxt context.Context, t *testing.T) {
			// A test macaroon created with permissions from pool,
			// to make sure CheckMacaroonPermissions RPC accepts
			// them.
			rootKeyID := uint64(4200)
			req := &lnrpc.BakeMacaroonRequest{
				RootKeyId: rootKeyID,
				Permissions: []*lnrpc.MacaroonPermission{{
					Entity: "account",
					Action: "read",
				}, {
					Entity: "recommendation",
					Action: "read",
				}},
				AllowExternalPermissions: true,
			}
			bakeResp, err := testClient.BakeMacaroon(ctxt, req)
			require.NoError(t, err)

			macBytes, err := hex.DecodeString(bakeResp.Macaroon)
			require.NoError(t, err)

			checkReq := &lnrpc.CheckMacPermRequest{
				Macaroon:    macBytes,
				Permissions: req.Permissions,
			}

			// Test that CheckMacaroonPermissions accurately
			// characterizes macaroon as valid, even if the
			// permissions are not native to LND.
			checkResp, err := testClient.CheckMacaroonPermissions(
				ctxt, checkReq,
			)
			require.NoError(t, err)
			require.Equal(t, checkResp.Valid, true)

			mac, err := readMacaroonFromHex(bakeResp.Macaroon)
			require.NoError(t, err)

			// Test that CheckMacaroonPermissions responds that the
			// macaroon is invalid if timed out.
			timeoutMac, err := macaroons.AddConstraints(
				mac, macaroons.TimeoutConstraint(-30),
			)
			require.NoError(t, err)

			timeoutMacBytes, err := timeoutMac.MarshalBinary()
			require.NoError(t, err)

			checkReq.Macaroon = timeoutMacBytes

			_, err = testClient.CheckMacaroonPermissions(
				ctxt, checkReq,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), "macaroon has expired")

			// Test that CheckMacaroonPermissions labels macaroon
			// input with wrong permissions as invalid.
			wrongPermissions := []*lnrpc.MacaroonPermission{{
				Entity: "invoice",
				Action: "read",
			}}

			checkReq.Permissions = wrongPermissions
			checkReq.Macaroon = macBytes

			_, err = testClient.CheckMacaroonPermissions(
				ctxt, checkReq,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), "permission denied")
		},
	}, {
		// Check that with the CheckMacaroonPermissions RPC, we can
		// check that a macaroon follows the permissions of a given
		// method.
		name: "default permissions from full method",
		run: func(ctxt context.Context, t *testing.T) {
			// We test that the macaroon of the test client has
			// all the permissions for calling the BakeMacaroon RPC.
			mac, err := testNode.ReadMacaroon(
				testNode.Cfg.AdminMacPath, wait.DefaultTimeout,
			)
			require.NoError(t, err)

			macBytes, err := mac.MarshalBinary()
			require.NoError(t, err)

			rpcURI := "/lnrpc.Lightning/BakeMacaroon"
			checkReq := &lnrpc.CheckMacPermRequest{
				Macaroon:                        macBytes,
				FullMethod:                      rpcURI,
				CheckDefaultPermsFromFullMethod: true,
			}

			// Test that CheckMacaroonPermissions accurately
			// characterizes macaroon as valid, since the admin
			// macaroon should have all the permissions.
			checkResp, err := testClient.CheckMacaroonPermissions(
				ctxt, checkReq,
			)
			require.NoError(t, err)
			require.Equal(t, checkResp.Valid, true)

			// Check different error cases.
			dummy := []*lnrpc.MacaroonPermission{{
				Entity: "foo",
			}}
			_, err = testClient.CheckMacaroonPermissions(
				ctxt, &lnrpc.CheckMacPermRequest{
					Permissions:                     dummy,
					CheckDefaultPermsFromFullMethod: true,
				},
			)
			require.ErrorContains(
				t, err, "cannot check default permissions "+
					"from full method and from provided "+
					"permission list at the same time",
			)

			_, err = testClient.CheckMacaroonPermissions(
				ctxt, &lnrpc.CheckMacPermRequest{
					FullMethod:                      "",
					CheckDefaultPermsFromFullMethod: true,
				},
			)
			require.ErrorContains(
				t, err, "cannot check default permissions "+
					"from full method without providing "+
					"the full method name",
			)

			_, err = testClient.CheckMacaroonPermissions(
				ctxt, &lnrpc.CheckMacPermRequest{
					FullMethod:                      "baz",
					CheckDefaultPermsFromFullMethod: true,
				},
			)
			require.ErrorContains(
				t, err, "no permissions found for full method "+
					"baz",
			)
		},
	}}

	for _, tc := range testCases {
		tc := tc
		ht.Run(tc.name, func(tt *testing.T) {
			ctxt, cancel := context.WithTimeout(
				ht.Context(), defaultTimeout,
			)
			defer cancel()

			tc.run(ctxt, tt)
		})
	}
}

// testBakeMacaroon checks that when creating macaroons, the permissions param
// in the request must be set correctly, and the baked macaroon has the intended
// permissions.
func testBakeMacaroon(ht *lntest.HarnessTest) {
	var testNode = ht.NewNode("Alice", nil)

	testCases := []struct {
		name string
		run  func(ctxt context.Context, t *testing.T,
			adminClient lnrpc.LightningClient)
	}{{
		// First test: when the permission list is empty in the request,
		// an error should be returned.
		name: "no permission list",
		run: func(ctxt context.Context, t *testing.T,
			adminClient lnrpc.LightningClient) {

			req := &lnrpc.BakeMacaroonRequest{}
			_, err := adminClient.BakeMacaroon(ctxt, req)
			require.Error(t, err)
			assert.Contains(
				t, err.Error(), "permission list cannot be "+
					"empty",
			)
		},
	}, {
		// Second test: when the action in the permission list is not
		// valid, an error should be returned.
		name: "invalid permission list",
		run: func(ctxt context.Context, t *testing.T,
			adminClient lnrpc.LightningClient) {

			req := &lnrpc.BakeMacaroonRequest{
				Permissions: []*lnrpc.MacaroonPermission{{
					Entity: "macaroon",
					Action: "invalid123",
				}},
			}
			_, err := adminClient.BakeMacaroon(ctxt, req)
			require.Error(t, err)
			assert.Contains(
				t, err.Error(), "invalid permission action",
			)
		},
	}, {
		// Third test: when the entity in the permission list is not
		// valid, an error should be returned.
		name: "invalid permission entity",
		run: func(ctxt context.Context, t *testing.T,
			adminClient lnrpc.LightningClient) {

			req := &lnrpc.BakeMacaroonRequest{
				Permissions: []*lnrpc.MacaroonPermission{{
					Entity: "invalid123",
					Action: "read",
				}},
			}
			_, err := adminClient.BakeMacaroon(ctxt, req)
			require.Error(t, err)
			assert.Contains(
				t, err.Error(), "invalid permission entity",
			)
		},
	}, {
		// Fourth test: check that when no root key ID is specified, the
		// default root keyID is used.
		name: "default root key ID",
		run: func(ctxt context.Context, t *testing.T,
			adminClient lnrpc.LightningClient) {

			req := &lnrpc.BakeMacaroonRequest{
				Permissions: []*lnrpc.MacaroonPermission{{
					Entity: "macaroon",
					Action: "read",
				}},
			}
			_, err := adminClient.BakeMacaroon(ctxt, req)
			require.NoError(t, err)

			listReq := &lnrpc.ListMacaroonIDsRequest{}
			resp, err := adminClient.ListMacaroonIDs(ctxt, listReq)
			require.NoError(t, err)
			require.Equal(t, resp.RootKeyIds[0], uint64(0))
		},
	}, {
		// Fifth test: create a macaroon use a non-default root key ID.
		name: "custom root key ID",
		run: func(ctxt context.Context, t *testing.T,
			adminClient lnrpc.LightningClient) {

			rootKeyID := uint64(4200)
			req := &lnrpc.BakeMacaroonRequest{
				RootKeyId: rootKeyID,
				Permissions: []*lnrpc.MacaroonPermission{{
					Entity: "macaroon",
					Action: "read",
				}},
			}
			_, err := adminClient.BakeMacaroon(ctxt, req)
			require.NoError(t, err)

			listReq := &lnrpc.ListMacaroonIDsRequest{}
			resp, err := adminClient.ListMacaroonIDs(ctxt, listReq)
			require.NoError(t, err)

			// the ListMacaroonIDs should give a list of two IDs,
			// the default ID 0, and the newly created ID. The
			// returned response is sorted to guarantee the order so
			// that we can compare them one by one.
			sort.Slice(resp.RootKeyIds, func(i, j int) bool {
				return resp.RootKeyIds[i] < resp.RootKeyIds[j]
			})
			require.Equal(t, resp.RootKeyIds[0], uint64(0))
			require.Equal(t, resp.RootKeyIds[1], rootKeyID)
		},
	}, {
		// Sixth test: check the baked macaroon has the intended
		// permissions. It should succeed in reading, and fail to write
		// a macaroon.
		name: "custom macaroon permissions",
		run: func(ctxt context.Context, t *testing.T,
			adminClient lnrpc.LightningClient) {

			rootKeyID := uint64(4200)
			req := &lnrpc.BakeMacaroonRequest{
				RootKeyId: rootKeyID,
				Permissions: []*lnrpc.MacaroonPermission{{
					Entity: "macaroon",
					Action: "read",
				}},
			}
			bakeResp, err := adminClient.BakeMacaroon(ctxt, req)
			require.NoError(t, err)

			newMac, err := readMacaroonFromHex(bakeResp.Macaroon)
			require.NoError(t, err)
			cleanup, readOnlyClient := macaroonClient(
				t, testNode, newMac,
			)
			defer cleanup()

			// BakeMacaroon requires a write permission, so this
			// call should return an error.
			_, err = readOnlyClient.BakeMacaroon(ctxt, req)
			require.Error(t, err)
			require.Contains(t, err.Error(), "permission denied")

			// ListMacaroon requires a read permission, so this call
			// should succeed.
			listReq := &lnrpc.ListMacaroonIDsRequest{}
			_, err = readOnlyClient.ListMacaroonIDs(ctxt, listReq)
			require.NoError(t, err)

			// Current macaroon can only work on entity macaroon, so
			// a GetInfo request will fail.
			infoReq := &lnrpc.GetInfoRequest{}
			_, err = readOnlyClient.GetInfo(ctxt, infoReq)
			require.Error(t, err)
			require.Contains(t, err.Error(), "permission denied")
		},
	}, {
		// Seventh test: check that if the allow_external_permissions
		// flag is set, we are able to feed BakeMacaroons permissions
		// that LND is not familiar with.
		name: "allow external macaroon permissions",
		run: func(ctxt context.Context, t *testing.T,
			adminClient lnrpc.LightningClient) {

			// We'll try permissions from Pool to test that the
			// allow_external_permissions flag properly allows it.
			rootKeyID := uint64(4200)
			req := &lnrpc.BakeMacaroonRequest{
				RootKeyId: rootKeyID,
				Permissions: []*lnrpc.MacaroonPermission{{
					Entity: "account",
					Action: "read",
				}},
				AllowExternalPermissions: true,
			}

			resp, err := adminClient.BakeMacaroon(ctxt, req)
			require.NoError(t, err)

			// We'll also check that the external permission was
			// successfully added to the macaroon.
			macBytes, err := hex.DecodeString(resp.Macaroon)
			require.NoError(t, err)

			mac := &macaroon.Macaroon{}
			err = mac.UnmarshalBinary(macBytes)
			require.NoError(t, err)

			rawID := mac.Id()
			decodedID := &lnrpc.MacaroonId{}
			idProto := rawID[1:]
			err = proto.Unmarshal(idProto, decodedID)
			require.NoError(t, err)

			require.Equal(t, "account", decodedID.Ops[0].Entity)
			require.Equal(t, "read", decodedID.Ops[0].Actions[0])
		},
	}}

	for _, tc := range testCases {
		tc := tc
		ht.Run(tc.name, func(tt *testing.T) {
			ctxt, cancel := context.WithTimeout(
				ht.Context(), defaultTimeout,
			)
			defer cancel()

			adminMac, err := testNode.ReadMacaroon(
				testNode.Cfg.AdminMacPath, defaultTimeout,
			)
			require.NoError(tt, err)
			cleanup, client := macaroonClient(tt, testNode, adminMac)
			defer cleanup()

			tc.run(ctxt, tt, client)
		})
	}
}

// testDeleteMacaroonID checks that when deleting a macaroon ID, it removes the
// specified ID and invalidates all macaroons derived from the key with that ID.
// Also, it checks deleting the reserved marcaroon ID, DefaultRootKeyID or is
// forbidden.
func testDeleteMacaroonID(ht *lntest.HarnessTest) {
	var (
		ctxb     = ht.Context()
		testNode = ht.NewNode("Alice", nil)
	)
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Use admin macaroon to create a connection.
	adminMac, err := testNode.ReadMacaroon(
		testNode.Cfg.AdminMacPath, defaultTimeout,
	)
	require.NoError(ht, err)
	cleanup, client := macaroonClient(ht.T, testNode, adminMac)
	defer cleanup()

	// Record the number of macaroon IDs before creation.
	listReq := &lnrpc.ListMacaroonIDsRequest{}
	listResp, err := client.ListMacaroonIDs(ctxt, listReq)
	require.NoError(ht, err)
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
		resp, err := client.BakeMacaroon(ctxt, req)
		require.NoError(ht, err)
		macList = append(macList, resp.Macaroon)
	}

	// Check that the creation is successful.
	listReq = &lnrpc.ListMacaroonIDsRequest{}
	listResp, err = client.ListMacaroonIDs(ctxt, listReq)
	require.NoError(ht, err)

	// The number of macaroon IDs should be increased by len(rootKeyIDs).
	require.Equal(ht, numMacIDs+len(rootKeyIDs), len(listResp.RootKeyIds))

	// First test: check deleting the DefaultRootKeyID returns an error.
	defaultID, _ := strconv.ParseUint(
		string(macaroons.DefaultRootKeyID), 10, 64,
	)
	req := &lnrpc.DeleteMacaroonIDRequest{
		RootKeyId: defaultID,
	}
	_, err = client.DeleteMacaroonID(ctxt, req)
	require.Error(ht, err)
	require.Contains(ht, err.Error(),
		macaroons.ErrDeletionForbidden.Error())

	// Second test: check deleting the customized ID returns success.
	req = &lnrpc.DeleteMacaroonIDRequest{
		RootKeyId: rootKeyIDs[0],
	}
	resp, err := client.DeleteMacaroonID(ctxt, req)
	require.NoError(ht, err)
	require.True(ht, resp.Deleted)

	// Check that the deletion is successful.
	listReq = &lnrpc.ListMacaroonIDsRequest{}
	listResp, err = client.ListMacaroonIDs(ctxt, listReq)
	require.NoError(ht, err)

	// The number of macaroon IDs should be decreased by 1.
	require.Equal(ht, numMacIDs+len(rootKeyIDs)-1, len(listResp.RootKeyIds))

	// Check that the deleted macaroon can no longer access macaroon:read.
	deletedMac, err := readMacaroonFromHex(macList[0])
	require.NoError(ht, err)
	cleanup, client = macaroonClient(ht.T, testNode, deletedMac)
	defer cleanup()

	// Because the macaroon is deleted, it will be treated as an invalid
	// one.
	listReq = &lnrpc.ListMacaroonIDsRequest{}
	_, err = client.ListMacaroonIDs(ctxt, listReq)
	require.Error(ht, err)
	require.Contains(ht, err.Error(), "cannot get macaroon")
}

// testStatelessInit checks that the stateless initialization of the daemon
// does not write any macaroon files to the daemon's file system and returns
// the admin macaroon in the response. It then checks that the password
// change of the wallet can also happen stateless.
func testStatelessInit(ht *lntest.HarnessTest) {
	var (
		initPw     = []byte("stateless")
		newPw      = []byte("stateless-new")
		newAddrReq = &lnrpc.NewAddressRequest{
			Type: AddrTypeWitnessPubkeyHash,
		}
	)

	// First, create a new node and request it to initialize stateless.
	// This should return us the binary serialized admin macaroon that we
	// can then use for further calls.
	carol, _, macBytes := ht.NewNodeWithSeed("Carol", nil, initPw, true)
	require.NotEmpty(ht, macBytes,
		"invalid macaroon returned in stateless init")

	// Now make sure no macaroon files have been created by the node Carol.
	_, err := os.Stat(carol.Cfg.AdminMacPath)
	require.Error(ht, err)
	_, err = os.Stat(carol.Cfg.ReadMacPath)
	require.Error(ht, err)
	_, err = os.Stat(carol.Cfg.InvoiceMacPath)
	require.Error(ht, err)

	// Then check that we can unmarshal the binary serialized macaroon.
	adminMac := &macaroon.Macaroon{}
	err = adminMac.UnmarshalBinary(macBytes)
	require.NoError(ht, err)

	// Find out if we can actually use the macaroon that has been returned
	// to us for a RPC call.
	conn, err := carol.ConnectRPCWithMacaroon(adminMac)
	require.NoError(ht, err)
	defer conn.Close()
	adminMacClient := lnrpc.NewLightningClient(conn)
	ctxt, cancel := context.WithTimeout(ht.Context(), defaultTimeout)
	defer cancel()
	res, err := adminMacClient.NewAddress(ctxt, newAddrReq)
	require.NoError(ht, err)
	if !strings.HasPrefix(res.Address, harnessNetParams.Bech32HRPSegwit) {
		require.Fail(ht, "returned address was not a regtest address")
	}

	// As a second part, shut down the node and then try to change the
	// password when we start it up again.
	ht.RestartNodeNoUnlock(carol)
	changePwReq := &lnrpc.ChangePasswordRequest{
		CurrentPassword: initPw,
		NewPassword:     newPw,
		StatelessInit:   true,
	}
	response, err := carol.ChangePasswordAndInit(changePwReq)
	require.NoError(ht, err)

	// Again, make  sure no macaroon files have been created by the node
	// Carol.
	_, err = os.Stat(carol.Cfg.AdminMacPath)
	require.Error(ht, err)
	_, err = os.Stat(carol.Cfg.ReadMacPath)
	require.Error(ht, err)
	_, err = os.Stat(carol.Cfg.InvoiceMacPath)
	require.Error(ht, err)

	// Then check that we can unmarshal the new binary serialized macaroon
	// and that it really is a new macaroon.
	err = adminMac.UnmarshalBinary(response.AdminMacaroon)
	require.NoError(ht, err, "unable to unmarshal macaroon")
	require.NotEqual(ht, response.AdminMacaroon, macBytes,
		"expected new macaroon to be different")

	// Finally, find out if we can actually use the new macaroon that has
	// been returned to us for a RPC call.
	conn2, err := carol.ConnectRPCWithMacaroon(adminMac)
	require.NoError(ht, err)
	defer conn2.Close()
	adminMacClient = lnrpc.NewLightningClient(conn2)

	// Changing the password takes a while, so we use the default timeout
	// of 30 seconds to wait for the connection to be ready.
	ctxt, cancel = context.WithTimeout(ht.Context(), defaultTimeout)
	defer cancel()
	res, err = adminMacClient.NewAddress(ctxt, newAddrReq)
	require.NoError(ht, err)
	if !strings.HasPrefix(res.Address, harnessNetParams.Bech32HRPSegwit) {
		require.Fail(ht, "returned address was not a regtest address")
	}
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

func macaroonClient(t *testing.T, testNode *node.HarnessNode,
	mac *macaroon.Macaroon) (func(), lnrpc.LightningClient) {

	t.Helper()

	conn, err := testNode.ConnectRPCWithMacaroon(mac)
	require.NoError(t, err, "connect to alice")

	cleanup := func() {
		err := conn.Close()
		require.NoError(t, err, "close")
	}
	return cleanup, lnrpc.NewLightningClient(conn)
}

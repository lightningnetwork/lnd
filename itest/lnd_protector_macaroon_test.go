package itest

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
)

// channelMgmtProfile is the name of the channel management protector profile
// exercised by this test.
const channelMgmtProfile = "channel-management-v1"

// testProtectorMacaroon tests the protector macaroon caveat end to end: a
// macaroon constrained with the channel-management-v1 protector profile must
// be able to open, close and update the policy of channels, but must not be
// able to set any of the request fields that could redirect channel funds to a
// third party (push_sat, close_address, funding_shim, delivery_address).
func testProtectorMacaroon(ht *lntest.HarnessTest) {
	alice := ht.NewNodeWithCoins("Alice", nil)
	bob := ht.NewNodeWithCoins("Bob", nil)
	ht.ConnectNodes(alice, bob)

	ctxb := context.Background()
	ctxt, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Derive a protected macaroon by attaching the protector caveat to
	// the admin macaroon. This is the offline attenuation path any
	// macaroon holder can use (lncli constrainmacaroon --protector).
	adminMac, err := alice.ReadMacaroon(
		alice.Cfg.AdminMacPath, defaultTimeout,
	)
	require.NoError(ht, err)

	protectedMac, err := macaroons.AddConstraints(
		adminMac, macaroons.ProtectorConstraint(channelMgmtProfile),
	)
	require.NoError(ht, err)

	cleanup, protClient := macaroonClient(ht.T, alice, protectedMac)
	defer cleanup()

	cleanupAdmin, adminClient := macaroonClient(ht.T, alice, adminMac)
	defer cleanupAdmin()

	// A valid compressed pubkey (the secp256k1 generator point) that no
	// known peer uses. Requests to it pass the interceptor but fail in
	// the handler with a "peer is not online" style error, which lets us
	// tell interceptor rejections and handler rejections apart.
	unknownPeer := []byte{
		0x02,
		0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb, 0xac,
		0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b, 0x07,
		0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28, 0xd9,
		0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17, 0x98,
	}

	// Part 1: the deny matrix. Every value redirection field must be
	// rejected by the interceptor chain, on both unary and streaming
	// methods, before it ever reaches a handler.
	_, err = protClient.OpenChannelSync(ctxt, &lnrpc.OpenChannelRequest{
		NodePubkey:         bob.PubKey[:],
		LocalFundingAmount: 1_000_000,
		PushSat:            10_000,
	})
	require.ErrorContains(ht, err, "push_sat")

	_, err = protClient.OpenChannelSync(ctxt, &lnrpc.OpenChannelRequest{
		NodePubkey:         bob.PubKey[:],
		LocalFundingAmount: 1_000_000,
		CloseAddress:       "bcrt1qattacker",
	})
	require.ErrorContains(ht, err, "close_address")

	_, err = protClient.OpenChannelSync(ctxt, &lnrpc.OpenChannelRequest{
		NodePubkey:         bob.PubKey[:],
		LocalFundingAmount: 1_000_000,
		FundingShim:        &lnrpc.FundingShim{},
	})
	require.ErrorContains(ht, err, "funding_shim")

	_, err = protClient.BatchOpenChannel(
		ctxt, &lnrpc.BatchOpenChannelRequest{
			Channels: []*lnrpc.BatchOpenChannel{{
				NodePubkey:         bob.PubKey[:],
				LocalFundingAmount: 500_000,
			}, {
				NodePubkey:         bob.PubKey[:],
				LocalFundingAmount: 500_000,
				PushSat:            10_000,
			}},
		},
	)
	require.ErrorContains(ht, err, "push_sat")

	// The streaming OpenChannel RPC must be covered as well; its request
	// message only becomes available to the server after stream open, so
	// this exercises the per-message enforcement path.
	openStream, err := protClient.OpenChannel(
		ctxt, &lnrpc.OpenChannelRequest{
			NodePubkey:         bob.PubKey[:],
			LocalFundingAmount: 1_000_000,
			PushSat:            10_000,
		},
	)
	require.NoError(ht, err)
	_, err = openStream.Recv()
	require.ErrorContains(ht, err, "push_sat")

	// CloseChannel (also streaming) with a delivery address must be
	// rejected by the interceptor even for a bogus channel point, since
	// enforcement runs before the handler.
	bogusChanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: make([]byte, 32),
		},
	}
	closeStream, err := protClient.CloseChannel(
		ctxt, &lnrpc.CloseChannelRequest{
			ChannelPoint:    bogusChanPoint,
			DeliveryAddress: "bcrt1qattacker",
		},
	)
	require.NoError(ht, err)
	_, err = closeStream.Recv()
	require.ErrorContains(ht, err, "delivery_address")

	// Part 2: control. The exact same denied request must pass the
	// interceptor with the unconstrained admin macaroon, proving the
	// restriction comes from the caveat, not from the endpoint. We use an
	// unknown peer so the request dies in the handler instead of opening
	// a real channel.
	pushReq := &lnrpc.OpenChannelRequest{
		NodePubkey:         unknownPeer,
		LocalFundingAmount: 1_000_000,
		PushSat:            10_000,
	}
	_, err = adminClient.OpenChannelSync(ctxt, pushReq)
	require.Error(ht, err)
	require.NotContains(ht, err.Error(), "push_sat")

	_, err = protClient.OpenChannelSync(ctxt, pushReq)
	require.ErrorContains(ht, err, "push_sat")

	// Part 3: a macaroon referencing a protector profile unknown to this
	// lnd must be rejected as a whole, even on methods the profile
	// wouldn't cover.
	futureMac, err := macaroons.AddConstraints(
		adminMac, macaroons.ProtectorConstraint("future-profile-v9"),
	)
	require.NoError(ht, err)

	cleanupFuture, futureClient := macaroonClient(ht.T, alice, futureMac)
	defer cleanupFuture()

	_, err = futureClient.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	require.ErrorContains(ht, err, "unknown protector profile")

	// Part 4: the positive path. The protected macaroon must be able to
	// run the full channel lifecycle as long as no denied field is set.
	//
	// Uncovered methods pass through unaffected.
	_, err = protClient.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	require.NoError(ht, err)

	// Open a channel to Bob with a clean request.
	chanPoint, err := protClient.OpenChannelSync(
		ctxt, &lnrpc.OpenChannelRequest{
			NodePubkey:         bob.PubKey[:],
			LocalFundingAmount: 1_000_000,
		},
	)
	require.NoError(ht, err)

	// Confirm the funding transaction and wait for the channel to become
	// active. We also wait for the channel to show up in Alice's graph,
	// since the policy update below can only succeed once the edge policy
	// exists.
	ht.MineBlocksAndAssertNumTxes(6, 1)
	ht.AssertNodeNumChannels(alice, 1)
	ht.AssertChannelInGraph(alice, chanPoint)

	// Update the channel policy through the protected macaroon.
	policyCtxt, policyCancel := context.WithTimeout(ctxb, defaultTimeout)
	defer policyCancel()
	policyResp, err := protClient.UpdateChannelPolicy(
		policyCtxt, &lnrpc.PolicyUpdateRequest{
			Scope: &lnrpc.PolicyUpdateRequest_Global{
				Global: true,
			},
			BaseFeeMsat:   1_100,
			FeeRatePpm:    550,
			TimeLockDelta: 80,
		},
	)
	require.NoError(ht, err)
	require.Empty(ht, policyResp.FailedUpdates)

	// Cooperatively close the channel through the protected macaroon,
	// without a delivery address.
	closeCtxt, closeCancel := context.WithTimeout(ctxb, defaultTimeout)
	defer closeCancel()
	closeStream, err = protClient.CloseChannel(
		closeCtxt, &lnrpc.CloseChannelRequest{
			ChannelPoint: chanPoint,
		},
	)
	require.NoError(ht, err)

	// The first update signals the close is pending in the mempool.
	closeUpdate, err := closeStream.Recv()
	require.NoError(ht, err)
	require.NotNil(ht, closeUpdate.GetClosePending())

	// Confirm the close transaction and make sure the channel is gone.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.AssertNodeNumChannels(alice, 0)

	// Part 5: interplay with the RPC middleware. A registered middleware
	// runs before protector enforcement and is allowed to replace the
	// request, so the field rules must be enforced against the replaced
	// request the handler would actually execute. We register a real
	// middleware that rewrites a clean open request into one carrying
	// push_sat and assert the rewritten request is rejected.
	ht.RestartNodeWithExtraArgs(alice, []string{"--rpcmiddleware.enable"})

	mw := registerMiddleware(
		ht.T, alice, &lnrpc.MiddlewareRegistration{
			MiddlewareName:           "protector-itest",
			CustomMacaroonCaveatName: "itest-caveat",
		}, true,
	)
	defer mw.cancel()

	// The macaroon carries both the middleware's custom caveat (so the
	// middleware intercepts the request) and the protector caveat.
	mwMac, err := macaroons.AddConstraints(
		adminMac,
		macaroons.CustomConstraint("itest-caveat", "itest-value"),
		macaroons.ProtectorConstraint(channelMgmtProfile),
	)
	require.NoError(ht, err)

	cleanupMw, mwClient := macaroonClient(ht.T, alice, mwMac)
	defer cleanupMw()

	cleanOpenReq := &lnrpc.OpenChannelRequest{
		NodePubkey:         unknownPeer,
		LocalFundingAmount: 1_000_000,
	}
	rewrittenReq := &lnrpc.OpenChannelRequest{
		NodePubkey:         unknownPeer,
		LocalFundingAmount: 1_000_000,
		PushSat:            10_000,
	}

	go mw.interceptUnary(
		"/lnrpc.Lightning/OpenChannelSync", cleanOpenReq,
		rewrittenReq, false, true, nil,
	)

	mwCtxt, mwCancel := context.WithTimeout(ctxb, defaultTimeout)
	defer mwCancel()
	_, err = mwClient.OpenChannelSync(mwCtxt, cleanOpenReq)
	require.ErrorContains(ht, err, "push_sat")

	// The middleware sees the erroring response for its rewritten
	// request, which proves enforcement ran after the replacement.
	mwResp := <-mw.responsesChan
	require.True(ht, mwResp.IsError)
	require.Contains(ht, string(mwResp.Serialized), "push_sat")
}

package itest

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/neutrinorpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testNeutrinoBlock checks that the neutrino sub-server is active and can
// fetch blocks and block headers.
func testNeutrinoGetBlock(net *lntest.NetworkHarness, t *harnessTest) {
	if net.BackendCfg.Name() != lntest.NeutrinoBackendName {
		return
	}
	ctxt, cancel := context.WithTimeout(
		context.Background(), defaultTimeout,
	)
	defer cancel()

	resStatus, err := net.Alice.NeutrinoClient.Status(
		ctxt, &neutrinorpc.StatusRequest{},
	)
	require.NoError(t.t, err, "failed to fetch neutrino status")
	require.True(t.t, resStatus.Active)

	reqGetBlock := &neutrinorpc.GetBlockRequest{Hash: resStatus.BlockHash}
	resGetBlock, err := net.Alice.NeutrinoClient.GetBlock(ctxt, reqGetBlock)
	require.NoError(t.t, err, "failed to fetch block from neutrino")
	if resStatus.BlockHash != resGetBlock.Hash {
		t.Fatalf("did not recive the correct block")
	}

	reqGetBlockHeader := &neutrinorpc.GetBlockHeaderRequest{
		Hash: resStatus.BlockHash,
	}
	resGetBlockHeader, err := net.Alice.NeutrinoClient.GetBlockHeader(
		ctxt, reqGetBlockHeader,
	)
	require.NoError(t.t, err, "failed to fetch block header from neutrino")
	if resStatus.BlockHash != resGetBlockHeader.Hash {
		t.Fatalf("did not recive the correct block header")
	}
}

// testNeutrinoTest checks that the neutrino sub-server is active.
func testNeutrinoGetCFilter(net *lntest.NetworkHarness, t *harnessTest) {
	if net.BackendCfg.Name() != lntest.NeutrinoBackendName {
		return
	}
	ctxt, cancel := context.WithTimeout(
		context.Background(), defaultTimeout,
	)
	defer cancel()

	resStatus, err := net.Alice.NeutrinoClient.Status(
		ctxt, &neutrinorpc.StatusRequest{},
	)
	require.NoError(t.t, err, "failed to fetch neutrino status")
	require.True(t.t, resStatus.Active)

	req := &neutrinorpc.GetCFilterRequest{Hash: resStatus.BlockHash}

	_, err = net.Alice.NeutrinoClient.GetCFilter(ctxt, req)
	require.NoError(t.t, err, "failed to fetch a compact filer from "+
		"neutrino")
}

// testNeutrinoConnectPeer checks that the neutrino sub-server is active.
func testNeutrinoConnectPeer(net *lntest.NetworkHarness, t *harnessTest) {
	if net.BackendCfg.Name() != lntest.NeutrinoBackendName {
		return
	}
	ctxt, cancel := context.WithTimeout(
		context.Background(), defaultTimeout,
	)
	defer cancel()

	resStatus, err := net.Alice.NeutrinoClient.Status(
		ctxt, &neutrinorpc.StatusRequest{},
	)
	require.NoError(t.t, err, "failed to fetch neutrino status")
	require.True(t.t, resStatus.Active)

	req := &neutrinorpc.AddPeerRequest{PeerAddrs {Hash: resStatus.BlockHash}

	_, err = net.Alice.NeutrinoClient.AddPeer(ctxt,).GetCFilter(ctxt, req)
	require.NoError(t.t, err, "failed to fetch a compact filer from "+
		"neutrino")
}

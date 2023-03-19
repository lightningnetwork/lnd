package rpc

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/stretchr/testify/require"
)

// =====================
// PeerClient related RPCs.
// =====================

type (
	AnnReq  *peersrpc.NodeAnnouncementUpdateRequest
	AnnResp *peersrpc.NodeAnnouncementUpdateResponse
)

// UpdateNodeAnnouncement makes an UpdateNodeAnnouncement RPC call the peersrpc
// client and asserts.
func (h *HarnessRPC) UpdateNodeAnnouncement(req AnnReq) AnnResp {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	resp, err := h.Peer.UpdateNodeAnnouncement(ctxt, req)
	h.NoError(err, "UpdateNodeAnnouncement")

	return resp
}

// UpdateNodeAnnouncementErr makes an UpdateNodeAnnouncement RPC call the
// peersrpc client and asserts an error is returned.
func (h *HarnessRPC) UpdateNodeAnnouncementErr(req AnnReq) {
	ctxt, cancel := context.WithTimeout(h.runCtx, DefaultTimeout)
	defer cancel()

	_, err := h.Peer.UpdateNodeAnnouncement(ctxt, req)
	require.Error(h, err, "expect an error from update announcement")
}

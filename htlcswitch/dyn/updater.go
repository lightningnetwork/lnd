package dyn

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// UpdateLinkStatus is used in the `UpdateLinkResponse` to indicate the current
// status of a given update request.
type UpdateLinkStatus uint8

const (
	// UpdateLinkStatusInitialized is the status the update flow is in when
	// the request is received by the Updater.
	UpdateLinkStatusInitialized UpdateLinkStatus = iota

	// UpdateLinkStatusPending defines the status when the request is being
	// processed by the Updater.
	UpdateLinkStatusPending

	// UpdateLinkStatusSucceeded defines the status when the update finished
	// successfully.
	UpdateLinkStatusSucceeded

	// UpdateLinkStatusFailed defines the status when the update failed.
	UpdateLinkStatusFailed
)

// String returns a human readable string that represents the status.
func (u UpdateLinkStatus) String() string {
	switch u {
	case UpdateLinkStatusInitialized:
		return "Initialized"

	case UpdateLinkStatusPending:
		return "Pending"

	case UpdateLinkStatusSucceeded:
		return "Succeeded"

	case UpdateLinkStatusFailed:
		return "Failed"

	default:
		return "Unknown"
	}
}

// UpdateLinkRequest defines a request that contains the channel params to be
// changed.
type UpdateLinkRequest struct {
	// ChanID identifies the channel link to be updated.
	ChanID lnwire.ChannelID

	// DustLimit is the threshold (in satoshis) below which any outputs
	// should be trimmed. When an output is trimmed, it isn't materialized
	// as an actual output, but is instead burned to miner's fees.
	DustLimit fn.Option[btcutil.Amount]

	// MaxPendingAmount is the maximum pending HTLC value that the
	// owner of these constraints can offer the remote node at a
	// particular time.
	MaxPendingAmount fn.Option[lnwire.MilliSatoshi]

	// ChanReserve is an absolute reservation on the channel for the
	// owner of this set of constraints. This means that the current
	// settled balance for this node CANNOT dip below the reservation
	// amount. This acts as a defense against costless attacks when
	// either side no longer has any skin in the game.
	ChanReserve fn.Option[btcutil.Amount]

	// MinHTLC is the minimum HTLC value that the owner of these
	// constraints can offer the remote node. If any HTLCs below this
	// amount are offered, then the HTLC will be rejected. This, in
	// tandem with the dust limit allows a node to regulate the
	// smallest HTLC that it deems economically relevant.
	MinHTLC fn.Option[lnwire.MilliSatoshi]

	// CsvDelay is the relative time lock delay expressed in blocks. Any
	// settled outputs that pay to the owner of this channel configuration
	// MUST ensure that the delay branch uses this value as the relative
	// time lock. Similarly, any HTLC's offered by this node should use
	// this value as well.
	CsvDelay fn.Option[uint16]

	// MaxAcceptedHtlcs is the maximum number of HTLCs that the owner of
	// this set of constraints can offer the remote node. This allows each
	// node to limit their over all exposure to HTLCs that may need to be
	// acted upon in the case of a unilateral channel closure or a contract
	// breach.
	MaxAcceptedHtlcs fn.Option[uint16]
}

// UpdateLinkResponse defines a response to be returned to the caller.
type UpdateLinkResponse struct {
	// Status gives the current status of the update flow.
	Status UpdateLinkStatus

	// Err gives the details about a failed update request.
	Err error
}

// UpdateReq is a `fn.Req` type that wraps the request and response into a
// single struct.
type UpdateReq = fn.Req[UpdateLinkRequest, UpdateLinkResponse]

// Updater defines an interface that is used to update the channel link params.
// It provides the basic flow control methods like Start and Stop. In addition,
// `InitReq` is used for the user to initialize an upgrade request flow, and
// `ReceiveMsg` should be called in the link when a related msg is received,
// such as DynProposal, DynAck, DynCommit, DynReject, CommitSig, and
// RevokeAndAck.
type Updater interface {
	// Start starts the updater so it can accept update requests.
	Start()

	// Stop stops the updater.
	Stop()

	// InitReq initializes a channel update request, which is used by the
	// user to start the update flow via RPC.
	InitReq(r *UpdateReq) error

	// ReceiveMsg takes a msg sent from the peer and processes it.
	ReceiveMsg(msg lnwire.Message)
}

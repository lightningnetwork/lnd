package types

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// CloseOutput represents an output that should be included in the close
// transaction.
type CloseOutput struct {
	// Amt is the amount of the output.
	Amt btcutil.Amount

	// DustLimit is the dust limit for the local node.
	DustLimit btcutil.Amount

	// PkScript is the script that should be used to pay to the output.
	PkScript []byte

	// ShutdownRecords is the set of custom records that may result in
	// extra close outputs being added.
	ShutdownRecords lnwire.CustomRecords
}

// AuxShutdownReq is used to request a set of extra custom records to include
// in the shutdown message.
type AuxShutdownReq struct {
	// ChanPoint is the channel point of the channel that is being shut
	// down.
	ChanPoint wire.OutPoint

	// ShortChanID is the short channel ID of the channel that is being
	// closed.
	ShortChanID lnwire.ShortChannelID

	// Initiator is true if the local node is the initiator of the channel.
	Initiator bool

	// InternalKey is the internal key for the shutdown addr. This will
	// only be set for taproot shutdown addrs.
	InternalKey fn.Option[btcec.PublicKey]

	// CommitBlob is the blob that was included in the last commitment.
	CommitBlob fn.Option[tlv.Blob]

	// FundingBlob is the blob that was included in the funding state.
	FundingBlob fn.Option[tlv.Blob]
}

// AuxCloseDesc is used to describe the channel close that is being performed.
type AuxCloseDesc struct {
	AuxShutdownReq

	// CloseFee is the closing fee to be paid for this state.
	CloseFee btcutil.Amount

	// CommitFee is the fee that was paid for the last commitment.
	CommitFee btcutil.Amount

	// LocalCloseOutput is the output that the local node should be paid
	// to. This is None if the local party will not have an output on the
	// co-op close transaction.
	LocalCloseOutput fn.Option[CloseOutput]

	// RemoteCloseOutput is the output that the remote node should be paid
	// to. This will be None if the remote party will not have an output on
	// the co-op close transaction.
	RemoteCloseOutput fn.Option[CloseOutput]
}

package lnwallet

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// CloseType is an enum that represents the type of close that we are trying to
// resolve.
type CloseType uint8

const (
	// LocalForceClose represents a local force close.
	LocalForceClose CloseType = iota

	// RemoteForceClose represents a remote force close.
	RemoteForceClose

	// Breach represents a breach by the remote party.
	Breach
)

// ResolutionReq is used to ask an outside sub-system for additional
// information needed to resolve a contract.
type ResolutionReq struct {
	// ChanPoint is the channel point of the channel that we are trying to
	// resolve.
	ChanPoint wire.OutPoint

	// ShortChanID is the short channel ID of the channel that we are
	// trying to resolve.
	ShortChanID lnwire.ShortChannelID

	// Initiator is a bool if we're the initiator of the channel.
	Initiator bool

	// CommitBlob is an optional commit blob for the channel.
	CommitBlob fn.Option[tlv.Blob]

	// FundingBlob is an optional funding blob for the channel.
	FundingBlob fn.Option[tlv.Blob]

	// Type is the type of the witness that we are trying to resolve.
	Type input.WitnessType

	// CloseType is the type of close that we are trying to resolve.
	CloseType CloseType

	// CommitTx is the force close commitment transaction.
	CommitTx *wire.MsgTx

	// CommitFee is the fee that was paid for the commitment transaction.
	CommitFee btcutil.Amount

	// ContractPoint is the outpoint of the contract we're trying to
	// resolve.
	ContractPoint wire.OutPoint

	// SignDesc is the sign descriptor for the contract.
	SignDesc input.SignDescriptor

	// KeyRing is the key ring for the channel.
	KeyRing *CommitmentKeyRing

	// CsvDelay is the CSV delay for the local output for this commitment.
	CsvDelay uint32

	// BreachCsvDelay is the CSV delay for the remote output. This is only
	// set when the CloseType is Breach. This indicates the CSV delay to
	// use for the remote party's to_local delayed output, that is now
	// rightfully ours in a breach situation.
	BreachCsvDelay fn.Option[uint32]

	// CltvDelay is the CLTV delay for the outpoint.
	CltvDelay fn.Option[uint32]
}

// AuxContractResolver is an interface that is used to resolve contracts that
// may need additional outside information to resolve correctly.
type AuxContractResolver interface {
	// ResolveContract is called to resolve a contract that needs
	// additional information to resolve properly. If no extra information
	// is required, a nil Result error is returned.
	ResolveContract(ResolutionReq) fn.Result[tlv.Blob]
}

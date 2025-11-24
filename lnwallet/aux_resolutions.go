package lnwallet

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
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

// AuxSigDesc stores optional information related to 2nd level HTLCs for aux
// channels.
type AuxSigDesc struct {
	// AuxSig is the second-level signature for the HTLC that we are trying
	// to resolve. This is only present if this is a resolution request for
	// an HTLC on our commitment transaction.
	AuxSig []byte

	// SignDetails is the sign details for the second-level HTLC. This may
	// be used to generate the second signature needed for broadcast.
	SignDetails input.SignDetails
}

// ResolutionReq is used to ask an outside sub-system for additional
// information needed to resolve a contract.
type ResolutionReq struct {
	// ChanPoint is the channel point of the channel that we are trying to
	// resolve.
	ChanPoint wire.OutPoint

	// ChanType is the type of the channel that we are trying to resolve.
	ChanType channeldb.ChannelType

	// ShortChanID is the short channel ID of the channel that we are
	// trying to resolve.
	ShortChanID lnwire.ShortChannelID

	// Initiator is a bool if we're the initiator of the channel.
	Initiator bool

	// CommitBlob is an optional commit blob for the channel.
	CommitBlob fn.Option[tlv.Blob]

	// FundingBlob is an optional funding blob for the channel.
	FundingBlob fn.Option[tlv.Blob]

	// HtlcID is the ID of the HTLC that we are trying to resolve. This is
	// only set if this is a resolution request for an HTLC.
	HtlcID fn.Option[input.HtlcIndex]

	// HtlcAmt is the amount of the HTLC that we are trying to resolve.
	HtlcAmt btcutil.Amount

	// Type is the type of the witness that we are trying to resolve.
	Type input.WitnessType

	// CloseType is the type of close that we are trying to resolve.
	CloseType CloseType

	// CommitTx is the force close commitment transaction.
	CommitTx *wire.MsgTx

	// CommitTxBlockHeight is the block height where the commitment
	// transaction confirmed. It is 0 if unknown or not confirmed yet.
	CommitTxBlockHeight uint32

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

	// CommitCsvDelay is the CSV delay for the remote output for this
	// commitment.
	CommitCsvDelay uint32

	// BreachCsvDelay is the CSV delay for the remote output. This is only
	// set when the CloseType is Breach. This indicates the CSV delay to
	// use for the remote party's to_local delayed output, that is now
	// rightfully ours in a breach situation.
	BreachCsvDelay fn.Option[uint32]

	// CltvDelay is the CLTV delay for the outpoint/transaction.
	CltvDelay fn.Option[uint32]

	// PayHash is the payment hash for the HTLC that we are trying to
	// resolve. This is optional as it only applies HTLC outputs.
	PayHash fn.Option[[32]byte]

	// AuxSigDesc is an optional field that contains additional information
	// needed to sweep second level HTLCs.
	AuxSigDesc fn.Option[AuxSigDesc]
}

// AuxContractResolver is an interface that is used to resolve contracts that
// may need additional outside information to resolve correctly.
type AuxContractResolver interface {
	// ResolveContract is called to resolve a contract that needs
	// additional information to resolve properly. If no extra information
	// is required, a nil Result error is returned.
	ResolveContract(ResolutionReq) fn.Result[tlv.Blob]
}

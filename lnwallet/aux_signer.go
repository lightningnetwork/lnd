package lnwallet

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// htlcCustomSigType is the TLV type that is used to encode the custom
	// HTLC signatures within the custom data for an existing HTLC.
	htlcCustomSigType tlv.TlvType65543

	// NoOpHtlcTLVEntry is the TLV that that's used in the update_add_htlc
	// message to indicate the presence of a noop HTLC. This has no encoded
	// value, its presence is used to indicate that the HTLC is a noop.
	NoOpHtlcTLVEntry tlv.TlvType65544
)

// NoOpHtlcTLVType is the (golang) type of the TLV record that's used to signal
// that an HTLC should be a noop HTLC.
type NoOpHtlcTLVType = tlv.TlvType65544

// AuxHtlcView is a struct that contains a safe copy of an HTLC view that can
// be used by aux components.
type AuxHtlcView struct {
	// NextHeight is the height of the commitment transaction that will be
	// created using this view.
	NextHeight uint64

	// Updates is a Dual of the Local and Remote HTLCs.
	Updates lntypes.Dual[[]AuxHtlcDescriptor]

	// FeePerKw is the fee rate in sat/kw of the commitment transaction.
	FeePerKw chainfee.SatPerKWeight
}

// newAuxHtlcView creates a new safe copy of the HTLC view that can be used by
// aux components.
//
// NOTE: This function should only be called while holding the channel's read
// lock, since the underlying local/remote payment descriptors are accessed
// directly.
func newAuxHtlcView(v *HtlcView) AuxHtlcView {
	return AuxHtlcView{
		NextHeight: v.NextHeight,
		Updates: lntypes.Dual[[]AuxHtlcDescriptor]{
			Local:  fn.Map(v.Updates.Local, newAuxHtlcDescriptor),
			Remote: fn.Map(v.Updates.Remote, newAuxHtlcDescriptor),
		},
		FeePerKw: v.FeePerKw,
	}
}

// AuxHtlcDescriptor is a struct that contains the information needed to sign or
// verify an HTLC for custom channels.
type AuxHtlcDescriptor struct {
	// ChanID is the ChannelID of the LightningChannel that this
	// paymentDescriptor belongs to. We track this here so we can
	// reconstruct the Messages that this paymentDescriptor is built from.
	ChanID lnwire.ChannelID

	// RHash is the payment hash for this HTLC. The HTLC can be settled iff
	// the preimage to this hash is presented.
	RHash PaymentHash

	// Timeout is the absolute timeout in blocks, after which this HTLC
	// expires.
	Timeout uint32

	// Amount is the HTLC amount in milli-satoshis.
	Amount lnwire.MilliSatoshi

	// HtlcIndex is the index within the main update log for this HTLC.
	// Entries within the log of type Add will have this field populated,
	// as other entries will point to the entry via this counter.
	//
	// NOTE: This field will only be populated if EntryType is Add.
	HtlcIndex uint64

	// ParentIndex is the HTLC index of the entry that this update settles
	// or times out.
	//
	// NOTE: This field will only be populated if EntryType is Fail or
	// Settle.
	ParentIndex uint64

	// EntryType denotes the exact type of the paymentDescriptor. In the
	// case of a Timeout, or Settle type, then the Parent field will point
	// into the log to the HTLC being modified.
	EntryType updateType

	// CustomRecords also stores the set of optional custom records that
	// may have been attached to a sent HTLC.
	CustomRecords lnwire.CustomRecords

	// addCommitHeight[Remote|Local] encodes the height of the commitment
	// which included this HTLC on either the remote or local commitment
	// chain. This value is used to determine when an HTLC is fully
	// "locked-in".
	addCommitHeightRemote uint64
	addCommitHeightLocal  uint64

	// removeCommitHeight[Remote|Local] encodes the height of the
	// commitment which removed the parent pointer of this
	// paymentDescriptor either due to a timeout or a settle. Once both
	// these heights are below the tail of both chains, the log entries can
	// safely be removed.
	removeCommitHeightRemote uint64
	removeCommitHeightLocal  uint64
}

// AddHeight returns the height at which the HTLC was added to the commitment
// chain. The height is returned based on the chain the HTLC is being added to
// (local or remote chain).
func (a *AuxHtlcDescriptor) AddHeight(
	whoseCommitChain lntypes.ChannelParty) uint64 {

	if whoseCommitChain.IsRemote() {
		return a.addCommitHeightRemote
	}

	return a.addCommitHeightLocal
}

// IsAdd checks if the entry type of the Aux HTLC Descriptor is an add type.
func (a *AuxHtlcDescriptor) IsAdd() bool {
	switch a.EntryType {
	case Add:
		fallthrough
	case NoOpAdd:
		return true
	default:
		return false
	}
}

// RemoveHeight returns the height at which the HTLC was removed from the
// commitment chain. The height is returned based on the chain the HTLC is being
// removed from (local or remote chain).
func (a *AuxHtlcDescriptor) RemoveHeight(
	whoseCommitChain lntypes.ChannelParty) uint64 {

	if whoseCommitChain.IsRemote() {
		return a.removeCommitHeightRemote
	}

	return a.removeCommitHeightLocal
}

// newAuxHtlcDescriptor creates a new AuxHtlcDescriptor from a payment
// descriptor.
//
// NOTE: This function should only be called while holding the channel's read
// lock, since the underlying payment descriptors are accessed directly.
func newAuxHtlcDescriptor(p *paymentDescriptor) AuxHtlcDescriptor {
	return AuxHtlcDescriptor{
		ChanID:                   p.ChanID,
		RHash:                    p.RHash,
		Timeout:                  p.Timeout,
		Amount:                   p.Amount,
		HtlcIndex:                p.HtlcIndex,
		ParentIndex:              p.ParentIndex,
		EntryType:                p.EntryType,
		CustomRecords:            p.CustomRecords.Copy(),
		addCommitHeightRemote:    p.addCommitHeights.Remote,
		addCommitHeightLocal:     p.addCommitHeights.Local,
		removeCommitHeightRemote: p.removeCommitHeights.Remote,
		removeCommitHeightLocal:  p.removeCommitHeights.Local,
	}
}

// BaseAuxJob is a struct that contains the common fields that are shared among
// the aux sign/verify jobs.
type BaseAuxJob struct {
	// OutputIndex is the output index of the HTLC on the commitment
	// transaction being signed.
	//
	// NOTE: If the output is dust from the PoV of the commitment chain,
	// then this value will be -1.
	OutputIndex int32

	// KeyRing is the commitment key ring that contains the keys needed to
	// generate the second level HTLC signatures.
	KeyRing CommitmentKeyRing

	// HTLC is the HTLC that is being signed or verified.
	HTLC AuxHtlcDescriptor

	// Incoming is a boolean that indicates if the HTLC is incoming or
	// outgoing.
	Incoming bool

	// CommitBlob is the commitment transaction blob that contains the aux
	// information for this channel.
	CommitBlob fn.Option[tlv.Blob]

	// HtlcLeaf is the aux tap leaf that corresponds to the HTLC being
	// signed/verified.
	HtlcLeaf input.AuxTapLeaf
}

// AuxSigJob is a struct that contains all the information needed to sign an
// HTLC for custom channels.
type AuxSigJob struct {
	// SignDesc is the sign desc for this HTLC.
	SignDesc input.SignDescriptor

	BaseAuxJob

	// Resp is a channel that will be used to send the result of the sign
	// job. This channel MUST be buffered.
	Resp chan AuxSigJobResp

	// Cancel is a channel that is closed by the caller if they wish to
	// abandon all pending sign jobs part of a single batch. This should
	// never be closed by the validator.
	Cancel <-chan struct{}
}

// NewAuxSigJob creates a new AuxSigJob.
func NewAuxSigJob(sigJob SignJob, keyRing CommitmentKeyRing, incoming bool,
	htlc AuxHtlcDescriptor, commitBlob fn.Option[tlv.Blob],
	htlcLeaf input.AuxTapLeaf, cancelChan <-chan struct{}) AuxSigJob {

	return AuxSigJob{
		SignDesc: sigJob.SignDesc,
		BaseAuxJob: BaseAuxJob{
			OutputIndex: sigJob.OutputIndex,
			KeyRing:     keyRing,
			HTLC:        htlc,
			Incoming:    incoming,
			CommitBlob:  commitBlob,
			HtlcLeaf:    htlcLeaf,
		},
		Resp:   make(chan AuxSigJobResp, 1),
		Cancel: cancelChan,
	}
}

// AuxSigJobResp is a struct that contains the result of a sign job.
type AuxSigJobResp struct {
	// SigBlob is the signature blob that was generated for the HTLC. This
	// is an opaque TLV field that may contain the signature and other data.
	SigBlob fn.Option[tlv.Blob]

	// HtlcIndex is the index of the HTLC that was signed.
	HtlcIndex uint64

	// Err is the error that occurred when executing the specified
	// signature job. In the case that no error occurred, this value will
	// be nil.
	Err error
}

// AuxVerifyJob is a struct that contains all the information needed to verify
// an HTLC for custom channels.
type AuxVerifyJob struct {
	// SigBlob is the signature blob that was generated for the HTLC. This
	// is an opaque TLV field that may contain the signature and other data.
	SigBlob fn.Option[tlv.Blob]

	BaseAuxJob
}

// NewAuxVerifyJob creates a new AuxVerifyJob.
func NewAuxVerifyJob(sig fn.Option[tlv.Blob], keyRing CommitmentKeyRing,
	incoming bool, htlc AuxHtlcDescriptor, commitBlob fn.Option[tlv.Blob],
	htlcLeaf input.AuxTapLeaf) AuxVerifyJob {

	return AuxVerifyJob{
		SigBlob: sig,
		BaseAuxJob: BaseAuxJob{
			KeyRing:    keyRing,
			HTLC:       htlc,
			Incoming:   incoming,
			CommitBlob: commitBlob,
			HtlcLeaf:   htlcLeaf,
		},
	}
}

// AuxSigner is an interface that is used to sign and verify HTLCs for custom
// channels. It is similar to the existing SigPool, but uses opaque blobs to
// shuffle around signature information and other metadata.
type AuxSigner interface {
	// SubmitSecondLevelSigBatch takes a batch of aux sign jobs and
	// processes them asynchronously.
	SubmitSecondLevelSigBatch(chanState AuxChanState, commitTx *wire.MsgTx,
		sigJob []AuxSigJob) error

	// PackSigs takes a series of aux signatures and packs them into a
	// single blob that can be sent alongside the CommitSig messages.
	PackSigs([]fn.Option[tlv.Blob]) fn.Result[fn.Option[tlv.Blob]]

	// UnpackSigs takes a packed blob of signatures and returns the
	// original signatures for each HTLC, keyed by HTLC index.
	UnpackSigs(fn.Option[tlv.Blob]) fn.Result[[]fn.Option[tlv.Blob]]

	// VerifySecondLevelSigs attempts to synchronously verify a batch of aux
	// sig jobs.
	VerifySecondLevelSigs(chanState AuxChanState, commitTx *wire.MsgTx,
		verifyJob []AuxVerifyJob) error
}

package lnwallet

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/tlv"
)

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
	HTLC PaymentDescriptor

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
	htlc PaymentDescriptor, commitBlob fn.Option[tlv.Blob],
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

	// Cancel is a channel that should be closed if the caller wishes to
	// abandon the job.
	Cancel chan struct{}

	// ErrResp is a channel that will be used to send the result of the
	// verify job.
	ErrResp chan error
}

// NewAuxVerifyJob creates a new AuxVerifyJob.
func NewAuxVerifyJob(sig fn.Option[tlv.Blob], keyRing CommitmentKeyRing,
	incoming bool, htlc PaymentDescriptor, commitBlob fn.Option[tlv.Blob],
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

// htlcCustomSigType is the TLV type that is used to encode the custom HTLC
// signatures within the custom data for an existing HTLC.
var htlcCustomSigType tlv.TlvType65543

// AuxSigner is an interface that is used to sign and verify HTLCs for custom
// channels. It is similar to the existing SigPool, but uses opaque blobs to
// shuffle around signature information and other metadata.
type AuxSigner interface {
	// SubmitSecondLevelSigBatch takes a batch of aux sign jobs and
	// processes them asynchronously.
	SubmitSecondLevelSigBatch(chanState *channeldb.OpenChannel,
		commitTx *wire.MsgTx, sigJob []AuxSigJob) error

	// PackSigs takes a series of aux signatures and packs them into a
	// single blob that can be sent alongside the CommitSig messages.
	PackSigs([]fn.Option[tlv.Blob]) (fn.Option[tlv.Blob], error)

	// UnpackSigs takes a packed blob of signatures and returns the
	// original signatures for each HTLC, keyed by HTLC index.
	UnpackSigs(fn.Option[tlv.Blob]) ([]fn.Option[tlv.Blob], error)

	// VerifySecondLevelSigs attempts to synchronously verify a batch of aux
	// sig jobs.
	VerifySecondLevelSigs(chanState *channeldb.OpenChannel,
		commitTx *wire.MsgTx, verifyJob []AuxVerifyJob) error
}

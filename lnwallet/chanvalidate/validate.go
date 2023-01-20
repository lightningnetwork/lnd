package chanvalidate

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrInvalidOutPoint is returned when the ChanLocator is unable to
	// find the target outpoint.
	ErrInvalidOutPoint = fmt.Errorf("output meant to create channel cannot " +
		"be found")

	// ErrWrongPkScript is returned when the alleged funding transaction is
	// found to have an incorrect pkSript.
	ErrWrongPkScript = fmt.Errorf("wrong pk script")

	// ErrInvalidSize is returned when the alleged funding transaction
	// output has the wrong size (channel capacity).
	ErrInvalidSize = fmt.Errorf("channel has wrong size")
)

// ErrScriptValidateError is returned when Script VM validation fails for an
// alleged channel output.
type ErrScriptValidateError struct {
	err error
}

// Error returns a human readable string describing the error.
func (e *ErrScriptValidateError) Error() string {
	return fmt.Sprintf("script validation failed: %v", e.err)
}

// Unwrap returns the underlying wrapped VM execution failure error.
func (e *ErrScriptValidateError) Unwrap() error {
	return e.err
}

// ChanLocator abstracts away obtaining the output that created the channel, as
// well as validating its existence given the funding transaction.  We need
// this as there are several ways (outpoint, short chan ID) to identify the
// output of a channel given the funding transaction.
type ChanLocator interface {
	// Locate attempts to locate the funding output within the funding
	// transaction. It also returns the final out point of the channel
	// which uniquely identifies the output which creates the channel. If
	// the target output cannot be found, or cannot exist on the funding
	// transaction, then an error is to be returned.
	Locate(*wire.MsgTx) (*wire.TxOut, *wire.OutPoint, error)
}

// OutPointChanLocator is an implementation of the ChanLocator that can be used
// when one already knows the expected chan point.
type OutPointChanLocator struct {
	// ChanPoint is the expected chan point.
	ChanPoint wire.OutPoint
}

// Locate attempts to locate the funding output within the passed funding
// transaction.
//
// NOTE: Part of the ChanLocator interface.
func (o *OutPointChanLocator) Locate(fundingTx *wire.MsgTx) (
	*wire.TxOut, *wire.OutPoint, error) {

	// If the expected index is greater than the amount of output in the
	// transaction, then we'll reject this channel as it's invalid.
	if int(o.ChanPoint.Index) >= len(fundingTx.TxOut) {
		return nil, nil, ErrInvalidOutPoint
	}

	// As an extra sanity check, we'll also ensure the txid hash matches.
	fundingHash := fundingTx.TxHash()
	if !bytes.Equal(fundingHash[:], o.ChanPoint.Hash[:]) {
		return nil, nil, ErrInvalidOutPoint
	}

	return fundingTx.TxOut[o.ChanPoint.Index], &o.ChanPoint, nil
}

// ShortChanIDChanLocator is an implementation of the ChanLocator that can be
// used when one only knows the short channel ID of a channel. This should be
// used in contexts when one is verifying a 3rd party channel.
type ShortChanIDChanLocator struct {
	// ID is the short channel ID of the target channel.
	ID lnwire.ShortChannelID
}

// Locate attempts to locate the funding output within the passed funding
// transaction.
//
// NOTE: Part of the ChanLocator interface.
func (s *ShortChanIDChanLocator) Locate(fundingTx *wire.MsgTx) (
	*wire.TxOut, *wire.OutPoint, error) {

	// If the expected index is greater than the amount of output in the
	// transaction, then we'll reject this channel as it's invalid.
	outputIndex := s.ID.TxPosition
	if int(outputIndex) >= len(fundingTx.TxOut) {
		return nil, nil, ErrInvalidOutPoint
	}

	chanPoint := wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: uint32(outputIndex),
	}

	return fundingTx.TxOut[outputIndex], &chanPoint, nil
}

// CommitmentContext is optional validation context that can be passed into the
// main Validate for self-owned channel. The information in this context allows
// us to fully verify out initial commitment spend based on the on-chain state
// of the funding output.
type CommitmentContext struct {
	// Value is the known size of the channel.
	Value btcutil.Amount

	// FullySignedCommitTx is the fully signed commitment transaction. This
	// should include a valid witness.
	FullySignedCommitTx *wire.MsgTx
}

// Context is the main validation contxet. For a given channel, all fields but
// the optional CommitCtx should be populated based on existing
// known-to-be-valid parameters.
type Context struct {
	// Locator is a concrete implementation of the ChanLocator interface.
	Locator ChanLocator

	// MultiSigPkScript is the fully serialized witness script of the
	// multi-sig output. This is the final witness program that should be
	// found in the funding output.
	MultiSigPkScript []byte

	// FundingTx is channel funding transaction as found confirmed in the
	// chain.
	FundingTx *wire.MsgTx

	// CommitCtx is an optional additional set of validation context
	// required to validate a self-owned channel. If present, then a full
	// Script VM validation will be performed.
	CommitCtx *CommitmentContext
}

// Validate given the specified context, this function validates that the
// alleged channel is well formed, and spendable (if the optional CommitCtx is
// specified).  If this method returns an error, then the alleged channel is
// invalid and should be abandoned immediately.
func Validate(ctx *Context) (*wire.OutPoint, error) {
	// First, we'll attempt to locate the target outpoint in the funding
	// transaction. If this returns an error, then we know that the
	// outpoint doesn't actually exist, so we'll exit early.
	fundingOutput, chanPoint, err := ctx.Locator.Locate(
		ctx.FundingTx,
	)
	if err != nil {
		return nil, err
	}

	// The scripts should match up exactly, otherwise the channel is
	// invalid.
	fundingScript := fundingOutput.PkScript
	if !bytes.Equal(ctx.MultiSigPkScript, fundingScript) {
		return nil, ErrWrongPkScript
	}

	// If there's no commitment context, then we're done here as this is a
	// 3rd party channel.
	if ctx.CommitCtx == nil {
		return chanPoint, nil
	}

	// Now that we know this is our channel, we'll verify the amount of the
	// created output against our expected size of the channel.
	fundingValue := fundingOutput.Value
	if btcutil.Amount(fundingValue) != ctx.CommitCtx.Value {
		return nil, ErrInvalidSize
	}

	// If we reach this point, then all other checks have succeeded, so
	// we'll now attempt a full Script VM execution to ensure that we're
	// able to close the channel using this initial state.
	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		ctx.MultiSigPkScript, fundingValue,
	)
	commitTx := ctx.CommitCtx.FullySignedCommitTx
	hashCache := txscript.NewTxSigHashes(commitTx, prevFetcher)
	vm, err := txscript.NewEngine(
		ctx.MultiSigPkScript, commitTx, 0, txscript.StandardVerifyFlags,
		nil, hashCache, fundingValue, prevFetcher,
	)
	if err != nil {
		return nil, err
	}

	// Finally, we'll attempt to verify our full spend, if this fails then
	// the channel is definitely invalid.
	err = vm.Execute()
	if err != nil {
		return nil, &ErrScriptValidateError{err: err}
	}

	return chanPoint, nil
}

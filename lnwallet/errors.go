package lnwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrZeroCapacity returns an error indicating the funder attempted to put zero
// funds into the channel.
func ErrZeroCapacity() *lnwire.CodedError {
	return lnwire.NewErroneousFieldErr(lnwire.MsgOpenChannel, 2, 0, nil)
}

// ErrChainMismatch returns an error indicating that the initiator tried to
// open a channel for an unknown chain.
func ErrChainMismatch(knownChain,
	unknownChain *chainhash.Hash) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 0, unknownChain, knownChain,
	)
}

// ErrFunderBalanceDust returns an error indicating the initial balance of the
// funder is considered dust at the current commitment fee.
func ErrFunderBalanceDust(commitFee, funderBalance,
	minBalance uint64) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 4, funderBalance, minBalance,
	)
}

// ErrCsvDelayTooLarge returns an error indicating that the CSV delay was to
// large to be accepted, along with the current max.
func ErrCsvDelayTooLarge(remoteDelay,
	maxDelay uint16) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 9, remoteDelay, maxDelay,
	)
}

// ErrChanReserveTooSmall returns an error indicating that the channel reserve
// the remote is requiring is too small to be accepted.
func ErrChanReserveTooSmall(reserve,
	dustLimit btcutil.Amount) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 6, uint64(reserve), uint64(dustLimit),
	)
}

// ErrChanReserveTooLarge returns an error indicating that the chan reserve the
// remote is requiring, is too large to be accepted.
func ErrChanReserveTooLarge(reserve,
	maxReserve btcutil.Amount) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 6, uint64(reserve), uint64(maxReserve),
	)
}

// ErrNonZeroPushAmount is returned by a remote peer that receives a
// FundingOpen request for a channel with non-zero push amount while
// they have 'rejectpush' enabled.
func ErrNonZeroPushAmount(amt uint64) *lnwire.CodedError {
	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 3, amt, uint64(0),
	)
}

// ErrMinHtlcTooLarge returns an error indicating that the MinHTLC value the
// remote required is too large to be accepted.
func ErrMinHtlcTooLarge(minHtlc,
	maxMinHtlc lnwire.MilliSatoshi) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 7, uint64(minHtlc), uint64(maxMinHtlc),
	)
}

// ErrMaxHtlcNumTooLarge returns an error indicating that the 'max HTLCs in
// flight' value the remote required is too large to be accepted.
func ErrMaxHtlcNumTooLarge(maxHtlc, maxMaxHtlc uint16) *lnwire.CodedError {
	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 10, maxMaxHtlc, maxMaxHtlc,
	)
}

// ErrMaxHtlcNumTooSmall returns an error indicating that the 'max HTLCs in
// flight' value the remote required is too small to be accepted.
func ErrMaxHtlcNumTooSmall(maxHtlc, minMaxHtlc uint16) *lnwire.CodedError {
	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 10, maxHtlc, minMaxHtlc,
	)
}

// ErrMaxValueInFlightTooSmall returns an error indicating that the 'max HTLC
// value in flight' the remote required is too small to be accepted.
func ErrMaxValueInFlightTooSmall(maxValInFlight,
	minMaxValInFlight lnwire.MilliSatoshi) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 5, maxValInFlight, minMaxValInFlight,
	)
}

// ErrNumConfsTooLarge returns an error indicating that the number of
// confirmations required for a channel is too large.
func ErrNumConfsTooLarge(numConfs, maxNumConfs uint32) *lnwire.CodedError {
	return lnwire.NewErroneousFieldErr(
		lnwire.MsgAcceptChannel, 5, numConfs, maxNumConfs,
	)
}

// ErrChanTooSmall returns an error indicating that an incoming channel request
// was too small. We'll reject any incoming channels if they're below our
// configured value for the min channel size we'll accept.
func ErrChanTooSmall(chanSize,
	minChanSize btcutil.Amount) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 2, uint64(chanSize), uint64(minChanSize),
	)
}

// ErrChanTooLarge returns an error indicating that an incoming channel request
// was too large. We'll reject any incoming channels if they're above our
// configured value for the max channel size we'll accept.
func ErrChanTooLarge(chanSize,
	maxChanSize btcutil.Amount) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 2, uint64(chanSize), uint64(maxChanSize),
	)
}

// ErrInvalidDustLimit returns an error indicating that a proposed DustLimit
// was rejected.
func ErrInvalidDustLimit(dustLimit,
	recommended btcutil.Amount) *lnwire.CodedError {

	return lnwire.NewErroneousFieldErr(
		lnwire.MsgOpenChannel, 2, uint64(dustLimit),
		uint64(recommended),
	)
}

// ErrHtlcIndexAlreadyFailed is returned when the HTLC index has already been
// failed, but has not been committed by our commitment state.
type ErrHtlcIndexAlreadyFailed uint64

// Error returns a message indicating the index that had already been failed.
func (e ErrHtlcIndexAlreadyFailed) Error() string {
	return fmt.Sprintf("HTLC with ID %d has already been failed", e)
}

// ErrHtlcIndexAlreadySettled is returned when the HTLC index has already been
// settled, but has not been committed by our commitment state.
type ErrHtlcIndexAlreadySettled uint64

// Error returns a message indicating the index that had already been settled.
func (e ErrHtlcIndexAlreadySettled) Error() string {
	return fmt.Sprintf("HTLC with ID %d has already been settled", e)
}

// ErrInvalidSettlePreimage is returned when trying to settle an HTLC, but the
// preimage does not correspond to the payment hash.
type ErrInvalidSettlePreimage struct {
	preimage []byte
	rhash    []byte
}

// Error returns an error message with the offending preimage and intended
// payment hash.
func (e ErrInvalidSettlePreimage) Error() string {
	return fmt.Sprintf("Invalid payment preimage %x for hash %x",
		e.preimage, e.rhash)
}

// ErrUnknownHtlcIndex is returned when locally settling or failing an HTLC, but
// the HTLC index is not known to the channel. This typically indicates that the
// HTLC was already settled in a prior commitment.
type ErrUnknownHtlcIndex struct {
	chanID lnwire.ShortChannelID
	index  uint64
}

// Error returns an error logging the channel and HTLC index that was unknown.
func (e ErrUnknownHtlcIndex) Error() string {
	return fmt.Sprintf("No HTLC with ID %d in channel %v",
		e.index, e.chanID)
}

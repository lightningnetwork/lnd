package lnwallet

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcutil"
)

// ReservationError wraps certain errors returned during channel reservation
// that can be sent across the wire to the remote peer. Errors not being
// ReservationErrors will not be sent to the remote in case of a failed channel
// reservation, as they may contain private information.
type ReservationError struct {
	error
}

// A compile time check to ensure ReservationError implements the error
// interface.
var _ error = (*ReservationError)(nil)

// ErrZeroCapacity returns an error indicating the funder attempted to put zero
// funds into the channel.
func ErrZeroCapacity() ReservationError {
	return ReservationError{
		errors.New("zero channel funds"),
	}
}

// ErrChainMismatch returns an error indicating that the initiator tried to
// open a channel for an unknown chain.
func ErrChainMismatch(knownChain,
	unknownChain *chainhash.Hash) ReservationError {
	return ReservationError{
		fmt.Errorf("Unknown chain=%v. Supported chain=%v",
			unknownChain, knownChain),
	}
}

// ErrFunderBalanceDust returns an error indicating the initial balance of the
// funder is considered dust at the current commitment fee.
func ErrFunderBalanceDust(commitFee, funderBalance,
	minBalance int64) ReservationError {
	return ReservationError{
		fmt.Errorf("Funder balance too small (%v) with fee=%v sat, "+
			"minimum=%v sat required", funderBalance,
			commitFee, minBalance),
	}
}

// ErrCsvDelayTooLarge returns an error indicating that the CSV delay was to
// large to be accepted, along with the current max.
func ErrCsvDelayTooLarge(remoteDelay, maxDelay uint16) ReservationError {
	return ReservationError{
		fmt.Errorf("CSV delay too large: %v, max is %v",
			remoteDelay, maxDelay),
	}
}

// ErrChanReserveTooLarge returns an error indicating that the chan reserve the
// remote is requiring, is too large to be accepted.
func ErrChanReserveTooLarge(reserve,
	maxReserve btcutil.Amount) ReservationError {
	return ReservationError{
		fmt.Errorf("Channel reserve is too large: %v sat, max "+
			"is %v sat", int64(reserve), int64(maxReserve)),
	}
}

// ErrMinHtlcTooLarge returns an error indicating that the MinHTLC value the
// remote required is too large to be accepted.
func ErrMinHtlcTooLarge(minHtlc,
	maxMinHtlc lnwire.MilliSatoshi) ReservationError {
	return ReservationError{
		fmt.Errorf("Minimum HTLC value is too large: %v, max is %v",
			minHtlc, maxMinHtlc),
	}
}

// ErrMaxHtlcNumTooLarge returns an error indicating that the 'max HTLCs in
// flight' value the remote required is too large to be accepted.
func ErrMaxHtlcNumTooLarge(maxHtlc, maxMaxHtlc uint16) ReservationError {
	return ReservationError{
		fmt.Errorf("maxHtlcs is too large: %d, max is %d",
			maxHtlc, maxMaxHtlc),
	}
}

// ErrMaxHtlcNumTooSmall returns an error indicating that the 'max HTLCs in
// flight' value the remote required is too small to be accepted.
func ErrMaxHtlcNumTooSmall(maxHtlc, minMaxHtlc uint16) ReservationError {
	return ReservationError{
		fmt.Errorf("maxHtlcs is too small: %d, min is %d",
			maxHtlc, minMaxHtlc),
	}
}

// ErrMaxValueInFlightTooSmall returns an error indicating that the 'max HTLC
// value in flight' the remote required is too small to be accepted.
func ErrMaxValueInFlightTooSmall(maxValInFlight,
	minMaxValInFlight lnwire.MilliSatoshi) ReservationError {
	return ReservationError{
		fmt.Errorf("maxValueInFlight too small: %v, min is %v",
			maxValInFlight, minMaxValInFlight),
	}
}

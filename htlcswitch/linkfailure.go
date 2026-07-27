package htlcswitch

import (
	"errors"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/sqldb"
)

var (
	// ErrLinkShuttingDown signals that the link is shutting down.
	ErrLinkShuttingDown = errors.New("link shutting down")

	// ErrLinkFailedShutdown signals that a requested shutdown failed.
	ErrLinkFailedShutdown = errors.New("link failed to shutdown")
)

// errorCode encodes the possible types of errors that will make us fail the
// current link.
type errorCode uint8

const (
	// ErrInternalError indicates that something internal in the link
	// failed. In this case we will send a generic error to our peer.
	ErrInternalError errorCode = iota

	// ErrRemoteError indicates that our peer sent an error, prompting up
	// to fail the link.
	ErrRemoteError

	// ErrRemoteUnresponsive indicates that our peer took too long to
	// complete a commitment dance.
	ErrRemoteUnresponsive

	// ErrSyncError indicates that we failed synchronizing the state of the
	// channel with our peer.
	ErrSyncError

	// ErrInvalidUpdate indicates that the peer send us an invalid update.
	ErrInvalidUpdate

	// ErrInvalidCommitment indicates that the remote peer sent us an
	// invalid commitment signature.
	ErrInvalidCommitment

	// ErrInvalidRevocation indicates that the remote peer send us an
	// invalid revocation message.
	ErrInvalidRevocation

	// ErrRecoveryError the channel was unable to be resumed, we need the
	// remote party to force close the channel out on chain now as a
	// result.
	ErrRecoveryError

	// ErrCircuitError indicates a duplicate keystone error was hit in the
	// circuit map. This is non-fatal and will resolve itself (usually
	// within several minutes).
	ErrCircuitError

	// ErrStfuViolation indicates that the quiescence protocol has been
	// violated, either because Stfu has been sent/received at an invalid
	// time, or that an update has been sent/received while the channel is
	// quiesced.
	ErrStfuViolation

	// ErrInternalDBError indicates that we were unable to process a message
	// from our peer because our own database is in trouble. This is a local
	// infrastructure failure and not peer misbehavior, so we never report
	// it to the peer and we never force close because of it.
	ErrInternalDBError
)

// LinkFailureAction is an enum-like type that describes the action that should
// be taken in response to a link failure.
type LinkFailureAction uint8

const (
	// LinkFailureForceNone indicates no action is to be taken.
	LinkFailureForceNone LinkFailureAction = iota

	// LinkFailureForceClose indicates that the channel should be force
	// closed.
	LinkFailureForceClose

	// LinkFailureDisconnect indicates that we should disconnect in an
	// attempt to recycle the connection. This can be useful if we think a
	// TCP connection or state machine is stalled.
	LinkFailureDisconnect
)

// LinkFailureError encapsulates an error that will make us fail the current
// link. It contains the necessary information needed to determine if we should
// force close the channel in the process, and if any error data should be sent
// to the peer.
type LinkFailureError struct {
	// code is the type of error this LinkFailureError encapsulates.
	code errorCode

	// FailureAction describes what we should do to fail the channel.
	FailureAction LinkFailureAction

	// PermanentFailure indicates whether this failure is permanent, and
	// the channel should not be attempted loaded again.
	PermanentFailure bool

	// Warning denotes if this is a non-terminal error that doesn't warrant
	// failing the channel all together.
	Warning bool

	// SendData is a byte slice that will be sent to the peer. If nil a
	// generic error will be sent.
	SendData []byte
}

// A compile time check to ensure LinkFailureError implements the error
// interface.
var _ error = (*LinkFailureError)(nil)

// Error returns a generic error for the LinkFailureError.
//
// NOTE: Part of the error interface.
func (e LinkFailureError) Error() string {
	switch e.code {
	case ErrInternalError:
		return "internal error"
	case ErrRemoteError:
		return "remote error"
	case ErrRemoteUnresponsive:
		return "remote unresponsive"
	case ErrSyncError:
		return "sync error"
	case ErrInvalidUpdate:
		return "invalid update"
	case ErrInvalidCommitment:
		return "invalid commitment"
	case ErrInvalidRevocation:
		return "invalid revocation"
	case ErrRecoveryError:
		return "unable to resume channel, recovery required"
	case ErrCircuitError:
		return "non-fatal circuit map error"
	case ErrStfuViolation:
		return "quiescence protocol executed improperly"
	case ErrInternalDBError:
		return "internal database error"
	default:
		return "unknown error"
	}
}

// ShouldSendToPeer indicates whether we should send an error to the peer if
// the link fails with this LinkFailureError.
func (e LinkFailureError) ShouldSendToPeer() bool {
	switch e.code {

	// Since sending an error can lead some nodes to force close the
	// channel, create a whitelist of the failures we want to send so that
	// newly added error codes aren't automatically sent to the remote peer.
	case
		ErrInternalError,
		ErrRemoteError,
		ErrSyncError,
		ErrInvalidUpdate,
		ErrInvalidCommitment,
		ErrInvalidRevocation,
		ErrRecoveryError:

		return true

	// In all other cases we will not attempt to send our peer an error.
	default:
		return false
	}
}

// dbErrLinkFailure is the failure we use whenever a message from our peer
// could not be processed because of a local database error. We disconnect
// instead of failing the channel: once the connection is re-established, the
// channel reestablish flow resyncs both sides and the state transition that we
// couldn't persist is simply retried.
var dbErrLinkFailure = LinkFailureError{
	code:          ErrInternalDBError,
	FailureAction: LinkFailureDisconnect,
}

// linkFailureForDBErr returns the link failure that should be used to fail the
// link given the error that was hit while processing a message from our peer.
// If the error was caused by our own database rather than by the peer, we
// return a failure that neither reports anything to the peer nor force closes
// the channel. Otherwise the passed default failure is returned unchanged.
//
// This exists because a local database problem used to be translated into an
// lnwire.Error on the wire, which some peers answer by force closing the
// channel. Losing a channel to a transient database hiccup is never the right
// trade, see https://github.com/lightningnetwork/lnd/issues/10995.
func linkFailureForDBErr(err error,
	defaultFailure LinkFailureError) LinkFailureError {

	if !sqldb.IsInternalDBError(err) {
		return defaultFailure
	}

	return dbErrLinkFailure
}

const (
	// dbFailureEscalation is the number of database caused link failures we
	// tolerate within dbFailureWindow before we start logging about the
	// health of the database itself rather than about the individual links.
	dbFailureEscalation = 3

	// dbFailureWindow is how far apart two database caused link failures
	// can be before we stop considering them related.
	dbFailureWindow = 5 * time.Minute
)

// linkDBFailures counts the database caused link failures across all links of
// this daemon. Failing a link because of a database error is deliberately quiet
// on the wire, so without this the only trace of a sick database would be one
// error line per failed link, which reads exactly like a peer that keeps
// flapping. This exists to tell those two apart for an operator.
var linkDBFailures dbFailureTracker

// dbFailureTracker counts how often links have recently been failed because of
// a local database error.
type dbFailureTracker struct {
	mu sync.Mutex

	// count is the number of failures seen so far within the current
	// window.
	count int

	// lastSeen is when we recorded the most recent failure.
	lastSeen time.Time
}

// record notes another database caused link failure that happened at the given
// time, and returns the number of failures seen within the current window,
// including this one. Failures that are further apart than dbFailureWindow are
// treated as unrelated, and start a fresh window.
func (d *dbFailureTracker) record(now time.Time) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.lastSeen.IsZero() && now.Sub(d.lastSeen) > dbFailureWindow {
		d.count = 0
	}

	d.count++
	d.lastSeen = now

	return d.count
}

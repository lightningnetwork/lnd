package wtwire

import "fmt"

// ErrorCode represents a generic error code used when replying to watchtower
// clients. Specific reply messages may extend the ErrorCode primitive and add
// custom codes, so long as they don't collide with the generic error codes..
type ErrorCode uint16

const (
	// CodeOK signals that the request was successfully processed by the
	// watchtower.
	CodeOK ErrorCode = 0

	// CodeTemporaryFailure alerts the client that the watchtower is
	// temporarily unavailable, but that it may try again at a later time.
	CodeTemporaryFailure ErrorCode = 40

	// CodePermanentFailure alerts the client that the watchtower has
	// permanently failed, and further communication should be avoided.
	CodePermanentFailure ErrorCode = 50
)

// String returns a human-readable description of an ErrorCode.
func (c ErrorCode) String() string {
	switch c {
	case CodeOK:
		return "CodeOK"
	case CodeTemporaryFailure:
		return "CodeTemporaryFailure"
	case CodePermanentFailure:
		return "CodePermanentFailure"
	case CreateSessionCodeAlreadyExists:
		return "CreateSessionCodeAlreadyExists"
	case CreateSessionCodeRejectMaxUpdates:
		return "CreateSessionCodeRejectMaxUpdates"
	case CreateSessionCodeRejectRewardRate:
		return "CreateSessionCodeRejectRewardRate"
	case CreateSessionCodeRejectSweepFeeRate:
		return "CreateSessionCodeRejectSweepFeeRate"
	case CreateSessionCodeRejectBlobType:
		return "CreateSessionCodeRejectBlobType"
	case StateUpdateCodeClientBehind:
		return "StateUpdateCodeClientBehind"
	case StateUpdateCodeMaxUpdatesExceeded:
		return "StateUpdateCodeMaxUpdatesExceeded"
	case StateUpdateCodeSeqNumOutOfOrder:
		return "StateUpdateCodeSeqNumOutOfOrder"
	default:
		return fmt.Sprintf("UnknownErrorCode: %d", c)
	}
}

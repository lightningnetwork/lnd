package lnwire

import (
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// ErrorCode is an enum that defines the different types of error codes that
// are used to enrich the meaning of errors.
type ErrorCode uint16

const (
	// CodeMaxPendingChannels indicates that the number of active pending
	// channels exceeds their maximum policy limit.
	CodeMaxPendingChannels ErrorCode = 1

	// CodeSynchronizingChain indicates that the peer is still busy syncing
	// the latest state of the blockchain.
	CodeSynchronizingChain ErrorCode = 3

	// CodePendingHtlcCountExceeded indicates that the remote peer has tried
	// to add more htlcs that our local policy allows to a commitment.
	CodePendingHtlcCountExceeded ErrorCode = 5

	// CodePendingHtlcAmountExceeded indicates that the remote peer has
	// tried to add more than our pending amount in flight local policy
	// limit to a commitment.
	CodePendingHtlcAmountExceeded ErrorCode = 7

	// CodeInternalError indicates that something internal has failed, and
	// we do not want to provide our peer with further information.
	CodeInternalError ErrorCode = 9

	// CodeRemoteError indicates that our peer sent an error, prompting us
	// to fail the connection.
	CodeRemoteError ErrorCode = 11

	// CodeSyncError indicates that we failed synchronizing the state of the
	// channel with our peer.
	CodeSyncError ErrorCode = 13

	// CodeRecoveryError the channel was unable to be resumed, we need the
	// remote party to force close the channel out on chain now as a
	// result.
	CodeRecoveryError ErrorCode = 15

	// CodeInvalidUpdate indicates that the peer send us an invalid update.
	CodeInvalidUpdate ErrorCode = 17

	// CodeInvalidRevocation indicates that the remote peer send us an
	// invalid revocation message.
	CodeInvalidRevocation ErrorCode = 19
)

// Compile time assertion that CodedError implements the ExtendedError
// interface.
var _ ExtendedError = (*CodedError)(nil)

// CodedError is an error that has been enriched with an error code, and
// optional additional information.
type CodedError struct {
	// ErrorCode is the error code that defines the type of error this is.
	ErrorCode
}

// NewCodedError creates an error with the code provided.
func NewCodedError(e ErrorCode) *CodedError {
	return &CodedError{
		ErrorCode: e,
	}
}

// Error provides a string representation of a coded error.
func (e *CodedError) Error() string {
	var errStr string

	switch e.ErrorCode {
	case CodeMaxPendingChannels:
		errStr = "number of pending channels exceed maximum"

	case CodeSynchronizingChain:
		errStr = "synchronizing blockchain"

	case CodePendingHtlcCountExceeded:
		errStr = "commitment exceeds max htlcs"

	case CodePendingHtlcAmountExceeded:
		errStr = "commitment exceeds max in flight value"

	case CodeInternalError:
		errStr = "internal error"

	case CodeRemoteError:
		errStr = "remote error"

	case CodeSyncError:
		errStr = "sync error"

	case CodeRecoveryError:
		errStr = "unable to resume channel, recovery required"

	case CodeInvalidUpdate:
		errStr = "invalid update"

	case CodeInvalidRevocation:
		errStr = "invalid revocation"

	default:
		errStr = "unknown"
	}

	return fmt.Sprintf("Error code: %d: %v", e.ErrorCode, errStr)
}

// Record provides a tlv record for coded errors.
func (e *CodedError) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		typeErrorCode, e, e.sizeFunc, codedErrorEncoder,
		codedErrorDecoder,
	)
}

func (e *CodedError) sizeFunc() uint64 {
	return 2
}

func codedErrorEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	v, ok := val.(*CodedError)
	if ok {
		code := uint16(v.ErrorCode)
		if err := tlv.EUint16(w, &code, buf); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.CodedError")
}

func codedErrorDecoder(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	v, ok := val.(*CodedError)
	if ok {
		var errCode uint16
		if err := tlv.DUint16(r, &errCode, buf, l); err != nil {
			return err
		}

		*v = CodedError{
			ErrorCode: ErrorCode(errCode),
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.CodedError")
}

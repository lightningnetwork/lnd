package lnwire

import (
	"bytes"
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

	// ErrContext contains additional information used to enrich the error.
	ErrContext
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

	if e.ErrContext == nil {
		return fmt.Sprintf("Error code: %d: %v", e.ErrorCode, errStr)
	}

	return fmt.Sprintf("Error code: %d: %v: %v", e.ErrorCode, errStr,
		e.ErrContext)
}

// ErrContext is an interface implemented by coded errors with additional
// information about their error code.
type ErrContext interface {
	// Records returns a set of TLVs describing additional information
	// added to an error code.
	Records() []tlv.Record

	// String returns a string representing the error context.
	String() string
}

// knownErrorCodeContext maps known error codes to additional information that
// is included in tlvs.
var knownErrorCodeContext = map[ErrorCode]ErrContext{}

// Record provides a tlv record for coded errors.
func (e *CodedError) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		typeErrorCode, e, e.sizeFunc, codedErrorEncoder,
		codedErrorDecoder,
	)
}

func (e *CodedError) sizeFunc() uint64 {
	var (
		b   bytes.Buffer
		buf [8]byte
	)

	// TODO(carla): copied, maybe another way here, log error?
	if err := codedErrorEncoder(&b, e, &buf); err != nil {
		panic(fmt.Sprintf("coded error encoder failed: %v", err))
	}

	return uint64(len(b.Bytes()))
}

func codedErrorEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	v, ok := val.(*CodedError)
	if ok {
		code := uint16(v.ErrorCode)
		if err := tlv.EUint16(w, &code, buf); err != nil {
			return err
		}

		// If we have extra records present, we want to store them.
		// Even if records is empty, we continue with the nested record
		// encoding so that a 0 value length will be written.
		var records []tlv.Record
		if v.ErrContext != nil {
			records = v.Records()
		}

		// Create a tlv stream containing the nested tlvs.
		tlvStream, err := tlv.NewStream(records...)
		if err != nil {
			return err
		}

		// Encode the nested tlvs is a _separate_ buffer so that we
		// can get the length of our nested stream.
		var nestedBuffer bytes.Buffer
		if err := tlvStream.Encode(&nestedBuffer); err != nil {
			return err
		}

		// Write the length of the nested tlv stream to the main
		// buffer, followed by the actual values.
		nestedBytes := uint64(nestedBuffer.Len())
		if err := tlv.WriteVarInt(w, nestedBytes, buf); err != nil {
			return err
		}

		if _, err := w.Write(nestedBuffer.Bytes()); err != nil {
			return err
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.CodedError")
}

func codedErrorDecoder(r io.Reader, val interface{}, buf *[8]byte,
	_ uint64) error {

	v, ok := val.(*CodedError)
	if ok {
		var errCode uint16
		if err := tlv.DUint16(r, &errCode, buf, 2); err != nil {
			return err
		}

		errorCode := ErrorCode(errCode)
		*v = CodedError{
			ErrorCode: errorCode,
		}

		nestedLen, err := tlv.ReadVarInt(r, buf)
		if err != nil {
			return err
		}

		// If there are no nested fields, we don't need to ready any
		// further values.
		if nestedLen == 0 {
			return nil
		}

		// Using this information, we'll create a new limited
		// reader that'll return an EOF once the end has been
		// reached so the stream stops consuming bytes.
		//
		// TODO(carla): copied from #5803
		innerTlvReader := io.LimitedReader{
			R: r,
			N: int64(nestedLen),
		}

		// Lookup the records for this error code. If we don't know of
		// any additional records that are nested for this error code,
		// that's ok, we just don't read them (allowing forwards
		// compatibility for new fields).
		errContext, known := knownErrorCodeContext[errorCode]
		if !known {
			return nil
		}

		tlvStream, err := tlv.NewStream(errContext.Records()...)
		if err != nil {
			return err
		}

		if err := tlvStream.Decode(&innerTlvReader); err != nil {
			return err
		}

		*v = CodedError{
			ErrorCode:  errorCode,
			ErrContext: errContext,
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.CodedError")
}

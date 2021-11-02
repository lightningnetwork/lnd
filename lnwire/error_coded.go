package lnwire

import (
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// ErrorCode is an enum that defines the different types of error codes that
// are used to enrich the meaning of errors.
type ErrorCode uint16

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
	return fmt.Sprintf("Error code: %d", e.ErrorCode)
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

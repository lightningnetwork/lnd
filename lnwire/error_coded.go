package lnwire

import (
	"bytes"
	"encoding/binary"
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

	// CodeInvalidCommitSig indicates that we have received an invalid
	// commitment signature.
	CodeInvalidCommitSig ErrorCode = 21

	// CodeInvalidHtlcSig indicates that we have received an invalid htlc
	// signature.
	CodeInvalidHtlcSig ErrorCode = 23

	// CodeErroneousField indicates that a specific field is problematic.
	CodeErroneousField ErrorCode = 25
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

	// TODO(carla): better error string here using other info?
	case CodeInvalidCommitSig:
		errStr = "invalid commit sig"

	case CodeInvalidHtlcSig:
		errStr = "invalid htlc sig"

	case CodeErroneousField:
		errStr = "erroneous field"

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
var knownErrorCodeContext = map[ErrorCode]ErrContext{
	CodeInvalidCommitSig: &InvalidCommitSigError{},
	CodeInvalidHtlcSig:   &InvalidHtlcSigError{},
	CodeErroneousField:   &ErroneousFieldErr{},
}

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

// InvalidCommitSigError contains the error information we transmit upon
// receiving an invalid commit signature
type InvalidCommitSigError struct {
	commitHeight uint64
	commitSig    []byte
	sigHash      []byte
	commitTx     []byte
}

// A compile time flag to ensure that InvalidCommitSigError implements the
// ErrContext interface.
var _ ErrContext = (*InvalidCommitSigError)(nil)

// NewInvalidCommitSigError creates an invalid sig error.
func NewInvalidCommitSigError(commitHeight uint64, commitSig, sigHash,
	commitTx []byte) *CodedError {

	return &CodedError{
		ErrorCode: CodeInvalidCommitSig,
		ErrContext: &InvalidCommitSigError{
			commitHeight: commitHeight,
			commitSig:    commitSig,
			sigHash:      sigHash,
			commitTx:     commitTx,
		},
	}
}

// String returns a string representing an invalid sig error.
func (i *InvalidCommitSigError) String() string {
	return fmt.Sprintf("rejected commitment: commit_height=%v, "+
		"invalid_commit_sig=%x, commit_tx=%x, sig_hash=%x",
		i.commitHeight, i.commitSig, i.commitTx, i.sigHash)
}

// Records returns a set of record producers for the tlvs associated
// with an enriched error.
func (i *InvalidCommitSigError) Records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(typeNestedCommitHeight, &i.commitHeight),
		tlv.MakePrimitiveRecord(typeNestedCommitSig, &i.commitSig),
		tlv.MakePrimitiveRecord(typeNestedSigHash, &i.sigHash),
		tlv.MakePrimitiveRecord(typeNestedCommitTx, &i.commitTx),
	}
}

// InvalidHtlcSigError is a struct that implements the error interface to
// report a failure to validate an htlc signature from a remote peer. We'll use
// the items in this struct to generate a rich error message for the remote
// peer when we receive an invalid signature from it. Doing so can greatly aide
// in debugging across implementation issues.
type InvalidHtlcSigError struct {
	commitHeight uint64
	htlcSig      []byte
	htlcIndex    uint64
	sigHash      []byte
	commitTx     []byte
}

// A compile time flag to ensure that InvalidHtlcSigError implements the
// ErrContext interface.
var _ ErrContext = (*InvalidHtlcSigError)(nil)

// NewInvalidHtlcSigError creates an invalid htlc signature error.
func NewInvalidHtlcSigError(commitHeight, htlcIndex uint64, htlcSig, sigHash,
	commitTx []byte) *CodedError {

	return &CodedError{
		ErrorCode: CodeInvalidHtlcSig,
		ErrContext: &InvalidHtlcSigError{
			commitHeight: commitHeight,
			htlcIndex:    htlcIndex,
			htlcSig:      htlcSig,
			sigHash:      sigHash,
			commitTx:     commitTx,
		},
	}
}

// String returns a string representing an invalid htlc sig error.
func (i *InvalidHtlcSigError) String() string {
	return fmt.Sprintf("rejected commitment: commit_height=%v, "+
		"invalid_htlc_sig=%x, commit_tx=%x, sig_hash=%x",
		i.commitHeight, i.htlcSig, i.commitTx, i.sigHash)
}

// Records returns a set of record producers for the tlvs associated with
// an enriched error.
func (i *InvalidHtlcSigError) Records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(typeNestedCommitHeight, &i.commitHeight),
		tlv.MakePrimitiveRecord(typeNestedHtlcIndex, &i.htlcIndex),
		tlv.MakePrimitiveRecord(typeNestedHtlcSig, &i.htlcSig),
		tlv.MakePrimitiveRecord(typeNestedSigHash, &i.sigHash),
		tlv.MakePrimitiveRecord(typeNestedCommitTx, &i.commitTx),
	}
}

// ErroneousFieldErr is an error that indicates that a specific message's
// field value is problematic. It optionally includes a suggested value for the
// field.
type ErroneousFieldErr struct {
	erroneousField
	suggestedValue []byte
}

// A compile time flag to ensure that ErroneousFieldErr implements the
// ErrContext interface.
var _ ErrContext = (*ErroneousFieldErr)(nil)

// String returns a string representation of an erroneous field error.
func (e *ErroneousFieldErr) String() string {
	return fmt.Sprintf("Message: %v, field: %v problematic", e.messageType,
		e.fieldNumber)
}

// Records returns a set of TLVs describing additional information added to an
// error code.
func (e *ErroneousFieldErr) Records() []tlv.Record {
	records := []tlv.Record{
		e.erroneousField.Record(),
		tlv.MakePrimitiveRecord(
			typeNestedSuggestedValue, &e.suggestedValue,
		),
	}

	return records
}

type erroneousField struct {
	messageType MessageType
	fieldNumber uint16
	value       []byte
}

func (e *erroneousField) sizeFunc() uint64 {
	// Record size:
	// - 2 bytes message type
	// - 2 bytes field number
	// - n bytes: value
	return 2 + 2 + tlv.SizeVarBytes(&e.value)()
}

// Record creates a tlv record for an erroneous field.
func (e *erroneousField) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		typeNestedErroneousField, e, e.sizeFunc,
		encodeErroneousField, decodeErroneousField,
	)
}

// encodeErroneousField encodes the erroneous message type and field number
// in a single tlv record.
func encodeErroneousField(w io.Writer, val interface{}, buf *[8]byte) error {
	errField, ok := val.(*erroneousField)
	if !ok {
		return fmt.Errorf("expected erroneous field, got: %T",
			val)
	}

	msgNr := uint16(errField.messageType)
	if err := tlv.EUint16(w, &msgNr, buf); err != nil {
		return err
	}

	if err := tlv.EUint16(w, &errField.fieldNumber, buf); err != nil {
		return err
	}

	if err := tlv.EVarBytes(w, &errField.value, buf); err != nil {
		return err
	}

	return nil
}

// decodeErroneousField decodes an erroneous field tlv message type and field
// number. We can't use 2x tlv.DUint16 because these functions expect to only
// read 2 bytes from the reader, and we have 2x 2bytes concatenated here plus
// the var bytes value.
func decodeErroneousField(r io.Reader, val interface{}, buf *[8]byte,
	l uint64) error {

	errField, ok := val.(*erroneousField)
	if !ok {
		return fmt.Errorf("expected erroneous field, got: %T",
			val)
	}

	if l < 4 {
		return fmt.Errorf("expected at least 4 bytes for erroneous "+
			"field, got: %v", l)
	}

	n, err := r.Read(buf[:2])
	if err != nil {
		return err
	}
	if n != 2 {
		return fmt.Errorf("expected 2 bytes for message type, got: %v",
			n)
	}

	msgType := MessageType(binary.BigEndian.Uint16(buf[:2]))

	n, err = r.Read(buf[2:4])
	if err != nil {
		return err
	}
	if n != 2 {
		return fmt.Errorf("expected 2 bytes for field number, got: %v",
			n)
	}
	fieldNumber := binary.BigEndian.Uint16(buf[2:4])

	*errField = erroneousField{
		messageType: msgType,
		fieldNumber: fieldNumber,
	}

	// Now that we've read the first two elements out of the buffer, we can
	// read the var bytes value out using the standard tlv method, since
	// it's all that's left.
	if err := tlv.DVarBytes(r, &errField.value, buf, l-4); err != nil {
		return err
	}

	return nil
}

// errFieldHelper has the functionality we need to serialize and deserialize
// the erroneous/suggested value tlvs.
type errFieldHelper struct {
	fieldName string

	// decode provides typed read of a value which is stored as a byte
	// slice. This is required because we need to read into an interface
	// with an underlying type set, otherwise we don't know what type the
	// bytes represent.
	decode func([]byte) (interface{}, error)
}

// supportedErroneousFields contains a map of specification message types to
// helpers for each of the fields in that message for which we create errors.
// If a message/field combination is not included in this map, we do not know
// how to encode/decode it.
//
// Field number is defined as follows:
// * For fixed fields: 0-based index of the field as defined in #BOLT 1
// * For TLV fields: number of fixed fields + TLV field number
var supportedErroneousFields = map[MessageType]map[uint16]*errFieldHelper{}

// getFieldHelper looks up the helper struct for a message/ field combination
// in our map of known structured errors, returning a nil struct if the
// combination is unknown.
func getFieldHelper(errField erroneousField) *errFieldHelper {
	msgFields, ok := supportedErroneousFields[errField.messageType]
	if !ok {
		return nil
	}

	fieldHelper, ok := msgFields[errField.fieldNumber]
	if !ok {
		return nil
	}

	return fieldHelper
}

// NewErroneousFieldErr creates an error for message field that is incorrect.
// This function will panic if the message/field combination is not known to
// us, because we should not create errors that we don't know how to decode.
func NewErroneousFieldErr(messageType MessageType, fieldNumber uint16,
	erroneousValue, suggestedValue interface{}) *CodedError {

	// Panic on creation of unsupported errors because we expect them
	// to be added to our list of supported errors.
	errField := erroneousField{
		messageType: messageType,
		fieldNumber: fieldNumber,
	}

	fieldHelper := getFieldHelper(errField)
	if fieldHelper == nil {
		panic(fmt.Sprintf("Structured errors not supported for: %v "+
			"field: %v", messageType, fieldNumber))
	}

	badFieldErr := &ErroneousFieldErr{
		erroneousField: errField,
	}

	codedErr := &CodedError{
		ErrorCode:  CodeErroneousField,
		ErrContext: badFieldErr,
	}

	// Encode straight to bytes so that the tlv record can just encode/
	// decode var bytes rather than needing to know message type + field
	// in advance to parse the record.
	//
	// TODO(carla): how to handle this error?
	if erroneousValue != nil {
		var b bytes.Buffer
		if err := WriteElement(&b, erroneousValue); err != nil {
			panic(fmt.Sprintf("erroneous value write failed: %v",
				err))
		}

		badFieldErr.value = b.Bytes()
	}

	if suggestedValue != nil {
		var b bytes.Buffer
		if err := WriteElement(&b, suggestedValue); err != nil {
			panic(fmt.Sprintf("suggested value write failed: %v",
				err))
		}

		badFieldErr.suggestedValue = b.Bytes()
	}

	return codedErr
}

// ErroneousValue returns the erroneous value for an error. If the value is not
// set or the message type/ field number combination are unknown, a nil value
// will be returned.
func (e *ErroneousFieldErr) ErroneousValue() (interface{}, error) {
	if e.value == nil {
		return nil, nil
	}

	fieldHelper := getFieldHelper(e.erroneousField)
	if fieldHelper == nil {
		return nil, nil
	}

	return fieldHelper.decode(e.value)
}

// SuggestedValue returns the suggested value for an error. If the value is not
// set or the message type/ field number combination are unknown, a nil value
// will be returned.
func (e *ErroneousFieldErr) SuggestedValue() (interface{}, error) {
	if e.suggestedValue == nil {
		return nil, nil
	}

	fieldHelper := getFieldHelper(e.erroneousField)
	if fieldHelper == nil {
		return nil, nil
	}

	return fieldHelper.decode(e.suggestedValue)
}

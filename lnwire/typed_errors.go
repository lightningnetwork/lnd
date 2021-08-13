package lnwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// typeErroneousField contains to a message type, field number and
	// field value that cased an error.
	typeErroneousField tlv.Type = 1

	// typeSuggestedValue contains a suggested value for a field that
	// caused an error.
	typeSuggestedValue tlv.Type = 3

	// typeErrorCode contains an error code that is not related to a
	// specific message/field combination.
	typeErrorCode tlv.Type = 5
)

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

func decode32Byte(value []byte) (interface{}, error) {
	var (
		val [32]byte
		r   = bytes.NewBuffer(value)
	)

	if err := ReadElements(r, &val); err != nil {
		return nil, err
	}

	return val, nil
}

func decodeUint64(value []byte) (interface{}, error) {
	var (
		val uint64
		r   = bytes.NewBuffer(value)
	)

	if err := ReadElements(r, &val); err != nil {
		return nil, err
	}

	return val, nil
}

func decodeUint32(value []byte) (interface{}, error) {
	var (
		val uint32
		r   = bytes.NewBuffer(value)
	)

	if err := ReadElements(r, &val); err != nil {
		return nil, err
	}

	return val, nil
}

func decodeUint16(value []byte) (interface{}, error) {
	var (
		val uint16
		r   = bytes.NewBuffer(value)
	)

	if err := ReadElements(r, &val); err != nil {
		return nil, err
	}

	return val, nil
}

type erroneousField struct {
	messageType MessageType
	fieldNumber uint16
	value       []byte
}

// getFieldHelper looks up the helper struct for a message/ field combination
// in our map of known structured errors, returning a nil struct if the
// combination is unknown.
func getFieldHelper(errField erroneousField) *errFieldHelper {
	msgFields, ok := supportedStructuredError[errField.messageType]
	if !ok {
		return nil
	}

	fieldHelper, ok := msgFields[errField.fieldNumber]
	if !ok {
		return nil
	}

	return fieldHelper
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

// createErrFieldRecord creates a tlv record for our erroneous field.
func createErrFieldRecord(value *erroneousField) tlv.Record {
	// Record size:
	// 2 bytes message type
	// 2 bytes field number
	// var bytes value
	sizeFunc := func() uint64 {
		return 2 + 2 + tlv.SizeVarBytes(&value.value)()
	}

	return tlv.MakeDynamicRecord(
		typeErroneousField, value, sizeFunc,
		encodeErroneousField, decodeErroneousField,
	)
}

// supportedStructuredError contains a map of specification message types to
// helpers for each of the fields in that message for which we understand
// structured errors. If a message is not contained in this map, we do not
// understand structured errors for that message or field.
//
// Field number is defined as follows:
// * For fixed fields: 0-based index of the field as defined in #BOLT 1
// * For TLV fields: number of fixed fields + TLV field number
var supportedStructuredError = map[MessageType]map[uint16]*errFieldHelper{
	MsgOpenChannel: {
		0: {
			fieldName: "chain hash",
			decode:    decode32Byte,
		},
		1: {
			fieldName: "channel id",
			decode:    decode32Byte,
		},
		2: {
			fieldName: "funding sats",
			decode:    decodeUint64,
		},
		3: {
			fieldName: "push amount",
			decode:    decodeUint64,
		},
		4: {
			fieldName: "dust limit",
			decode:    decodeUint64,
		},
		5: {
			fieldName: "max htlc value in flight msat",
			decode:    decodeUint64,
		},
		6: {
			fieldName: "channel reserve",
			decode:    decodeUint64,
		},
		7: {
			fieldName: "htlc minimum msat",
			decode:    decodeUint64,
		},
		8: {
			fieldName: "feerate per kw",
			decode:    decodeUint32,
		},
		9: {
			fieldName: "to self delay",
			decode:    decodeUint16,
		},
		10: {
			fieldName: "max accepted htlcs",
			decode:    decodeUint16,
		},
	},
	MsgAcceptChannel: {
		5: {
			fieldName: "min depth",
			decode:    decodeUint32,
		},
	},
}

// Compile time assertion that StructuredError implements the error interface.
var _ error = (*StructuredError)(nil)

// StrucutredError contains structured error information for an error.
type StructuredError struct {
	erroneousField
	suggestedValue []byte
}

// ErroneousValue returns the erroneous value for an error. If the value is not
// set or the message type/ field number combination are unknown, a nil value
// will be returned.
func (s *StructuredError) ErroneousValue() (interface{}, error) {
	if s.value == nil {
		return nil, nil
	}

	fieldHelper := getFieldHelper(s.erroneousField)
	if fieldHelper == nil {
		return nil, nil
	}

	return fieldHelper.decode(s.value)
}

// SuggestedValue returns the suggested value for an error. If the value is not
// set or the message type/ field number combination are unknown, a nil value
// will be returned.
func (s *StructuredError) SuggestedValue() (interface{}, error) {
	if s.suggestedValue == nil {
		return nil, nil
	}

	fieldHelper := getFieldHelper(s.erroneousField)
	if fieldHelper == nil {
		return nil, nil
	}

	return fieldHelper.decode(s.suggestedValue)
}

// Error returns an error string for our structured errors, including the
// suggested value if it is present.
func (s *StructuredError) Error() string {
	errStr := fmt.Sprintf("Message: %v failed", s.messageType)

	// Include field name in our error string if we know it.
	helper := getFieldHelper(s.erroneousField)
	if helper == nil {
		return fmt.Sprintf("%v, field: %v", errStr, s.fieldNumber)
	}

	errStr = fmt.Sprintf("%v, field: %v (%v)", errStr, helper.fieldName,
		s.fieldNumber)

	if s.value != nil {
		errStr = fmt.Sprintf("%v, erroneous value: %v", errStr,
			s.value)
	}

	if s.suggestedValue != nil {
		errStr = fmt.Sprintf("%v, suggested value: %v", errStr,
			s.suggestedValue)
	}

	return errStr
}

// NewStructuredError creates a structured error containing information about
// the field we have a problem with.
func NewStructuredError(messageType MessageType, fieldNumber uint16,
	erroneousValue, suggestedValue interface{}) *StructuredError {

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

	structuredErr := &StructuredError{
		erroneousField: errField,
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

		structuredErr.value = b.Bytes()
	}

	if suggestedValue != nil {
		var b bytes.Buffer
		if err := WriteElement(&b, suggestedValue); err != nil {
			panic(fmt.Sprintf("suggested value write failed: %v",
				err))
		}

		structuredErr.suggestedValue = b.Bytes()
	}

	return structuredErr
}

// ToWireError creates an error containing TLV fields that are used to point
// the recipient towards problematic field values.
func (s *StructuredError) ToWireError(chanID ChannelID) (*Error, error) {
	resp := &Error{
		ChanID: chanID,
		Data:   ErrorData(s.Error()),
	}

	records := []tlv.Record{
		createErrFieldRecord(&s.erroneousField),
	}

	if s.suggestedValue != nil {
		record := tlv.MakePrimitiveRecord(
			typeSuggestedValue, &s.suggestedValue,
		)

		records = append(records, record)
	}

	if err := resp.ExtraData.PackRecords(records...); err != nil {
		return nil, err
	}

	return resp, nil
}

// CodedError is a structured error that relies on an error code to provide
// additional information about an error.
type CodedError uint8

// Compile time check that CodedError implements error.
var _ error = (*CodedError)(nil)

// Error returns an error string for a coded error.
func (c CodedError) Error() string {
	return fmt.Sprintf("Coded error: %d", c)
}

// ToWireError returns a wire error with our error code packed into the
// ExtraData field.
func (c CodedError) ToWireError(chanID ChannelID) (*Error, error) {
	resp := &Error{
		ChanID: chanID,
		Data:   ErrorData(c.Error()),
	}

	errCode := uint8(c)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(typeErrorCode, &errCode),
	}

	if err := resp.ExtraData.PackRecords(records...); err != nil {
		return nil, err
	}

	return resp, nil

}

// StructuredErrorFromWire extracts a structured error from our error's extra
// data, if present.
func StructuredErrorFromWire(err *Error) (error, error) {
	if err == nil {
		return nil, nil
	}

	if len(err.ExtraData) == 0 {
		return nil, nil
	}

	// First we try to extract our message and field number records.
	var (
		structuredErr = &StructuredError{}
		codedErr      uint8
	)

	records := []tlv.Record{
		createErrFieldRecord(&structuredErr.erroneousField),
		tlv.MakePrimitiveRecord(
			typeSuggestedValue, &structuredErr.suggestedValue,
		),
		tlv.MakePrimitiveRecord(
			typeErrorCode, &codedErr,
		),
	}

	tlvs, extractErr := err.ExtraData.ExtractRecords(records...)
	if extractErr != nil {
		return nil, extractErr
	}

	// If we have the error code TLV, we don't expect any other fields so
	// we just return a coded error using the value.
	if _, ok := tlvs[typeErrorCode]; ok {
		return CodedError(codedErr), nil
	}

	// If we don't know the problematic message type and field, we can't
	// add any additional information to this error.
	if _, ok := tlvs[typeErroneousField]; !ok {
		return nil, nil
	}

	return structuredErr, nil
}

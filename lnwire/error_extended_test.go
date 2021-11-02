package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExtendedError tests packing and extracting of additional information in
// error message TLVs.
func TestExtendedError(t *testing.T) {
	// Create a test error code that we'll pack into our error, and a fake
	// channel ID for our tests.
	var (
		testErrCode = ErrorCode(999)
		testChanID  = ChannelID([32]byte{1, 2, 3})
	)

	codedErr := NewCodedError(testErrCode)

	// Assert that we can pack this coded error into a wire error.
	wireErr, err := WireErrorFromExtended(codedErr, testChanID)
	require.NoError(t, err)

	// Assert that we can extract our error code from the wire error we
	// just packed.
	actual, err := ExtendedErrorFromWire(wireErr)
	require.NoError(t, err)

	actualCoded, ok := actual.(*CodedError)
	require.True(t, ok)
	require.Equal(t, codedErr, actualCoded)

	// Create a wire error that does not have any additional information.
	legacyErr := &Error{
		ChanID: testChanID,
	}
	empty, err := ExtendedErrorFromWire(legacyErr)
	require.NoError(t, err)
	require.Nil(t, empty)

	// Next, we create an invalid commit sig error which has additional
	// information attached to it, and test that we can pack and unpack it.
	invalidCommit := NewInvalidCommitSigError(
		1, []byte{1}, []byte{2}, []byte{3},
	)

	wireErr, err = WireErrorFromExtended(invalidCommit, testChanID)
	require.NoError(t, err)

	// Assert that when we extract the tlv records for this error, they are
	// the same as the ones that we originally packed.
	actual, err = ExtendedErrorFromWire(wireErr)
	require.NoError(t, err)

	actualCoded, ok = actual.(*CodedError)
	require.True(t, ok)
	require.Equal(t, invalidCommit, actualCoded)

	// Now we test the class of errors that just indicate that a message/
	// field combination are undesirable.
	var (
		errValue       = uint32(100)
		suggestedValue = uint32(101)

		helper = &errFieldHelper{
			fieldName: "uint32",
			decode: func(value []byte) (interface{}, error) {
				var (
					val uint32
					r   = bytes.NewBuffer(value)
				)

				if err := ReadElements(r, &val); err != nil {
					return nil, err
				}

				return val, nil
			},
		}

		knownField uint16 = 2
	)

	// Populate our lookup map so that we can encode/decode full values.
	supportedErroneousFields = map[MessageType]map[uint16]*errFieldHelper{
		MsgOpenChannel: {
			knownField: helper,
		},
	}

	// Create an error where we know the message/field combination.
	allFieldsKnown := NewErroneousFieldErr(
		MsgOpenChannel, knownField, errValue, suggestedValue,
	)

	// Start by encoding an error that we know all the fields for.
	wireErr, err = WireErrorFromExtended(allFieldsKnown, testChanID)
	require.Nil(t, err)

	// Retrieve a structured error from the encoded error and assert equal.
	actual, err = ExtendedErrorFromWire(wireErr)
	require.Nil(t, err)

	actualCoded, ok = actual.(*CodedError)
	require.True(t, ok)
	require.Equal(t, allFieldsKnown, actualCoded)

	// Access the fields and assert that we get our uint32 values again.
	errValues, ok := actualCoded.ErrContext.(*ErroneousFieldErr)
	require.True(t, ok)

	decodedErrVal, err := errValues.ErroneousValue()
	require.NoError(t, err)
	require.Equal(t, errValue, decodedErrVal)

	decodedSuggestedVal, err := errValues.SuggestedValue()
	require.NoError(t, err)
	require.Equal(t, suggestedValue, decodedSuggestedVal)

	// Now we create an error that we don't know the message type for.
	// Pack records manually because we're testing a case where our own
	// packing would fail because we don't know the message type.
	unknownMessage := &CodedError{
		ErrorCode: CodeErroneousField,
		ErrContext: &ErroneousFieldErr{
			erroneousField: erroneousField{
				messageType: 999,
				fieldNumber: 1,
				value:       []byte{1},
			},
			suggestedValue: []byte{2},
		},
	}

	// Manually pack so that we can test unknown decode.
	wireErr, err = WireErrorFromExtended(unknownMessage, testChanID)
	require.NoError(t, err)

	actual, err = ExtendedErrorFromWire(wireErr)
	require.NoError(t, err)

	actualCoded, ok = actual.(*CodedError)
	require.True(t, ok)
	require.Equal(t, unknownMessage, actualCoded)

	// Access the value fields and assert that we get nil value because we
	// don't know this message type.
	errValues, ok = actualCoded.ErrContext.(*ErroneousFieldErr)
	require.True(t, ok)

	decodedErrVal, err = errValues.ErroneousValue()
	require.NoError(t, err)
	require.Nil(t, decodedErrVal)

	decodedSuggestedVal, err = errValues.SuggestedValue()
	require.NoError(t, err)
	require.Nil(t, decodedSuggestedVal)
}

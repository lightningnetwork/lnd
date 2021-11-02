package lnwire

import (
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
}

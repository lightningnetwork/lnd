package tlv_test

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestVarBytesCompatibility checks empty values, non-P2P values above the
// P2P limit, and the boundary between consecutive records.
func TestVarBytesCompatibility(t *testing.T) {
	t.Parallel()

	sizes := []int{0, 1, tlv.MaxRecordSize, tlv.MaxRecordSize + 1}
	for _, size := range sizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			value := bytes.Repeat([]byte{42}, size)
			var scratch [8]byte
			var decoded []byte
			reader := bytes.NewReader(append(value, 99))
			err := tlv.DVarBytes(
				reader, &decoded, &scratch, uint64(size),
			)
			require.NoError(t, err)
			require.NotNil(t, decoded)
			require.Equal(t, value, decoded)
			require.Equal(t, 1, reader.Len())

			var encoded bytes.Buffer
			stream := tlv.MustNewStream(
				tlv.MakePrimitiveRecord(1, &value),
			)
			require.NoError(t, stream.Encode(&encoded))
			unknown := tlv.MustNewStream()
			parsed, err := unknown.DecodeWithParsedTypes(
				bytes.NewReader(encoded.Bytes()),
			)
			require.NoError(t, err)
			require.NotNil(t, parsed[1])
			require.Equal(t, value, parsed[1])

			_, err = unknown.DecodeWithParsedTypesP2P(
				bytes.NewReader(encoded.Bytes()),
			)
			if size > tlv.MaxRecordSize {
				require.ErrorIs(t, err, tlv.ErrRecordTooLarge)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

type finalErrorReader struct {
	err error
}

func (r finalErrorReader) Read(p []byte) (int, error) {
	p[0] = 1
	return 1, r.err
}

// TestVarBytesReaderErrors preserves io.ReadFull's EOF and reader error
// semantics when a read supplies only part of the declared value.
func TestVarBytesReaderErrors(t *testing.T) {
	t.Parallel()

	wrappedEOF := fmt.Errorf("reader failed: %w", io.EOF)
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"EOF", io.EOF, io.ErrUnexpectedEOF},
		{"wrapped EOF", wrappedEOF, wrappedEOF},
		{"reader error", io.ErrClosedPipe, io.ErrClosedPipe},
	} {
		t.Run(test.name, func(t *testing.T) {
			var decoded []byte
			var scratch [8]byte
			err := tlv.DVarBytes(
				finalErrorReader{err: test.err}, &decoded,
				&scratch, 2,
			)
			require.ErrorIs(t, err, test.want)
		})
	}
}

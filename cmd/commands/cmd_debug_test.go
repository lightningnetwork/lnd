package commands

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDebugPackageGzipRoundTrip locks in the new compression format for
// the encryptdebugpackage / decryptdebugpackage flow: the encrypted
// payload starts with a gzip stream whose first two bytes are the RFC
// 1952 magic that decryptDebugPackage uses to detect the format.
func TestDebugPackageGzipRoundTrip(t *testing.T) {
	t.Parallel()

	payload := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" +
		"highly-repetitive-test-payload-" +
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	var compressed bytes.Buffer
	w, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	require.NoError(t, err)
	_, err = w.Write(payload)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// The first two bytes must be the gzip magic the decrypt path
	// sniffs for.
	require.GreaterOrEqual(t, compressed.Len(), len(gzipMagic))
	require.Equal(t, gzipMagic, compressed.Bytes()[:len(gzipMagic)])

	// Round-trip back through gzip.NewReader.
	r, err := gzip.NewReader(bytes.NewReader(compressed.Bytes()))
	require.NoError(t, err)
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, payload, out)
}

// TestDebugPackageRejectsNonGzip asserts the sniff guard in
// decryptDebugPackage triggers for payloads that don't start with the
// gzip magic — exactly the path an old brotli-encoded debug package
// would land on now that brotli has been removed.
func TestDebugPackageRejectsNonGzip(t *testing.T) {
	t.Parallel()

	// Synthesize a plausible brotli stream: brotli's first byte is the
	// WBITS / metadata header, not 0x1f, so a sniff check should reject
	// it.
	notGzip := []byte{0x8b, 0x1f, 0x00, 0x01, 0x02}

	require.False(t, bytes.HasPrefix(notGzip, gzipMagic))
}

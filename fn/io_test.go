package fn

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	data1 = "The network is robust in its unstructured simplicity."
	data2 = "Programming today is a race between software engineers " +
		"striving to build bigger and better idiot-proof programs, " +
		"and the Universe trying to produce bigger and better " +
		"idiots. So far, the Universe is winning."
)

// TestWriteFile uses same scenario of ioutil asserting the content created and
// stored on new file with the original one.
func TestWriteFile(t *testing.T) {
	t.Parallel()

	f, deferred := ensureTempfile(t)
	filename := f.Name()
	defer deferred()

	// Write file.
	err := WriteFile(filename, []byte(data2), 0644, KeepFileOnErr)
	require.NoError(t, err, "couldn't write file")
	ensureFileContents(t, filename, data2)

	// Overwrite file.
	err = WriteFile(filename, []byte(data1), 0644, KeepFileOnErr)
	require.NoError(t, err, "couldn't overwrite file")
	ensureFileContents(t, filename, data1)

	// Change file permission to read-only.
	err = os.Chmod(filename, 0444)
	require.NoError(t, err, "couldn't change file to read-only")

	// Write must fail.
	err = WriteFile(filename, []byte(data2), 0644, KeepFileOnErr)
	require.Error(t, err, "shouldn't write a read-only file")

	_, err = os.Stat(filename)
	require.NoError(t, err, "shouldn't remove file on error")
	ensureFileContents(t, filename, data1)
}

func ensureTempfile(t *testing.T) (*os.File, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "fn-io-test-TestWriteFile")
	require.NoError(t, err, "couldn't create temporary file")

	return f, func() {
		f.Close()
		os.Remove(f.Name())
	}
}

func ensureFileContents(t *testing.T, filename string, data string) {
	t.Helper()
	contents, err := os.ReadFile(filename)
	require.NoError(t, err, "couldn't read file")

	require.Equal(t, data, string(contents))
}

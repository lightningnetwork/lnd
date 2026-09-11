package build

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRotatingLogWriterConcurrentWrites verifies parallel callers are
// serialized through the rotator's pipe.
func TestRotatingLogWriterConcurrentWrites(t *testing.T) {
	t.Parallel()

	const testLogFileSize = 1000 * 1024

	logPath := filepath.Join(t.TempDir(), "lnd.log")
	logConfig := DefaultLogConfig().File
	logConfig.MaxLogFileSize = 1

	logWriter := NewRotatingLogWriter()
	require.NoError(t, logWriter.InitLogRotator(logConfig, logPath))

	logLine := []byte(strings.Repeat("a", testLogFileSize/2-1) + "\n")
	start := make(chan struct{})
	writeErrs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := logWriter.Write(logLine)
			writeErrs <- err
		}()
	}
	close(start)

	for range 2 {
		require.NoError(t, <-writeErrs)
	}
	require.NoError(t, logWriter.Close())
	require.NoError(t, logWriter.Close())

	backupFiles, err := filepath.Glob(logPath + ".*")
	require.NoError(t, err)
	require.Equal(t, []string{logPath + ".1.gz"}, backupFiles)

	backupFile, err := os.Open(backupFiles[0])
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, backupFile.Close())
	})

	gzipReader, err := gzip.NewReader(backupFile)
	require.NoError(t, err)
	backupBytes, err := io.ReadAll(gzipReader)
	require.NoError(t, err)
	require.NoError(t, gzipReader.Close())
	require.Len(t, backupBytes, testLogFileSize)
}

// +build stdlog

package build

import "os"

// LoggingType is a log type that only writes to stdout.
const LoggingType = LogTypeStdOut

// Write writes the provided byte slice to stdout.
func (w *LogWriter) Write(b []byte) (int, error) {
	os.Stdout.Write(b)
	return len(b), nil
}

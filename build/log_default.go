// +build !stdlog,!nolog

package build

import "os"

// LoggingType is a log type that writes to both stdout and the log rotator, if
// present.
const LoggingType = LogTypeDefault

// Write writes the byte slice to both stdout and the log rotator, if present.
func (w *LogWriter) Write(b []byte) (int, error) {
	os.Stdout.Write(b)
	if w.RotatorPipe != nil {
		w.RotatorPipe.Write(b)
	}
	return len(b), nil
}

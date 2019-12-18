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

		// A new line is appended to the first byte slice that is logged. This is required
		// so that logging starts on a new line on restart, rather than appending to the
		// last log written
		w.startNewLine.Do(func() {
			b = []byte("\n" + string(b))
		})

		w.RotatorPipe.Write(b)
	}
	return len(b), nil
}

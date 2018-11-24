// +build nolog

package build

// LoggingType is a log type that writes no logs.
const LoggingType = LogTypeNone

// Write is a noop.
func (w *LogWriter) Write(b []byte) (int, error) {
	return len(b), nil
}

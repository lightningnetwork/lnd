//go:build !stdlog && !nolog
// +build !stdlog,!nolog

package build

// LoggingType is a log type that writes to both stdout and the log rotator, if
// present.
const LoggingType = LogTypeDefault

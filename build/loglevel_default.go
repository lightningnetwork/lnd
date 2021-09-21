//go:build !info && !debug && !trace && !warn && !error && !critical && !off
// +build !info,!debug,!trace,!warn,!error,!critical,!off

package build

// LogLevel specifies a default log level of info.
var LogLevel = "info"

package signal

import "fmt"

// The exit codes below are reported to the parent process when lnd stops. They
// let process managers such as systemd tell a deliberate stop apart from one
// forced by a failure, and identify which failure it was, without parsing the
// logs. A unit configured with Restart=on-failure only restarts lnd for a
// non-zero code, and RestartPreventExitStatus can exclude specific codes.
//
// Health check codes start at 10 to stay clear of the codes used by the Go
// runtime and by shells: 2 is an unrecovered panic or fatal runtime error.
//
// A health check code is only reported when that check gave up, so it always
// names the real cause. The reverse does not hold: the failure a check exists
// to catch can surface elsewhere first, for instance when the chain backend
// goes away between two check intervals and a component that depends on it
// raises a critical error. lnd then exits with ExitCodeCriticalError, so that
// code does not rule out a cause that has its own code.

// ExitCode is the status lnd reports to the parent process when it stops.
type ExitCode int

const (
	// ExitCodeSuccess is used when lnd was stopped deliberately, either by
	// an OS signal or by a StopDaemon RPC call (lncli stop).
	ExitCodeSuccess ExitCode = 0

	// ExitCodeCriticalError is used when lnd stopped because of a critical
	// error that is not covered by a more specific code below. Startup and
	// configuration failures also exit with this code.
	ExitCodeCriticalError ExitCode = 1

	// ExitCodeChainBackend is used when the chain backend health check
	// failed.
	ExitCodeChainBackend ExitCode = 10

	// ExitCodeDiskSpace is used when the disk space health check failed.
	ExitCodeDiskSpace ExitCode = 11

	// ExitCodeTLSCert is used when the TLS certificate health check
	// failed.
	ExitCodeTLSCert ExitCode = 12

	// ExitCodeTorConnection is used when the tor connection health check
	// failed.
	ExitCodeTorConnection ExitCode = 13

	// ExitCodeRemoteSigner is used when the remote signer connection
	// health check failed.
	ExitCodeRemoteSigner ExitCode = 14

	// ExitCodeLeaderStatus is used when the leader status health check
	// failed.
	ExitCodeLeaderStatus ExitCode = 15
)

// String returns the reason an exit code stands for, so log lines and error
// messages can name it without the reader looking up the number.
func (c ExitCode) String() string {
	switch c {
	case ExitCodeSuccess:
		return "stop requested"

	case ExitCodeCriticalError:
		return "critical error or startup failure"

	case ExitCodeChainBackend:
		return "chain backend health check failed"

	case ExitCodeDiskSpace:
		return "disk space health check failed"

	case ExitCodeTLSCert:
		return "TLS certificate health check failed"

	case ExitCodeTorConnection:
		return "tor connection health check failed"

	case ExitCodeRemoteSigner:
		return "remote signer health check failed"

	case ExitCodeLeaderStatus:
		return "leader status health check failed"

	default:
		return fmt.Sprintf("unknown exit code %d", int(c))
	}
}

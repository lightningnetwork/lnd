package lntest

import (
	"flag"
	"fmt"
	"net"
	"sync/atomic"
)

const (
	// defaultNodePort is the start of the range for listening ports of
	// harness nodes. Ports are monotonically increasing starting from this
	// number and are determined by the results of nextAvailablePort().
	defaultNodePort = 5555

	// listenerFormat is the format string that is used to generate local
	// listener addresses.
	listenerFormat = "127.0.0.1:%d"

	// NeutrinoBackendName is the name of the neutrino backend.
	NeutrinoBackendName = "neutrino"
)

type DatabaseBackend int

const (
	BackendBbolt DatabaseBackend = iota
	BackendEtcd
	BackendPostgres
)

var (
	// lastPort is the last port determined to be free for use by a new
	// node. It should be used atomically.
	lastPort uint32 = defaultNodePort

	// logOutput is a flag that can be set to append the output from the
	// seed nodes to log files.
	logOutput = flag.Bool("logoutput", false,
		"log output from node n to file output-n.log")

	// logSubDir is the default directory where the logs are written to if
	// logOutput is true.
	logSubDir = flag.String("logdir", ".", "default dir to write logs to")

	// goroutineDump is a flag that can be set to dump the active
	// goroutines of test nodes on failure.
	goroutineDump = flag.Bool("goroutinedump", false,
		"write goroutine dump from node n to file pprof-n.log")

	// btcdExecutable is the full path to the btcd binary.
	btcdExecutable = flag.String(
		"btcdexec", "", "full path to btcd binary",
	)
)

// NextAvailablePort returns the first port that is available for listening by
// a new node. It panics if no port is found and the maximum available TCP port
// is reached.
func NextAvailablePort() int {
	port := atomic.AddUint32(&lastPort, 1)
	for port < 65535 {
		// If there are no errors while attempting to listen on this
		// port, close the socket and return it as available. While it
		// could be the case that some other process picks up this port
		// between the time the socket is closed and it's reopened in
		// the harness node, in practice in CI servers this seems much
		// less likely than simply some other process already being
		// bound at the start of the tests.
		addr := fmt.Sprintf(listenerFormat, port)
		l, err := net.Listen("tcp4", addr)
		if err == nil {
			err := l.Close()
			if err == nil {
				return int(port)
			}
		}
		port = atomic.AddUint32(&lastPort, 1)
	}

	// No ports available? Must be a mistake.
	panic("no ports available for listening")
}

// ApplyPortOffset adds the given offset to the lastPort variable, making it
// possible to run the tests in parallel without colliding on the same ports.
func ApplyPortOffset(offset uint32) {
	_ = atomic.AddUint32(&lastPort, offset)
}

// GetLogDir returns the passed --logdir flag or the default value if it wasn't
// set.
func GetLogDir() string {
	if logSubDir != nil && *logSubDir != "" {
		return *logSubDir
	}
	return "."
}

// GetBtcdBinary returns the full path to the binary of the custom built btcd
// executable or an empty string if none is set.
func GetBtcdBinary() string {
	if btcdExecutable != nil {
		return *btcdExecutable
	}

	return ""
}

// GenerateBtcdListenerAddresses is a function that returns two listener
// addresses with unique ports and should be used to overwrite rpctest's
// default generator which is prone to use colliding ports.
func GenerateBtcdListenerAddresses() (string, string) {
	return fmt.Sprintf(listenerFormat, NextAvailablePort()),
		fmt.Sprintf(listenerFormat, NextAvailablePort())
}

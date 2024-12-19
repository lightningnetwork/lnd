package port

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/lntest/wait"
)

const (
	// ListenerFormat is the format string that is used to generate local
	// listener addresses.
	ListenerFormat = "127.0.0.1:%d"

	// defaultNodePort is the start of the range for listening ports of
	// harness nodes. Ports are monotonically increasing starting from this
	// number and are determined by the results of NextAvailablePort().
	defaultNodePort int = 10000

	// uniquePortFile is the name of the file that is used to store the
	// last port that was used by a node. This is used to make sure that
	// the same port is not used by multiple nodes at the same time. The
	// file is located in the temp directory of a system.
	uniquePortFile = "rpctest-port"
)

var (
	// portFileMutex is a mutex that is used to make sure that the port file
	// is not accessed by multiple goroutines of the same process at the
	// same time. This is used in conjunction with the lock file to make
	// sure that the port file is not accessed by multiple processes at the
	// same time either. So the lock file is to guard between processes and
	// the mutex is to guard between goroutines of the same process.
	portFileMutex sync.Mutex
)

// NextAvailablePort returns the first port that is available for listening by a
// new node, using a lock file to make sure concurrent access for parallel tasks
// on the same system don't re-use the same port.
func NextAvailablePort() int {
	portFileMutex.Lock()
	defer portFileMutex.Unlock()

	lockFile := filepath.Join(os.TempDir(), uniquePortFile+".lock")
	timeout := time.After(wait.DefaultTimeout)

	var (
		lockFileHandle *os.File
		err            error
	)
	for {
		// Attempt to acquire the lock file. If it already exists, wait
		// for a bit and retry.
		lockFileHandle, err = os.OpenFile(
			lockFile, os.O_CREATE|os.O_EXCL, 0600,
		)
		if err == nil {
			// Lock acquired.
			break
		}

		// Wait for a bit and retry.
		select {
		case <-timeout:
			panic("timeout waiting for lock file")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Release the lock file when we're done.
	defer func() {
		// Always close file first, Windows won't allow us to remove it
		// otherwise.
		_ = lockFileHandle.Close()
		err := os.Remove(lockFile)
		if err != nil {
			panic(fmt.Errorf("couldn't remove lock file: %w", err))
		}
	}()

	portFile := filepath.Join(os.TempDir(), uniquePortFile)
	port, err := os.ReadFile(portFile)
	if err != nil {
		if !os.IsNotExist(err) {
			panic(fmt.Errorf("error reading port file: %w", err))
		}
		port = []byte(strconv.Itoa(defaultNodePort))
	}

	lastPort, err := strconv.Atoi(string(port))
	if err != nil {
		panic(fmt.Errorf("error parsing port: %w", err))
	}

	// We take the next one.
	lastPort++
	for lastPort < 65535 {
		// If there are no errors while attempting to listen on this
		// port, close the socket and return it as available. While it
		// could be the case that some other process picks up this port
		// between the time the socket is closed, and it's reopened in
		// the harness node, in practice in CI servers this seems much
		// less likely than simply some other process already being
		// bound at the start of the tests.
		addr := fmt.Sprintf(ListenerFormat, lastPort)
		l, err := net.Listen("tcp4", addr)
		if err == nil {
			err := l.Close()
			if err == nil {
				err := os.WriteFile(
					portFile,
					[]byte(strconv.Itoa(lastPort)), 0600,
				)
				if err != nil {
					panic(fmt.Errorf("error updating "+
						"port file: %w", err))
				}

				return lastPort
			}
		}
		lastPort++

		// Start from the beginning if we reached the end of the port
		// range. We need to do this because the lock file now is
		// persistent across runs on the same machine during the same
		// boot/uptime cycle. So in order to make this work on
		// developer's machines, we need to reset the port to the
		// default value when we reach the end of the range.
		if lastPort == 65535 {
			lastPort = defaultNodePort
		}
	}

	// No ports available? Must be a mistake.
	panic("no ports available for listening")
}

// GenerateSystemUniqueListenerAddresses is a function that returns two
// listener addresses with unique ports per system and should be used to
// overwrite rpctest's default generator which is prone to use colliding ports.
func GenerateSystemUniqueListenerAddresses() (string, string) {
	port1 := NextAvailablePort()
	port2 := NextAvailablePort()
	return fmt.Sprintf(ListenerFormat, port1),
		fmt.Sprintf(ListenerFormat, port2)
}

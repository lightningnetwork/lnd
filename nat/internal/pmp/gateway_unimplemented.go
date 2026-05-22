//go:build !linux && !darwin && !windows && !freebsd && !openbsd && !netbsd && !dragonfly && !solaris

package pmp

import (
	"fmt"
	"net"
	"runtime"
)

// DiscoverGateway returns an error on platforms where lnd does not know
// how to read the system route table. Callers can still construct a
// Client directly if they obtain the gateway IP some other way.
func DiscoverGateway() (net.IP, error) {
	return nil, fmt.Errorf("DiscoverGateway not implemented for OS %s",
		runtime.GOOS)
}

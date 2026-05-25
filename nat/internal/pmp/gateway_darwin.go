//go:build darwin

package pmp

import (
	"net"
	"os/exec"
)

// DiscoverGateway returns the IPv4 default gateway on macOS by parsing
// `route -n get 0.0.0.0` output.
func DiscoverGateway() (net.IP, error) {
	out, err := exec.Command(
		"/sbin/route", "-n", "get", "0.0.0.0",
	).CombinedOutput()
	if err != nil {
		return nil, err
	}
	return parseDarwinRouteGet(out)
}

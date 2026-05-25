//go:build freebsd || openbsd || netbsd || dragonfly || solaris

package pmp

import (
	"net"
	"os/exec"
)

// DiscoverGateway returns the IPv4 default gateway on the BSD family +
// Solaris by parsing `netstat -rn` output.
func DiscoverGateway() (net.IP, error) {
	out, err := exec.Command("netstat", "-rn").CombinedOutput()
	if err != nil {
		return nil, err
	}
	return parseBSDSolarisNetstat(out)
}

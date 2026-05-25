//go:build windows

package pmp

import (
	"net"
	"os/exec"
	"syscall"
)

// DiscoverGateway returns the IPv4 default gateway on Windows by parsing
// `route print 0.0.0.0` output. HideWindow is set so we do not flash a
// console window on GUI builds.
func DiscoverGateway() (net.IP, error) {
	cmd := exec.Command("route", "print", "0.0.0.0")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	return parseWindowsRoutePrint(out)
}

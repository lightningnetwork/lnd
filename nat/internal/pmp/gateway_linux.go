//go:build linux

package pmp

import (
	"net"
	"os/exec"
)

// DiscoverGateway returns the IPv4 default gateway by shelling out to the
// best available route-table tool. The same fallback ladder is preserved
// from upstream jackpal/gateway: `route -n` first (universally available
// on older distros), then iproute2 `ip route show`, then `ip route get`.
func DiscoverGateway() (net.IP, error) {
	if ip, err := discoverViaRoute(); err == nil {
		return ip, nil
	}
	if ip, err := discoverViaIPRouteShow(); err == nil {
		return ip, nil
	}
	return discoverViaIPRouteGet()
}

func discoverViaRoute() (net.IP, error) {
	out, err := exec.Command("route", "-n").CombinedOutput()
	if err != nil {
		return nil, err
	}
	return parseLinuxRoute(out)
}

func discoverViaIPRouteShow() (net.IP, error) {
	out, err := exec.Command("ip", "route", "show").CombinedOutput()
	if err != nil {
		return nil, err
	}
	return parseLinuxIPRouteShow(out)
}

func discoverViaIPRouteGet() (net.IP, error) {
	out, err := exec.Command(
		"ip", "route", "get", "8.8.8.8",
	).CombinedOutput()
	if err != nil {
		return nil, err
	}
	return parseLinuxIPRouteGet(out)
}

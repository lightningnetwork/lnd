package pmp

import (
	"errors"
	"net"
	"strings"
)

// errNoGateway is returned by every per-OS DiscoverGateway implementation
// when the platform's route table did not yield a usable default route.
var errNoGateway = errors.New("no gateway found")

// parseLinuxIPRouteShow parses the output of `ip route show`. Format:
//
//	default via 192.168.178.1 dev wlp3s0 metric 303
//	192.168.178.0/24 dev wlp3s0 proto kernel ...
func parseLinuxIPRouteShow(output []byte) (net.IP, error) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "default" {
			if ip := net.ParseIP(fields[2]); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errNoGateway
}

// parseLinuxIPRouteGet parses the output of `ip route get 8.8.8.8`.
// Format:
//
//	8.8.8.8 via 10.0.1.1 dev eth0 src 10.0.1.36 uid 2000
func parseLinuxIPRouteGet(output []byte) (net.IP, error) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == "via" {
			if ip := net.ParseIP(fields[2]); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errNoGateway
}

// parseLinuxRoute parses the legacy `route -n` output. Format:
//
//	Destination     Gateway         Genmask         Flags ...
//	0.0.0.0         192.168.1.1     0.0.0.0         UG    ...
func parseLinuxRoute(output []byte) (net.IP, error) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "0.0.0.0" {
			if ip := net.ParseIP(fields[1]); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errNoGateway
}

// parseDarwinRouteGet parses `route -n get 0.0.0.0`. Format:
//
//	   route to: default
//	destination: default
//	       mask: default
//	    gateway: 192.168.1.1
func parseDarwinRouteGet(output []byte) (net.IP, error) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "gateway:" {
			if ip := net.ParseIP(fields[1]); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errNoGateway
}

// parseBSDSolarisNetstat parses `netstat -rn` output. Format:
//
//	Destination        Gateway            Flags      Netif Expire
//	default            10.88.88.2         UGS         em0
func parseBSDSolarisNetstat(output []byte) (net.IP, error) {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "default" {
			if ip := net.ParseIP(fields[1]); ip != nil {
				return ip, nil
			}
		}
	}
	return nil, errNoGateway
}

// parseWindowsRoutePrint parses `route print 0.0.0.0`. Format:
//
//	Active Routes:
//	Network Destination        Netmask          Gateway       Interface  Metric
//	          0.0.0.0          0.0.0.0      192.168.1.1    192.168.1.100     20
func parseWindowsRoutePrint(output []byte) (net.IP, error) {
	lines := strings.Split(string(output), "\n")
	for idx, line := range lines {
		if !strings.HasPrefix(line, "Active Routes:") {
			continue
		}
		if len(lines) <= idx+2 {
			return nil, errNoGateway
		}
		fields := strings.Fields(lines[idx+2])
		if len(fields) < 3 {
			return nil, errNoGateway
		}
		if ip := net.ParseIP(fields[2]); ip != nil {
			return ip, nil
		}
	}
	return nil, errNoGateway
}

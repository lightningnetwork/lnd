package tor

import (
	"fmt"
	"net"
	"strings"
)

var defaultNoProxyTargets = []string{
	"localhost",
	"127.0.0.0/8",
	"::1/128",
}

var defaultBypassMatcher = func() *bypassMatcher {
	matcher, err := newBypassMatcher(nil)
	if err != nil {
		panic(err)
	}

	return matcher
}()

// bypassMatcher determines whether a destination should be connected to
// directly instead of through a proxy.
type bypassMatcher struct {
	hosts    map[string]struct{}
	zones    []string
	ips      []net.IP
	networks []*net.IPNet
}

// newBypassMatcher validates the configured bypass targets and constructs a
// matcher. The loopback defaults are always included.
func newBypassMatcher(targets []string) (*bypassMatcher, error) {
	m := &bypassMatcher{
		hosts: make(map[string]struct{}),
	}

	allTargets := make([]string, 0, len(defaultNoProxyTargets)+len(targets))
	allTargets = append(allTargets, defaultNoProxyTargets...)
	allTargets = append(allTargets, targets...)

	seen := make(map[string]struct{}, len(allTargets))
	for _, target := range allTargets {
		if err := m.addTarget(target, seen); err != nil {
			return nil, err
		}
	}

	return m, nil
}

// addTarget validates and adds one exact host, wildcard DNS zone, IP address,
// or CIDR network.
func (m *bypassMatcher) addTarget(target string,
	seen map[string]struct{}) error {

	if target == "" || strings.TrimSpace(target) != target {
		return fmt.Errorf("invalid proxy bypass target %q", target)
	}

	if strings.ContainsAny(target, "/") {
		_, network, err := net.ParseCIDR(target)
		if err != nil {
			return fmt.Errorf("invalid proxy bypass CIDR %q: %w",
				target, err)
		}

		key := "cidr:" + network.String()
		if _, ok := seen[key]; ok {
			return nil
		}

		seen[key] = struct{}{}
		m.networks = append(m.networks, network)

		return nil
	}

	if ip := net.ParseIP(target); ip != nil {
		key := "ip:" + ip.String()
		if _, ok := seen[key]; ok {
			return nil
		}

		seen[key] = struct{}{}
		m.ips = append(m.ips, ip)

		return nil
	}

	zone := strings.HasPrefix(target, "*.")
	host := target
	if zone {
		host = strings.TrimPrefix(target, "*.")
	}
	if err := validateProxyHost(host); err != nil {
		return fmt.Errorf("invalid proxy bypass target %q: %w",
			target, err)
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if zone {
		key := "zone:" + host
		if _, ok := seen[key]; ok {
			return nil
		}

		seen[key] = struct{}{}
		m.zones = append(m.zones, host)

		return nil
	}

	key := "host:" + host
	if _, ok := seen[key]; ok {
		return nil
	}

	seen[key] = struct{}{}
	m.hosts[host] = struct{}{}

	return nil
}

// validateProxyHost validates a DNS host without performing a lookup.
func validateProxyHost(host string) error {
	if host == "" || len(host) > 253 {
		return fmt.Errorf("invalid host length")
	}
	if strings.ContainsAny(host, ":*[]") {
		return fmt.Errorf("host must not contain a port or wildcard")
	}

	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' ||
			label[len(label)-1] == '-' {

			return fmt.Errorf("invalid DNS label %q", label)
		}

		for _, char := range label {
			valid := char >= 'a' && char <= 'z' ||
				char >= 'A' && char <= 'Z' ||
				char >= '0' && char <= '9' || char == '-'
			if !valid {
				return fmt.Errorf("invalid character %q", char)
			}
		}
	}

	return nil
}

// Match reports whether the host matches a configured bypass target.
func (m *bypassMatcher) Match(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if _, ok := m.hosts[host]; ok {
		return true
	}

	if ip := net.ParseIP(host); ip != nil {
		for _, exactIP := range m.ips {
			if exactIP.Equal(ip) {
				return true
			}
		}
		for _, network := range m.networks {
			if network.Contains(ip) {
				return true
			}
		}

		return false
	}

	for _, zone := range m.zones {
		if len(host) > len(zone) && strings.HasSuffix(host, "."+zone) {
			return true
		}
	}

	return false
}

//go:build !monitoring
// +build !monitoring

package lncfg

// Prometheus configures the Prometheus exporter when monitoring is enabled.
// Monitoring is currently disabled.
type Prometheus struct{}

// DefaultPrometheus is the default configuration for the Prometheus metrics
// exporter when monitoring is enabled. Monitoring is currently disabled.
func DefaultPrometheus() Prometheus {
	return Prometheus{}
}

// Enabled returns whether or not Prometheus monitoring is enabled. Monitoring
// is currently disabled, so Enabled will always return false.
func (p *Prometheus) Enabled() bool {
	return false
}

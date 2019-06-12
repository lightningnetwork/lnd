// +build monitoring

package lncfg

// Prometheus is the set of configuration data that specifies the listening
// address of the Prometheus exporter.
type Prometheus struct {
	// Listen is the listening address that we should use to allow the main
	// Prometheus server to scrape our metrics.
	Listen string `long:"listen" description:"the interface we should listen on for Prometheus"`

	// Enable indicates whether to export lnd gRPC performance metrics to
	// Prometheus. Default is false.
	Enable bool `long:"enable" description:"enable Prometheus exporting of lnd gRPC performance metrics."`
}

// DefaultPrometheus is the default configuration for the Prometheus metrics
// exporter.
func DefaultPrometheus() Prometheus {
	return Prometheus{
		Listen: "127.0.0.1:8989",
		Enable: false,
	}
}

// Enabled returns whether or not Prometheus monitoring is enabled. Monitoring
// is disabled by default, but may be enabled by the user.
func (p *Prometheus) Enabled() bool {
	return p.Enable
}

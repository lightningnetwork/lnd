//go:build monitoring
// +build monitoring

package lncfg

// Prometheus is the set of configuration data that specifies the listening
// address of the Prometheus exporter.
//
//nolint:ll
type Prometheus struct {
	// Listen is the listening address that we should use to allow the main
	// Prometheus server to scrape our metrics.
	Listen string `long:"listen" description:"the interface we should listen on for Prometheus"`

	// Enable indicates whether to export lnd gRPC performance metrics to
	// Prometheus. Default is false.
	Enable bool `long:"enable" description:"enable Prometheus exporting of lnd gRPC performance metrics."`

	// PerfHistograms indicates if the additional histogram information for
	// latency, and handling time of gRPC calls should be enabled. This
	// generates additional data, and consume more memory for the
	// Prometheus server.
	PerfHistograms bool `long:"perfhistograms" description:"enable additional histogram to track gRPC call processing performance (latency, etc)"`
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

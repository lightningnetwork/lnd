//go:build monitoring
// +build monitoring

package monitoring

import (
	"net/http"
	"sync"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
)

var started sync.Once

// GetPromInterceptors returns the set of interceptors for Prometheus
// monitoring.
func GetPromInterceptors() ([]grpc.UnaryServerInterceptor, []grpc.StreamServerInterceptor) {
	unaryInterceptors := []grpc.UnaryServerInterceptor{
		grpc_prometheus.UnaryServerInterceptor,
	}
	streamInterceptors := []grpc.StreamServerInterceptor{
		grpc_prometheus.StreamServerInterceptor,
	}
	return unaryInterceptors, streamInterceptors
}

// ExportPrometheusMetrics sets server options, registers gRPC metrics and
// launches the Prometheus exporter on the specified address.
func ExportPrometheusMetrics(grpcServer *grpc.Server, cfg lncfg.Prometheus) error {
	started.Do(func() {
		log.Infof("Prometheus exporter started on %v/metrics", cfg.Listen)

		grpc_prometheus.Register(grpcServer)

		// Enable the histograms which can allow plotting latency
		// distributions of inbound calls. However we guard this behind
		// another flag as this can generate a lot of additional data,
		// as its a high cardinality metric typically.
		if cfg.PerfHistograms {
			grpc_prometheus.EnableHandlingTimeHistogram()
		}

		http.Handle("/metrics", promhttp.Handler())
		go func() {
			http.ListenAndServe(cfg.Listen, nil)
		}()
	})

	return nil
}

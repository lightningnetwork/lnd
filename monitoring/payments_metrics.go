//go:build monitoring
// +build monitoring

package monitoring

import (
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/prometheus/client_golang/prometheus"
)

const paymentsMetricGroupName string = "payments"

// paymentMetrics tracks payment-related Prometheus metrics.
type paymentMetrics struct {
	totalPayments     prometheus.Counter
	totalHTLCAttempts prometheus.Counter
}

// NewPaymentMetrics creates a new instance of payment metrics.
func NewPaymentMetrics(cfg *lncfg.Prometheus) (MetricGroup, error) {
	return &paymentMetrics{
		totalPayments: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lnd_total_payments",
			Help: "Total number of payments initiated",
		}),
		totalHTLCAttempts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "lnd_total_htlc_attempts",
			Help: "Total number of HTLC attempts",
		}),
	}, nil
}

// Name returns the metric group name.
func (p *paymentMetrics) Name() string {
	return paymentsMetricGroupName
}

// RegisterMetrics registers payment-related Prometheus metrics.
func (p *paymentMetrics) RegisterMetrics() error {
	err := prometheus.Register(p.totalPayments)
	if err != nil {
		return err
	}
	return prometheus.Register(p.totalHTLCAttempts)
}

// ShouldRegister indicates whether the payments related metrics should be
// registered with prometheus.
func (p *paymentMetrics) ShouldRegister(cfg *lncfg.Prometheus) bool {
	// TODO: Can condition this on application config.
	return true
}

// IncrementPaymentCount increments the counter tracking the number of payments
// made by lnd when monitoring is enabled and the metric is configured
func IncrementPaymentCount() {
	group, ok := activeMetrics[paymentsMetricGroupName].(*paymentMetrics)
	if !ok {
		return
	}

	group.totalPayments.Inc()
}

// IncrementHTLCAttemptCount increments the counter tracking the number of HTLC
// attempts made by lnd when monitoring is enabled and the metric is configured.
func IncrementHTLCAttemptCount() {
	group, ok := activeMetrics[paymentsMetricGroupName].(*paymentMetrics)
	if !ok {
		return
	}

	group.totalHTLCAttempts.Inc()
}

// Register the payments metric group.
func init() {
	RegisterMetricGroup(paymentsMetricGroupName, NewPaymentMetrics)
}

//go:build monitoring
// +build monitoring

package monitoring

import (
	"sync"

	"github.com/lightningnetwork/lnd/lncfg"
)

// MetricGroupCreator is a factory function that initializes a metric group.
type MetricGroupCreator func(cfg *lncfg.Prometheus) (MetricGroup, error)

// Global registry for all metric groups.
var (
	// metricGroups is a global variable of all registered metrics
	// protected by the mutex below. All new MetricGroups should add
	// themselves to this map within the init() method of their file.
	metricGroups = make(map[string]MetricGroupCreator)

	// activeGroups is a global map of all active metric groups. This can
	// be used by some of the "static' package level methods to look up the
	// target metric group to export observations.
	activeMetrics = make(map[string]MetricGroup)

	metricsMtx sync.Mutex
)

// MetricGroup is the primary interface for metric groups.
type MetricGroup interface {
	// Name returns the name of the metric group.
	Name() string

	// RegisterMetrics registers all metrics within the group.
	RegisterMetrics() error

	// ShouldRegister indicates whether this groups metrics should actually
	// be registered.
	ShouldRegister(cfg *lncfg.Prometheus) bool
}

// RegisterMetricGroup adds a new metric group to the registry.
func RegisterMetricGroup(name string, creator MetricGroupCreator) {
	metricsMtx.Lock()
	defer metricsMtx.Unlock()
	metricGroups[name] = creator
}

// InitializeMetrics initializes and registers all active metric groups.
func InitializeMetrics(cfg *lncfg.Prometheus) error {
	metricsMtx.Lock()
	defer metricsMtx.Unlock()

	for name, creator := range metricGroups {
		// We'll pass the configuration struct to permit conditional
		// metric registration in the future.
		group, err := creator(cfg)
		if err != nil {
			return err
		}

		// Check whether this metric group should be registered.
		if !group.ShouldRegister(cfg) {
			continue
		}

		if err := group.RegisterMetrics(); err != nil {
			return err
		}

		activeMetrics[name] = group
	}

	return nil
}

package routing

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Estimator estimates the probability to reach a node.
type Estimator interface {
	// PairProbability estimates the probability of successfully traversing
	// to toNode based on historical payment outcomes for the from node.
	// Those outcomes are passed in via the results parameter.
	PairProbability(now time.Time, results NodeResults,
		toNode route.Vertex, amt lnwire.MilliSatoshi,
		capacity btcutil.Amount) float64

	// LocalPairProbability estimates the probability of successfully
	// traversing our own local channels to toNode.
	LocalPairProbability(now time.Time, results NodeResults,
		toNode route.Vertex) float64

	// Config returns the estimator's configuration.
	Config() estimatorConfig

	// String returns the string representation of the estimator's
	// configuration.
	String() string
}

// estimatorConfig represents a configuration for a probability estimator.
type estimatorConfig interface {
	// validate checks that all configuration parameters are sane.
	validate() error
}

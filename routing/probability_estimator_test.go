package routing

import (
	"math"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/routing/route"
)

// Create a set of test results.
var resultTime = time.Unix(1674169200, 0) // 20.01.2023
var now = time.Unix(1674190800, 0)        // 6 hours later
var results = NodeResults{
	route.Vertex{byte(0)}: TimedPairResult{
		FailAmt:     200_000_000,
		FailTime:    resultTime,
		SuccessAmt:  100_000_000,
		SuccessTime: resultTime,
	},
	route.Vertex{byte(1)}: TimedPairResult{
		FailAmt:     200_000_000,
		FailTime:    resultTime,
		SuccessAmt:  100_000_000,
		SuccessTime: resultTime,
	},
	route.Vertex{byte(2)}: TimedPairResult{
		FailAmt:     200_000_000,
		FailTime:    resultTime,
		SuccessAmt:  100_000_000,
		SuccessTime: resultTime,
	},
	route.Vertex{byte(3)}: TimedPairResult{
		FailAmt:     200_000_000,
		FailTime:    resultTime,
		SuccessAmt:  100_000_000,
		SuccessTime: resultTime,
	},
	route.Vertex{byte(4)}: TimedPairResult{
		FailAmt:     200_000_000,
		FailTime:    resultTime,
		SuccessAmt:  100_000_000,
		SuccessTime: resultTime,
	},
}

// probability is a package level variable to prevent the compiler from
// optimizing the benchmark.
var probability float64

// BenchmarkBimodalPairProbability benchmarks the probability calculation.
func BenchmarkBimodalPairProbability(b *testing.B) {
	estimator := BimodalEstimator{
		BimodalConfig: BimodalConfig{
			BimodalScaleMsat:  scale,
			BimodalNodeWeight: 0.2,
			BimodalDecayTime:  48 * time.Hour,
		},
	}

	toNode := route.Vertex{byte(0)}
	var p float64
	for i := 0; i < b.N; i++ {
		p = estimator.PairProbability(now, results, toNode,
			150_000_000, 300_000)
	}
	probability = p
}

// BenchmarkAprioriPairProbability benchmarks the probability calculation.
func BenchmarkAprioriPairProbability(b *testing.B) {
	estimator := AprioriEstimator{
		AprioriConfig: AprioriConfig{
			AprioriWeight:         0.2,
			PenaltyHalfLife:       48 * time.Hour,
			AprioriHopProbability: 0.5,
		},
	}

	toNode := route.Vertex{byte(0)}
	var p float64
	for i := 0; i < b.N; i++ {
		p = estimator.PairProbability(now, results, toNode,
			150_000_000, 300_000)
	}
	probability = p
}

// BenchmarkExp benchmarks the exponential function as provided by the math
// library.
func BenchmarkExp(b *testing.B) {
	for i := 0; i < b.N; i++ {
		math.Exp(0.1)
	}
}

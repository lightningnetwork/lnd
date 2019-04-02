package routing

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
)

func TestMissionControl(t *testing.T) {
	now := testTime

	mc := newMissionControl(nil, nil, nil)
	mc.now = func() time.Time { return now }
	mc.hardPruneDuration = 5 * time.Minute
	mc.edgeDecay = time.Hour
	mc.vertexDecay = 2 * time.Hour

	testNode := Vertex{}
	testEdge := EdgeLocator{
		ChannelID: 123,
	}

	amt := lnwire.NewMSatFromSatoshis(1000)
	capacity := btcutil.Amount(5000)

	expectP := func(expected float64) {
		p := mc.getEdgeProbability(testNode, amt, testEdge, capacity)
		if p != expected {
			t.Fatalf("unexpected probability %v", p)
		}
	}

	expectP(0.8)

	// Expect probability to be zero after reporting the edge as failed.
	mc.reportEdgeFailure(&testEdge)
	expectP(0)

	// Still in the hard prune window.
	now = testTime.Add(5 * time.Minute)
	expectP(0)

	// Edge decay started.
	now = testTime.Add(20 * time.Minute)
	expectP(0.2)

	// Edge fully decayed and probability back up to the a priori value.
	now = testTime.Add(65 * time.Minute)
	expectP(0.8)
}

type testChannelConfig struct {
	capacity   btcutil.Amount
	minBalance btcutil.Amount
	maxBalance btcutil.Amount
}

func TestMissionControlDynamic(t *testing.T) {
	// Make test deterministic.
	rand.Seed(0)

	// Configure several channels for which mission control is going to
	// provide probability estimates. The balance in a channel is simulated
	// as a random value between minBalance and maxBalance.
	channels := []*testChannelConfig{
		&testChannelConfig{
			capacity:   100000,
			minBalance: 0,
			maxBalance: 20000,
		},
		&testChannelConfig{
			capacity:   100000,
			minBalance: 40000,
			maxBalance: 60000,
		},
		&testChannelConfig{
			capacity:   100000,
			minBalance: 80000,
			maxBalance: 100000,
		},
		&testChannelConfig{
			capacity:   300000,
			minBalance: 0,
			maxBalance: 300000,
		},
		&testChannelConfig{
			capacity:   300000,
			minBalance: 0,
			maxBalance: 100000,
		},
	}

	// Use a set of fixed amounts to choose from for every payment.
	amts := []btcutil.Amount{10000, 50000, 100000, 150000}

	// Setup mock wall clock time.
	now := testTime
	mc := newMissionControl(nil, nil, nil)
	mc.now = func() time.Time { return now }

	testNode := Vertex{}

	const (
		totalIterations  = 15000
		loggedIterations = 10
	)

	iter := 0
	successes := 0

	// Simulate payments and keep track of successes.
	for iter < totalIterations {

		// Only log details for the first iterations. Useful for
		// debugging this test.
		if iter == loggedIterations {
			DisableLog()
			fmt.Printf("Disabling logging for remaining %v iterations\n",
				totalIterations-iter)
		}
		print := func(format string, a ...interface{}) (n int, err error) {
			if iter >= loggedIterations {
				return 0, nil
			}
			return fmt.Printf(format, a...)
		}

		// Pick a random payment amount.
		amt := amts[rand.Intn(len(amts))]
		print("Iteration %v: amt=%d, time=%v\n", iter, amt, now)

		// Determine channel with highest success probability.
		var best int
		var bestProbability float64
		for i, channel := range channels {
			// Skip channels that do not have enough capacity. In
			// normal path finding those channels are skipped too.
			if amt > channel.capacity {
				continue
			}

			// Retrieve success probability from mission control.
			edge := EdgeLocator{
				ChannelID: uint64(i),
			}
			p := mc.getEdgeProbability(
				testNode, lnwire.NewMSatFromSatoshis(amt), edge,
				channel.capacity,
			)

			print("   chan %v: cap=%d, probability=%v\n",
				i, channel.capacity, p)

			// Update best channel.
			if p >= bestProbability {
				best = i
				bestProbability = p
			}
		}

		// For the best channel, determine a simulated balance.
		bc := channels[best]
		balance := bc.minBalance + btcutil.Amount(
			rand.Int63n(int64(bc.maxBalance-bc.minBalance)),
		)

		print("   highest probability: id=%v, balance=%d / %d, p: %v\n",
			best, balance, bc.capacity, bestProbability)

		// Check balance to see if this payment would succeed or not. If
		// not, report the edge to mission control so that its
		// probability can be updated.
		if balance >= amt {
			successes++
			print("   success\n")
		} else {
			mc.reportEdgeFailure(&EdgeLocator{
				ChannelID: uint64(best),
			})
			print("   fail\n")
		}

		// Let an imaginary minute go by.
		now = now.Add(time.Minute)

		iter++
	}

	// Assert that the success rate in this scenario is above the expected
	// value.
	successRate := float64(successes) / float64(iter)
	fmt.Printf("Success rate: %f\n", successRate)
	if successRate < 0.7 {
		t.Fatal("unexpectedly low mission control performance")
	}
}

package sweep

import (
	"sort"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// DefaultFeeRateBucketSize is the default size of fee rate buckets
	// we'll use when clustering inputs into buckets with similar fee rates
	// within the SimpleAggregator.
	//
	// Given a minimum relay fee rate of 1 sat/vbyte, a multiplier of 10
	// would result in the following fee rate buckets up to the maximum fee
	// rate:
	//
	//   #1: min = 1 sat/vbyte, max = 10 sat/vbyte
	//   #2: min = 11 sat/vbyte, max = 20 sat/vbyte...
	DefaultFeeRateBucketSize = 10
)

// inputSet is a set of inputs that can be used as the basis to generate a tx
// on.
type inputSet []input.Input

// Cluster defines an interface that prepares inputs of a cluster to be grouped
// into a list of sets that can be used to create sweep transactions.
type Cluster interface {
	// CreateInputSets goes through the cluster's inputs and constructs
	// sets of inputs that can be used to generate a sensible transaction.
	CreateInputSets(wallet Wallet, maxFeeRate chainfee.SatPerKWeight,
		maxInputs int) ([]inputSet, error)

	// FeeRate returns the fee rate of the cluster.
	FeeRate() chainfee.SatPerKWeight
}

// Compile-time constraint to ensure inputCluster implements Cluster.
var _ Cluster = (*inputCluster)(nil)

// inputCluster is a helper struct to gather a set of pending inputs that
// should be swept with the specified fee rate.
type inputCluster struct {
	lockTime     *uint32
	sweepFeeRate chainfee.SatPerKWeight
	inputs       pendingInputs
}

// FeeRate returns the fee rate of the cluster.
func (c *inputCluster) FeeRate() chainfee.SatPerKWeight {
	return c.sweepFeeRate
}

// GroupInputs goes through the cluster's inputs and constructs sets of inputs
// that can be used to generate a sensible transaction. Each set contains up to
// the configured maximum number of inputs. Negative yield inputs are skipped.
// No input sets with a total value after fees below the dust limit are
// returned.
func (c *inputCluster) CreateInputSets(
	wallet Wallet, maxFeeRate chainfee.SatPerKWeight,
	maxInputs int) ([]inputSet, error) {

	// Turn the inputs into a slice so we can sort them.
	inputList := make([]*pendingInput, 0, len(c.inputs))
	for _, input := range c.inputs {
		inputList = append(inputList, input)
	}

	// Yield is calculated as the difference between value and added fee
	// for this input. The fee calculation excludes fee components that are
	// common to all inputs, as those wouldn't influence the order. The
	// single component that is differentiating is witness size.
	//
	// For witness size, the upper limit is taken. The actual size depends
	// on the signature length, which is not known yet at this point.
	calcYield := func(input *pendingInput) int64 {
		size, _, err := input.WitnessType().SizeUpperBound()
		if err != nil {
			log.Errorf("Failed to get input weight: %v", err)

			return 0
		}

		yield := input.SignDesc().Output.Value -
			int64(c.sweepFeeRate.FeeForWeight(int64(size)))

		return yield
	}

	// Sort input by yield. We will start constructing input sets starting
	// with the highest yield inputs. This is to prevent the construction
	// of a set with an output below the dust limit, causing the sweep
	// process to stop, while there are still higher value inputs
	// available. It also allows us to stop evaluating more inputs when the
	// first input in this ordering is encountered with a negative yield.
	sort.Slice(inputList, func(i, j int) bool {
		// Because of the specific ordering and termination condition
		// that is described above, we place force sweeps at the start
		// of the list. Otherwise we can't be sure that they will be
		// included in an input set.
		if inputList[i].parameters().Force {
			return true
		}

		return calcYield(inputList[i]) > calcYield(inputList[j])
	})

	// Select blocks of inputs up to the configured maximum number.
	var sets []inputSet
	for len(inputList) > 0 {
		// Start building a set of positive-yield tx inputs under the
		// condition that the tx will be published with the specified
		// fee rate.
		txInputs := newTxInputSet(
			wallet, c.sweepFeeRate, maxFeeRate, maxInputs,
		)

		// From the set of sweepable inputs, keep adding inputs to the
		// input set until the tx output value no longer goes up or the
		// maximum number of inputs is reached.
		txInputs.addPositiveYieldInputs(inputList)

		// If there are no positive yield inputs, we can stop here.
		inputCount := len(txInputs.inputs)
		if inputCount == 0 {
			return sets, nil
		}

		// Check the current output value and add wallet utxos if
		// needed to push the output value to the lower limit.
		if err := txInputs.tryAddWalletInputsIfNeeded(); err != nil {
			return nil, err
		}

		// If the output value of this block of inputs does not reach
		// the dust limit, stop sweeping. Because of the sorting,
		// continuing with the remaining inputs will only lead to sets
		// with an even lower output value.
		if !txInputs.enoughInput() {
			// The change output is always a p2tr here.
			dl := lnwallet.DustLimitForSize(input.P2TRSize)
			log.Debugf("Input set value %v (required=%v, "+
				"change=%v) below dust limit of %v",
				txInputs.totalOutput(), txInputs.requiredOutput,
				txInputs.changeOutput, dl)
			return sets, nil
		}

		log.Infof("Candidate sweep set of size=%v (+%v wallet inputs),"+
			" has yield=%v, weight=%v",
			inputCount, len(txInputs.inputs)-inputCount,
			txInputs.totalOutput()-txInputs.walletInputTotal,
			txInputs.weightEstimate(true).weight())

		sets = append(sets, txInputs.inputs)
		inputList = inputList[inputCount:]
	}

	return sets, nil
}

// UtxoAggregator defines an interface that takes a list of inputs and
// aggregate them into groups. Each group is used as the inputs to create a
// sweeping transaction.
type UtxoAggregator interface {
	// ClusterInputs takes a list of inputs and groups them into clusters.
	ClusterInputs(pendingInputs) []Cluster
}

// SimpleAggregator aggregates inputs known by the Sweeper based on each
// input's locktime and feerate.
type SimpleAggregator struct {
	// FeeEstimator is used when crafting sweep transactions to estimate
	// the necessary fee relative to the expected size of the sweep
	// transaction.
	FeeEstimator chainfee.Estimator

	// MaxFeeRate is the maximum fee rate allowed within the
	// SimpleAggregator.
	MaxFeeRate chainfee.SatPerKWeight

	// FeeRateBucketSize is the default size of fee rate buckets we'll use
	// when clustering inputs into buckets with similar fee rates within
	// the SimpleAggregator.
	//
	// Given a minimum relay fee rate of 1 sat/vbyte, a fee rate bucket
	// size of 10 would result in the following fee rate buckets up to the
	// maximum fee rate:
	//
	//   #1: min = 1 sat/vbyte, max (exclusive) = 11 sat/vbyte
	//   #2: min = 11 sat/vbyte, max (exclusive) = 21 sat/vbyte...
	FeeRateBucketSize int
}

// Compile-time constraint to ensure SimpleAggregator implements UtxoAggregator.
var _ UtxoAggregator = (*SimpleAggregator)(nil)

// NewSimpleUtxoAggregator creates a new instance of a SimpleAggregator.
func NewSimpleUtxoAggregator(estimator chainfee.Estimator,
	max chainfee.SatPerKWeight) *SimpleAggregator {

	return &SimpleAggregator{
		FeeEstimator:      estimator,
		MaxFeeRate:        max,
		FeeRateBucketSize: DefaultFeeRateBucketSize,
	}
}

// ClusterInputs creates a list of input clusters from the set of pending
// inputs known by the UtxoSweeper. It clusters inputs by
// 1) Required tx locktime
// 2) Similar fee rates.
func (s *SimpleAggregator) ClusterInputs(inputs pendingInputs) []Cluster {
	// We start by getting the inputs clusters by locktime. Since the
	// inputs commit to the locktime, they can only be clustered together
	// if the locktime is equal.
	lockTimeClusters, nonLockTimeInputs := s.clusterByLockTime(inputs)

	// Cluster the remaining inputs by sweep fee rate.
	feeClusters := s.clusterBySweepFeeRate(nonLockTimeInputs)

	// Since the inputs that we clustered by fee rate don't commit to a
	// specific locktime, we can try to merge a locktime cluster with a fee
	// cluster.
	clusters := zipClusters(lockTimeClusters, feeClusters)

	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].sweepFeeRate >
			clusters[j].sweepFeeRate
	})

	result := make([]Cluster, 0, len(clusters))
	for _, c := range clusters {
		result = append(result, &c)
	}

	return result
}

// clusterByLockTime takes the given set of pending inputs and clusters those
// with equal locktime together. Each cluster contains a sweep fee rate, which
// is determined by calculating the average fee rate of all inputs within that
// cluster. In addition to the created clusters, inputs that did not specify a
// required locktime are returned.
func (s *SimpleAggregator) clusterByLockTime(
	inputs pendingInputs) ([]inputCluster, pendingInputs) {

	locktimes := make(map[uint32]pendingInputs)
	rem := make(pendingInputs)

	// Go through all inputs and check if they require a certain locktime.
	for op, input := range inputs {
		lt, ok := input.RequiredLockTime()
		if !ok {
			rem[op] = input
			continue
		}

		// Check if we already have inputs with this locktime.
		cluster, ok := locktimes[lt]
		if !ok {
			cluster = make(pendingInputs)
		}

		// Get the fee rate based on the fee preference. If an error is
		// returned, we'll skip sweeping this input for this round of
		// cluster creation and retry it when we create the clusters
		// from the pending inputs again.
		feeRate, err := input.params.Fee.Estimate(
			s.FeeEstimator, s.MaxFeeRate,
		)
		if err != nil {
			log.Warnf("Skipping input %v: %v", op, err)
			continue
		}

		log.Debugf("Adding input %v to cluster with locktime=%v, "+
			"feeRate=%v", op, lt, feeRate)

		// Attach the fee rate to the input.
		input.lastFeeRate = feeRate

		// Update the cluster about the updated input.
		cluster[op] = input
		locktimes[lt] = cluster
	}

	// We'll then determine the sweep fee rate for each set of inputs by
	// calculating the average fee rate of the inputs within each set.
	inputClusters := make([]inputCluster, 0, len(locktimes))
	for lt, cluster := range locktimes {
		lt := lt

		var sweepFeeRate chainfee.SatPerKWeight
		for _, input := range cluster {
			sweepFeeRate += input.lastFeeRate
		}

		sweepFeeRate /= chainfee.SatPerKWeight(len(cluster))
		inputClusters = append(inputClusters, inputCluster{
			lockTime:     &lt,
			sweepFeeRate: sweepFeeRate,
			inputs:       cluster,
		})
	}

	return inputClusters, rem
}

// clusterBySweepFeeRate takes the set of pending inputs within the UtxoSweeper
// and clusters those together with similar fee rates. Each cluster contains a
// sweep fee rate, which is determined by calculating the average fee rate of
// all inputs within that cluster.
func (s *SimpleAggregator) clusterBySweepFeeRate(
	inputs pendingInputs) []inputCluster {

	bucketInputs := make(map[int]*bucketList)
	inputFeeRates := make(map[wire.OutPoint]chainfee.SatPerKWeight)

	// First, we'll group together all inputs with similar fee rates. This
	// is done by determining the fee rate bucket they should belong in.
	for op, input := range inputs {
		feeRate, err := input.params.Fee.Estimate(
			s.FeeEstimator, s.MaxFeeRate,
		)
		if err != nil {
			log.Warnf("Skipping input %v: %v", op, err)
			continue
		}

		// Only try to sweep inputs with an unconfirmed parent if the
		// current sweep fee rate exceeds the parent tx fee rate. This
		// assumes that such inputs are offered to the sweeper solely
		// for the purpose of anchoring down the parent tx using cpfp.
		parentTx := input.UnconfParent()
		if parentTx != nil {
			parentFeeRate :=
				chainfee.SatPerKWeight(parentTx.Fee*1000) /
					chainfee.SatPerKWeight(parentTx.Weight)

			if parentFeeRate >= feeRate {
				log.Debugf("Skipping cpfp input %v: "+
					"fee_rate=%v, parent_fee_rate=%v", op,
					feeRate, parentFeeRate)

				continue
			}
		}

		feeGroup := s.bucketForFeeRate(feeRate)

		// Create a bucket list for this fee rate if there isn't one
		// yet.
		buckets, ok := bucketInputs[feeGroup]
		if !ok {
			buckets = &bucketList{}
			bucketInputs[feeGroup] = buckets
		}

		// Request the bucket list to add this input. The bucket list
		// will take into account exclusive group constraints.
		buckets.add(input)

		input.lastFeeRate = feeRate
		inputFeeRates[op] = feeRate
	}

	// We'll then determine the sweep fee rate for each set of inputs by
	// calculating the average fee rate of the inputs within each set.
	inputClusters := make([]inputCluster, 0, len(bucketInputs))
	for _, buckets := range bucketInputs {
		for _, inputs := range buckets.buckets {
			var sweepFeeRate chainfee.SatPerKWeight
			for op := range inputs {
				sweepFeeRate += inputFeeRates[op]
			}
			sweepFeeRate /= chainfee.SatPerKWeight(len(inputs))
			inputClusters = append(inputClusters, inputCluster{
				sweepFeeRate: sweepFeeRate,
				inputs:       inputs,
			})
		}
	}

	return inputClusters
}

// bucketForFeeReate determines the proper bucket for a fee rate. This is done
// in order to batch inputs with similar fee rates together.
func (s *SimpleAggregator) bucketForFeeRate(
	feeRate chainfee.SatPerKWeight) int {

	relayFeeRate := s.FeeEstimator.RelayFeePerKW()

	// Create an isolated bucket for sweeps at the minimum fee rate. This
	// is to prevent very small outputs (anchors) from becoming
	// uneconomical if their fee rate would be averaged with higher fee
	// rate inputs in a regular bucket.
	if feeRate == relayFeeRate {
		return 0
	}

	return 1 + int(feeRate-relayFeeRate)/s.FeeRateBucketSize
}

// mergeClusters attempts to merge cluster a and b if they are compatible. The
// new cluster will have the locktime set if a or b had a locktime set, and a
// sweep fee rate that is the maximum of a and b's. If the two clusters are not
// compatible, they will be returned unchanged.
func mergeClusters(a, b inputCluster) []inputCluster {
	newCluster := inputCluster{}

	switch {
	// Incompatible locktimes, return the sets without merging them.
	case a.lockTime != nil && b.lockTime != nil &&
		*a.lockTime != *b.lockTime:

		return []inputCluster{a, b}

	case a.lockTime != nil:
		newCluster.lockTime = a.lockTime

	case b.lockTime != nil:
		newCluster.lockTime = b.lockTime
	}

	if a.sweepFeeRate > b.sweepFeeRate {
		newCluster.sweepFeeRate = a.sweepFeeRate
	} else {
		newCluster.sweepFeeRate = b.sweepFeeRate
	}

	newCluster.inputs = make(pendingInputs)

	for op, in := range a.inputs {
		newCluster.inputs[op] = in
	}

	for op, in := range b.inputs {
		newCluster.inputs[op] = in
	}

	return []inputCluster{newCluster}
}

// zipClusters merges pairwise clusters from as and bs such that cluster a from
// as is merged with a cluster from bs that has at least the fee rate of a.
// This to ensure we don't delay confirmation by decreasing the fee rate (the
// lock time inputs are typically second level HTLC transactions, that are time
// sensitive).
func zipClusters(as, bs []inputCluster) []inputCluster {
	// Sort the clusters by decreasing fee rates.
	sort.Slice(as, func(i, j int) bool {
		return as[i].sweepFeeRate >
			as[j].sweepFeeRate
	})
	sort.Slice(bs, func(i, j int) bool {
		return bs[i].sweepFeeRate >
			bs[j].sweepFeeRate
	})

	var (
		finalClusters []inputCluster
		j             int
	)

	// Go through each cluster in as, and merge with the next one from bs
	// if it has at least the fee rate needed.
	for i := range as {
		a := as[i]

		switch {
		// If the fee rate for the next one from bs is at least a's, we
		// merge.
		case j < len(bs) && bs[j].sweepFeeRate >= a.sweepFeeRate:
			merged := mergeClusters(a, bs[j])
			finalClusters = append(finalClusters, merged...)

			// Increment j for the next round.
			j++

		// We did not merge, meaning all the remaining clusters from bs
		// have lower fee rate. Instead we add a directly to the final
		// clusters.
		default:
			finalClusters = append(finalClusters, a)
		}
	}

	// Add any remaining clusters from bs.
	for ; j < len(bs); j++ {
		b := bs[j]
		finalClusters = append(finalClusters, b)
	}

	return finalClusters
}

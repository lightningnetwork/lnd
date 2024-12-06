package sweep

import (
	"sort"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// UtxoAggregator defines an interface that takes a list of inputs and
// aggregate them into groups. Each group is used as the inputs to create a
// sweeping transaction.
type UtxoAggregator interface {
	// ClusterInputs takes a list of inputs and groups them into input
	// sets. Each input set will be used to create a sweeping transaction.
	ClusterInputs(inputs InputsMap) []InputSet
}

// BudgetAggregator is a budget-based aggregator that creates clusters based on
// deadlines and budgets of inputs.
type BudgetAggregator struct {
	// estimator is used when crafting sweep transactions to estimate the
	// necessary fee relative to the expected size of the sweep
	// transaction.
	estimator chainfee.Estimator

	// maxInputs specifies the maximum number of inputs allowed in a single
	// sweep tx.
	maxInputs uint32

	// auxSweeper is an optional interface that can be used to modify the
	// way sweep transaction are generated.
	auxSweeper fn.Option[AuxSweeper]
}

// Compile-time constraint to ensure BudgetAggregator implements UtxoAggregator.
var _ UtxoAggregator = (*BudgetAggregator)(nil)

// NewBudgetAggregator creates a new instance of a BudgetAggregator.
func NewBudgetAggregator(estimator chainfee.Estimator,
	maxInputs uint32, auxSweeper fn.Option[AuxSweeper]) *BudgetAggregator {

	return &BudgetAggregator{
		estimator:  estimator,
		maxInputs:  maxInputs,
		auxSweeper: auxSweeper,
	}
}

// clusterGroup defines an alias for a set of inputs that are to be grouped.
type clusterGroup map[int32][]SweeperInput

// ClusterInputs creates a list of input sets from pending inputs.
// 1. filter out inputs whose budget cannot cover min relay fee.
// 2. filter a list of exclusive inputs.
// 3. group the inputs into clusters based on their deadline height.
// 4. sort the inputs in each cluster by their budget.
// 5. optionally split a cluster if it exceeds the max input limit.
// 6. create input sets from each of the clusters.
// 7. create input sets for each of the exclusive inputs.
func (b *BudgetAggregator) ClusterInputs(inputs InputsMap) []InputSet {
	// Filter out inputs that have a budget below min relay fee.
	filteredInputs := b.filterInputs(inputs)

	// Create clusters to group inputs based on their deadline height.
	clusters := make(clusterGroup, len(filteredInputs))

	// exclusiveInputs is a set of inputs that are not to be included in
	// any cluster. These inputs can only be swept independently as there's
	// no guarantee which input will be confirmed first, which means
	// grouping exclusive inputs may jeopardize non-exclusive inputs.
	exclusiveInputs := make(map[wire.OutPoint]clusterGroup)

	// Iterate all the inputs and group them based on their specified
	// deadline heights.
	for _, input := range filteredInputs {
		// Get deadline height, and use the specified default deadline
		// height if it's not set.
		height := input.DeadlineHeight

		// Put exclusive inputs in their own set.
		if input.params.ExclusiveGroup != nil {
			log.Tracef("Input %v is exclusive", input.OutPoint())
			exclusiveInputs[input.OutPoint()] = clusterGroup{
				height: []SweeperInput{*input},
			}

			continue
		}

		cluster, ok := clusters[height]
		if !ok {
			cluster = make([]SweeperInput, 0)
		}

		cluster = append(cluster, *input)
		clusters[height] = cluster
	}

	// Now that we have the clusters, we can create the input sets.
	//
	// NOTE: cannot pre-allocate the slice since we don't know the number
	// of input sets in advance.
	inputSets := make([]InputSet, 0)
	for height, cluster := range clusters {
		// Sort the inputs by their economical value.
		sortedInputs := b.sortInputs(cluster)

		// Split on locktimes if they are different.
		splitClusters := splitOnLocktime(sortedInputs)

		// Create input sets from the cluster.
		for _, cluster := range splitClusters {
			sets := b.createInputSets(cluster, height)
			inputSets = append(inputSets, sets...)
		}
	}

	// Create input sets from the exclusive inputs.
	for _, cluster := range exclusiveInputs {
		for height, input := range cluster {
			sets := b.createInputSets(input, height)
			inputSets = append(inputSets, sets...)
		}
	}

	return inputSets
}

// createInputSet takes a set of inputs which share the same deadline height
// and turns them into a list of `InputSet`, each set is then used to create a
// sweep transaction.
//
// TODO(yy): by the time we call this method, all the invalid/uneconomical
// inputs have been filtered out, all the inputs have been sorted based on
// their budgets, and we are about to create input sets. The only thing missing
// here is, we need to group the inputs here even further based on whether
// their budgets can cover the starting fee rate used for this input set.
func (b *BudgetAggregator) createInputSets(inputs []SweeperInput,
	deadlineHeight int32) []InputSet {

	// sets holds the InputSets that we will return.
	sets := make([]InputSet, 0)

	// Copy the inputs to a new slice so we can modify it.
	remainingInputs := make([]SweeperInput, len(inputs))
	copy(remainingInputs, inputs)

	// If the number of inputs is greater than the max inputs allowed, we
	// will split them into smaller clusters.
	for uint32(len(remainingInputs)) > b.maxInputs {
		log.Tracef("Cluster has %v inputs, max is %v, dividing...",
			len(inputs), b.maxInputs)

		// Copy the inputs to be put into the new set, and update the
		// remaining inputs by removing currentInputs.
		currentInputs := make([]SweeperInput, b.maxInputs)
		copy(currentInputs, remainingInputs[:b.maxInputs])
		remainingInputs = remainingInputs[b.maxInputs:]

		// Create an InputSet using the max allowed number of inputs.
		set, err := NewBudgetInputSet(
			currentInputs, deadlineHeight, b.auxSweeper,
		)
		if err != nil {
			log.Errorf("unable to create input set: %v", err)

			continue
		}

		sets = append(sets, set)
	}

	// Create an InputSet from the remaining inputs.
	if len(remainingInputs) > 0 {
		set, err := NewBudgetInputSet(
			remainingInputs, deadlineHeight, b.auxSweeper,
		)
		if err != nil {
			log.Errorf("unable to create input set: %v", err)
			return nil
		}

		sets = append(sets, set)
	}

	return sets
}

// filterInputs filters out inputs that have,
// - a budget below the min relay fee.
// - a budget below its requested starting fee.
// - a required output that's below the dust.
func (b *BudgetAggregator) filterInputs(inputs InputsMap) InputsMap {
	// Get the current min relay fee for this round.
	minFeeRate := b.estimator.RelayFeePerKW()

	// filterInputs stores a map of inputs that has a budget that at least
	// can pay the minimal fee.
	filteredInputs := make(InputsMap, len(inputs))

	// Iterate all the inputs and filter out the ones whose budget cannot
	// cover the min fee.
	for _, pi := range inputs {
		op := pi.OutPoint()

		// Get the size of the witness and skip if there's an error.
		witnessSize, _, err := pi.WitnessType().SizeUpperBound()
		if err != nil {
			log.Warnf("Skipped input=%v: cannot get its size: %v",
				op, err)

			continue
		}

		//nolint:ll
		// Calculate the size if the input is included in the tx.
		//
		// NOTE: When including this input, we need to account the
		// non-witness data which is expressed in vb.
		//
		// TODO(yy): This is not accurate for tapscript input. We need
		// to unify calculations used in the `TxWeightEstimator` inside
		// `input/size.go` and `weightEstimator` in
		// `weight_estimator.go`. And calculate the expected weights
		// similar to BOLT-3:
		// https://github.com/lightning/bolts/blob/master/03-transactions.md#appendix-a-expected-weights
		wu := lntypes.VByte(input.InputSize).ToWU() + witnessSize

		// Skip inputs that has too little budget.
		minFee := minFeeRate.FeeForWeight(wu)
		if pi.params.Budget < minFee {
			log.Warnf("Skipped input=%v: has budget=%v, but the "+
				"min fee requires %v (feerate=%v), size=%v", op,
				pi.params.Budget, minFee,
				minFeeRate.FeePerVByte(), wu.ToVB())

			continue
		}

		// Skip inputs that has cannot cover its starting fees.
		startingFeeRate := pi.params.StartingFeeRate.UnwrapOr(
			chainfee.SatPerKWeight(0),
		)
		startingFee := startingFeeRate.FeeForWeight(wu)
		if pi.params.Budget < startingFee {
			log.Errorf("Skipped input=%v: has budget=%v, but the "+
				"starting fee requires %v (feerate=%v), "+
				"size=%v", op, pi.params.Budget, startingFee,
				startingFeeRate.FeePerVByte(), wu.ToVB())

			continue
		}

		// If the input comes with a required tx out that is below
		// dust, we won't add it.
		//
		// NOTE: only HtlcSecondLevelAnchorInput returns non-nil
		// RequiredTxOut.
		reqOut := pi.RequiredTxOut()
		if reqOut != nil {
			if isDustOutput(reqOut) {
				log.Errorf("Rejected input=%v due to dust "+
					"required output=%v", op, reqOut.Value)

				continue
			}
		}

		filteredInputs[op] = pi
	}

	return filteredInputs
}

// sortInputs sorts the inputs based on their economical value.
//
// NOTE: besides the forced inputs, the sorting won't make any difference
// because all the inputs are added to the same set. The exception is when the
// number of inputs exceeds the maxInputs limit, it requires us to split them
// into smaller clusters. In that case, the sorting will make a difference as
// the budgets of the clusters will be different.
func (b *BudgetAggregator) sortInputs(inputs []SweeperInput) []SweeperInput {
	// sortedInputs is the final list of inputs sorted by their economical
	// value.
	sortedInputs := make([]SweeperInput, 0, len(inputs))

	// Copy the inputs.
	sortedInputs = append(sortedInputs, inputs...)

	// Sort the inputs based on their budgets.
	//
	// NOTE: We can implement more sophisticated algorithm as the budget
	// left is a function f(minFeeRate, size) = b1 - s1 * r > b2 - s2 * r,
	// where b1 and b2 are budgets, s1 and s2 are sizes of the inputs.
	sort.Slice(sortedInputs, func(i, j int) bool {
		left := sortedInputs[i].params.Budget
		right := sortedInputs[j].params.Budget

		// Make sure forced inputs are always put in the front.
		leftForce := sortedInputs[i].params.Immediate
		rightForce := sortedInputs[j].params.Immediate

		// If both are forced inputs, we return the one with the higher
		// budget. If neither are forced inputs, we also return the one
		// with the higher budget.
		if leftForce == rightForce {
			return left > right
		}

		// Otherwise, it's either the left or the right is forced. We
		// can simply return `leftForce` here as, if it's true, the
		// left is forced and should be put in the front. Otherwise,
		// the right is forced and should be put in the front.
		return leftForce
	})

	return sortedInputs
}

// splitOnLocktime splits the list of inputs based on their locktime.
//
// TODO(yy): this is a temporary hack as the blocks are not synced among the
// contractcourt and the sweeper.
func splitOnLocktime(inputs []SweeperInput) map[uint32][]SweeperInput {
	result := make(map[uint32][]SweeperInput)
	noLocktimeInputs := make([]SweeperInput, 0, len(inputs))

	// mergeLocktime is the locktime that we use to merge all the
	// nolocktime inputs into.
	var mergeLocktime uint32

	// Iterate all inputs and split them based on their locktimes.
	for _, inp := range inputs {
		locktime, required := inp.RequiredLockTime()
		if !required {
			log.Tracef("No locktime required for input=%v",
				inp.OutPoint())

			noLocktimeInputs = append(noLocktimeInputs, inp)

			continue
		}

		log.Tracef("Split input=%v on locktime=%v", inp.OutPoint(),
			locktime)

		// Get the slice - the slice will be initialized if not found.
		inputList := result[locktime]

		// Add the input to the list.
		inputList = append(inputList, inp)

		// Update the map.
		result[locktime] = inputList

		// Update the merge locktime.
		mergeLocktime = locktime
	}

	// If there are locktime inputs, we will merge the no locktime inputs
	// to the last locktime group found.
	if len(result) > 0 {
		log.Tracef("No locktime inputs has been merged to locktime=%v",
			mergeLocktime)
		result[mergeLocktime] = append(
			result[mergeLocktime], noLocktimeInputs...,
		)
	} else {
		// Otherwise just return the no locktime inputs.
		result[mergeLocktime] = noLocktimeInputs
	}

	return result
}

// isDustOutput checks if the given output is considered as dust.
func isDustOutput(output *wire.TxOut) bool {
	// Fetch the dust limit for this output.
	dustLimit := lnwallet.DustLimitForSize(len(output.PkScript))

	// If the output is below the dust limit, we consider it dust.
	return btcutil.Amount(output.Value) < dustLimit
}

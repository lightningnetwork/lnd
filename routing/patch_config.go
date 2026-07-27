package routing

// PatchConfig gates two changes to how a payment reacts to the knowledge
// mission control already holds. Both default to false, in which case every
// code path guarded by this struct is exactly the code that shipped before
// it existed.
//
// The two knobs are independent so that they can be ablated separately, but
// they share a motivation. Mission control records a failure as an amount
// bound: "this pair could not carry X". Path finding consults that bound,
// because the estimator gates on the amount it is asked about. Nothing above
// path finding ever asks a different amount, so the bound can only ever be
// used to answer the question the caller already fixed. These two knobs let
// the payment loop ask a better question instead.
type PatchConfig struct {
	// AdaptiveSplit replaces the blind halving of the shard amount on a
	// no-route result with a search for the largest amount that path
	// finding can still route. Every probe of that search is an ordinary
	// path finding call, so every probe respects every bound mission
	// control holds; no new state is recorded and the estimator is
	// untouched.
	AdaptiveSplit bool

	// SoftUnknown replaces the whole-route penalty applied to a failure
	// that could not be attributed to any hop with a penalty on the single
	// least promising hop of the attempted route.
	SoftUnknown bool
}

// LinearFeeFunction is the abstract model of our fee function. We use one
// here to indicate that only one of them exists in our model. Our model will
// use time to drive this single instance.
one sig LinearFeeFunction {
  // startFeeRate is the starting fee rate of our model.
  var startingFeeRate: Int, 
  
  // endingFeeRate is the max fee rate we'll be ending out once the deadline
  // has been reached.
  var endingFeeRate: Int,

  // currentFeeRate is the current fee rate we're using to bump the
  // transaction.
  var currentFeeRate: Int,

  // width is the number of blocks between the starting block height and the
  // deadline height.
  var width: Int,

  // position is the number of between the current height and the starting
  // height. This is our progress as time passes, we'll increment by a single
  // block.
  var position: Int, 

  // deltaFeeRate is the fee rate time step in msat/kw that we'll take each
  // time we update our progress (scaled by position). In this simplified
  // model, this will always be 1.
  var deltaFeeRate: Int,
} {
  // Here we have some _implicit_ facts attached to the definition of
  // LinearFeeFunction. These ensure that we always get well formed traces in
  // our model.

  // position must be positive, and can never exceed our width. In our model,
  // position is the amount of blocks that have elapsed, and width is the conf
  // target.
  position >= 0
  position <= width

  // startingFeeRate must be positive.
  startingFeeRate > 0 

  // currentFeeRate must be positive.
  currentFeeRate > 0 

  // endingFeeRate must be positive.
  endingFeeRate > 0 

  // deltaFeeRate always just increments by one. This simplifies our model, as
  // Alloy doesn't support arithmetics with large numbers easily.
  deltaFeeRate = 1
}

// ActiveFeeFunctions is a signature for the set of all active fee functions.
// This is used to properly initialize a fee function before it can be used
// elsewhere in the model.
sig ActiveFeeFunctions {
   // activeFuncs is the set of all active functions.
   var activeFuncs: set LinearFeeFunction
}

// feeFuncInitZeroValues is a basic fact that capture the semantics of fee
// functions and initialization.
fact feeFuncInitZeroValues {
   // If a function isn't in the set of active functions, then its position
   // must be zero.
   all f: LinearFeeFunction | 
       f not in ActiveFeeFunctions.activeFuncs => f.position = 0
}

// feeFuncSetStartsEmpty captures a basic fact that at the very start of the
// trace, there are no active fee functions. There're no temporal operators in
// this fact, so it only applies to the very start of the trace.
fact feeFuncSetStartsEmpty {
  no activeFuncs
}

// init is a predicate to initialize the params of the fee function.
pred init[f: LinearFeeFunction, maxFeeRate, startFeeRate: Int, confTarget: Int] {
  // We only want to initiate once, so we'll have a guard that only proceeds if
  // f isn't in the set of active functions.
  f not in ActiveFeeFunctions.activeFuncs

  // The starting rate and the ending rate shouldn't be equal to each other. 
  maxFeeRate != startFeeRate
  maxFeeRate > startFeeRate

  // If the conf target is zero, then we'll just jump straight to max params.
  confTarget = 0 implies {
    f.startingFeeRate' = maxFeeRate
    f.endingFeeRate' = maxFeeRate
    f.currentFeeRate' = maxFeeRate
    f.position' = 0
    f.width' = confTarget
    f.deltaFeeRate' = 1
    ActiveFeeFunctions.activeFuncs' = ActiveFeeFunctions.activeFuncs + f
  } else {
    // Otherwise, we'll be starting from a position that we can use to drive
    // forward the system. 
    confTarget != 0 

    f.startingFeeRate' = startFeeRate
    f.currentFeeRate' = startFeeRate
    f.endingFeeRate' = maxFeeRate
    f.width' = confTarget
    f.position' = 0

    // Our delta fee rate is just always 1, we'll take a single step towards
    // the final solution at a time. 
    f.deltaFeeRate' = 1
    ActiveFeeFunctions.activeFuncs' = ActiveFeeFunctions.activeFuncs + f
  }
}

// increment is a predicate that implements our fee bumping routine.
pred increment[f: LinearFeeFunction] {
  // Update our fee rate to take into account our new position.  Increase our
  // position by one, as a single block has past.
  increase_fee_rate[f, f.position.add[1]]
}

// increase_fee_rate takes a new position, and our fee function, then updates
// based on this relationship:
//  feeRate = startingFeeRate + position * delta.
//	- width: deadlineBlockHeight - startingBlockHeight
//	- delta: (endingFeeRate - startingFeeRate) / width
//	 - position: currentBlockHeight - startingBlockHeight
pred increase_fee_rate[f : LinearFeeFunction, newPosition: Int] {
  // Update the position of the model in the next timestep.
  f.position' = newPosition

  // Ensure that our position hasn't passed our width yet. This is an error
  // scenario in the original Go code.
  f.position' <= f.width

  // Our new position is the distance between the 
  f.currentFeeRate' = fee_rate_at_position[f, newPosition]
 
  f.startingFeeRate' = f.startingFeeRate
  f.endingFeeRate' = f.endingFeeRate
  f.width' = f.width
  f.deltaFeeRate' = f.deltaFeeRate
  activeFuncs' = activeFuncs
}

// fee_rate_at_position computes the new fee rate based on an updated position.
fun fee_rate_at_position[f: LinearFeeFunction, p: Int]: Int {
  // If the position is equal to the width, then we'll go straight to the max
  // fee rate.
  p >= f.width.sub[1] => f.endingFeeRate
  //p >= f.width => f.endingFeeRate -- NOTE: Uncomment this to re-introduce the original bug.
  else 
    // Otherwise, we'll do the fee rate bump.
    let deltaRate = f.deltaFeeRate,
        newFeeRate = f.currentFeeRate.add[deltaRate] |

        // If the new fee rate would exceed the ending rate, then we clamp it
        // to our max fee rate.  Otherwise we return our normal fee rate.
        newFeeRate > f.endingFeeRate => f.endingFeeRate else newFeeRate
}

// fee_bump is a predicate that executes a fee bump step.
pred fee_bump[f: LinearFeeFunction] {
  // We use a guard to ensure that fee_bump can only be called after init
  // (which will add the func to the set of active funcs).
  f in ActiveFeeFunctions.activeFuncs

  increment[f]
}

// stutter does nothing. This just assigns all variables to themselves, this is
// useful in a trace to allow it to noop.
pred stutter[f: LinearFeeFunction] {
  // No change in any of our variables.
  f.startingFeeRate' = f.startingFeeRate
  f.endingFeeRate' = f.endingFeeRate
  f.currentFeeRate' = f.currentFeeRate
  f.position' = f.position 
  f.width' = f.width
  f.deltaFeeRate' = f.deltaFeeRate
  activeFuncs' = activeFuncs
}

// Event is used to generate easier to follow traces. These these enums are
// events that correspond to the main state transition predicates above.
//
// See this portion of documentation for more information on this idiom:
// https://haslab.github.io/formal-software-design/modelling-tips/index.html#an-idiom-to-depict-events.
enum Event { Stutter, Init, FeeBump }

// stutter is a derived relation that's populated whenever a stutter event
// happens.
fun stutter : set Event -> LinearFeeFunction {
  { e: Stutter, f: LinearFeeFunction | stutter[f] }
}

// init is a derived relation that's populated whenever an init event happens.
fun init : Event -> LinearFeeFunction -> Int -> Int -> Int {
  { e: Init, f: LinearFeeFunction, m: Int, s: Int, c: Int | init[f, m, s, c] }
}

// fee_bump is a derived relation that's populated whenever a fee_bump event
// happens.
fun fee_bump : Event -> LinearFeeFunction {
  { e: FeeBump, f: LinearFeeFunction | fee_bump[f] }
}

// events is our final derived relation that stores all the possible event
// types.
fun events : set Event {
  stutter.LinearFeeFunction + init.Int.Int.Int.LinearFeeFunction + fee_bump.LinearFeeFunction
}

// traces is a fact to ensure that we always get one of the above events in
// traces we generate.
fact traces {
  always some events
}

// init_traces builds on the fact traces above, and also asserts that there's
// always at least one init event (to ensure proper initialization).
fact init_traces {
  eventually (one init)
}

// fee_bump_happens ensures that within our trace, there's always at least a
// single fee bump event.
fact fee_bump_happens {
  eventually (some fee_bump)
}


// valid_fee_rates is a fact that ensures that the ending fee rate is always
// greater than the starting fee rate.
fact valid_fee_rates {
  all f: LinearFeeFunction | f.endingFeeRate > f.startingFeeRate
}

// init_correctness is an assertion that makes sure once an init event happens,
// then we have the function in our set of active functions.
assert init_correctness {
  all f: LinearFeeFunction | {
    eventually (some init) implies 
      eventually f in ActiveFeeFunctions.activeFuncs
  }
}

// check init_correctness

// init_always_happens is a trivial assertion that ensures that an init
// eventually happens in a trace.
assert init_always_happens {
   eventually (some init)
}

// check init_always_happens

// init_then_fee_bump asserts that within our trace, we'll always have at least
// a single fee bump event after an init.
assert init_then_fee_bump {
  eventually (some init => some fee_bump)
}

// check init_then_fee_bump

// bump_to_completion is a predicate that can be used to force fee_bump events
// to be emitted until we're right at our confirmation deadline.
pred bump_to_completion {
  always (all f: LinearFeeFunction | f.position < f.width => eventually (some fee_bump))
}

// bump_to_final_block is a predicate that can be used to force fee_bump events
// to be emitted until we're one block before our confirmation deadline.
pred bump_to_final_block {
  always (all f: LinearFeeFunction | f.position < f.width.sub[1] => eventually (some fee_bump))
}

// req_num_blocks_to_conf is a predicate that can be used to restrict the
// functions in a fact to a given confirmation target.
pred req_num_blocks_to_conf[n: Int] {
  all f: LinearFeeFunction | f.width = n
}

// req_starting_fee_rate is a predicate that can be used to restrict functions
// in a fact to a given starting fee rate.
pred req_starting_fee_rate[n: Int] {
  all f: LinearFeeFunction | f.startingFeeRate = n
}

// max_fee_rate_before_deadline is the main assertion in this model. This
// captures a model violation for our fee function, but only if the line in
// fee_rate_at_position is uncommented.
//
// In this assertion, we declare that if we have a fee function that has a conf
// target of 4 (we want a few fee bumps), and we bump to the final block, then
// at that point our current fee rate is the ending fee rate. In the original
// code, assertion isn't upheld, due to an off by one error.
assert max_fee_rate_before_deadline {
  always req_num_blocks_to_conf[4] => bump_to_final_block => eventually (
    all f: LinearFeeFunction | f.position = f.width.sub[1] => f.currentFeeRate = f.endingFeeRate
  )
}

check max_fee_rate_before_deadline

run {
  // Our default command just shows a trace that has at least 4 fee bump
  // events.
  always req_num_blocks_to_conf[4] 
  always bump_to_completion
} 

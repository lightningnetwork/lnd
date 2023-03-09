# Release Notes

## Payment

The payment lifecycle code has been refactored to improve its maintainablity.
In particular, the complexity involved in the lifecycle loop has been decoupled
into logical steps, with each step has its own responsibility, making it easier
to reason about the payment flow.

* [Deprecated](https://github.com/lightningnetwork/lnd/pull/7175)
  `StatusUnknown` from the payment's rpc response in its status and replaced it
  with `StatusInitiated` to explicitly report its current state.

* [The payment lifecycle inside
  router](https://github.com/lightningnetwork/lnd/pull/7391) has been
  refactored to return more verbose errors on payments.

* [Fixed a case](https://github.com/lightningnetwork/lnd/pull/6683) where it's
  possible a failed payment might be stuck in pending.

# Contributors (Alphabetical Order)

* Yong Yu

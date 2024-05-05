package lncfg

import "fmt"

const (
	// DefaultHoldInvoiceExpiryDelta defines the number of blocks before the
	// expiry height of a hold invoice's htlc that lnd will automatically
	// cancel the invoice to prevent the channel from force closing. This
	// value *must* be greater than DefaultIncomingBroadcastDelta to prevent
	// force closes.
	DefaultHoldInvoiceExpiryDelta = DefaultIncomingBroadcastDelta + 2

	// DefaultMinNumRealBlindedPathHops is the minimum number of _real_
	// hops to include in a blinded payment path. This doesn't include our
	// node (the destination node), so if the minimum is 1, then the path
	// will contain at minimum our node along with an introduction node hop.
	DefaultMinNumRealBlindedPathHops = 1

	// DefaultNumBlindedPathHops is the number of hops to include in a
	// blinded payment path. If paths shorter than this number are found,
	// then dummy hops are used to pad the path to this length.
	DefaultNumBlindedPathHops = 2

	// DefaultMaxNumBlindedPaths is the maximum number of different blinded
	// payment paths to include in an invoice.
	DefaultMaxNumBlindedPaths = 3

	// DefaultBlindedPathPolicyIncreaseMultiplier is the default multiplier
	// used to increase certain blinded hop policy values in order to add
	// a probing buffer.
	DefaultBlindedPathPolicyIncreaseMultiplier = 1.1

	// DefaultBlindedPathPolicyDecreaseMultiplier is the default multiplier
	// used to decrease certain blinded hop policy values in order to add a
	// probing buffer.
	DefaultBlindedPathPolicyDecreaseMultiplier = 0.9
)

// Invoices holds the configuration options for invoices.
//
//nolint:lll
type Invoices struct {
	HoldExpiryDelta uint32 `long:"holdexpirydelta" description:"The number of blocks before a hold invoice's htlc expires that the invoice should be canceled to prevent a force close. Force closes will not be prevented if this value is not greater than DefaultIncomingBroadcastDelta."`

	BlindedPaths BlindedPaths `group:"blinding" namespace:"blinding"`
}

// BlindedPaths holds the configuration options for blinded paths added to
// invoices.
//
//nolint:lll
type BlindedPaths struct {
	MinNumRealHops           uint8   `long:"min-num-real-hops" description:"The minimum number of real hops to include in a blinded path. This doesn't include our node, so if the minimum is 1, then the path will contain at minimum our node along with an introduction node hop. If it is zero then the shortest path will use our node as an introduction node."`
	NumHops                  uint8   `long:"num-hops" description:"The number of hops to include in a blinded path. This doesn't include our node, so if it is 1, then the path will contain our node along with an introduction node or dummy node hop. If paths shorter than NumHops is found, then they will be padded using dummy hops."`
	MaxNumPaths              uint8   `long:"max-num-paths" description:"The maximum number of blinded paths to select and add to an invoice."`
	PolicyIncreaseMultiplier float64 `long:"policy-increase-multiplier" description:"The amount by which to increase certain policy values of hops on a blinded path in order to add a probing buffer."`
	PolicyDecreaseMultiplier float64 `long:"policy-decrease-multiplier" description:"The amount by which to decrease certain policy values of hops on a blinded path in order to add a probing buffer."`
}

// Validate checks that the various invoice config options are sane.
//
// NOTE: this is part of the Validator interface.
func (i *Invoices) Validate() error {
	// Log a warning if our expiry delta is not greater than our incoming
	// broadcast delta. We do not fail here because this value may be set
	// to zero to intentionally keep lnd's behavior unchanged from when we
	// didn't auto-cancel these invoices.
	if i.HoldExpiryDelta <= DefaultIncomingBroadcastDelta {
		log.Warnf("Invoice hold expiry delta: %v <= incoming "+
			"delta: %v, accepted hold invoices will force close "+
			"channels if they are not canceled manually",
			i.HoldExpiryDelta, DefaultIncomingBroadcastDelta)
	}

	if i.BlindedPaths.MinNumRealHops > i.BlindedPaths.NumHops {
		return fmt.Errorf("the minimum number of real hops in a " +
			"blinded path must be smaller than or equal to the " +
			"number of hops expected to be included in each path")
	}

	if i.BlindedPaths.PolicyIncreaseMultiplier < 1 {
		return fmt.Errorf("the blinded route policy increase " +
			"multiplier must be greater than or equal to 1")
	}

	if i.BlindedPaths.PolicyDecreaseMultiplier > 1 ||
		i.BlindedPaths.PolicyDecreaseMultiplier < 0 {

		return fmt.Errorf("the blinded route policy decrease " +
			"multiplier must be in the range (0,1]")
	}

	return nil
}

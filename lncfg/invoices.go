package lncfg

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

	return nil
}

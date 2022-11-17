package lncfg

// DefaultHoldInvoiceExpiryDelta defines the number of blocks before the expiry
// height of a hold invoice's htlc that lnd will automatically cancel the
// invoice to prevent the channel from force closing. This value *must* be
// greater than DefaultIncomingBroadcastDelta to prevent force closes.
const DefaultHoldInvoiceExpiryDelta = DefaultIncomingBroadcastDelta + 2

// Invoices holds the configuration options for invoices.
//
//nolint:lll
type Invoices struct {
	HoldExpiryDelta uint32 `long:"holdexpirydelta" description:"The number of blocks before a hold invoice's htlc expires that the invoice should be canceled to prevent a force close. Force closes will not be prevented if this value is not greater than DefaultIncomingBroadcastDelta."`
}

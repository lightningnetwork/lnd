package channeldb

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// PaymentCreationInfo is the information necessary to have ready when
// initiating a payment, moving it into state InFlight.
type PaymentCreationInfo struct {
	// PaymentIdentifier is the hash this payment is paying to in case of
	// non-AMP payments, and the SetID for AMP payments.
	PaymentIdentifier lntypes.Hash

	// Value is the amount we are paying.
	Value lnwire.MilliSatoshi

	// CreationTime is the time when this payment was initiated.
	CreationTime time.Time

	// PaymentRequest is the full payment request, if any.
	PaymentRequest []byte

	// FirstHopCustomRecords are the TLV records that are to be sent to the
	// first hop of this payment. These records will be transmitted via the
	// wire message only and therefore do not affect the onion payload size.
	FirstHopCustomRecords lnwire.CustomRecords
}

// String returns a human-readable description of the payment creation info.
func (p *PaymentCreationInfo) String() string {
	return fmt.Sprintf("payment_id=%v, amount=%v, created_at=%v",
		p.PaymentIdentifier, p.Value, p.CreationTime)
}

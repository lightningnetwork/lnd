package channeldb

// PaymentStatus represent current status of payment.
type PaymentStatus byte

const (
	// NOTE: PaymentStatus = 0 was previously used for status unknown and
	// is now deprecated.

	// StatusInitiated is the status where a payment has just been
	// initiated.
	StatusInitiated PaymentStatus = 1

	// StatusInFlight is the status where a payment has been initiated, but
	// a response has not been received.
	StatusInFlight PaymentStatus = 2

	// StatusSucceeded is the status where a payment has been initiated and
	// the payment was completed successfully.
	StatusSucceeded PaymentStatus = 3

	// StatusFailed is the status where a payment has been initiated and a
	// failure result has come back.
	StatusFailed PaymentStatus = 4
)

// String returns readable representation of payment status.
func (ps PaymentStatus) String() string {
	switch ps {
	case StatusInitiated:
		return "Initiated"

	case StatusInFlight:
		return "In Flight"

	case StatusSucceeded:
		return "Succeeded"

	case StatusFailed:
		return "Failed"

	default:
		return "Unknown"
	}
}

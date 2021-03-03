package invoices

import (
	"github.com/lightningnetwork/lnd/record"
)

// Payload abstracts access to any additional fields provided in the final hop's
// TLV onion payload.
type Payload interface {
	// MultiPath returns the record corresponding the option_mpp parsed from
	// the onion payload.
	MultiPath() *record.MPP

	// AMPRecord returns the record corresponding to the option_amp record
	// parsed from the onion payload.
	AMPRecord() *record.AMP

	// CustomRecords returns the custom tlv type records that were parsed
	// from the payload.
	CustomRecords() record.CustomSet
}

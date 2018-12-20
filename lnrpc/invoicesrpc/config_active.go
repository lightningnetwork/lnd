// +build invoicesrpc

package invoicesrpc

import (
	"github.com/lightningnetwork/lnd/invoices"
)

// Config is the primary configuration struct for the invoices RPC server. It
// contains all the items required for the rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	InvoiceRegistry *invoices.InvoiceRegistry
}

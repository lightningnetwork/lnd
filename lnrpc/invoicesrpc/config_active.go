// +build invoicesrpc

package invoicesrpc

import (
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/macaroons"
)

// Config is the primary configuration struct for the invoices RPC server. It
// contains all the items required for the rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	// NetworkDir is the main network directory wherein the invoices rpc
	// server will find the macaroon named DefaultInvoicesMacFilename.
	NetworkDir string

	// MacService is the main macaroon service that we'll use to handle
	// authentication for the invoices rpc server.
	MacService *macaroons.Service

	// InvoiceRegistry is a central registry of all the outstanding invoices
	// created by the daemon.
	InvoiceRegistry *invoices.InvoiceRegistry

	// ChainParams are required to properly decode invoice payment requests
	// that are marshalled over rpc.
	ChainParams *chaincfg.Params
}

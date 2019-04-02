// +build invoicesrpc

package invoicesrpc

import (
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/netann"
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

	// IsChannelActive is used to generate valid hop hints.
	IsChannelActive func(chanID lnwire.ChannelID) bool

	// ChainParams are required to properly decode invoice payment requests
	// that are marshalled over rpc.
	ChainParams *chaincfg.Params

	// NodeSigner is an implementation of the MessageSigner implementation
	// that's backed by the identity private key of the running lnd node.
	NodeSigner *netann.NodeSigner

	// MaxPaymentMSat is the maximum allowed payment.
	MaxPaymentMSat lnwire.MilliSatoshi

	// DefaultCLTVExpiry is the default invoice expiry if no values is
	// specified.
	DefaultCLTVExpiry uint32

	// ChanDB is a global boltdb instance which is needed to access the
	// channel graph.
	ChanDB *channeldb.DB
}

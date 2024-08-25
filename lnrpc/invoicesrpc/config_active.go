//go:build invoicesrpc
// +build invoicesrpc

package invoicesrpc

import (
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/protobuf/proto"
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

	// HtlcModifier is a service which intercepts invoice HTLCs during the
	// settlement phase, enabling a subscribed client to modify certain
	// aspects of those HTLCs.
	HtlcModifier invoices.HtlcModifier

	// IsChannelActive is used to generate valid hop hints.
	IsChannelActive func(chanID lnwire.ChannelID) bool

	// ChainParams are required to properly decode invoice payment requests
	// that are marshalled over rpc.
	ChainParams *chaincfg.Params

	// NodeSigner is an implementation of the MessageSigner implementation
	// that's backed by the identity private key of the running lnd node.
	NodeSigner *netann.NodeSigner

	// DefaultCLTVExpiry is the default invoice expiry if no values is
	// specified.
	DefaultCLTVExpiry uint32

	// GraphDB is a global database instance which is needed to access the
	// channel graph.
	GraphDB *channeldb.ChannelGraph

	// ChanStateDB is a possibly replicated db instance which contains the
	// channel state
	ChanStateDB *channeldb.ChannelStateDB

	// GenInvoiceFeatures returns a feature containing feature bits that
	// should be advertised on freshly generated invoices.
	GenInvoiceFeatures func(blinded bool) *lnwire.FeatureVector

	// GenAmpInvoiceFeatures returns a feature containing feature bits that
	// should be advertised on freshly generated AMP invoices.
	GenAmpInvoiceFeatures func() *lnwire.FeatureVector

	// GetAlias returns the peer's alias SCID if it exists given the
	// 32-byte ChannelID.
	GetAlias func(lnwire.ChannelID) (lnwire.ShortChannelID, error)

	// ParseAuxData is a function that can be used to parse the auxiliary
	// data from the invoice.
	ParseAuxData func(message proto.Message) error

	// BlindedPathCfg takes the global routing blinded path policies and the
	// given per-payment blinded path config values and uses these to
	// construct the config values passed to the invoice server.
	BlindedPathCfg func(bool, *lnrpc.BlindedPathConfig) (
		*BlindedPathConfig, error)

	// BestHeight can be used to get the current best block height known to
	// LND.
	BestHeight func() (uint32, error)

	// QueryBlindedRoutes can be used to generate a few routes to this node
	// that can then be used in the construction of a blinded payment path.
	QueryBlindedRoutes func(*routing.BlindedPathRestrictions,
		lnwire.MilliSatoshi) ([]*route.Route, error)
}

package lnd

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	proxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/chanfitness"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/funding"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/onionmessage"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/peer"
	"github.com/lightningnetwork/lnd/peernotifier"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/blindedpath"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/watchtower"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/tv42/zbase32"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// defaultNumBlocksEstimate is the number of blocks that we fall back
	// to issuing an estimate for if a fee pre fence doesn't specify an
	// explicit conf target or fee rate.
	defaultNumBlocksEstimate = 6
)

var (
	// readPermissions is a slice of all entities that allow read
	// permissions for authorization purposes, all lowercase.
	readPermissions = []bakery.Op{
		{
			Entity: "onchain",
			Action: "read",
		},
		{
			Entity: "offchain",
			Action: "read",
		},
		{
			Entity: "address",
			Action: "read",
		},
		{
			Entity: "message",
			Action: "read",
		},
		{
			Entity: "peers",
			Action: "read",
		},
		{
			Entity: "info",
			Action: "read",
		},
		{
			Entity: "invoices",
			Action: "read",
		},
		{
			Entity: "signer",
			Action: "read",
		},
		{
			Entity: "macaroon",
			Action: "read",
		},
	}

	// writePermissions is a slice of all entities that allow write
	// permissions for authorization purposes, all lowercase.
	writePermissions = []bakery.Op{
		{
			Entity: "onchain",
			Action: "write",
		},
		{
			Entity: "offchain",
			Action: "write",
		},
		{
			Entity: "address",
			Action: "write",
		},
		{
			Entity: "message",
			Action: "write",
		},
		{
			Entity: "peers",
			Action: "write",
		},
		{
			Entity: "info",
			Action: "write",
		},
		{
			Entity: "invoices",
			Action: "write",
		},
		{
			Entity: "signer",
			Action: "generate",
		},
		{
			Entity: "macaroon",
			Action: "generate",
		},
		{
			Entity: "macaroon",
			Action: "write",
		},
	}

	// invoicePermissions is a slice of all the entities that allows a user
	// to only access calls that are related to invoices, so: streaming
	// RPCs, generating, and listening invoices.
	invoicePermissions = []bakery.Op{
		{
			Entity: "invoices",
			Action: "read",
		},
		{
			Entity: "invoices",
			Action: "write",
		},
		{
			Entity: "address",
			Action: "read",
		},
		{
			Entity: "address",
			Action: "write",
		},
		{
			Entity: "onchain",
			Action: "read",
		},
	}

	// TODO(guggero): Refactor into constants that are used for all
	// permissions in this file. Also expose the list of possible
	// permissions in an RPC when per RPC permissions are
	// implemented.
	validActions  = []string{"read", "write", "generate"}
	validEntities = []string{
		"onchain", "offchain", "address", "message",
		"peers", "info", "invoices", "signer", "macaroon",
		macaroons.PermissionEntityCustomURI,
	}

	// If the --no-macaroons flag is used to start lnd, the macaroon service
	// is not initialized. errMacaroonDisabled is then returned when
	// macaroon related services are used.
	errMacaroonDisabled = fmt.Errorf("macaroon authentication disabled, " +
		"remove --no-macaroons flag to enable")
)

// stringInSlice returns true if a string is contained in the given slice.
func stringInSlice(a string, slice []string) bool {
	for _, b := range slice {
		if b == a {
			return true
		}
	}
	return false
}

// GetAllPermissions returns all the permissions required to interact with lnd.
func GetAllPermissions() []bakery.Op {
	allPerms := make([]bakery.Op, 0)

	// The map will help keep track of which specific permission pairs have
	// already been added to the slice.
	allPermsMap := make(map[string]map[string]struct{})

	for _, perms := range MainRPCServerPermissions() {
		for _, perm := range perms {
			entity := perm.Entity
			action := perm.Action

			// If this specific entity-action permission pair isn't
			// in the map yet. Add it to map, and the permission
			// slice.
			if acts, ok := allPermsMap[entity]; ok {
				if _, ok := acts[action]; !ok {
					allPermsMap[entity][action] = struct{}{}

					allPerms = append(
						allPerms, perm,
					)
				}
			} else {
				allPermsMap[entity] = make(map[string]struct{})
				allPermsMap[entity][action] = struct{}{}
				allPerms = append(allPerms, perm)
			}
		}
	}

	return allPerms
}

// MainRPCServerPermissions returns a mapping of the main RPC server calls to
// the permissions they require.
func MainRPCServerPermissions() map[string][]bakery.Op {
	return map[string][]bakery.Op{
		"/lnrpc.Lightning/SendCoins": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/ListUnspent": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SendMany": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/NewAddress": {{
			Entity: "address",
			Action: "write",
		}},
		"/lnrpc.Lightning/SignMessage": {{
			Entity: "message",
			Action: "write",
		}},
		"/lnrpc.Lightning/VerifyMessage": {{
			Entity: "message",
			Action: "read",
		}},
		"/lnrpc.Lightning/ConnectPeer": {{
			Entity: "peers",
			Action: "write",
		}},
		"/lnrpc.Lightning/DisconnectPeer": {{
			Entity: "peers",
			Action: "write",
		}},
		"/lnrpc.Lightning/OpenChannel": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/BatchOpenChannel": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/OpenChannelSync": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/CloseChannel": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/AbandonChannel": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/GetInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetDebugInfo": {{
			Entity: "info",
			Action: "read",
		}, {
			Entity: "offchain",
			Action: "read",
		}, {
			Entity: "onchain",
			Action: "read",
		}, {
			Entity: "peers",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetRecoveryInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListPeers": {{
			Entity: "peers",
			Action: "read",
		}},
		"/lnrpc.Lightning/WalletBalance": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/EstimateFee": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ChannelBalance": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/PendingChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeChannelEvents": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ClosedChannels": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SendPayment": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendPaymentSync": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendToRoute": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendToRouteSync": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/AddInvoice": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/lnrpc.Lightning/LookupInvoice": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListInvoices": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeInvoices": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeTransactions": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetTransactions": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/DescribeGraph": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetNodeMetrics": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetChanInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetNodeInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/QueryRoutes": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/GetNetworkInfo": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/StopDaemon": {{
			Entity: "info",
			Action: "write",
		}},
		"/lnrpc.Lightning/SubscribeChannelGraph": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/DeleteCanceledInvoice": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/lnrpc.Lightning/ListPayments": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/DeletePayment": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/DeleteAllPayments": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/DebugLevel": {{
			Entity: "info",
			Action: "write",
		}},
		"/lnrpc.Lightning/DecodePayReq": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/FeeReport": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/UpdateChannelPolicy": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/ForwardingHistory": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/RestoreChannelBackups": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/ExportChannelBackup": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/VerifyChanBackup": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ExportAllChannelBackups": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribeChannelBackups": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ChannelAcceptor": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/BakeMacaroon": {{
			Entity: "macaroon",
			Action: "generate",
		}},
		"/lnrpc.Lightning/ListMacaroonIDs": {{
			Entity: "macaroon",
			Action: "read",
		}},
		"/lnrpc.Lightning/DeleteMacaroonID": {{
			Entity: "macaroon",
			Action: "write",
		}},
		"/lnrpc.Lightning/ListPermissions": {{
			Entity: "info",
			Action: "read",
		}},
		"/lnrpc.Lightning/CheckMacaroonPermissions": {{
			Entity: "macaroon",
			Action: "read",
		}},
		"/lnrpc.Lightning/SubscribePeerEvents": {{
			Entity: "peers",
			Action: "read",
		}},
		"/lnrpc.Lightning/FundingStateStep": {{
			Entity: "onchain",
			Action: "write",
		}, {
			Entity: "offchain",
			Action: "write",
		}},
		lnrpc.RegisterRPCMiddlewareURI: {{
			Entity: "macaroon",
			Action: "write",
		}},
		"/lnrpc.Lightning/SendCustomMessage": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SubscribeCustomMessages": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/SendOnionMessage": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/lnrpc.Lightning/SubscribeOnionMessages": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/LookupHtlcResolution": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/lnrpc.Lightning/ListAliases": {{
			Entity: "offchain",
			Action: "read",
		}},
	}
}

// AuxDataParser is an interface that is used to parse auxiliary custom data
// within RPC messages. This is used to transform binary blobs to human-readable
// JSON representations.
type AuxDataParser interface {
	// InlineParseCustomData replaces any custom data binary blob in the
	// given RPC message with its corresponding JSON formatted data. This
	// transforms the binary (likely TLV encoded) data to a human-readable
	// JSON representation (still as byte slice).
	InlineParseCustomData(msg proto.Message) error
}

// rpcServer is a gRPC, RPC front end to the lnd daemon.
// TODO(roasbeef): pagination support for the list-style calls
type rpcServer struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	lnrpc.UnimplementedLightningServer

	server *server

	cfg *Config

	// subServers are a set of sub-RPC servers that use the same gRPC and
	// listening sockets as the main RPC server, but which maintain their
	// own independent service. This allows us to expose a set of
	// micro-service like abstractions to the outside world for users to
	// consume.
	subServers      []lnrpc.SubServer
	subGrpcHandlers []lnrpc.GrpcHandler

	// routerBackend contains the backend implementation of the router
	// rpc sub server.
	routerBackend *routerrpc.RouterBackend

	// chanPredicate is used in the bidirectional ChannelAcceptor streaming
	// method.
	chanPredicate chanacceptor.MultiplexAcceptor

	quit chan struct{}

	// macService is the macaroon service that we need to mint new
	// macaroons.
	macService *macaroons.Service

	// selfNode is our own pubkey.
	selfNode route.Vertex

	// interceptorChain is the interceptor added to our gRPC server.
	interceptorChain *rpcperms.InterceptorChain

	// implCfg is the configuration for some of the interfaces that can be
	// provided externally.
	implCfg *ImplementationCfg

	// interceptor is used to be able to request a shutdown
	interceptor signal.Interceptor

	graphCache        sync.RWMutex
	describeGraphResp *lnrpc.ChannelGraph
	graphCacheEvictor *time.Timer
}

// A compile time check to ensure that rpcServer fully implements the
// LightningServer gRPC service.
var _ lnrpc.LightningServer = (*rpcServer)(nil)

// newRPCServer creates and returns a new instance of the rpcServer. Before
// dependencies are added, this will be an non-functioning RPC server only to
// be used to register the LightningService with the gRPC server.
func newRPCServer(cfg *Config, interceptorChain *rpcperms.InterceptorChain,
	implCfg *ImplementationCfg, interceptor signal.Interceptor) *rpcServer {

	// We go trhough the list of registered sub-servers, and create a gRPC
	// handler for each. These are used to register with the gRPC server
	// before all dependencies are available.
	registeredSubServers := lnrpc.RegisteredSubServers()

	var subServerHandlers []lnrpc.GrpcHandler
	for _, subServer := range registeredSubServers {
		subServerHandlers = append(
			subServerHandlers, subServer.NewGrpcHandler(),
		)
	}

	return &rpcServer{
		cfg:              cfg,
		subGrpcHandlers:  subServerHandlers,
		interceptorChain: interceptorChain,
		implCfg:          implCfg,
		quit:             make(chan struct{}, 1),
		interceptor:      interceptor,
	}
}

// addDeps populates all dependencies needed by the RPC server, and any
// of the sub-servers that it maintains. When this is done, the RPC server can
// be started, and start accepting RPC calls.
func (r *rpcServer) addDeps(ctx context.Context, s *server,
	macService *macaroons.Service,
	subServerCgs *subRPCServerConfigs, atpl *autopilot.Manager,
	invoiceRegistry *invoices.InvoiceRegistry, tower *watchtower.Standalone,
	chanPredicate chanacceptor.MultiplexAcceptor,
	invoiceHtlcModifier *invoices.HtlcModificationInterceptor) error {

	// Set up router rpc backend.
	selfNode, err := s.graphDB.SourceNode(ctx)
	if err != nil {
		return err
	}
	graph := s.graphDB

	routerBackend := &routerrpc.RouterBackend{
		SelfNode: selfNode.PubKeyBytes,
		Clock:    clock.NewDefaultClock(),
		FetchChannelCapacity: func(chanID uint64) (btcutil.Amount,
			error) {

			info, _, _, err := graph.FetchChannelEdgesByID(chanID)
			if err != nil {
				return 0, err
			}
			return info.Capacity, nil
		},
		FetchAmountPairCapacity: func(nodeFrom, nodeTo route.Vertex,
			amount lnwire.MilliSatoshi) (btcutil.Amount, error) {

			return routing.FetchAmountPairCapacity(
				graph, selfNode.PubKeyBytes, nodeFrom, nodeTo,
				amount,
			)
		},
		FetchChannelEndpoints: func(chanID uint64) (route.Vertex,
			route.Vertex, error) {

			info, _, _, err := graph.FetchChannelEdgesByID(
				chanID,
			)
			if err != nil {
				return route.Vertex{}, route.Vertex{},
					fmt.Errorf("unable to fetch channel "+
						"edges by channel ID %d: %v",
						chanID, err)
			}

			return info.NodeKey1Bytes, info.NodeKey2Bytes, nil
		},
		FindRoute:              s.chanRouter.FindRoute,
		MissionControl:         s.defaultMC,
		ActiveNetParams:        r.cfg.ActiveNetParams.Params,
		Tower:                  s.controlTower,
		MaxTotalTimelock:       r.cfg.MaxOutgoingCltvExpiry,
		DefaultFinalCltvDelta:  uint16(r.cfg.Bitcoin.TimeLockDelta),
		SubscribeHtlcEvents:    s.htlcNotifier.SubscribeHtlcEvents,
		InterceptableForwarder: s.interceptableSwitch,
		SetChannelEnabled: func(outpoint wire.OutPoint) error {
			return s.chanStatusMgr.RequestEnable(outpoint, true)
		},
		SetChannelDisabled: func(outpoint wire.OutPoint) error {
			return s.chanStatusMgr.RequestDisable(outpoint, true)
		},
		SetChannelAuto:     s.chanStatusMgr.RequestAuto,
		UseStatusInitiated: subServerCgs.RouterRPC.UseStatusInitiated,
		ParseCustomChannelData: func(msg proto.Message) error {
			err = fn.MapOptionZ(
				r.server.implCfg.AuxDataParser,
				func(parser AuxDataParser) error {
					return parser.InlineParseCustomData(msg)
				},
			)
			if err != nil {
				return fmt.Errorf("error parsing custom data: "+
					"%w", err)
			}

			return nil
		},
		ShouldSetExpEndorsement: func() bool {
			if s.cfg.ProtocolOptions.NoExperimentalEndorsement() {
				return false
			}

			return clock.NewDefaultClock().Now().Before(
				EndorsementExperimentEnd,
			)
		},
	}

	genInvoiceFeatures := func() *lnwire.FeatureVector {
		return s.featureMgr.Get(feature.SetInvoice)
	}
	genAmpInvoiceFeatures := func() *lnwire.FeatureVector {
		return s.featureMgr.Get(feature.SetInvoiceAmp)
	}

	parseAddr := func(addr string) (net.Addr, error) {
		return parseAddr(addr, r.cfg.net)
	}

	var (
		subServers     []lnrpc.SubServer
		subServerPerms []lnrpc.MacaroonPerms
	)

	// Before we create any of the sub-servers, we need to ensure that all
	// the dependencies they need are properly populated within each sub
	// server configuration struct.
	//
	// TODO(roasbeef): extend sub-sever config to have both (local vs remote) DB
	err = subServerCgs.PopulateDependencies(
		r.cfg, s.cc, r.cfg.networkDir, macService, atpl, invoiceRegistry,
		s.htlcSwitch, r.cfg.ActiveNetParams.Params, s.chanRouter,
		routerBackend, s.nodeSigner, s.graphDB, s.chanStateDB,
		s.sweeper, tower, s.towerClientMgr, r.cfg.net.ResolveTCPAddr,
		genInvoiceFeatures, genAmpInvoiceFeatures,
		s.getNodeAnnouncement, s.updateAndBroadcastSelfNode, parseAddr,
		rpcsLog, s.aliasMgr, r.implCfg.AuxDataParser,
		invoiceHtlcModifier,
	)
	if err != nil {
		return err
	}

	// Now that the sub-servers have all their dependencies in place, we
	// can create each sub-server!
	for _, subServerInstance := range r.subGrpcHandlers {
		subServer, macPerms, err := subServerInstance.CreateSubServer(
			subServerCgs,
		)
		if err != nil {
			return err
		}

		// We'll collect the sub-server, and also the set of
		// permissions it needs for macaroons so we can apply the
		// interceptors below.
		subServers = append(subServers, subServer)
		subServerPerms = append(subServerPerms, macPerms)
	}

	// Next, we need to merge the set of sub server macaroon permissions
	// with the main RPC server permissions so we can unite them under a
	// single set of interceptors.
	for m, ops := range MainRPCServerPermissions() {
		err := r.interceptorChain.AddPermission(m, ops)
		if err != nil {
			return err
		}
	}

	for _, subServerPerm := range subServerPerms {
		for method, ops := range subServerPerm {
			err := r.interceptorChain.AddPermission(method, ops)
			if err != nil {
				return err
			}
		}
	}

	// External subserver possibly need to register their own permissions
	// and macaroon validator.
	for method, ops := range r.implCfg.ExternalValidator.Permissions() {
		err := r.interceptorChain.AddPermission(method, ops)
		if err != nil {
			return err
		}

		// Give the external subservers the possibility to also use
		// their own validator to check any macaroons attached to calls
		// to this method. This allows them to have their own root key
		// ID database and permission entities.
		err = macService.RegisterExternalValidator(
			method, r.implCfg.ExternalValidator,
		)
		if err != nil {
			return fmt.Errorf("could not register external "+
				"macaroon validator: %v", err)
		}
	}

	// Finally, with all the set up complete, add the last dependencies to
	// the rpc server.
	r.server = s
	r.subServers = subServers
	r.routerBackend = routerBackend
	r.chanPredicate = chanPredicate
	r.macService = macService
	r.selfNode = selfNode.PubKeyBytes

	graphCacheDuration := r.cfg.Caches.RPCGraphCacheDuration
	if graphCacheDuration != 0 {
		r.graphCacheEvictor = time.NewTimer(graphCacheDuration)

		go func() {
			for {
				select {
				// The timer fired, so we'll purge the graph
				// cache.
				case <-r.graphCacheEvictor.C:
					r.graphCache.Lock()
					r.describeGraphResp = nil
					r.graphCache.Unlock()

					// Reset the timer so we'll fire
					// again after the specified
					// duration.
					r.graphCacheEvictor.Reset(
						graphCacheDuration,
					)

				// The server is quitting, so we'll stop the
				// timer and exit.
				case <-r.quit:
					if !r.graphCacheEvictor.Stop() {
						// Drain the channel if Stop()
						// returns false, meaning the
						// timer has already fired.
						<-r.graphCacheEvictor.C
					}

					return
				}
			}
		}()
	}

	return nil
}

// RegisterWithGrpcServer registers the rpcServer and any subservers with the
// root gRPC server.
func (r *rpcServer) RegisterWithGrpcServer(grpcServer *grpc.Server) error {
	// Register the main RPC server.
	lnrpc.RegisterLightningServer(grpcServer, r)

	// Now the main RPC server has been registered, we'll iterate through
	// all the sub-RPC servers and register them to ensure that requests
	// are properly routed towards them.
	for _, subServer := range r.subGrpcHandlers {
		err := subServer.RegisterWithRootServer(grpcServer)
		if err != nil {
			return fmt.Errorf("unable to register "+
				"sub-server with root: %v", err)
		}
	}

	// Before actually listening on the gRPC listener, give external
	// subservers the chance to register to our gRPC server. Those external
	// subservers (think GrUB) are responsible for starting/stopping on
	// their own, we just let them register their services to the same
	// server instance so all of them can be exposed on the same
	// port/listener.
	err := r.implCfg.RegisterGrpcSubserver(grpcServer)
	if err != nil {
		rpcsLog.Errorf("error registering external gRPC "+
			"subserver: %v", err)
	}

	return nil
}

// Start launches any helper goroutines required for the rpcServer to function.
func (r *rpcServer) Start() error {
	if atomic.AddInt32(&r.started, 1) != 1 {
		return nil
	}

	// First, we'll start all the sub-servers to ensure that they're ready
	// to take new requests in.
	//
	// TODO(roasbeef): some may require that the entire daemon be started
	// at that point
	for _, subServer := range r.subServers {
		rpcsLog.Debugf("Starting sub RPC server: %v", subServer.Name())

		if err := subServer.Start(); err != nil {
			return err
		}
	}

	return nil
}

// RegisterWithRestProxy registers the RPC server and any subservers with the
// given REST proxy.
func (r *rpcServer) RegisterWithRestProxy(restCtx context.Context,
	restMux *proxy.ServeMux, restDialOpts []grpc.DialOption,
	restProxyDest string) error {

	// With our custom REST proxy mux created, register our main RPC and
	// give all subservers a chance to register as well.
	err := lnrpc.RegisterLightningHandlerFromEndpoint(
		restCtx, restMux, restProxyDest, restDialOpts,
	)
	if err != nil {
		return err
	}

	// Register our State service with the REST proxy.
	err = lnrpc.RegisterStateHandlerFromEndpoint(
		restCtx, restMux, restProxyDest, restDialOpts,
	)
	if err != nil {
		return err
	}

	// Register all the subservers with the REST proxy.
	for _, subServer := range r.subGrpcHandlers {
		err := subServer.RegisterWithRestServer(
			restCtx, restMux, restProxyDest, restDialOpts,
		)
		if err != nil {
			return fmt.Errorf("unable to register REST sub-server "+
				"with root: %v", err)
		}
	}

	// Before listening on any of the interfaces, we also want to give the
	// external subservers a chance to register their own REST proxy stub
	// with our mux instance.
	err = r.implCfg.RegisterRestSubserver(
		restCtx, restMux, restProxyDest, restDialOpts,
	)
	if err != nil {
		rpcsLog.Errorf("error registering external REST subserver: %v",
			err)
	}
	return nil
}

// Stop signals any active goroutines for a graceful closure.
func (r *rpcServer) Stop() error {
	if atomic.AddInt32(&r.shutdown, 1) != 1 {
		return nil
	}

	rpcsLog.Infof("Stopping RPC Server")

	close(r.quit)

	// After we've signalled all of our active goroutines to exit, we'll
	// then do the same to signal a graceful shutdown of all the sub
	// servers.
	for _, subServer := range r.subServers {
		rpcsLog.Infof("Stopping %v Sub-RPC Server",
			subServer.Name())

		if err := subServer.Stop(); err != nil {
			rpcsLog.Errorf("unable to stop sub-server %v: %v",
				subServer.Name(), err)
			continue
		}
	}

	return nil
}

// addrPairsToOutputs converts a map describing a set of outputs to be created,
// the outputs themselves. The passed map pairs up an address, to a desired
// output value amount. Each address is converted to its corresponding pkScript
// to be used within the constructed output(s).
func addrPairsToOutputs(addrPairs map[string]int64,
	params *chaincfg.Params) ([]*wire.TxOut, error) {

	outputs := make([]*wire.TxOut, 0, len(addrPairs))
	for addr, amt := range addrPairs {
		addr, err := btcutil.DecodeAddress(addr, params)
		if err != nil {
			return nil, err
		}

		if !addr.IsForNet(params) {
			return nil, fmt.Errorf("address is not for %s",
				params.Name)
		}

		pkscript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}

		outputs = append(outputs, wire.NewTxOut(amt, pkscript))
	}

	return outputs, nil
}

// allowCORS wraps the given http.Handler with a function that adds the
// Access-Control-Allow-Origin header to the response.
func allowCORS(handler http.Handler, origins []string) http.Handler {
	allowHeaders := "Access-Control-Allow-Headers"
	allowMethods := "Access-Control-Allow-Methods"
	allowOrigin := "Access-Control-Allow-Origin"

	// If the user didn't supply any origins that means CORS is disabled
	// and we should return the original handler.
	if len(origins) == 0 {
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Skip everything if the browser doesn't send the Origin field.
		if origin == "" {
			handler.ServeHTTP(w, r)
			return
		}

		// Set the static header fields first.
		w.Header().Set(
			allowHeaders,
			"Content-Type, Accept, Grpc-Metadata-Macaroon",
		)
		w.Header().Set(allowMethods, "GET, POST, DELETE")

		// Either we allow all origins or the incoming request matches
		// a specific origin in our list of allowed origins.
		for _, allowedOrigin := range origins {
			if allowedOrigin == "*" || origin == allowedOrigin {
				// Only set allowed origin to requested origin.
				w.Header().Set(allowOrigin, origin)

				break
			}
		}

		// For a pre-flight request we only need to send the headers
		// back. No need to call the rest of the chain.
		if r.Method == "OPTIONS" {
			return
		}

		// Everything's prepared now, we can pass the request along the
		// chain of handlers.
		handler.ServeHTTP(w, r)
	})
}

// sendCoinsOnChain makes an on-chain transaction in or to send coins to one or
// more addresses specified in the passed payment map. The payment map maps an
// address to a specified output value to be sent to that address.
func (r *rpcServer) sendCoinsOnChain(paymentMap map[string]int64,
	feeRate chainfee.SatPerKWeight, minConfs int32, label string,
	strategy wallet.CoinSelectionStrategy,
	selectedUtxos fn.Set[wire.OutPoint]) (*chainhash.Hash, error) {

	outputs, err := addrPairsToOutputs(paymentMap, r.cfg.ActiveNetParams.Params)
	if err != nil {
		return nil, err
	}

	// We first do a dry run, to sanity check we won't spend our wallet
	// balance below the reserved amount.
	authoredTx, err := r.server.cc.Wallet.CreateSimpleTx(
		selectedUtxos, outputs, feeRate, minConfs, strategy, true,
	)
	if err != nil {
		return nil, err
	}

	// Check the authored transaction and use the explicitly set change index
	// to make sure that the wallet reserved balance is not invalidated.
	_, err = r.server.cc.Wallet.CheckReservedValueTx(
		lnwallet.CheckReservedValueTxReq{
			Tx:          authoredTx.Tx,
			ChangeIndex: &authoredTx.ChangeIndex,
		},
	)
	if err != nil {
		return nil, err
	}

	// If that checks out, we're fairly confident that creating sending to
	// these outputs will keep the wallet balance above the reserve.
	tx, err := r.server.cc.Wallet.SendOutputs(
		selectedUtxos, outputs, feeRate, minConfs, label, strategy,
	)
	if err != nil {
		return nil, err
	}

	txHash := tx.TxHash()
	return &txHash, nil
}

// ListUnspent returns useful information about each unspent output owned by
// the wallet, as reported by the underlying `ListUnspentWitness`; the
// information returned is: outpoint, amount in satoshis, address, address
// type, scriptPubKey in hex and number of confirmations.  The result is
// filtered to contain outputs whose number of confirmations is between a
// minimum and maximum number of confirmations specified by the user, with
// 0 meaning unconfirmed.
func (r *rpcServer) ListUnspent(ctx context.Context,
	in *lnrpc.ListUnspentRequest) (*lnrpc.ListUnspentResponse, error) {

	// Validate the confirmation arguments.
	minConfs, maxConfs, err := lnrpc.ParseConfs(in.MinConfs, in.MaxConfs)
	if err != nil {
		return nil, err
	}

	// With our arguments validated, we'll query the internal wallet for
	// the set of UTXOs that match our query.
	//
	// We'll acquire the global coin selection lock to ensure there aren't
	// any other concurrent processes attempting to lock any UTXOs which may
	// be shown available to us.
	var utxos []*lnwallet.Utxo
	err = r.server.cc.Wallet.WithCoinSelectLock(func() error {
		utxos, err = r.server.cc.Wallet.ListUnspentWitness(
			minConfs, maxConfs, in.Account,
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	rpcUtxos, err := lnrpc.MarshalUtxos(utxos, r.cfg.ActiveNetParams.Params)
	if err != nil {
		return nil, err
	}

	maxStr := ""
	if maxConfs != math.MaxInt32 {
		maxStr = " max=" + fmt.Sprintf("%d", maxConfs)
	}

	rpcsLog.Debugf("[listunspent] min=%v%v, generated utxos: %v", minConfs,
		maxStr, utxos)

	return &lnrpc.ListUnspentResponse{
		Utxos: rpcUtxos,
	}, nil
}

// EstimateFee handles a request for estimating the fee for sending a
// transaction spending to multiple specified outputs in parallel.
func (r *rpcServer) EstimateFee(ctx context.Context,
	in *lnrpc.EstimateFeeRequest) (*lnrpc.EstimateFeeResponse, error) {

	// Create the list of outputs we are spending to.
	outputs, err := addrPairsToOutputs(in.AddrToAmount, r.cfg.ActiveNetParams.Params)
	if err != nil {
		return nil, err
	}

	// Query the fee estimator for the fee rate for the given confirmation
	// target.
	target := in.TargetConf
	feePref := sweep.FeeEstimateInfo{
		ConfTarget: uint32(target),
	}

	// Since we are providing a fee estimation as an RPC response, there's
	// no need to set a max feerate here, so we use 0.
	feePerKw, err := feePref.Estimate(r.server.cc.FeeEstimator, 0)
	if err != nil {
		return nil, err
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the transaction should satisfy.
	minConfs, err := lnrpc.ExtractMinConfs(
		in.GetMinConfs(), in.GetSpendUnconfirmed(),
	)
	if err != nil {
		return nil, err
	}

	coinSelectionStrategy, err := lnrpc.UnmarshallCoinSelectionStrategy(
		in.CoinSelectionStrategy,
		r.server.cc.Wallet.Cfg.CoinSelectionStrategy,
	)
	if err != nil {
		return nil, err
	}

	// We will ask the wallet to create a tx using this fee rate. We set
	// dryRun=true to avoid inflating the change addresses in the db.
	var tx *txauthor.AuthoredTx
	wallet := r.server.cc.Wallet
	err = wallet.WithCoinSelectLock(func() error {
		tx, err = wallet.CreateSimpleTx(
			nil, outputs, feePerKw, minConfs, coinSelectionStrategy,
			true,
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Use the created tx to calculate the total fee.
	totalOutput := int64(0)
	for _, out := range tx.Tx.TxOut {
		totalOutput += out.Value
	}
	totalFee := int64(tx.TotalInput) - totalOutput

	resp := &lnrpc.EstimateFeeResponse{
		FeeSat:      totalFee,
		SatPerVbyte: uint64(feePerKw.FeePerVByte()),

		// Deprecated field.
		FeerateSatPerByte: int64(feePerKw.FeePerVByte()),
	}

	rpcsLog.Debugf("[estimatefee] fee estimate for conf target %d: %v",
		target, resp)

	return resp, nil
}

// maybeUseDefaultConf makes sure that when the user doesn't set either the fee
// rate or conf target, the default conf target is used.
func maybeUseDefaultConf(satPerByte int64, satPerVByte uint64,
	targetConf uint32) uint32 {

	// If the fee rate is set, there's no need to use the default conf
	// target. In this case, we just return the targetConf from the
	// request.
	if satPerByte != 0 || satPerVByte != 0 {
		return targetConf
	}

	// Return the user specified conf target if set.
	if targetConf != 0 {
		return targetConf
	}

	// If the fee rate is not set, yet the conf target is zero, the default
	// 6 will be returned.
	rpcsLog.Warnf("Expected either 'sat_per_vbyte' or 'conf_target' to " +
		"be set, using default conf of 6 instead")

	return defaultNumBlocksEstimate
}

// SendCoins executes a request to send coins to a particular address. Unlike
// SendMany, this RPC call only allows creating a single output at a time.
func (r *rpcServer) SendCoins(ctx context.Context,
	in *lnrpc.SendCoinsRequest) (*lnrpc.SendCoinsResponse, error) {

	// Keep the old behavior prior to 0.18.0 - when the user doesn't set
	// fee rate or conf target, the default conf target of 6 is used.
	targetConf := maybeUseDefaultConf(
		in.SatPerByte, in.SatPerVbyte, uint32(in.TargetConf),
	)

	// Calculate an appropriate fee rate for this transaction.
	feePerKw, err := lnrpc.CalculateFeeRate(
		uint64(in.SatPerByte), in.SatPerVbyte, // nolint:staticcheck
		targetConf, r.server.cc.FeeEstimator,
	)
	if err != nil {
		return nil, err
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the transaction should satisfy.
	minConfs, err := lnrpc.ExtractMinConfs(in.MinConfs, in.SpendUnconfirmed)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendcoins] addr=%v, amt=%v, sat/kw=%v, min_confs=%v, "+
		"send_all=%v, select_outpoints=%v",
		in.Addr, btcutil.Amount(in.Amount), int64(feePerKw), minConfs,
		in.SendAll, len(in.Outpoints))

	// Decode the address receiving the coins, we need to check whether the
	// address is valid for this network.
	targetAddr, err := btcutil.DecodeAddress(
		in.Addr, r.cfg.ActiveNetParams.Params,
	)
	if err != nil {
		return nil, err
	}

	// Make the check on the decoded address according to the active network.
	if !targetAddr.IsForNet(r.cfg.ActiveNetParams.Params) {
		return nil, fmt.Errorf("address: %v is not valid for this "+
			"network: %v", targetAddr.String(),
			r.cfg.ActiveNetParams.Params.Name)
	}

	// If the destination address parses to a valid pubkey, we assume the user
	// accidentally tried to send funds to a bare pubkey address. This check is
	// here to prevent unintended transfers.
	decodedAddr, _ := hex.DecodeString(in.Addr)
	_, err = btcec.ParsePubKey(decodedAddr)
	if err == nil {
		return nil, fmt.Errorf("cannot send coins to pubkeys")
	}

	label, err := labels.ValidateAPI(in.Label)
	if err != nil {
		return nil, err
	}

	coinSelectionStrategy, err := lnrpc.UnmarshallCoinSelectionStrategy(
		in.CoinSelectionStrategy,
		r.server.cc.Wallet.Cfg.CoinSelectionStrategy,
	)
	if err != nil {
		return nil, err
	}

	var txid *chainhash.Hash

	wallet := r.server.cc.Wallet
	maxFeeRate := r.cfg.Sweeper.MaxFeeRate.FeePerKWeight()

	var selectOutpoints fn.Set[wire.OutPoint]
	if len(in.Outpoints) != 0 {
		wireOutpoints, err := toWireOutpoints(in.Outpoints)
		if err != nil {
			return nil, fmt.Errorf("can't create outpoints "+
				"%w", err)
		}

		if fn.HasDuplicates(wireOutpoints) {
			return nil, fmt.Errorf("selected outpoints contain " +
				"duplicate values")
		}

		selectOutpoints = fn.NewSet(wireOutpoints...)
	}

	// If the send all flag is active, then we'll attempt to sweep all the
	// coins in the wallet in a single transaction (if possible),
	// otherwise, we'll respect the amount, and attempt a regular 2-output
	// send.
	if in.SendAll {
		// At this point, the amount shouldn't be set since we've been
		// instructed to sweep all the coins from the wallet.
		if in.Amount != 0 {
			return nil, fmt.Errorf("amount set while SendAll is " +
				"active")
		}

		_, bestHeight, err := r.server.cc.ChainIO.GetBestBlock()
		if err != nil {
			return nil, err
		}

		// With the sweeper instance created, we can now generate a
		// transaction that will sweep ALL outputs from the wallet in a
		// single transaction. This will be generated in a concurrent
		// safe manner, so no need to worry about locking. The tx will
		// pay to the change address created above if we needed to
		// reserve any value, the rest will go to targetAddr.
		sweepTxPkg, err := sweep.CraftSweepAllTx(
			feePerKw, maxFeeRate, uint32(bestHeight), nil,
			targetAddr, wallet, wallet, wallet.WalletController,
			r.server.cc.Signer, minConfs, selectOutpoints,
		)
		if err != nil {
			return nil, err
		}

		// Before we publish the transaction we make sure it won't
		// violate our reserved wallet value.
		var reservedVal btcutil.Amount
		err = wallet.WithCoinSelectLock(func() error {
			var err error
			reservedVal, err = wallet.CheckReservedValueTx(
				lnwallet.CheckReservedValueTxReq{
					Tx: sweepTxPkg.SweepTx,
				},
			)
			return err
		})

		// If sending everything to this address would invalidate our
		// reserved wallet balance, we create a new sweep tx, where
		// we'll send the reserved value back to our wallet.
		if err == lnwallet.ErrReservedValueInvalidated {
			sweepTxPkg.CancelSweepAttempt()

			rpcsLog.Debugf("Reserved value %v not satisfied after "+
				"send_all, trying with change output",
				reservedVal)

			// We'll request a change address from the wallet,
			// where we'll send this reserved value back to. This
			// ensures this is an address the wallet knows about,
			// allowing us to pass the reserved value check.
			changeAddr, err := r.server.cc.Wallet.NewAddress(
				lnwallet.TaprootPubkey, true,
				lnwallet.DefaultAccountName,
			)
			if err != nil {
				return nil, err
			}

			// Send the reserved value to this change address, the
			// remaining funds will go to the targetAddr.
			outputs := []sweep.DeliveryAddr{
				{
					Addr: changeAddr,
					Amt:  reservedVal,
				},
			}

			sweepTxPkg, err = sweep.CraftSweepAllTx(
				feePerKw, maxFeeRate, uint32(bestHeight),
				outputs, targetAddr, wallet, wallet,
				wallet.WalletController,
				r.server.cc.Signer, minConfs, selectOutpoints,
			)
			if err != nil {
				return nil, err
			}

			// Sanity check the new tx by re-doing the check.
			err = wallet.WithCoinSelectLock(func() error {
				_, err := wallet.CheckReservedValueTx(
					lnwallet.CheckReservedValueTxReq{
						Tx: sweepTxPkg.SweepTx,
					},
				)
				return err
			})
			if err != nil {
				sweepTxPkg.CancelSweepAttempt()

				return nil, err
			}
		} else if err != nil {
			sweepTxPkg.CancelSweepAttempt()

			return nil, err
		}

		rpcsLog.Debugf("Sweeping coins from wallet to addr=%v, "+
			"with tx=%v", in.Addr,
			lnutils.SpewLogClosure(sweepTxPkg.SweepTx))

		// As our sweep transaction was created, successfully, we'll
		// now attempt to publish it, cancelling the sweep pkg to
		// return all outputs if it fails.
		err = wallet.PublishTransaction(sweepTxPkg.SweepTx, label)
		if err != nil {
			sweepTxPkg.CancelSweepAttempt()

			return nil, fmt.Errorf("unable to broadcast sweep "+
				"transaction: %v", err)
		}

		sweepTXID := sweepTxPkg.SweepTx.TxHash()
		txid = &sweepTXID
	} else {

		// We'll now construct out payment map, and use the wallet's
		// coin selection synchronization method to ensure that no coin
		// selection (funding, sweep alls, other sends) can proceed
		// while we instruct the wallet to send this transaction.
		paymentMap := map[string]int64{targetAddr.String(): in.Amount}
		err := wallet.WithCoinSelectLock(func() error {
			newTXID, err := r.sendCoinsOnChain(
				paymentMap, feePerKw, minConfs, label,
				coinSelectionStrategy, selectOutpoints,
			)
			if err != nil {
				return err
			}

			txid = newTXID

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	rpcsLog.Infof("[sendcoins] spend generated txid: %v", txid.String())

	return &lnrpc.SendCoinsResponse{Txid: txid.String()}, nil
}

// SendMany handles a request for a transaction create multiple specified
// outputs in parallel.
func (r *rpcServer) SendMany(ctx context.Context,
	in *lnrpc.SendManyRequest) (*lnrpc.SendManyResponse, error) {

	// Keep the old behavior prior to 0.18.0 - when the user doesn't set
	// fee rate or conf target, the default conf target of 6 is used.
	targetConf := maybeUseDefaultConf(
		in.SatPerByte, in.SatPerVbyte, uint32(in.TargetConf),
	)

	// Calculate an appropriate fee rate for this transaction.
	feePerKw, err := lnrpc.CalculateFeeRate(
		uint64(in.SatPerByte), in.SatPerVbyte, // nolint:staticcheck
		targetConf, r.server.cc.FeeEstimator,
	)
	if err != nil {
		return nil, err
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the transaction should satisfy.
	minConfs, err := lnrpc.ExtractMinConfs(in.MinConfs, in.SpendUnconfirmed)
	if err != nil {
		return nil, err
	}

	label, err := labels.ValidateAPI(in.Label)
	if err != nil {
		return nil, err
	}

	coinSelectionStrategy, err := lnrpc.UnmarshallCoinSelectionStrategy(
		in.CoinSelectionStrategy,
		r.server.cc.Wallet.Cfg.CoinSelectionStrategy,
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendmany] outputs=%v, sat/kw=%v",
		lnutils.SpewLogClosure(in.AddrToAmount), int64(feePerKw))

	var txid *chainhash.Hash

	// We'll attempt to send to the target set of outputs, ensuring that we
	// synchronize with any other ongoing coin selection attempts which
	// happen to also be concurrently executing.
	wallet := r.server.cc.Wallet
	err = wallet.WithCoinSelectLock(func() error {
		sendManyTXID, err := r.sendCoinsOnChain(
			in.AddrToAmount, feePerKw, minConfs, label,
			coinSelectionStrategy, nil,
		)
		if err != nil {
			return err
		}

		txid = sendManyTXID

		return nil
	})
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[sendmany] spend generated txid: %v", txid.String())

	return &lnrpc.SendManyResponse{Txid: txid.String()}, nil
}

// NewAddress creates a new address under control of the local wallet.
func (r *rpcServer) NewAddress(ctx context.Context,
	in *lnrpc.NewAddressRequest) (*lnrpc.NewAddressResponse, error) {

	// Always use the default wallet account unless one was specified.
	account := lnwallet.DefaultAccountName
	if in.Account != "" {
		account = in.Account
	}

	// Translate the gRPC proto address type to the wallet controller's
	// available address types.
	var (
		addr btcutil.Address
		err  error
	)
	switch in.Type {
	case lnrpc.AddressType_WITNESS_PUBKEY_HASH:
		addr, err = r.server.cc.Wallet.NewAddress(
			lnwallet.WitnessPubKey, false, account,
		)
		if err != nil {
			return nil, err
		}

	case lnrpc.AddressType_NESTED_PUBKEY_HASH:
		addr, err = r.server.cc.Wallet.NewAddress(
			lnwallet.NestedWitnessPubKey, false, account,
		)
		if err != nil {
			return nil, err
		}

	case lnrpc.AddressType_TAPROOT_PUBKEY:
		addr, err = r.server.cc.Wallet.NewAddress(
			lnwallet.TaprootPubkey, false, account,
		)
		if err != nil {
			return nil, err
		}

	case lnrpc.AddressType_UNUSED_WITNESS_PUBKEY_HASH:
		addr, err = r.server.cc.Wallet.LastUnusedAddress(
			lnwallet.WitnessPubKey, account,
		)
		if err != nil {
			return nil, err
		}

	case lnrpc.AddressType_UNUSED_NESTED_PUBKEY_HASH:
		addr, err = r.server.cc.Wallet.LastUnusedAddress(
			lnwallet.NestedWitnessPubKey, account,
		)
		if err != nil {
			return nil, err
		}

	case lnrpc.AddressType_UNUSED_TAPROOT_PUBKEY:
		addr, err = r.server.cc.Wallet.LastUnusedAddress(
			lnwallet.TaprootPubkey, account,
		)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unknown address type: %v", in.Type)
	}

	rpcsLog.Debugf("[newaddress] account=%v type=%v addr=%v", account,
		in.Type, addr.String())
	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}

var (
	// signedMsgPrefix is a special prefix that we'll prepend to any
	// messages we sign/verify. We do this to ensure that we don't
	// accidentally sign a sighash, or other sensitive material. By
	// prepending this fragment, we mind message signing to our particular
	// context.
	signedMsgPrefix = []byte("Lightning Signed Message:")
)

// SignMessage signs a message with the resident node's private key. The
// returned signature string is zbase32 encoded and pubkey recoverable, meaning
// that only the message digest and signature are needed for verification.
func (r *rpcServer) SignMessage(_ context.Context,
	in *lnrpc.SignMessageRequest) (*lnrpc.SignMessageResponse, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("need a message to sign")
	}

	in.Msg = append(signedMsgPrefix, in.Msg...)
	sigBytes, err := r.server.nodeSigner.SignMessageCompact(
		in.Msg, !in.SingleHash,
	)
	if err != nil {
		return nil, err
	}

	sig := zbase32.EncodeToString(sigBytes)
	return &lnrpc.SignMessageResponse{Signature: sig}, nil
}

// VerifyMessage verifies a signature over a msg. The signature must be zbase32
// encoded and signed by an active node in the resident node's channel
// database. In addition to returning the validity of the signature,
// VerifyMessage also returns the recovered pubkey from the signature.
func (r *rpcServer) VerifyMessage(ctx context.Context,
	in *lnrpc.VerifyMessageRequest) (*lnrpc.VerifyMessageResponse, error) {

	if in.Msg == nil {
		return nil, fmt.Errorf("need a message to verify")
	}

	// The signature should be zbase32 encoded
	sig, err := zbase32.DecodeString(in.Signature)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %w", err)
	}

	// The signature is over the double-sha256 hash of the message.
	in.Msg = append(signedMsgPrefix, in.Msg...)
	digest := chainhash.DoubleHashB(in.Msg)

	// RecoverCompact both recovers the pubkey and validates the signature.
	pubKey, _, err := ecdsa.RecoverCompact(sig, digest)
	if err != nil {
		return &lnrpc.VerifyMessageResponse{Valid: false}, nil
	}
	pubKeyHex := hex.EncodeToString(pubKey.SerializeCompressed())

	var pub [33]byte
	copy(pub[:], pubKey.SerializeCompressed())

	// Query the channel graph to ensure a node in the network with active
	// channels signed the message.
	//
	// TODO(phlip9): Require valid nodes to have capital in active channels.
	graph := r.server.graphDB
	_, active, err := graph.HasNode(ctx, pub)
	if err != nil {
		return nil, fmt.Errorf("failed to query graph: %w", err)
	}

	return &lnrpc.VerifyMessageResponse{
		Valid:  active,
		Pubkey: pubKeyHex,
	}, nil
}

// ConnectPeer attempts to establish a connection to a remote peer.
func (r *rpcServer) ConnectPeer(ctx context.Context,
	in *lnrpc.ConnectPeerRequest) (*lnrpc.ConnectPeerResponse, error) {

	// The server hasn't yet started, so it won't be able to service any of
	// our requests, so we'll bail early here.
	if !r.server.Started() {
		return nil, ErrServerNotActive
	}

	if in.Addr == nil {
		return nil, fmt.Errorf("need: lnc pubkeyhash@hostname")
	}

	pubkeyHex, err := hex.DecodeString(in.Addr.Pubkey)
	if err != nil {
		return nil, err
	}
	pubKey, err := btcec.ParsePubKey(pubkeyHex)
	if err != nil {
		return nil, err
	}

	// Connections to ourselves are disallowed for obvious reasons.
	if pubKey.IsEqual(r.server.identityECDH.PubKey()) {
		return nil, fmt.Errorf("cannot make connection to self")
	}

	addr, err := parseAddr(in.Addr.Host, r.cfg.net)
	if err != nil {
		return nil, err
	}

	peerAddr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     addr,
		ChainNet:    r.cfg.ActiveNetParams.Net,
	}

	rpcsLog.Debugf("[connectpeer] requested connection to %x@%s",
		peerAddr.IdentityKey.SerializeCompressed(), peerAddr.Address)

	// By default, we will use the global connection timeout value.
	timeout := r.cfg.ConnectionTimeout

	// Check if the connection timeout is set. If set, we will use it in our
	// request.
	if in.Timeout != 0 {
		timeout = time.Duration(in.Timeout) * time.Second
		rpcsLog.Debugf("[connectpeer] connection timeout is set to %v",
			timeout)
	}

	if err := r.server.ConnectToPeer(
		peerAddr, in.Perm, timeout,
	); err != nil {
		rpcsLog.Errorf("[connectpeer]: error connecting to peer: %v",
			err)
		return nil, err
	}

	rpcsLog.Debugf("Connected to peer: %v", peerAddr.String())

	return &lnrpc.ConnectPeerResponse{
		Status: fmt.Sprintf("connection to %v initiated",
			peerAddr.String()),
	}, nil
}

// DisconnectPeer attempts to disconnect one peer from another identified by a
// given pubKey. In the case that we currently have a pending or active channel
// with the target peer, this action will be disallowed.
func (r *rpcServer) DisconnectPeer(ctx context.Context,
	in *lnrpc.DisconnectPeerRequest) (*lnrpc.DisconnectPeerResponse, error) {

	rpcsLog.Debugf("[disconnectpeer] from peer(%s)", in.PubKey)

	if !r.server.Started() {
		return nil, ErrServerNotActive
	}

	// First we'll validate the string passed in within the request to
	// ensure that it's a valid hex-string, and also a valid compressed
	// public key.
	pubKeyBytes, err := hex.DecodeString(in.PubKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode pubkey bytes: %w", err)
	}
	peerPubKey, err := btcec.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("unable to parse pubkey: %w", err)
	}

	// Next, we'll fetch the pending/active channels we have with a
	// particular peer.
	nodeChannels, err := r.server.chanStateDB.FetchOpenChannels(peerPubKey)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channels for peer: %w",
			err)
	}

	// In order to avoid erroneously disconnecting from a peer that we have
	// an active channel with, if we have any channels active with this
	// peer, then we'll disallow disconnecting from them in certain
	// situations.
	if len(nodeChannels) != 0 {
		// If the configured dev value `unsafedisconnect` is false, we
		// return an error since there are active channels. For
		// production environments, we allow disconnecting from a peer
		// even if there are channels active with them.
		if !r.cfg.Dev.GetUnsafeDisconnect() {
			return nil, fmt.Errorf("cannot disconnect from "+
				"peer(%x), still has %d active channels",
				pubKeyBytes, len(nodeChannels))
		}

		// We are in a dev environment, print a warning log and
		// disconnect.
		rpcsLog.Warnf("UnsafeDisconnect mode, disconnecting from "+
			"peer(%x) while there are %d active channels",
			pubKeyBytes, len(nodeChannels))
	}

	// With all initial validation complete, we'll now request that the
	// server disconnects from the peer.
	err = r.server.DisconnectPeer(peerPubKey)
	if err != nil {
		return nil, fmt.Errorf("unable to disconnect peer: %w", err)
	}

	return &lnrpc.DisconnectPeerResponse{
		Status: "disconnect initiated",
	}, nil
}

// newFundingShimAssembler returns a new fully populated
// chanfunding.CannedAssembler using a FundingShim obtained from an RPC caller.
func newFundingShimAssembler(chanPointShim *lnrpc.ChanPointShim, initiator bool,
	keyRing keychain.KeyRing) (chanfunding.Assembler, error) {

	// Perform some basic sanity checks to ensure that all the expected
	// fields are populated.
	switch {
	case chanPointShim.RemoteKey == nil:
		return nil, fmt.Errorf("remote key not set")

	case chanPointShim.LocalKey == nil:
		return nil, fmt.Errorf("local key desc not set")

	case chanPointShim.LocalKey.RawKeyBytes == nil:
		return nil, fmt.Errorf("local raw key bytes not set")

	case chanPointShim.LocalKey.KeyLoc == nil:
		return nil, fmt.Errorf("local key loc not set")

	case chanPointShim.ChanPoint == nil:
		return nil, fmt.Errorf("chan point not set")

	case len(chanPointShim.PendingChanId) != 32:
		return nil, fmt.Errorf("pending chan ID not set")
	}

	// First, we'll map the RPC's channel point to one we can actually use.
	index := chanPointShim.ChanPoint.OutputIndex
	txid, err := lnrpc.GetChanPointFundingTxid(chanPointShim.ChanPoint)
	if err != nil {
		return nil, err
	}
	chanPoint := wire.NewOutPoint(txid, index)

	// Next we'll parse out the remote party's funding key, as well as our
	// full key descriptor.
	remoteKey, err := btcec.ParsePubKey(chanPointShim.RemoteKey)
	if err != nil {
		return nil, err
	}

	shimKeyDesc := chanPointShim.LocalKey
	localKey, err := btcec.ParsePubKey(shimKeyDesc.RawKeyBytes)
	if err != nil {
		return nil, err
	}
	localKeyDesc := keychain.KeyDescriptor{
		PubKey: localKey,
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(
				shimKeyDesc.KeyLoc.KeyFamily,
			),
			Index: uint32(shimKeyDesc.KeyLoc.KeyIndex),
		},
	}

	// Verify that if we re-derive this key according to the passed
	// KeyLocator, that we get the exact same key back. Otherwise, we may
	// end up in a situation where we aren't able to actually sign for this
	// newly created channel.
	derivedKey, err := keyRing.DeriveKey(localKeyDesc.KeyLocator)
	if err != nil {
		return nil, err
	}
	if !derivedKey.PubKey.IsEqual(localKey) {
		return nil, fmt.Errorf("KeyLocator does not match attached " +
			"raw pubkey")
	}

	// With all the parts assembled, we can now make the canned assembler
	// to pass into the wallet.
	//
	// TODO(roasbeef): update to support musig2
	return chanfunding.NewCannedAssembler(
		chanPointShim.ThawHeight, *chanPoint,
		btcutil.Amount(chanPointShim.Amt), &localKeyDesc,
		remoteKey, initiator, chanPointShim.Musig2,
	), nil
}

// newPsbtAssembler returns a new fully populated
// chanfunding.PsbtAssembler using a FundingShim obtained from an RPC caller.
func newPsbtAssembler(req *lnrpc.OpenChannelRequest,
	psbtShim *lnrpc.PsbtShim, netParams *chaincfg.Params) (
	chanfunding.Assembler, error) {

	var (
		packet *psbt.Packet
		err    error
	)

	// Perform some basic sanity checks to ensure that all the expected
	// fields are populated and none of the incompatible fields are.
	if len(psbtShim.PendingChanId) != 32 {
		return nil, fmt.Errorf("pending chan ID not set")
	}
	if req.SatPerByte != 0 || req.SatPerVbyte != 0 || req.TargetConf != 0 { // nolint:staticcheck
		return nil, fmt.Errorf("specifying fee estimation parameters " +
			"is not supported for PSBT funding")
	}

	// The base PSBT is optional. But if it's set, it has to be a valid,
	// binary serialized PSBT.
	if len(psbtShim.BasePsbt) > 0 {
		packet, err = psbt.NewFromRawBytes(
			bytes.NewReader(psbtShim.BasePsbt), false,
		)
		if err != nil {
			return nil, fmt.Errorf("error parsing base PSBT: %w",
				err)
		}
	}

	// With all the parts assembled, we can now make the canned assembler
	// to pass into the wallet.
	return chanfunding.NewPsbtAssembler(
		btcutil.Amount(req.LocalFundingAmount), packet, netParams,
		!psbtShim.NoPublish,
	), nil
}

// canOpenChannel returns an error if the necessary subsystems for channel
// funding are not ready.
func (r *rpcServer) canOpenChannel() error {
	// We can't open a channel until the main server has started.
	if !r.server.Started() {
		return ErrServerNotActive
	}

	// Creation of channels before the wallet syncs up is currently
	// disallowed.
	isSynced, _, err := r.server.cc.Wallet.IsSynced()
	if err != nil {
		return err
	}
	if !isSynced {
		return errors.New("channels cannot be created before the " +
			"wallet is fully synced")
	}

	return nil
}

// parseOpenChannelReq parses an OpenChannelRequest message into an InitFundingMsg
// struct. The logic is abstracted so that it can be shared between OpenChannel
// and OpenChannelSync.
func (r *rpcServer) parseOpenChannelReq(in *lnrpc.OpenChannelRequest,
	isSync bool) (*funding.InitFundingMsg, error) {

	rpcsLog.Debugf("[openchannel] request to NodeKey(%x) "+
		"allocation(us=%v, them=%v)", in.NodePubkey,
		in.LocalFundingAmount, in.PushSat)

	localFundingAmt := btcutil.Amount(in.LocalFundingAmount)
	remoteInitialBalance := btcutil.Amount(in.PushSat)

	// If we are not committing the maximum viable balance towards a channel
	// then the local funding amount must be specified. In case FundMax is
	// set the funding amount is specified as the interval between minimum
	// funding amount and by the configured maximum channel size.
	if !in.FundMax && localFundingAmt == 0 {
		return nil, fmt.Errorf("local funding amount must be non-zero")
	}

	// Ensure that the initial balance of the remote party (if pushing
	// satoshis) does not exceed the amount the local party has requested
	// for funding. This is only checked if we are not committing the
	// maximum viable amount towards the channel balance. If we do commit
	// the maximum then the remote balance is checked in a dedicated FundMax
	// check.
	if !in.FundMax && remoteInitialBalance >= localFundingAmt {
		return nil, fmt.Errorf("amount pushed to remote peer for " +
			"initial state must be below the local funding amount")
	}

	// We either allow the fundmax or the psbt flow hence we return an error
	// if both are set.
	if in.FundingShim != nil && in.FundMax {
		return nil, fmt.Errorf("cannot provide a psbt funding shim " +
			"while committing the maximum wallet balance towards " +
			"the channel opening")
	}

	// If the FundMax flag is set, ensure that the acceptable minimum local
	// amount adheres to the amount to be pushed to the remote, and to
	// current rules, while also respecting the settings for the maximum
	// channel size.
	var minFundAmt, fundUpToMaxAmt btcutil.Amount
	if in.FundMax {
		// We assume the configured maximum channel size to be the upper
		// bound of our "maxed" out funding attempt.
		fundUpToMaxAmt = btcutil.Amount(r.cfg.MaxChanSize)

		// Since the standard non-fundmax flow requires the minimum
		// funding amount to be at least in the amount of the initial
		// remote balance(push amount) we need to adjust the minimum
		// funding amount accordingly. We initially assume the minimum
		// allowed channel size as minimum funding amount.
		minFundAmt = funding.MinChanFundingSize

		// If minFundAmt is less than the initial remote balance we
		// simply assign the initial remote balance to minFundAmt in
		// order to fullfil the criterion. Whether or not this so
		// determined minimum amount is actually available is
		// ascertained downstream in the lnwallet's reservation
		// workflow.
		if remoteInitialBalance >= minFundAmt {
			minFundAmt = remoteInitialBalance
		}
	}

	minHtlcIn := lnwire.MilliSatoshi(in.MinHtlcMsat)
	remoteCsvDelay := uint16(in.RemoteCsvDelay)
	maxValue := lnwire.MilliSatoshi(in.RemoteMaxValueInFlightMsat)
	maxHtlcs := uint16(in.RemoteMaxHtlcs)
	remoteChanReserve := btcutil.Amount(in.RemoteChanReserveSat)

	globalFeatureSet := r.server.featureMgr.Get(feature.SetNodeAnn)

	// Determine if the user provided channel fees
	// and if so pass them on to the funding workflow.
	var channelBaseFee, channelFeeRate *uint64
	if in.UseBaseFee {
		channelBaseFee = &in.BaseFee
	}
	if in.UseFeeRate {
		channelFeeRate = &in.FeeRate
	}

	// Ensure that the remote channel reserve does not exceed 20% of the
	// channel capacity.
	if !in.FundMax && remoteChanReserve >= localFundingAmt/5 {
		return nil, fmt.Errorf("remote channel reserve must be less " +
			"than the %%20 of the channel capacity")
	}

	// Ensure that the user doesn't exceed the current soft-limit for
	// channel size. If the funding amount is above the soft-limit, then
	// we'll reject the request.
	// If the FundMax flag is set the local amount is determined downstream
	// in the wallet hence we do not check it here against the maximum
	// funding amount. Only if the localFundingAmt is specified we can check
	// if it exceeds the maximum funding amount.
	wumboEnabled := globalFeatureSet.HasFeature(
		lnwire.WumboChannelsOptional,
	)
	if !in.FundMax && !wumboEnabled && localFundingAmt > MaxFundingAmount {
		return nil, fmt.Errorf("funding amount is too large, the max "+
			"channel size is: %v", MaxFundingAmount)
	}

	// Restrict the size of the channel we'll actually open. At a later
	// level, we'll ensure that the output we create, after accounting for
	// fees, does not leave a dust output. In case of the FundMax flow
	// dedicated checks ensure that the lower boundary of the channel size
	// is at least in the amount of MinChanFundingSize or potentially higher
	// if a remote balance is specified.
	if !in.FundMax && localFundingAmt < funding.MinChanFundingSize {
		return nil, fmt.Errorf("channel is too small, the minimum "+
			"channel size is: %v SAT", int64(funding.MinChanFundingSize))
	}

	// Prevent users from submitting a max-htlc value that would exceed the
	// protocol maximum.
	if maxHtlcs > input.MaxHTLCNumber/2 {
		return nil, fmt.Errorf("remote-max-htlcs (%v) cannot be "+
			"greater than %v", maxHtlcs, input.MaxHTLCNumber/2)
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the channel's funding transaction should
	// satisfy.
	minConfs, err := lnrpc.ExtractMinConfs(in.MinConfs, in.SpendUnconfirmed)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): also return channel ID?

	var nodePubKey *btcec.PublicKey

	// Parse the remote pubkey the NodePubkey field of the request. If it's
	// not present, we'll fallback to the deprecated version that parses the
	// key from a hex string if this is for REST for backwards compatibility.
	switch {
	// Parse the raw bytes of the node key into a pubkey object so we can
	// easily manipulate it.
	case len(in.NodePubkey) > 0:
		nodePubKey, err = btcec.ParsePubKey(in.NodePubkey)
		if err != nil {
			return nil, err
		}

	// Decode the provided target node's public key, parsing it into a pub
	// key object. For all sync call, byte slices are expected to be encoded
	// as hex strings.
	case isSync:
		keyBytes, err := hex.DecodeString(in.NodePubkeyString) // nolint:staticcheck
		if err != nil {
			return nil, err
		}

		nodePubKey, err = btcec.ParsePubKey(keyBytes)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("NodePubkey is not set")
	}

	// Making a channel to ourselves wouldn't be of any use, so we
	// explicitly disallow them.
	if nodePubKey.IsEqual(r.server.identityECDH.PubKey()) {
		return nil, fmt.Errorf("cannot open channel to self")
	}

	// NOTE: We also need to do the fee rate calculation for the psbt
	// funding flow because the `batchfund` depends on it.
	targetConf := maybeUseDefaultConf(
		in.SatPerByte, in.SatPerVbyte, uint32(in.TargetConf),
	)

	// Calculate an appropriate fee rate for this transaction.
	feeRate, err := lnrpc.CalculateFeeRate(
		uint64(in.SatPerByte), in.SatPerVbyte,
		targetConf, r.server.cc.FeeEstimator,
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Debugf("[openchannel]: using fee of %v sat/kw for "+
		"funding tx", int64(feeRate))

	script, err := chancloser.ParseUpfrontShutdownAddress(
		in.CloseAddress, r.cfg.ActiveNetParams.Params,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing upfront shutdown: %w",
			err)
	}

	var channelType *lnwire.ChannelType
	switch in.CommitmentType {
	case lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE:
		if in.ZeroConf {
			return nil, fmt.Errorf("use anchors for zero-conf")
		}

	case lnrpc.CommitmentType_LEGACY:
		channelType = new(lnwire.ChannelType)
		*channelType = lnwire.ChannelType(*lnwire.NewRawFeatureVector())

	case lnrpc.CommitmentType_STATIC_REMOTE_KEY:
		channelType = new(lnwire.ChannelType)
		*channelType = lnwire.ChannelType(*lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyRequired,
		))

	case lnrpc.CommitmentType_ANCHORS:
		channelType = new(lnwire.ChannelType)
		fv := lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyRequired,
			lnwire.AnchorsZeroFeeHtlcTxRequired,
		)

		if in.ZeroConf {
			fv.Set(lnwire.ZeroConfRequired)
		}

		if in.ScidAlias {
			fv.Set(lnwire.ScidAliasRequired)
		}

		*channelType = lnwire.ChannelType(*fv)

	case lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE:
		channelType = new(lnwire.ChannelType)
		fv := lnwire.NewRawFeatureVector(
			lnwire.StaticRemoteKeyRequired,
			lnwire.AnchorsZeroFeeHtlcTxRequired,
			lnwire.ScriptEnforcedLeaseRequired,
		)

		if in.ZeroConf {
			fv.Set(lnwire.ZeroConfRequired)
		}

		if in.ScidAlias {
			fv.Set(lnwire.ScidAliasRequired)
		}

		*channelType = lnwire.ChannelType(*fv)

	case lnrpc.CommitmentType_SIMPLE_TAPROOT:
		// If the taproot channel type is being set, then the channel
		// MUST be private (unadvertised) for now.
		if !in.Private {
			return nil, fmt.Errorf("taproot channels must be " +
				"private")
		}

		channelType = new(lnwire.ChannelType)
		fv := lnwire.NewRawFeatureVector(
			lnwire.SimpleTaprootChannelsRequiredStaging,
		)

		// TODO(roasbeef): no need for the rest as they're now
		// implicit?

		if in.ZeroConf {
			fv.Set(lnwire.ZeroConfRequired)
		}

		if in.ScidAlias {
			fv.Set(lnwire.ScidAliasRequired)
		}

		*channelType = lnwire.ChannelType(*fv)

	case lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY:
		// If the taproot overlay channel type is being set, then the
		// channel MUST be private.
		if !in.Private {
			return nil, fmt.Errorf("taproot overlay channels " +
				"must be private")
		}

		channelType = new(lnwire.ChannelType)
		fv := lnwire.NewRawFeatureVector(
			lnwire.SimpleTaprootOverlayChansRequired,
		)

		if in.ZeroConf {
			fv.Set(lnwire.ZeroConfRequired)
		}

		if in.ScidAlias {
			fv.Set(lnwire.ScidAliasRequired)
		}

		*channelType = lnwire.ChannelType(*fv)

	default:
		return nil, fmt.Errorf("unhandled request channel type %v",
			in.CommitmentType)
	}

	// We limit the channel memo to be 500 characters long. This enforces
	// a reasonable upper bound on storage consumption. This also mimics
	// the length limit for the label of a TX.
	const maxMemoLength = 500
	if len(in.Memo) > maxMemoLength {
		return nil, fmt.Errorf("provided memo (%s) is of length %d, "+
			"exceeds %d", in.Memo, len(in.Memo), maxMemoLength)
	}

	// Check, if manually selected outpoints are present to fund a channel.
	var outpoints []wire.OutPoint
	if len(in.Outpoints) > 0 {
		outpoints, err = toWireOutpoints(in.Outpoints)
		if err != nil {
			return nil, fmt.Errorf("can't create outpoints %w", err)
		}
	}

	// Instruct the server to trigger the necessary events to attempt to
	// open a new channel. A stream is returned in place, this stream will
	// be used to consume updates of the state of the pending channel.
	return &funding.InitFundingMsg{
		TargetPubkey:    nodePubKey,
		ChainHash:       *r.cfg.ActiveNetParams.GenesisHash,
		LocalFundingAmt: localFundingAmt,
		BaseFee:         channelBaseFee,
		FeeRate:         channelFeeRate,
		PushAmt: lnwire.NewMSatFromSatoshis(
			remoteInitialBalance,
		),
		MinHtlcIn:         minHtlcIn,
		FundingFeePerKw:   feeRate,
		Private:           in.Private,
		RemoteCsvDelay:    remoteCsvDelay,
		RemoteChanReserve: remoteChanReserve,
		MinConfs:          minConfs,
		ShutdownScript:    script,
		MaxValueInFlight:  maxValue,
		MaxHtlcs:          maxHtlcs,
		MaxLocalCsv:       uint16(in.MaxLocalCsv),
		ChannelType:       channelType,
		FundUpToMaxAmt:    fundUpToMaxAmt,
		MinFundAmt:        minFundAmt,
		Memo:              []byte(in.Memo),
		Outpoints:         outpoints,
	}, nil
}

// toWireOutpoints converts a list of outpoints from the rpc format to the wire
// format.
func toWireOutpoints(outpoints []*lnrpc.OutPoint) ([]wire.OutPoint, error) {
	var wireOutpoints []wire.OutPoint
	for _, outpoint := range outpoints {
		hash, err := chainhash.NewHashFromStr(outpoint.TxidStr)
		if err != nil {
			return nil, fmt.Errorf("cannot create chainhash")
		}

		wireOutpoint := wire.NewOutPoint(
			hash, outpoint.OutputIndex,
		)
		wireOutpoints = append(wireOutpoints, *wireOutpoint)
	}

	return wireOutpoints, nil
}

// OpenChannel attempts to open a singly funded channel specified in the
// request to a remote peer.
func (r *rpcServer) OpenChannel(in *lnrpc.OpenChannelRequest,
	updateStream lnrpc.Lightning_OpenChannelServer) error {

	if err := r.canOpenChannel(); err != nil {
		return err
	}

	req, err := r.parseOpenChannelReq(in, false)
	if err != nil {
		return err
	}

	// If the user has provided a shim, then we'll now augment the based
	// open channel request with this additional logic.
	if in.FundingShim != nil {
		switch {
		// If we have a chan point shim, then this means the funding
		// transaction was crafted externally. In this case we only
		// need to hand a channel point down into the wallet.
		case in.FundingShim.GetChanPointShim() != nil:
			chanPointShim := in.FundingShim.GetChanPointShim()

			// Map the channel point shim into a new
			// chanfunding.CannedAssembler that the wallet will use
			// to obtain the channel point details.
			copy(req.PendingChanID[:], chanPointShim.PendingChanId)
			req.ChanFunder, err = newFundingShimAssembler(
				chanPointShim, true, r.server.cc.KeyRing,
			)
			if err != nil {
				return err
			}

		// If we have a PSBT shim, then this means the funding
		// transaction will be crafted outside of the wallet, once the
		// funding multisig output script is known. We'll create an
		// intent that will supervise the multi-step process.
		case in.FundingShim.GetPsbtShim() != nil:
			psbtShim := in.FundingShim.GetPsbtShim()

			// Instruct the wallet to use the new
			// chanfunding.PsbtAssembler to construct the funding
			// transaction.
			copy(req.PendingChanID[:], psbtShim.PendingChanId)

			// NOTE: For the PSBT case we do also allow unconfirmed
			// utxos to fund the psbt transaction because we make
			// sure we only use stable utxos.
			req.ChanFunder, err = newPsbtAssembler(
				in, psbtShim,
				&r.server.cc.Wallet.Cfg.NetParams,
			)
			if err != nil {
				return err
			}
		}
	}

	updateChan, errChan := r.server.OpenChannel(req)

	var outpoint wire.OutPoint
out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("unable to open channel to NodeKey(%x): %v",
				req.TargetPubkey.SerializeCompressed(), err)
			return err
		case fundingUpdate := <-updateChan:
			rpcsLog.Tracef("[openchannel] sending update: %v",
				fundingUpdate)
			if err := updateStream.Send(fundingUpdate); err != nil {
				return err
			}

			// If a final channel open update is being sent, then
			// we can break out of our recv loop as we no longer
			// need to process any further updates.
			update, ok := fundingUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanOpen)
			if ok {
				chanPoint := update.ChanOpen.ChannelPoint
				txid, err := lnrpc.GetChanPointFundingTxid(chanPoint)
				if err != nil {
					return err
				}
				outpoint = wire.OutPoint{
					Hash:  *txid,
					Index: chanPoint.OutputIndex,
				}

				break out
			}
		case <-r.quit:
			return nil
		}
	}

	rpcsLog.Tracef("[openchannel] success NodeKey(%x), ChannelPoint(%v)",
		req.TargetPubkey.SerializeCompressed(), outpoint)
	return nil
}

// OpenChannelSync is a synchronous version of the OpenChannel RPC call. This
// call is meant to be consumed by clients to the REST proxy. As with all other
// sync calls, all byte slices are instead to be populated as hex encoded
// strings.
func (r *rpcServer) OpenChannelSync(ctx context.Context,
	in *lnrpc.OpenChannelRequest) (*lnrpc.ChannelPoint, error) {

	if err := r.canOpenChannel(); err != nil {
		return nil, err
	}

	req, err := r.parseOpenChannelReq(in, true)
	if err != nil {
		return nil, err
	}

	updateChan, errChan := r.server.OpenChannel(req)
	select {
	// If an error occurs them immediately return the error to the client.
	case err := <-errChan:
		rpcsLog.Errorf("unable to open channel to NodeKey(%x): %v",
			req.TargetPubkey.SerializeCompressed(), err)
		return nil, err

	// Otherwise, wait for the first channel update. The first update sent
	// is when the funding transaction is broadcast to the network.
	case fundingUpdate := <-updateChan:
		rpcsLog.Tracef("[openchannel] sending update: %v",
			fundingUpdate)

		// Parse out the txid of the pending funding transaction. The
		// sync client can use this to poll against the list of
		// PendingChannels.
		openUpdate := fundingUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
		chanUpdate := openUpdate.ChanPending

		return &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: chanUpdate.Txid,
			},
			OutputIndex: chanUpdate.OutputIndex,
		}, nil
	case <-r.quit:
		return nil, nil
	}
}

// BatchOpenChannel attempts to open multiple single-funded channels in a
// single transaction in an atomic way. This means either all channel open
// requests succeed at once or all attempts are aborted if any of them fail.
// This is the safer variant of using PSBTs to manually fund a batch of
// channels through the OpenChannel RPC.
func (r *rpcServer) BatchOpenChannel(ctx context.Context,
	in *lnrpc.BatchOpenChannelRequest) (*lnrpc.BatchOpenChannelResponse,
	error) {

	if err := r.canOpenChannel(); err != nil {
		return nil, err
	}

	// We need the wallet kit server to do the heavy lifting on the PSBT
	// part. If we didn't rely on re-using the wallet kit server's logic we
	// would need to re-implement everything here. Since we deliver lnd with
	// the wallet kit server enabled by default we can assume it's okay to
	// make this functionality dependent on that server being active.
	var walletKitServer walletrpc.WalletKitServer
	for _, subServer := range r.subServers {
		if subServer.Name() == walletrpc.SubServerName {
			walletKitServer = subServer.(walletrpc.WalletKitServer)
		}
	}
	if walletKitServer == nil {
		return nil, fmt.Errorf("batch channel open is only possible " +
			"if walletrpc subserver is active")
	}

	rpcsLog.Debugf("[batchopenchannel] request to open batch of %d "+
		"channels", len(in.Channels))

	// Make sure there is at least one channel to open. We could say we want
	// at least two channels for a batch. But maybe it's nice if developers
	// can use the same API for a single channel as well as a batch of
	// channels.
	if len(in.Channels) == 0 {
		return nil, fmt.Errorf("specify at least one channel")
	}

	// In case we remove a pending channel from the database, we need to set
	// a close height, so we'll just use the current best known height.
	_, bestHeight, err := r.server.cc.ChainIO.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("error fetching best block: %w", err)
	}

	// So far everything looks good and we can now start the heavy lifting
	// that's done in the funding package.
	requestParser := func(req *lnrpc.OpenChannelRequest) (
		*funding.InitFundingMsg, error) {

		return r.parseOpenChannelReq(req, false)
	}
	channelAbandoner := func(point *wire.OutPoint) error {
		return r.abandonChan(point, uint32(bestHeight))
	}
	batcher := funding.NewBatcher(&funding.BatchConfig{
		RequestParser:    requestParser,
		ChannelAbandoner: channelAbandoner,
		ChannelOpener:    r.server.OpenChannel,
		WalletKitServer:  walletKitServer,
		Wallet:           r.server.cc.Wallet,
		NetParams:        &r.server.cc.Wallet.Cfg.NetParams,
		Quit:             r.quit,
	})
	rpcPoints, err := batcher.BatchFund(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("batch funding failed: %w", err)
	}

	// Now all that's left to do is send back the response with the channel
	// points we created.
	return &lnrpc.BatchOpenChannelResponse{
		PendingChannels: rpcPoints,
	}, nil
}

// CloseChannel attempts to close an active channel identified by its channel
// point. The actions of this method can additionally be augmented to attempt
// a force close after a timeout period in the case of an inactive peer.
func (r *rpcServer) CloseChannel(in *lnrpc.CloseChannelRequest,
	updateStream lnrpc.Lightning_CloseChannelServer) error {

	if !r.server.Started() {
		return ErrServerNotActive
	}

	// If the user didn't specify a channel point, then we'll reject this
	// request all together.
	if in.GetChannelPoint() == nil {
		return fmt.Errorf("must specify channel point in close channel")
	}

	// If force closing a channel, the fee set in the commitment transaction
	// is used.
	if in.Force && (in.SatPerByte != 0 || in.SatPerVbyte != 0 || // nolint:staticcheck
		in.TargetConf != 0) {

		return fmt.Errorf("force closing a channel uses a pre-defined fee")
	}

	force := in.Force
	index := in.ChannelPoint.OutputIndex
	txid, err := lnrpc.GetChanPointFundingTxid(in.GetChannelPoint())
	if err != nil {
		rpcsLog.Errorf("[closechannel] unable to get funding txid: %v", err)
		return err
	}
	chanPoint := wire.NewOutPoint(txid, index)

	rpcsLog.Tracef("[closechannel] request for ChannelPoint(%v), force=%v",
		chanPoint, force)

	var (
		updateChan chan interface{}
		errChan    chan error
	)

	// TODO(roasbeef): if force and peer online then don't force?

	// First, we'll fetch the channel as is, as we'll need to examine it
	// regardless of if this is a force close or not.
	channel, err := r.server.chanStateDB.FetchChannel(*chanPoint)
	if err != nil {
		return err
	}

	// We can't coop or force close restored channels or channels that have
	// experienced local data loss. Normally we would detect this in the
	// channel arbitrator if the channel has the status
	// ChanStatusLocalDataLoss after connecting to its peer. But if no
	// connection can be established, the channel arbitrator doesn't know it
	// can't be force closed yet.
	if channel.HasChanStatus(channeldb.ChanStatusRestored) ||
		channel.HasChanStatus(channeldb.ChanStatusLocalDataLoss) {

		return fmt.Errorf("cannot close channel with state: %v",
			channel.ChanStatus())
	}

	// Retrieve the best height of the chain, which we'll use to complete
	// either closing flow.
	_, bestHeight, err := r.server.cc.ChainIO.GetBestBlock()
	if err != nil {
		return err
	}

	// Retrieve the number of active HTLCs on the channel.
	activeHtlcs := channel.ActiveHtlcs()

	// If a force closure was requested, then we'll handle all the details
	// around the creation and broadcast of the unilateral closure
	// transaction here rather than going to the switch as we don't require
	// interaction from the peer.
	if force {
		// As we're force closing this channel, as a precaution, we'll
		// ensure that the switch doesn't continue to see this channel
		// as eligible for forwarding HTLC's. If the peer is online,
		// then we'll also purge all of its indexes.
		remotePub := channel.IdentityPub
		if peer, err := r.server.FindPeer(remotePub); err == nil {
			// TODO(roasbeef): actually get the active channel
			// instead too?
			//  * so only need to grab from database
			peer.WipeChannel(&channel.FundingOutpoint)
		} else {
			chanID := lnwire.NewChanIDFromOutPoint(
				channel.FundingOutpoint,
			)
			r.server.htlcSwitch.RemoveLink(chanID)
		}

		// With the necessary indexes cleaned up, we'll now force close
		// the channel.
		chainArbitrator := r.server.chainArb
		closingTx, err := chainArbitrator.ForceCloseContract(
			*chanPoint,
		)
		if err != nil {
			rpcsLog.Errorf("unable to force close transaction: %v", err)
			return err
		}

		// Safety check which should never happen.
		//
		// TODO(ziggie): remove pointer as return value from
		// ForceCloseContract.
		if closingTx == nil {
			return fmt.Errorf("force close transaction is nil")
		}

		closingTxid := closingTx.TxHash()

		// With the transaction broadcast, we send our first update to
		// the client.
		updateChan = make(chan interface{}, 2)
		updateChan <- &peer.PendingUpdate{
			Txid: closingTxid[:],
		}

		errChan = make(chan error, 1)
		notifier := r.server.cc.ChainNotifier
		go peer.WaitForChanToClose(
			uint32(bestHeight), notifier, errChan, chanPoint,
			&closingTxid, closingTx.TxOut[0].PkScript, func() {
				// Respond to the local subsystem which
				// requested the channel closure.
				updateChan <- &peer.ChannelCloseUpdate{
					ClosingTxid: closingTxid[:],
					Success:     true,
					// Force closure transactions don't have
					// additional local/remote outputs.
				}
			},
		)
	} else {
		// If this is a frozen channel, then we only allow the co-op
		// close to proceed if we were the responder to this channel if
		// the absolute thaw height has not been met.
		if channel.IsInitiator {
			absoluteThawHeight, err := channel.AbsoluteThawHeight()
			if err != nil {
				return err
			}
			if uint32(bestHeight) < absoluteThawHeight {
				return fmt.Errorf("cannot co-op close frozen "+
					"channel as initiator until height=%v, "+
					"(current_height=%v)",
					absoluteThawHeight, bestHeight)
			}
		}

		var (
			chanInSwitch     = true
			chanHasRbfCloser = r.server.ChanHasRbfCoopCloser(
				channel.IdentityPub, *chanPoint,
			)
		)

		// If the link is not known by the switch, we cannot gracefully close
		// the channel.
		channelID := lnwire.NewChanIDFromOutPoint(*chanPoint)

		if _, err := r.server.htlcSwitch.GetLink(channelID); err != nil {
			chanInSwitch = false

			// The channel isn't in the switch, but if there's an
			// active chan closer for the channel, and it's of the
			// RBF variant, then we can actually bypass the switch.
			// Otherwise, we'll return an error.
			if !chanHasRbfCloser {
				rpcsLog.Debugf("Trying to non-force close "+
					"offline channel with chan_point=%v",
					chanPoint)

				return fmt.Errorf("unable to gracefully close "+
					"channel while peer is offline (try "+
					"force closing it instead): %v", err)
			}
		}

		// Keep the old behavior prior to 0.18.0 - when the user
		// doesn't set fee rate or conf target, the default conf target
		// of 6 is used.
		targetConf := maybeUseDefaultConf(
			in.SatPerByte, in.SatPerVbyte, uint32(in.TargetConf),
		)

		// Based on the passed fee related parameters, we'll determine
		// an appropriate fee rate for the cooperative closure
		// transaction.
		feeRate, err := lnrpc.CalculateFeeRate(
			uint64(in.SatPerByte), in.SatPerVbyte, // nolint:staticcheck
			targetConf, r.server.cc.FeeEstimator,
		)
		if err != nil {
			return err
		}

		rpcsLog.Debugf("Target sat/kw for closing transaction: %v",
			int64(feeRate))

		// If the user hasn't specified NoWait, then before we attempt
		// to close the channel we ensure there are no active HTLCs on
		// the link.
		if !in.NoWait && len(activeHtlcs) != 0 {
			return fmt.Errorf("cannot coop close channel with "+
				"active htlcs (number of active htlcs: %d), "+
				"bypass this check and initiate the coop "+
				"close by setting no_wait=true",
				len(activeHtlcs))
		}

		// Otherwise, the caller has requested a regular interactive
		// cooperative channel closure. So we'll forward the request to
		// the htlc switch which will handle the negotiation and
		// broadcast details.

		var deliveryScript lnwire.DeliveryAddress

		// If a delivery address to close out to was specified, decode it.
		if len(in.DeliveryAddress) > 0 {
			// Decode the address provided.
			addr, err := btcutil.DecodeAddress(
				in.DeliveryAddress, r.cfg.ActiveNetParams.Params,
			)
			if err != nil {
				return fmt.Errorf("invalid delivery address: "+
					"%v", err)
			}

			if !addr.IsForNet(r.cfg.ActiveNetParams.Params) {
				return fmt.Errorf("delivery address is not "+
					"for %s",
					r.cfg.ActiveNetParams.Params.Name)
			}

			// Create a script to pay out to the address provided.
			deliveryScript, err = txscript.PayToAddrScript(addr)
			if err != nil {
				return err
			}
		}

		maxFee := chainfee.SatPerKVByte(
			in.MaxFeePerVbyte * 1000,
		).FeePerKWeight()

		// In case the max fee was specified, we check if it's less than
		// the initial fee rate and abort if it is.
		if maxFee != 0 && maxFee < feeRate {
			return fmt.Errorf("max_fee_per_vbyte (%v) is less "+
				"than the required fee rate (%v)", maxFee,
				feeRate)
		}

		if chanHasRbfCloser && !chanInSwitch {
			rpcsLog.Infof("Bypassing Switch to do fee bump "+
				"for ChannelPoint(%v)", chanPoint)

			closeUpdates, err := r.server.AttemptRBFCloseUpdate(
				updateStream.Context(), *chanPoint, feeRate,
				deliveryScript,
			)
			if err != nil {
				return fmt.Errorf("unable to do RBF close "+
					"update: %w", err)
			}

			updateChan = closeUpdates.UpdateChan
			errChan = closeUpdates.ErrChan
		} else {
			maxFee := chainfee.SatPerKVByte(
				in.MaxFeePerVbyte * 1000,
			).FeePerKWeight()
			updateChan, errChan = r.server.htlcSwitch.CloseLink(
				updateStream.Context(), chanPoint,
				contractcourt.CloseRegular, feeRate, maxFee,
				deliveryScript,
			)
		}
	}

	// If the user doesn't want to wait for the txid to come back then we
	// will send an empty update to kick off the stream. This is also used
	// when active htlcs are still on the channel to give the client
	// immediate feedback.
	if in.NoWait {
		rpcsLog.Trace("[closechannel] sending instant update")
		if err := updateStream.Send(
			//nolint:ll
			&lnrpc.CloseStatusUpdate{
				Update: &lnrpc.CloseStatusUpdate_CloseInstant{
					CloseInstant: &lnrpc.InstantUpdate{
						NumPendingHtlcs: int32(len(activeHtlcs)),
					},
				},
			},
		); err != nil {
			return err
		}
	}

out:
	for {
		select {
		case err := <-errChan:
			rpcsLog.Errorf("[closechannel] unable to close "+
				"ChannelPoint(%v): %v", chanPoint, err)

			return err

		case closingUpdate := <-updateChan:
			rpcClosingUpdate, err := createRPCCloseUpdate(
				closingUpdate,
			)
			if err != nil {
				return err
			}

			err = fn.MapOptionZ(
				r.server.implCfg.AuxDataParser,
				func(parser AuxDataParser) error {
					return parser.InlineParseCustomData(
						rpcClosingUpdate,
					)
				},
			)
			if err != nil {
				return fmt.Errorf("error parsing custom data: "+
					"%w", err)
			}

			rpcsLog.Tracef("[closechannel] sending update: %v",
				rpcClosingUpdate)

			if err := updateStream.Send(rpcClosingUpdate); err != nil {
				return err
			}

			// If a final channel closing updates is being sent,
			// then we can break out of our dispatch loop as we no
			// longer need to process any further updates.
			switch closeUpdate := closingUpdate.(type) {
			case *peer.ChannelCloseUpdate:
				h, _ := chainhash.NewHash(closeUpdate.ClosingTxid)
				rpcsLog.Infof("[closechannel] close completed: "+
					"txid(%v)", h)

				break out
			}

		case <-r.quit:
			return nil
		}
	}

	return nil
}

func createRPCCloseUpdate(
	update interface{}) (*lnrpc.CloseStatusUpdate, error) {

	switch u := update.(type) {
	case *peer.ChannelCloseUpdate:
		ccu := &lnrpc.ChannelCloseUpdate{
			ClosingTxid: u.ClosingTxid,
			Success:     u.Success,
		}

		err := fn.MapOptionZ(
			u.LocalCloseOutput,
			func(closeOut chancloser.CloseOutput) error {
				cr, err := closeOut.ShutdownRecords.Serialize()
				if err != nil {
					return fmt.Errorf("error serializing "+
						"local close out custom "+
						"records: %w", err)
				}

				rpcCloseOut := &lnrpc.CloseOutput{
					AmountSat:         int64(closeOut.Amt),
					PkScript:          closeOut.PkScript,
					IsLocal:           true,
					CustomChannelData: cr,
				}
				ccu.LocalCloseOutput = rpcCloseOut

				return nil
			},
		)
		if err != nil {
			return nil, err
		}

		err = fn.MapOptionZ(
			u.RemoteCloseOutput,
			func(closeOut chancloser.CloseOutput) error {
				cr, err := closeOut.ShutdownRecords.Serialize()
				if err != nil {
					return fmt.Errorf("error serializing "+
						"remote close out custom "+
						"records: %w", err)
				}

				rpcCloseOut := &lnrpc.CloseOutput{
					AmountSat:         int64(closeOut.Amt),
					PkScript:          closeOut.PkScript,
					CustomChannelData: cr,
				}
				ccu.RemoteCloseOutput = rpcCloseOut

				return nil
			},
		)
		if err != nil {
			return nil, err
		}

		u.AuxOutputs.WhenSome(func(outs chancloser.AuxCloseOutputs) {
			for _, out := range outs.ExtraCloseOutputs {
				ccu.AdditionalOutputs = append(
					ccu.AdditionalOutputs,
					&lnrpc.CloseOutput{
						AmountSat: out.Value,
						PkScript:  out.PkScript,
						IsLocal:   out.IsLocal,
					},
				)
			}
		})

		return &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ChanClose{
				ChanClose: ccu,
			},
		}, nil

	case *peer.PendingUpdate:
		upd := &lnrpc.PendingUpdate{
			Txid:        u.Txid,
			OutputIndex: u.OutputIndex,
		}

		// Potentially set the optional fields that are only set for
		// the new RBF close flow.
		u.IsLocalCloseTx.WhenSome(func(isLocal bool) {
			upd.LocalCloseTx = isLocal
		})
		u.FeePerVbyte.WhenSome(func(feeRate chainfee.SatPerVByte) {
			upd.FeePerVbyte = int64(feeRate)
		})

		return &lnrpc.CloseStatusUpdate{
			Update: &lnrpc.CloseStatusUpdate_ClosePending{
				ClosePending: upd,
			},
		}, nil
	}

	return nil, errors.New("unknown close status update")
}

// abandonChanFromGraph attempts to remove a channel from the channel graph. If
// we can't find the chanID in the graph, then we assume it has already been
// removed, and will return a nop.
func abandonChanFromGraph(chanGraph *graphdb.ChannelGraph,
	chanPoint *wire.OutPoint) error {

	// First, we'll obtain the channel ID. If we can't locate this, then
	// it's the case that the channel may have already been removed from
	// the graph, so we'll return a nil error.
	chanID, err := chanGraph.ChannelID(chanPoint)
	switch {
	case errors.Is(err, graphdb.ErrEdgeNotFound):
		return nil
	case err != nil:
		return err
	}

	// If the channel ID is still in the graph, then that means the channel
	// is still open, so we'll now move to purge it from the graph.
	return chanGraph.DeleteChannelEdges(false, true, chanID)
}

// abandonChan removes a channel from the database, graph and contract court.
func (r *rpcServer) abandonChan(chanPoint *wire.OutPoint,
	bestHeight uint32) error {

	// Before we remove the channel we cancel the rebroadcasting of the
	// transaction. If this transaction does not exist in the rebroadcast
	// queue anymore it is a noop.
	txid, err := chainhash.NewHash(chanPoint.Hash[:])
	if err != nil {
		return err
	}
	r.server.cc.Wallet.CancelRebroadcast(*txid)

	// Abandoning a channel is a three-step process: remove from the open
	// channel state, remove from the graph, remove from the contract
	// court. Between any step it's possible that the users restarts the
	// process all over again. As a result, each of the steps below are
	// intended to be idempotent.
	err = r.server.chanStateDB.AbandonChannel(chanPoint, bestHeight)
	if err != nil {
		return err
	}
	err = abandonChanFromGraph(r.server.graphDB, chanPoint)
	if err != nil {
		return err
	}
	err = r.server.chainArb.ResolveContract(*chanPoint)
	if err != nil {
		return err
	}

	// If this channel was in the process of being closed, but didn't fully
	// close, then it's possible that the nursery is hanging on to some
	// state. To err on the side of caution, we'll now attempt to wipe any
	// state for this channel from the nursery.
	err = r.server.utxoNursery.RemoveChannel(chanPoint)
	if err != nil && err != contractcourt.ErrContractNotFound {
		return err
	}

	// Finally, notify the backup listeners that the channel can be removed
	// from any channel backups.
	r.server.channelNotifier.NotifyClosedChannelEvent(*chanPoint)

	return nil
}

// AbandonChannel removes all channel state from the database except for a
// close summary. This method can be used to get rid of permanently unusable
// channels due to bugs fixed in newer versions of lnd.
func (r *rpcServer) AbandonChannel(_ context.Context,
	in *lnrpc.AbandonChannelRequest) (*lnrpc.AbandonChannelResponse, error) {

	// If this isn't the dev build, then we won't allow the RPC to be
	// executed, as it's an advanced feature and won't be activated in
	// regular production/release builds except for the explicit case of
	// externally funded channels that are still pending. Due to repeated
	// requests, we also allow this requirement to be overwritten by a new
	// flag that attests to the user knowing what they're doing and the risk
	// associated with the command/RPC.
	if !in.IKnowWhatIAmDoing && !in.PendingFundingShimOnly &&
		!build.IsDevBuild() {

		return nil, fmt.Errorf("AbandonChannel RPC call only " +
			"available in dev builds")
	}

	// We'll parse out the arguments to we can obtain the chanPoint of the
	// target channel.
	txid, err := lnrpc.GetChanPointFundingTxid(in.GetChannelPoint())
	if err != nil {
		return nil, err
	}
	index := in.ChannelPoint.OutputIndex
	chanPoint := wire.NewOutPoint(txid, index)

	// When we remove the channel from the database, we need to set a close
	// height, so we'll just use the current best known height.
	_, bestHeight, err := r.server.cc.ChainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	dbChan, err := r.server.chanStateDB.FetchChannel(*chanPoint)
	switch {
	// If the channel isn't found in the set of open channels, then we can
	// continue on as it can't be loaded into the link/peer.
	case err == channeldb.ErrChannelNotFound:
		break

	// If the channel is still known to be open, then before we modify any
	// on-disk state, we'll remove the channel from the switch and peer
	// state if it's been loaded in.
	case err == nil:
		// If the user requested the more safe version that only allows
		// the removal of externally (shim) funded channels that are
		// still pending, we enforce this option now that we know the
		// state of the channel.
		//
		// TODO(guggero): Properly store the funding type (wallet, shim,
		// PSBT) on the channel so we don't need to use the thaw height.
		isShimFunded := dbChan.ThawHeight > 0
		isPendingShimFunded := isShimFunded && dbChan.IsPending
		if !in.IKnowWhatIAmDoing && in.PendingFundingShimOnly &&
			!isPendingShimFunded {

			return nil, fmt.Errorf("channel %v is not externally "+
				"funded or not pending", chanPoint)
		}

		// We'll mark the channel as borked before we remove the state
		// from the switch/peer so it won't be loaded back in if the
		// peer reconnects.
		if err := dbChan.MarkBorked(); err != nil {
			return nil, err
		}
		remotePub := dbChan.IdentityPub
		if peer, err := r.server.FindPeer(remotePub); err == nil {
			peer.WipeChannel(chanPoint)
		}

	default:
		return nil, err
	}

	// Remove the channel from the graph, database and contract court.
	if err := r.abandonChan(chanPoint, uint32(bestHeight)); err != nil {
		return nil, err
	}

	return &lnrpc.AbandonChannelResponse{
		Status: fmt.Sprintf("channel %v abandoned", chanPoint.String()),
	}, nil
}

// GetInfo returns general information concerning the lightning node including
// its identity pubkey, alias, the chains it is connected to, and information
// concerning the number of open+pending channels.
func (r *rpcServer) GetInfo(_ context.Context,
	_ *lnrpc.GetInfoRequest) (*lnrpc.GetInfoResponse, error) {

	serverPeers := r.server.Peers()

	openChannels, err := r.server.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	var activeChannels uint32
	for _, channel := range openChannels {
		chanID := lnwire.NewChanIDFromOutPoint(channel.FundingOutpoint)
		if r.server.htlcSwitch.HasActiveLink(chanID) {
			activeChannels++
		}
	}

	inactiveChannels := uint32(len(openChannels)) - activeChannels

	pendingChannels, err := r.server.chanStateDB.FetchPendingChannels()
	if err != nil {
		return nil, fmt.Errorf("unable to get retrieve pending "+
			"channels: %v", err)
	}
	nPendingChannels := uint32(len(pendingChannels))

	idPub := r.server.identityECDH.PubKey().SerializeCompressed()
	encodedIDPub := hex.EncodeToString(idPub)

	// Get the system's chain sync info.
	syncInfo, err := r.getChainSyncInfo()
	if err != nil {
		return nil, err
	}

	network := lncfg.NormalizeNetwork(r.cfg.ActiveNetParams.Name)
	activeChains := []*lnrpc.Chain{
		{
			Chain:   BitcoinChainName,
			Network: network,
		},
	}

	// Check if external IP addresses were provided to lnd and use them
	// to set the URIs.
	nodeAnn := r.server.getNodeAnnouncement()

	addrs := nodeAnn.Addresses
	uris := make([]string, len(addrs))
	for i, addr := range addrs {
		uris[i] = fmt.Sprintf("%s@%s", encodedIDPub, addr.String())
	}

	isGraphSynced := r.server.authGossiper.SyncManager().IsGraphSynced()

	features := make(map[uint32]*lnrpc.Feature)
	sets := r.server.featureMgr.ListSets()

	for _, set := range sets {
		// Get the a list of lnrpc features for each set we support.
		featureVector := r.server.featureMgr.Get(set)
		rpcFeatures := invoicesrpc.CreateRPCFeatures(featureVector)

		// Add the features to our map of features, allowing over writing of
		// existing values because features in different sets with the same bit
		// are duplicated across sets.
		maps.Copy(features, rpcFeatures)
	}

	// TODO(roasbeef): add synced height n stuff

	isTestNet := chainreg.IsTestnet(&r.cfg.ActiveNetParams)
	nodeColor := graphdb.EncodeHexColor(nodeAnn.RGBColor)
	version := build.Version() + " commit=" + build.Commit

	return &lnrpc.GetInfoResponse{
		IdentityPubkey:            encodedIDPub,
		NumPendingChannels:        nPendingChannels,
		NumActiveChannels:         activeChannels,
		NumInactiveChannels:       inactiveChannels,
		NumPeers:                  uint32(len(serverPeers)),
		BlockHeight:               uint32(syncInfo.bestHeight),
		BlockHash:                 syncInfo.blockHash.String(),
		SyncedToChain:             syncInfo.isSynced,
		Testnet:                   isTestNet,
		Chains:                    activeChains,
		Uris:                      uris,
		Alias:                     nodeAnn.Alias.String(),
		Color:                     nodeColor,
		BestHeaderTimestamp:       syncInfo.timestamp,
		Version:                   version,
		CommitHash:                build.CommitHash,
		SyncedToGraph:             isGraphSynced,
		Features:                  features,
		RequireHtlcInterceptor:    r.cfg.RequireInterceptor,
		StoreFinalHtlcResolutions: r.cfg.StoreFinalHtlcResolutions,
	}, nil
}

// GetDebugInfo returns debug information concerning the state of the daemon
// and its subsystems. This includes the full configuration and the latest log
// entries from the log file.
func (r *rpcServer) GetDebugInfo(_ context.Context,
	_ *lnrpc.GetDebugInfoRequest) (*lnrpc.GetDebugInfoResponse, error) {

	flatConfig, _, err := configToFlatMap(*r.cfg)
	if err != nil {
		return nil, fmt.Errorf("error converting config to flat map: "+
			"%w", err)
	}

	logFileName := filepath.Join(r.cfg.LogDir, defaultLogFilename)
	logContent, err := os.ReadFile(logFileName)
	if err != nil {
		return nil, fmt.Errorf("error reading log file '%s': %w",
			logFileName, err)
	}

	return &lnrpc.GetDebugInfoResponse{
		Config: flatConfig,
		Log:    strings.Split(string(logContent), "\n"),
	}, nil
}

// GetRecoveryInfo returns a boolean indicating whether the wallet is started
// in recovery mode, whether the recovery is finished, and the progress made
// so far.
func (r *rpcServer) GetRecoveryInfo(ctx context.Context,
	in *lnrpc.GetRecoveryInfoRequest) (*lnrpc.GetRecoveryInfoResponse, error) {

	isRecoveryMode, progress, err := r.server.cc.Wallet.GetRecoveryInfo()
	if err != nil {
		return nil, fmt.Errorf("unable to get wallet recovery info: %w",
			err)
	}

	rpcsLog.Debugf("[getrecoveryinfo] is recovery mode=%v, progress=%v",
		isRecoveryMode, progress)

	return &lnrpc.GetRecoveryInfoResponse{
		RecoveryMode:     isRecoveryMode,
		RecoveryFinished: progress == 1,
		Progress:         progress,
	}, nil
}

// ListPeers returns a verbose listing of all currently active peers.
func (r *rpcServer) ListPeers(ctx context.Context,
	in *lnrpc.ListPeersRequest) (*lnrpc.ListPeersResponse, error) {

	serverPeers := r.server.Peers()
	resp := &lnrpc.ListPeersResponse{
		Peers: make([]*lnrpc.Peer, 0, len(serverPeers)),
	}

	for _, serverPeer := range serverPeers {
		var (
			satSent int64
			satRecv int64
		)

		// In order to display the total number of satoshis of outbound
		// (sent) and inbound (recv'd) satoshis that have been
		// transported through this peer, we'll sum up the sent/recv'd
		// values for each of the active channels we have with the
		// peer.
		chans := serverPeer.ChannelSnapshots()
		for _, c := range chans {
			satSent += int64(c.TotalMSatSent.ToSatoshis())
			satRecv += int64(c.TotalMSatReceived.ToSatoshis())
		}

		nodePub := serverPeer.PubKey()

		// Retrieve the peer's sync type. If we don't currently have a
		// syncer for the peer, then we'll default to a passive sync.
		// This can happen if the RPC is called while a peer is
		// initializing.
		syncer, ok := r.server.authGossiper.SyncManager().GossipSyncer(
			nodePub,
		)

		var lnrpcSyncType lnrpc.Peer_SyncType
		if !ok {
			rpcsLog.Warnf("Gossip syncer for peer=%x not found",
				nodePub)
			lnrpcSyncType = lnrpc.Peer_UNKNOWN_SYNC
		} else {
			syncType := syncer.SyncType()
			switch syncType {
			case discovery.ActiveSync:
				lnrpcSyncType = lnrpc.Peer_ACTIVE_SYNC
			case discovery.PassiveSync:
				lnrpcSyncType = lnrpc.Peer_PASSIVE_SYNC
			case discovery.PinnedSync:
				lnrpcSyncType = lnrpc.Peer_PINNED_SYNC
			default:
				return nil, fmt.Errorf("unhandled sync type %v",
					syncType)
			}
		}

		features := invoicesrpc.CreateRPCFeatures(
			serverPeer.RemoteFeatures(),
		)

		rpcPeer := &lnrpc.Peer{
			PubKey:          hex.EncodeToString(nodePub[:]),
			Address:         serverPeer.Conn().RemoteAddr().String(),
			Inbound:         serverPeer.Inbound(),
			BytesRecv:       serverPeer.BytesReceived(),
			BytesSent:       serverPeer.BytesSent(),
			SatSent:         satSent,
			SatRecv:         satRecv,
			PingTime:        serverPeer.PingTime(),
			SyncType:        lnrpcSyncType,
			Features:        features,
			LastPingPayload: serverPeer.LastRemotePingPayload(),
		}

		var peerErrors []interface{}

		// If we only want the most recent error, get the most recent
		// error from the buffer and add it to our list of errors if
		// it is non-nil. If we want all the stored errors, simply
		// add the full list to our set of errors.
		if in.LatestError {
			latestErr := serverPeer.ErrorBuffer().Latest()
			if latestErr != nil {
				peerErrors = []interface{}{latestErr}
			}
		} else {
			peerErrors = serverPeer.ErrorBuffer().List()
		}

		// Add the relevant peer errors to our response.
		for _, error := range peerErrors {
			tsError := error.(*peer.TimestampedError)

			rpcErr := &lnrpc.TimestampedError{
				Timestamp: uint64(tsError.Timestamp.Unix()),
				Error:     tsError.Error.Error(),
			}

			rpcPeer.Errors = append(rpcPeer.Errors, rpcErr)
		}

		// If the server has started, we can query the event store
		// for our peer's flap count. If we do so when the server has
		// not started, the request will block.
		if r.server.Started() {
			vertex, err := route.NewVertexFromBytes(nodePub[:])
			if err != nil {
				return nil, err
			}

			flap, ts, err := r.server.chanEventStore.FlapCount(
				vertex,
			)

			// Log the error if we cannot get the flap count instead
			// of failing this RPC call.
			if err != nil {
				rpcsLog.Debugf("Failed to get flap count for "+
					"peer %v", vertex)
			}

			// If our timestamp is non-nil, we have values for our
			// peer's flap count, so we set them.
			if ts != nil {
				rpcPeer.FlapCount = int32(flap)
				rpcPeer.LastFlapNs = ts.UnixNano()
			}
		}

		resp.Peers = append(resp.Peers, rpcPeer)
	}

	rpcsLog.Debugf("[listpeers] yielded %v peers", serverPeers)

	return resp, nil
}

// SubscribePeerEvents returns a uni-directional stream (server -> client)
// for notifying the client of peer online and offline events.
func (r *rpcServer) SubscribePeerEvents(req *lnrpc.PeerEventSubscription,
	eventStream lnrpc.Lightning_SubscribePeerEventsServer) error {

	peerEventSub, err := r.server.peerNotifier.SubscribePeerEvents()
	if err != nil {
		return err
	}
	defer peerEventSub.Cancel()

	for {
		select {
		// A new update has been sent by the peer notifier, we'll
		// marshal it into the form expected by the gRPC client, then
		// send it off to the client.
		case e := <-peerEventSub.Updates():
			var event *lnrpc.PeerEvent

			switch peerEvent := e.(type) {
			case peernotifier.PeerOfflineEvent:
				event = &lnrpc.PeerEvent{
					PubKey: hex.EncodeToString(peerEvent.PubKey[:]),
					Type:   lnrpc.PeerEvent_PEER_OFFLINE,
				}

			case peernotifier.PeerOnlineEvent:
				event = &lnrpc.PeerEvent{
					PubKey: hex.EncodeToString(peerEvent.PubKey[:]),
					Type:   lnrpc.PeerEvent_PEER_ONLINE,
				}

			default:
				return fmt.Errorf("unexpected peer event: %v", event)
			}

			if err := eventStream.Send(event); err != nil {
				return err
			}

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-eventStream.Context().Done():
			if errors.Is(eventStream.Context().Err(), context.Canceled) {
				return nil
			}
			return eventStream.Context().Err()

		case <-r.quit:
			return nil
		}
	}
}

// WalletBalance returns total unspent outputs(confirmed and unconfirmed), all
// confirmed unspent outputs and all unconfirmed unspent outputs under control
// by the wallet. This method can be modified by having the request specify
// only witness outputs should be factored into the final output sum.
// TODO(roasbeef): add async hooks into wallet balance changes.
func (r *rpcServer) WalletBalance(ctx context.Context,
	in *lnrpc.WalletBalanceRequest) (*lnrpc.WalletBalanceResponse, error) {

	// Retrieve all existing wallet accounts. We'll compute the confirmed
	// and unconfirmed balance for each and tally them up.
	accounts, err := r.server.cc.Wallet.ListAccounts(in.Account, nil)
	if err != nil {
		return nil, err
	}

	var totalBalance, confirmedBalance, unconfirmedBalance btcutil.Amount
	rpcAccountBalances := make(
		map[string]*lnrpc.WalletAccountBalance, len(accounts),
	)
	for _, account := range accounts {
		// There are two default accounts, one for NP2WKH outputs and
		// another for P2WKH outputs. The balance will be computed for
		// both given one call to ConfirmedBalance with the default
		// wallet and imported account, so we'll skip the second
		// instance to avoid inflating the balance.
		switch account.AccountName {
		case waddrmgr.ImportedAddrAccountName:
			// Omit the imported account from the response unless we
			// actually have any keys imported.
			if account.ImportedKeyCount == 0 {
				continue
			}

			fallthrough

		case lnwallet.DefaultAccountName:
			if _, ok := rpcAccountBalances[account.AccountName]; ok {
				continue
			}

		default:
		}

		// There now also are the accounts for the internal channel
		// related keys. We skip those as they'll never have any direct
		// balance.
		if account.KeyScope.Purpose == keychain.BIP0043Purpose {
			continue
		}

		// Get total balance, from txs that have >= 0 confirmations.
		totalBal, err := r.server.cc.Wallet.ConfirmedBalance(
			0, account.AccountName,
		)
		if err != nil {
			return nil, err
		}
		totalBalance += totalBal

		// Get confirmed balance, from txs that have >= 1 confirmations.
		// TODO(halseth): get both unconfirmed and confirmed balance in
		// one call, as this is racy.
		if in.MinConfs <= 0 {
			in.MinConfs = 1
		}
		confirmedBal, err := r.server.cc.Wallet.ConfirmedBalance(
			in.MinConfs, account.AccountName,
		)
		if err != nil {
			return nil, err
		}
		confirmedBalance += confirmedBal

		// Get unconfirmed balance, from txs with 0 confirmations.
		unconfirmedBal := totalBal - confirmedBal
		unconfirmedBalance += unconfirmedBal

		rpcAccountBalances[account.AccountName] = &lnrpc.WalletAccountBalance{
			ConfirmedBalance:   int64(confirmedBal),
			UnconfirmedBalance: int64(unconfirmedBal),
		}
	}

	// Now that we have the base balance accounted for with each account,
	// we'll look at the set of locked UTXOs to tally that as well. If we
	// don't display this, then anytime we attempt a funding reservation,
	// the outputs will chose as being "gone" until they're confirmed on
	// chain.
	var lockedBalance btcutil.Amount
	leases, err := r.server.cc.Wallet.ListLeasedOutputs()
	if err != nil {
		return nil, err
	}
	for _, leasedOutput := range leases {
		lockedBalance += btcutil.Amount(leasedOutput.Value)
	}

	// Get the current number of non-private anchor channels.
	currentNumAnchorChans, err := r.server.cc.Wallet.CurrentNumAnchorChans()
	if err != nil {
		return nil, err
	}

	// Get the required reserve for the wallet.
	requiredReserve := r.server.cc.Wallet.RequiredReserve(
		uint32(currentNumAnchorChans),
	)

	rpcsLog.Debugf("[walletbalance] Total balance=%v (confirmed=%v, "+
		"unconfirmed=%v, locked=%v)", totalBalance, confirmedBalance,
		unconfirmedBalance, lockedBalance)

	return &lnrpc.WalletBalanceResponse{
		TotalBalance:              int64(totalBalance),
		ConfirmedBalance:          int64(confirmedBalance),
		UnconfirmedBalance:        int64(unconfirmedBalance),
		LockedBalance:             int64(lockedBalance),
		ReservedBalanceAnchorChan: int64(requiredReserve),
		AccountBalance:            rpcAccountBalances,
	}, nil
}

// ChannelBalance returns the total available channel flow across all open
// channels in satoshis.
func (r *rpcServer) ChannelBalance(ctx context.Context,
	in *lnrpc.ChannelBalanceRequest) (
	*lnrpc.ChannelBalanceResponse, error) {

	var (
		localBalance             lnwire.MilliSatoshi
		remoteBalance            lnwire.MilliSatoshi
		unsettledLocalBalance    lnwire.MilliSatoshi
		unsettledRemoteBalance   lnwire.MilliSatoshi
		pendingOpenLocalBalance  lnwire.MilliSatoshi
		pendingOpenRemoteBalance lnwire.MilliSatoshi
		customDataBuf            bytes.Buffer
	)

	openChannels, err := r.server.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	// Encode the number of open channels to the custom data buffer.
	err = wire.WriteVarInt(&customDataBuf, 0, uint64(len(openChannels)))
	if err != nil {
		return nil, err
	}

	for _, channel := range openChannels {
		c := channel.LocalCommitment
		localBalance += c.LocalBalance
		remoteBalance += c.RemoteBalance

		// Add pending htlc amount.
		for _, htlc := range c.Htlcs {
			if htlc.Incoming {
				unsettledLocalBalance += htlc.Amt
			} else {
				unsettledRemoteBalance += htlc.Amt
			}
		}

		// Encode the custom data for this open channel.
		openChanData := channel.LocalCommitment.CustomBlob.UnwrapOr(nil)
		err = wire.WriteVarBytes(&customDataBuf, 0, openChanData)
		if err != nil {
			return nil, err
		}
	}

	pendingChannels, err := r.server.chanStateDB.FetchPendingChannels()
	if err != nil {
		return nil, err
	}

	// Encode the number of pending channels to the custom data buffer.
	err = wire.WriteVarInt(&customDataBuf, 0, uint64(len(pendingChannels)))
	if err != nil {
		return nil, err
	}

	for _, channel := range pendingChannels {
		c := channel.LocalCommitment
		pendingOpenLocalBalance += c.LocalBalance
		pendingOpenRemoteBalance += c.RemoteBalance

		// Encode the custom data for this pending channel.
		openChanData := channel.LocalCommitment.CustomBlob.UnwrapOr(nil)
		err = wire.WriteVarBytes(&customDataBuf, 0, openChanData)
		if err != nil {
			return nil, err
		}
	}

	rpcsLog.Debugf("[channelbalance] local_balance=%v remote_balance=%v "+
		"unsettled_local_balance=%v unsettled_remote_balance=%v "+
		"pending_open_local_balance=%v pending_open_remote_balance=%v",
		localBalance, remoteBalance, unsettledLocalBalance,
		unsettledRemoteBalance, pendingOpenLocalBalance,
		pendingOpenRemoteBalance)

	resp := &lnrpc.ChannelBalanceResponse{
		LocalBalance: &lnrpc.Amount{
			Sat:  uint64(localBalance.ToSatoshis()),
			Msat: uint64(localBalance),
		},
		RemoteBalance: &lnrpc.Amount{
			Sat:  uint64(remoteBalance.ToSatoshis()),
			Msat: uint64(remoteBalance),
		},
		UnsettledLocalBalance: &lnrpc.Amount{
			Sat:  uint64(unsettledLocalBalance.ToSatoshis()),
			Msat: uint64(unsettledLocalBalance),
		},
		UnsettledRemoteBalance: &lnrpc.Amount{
			Sat:  uint64(unsettledRemoteBalance.ToSatoshis()),
			Msat: uint64(unsettledRemoteBalance),
		},
		PendingOpenLocalBalance: &lnrpc.Amount{
			Sat:  uint64(pendingOpenLocalBalance.ToSatoshis()),
			Msat: uint64(pendingOpenLocalBalance),
		},
		PendingOpenRemoteBalance: &lnrpc.Amount{
			Sat:  uint64(pendingOpenRemoteBalance.ToSatoshis()),
			Msat: uint64(pendingOpenRemoteBalance),
		},
		CustomChannelData: customDataBuf.Bytes(),

		// Deprecated fields.
		Balance:            int64(localBalance.ToSatoshis()),
		PendingOpenBalance: int64(pendingOpenLocalBalance.ToSatoshis()),
	}

	err = fn.MapOptionZ(
		r.server.implCfg.AuxDataParser,
		func(parser AuxDataParser) error {
			return parser.InlineParseCustomData(resp)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing custom data: %w", err)
	}

	return resp, nil
}

type (
	pendingOpenChannels  []*lnrpc.PendingChannelsResponse_PendingOpenChannel
	pendingForceClose    []*lnrpc.PendingChannelsResponse_ForceClosedChannel
	waitingCloseChannels []*lnrpc.PendingChannelsResponse_WaitingCloseChannel
)

// calcRemainingConfs calculates how many more confirmations are needed for a
// pending channel to be fully confirmed. It takes into account:
// 1. The current blockchain height
// 2. The block height at which the funding transaction was first confirmed
// 3. The total number of confirmations required for the channel.
func calcRemainingConfs(pendingChan *channeldb.OpenChannel,
	currentHeight uint32) uint32 {

	// If the funding transaction hasn't been confirmed yet,
	// we need all the required confirmations.
	if pendingChan.ConfirmationHeight == 0 {
		return uint32(pendingChan.NumConfsRequired)
	}

	// Calculate the target height at which the channel will be fully
	// confirmed. The -1 is because the confirmation height of the first
	// confirmation has to be taken into account.
	targetConfirmationHeight := pendingChan.ConfirmationHeight +
		uint32(pendingChan.NumConfsRequired) - 1

	// In case the current height is already past the target, return 0. This
	// should never happen because the channel should already be moved from
	// pending to open state but we handle this case in case of timing
	// issues.
	if currentHeight >= targetConfirmationHeight {
		return 0
	}

	return targetConfirmationHeight - currentHeight
}

// fetchPendingOpenChannels queries the database for a list of channels that
// have pending open state. The returned result is used in the response of the
// PendingChannels RPC.
func (r *rpcServer) fetchPendingOpenChannels() (pendingOpenChannels, error) {
	// First, we'll populate the response with all the channels that are
	// soon to be opened. We can easily fetch this data from the database
	// and map the db struct to the proto response.
	channels, err := r.server.chanStateDB.FetchPendingChannels()
	if err != nil {
		rpcsLog.Errorf("unable to fetch pending channels: %v", err)
		return nil, err
	}

	_, currentHeight, err := r.server.cc.ChainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	result := make(pendingOpenChannels, len(channels))
	for i, pendingChan := range channels {
		pub := pendingChan.IdentityPub.SerializeCompressed()

		// As this is required for display purposes, we'll calculate
		// the weight of the commitment transaction. We also add on the
		// estimated weight of the witness to calculate the weight of
		// the transaction if it were to be immediately unilaterally
		// broadcast.
		// TODO(roasbeef): query for funding tx from wallet, display
		// that also?
		var witnessWeight int64
		if pendingChan.ChanType.IsTaproot() {
			witnessWeight = input.TaprootKeyPathWitnessSize
		} else {
			witnessWeight = input.WitnessCommitmentTxWeight
		}

		localCommitment := pendingChan.LocalCommitment
		utx := btcutil.NewTx(localCommitment.CommitTx)
		commitBaseWeight := blockchain.GetTransactionWeight(utx)
		commitWeight := commitBaseWeight + witnessWeight

		// The value of waitBlocksForFundingConf is adjusted in a
		// development environment to enhance test capabilities.
		// Otherwise, it is set to DefaultMaxWaitNumBlocksFundingConf.
		waitBlocksForFundingConf := uint32(
			lncfg.DefaultMaxWaitNumBlocksFundingConf,
		)

		if lncfg.IsDevBuild() {
			waitBlocksForFundingConf =
				r.cfg.Dev.GetMaxWaitNumBlocksFundingConf()
		}

		// FundingExpiryBlocks is the distance from the current block
		// height to the broadcast height + waitBlocksForFundingConf.
		maxFundingHeight := waitBlocksForFundingConf +
			pendingChan.BroadcastHeight()
		fundingExpiryBlocks := int32(maxFundingHeight) - currentHeight

		// Calculate remainingConfs, the number of blocks left until the
		// funding transaction reaches the required confirmation height.
		//
		// ZeroConf channels are marked OPEN immediately upon creation,
		// so they never enter the "pending" state.
		remainingConfs := calcRemainingConfs(
			pendingChan, uint32(currentHeight),
		)

		customChanBytes, err := encodeCustomChanData(pendingChan)
		if err != nil {
			return nil, fmt.Errorf("unable to encode open chan "+
				"data: %w", err)
		}

		result[i] = &lnrpc.PendingChannelsResponse_PendingOpenChannel{
			Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
				RemoteNodePub:        hex.EncodeToString(pub),
				ChannelPoint:         pendingChan.FundingOutpoint.String(),
				Capacity:             int64(pendingChan.Capacity),
				LocalBalance:         int64(localCommitment.LocalBalance.ToSatoshis()),
				RemoteBalance:        int64(localCommitment.RemoteBalance.ToSatoshis()),
				LocalChanReserveSat:  int64(pendingChan.LocalChanCfg.ChanReserve),
				RemoteChanReserveSat: int64(pendingChan.RemoteChanCfg.ChanReserve),
				Initiator:            rpcInitiator(pendingChan.IsInitiator),
				CommitmentType:       rpcCommitmentType(pendingChan.ChanType),
				Private:              isPrivate(pendingChan),
				Memo:                 string(pendingChan.Memo),
				CustomChannelData:    customChanBytes,
			},
			CommitWeight: commitWeight,
			CommitFee:    int64(localCommitment.CommitFee),
			FeePerKw: int64(localCommitment.
				FeePerKw),
			FundingExpiryBlocks:      fundingExpiryBlocks,
			ConfirmationsUntilActive: remainingConfs,
			ConfirmationHeight: pendingChan.
				ConfirmationHeight,
		}
	}

	return result, nil
}

// fetchPendingForceCloseChannels queries the database for a list of channels
// that have their closing transactions confirmed but not fully resolved yet.
// The returned result is used in the response of the PendingChannels RPC.
func (r *rpcServer) fetchPendingForceCloseChannels() (pendingForceClose,
	int64, error) {

	_, currentHeight, err := r.server.cc.ChainIO.GetBestBlock()
	if err != nil {
		return nil, 0, err
	}

	// Next, we'll examine the channels that are soon to be closed so we
	// can populate these fields within the response.
	channels, err := r.server.chanStateDB.FetchClosedChannels(true)
	if err != nil {
		rpcsLog.Errorf("unable to fetch closed channels: %v", err)
		return nil, 0, err
	}

	result := make(pendingForceClose, 0)
	limboBalance := int64(0)

	for _, pendingClose := range channels {
		// First construct the channel struct itself, this will be
		// needed regardless of how this channel was closed.
		pub := pendingClose.RemotePub.SerializeCompressed()
		chanPoint := pendingClose.ChanPoint

		// Create the pending channel. If this channel was closed before
		// we started storing historical channel data, we will not know
		// who initiated the channel, so we set the initiator field to
		// unknown.
		channel := &lnrpc.PendingChannelsResponse_PendingChannel{
			RemoteNodePub:  hex.EncodeToString(pub),
			ChannelPoint:   chanPoint.String(),
			Capacity:       int64(pendingClose.Capacity),
			LocalBalance:   int64(pendingClose.SettledBalance),
			CommitmentType: lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			Initiator:      lnrpc.Initiator_INITIATOR_UNKNOWN,
		}

		// Lookup the channel in the historical channel bucket to obtain
		// initiator information. If the historical channel bucket was
		// not found, or the channel itself, this channel was closed
		// in a version before we started persisting historical
		// channels, so we silence the error.
		historical, err := r.server.chanStateDB.FetchHistoricalChannel(
			&pendingClose.ChanPoint,
		)
		switch err {
		// If the channel was closed in a version that did not record
		// historical channels, ignore the error.
		case channeldb.ErrNoHistoricalBucket:
		case channeldb.ErrChannelNotFound:

		case nil:
			channel.Initiator = rpcInitiator(historical.IsInitiator)
			channel.CommitmentType = rpcCommitmentType(
				historical.ChanType,
			)

			// Get the number of forwarding packages from the
			// historical channel.
			fwdPkgs, err := historical.LoadFwdPkgs()
			if err != nil {
				rpcsLog.Errorf("unable to load forwarding "+
					"packages for channel:%s, %v",
					historical.ShortChannelID, err)
				return nil, 0, err
			}
			channel.NumForwardingPackages = int64(len(fwdPkgs))

			channel.RemoteBalance = int64(
				historical.LocalCommitment.RemoteBalance.ToSatoshis(),
			)

			customChanBytes, err := encodeCustomChanData(historical)
			if err != nil {
				return nil, 0, fmt.Errorf("unable to encode "+
					"open chan data: %w", err)
			}
			channel.CustomChannelData = customChanBytes

			channel.Private = isPrivate(historical)
			channel.Memo = string(historical.Memo)

		// If the error is non-nil, and not due to older versions of lnd
		// not persisting historical channels, return it.
		default:
			return nil, 0, err
		}

		closeTXID := pendingClose.ClosingTXID.String()

		switch pendingClose.CloseType {

		// A coop closed channel should never be in the "pending close"
		// state. If a node upgraded from an older lnd version in the
		// middle of a their channel confirming, it will be in this
		// state. We log a warning that the channel will not be included
		// in the now deprecated pending close channels field.
		case channeldb.CooperativeClose:
			rpcsLog.Warnf("channel %v cooperatively closed and "+
				"in pending close state",
				pendingClose.ChanPoint)

		// If the channel was force closed, then we'll need to query
		// the utxoNursery for additional information.
		// TODO(halseth): distinguish remote and local case?
		case channeldb.LocalForceClose, channeldb.RemoteForceClose:
			forceClose := &lnrpc.PendingChannelsResponse_ForceClosedChannel{
				Channel:     channel,
				ClosingTxid: closeTXID,
			}

			// Fetch reports from both nursery and resolvers. At the
			// moment this is not an atomic snapshot. This is
			// planned to be resolved when the nursery is removed
			// and channel arbitrator will be the single source for
			// these kind of reports.
			err := r.nurseryPopulateForceCloseResp(
				&chanPoint, currentHeight, forceClose,
			)
			if err != nil {
				rpcsLog.Errorf("unable to populate nursery "+
					"force close resp:%s, %v",
					chanPoint, err)
				return nil, 0, err
			}

			err = r.arbitratorPopulateForceCloseResp(
				&chanPoint, currentHeight, forceClose,
			)
			if err != nil {
				rpcsLog.Errorf("unable to populate arbitrator "+
					"force close resp:%s, %v",
					chanPoint, err)
				return nil, 0, err
			}

			limboBalance += forceClose.LimboBalance
			result = append(result, forceClose)
		}
	}

	return result, limboBalance, nil
}

// fetchWaitingCloseChannels queries the database for a list of channels
// that have their closing transactions broadcast but not confirmed yet.
// The returned result is used in the response of the PendingChannels RPC.
func (r *rpcServer) fetchWaitingCloseChannels(
	includeRawTx bool) (waitingCloseChannels, int64, error) {

	// We'll also fetch all channels that are open, but have had their
	// commitment broadcasted, meaning they are waiting for the closing
	// transaction to confirm.
	channels, err := r.server.chanStateDB.FetchWaitingCloseChannels()
	if err != nil {
		rpcsLog.Errorf("unable to fetch channels waiting close: %v",
			err)
		return nil, 0, err
	}

	result := make(waitingCloseChannels, 0)
	limboBalance := int64(0)

	// getClosingTx is a helper closure that tries to find the closing tx of
	// a given waiting close channel. Notice that if the remote closes the
	// channel, we may not have the closing tx.
	getClosingTx := func(c *channeldb.OpenChannel) (*wire.MsgTx, error) {
		var (
			tx  *wire.MsgTx
			err error
		)

		// First, we try to locate the force closing tx. If not found,
		// we will then try to find its coop closing tx.
		tx, err = c.BroadcastedCommitment()
		if err == nil {
			return tx, nil
		}

		// If the error returned is not ErrNoCloseTx, something
		// unexpected happened and we will return the error.
		if err != channeldb.ErrNoCloseTx {
			return nil, err
		}

		// Otherwise, we continue to locate its coop closing tx.
		tx, err = c.BroadcastedCooperative()
		if err == nil {
			return tx, nil
		}

		// Return the error if it's not ErrNoCloseTx.
		if err != channeldb.ErrNoCloseTx {
			return nil, err
		}

		// Otherwise return an empty tx. This can happen if the remote
		// broadcast the closing tx and we haven't recorded it yet.
		return nil, nil
	}

	for _, waitingClose := range channels {
		pub := waitingClose.IdentityPub.SerializeCompressed()
		chanPoint := waitingClose.FundingOutpoint

		var commitments lnrpc.PendingChannelsResponse_Commitments

		// Report local commit. May not be present when DLP is active.
		if waitingClose.LocalCommitment.CommitTx != nil {
			commitments.LocalTxid =
				waitingClose.LocalCommitment.CommitTx.TxHash().
					String()

			commitments.LocalCommitFeeSat = uint64(
				waitingClose.LocalCommitment.CommitFee,
			)
		}

		// Report remote commit. May not be present when DLP is active.
		if waitingClose.RemoteCommitment.CommitTx != nil {
			commitments.RemoteTxid =
				waitingClose.RemoteCommitment.CommitTx.TxHash().
					String()

			commitments.RemoteCommitFeeSat = uint64(
				waitingClose.RemoteCommitment.CommitFee,
			)
		}

		// Report the remote pending commit if any.
		remoteCommitDiff, err := waitingClose.RemoteCommitChainTip()

		switch {
		// Don't set hash if there is no pending remote commit.
		case err == channeldb.ErrNoPendingCommit:

		// An unexpected error occurred.
		case err != nil:
			return nil, 0, err

		// There is a pending remote commit. Set its hash in the
		// response.
		default:
			hash := remoteCommitDiff.Commitment.CommitTx.TxHash()
			commitments.RemotePendingTxid = hash.String()
			commitments.RemoteCommitFeeSat = uint64(
				remoteCommitDiff.Commitment.CommitFee,
			)
		}

		fwdPkgs, err := waitingClose.LoadFwdPkgs()
		if err != nil {
			rpcsLog.Errorf("unable to load forwarding packages "+
				"for channel:%s, %v",
				waitingClose.ShortChannelID, err)
			return nil, 0, err
		}

		// Get the closing tx.
		// NOTE: the closing tx could be nil here if it's the remote
		// that broadcasted the closing tx.
		closingTx, err := getClosingTx(waitingClose)
		if err != nil {
			rpcsLog.Errorf("unable to find closing tx for "+
				"channel:%s, %v",
				waitingClose.ShortChannelID, err)
			return nil, 0, err
		}

		customChanBytes, err := encodeCustomChanData(waitingClose)
		if err != nil {
			return nil, 0, fmt.Errorf("unable to encode "+
				"open chan data: %w", err)
		}

		localCommit := waitingClose.LocalCommitment
		chanStatus := waitingClose.ChanStatus()
		channel := &lnrpc.PendingChannelsResponse_PendingChannel{
			RemoteNodePub: hex.EncodeToString(pub),
			ChannelPoint:  chanPoint.String(),
			Capacity:      int64(waitingClose.Capacity),
			LocalBalance: int64(
				localCommit.LocalBalance.ToSatoshis(),
			),
			RemoteBalance: int64(
				localCommit.RemoteBalance.ToSatoshis(),
			),
			LocalChanReserveSat: int64(
				waitingClose.LocalChanCfg.ChanReserve,
			),
			RemoteChanReserveSat: int64(
				waitingClose.RemoteChanCfg.ChanReserve,
			),
			Initiator: rpcInitiator(
				waitingClose.IsInitiator,
			),
			CommitmentType: rpcCommitmentType(
				waitingClose.ChanType,
			),
			NumForwardingPackages: int64(len(fwdPkgs)),
			ChanStatusFlags:       chanStatus.String(),
			Private:               isPrivate(waitingClose),
			Memo:                  string(waitingClose.Memo),
			CustomChannelData:     customChanBytes,
		}

		var closingTxid, closingTxHex string
		if closingTx != nil {
			closingTxid = closingTx.TxHash().String()
			if includeRawTx {
				var txBuf bytes.Buffer
				err = closingTx.Serialize(&txBuf)
				if err != nil {
					return nil, 0, fmt.Errorf("failed to "+
						"serialize closing transaction"+
						": %w", err)
				}
				closingTxHex = hex.EncodeToString(txBuf.Bytes())
			}
		}

		waitingCloseResp := &lnrpc.PendingChannelsResponse_WaitingCloseChannel{
			Channel:      channel,
			LimboBalance: channel.LocalBalance,
			Commitments:  &commitments,
			ClosingTxid:  closingTxid,
			ClosingTxHex: closingTxHex,
		}

		// A close tx has been broadcasted, all our balance will be in
		// limbo until it confirms.
		result = append(result, waitingCloseResp)
		limboBalance += channel.LocalBalance
	}

	return result, limboBalance, nil
}

// PendingChannels returns a list of all the channels that are currently
// considered "pending". A channel is pending if it has finished the funding
// workflow and is waiting for confirmations for the funding txn, or is in the
// process of closure, either initiated cooperatively or non-cooperatively.
func (r *rpcServer) PendingChannels(ctx context.Context,
	in *lnrpc.PendingChannelsRequest) (
	*lnrpc.PendingChannelsResponse, error) {

	resp := &lnrpc.PendingChannelsResponse{}

	// First, we find all the channels that will soon be opened.
	pendingOpenChannels, err := r.fetchPendingOpenChannels()
	if err != nil {
		return nil, err
	}
	resp.PendingOpenChannels = pendingOpenChannels

	// Second, we fetch all channels that considered pending force closing.
	// This means the channels here have their closing transactions
	// confirmed but not considered fully resolved yet. For instance, they
	// may have a second level HTLCs to be resolved onchain.
	pendingCloseChannels, limbo, err := r.fetchPendingForceCloseChannels()
	if err != nil {
		return nil, err
	}
	resp.PendingForceClosingChannels = pendingCloseChannels
	resp.TotalLimboBalance = limbo

	// Third, we fetch all channels that are open, but have had their
	// commitment broadcasted, meaning they are waiting for the closing
	// transaction to confirm.
	waitingCloseChannels, limbo, err := r.fetchWaitingCloseChannels(
		in.IncludeRawTx,
	)
	if err != nil {
		return nil, err
	}
	resp.WaitingCloseChannels = waitingCloseChannels
	resp.TotalLimboBalance += limbo

	err = fn.MapOptionZ(
		r.server.implCfg.AuxDataParser,
		func(parser AuxDataParser) error {
			return parser.InlineParseCustomData(resp)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing custom data: %w", err)
	}

	return resp, nil
}

// arbitratorPopulateForceCloseResp populates the pending channels response
// message with channel resolution information from the contract resolvers.
func (r *rpcServer) arbitratorPopulateForceCloseResp(chanPoint *wire.OutPoint,
	currentHeight int32,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel) error {

	// Query for contract resolvers state.
	arbitrator, err := r.server.chainArb.GetChannelArbitrator(*chanPoint)
	if err != nil {
		return err
	}
	reports := arbitrator.Report()

	for _, report := range reports {
		switch report.Type {
		// For a direct output, populate/update the top level
		// response properties.
		case contractcourt.ReportOutputUnencumbered:
			// Populate the maturity height fields for the direct
			// commitment output to us.
			forceClose.MaturityHeight = report.MaturityHeight

			// If the transaction has been confirmed, then we can
			// compute how many blocks it has left.
			if forceClose.MaturityHeight != 0 {
				forceClose.BlocksTilMaturity =
					int32(forceClose.MaturityHeight) -
						currentHeight
			}

		// Add htlcs to the PendingHtlcs response property.
		case contractcourt.ReportOutputIncomingHtlc,
			contractcourt.ReportOutputOutgoingHtlc:

			// Don't report details on htlcs that are no longer in
			// limbo.
			if report.LimboBalance == 0 {
				break
			}

			incoming := report.Type == contractcourt.ReportOutputIncomingHtlc
			htlc := &lnrpc.PendingHTLC{
				Incoming:       incoming,
				Amount:         int64(report.Amount),
				Outpoint:       report.Outpoint.String(),
				MaturityHeight: report.MaturityHeight,
				Stage:          report.Stage,
			}

			if htlc.MaturityHeight != 0 {
				htlc.BlocksTilMaturity =
					int32(htlc.MaturityHeight) - currentHeight
			}

			forceClose.PendingHtlcs = append(forceClose.PendingHtlcs, htlc)

		case contractcourt.ReportOutputAnchor:
			// There are three resolution states for the anchor:
			// limbo, lost and recovered. Derive the current state
			// from the limbo and recovered balances.
			switch {
			case report.RecoveredBalance != 0:
				forceClose.Anchor = lnrpc.PendingChannelsResponse_ForceClosedChannel_RECOVERED

			case report.LimboBalance != 0:
				forceClose.Anchor = lnrpc.PendingChannelsResponse_ForceClosedChannel_LIMBO

			default:
				forceClose.Anchor = lnrpc.PendingChannelsResponse_ForceClosedChannel_LOST
			}

		default:
			return fmt.Errorf("unknown report output type: %v",
				report.Type)
		}

		forceClose.LimboBalance += int64(report.LimboBalance)
		forceClose.RecoveredBalance += int64(report.RecoveredBalance)
	}

	return nil
}

// nurseryPopulateForceCloseResp populates the pending channels response
// message with contract resolution information from utxonursery.
func (r *rpcServer) nurseryPopulateForceCloseResp(chanPoint *wire.OutPoint,
	currentHeight int32,
	forceClose *lnrpc.PendingChannelsResponse_ForceClosedChannel) error {

	// Query for the maturity state for this force closed channel. If we
	// didn't have any time-locked outputs, then the nursery may not know of
	// the contract.
	nurseryInfo, err := r.server.utxoNursery.NurseryReport(chanPoint)
	if err == contractcourt.ErrContractNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("unable to obtain "+
			"nursery report for ChannelPoint(%v): %v",
			chanPoint, err)
	}

	// If the nursery knows of this channel, then we can populate
	// information detailing exactly how much funds are time locked and also
	// the height in which we can ultimately sweep the funds into the
	// wallet.
	forceClose.LimboBalance = int64(nurseryInfo.LimboBalance)
	forceClose.RecoveredBalance = int64(nurseryInfo.RecoveredBalance)

	for _, htlcReport := range nurseryInfo.Htlcs {
		// TODO(conner) set incoming flag appropriately after handling
		// incoming incubation
		htlc := &lnrpc.PendingHTLC{
			Incoming:       false,
			Amount:         int64(htlcReport.Amount),
			Outpoint:       htlcReport.Outpoint.String(),
			MaturityHeight: htlcReport.MaturityHeight,
			Stage:          htlcReport.Stage,
		}

		if htlc.MaturityHeight != 0 {
			htlc.BlocksTilMaturity =
				int32(htlc.MaturityHeight) -
					currentHeight
		}

		forceClose.PendingHtlcs = append(forceClose.PendingHtlcs,
			htlc)
	}

	return nil
}

// ClosedChannels returns a list of all the channels have been closed.
// This does not include channels that are still in the process of closing.
func (r *rpcServer) ClosedChannels(ctx context.Context,
	in *lnrpc.ClosedChannelsRequest) (*lnrpc.ClosedChannelsResponse,
	error) {

	// Show all channels when no filter flags are set.
	filterResults := in.Cooperative || in.LocalForce ||
		in.RemoteForce || in.Breach || in.FundingCanceled ||
		in.Abandoned

	resp := &lnrpc.ClosedChannelsResponse{}

	dbChannels, err := r.server.chanStateDB.FetchClosedChannels(false)
	if err != nil {
		return nil, err
	}

	// In order to make the response easier to parse for clients, we'll
	// sort the set of closed channels by their closing height before
	// serializing the proto response.
	sort.Slice(dbChannels, func(i, j int) bool {
		return dbChannels[i].CloseHeight < dbChannels[j].CloseHeight
	})

	for _, dbChannel := range dbChannels {
		if dbChannel.IsPending {
			continue
		}

		switch dbChannel.CloseType {
		case channeldb.CooperativeClose:
			if filterResults && !in.Cooperative {
				continue
			}
		case channeldb.LocalForceClose:
			if filterResults && !in.LocalForce {
				continue
			}
		case channeldb.RemoteForceClose:
			if filterResults && !in.RemoteForce {
				continue
			}
		case channeldb.BreachClose:
			if filterResults && !in.Breach {
				continue
			}
		case channeldb.FundingCanceled:
			if filterResults && !in.FundingCanceled {
				continue
			}
		case channeldb.Abandoned:
			if filterResults && !in.Abandoned {
				continue
			}
		}

		channel, err := r.createRPCClosedChannel(dbChannel)
		if err != nil {
			return nil, err
		}

		resp.Channels = append(resp.Channels, channel)
	}

	err = fn.MapOptionZ(
		r.server.implCfg.AuxDataParser,
		func(parser AuxDataParser) error {
			return parser.InlineParseCustomData(resp)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing custom data: %w", err)
	}

	return resp, nil
}

// LookupHtlcResolution retrieves a final htlc resolution from the database. If
// the htlc has no final resolution yet, a NotFound grpc status code is
// returned.
func (r *rpcServer) LookupHtlcResolution(
	_ context.Context, in *lnrpc.LookupHtlcResolutionRequest) (
	*lnrpc.LookupHtlcResolutionResponse, error) {

	if !r.cfg.StoreFinalHtlcResolutions {
		return nil, status.Error(codes.Unavailable, "cannot lookup "+
			"with flag --store-final-htlc-resolutions=false")
	}

	chanID := lnwire.NewShortChanIDFromInt(in.ChanId)

	info, err := r.server.chanStateDB.LookupFinalHtlc(chanID, in.HtlcIndex)
	switch {
	case errors.Is(err, channeldb.ErrHtlcUnknown):
		return nil, status.Error(codes.NotFound, err.Error())

	case err != nil:
		return nil, err
	}

	return &lnrpc.LookupHtlcResolutionResponse{
		Settled:  info.Settled,
		Offchain: info.Offchain,
	}, nil
}

// ListChannels returns a description of all the open channels that this node
// is a participant in.
func (r *rpcServer) ListChannels(ctx context.Context,
	in *lnrpc.ListChannelsRequest) (*lnrpc.ListChannelsResponse, error) {

	if in.ActiveOnly && in.InactiveOnly {
		return nil, fmt.Errorf("either `active_only` or " +
			"`inactive_only` can be set, but not both")
	}

	if in.PublicOnly && in.PrivateOnly {
		return nil, fmt.Errorf("either `public_only` or " +
			"`private_only` can be set, but not both")
	}

	if len(in.Peer) > 0 && len(in.Peer) != 33 {
		_, err := route.NewVertexFromBytes(in.Peer)
		return nil, fmt.Errorf("invalid `peer` key: %w", err)
	}

	resp := &lnrpc.ListChannelsResponse{}

	dbChannels, err := r.server.chanStateDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	rpcsLog.Debugf("[listchannels] fetched %v channels from DB",
		len(dbChannels))

	for _, dbChannel := range dbChannels {
		nodePub := dbChannel.IdentityPub
		nodePubBytes := nodePub.SerializeCompressed()
		chanPoint := dbChannel.FundingOutpoint

		// If the caller requested channels for a target node, skip any
		// that don't match the provided pubkey.
		if len(in.Peer) > 0 && !bytes.Equal(nodePubBytes, in.Peer) {
			continue
		}

		var peerOnline bool
		if _, err := r.server.FindPeer(nodePub); err == nil {
			peerOnline = true
		}

		channelID := lnwire.NewChanIDFromOutPoint(chanPoint)
		var linkActive bool
		if link, err := r.server.htlcSwitch.GetLink(channelID); err == nil {
			// A channel is only considered active if it is known
			// by the switch *and* able to forward
			// incoming/outgoing payments.
			linkActive = link.EligibleToForward()
		}

		// Next, we'll determine whether we should add this channel to
		// our list depending on the type of channels requested to us.
		isActive := peerOnline && linkActive
		channel, err := createRPCOpenChannel(
			ctx, r, dbChannel, isActive, in.PeerAliasLookup,
		)
		if err != nil {
			return nil, err
		}

		// We'll only skip returning this channel if we were requested
		// for a specific kind and this channel doesn't satisfy it.
		switch {
		case in.ActiveOnly && !isActive:
			continue
		case in.InactiveOnly && isActive:
			continue
		case in.PublicOnly && channel.Private:
			continue
		case in.PrivateOnly && !channel.Private:
			continue
		}

		resp.Channels = append(resp.Channels, channel)
	}

	err = fn.MapOptionZ(
		r.server.implCfg.AuxDataParser,
		func(parser AuxDataParser) error {
			return parser.InlineParseCustomData(resp)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing custom data: %w", err)
	}

	return resp, nil
}

// rpcCommitmentType takes the channel type and converts it to an rpc commitment
// type value.
func rpcCommitmentType(chanType channeldb.ChannelType) lnrpc.CommitmentType {
	// Extract the commitment type from the channel type flags. We must
	// first check whether it has anchors, since in that case it would also
	// be tweakless.
	switch {
	case chanType.HasTapscriptRoot():
		return lnrpc.CommitmentType_SIMPLE_TAPROOT_OVERLAY

	case chanType.IsTaproot():
		return lnrpc.CommitmentType_SIMPLE_TAPROOT

	case chanType.HasLeaseExpiration():
		return lnrpc.CommitmentType_SCRIPT_ENFORCED_LEASE

	case chanType.HasAnchors():
		return lnrpc.CommitmentType_ANCHORS

	case chanType.IsTweakless():
		return lnrpc.CommitmentType_STATIC_REMOTE_KEY

	default:

		return lnrpc.CommitmentType_LEGACY
	}
}

// createChannelConstraint creates a *lnrpc.ChannelConstraints using the
// *Channeldb.ChannelConfig.
func createChannelConstraint(
	chanCfg *channeldb.ChannelConfig) *lnrpc.ChannelConstraints {
	return &lnrpc.ChannelConstraints{
		CsvDelay:          uint32(chanCfg.CsvDelay),
		ChanReserveSat:    uint64(chanCfg.ChanReserve),
		DustLimitSat:      uint64(chanCfg.DustLimit),
		MaxPendingAmtMsat: uint64(chanCfg.MaxPendingAmount),
		MinHtlcMsat:       uint64(chanCfg.MinHTLC),
		MaxAcceptedHtlcs:  uint32(chanCfg.MaxAcceptedHtlcs),
	}
}

// isPrivate evaluates the ChannelFlags of the db channel to determine if the
// channel is private or not.
func isPrivate(dbChannel *channeldb.OpenChannel) bool {
	if dbChannel == nil {
		return false
	}
	return dbChannel.ChannelFlags&lnwire.FFAnnounceChannel != 1
}

// encodeCustomChanData encodes the custom channel data for the open channel.
// It encodes that data as a pair of var bytes blobs.
func encodeCustomChanData(lnChan *channeldb.OpenChannel) ([]byte, error) {
	customOpenChanData := lnChan.CustomBlob.UnwrapOr(nil)
	customLocalCommitData := lnChan.LocalCommitment.CustomBlob.UnwrapOr(nil)

	// Don't write any custom data if both blobs are empty.
	if len(customOpenChanData) == 0 && len(customLocalCommitData) == 0 {
		return nil, nil
	}

	// We'll encode our custom channel data as two blobs. The first is a
	// set of var bytes encoding of the open chan data, the second is an
	// encoding of the local commitment data.
	var customChanDataBuf bytes.Buffer
	err := wire.WriteVarBytes(&customChanDataBuf, 0, customOpenChanData)
	if err != nil {
		return nil, fmt.Errorf("unable to encode open chan "+
			"data: %w", err)
	}
	err = wire.WriteVarBytes(&customChanDataBuf, 0, customLocalCommitData)
	if err != nil {
		return nil, fmt.Errorf("unable to encode local commit "+
			"data: %w", err)
	}

	return customChanDataBuf.Bytes(), nil
}

// createRPCOpenChannel creates an *lnrpc.Channel from the *channeldb.Channel.
//
//nolint:funlen
func createRPCOpenChannel(ctx context.Context, r *rpcServer,
	dbChannel *channeldb.OpenChannel,
	isActive, peerAliasLookup bool) (*lnrpc.Channel, error) {

	nodePub := dbChannel.IdentityPub
	nodeID := hex.EncodeToString(nodePub.SerializeCompressed())
	chanPoint := dbChannel.FundingOutpoint
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	// As this is required for display purposes, we'll calculate
	// the weight of the commitment transaction. We also add on the
	// estimated weight of the witness to calculate the weight of
	// the transaction if it were to be immediately unilaterally
	// broadcast.
	var witnessWeight int64
	if dbChannel.ChanType.IsTaproot() {
		witnessWeight = input.TaprootKeyPathWitnessSize
	} else {
		witnessWeight = input.WitnessCommitmentTxWeight
	}

	localCommit := dbChannel.LocalCommitment
	utx := btcutil.NewTx(localCommit.CommitTx)
	commitBaseWeight := blockchain.GetTransactionWeight(utx)
	commitWeight := commitBaseWeight + witnessWeight

	localBalance := localCommit.LocalBalance
	remoteBalance := localCommit.RemoteBalance

	// As an artifact of our usage of mSAT internally, either party
	// may end up in a state where they're holding a fractional
	// amount of satoshis which can't be expressed within the
	// actual commitment output. Since we round down when going
	// from mSAT -> SAT, we may at any point be adding an
	// additional SAT to miners fees. As a result, we display a
	// commitment fee that accounts for this externally.
	var sumOutputs btcutil.Amount
	for _, txOut := range localCommit.CommitTx.TxOut {
		sumOutputs += btcutil.Amount(txOut.Value)
	}
	externalCommitFee := dbChannel.Capacity - sumOutputs

	// Extract the commitment type from the channel type flags.
	commitmentType := rpcCommitmentType(dbChannel.ChanType)

	dbScid := dbChannel.ShortChannelID

	// Fetch the set of aliases for the channel.
	channelAliases := r.server.aliasMgr.GetAliases(dbScid)

	// Fetch the peer alias. If one does not exist, errNoPeerAlias
	// is returned and peerScidAlias will be an empty ShortChannelID.
	peerScidAlias, _ := r.server.aliasMgr.GetPeerAlias(chanID)

	// Finally we'll attempt to encode the custom channel data if any
	// exists.
	customChanBytes, err := encodeCustomChanData(dbChannel)
	if err != nil {
		return nil, fmt.Errorf("unable to encode open chan data: %w",
			err)
	}

	channel := &lnrpc.Channel{
		Active:                isActive,
		Private:               isPrivate(dbChannel),
		RemotePubkey:          nodeID,
		ChannelPoint:          chanPoint.String(),
		ChanId:                dbScid.ToUint64(),
		Capacity:              int64(dbChannel.Capacity),
		LocalBalance:          int64(localBalance.ToSatoshis()),
		RemoteBalance:         int64(remoteBalance.ToSatoshis()),
		CommitFee:             int64(externalCommitFee),
		CommitWeight:          commitWeight,
		FeePerKw:              int64(localCommit.FeePerKw),
		TotalSatoshisSent:     int64(dbChannel.TotalMSatSent.ToSatoshis()),
		TotalSatoshisReceived: int64(dbChannel.TotalMSatReceived.ToSatoshis()),
		NumUpdates:            localCommit.CommitHeight,
		PendingHtlcs:          make([]*lnrpc.HTLC, len(localCommit.Htlcs)),
		Initiator:             dbChannel.IsInitiator,
		ChanStatusFlags:       dbChannel.ChanStatus().String(),
		StaticRemoteKey:       commitmentType == lnrpc.CommitmentType_STATIC_REMOTE_KEY,
		CommitmentType:        commitmentType,
		ThawHeight:            dbChannel.ThawHeight,
		LocalConstraints: createChannelConstraint(
			&dbChannel.LocalChanCfg,
		),
		RemoteConstraints: createChannelConstraint(
			&dbChannel.RemoteChanCfg,
		),
		AliasScids:            make([]uint64, 0, len(channelAliases)),
		PeerScidAlias:         peerScidAlias.ToUint64(),
		ZeroConf:              dbChannel.IsZeroConf(),
		ZeroConfConfirmedScid: dbChannel.ZeroConfRealScid().ToUint64(),
		Memo:                  string(dbChannel.Memo),
		CustomChannelData:     customChanBytes,
		// TODO: remove the following deprecated fields
		CsvDelay:             uint32(dbChannel.LocalChanCfg.CsvDelay),
		LocalChanReserveSat:  int64(dbChannel.LocalChanCfg.ChanReserve),
		RemoteChanReserveSat: int64(dbChannel.RemoteChanCfg.ChanReserve),
	}

	// Look up our channel peer's node alias if the caller requests it.
	if peerAliasLookup {
		peerAlias, err := r.server.graphDB.LookupAlias(ctx, nodePub)
		if err != nil {
			peerAlias = fmt.Sprintf("unable to lookup "+
				"peer alias: %v", err)
		}
		channel.PeerAlias = peerAlias
	}

	// Populate the set of aliases.
	for _, chanAlias := range channelAliases {
		channel.AliasScids = append(
			channel.AliasScids, chanAlias.ToUint64(),
		)
	}

	// Create two sets of the HTLCs found in the remote commitment, which is
	// used to decide whether the HTLCs from the local commitment has been
	// locked in or not.
	remoteIncomingHTLCs := fn.NewSet[uint64]()
	remoteOutgoingHTLCs := fn.NewSet[uint64]()
	for _, htlc := range dbChannel.RemoteCommitment.Htlcs {
		if htlc.Incoming {
			remoteIncomingHTLCs.Add(htlc.HtlcIndex)
		} else {
			remoteOutgoingHTLCs.Add(htlc.HtlcIndex)
		}
	}

	for i, htlc := range localCommit.Htlcs {
		var rHash [32]byte
		copy(rHash[:], htlc.RHash[:])

		circuitMap := r.server.htlcSwitch.CircuitLookup()

		var (
			forwardingChannel, forwardingHtlcIndex uint64
			lockedIn                               bool
		)
		switch {
		case htlc.Incoming:
			circuit := circuitMap.LookupCircuit(
				htlcswitch.CircuitKey{
					ChanID: dbChannel.ShortChannelID,
					HtlcID: htlc.HtlcIndex,
				},
			)
			if circuit != nil && circuit.Outgoing != nil {
				forwardingChannel = circuit.Outgoing.ChanID.
					ToUint64()

				forwardingHtlcIndex = circuit.Outgoing.HtlcID
			}

			lockedIn = remoteIncomingHTLCs.Contains(htlc.HtlcIndex)

		case !htlc.Incoming:
			circuit := circuitMap.LookupOpenCircuit(
				htlcswitch.CircuitKey{
					ChanID: dbChannel.ShortChannelID,
					HtlcID: htlc.HtlcIndex,
				},
			)

			// If the incoming channel id is the special hop.Source
			// value, the htlc index is a local payment identifier.
			// In this case, report nothing.
			if circuit != nil &&
				circuit.Incoming.ChanID != hop.Source {

				forwardingChannel = circuit.Incoming.ChanID.
					ToUint64()

				forwardingHtlcIndex = circuit.Incoming.HtlcID
			}

			lockedIn = remoteOutgoingHTLCs.Contains(htlc.HtlcIndex)
		}

		channel.PendingHtlcs[i] = &lnrpc.HTLC{
			Incoming:            htlc.Incoming,
			Amount:              int64(htlc.Amt.ToSatoshis()),
			HashLock:            rHash[:],
			ExpirationHeight:    htlc.RefundTimeout,
			HtlcIndex:           htlc.HtlcIndex,
			ForwardingChannel:   forwardingChannel,
			ForwardingHtlcIndex: forwardingHtlcIndex,
			LockedIn:            lockedIn,
		}

		// Add the Pending Htlc Amount to UnsettledBalance field.
		channel.UnsettledBalance += channel.PendingHtlcs[i].Amount
	}

	// If we initiated opening the channel, the zero height remote balance
	// is the push amount. Otherwise, our starting balance is the push
	// amount. If there is no push amount, these values will simply be zero.
	if dbChannel.IsInitiator {
		amt := dbChannel.InitialRemoteBalance.ToSatoshis()
		channel.PushAmountSat = uint64(amt)
	} else {
		amt := dbChannel.InitialLocalBalance.ToSatoshis()
		channel.PushAmountSat = uint64(amt)
	}

	if len(dbChannel.LocalShutdownScript) > 0 {
		_, addresses, _, err := txscript.ExtractPkScriptAddrs(
			dbChannel.LocalShutdownScript, r.cfg.ActiveNetParams.Params,
		)
		if err != nil {
			return nil, err
		}

		// We only expect one upfront shutdown address for a channel. If
		// LocalShutdownScript is non-zero, there should be one payout
		// address set.
		if len(addresses) != 1 {
			return nil, fmt.Errorf("expected one upfront shutdown "+
				"address, got: %v", len(addresses))
		}

		channel.CloseAddress = addresses[0].String()
	}

	// If the server hasn't fully started yet, it's possible that the
	// channel event store hasn't either, so it won't be able to consume any
	// requests until then. To prevent blocking, we'll just omit the uptime
	// related fields for now.
	if !r.server.Started() {
		return channel, nil
	}

	peer, err := route.NewVertexFromBytes(nodePub.SerializeCompressed())
	if err != nil {
		return nil, err
	}

	// Query the event store for additional information about the channel.
	// Do not fail if it is not available, because there is a potential
	// race between a channel being added to our node and the event store
	// being notified of it.
	outpoint := dbChannel.FundingOutpoint
	info, err := r.server.chanEventStore.GetChanInfo(outpoint, peer)
	switch {
	// If the store does not know about the peer, we just log it.
	case errors.Is(err, chanfitness.ErrPeerNotFound):
		rpcsLog.Warnf("peer: %v not found by channel event store",
			peer)

	// If the store does not know about the channel, we just log it.
	case errors.Is(err, chanfitness.ErrChannelNotFound):
		rpcsLog.Warnf("channel: %v not found by channel event store",
			outpoint)

	// If we got our channel info, we further populate the channel.
	case err == nil:
		channel.Uptime = int64(info.Uptime.Seconds())
		channel.Lifetime = int64(info.Lifetime.Seconds())

	// If we get an unexpected error, we return it.
	default:
		return nil, err
	}

	return channel, nil
}

// createRPCClosedChannel creates an *lnrpc.ClosedChannelSummary from a
// *channeldb.ChannelCloseSummary.
func (r *rpcServer) createRPCClosedChannel(
	dbChannel *channeldb.ChannelCloseSummary) (*lnrpc.ChannelCloseSummary,
	error) {

	nodePub := dbChannel.RemotePub
	nodeID := hex.EncodeToString(nodePub.SerializeCompressed())

	var (
		closeType      lnrpc.ChannelCloseSummary_ClosureType
		openInit       lnrpc.Initiator
		closeInitiator lnrpc.Initiator
		err            error
	)

	// Lookup local and remote cooperative initiators. If these values
	// are not known they will just return unknown.
	openInit, closeInitiator, err = r.getInitiators(&dbChannel.ChanPoint)
	if err != nil {
		return nil, err
	}

	// Convert the close type to rpc type.
	switch dbChannel.CloseType {
	case channeldb.CooperativeClose:
		closeType = lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE
	case channeldb.LocalForceClose:
		closeType = lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE
	case channeldb.RemoteForceClose:
		closeType = lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE
	case channeldb.BreachClose:
		closeType = lnrpc.ChannelCloseSummary_BREACH_CLOSE
	case channeldb.FundingCanceled:
		closeType = lnrpc.ChannelCloseSummary_FUNDING_CANCELED
	case channeldb.Abandoned:
		closeType = lnrpc.ChannelCloseSummary_ABANDONED
	}

	dbScid := dbChannel.ShortChanID

	// Fetch the set of aliases for this channel.
	channelAliases := r.server.aliasMgr.GetAliases(dbScid)

	channel := &lnrpc.ChannelCloseSummary{
		Capacity:          int64(dbChannel.Capacity),
		RemotePubkey:      nodeID,
		CloseHeight:       dbChannel.CloseHeight,
		CloseType:         closeType,
		ChannelPoint:      dbChannel.ChanPoint.String(),
		ChanId:            dbChannel.ShortChanID.ToUint64(),
		SettledBalance:    int64(dbChannel.SettledBalance),
		TimeLockedBalance: int64(dbChannel.TimeLockedBalance),
		ChainHash:         dbChannel.ChainHash.String(),
		ClosingTxHash:     dbChannel.ClosingTXID.String(),
		OpenInitiator:     openInit,
		CloseInitiator:    closeInitiator,
		AliasScids:        make([]uint64, 0, len(channelAliases)),
	}

	// Populate the set of aliases.
	for _, chanAlias := range channelAliases {
		channel.AliasScids = append(
			channel.AliasScids, chanAlias.ToUint64(),
		)
	}

	// Populate any historical data that the summary needs.
	histChan, err := r.server.chanStateDB.FetchHistoricalChannel(
		&dbChannel.ChanPoint,
	)
	switch err {
	// The channel was closed in a pre-historic version of lnd. Ignore the
	// error.
	case channeldb.ErrNoHistoricalBucket:
	case channeldb.ErrChannelNotFound:

	case nil:
		if histChan.IsZeroConf() && histChan.ZeroConfConfirmed() {
			// If the channel was zero-conf, it may have confirmed.
			// Populate the confirmed SCID if so.
			confirmedScid := histChan.ZeroConfRealScid().ToUint64()
			channel.ZeroConfConfirmedScid = confirmedScid
		}

		// Finally we'll attempt to encode the custom channel data if
		// any exists.
		channel.CustomChannelData, err = encodeCustomChanData(histChan)
		if err != nil {
			return nil, fmt.Errorf("unable to encode open chan "+
				"data: %w", err)
		}

	// Non-nil error not due to older versions of lnd.
	default:
		return nil, err
	}

	reports, err := r.server.miscDB.FetchChannelReports(
		*r.cfg.ActiveNetParams.GenesisHash, &dbChannel.ChanPoint,
	)
	switch err {
	// If the channel does not have its resolver outcomes stored,
	// ignore it.
	case channeldb.ErrNoChainHashBucket:
		fallthrough
	case channeldb.ErrNoChannelSummaries:
		return channel, nil

	// If there is no error, fallthrough the switch to process reports.
	case nil:

	// If another error occurred, return it.
	default:
		return nil, err
	}

	for _, report := range reports {
		rpcResolution, err := rpcChannelResolution(report)
		if err != nil {
			return nil, err
		}

		channel.Resolutions = append(channel.Resolutions, rpcResolution)
	}

	return channel, nil
}

func rpcChannelResolution(report *channeldb.ResolverReport) (*lnrpc.Resolution,
	error) {

	res := &lnrpc.Resolution{
		AmountSat: uint64(report.Amount),
		Outpoint:  lnrpc.MarshalOutPoint(&report.OutPoint),
	}

	if report.SpendTxID != nil {
		res.SweepTxid = report.SpendTxID.String()
	}

	switch report.ResolverType {
	case channeldb.ResolverTypeAnchor:
		res.ResolutionType = lnrpc.ResolutionType_ANCHOR

	case channeldb.ResolverTypeIncomingHtlc:
		res.ResolutionType = lnrpc.ResolutionType_INCOMING_HTLC

	case channeldb.ResolverTypeOutgoingHtlc:
		res.ResolutionType = lnrpc.ResolutionType_OUTGOING_HTLC

	case channeldb.ResolverTypeCommit:
		res.ResolutionType = lnrpc.ResolutionType_COMMIT

	default:
		return nil, fmt.Errorf("unknown resolver type: %v",
			report.ResolverType)
	}

	switch report.ResolverOutcome {
	case channeldb.ResolverOutcomeClaimed:
		res.Outcome = lnrpc.ResolutionOutcome_CLAIMED

	case channeldb.ResolverOutcomeUnclaimed:
		res.Outcome = lnrpc.ResolutionOutcome_UNCLAIMED

	case channeldb.ResolverOutcomeAbandoned:
		res.Outcome = lnrpc.ResolutionOutcome_ABANDONED

	case channeldb.ResolverOutcomeFirstStage:
		res.Outcome = lnrpc.ResolutionOutcome_FIRST_STAGE

	case channeldb.ResolverOutcomeTimeout:
		res.Outcome = lnrpc.ResolutionOutcome_TIMEOUT

	default:
		return nil, fmt.Errorf("unknown outcome: %v",
			report.ResolverOutcome)
	}

	return res, nil
}

// getInitiators returns an initiator enum that provides information about the
// party that initiated channel's open and close. This information is obtained
// from the historical channel bucket, so unknown values are returned when the
// channel is not present (which indicates that it was closed before we started
// writing channels to the historical close bucket).
func (r *rpcServer) getInitiators(chanPoint *wire.OutPoint) (
	lnrpc.Initiator,
	lnrpc.Initiator, error) {

	var (
		openInitiator  = lnrpc.Initiator_INITIATOR_UNKNOWN
		closeInitiator = lnrpc.Initiator_INITIATOR_UNKNOWN
	)

	// To get the close initiator for cooperative closes, we need
	// to get the channel status from the historical channel bucket.
	histChan, err := r.server.chanStateDB.FetchHistoricalChannel(chanPoint)
	switch {
	// The node has upgraded from a version where we did not store
	// historical channels, and has not closed a channel since. Do
	// not return an error, initiator values are unknown.
	case err == channeldb.ErrNoHistoricalBucket:
		return openInitiator, closeInitiator, nil

	// The channel was closed before we started storing historical
	// channels. Do  not return an error, initiator values are unknown.
	case err == channeldb.ErrChannelNotFound:
		return openInitiator, closeInitiator, nil

	case err != nil:
		return 0, 0, err
	}

	// If we successfully looked up the channel, determine initiator based
	// on channels status.
	if histChan.IsInitiator {
		openInitiator = lnrpc.Initiator_INITIATOR_LOCAL
	} else {
		openInitiator = lnrpc.Initiator_INITIATOR_REMOTE
	}

	localInit := histChan.HasChanStatus(
		channeldb.ChanStatusLocalCloseInitiator,
	)

	remoteInit := histChan.HasChanStatus(
		channeldb.ChanStatusRemoteCloseInitiator,
	)

	switch {
	// There is a possible case where closes were attempted by both parties.
	// We return the initiator as both in this case to provide full
	// information about the close.
	case localInit && remoteInit:
		closeInitiator = lnrpc.Initiator_INITIATOR_BOTH

	case localInit:
		closeInitiator = lnrpc.Initiator_INITIATOR_LOCAL

	case remoteInit:
		closeInitiator = lnrpc.Initiator_INITIATOR_REMOTE
	}

	return openInitiator, closeInitiator, nil
}

// SubscribeChannelEvents returns a uni-directional stream (server -> client)
// for notifying the client of newly active, inactive or closed channels.
func (r *rpcServer) SubscribeChannelEvents(req *lnrpc.ChannelEventSubscription,
	updateStream lnrpc.Lightning_SubscribeChannelEventsServer) error {

	channelEventSub, err := r.server.channelNotifier.SubscribeChannelEvents()
	if err != nil {
		return err
	}

	// Ensure that the resources for the client is cleaned up once either
	// the server, or client exits.
	defer channelEventSub.Cancel()

	for {
		//nolint:ll
		select {
		// A new update has been sent by the channel router, we'll
		// marshal it into the form expected by the gRPC client, then
		// send it off to the client(s).
		case e := <-channelEventSub.Updates():
			var update *lnrpc.ChannelEventUpdate
			switch event := e.(type) {
			case channelnotifier.PendingOpenChannelEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_PENDING_OPEN_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_PendingOpenChannel{
						PendingOpenChannel: &lnrpc.PendingUpdate{
							Txid:        event.ChannelPoint.Hash[:],
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}
			case channelnotifier.OpenChannelEvent:
				channel, err := createRPCOpenChannel(
					updateStream.Context(), r,
					event.Channel, true, false,
				)
				if err != nil {
					return err
				}

				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_OPEN_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_OpenChannel{
						OpenChannel: channel,
					},
				}

			case channelnotifier.ClosedChannelEvent:
				closedChannel, err := r.createRPCClosedChannel(
					event.CloseSummary,
				)
				if err != nil {
					return err
				}

				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_CLOSED_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_ClosedChannel{
						ClosedChannel: closedChannel,
					},
				}

			case channelnotifier.ActiveChannelEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_ACTIVE_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_ActiveChannel{
						ActiveChannel: &lnrpc.ChannelPoint{
							FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
								FundingTxidBytes: event.ChannelPoint.Hash[:],
							},
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}

			case channelnotifier.InactiveChannelEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_INACTIVE_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_InactiveChannel{
						InactiveChannel: &lnrpc.ChannelPoint{
							FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
								FundingTxidBytes: event.ChannelPoint.Hash[:],
							},
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}

			// Completely ignore ActiveLinkEvent and
			// InactiveLinkEvent as this is explicitly not exposed
			// to the RPC.
			case channelnotifier.ActiveLinkEvent,
				channelnotifier.InactiveLinkEvent:

				continue

			case channelnotifier.FullyResolvedChannelEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_FULLY_RESOLVED_CHANNEL,
					Channel: &lnrpc.ChannelEventUpdate_FullyResolvedChannel{
						FullyResolvedChannel: &lnrpc.ChannelPoint{
							FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
								FundingTxidBytes: event.ChannelPoint.Hash[:],
							},
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}

			case channelnotifier.FundingTimeoutEvent:
				update = &lnrpc.ChannelEventUpdate{
					Type: lnrpc.ChannelEventUpdate_CHANNEL_FUNDING_TIMEOUT,
					Channel: &lnrpc.ChannelEventUpdate_ChannelFundingTimeout{
						ChannelFundingTimeout: &lnrpc.ChannelPoint{
							FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
								FundingTxidBytes: event.ChannelPoint.Hash[:],
							},
							OutputIndex: event.ChannelPoint.Index,
						},
					},
				}

			default:
				return fmt.Errorf("unexpected channel event update: %v", event)
			}

			if err := updateStream.Send(update); err != nil {
				return err
			}

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-updateStream.Context().Done():
			if errors.Is(updateStream.Context().Err(), context.Canceled) {
				return nil
			}
			return updateStream.Context().Err()

		case <-r.quit:
			return nil
		}
	}
}

// paymentStream enables different types of payment streams, such as:
// lnrpc.Lightning_SendPaymentServer and lnrpc.Lightning_SendToRouteServer to
// execute sendPayment. We use this struct as a sort of bridge to enable code
// re-use between SendPayment and SendToRoute.
type paymentStream struct {
	getCtx func() context.Context
	recv   func() (*rpcPaymentRequest, error)
	send   func(*lnrpc.SendResponse) error
}

// rpcPaymentRequest wraps lnrpc.SendRequest so that routes from
// lnrpc.SendToRouteRequest can be passed to sendPayment.
type rpcPaymentRequest struct {
	*lnrpc.SendRequest
	route *route.Route
}

// SendPayment dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network. A single RPC invocation creates a persistent
// bi-directional stream allowing clients to rapidly send payments through the
// Lightning Network with a single persistent connection.
func (r *rpcServer) SendPayment(
	stream lnrpc.Lightning_SendPaymentServer) error {

	var lock sync.Mutex

	return r.sendPayment(&paymentStream{
		getCtx: stream.Context,
		recv: func() (*rpcPaymentRequest, error) {
			req, err := stream.Recv()
			if err != nil {
				return nil, err
			}

			return &rpcPaymentRequest{
				SendRequest: req,
			}, nil
		},
		send: func(r *lnrpc.SendResponse) error {
			// Calling stream.Send concurrently is not safe.
			lock.Lock()
			defer lock.Unlock()
			return stream.Send(r)
		},
	})
}

// SendToRoute dispatches a bi-directional streaming RPC for sending payments
// through the Lightning Network via predefined routes passed in. A single RPC
// invocation creates a persistent bi-directional stream allowing clients to
// rapidly send payments through the Lightning Network with a single persistent
// connection.
func (r *rpcServer) SendToRoute(
	stream lnrpc.Lightning_SendToRouteServer) error {

	var lock sync.Mutex

	return r.sendPayment(&paymentStream{
		getCtx: stream.Context,
		recv: func() (*rpcPaymentRequest, error) {
			req, err := stream.Recv()
			if err != nil {
				return nil, err
			}

			return r.unmarshallSendToRouteRequest(req)
		},
		send: func(r *lnrpc.SendResponse) error {
			// Calling stream.Send concurrently is not safe.
			lock.Lock()
			defer lock.Unlock()
			return stream.Send(r)
		},
	})
}

// unmarshallSendToRouteRequest unmarshalls an rpc sendtoroute request
func (r *rpcServer) unmarshallSendToRouteRequest(
	req *lnrpc.SendToRouteRequest) (*rpcPaymentRequest, error) {

	if req.Route == nil {
		return nil, fmt.Errorf("unable to send, no route provided")
	}

	route, err := r.routerBackend.UnmarshallRoute(req.Route)
	if err != nil {
		return nil, err
	}

	return &rpcPaymentRequest{
		SendRequest: &lnrpc.SendRequest{
			PaymentHash:       req.PaymentHash,
			PaymentHashString: req.PaymentHashString,
		},
		route: route,
	}, nil
}

// rpcPaymentIntent is a small wrapper struct around the of values we can
// receive from a client over RPC if they wish to send a payment. We'll either
// extract these fields from a payment request (which may include routing
// hints), or we'll get a fully populated route from the user that we'll pass
// directly to the channel router for dispatching.
type rpcPaymentIntent struct {
	msat               lnwire.MilliSatoshi
	feeLimit           lnwire.MilliSatoshi
	cltvLimit          uint32
	dest               route.Vertex
	rHash              [32]byte
	cltvDelta          uint16
	routeHints         [][]zpay32.HopHint
	outgoingChannelIDs []uint64
	lastHop            *route.Vertex
	destFeatures       *lnwire.FeatureVector
	paymentAddr        fn.Option[[32]byte]
	payReq             []byte
	metadata           []byte
	blindedPathSet     *routing.BlindedPaymentPathSet

	destCustomRecords record.CustomSet

	route *route.Route
}

// extractPaymentIntent attempts to parse the complete details required to
// dispatch a client from the information presented by an RPC client. There are
// three ways a client can specify their payment details: a payment request,
// via manual details, or via a complete route.
//
//nolint:funlen
func (r *rpcServer) extractPaymentIntent(
	rpcPayReq *rpcPaymentRequest) (rpcPaymentIntent, error) {

	payIntent := rpcPaymentIntent{}

	// If a route was specified, then we can use that directly.
	if rpcPayReq.route != nil {
		// If the user is using the REST interface, then they'll be
		// passing the payment hash as a hex encoded string.
		if rpcPayReq.PaymentHashString != "" {
			paymentHash, err := hex.DecodeString(
				rpcPayReq.PaymentHashString,
			)
			if err != nil {
				return payIntent, err
			}

			copy(payIntent.rHash[:], paymentHash)
		} else {
			copy(payIntent.rHash[:], rpcPayReq.PaymentHash)
		}

		payIntent.route = rpcPayReq.route
		return payIntent, nil
	}

	// If there are no routes specified, pass along a outgoing channel
	// restriction if specified. The main server rpc does not support
	// multiple channel restrictions.
	if rpcPayReq.OutgoingChanId != 0 {
		payIntent.outgoingChannelIDs = []uint64{
			rpcPayReq.OutgoingChanId,
		}
	}

	// Pass along a last hop restriction if specified.
	if len(rpcPayReq.LastHopPubkey) > 0 {
		lastHop, err := route.NewVertexFromBytes(
			rpcPayReq.LastHopPubkey,
		)
		if err != nil {
			return payIntent, err
		}
		payIntent.lastHop = &lastHop
	}

	// Take the CLTV limit from the request if set, otherwise use the max.
	cltvLimit, err := routerrpc.ValidateCLTVLimit(
		rpcPayReq.CltvLimit, r.cfg.MaxOutgoingCltvExpiry,
	)
	if err != nil {
		return payIntent, err
	}
	payIntent.cltvLimit = cltvLimit

	customRecords := record.CustomSet(rpcPayReq.DestCustomRecords)
	if err := customRecords.Validate(); err != nil {
		return payIntent, err
	}
	payIntent.destCustomRecords = customRecords

	validateDest := func(dest route.Vertex) error {
		if rpcPayReq.AllowSelfPayment {
			return nil
		}

		if dest == r.selfNode {
			return errors.New("self-payments not allowed")
		}

		return nil
	}

	// If the payment request field isn't blank, then the details of the
	// invoice are encoded entirely within the encoded payReq.  So we'll
	// attempt to decode it, populating the payment accordingly.
	if rpcPayReq.PaymentRequest != "" {
		payReq, err := zpay32.Decode(
			rpcPayReq.PaymentRequest, r.cfg.ActiveNetParams.Params,
			zpay32.WithErrorOnUnknownFeatureBit(),
		)
		if err != nil {
			return payIntent, err
		}

		// Next, we'll ensure that this payreq hasn't already expired.
		err = routerrpc.ValidatePayReqExpiry(
			r.routerBackend.Clock, payReq,
		)
		if err != nil {
			return payIntent, err
		}

		// If the amount was not included in the invoice, then we let
		// the payer specify the amount of satoshis they wish to send.
		// We override the amount to pay with the amount provided from
		// the payment request.
		if payReq.MilliSat == nil {
			amt, err := lnrpc.UnmarshallAmt(
				rpcPayReq.Amt, rpcPayReq.AmtMsat,
			)
			if err != nil {
				return payIntent, err
			}
			if amt == 0 {
				return payIntent, errors.New("amount must be " +
					"specified when paying a zero amount " +
					"invoice")
			}

			payIntent.msat = amt
		} else {
			payIntent.msat = *payReq.MilliSat
		}

		// Calculate the fee limit that should be used for this payment.
		payIntent.feeLimit = lnrpc.CalculateFeeLimit(
			rpcPayReq.FeeLimit, payIntent.msat,
		)

		copy(payIntent.rHash[:], payReq.PaymentHash[:])
		destKey := payReq.Destination.SerializeCompressed()
		copy(payIntent.dest[:], destKey)
		payIntent.cltvDelta = uint16(payReq.MinFinalCLTVExpiry())
		payIntent.routeHints = payReq.RouteHints
		payIntent.payReq = []byte(rpcPayReq.PaymentRequest)
		payIntent.destFeatures = payReq.Features
		payIntent.paymentAddr = payReq.PaymentAddr
		payIntent.metadata = payReq.Metadata

		if len(payReq.BlindedPaymentPaths) > 0 {
			pathSet, err := routerrpc.BuildBlindedPathSet(
				payReq.BlindedPaymentPaths,
			)
			if err != nil {
				return payIntent, err
			}
			payIntent.blindedPathSet = pathSet

			// Replace the destination node with the target public
			// key of the blinded path set.
			copy(
				payIntent.dest[:],
				pathSet.TargetPubKey().SerializeCompressed(),
			)

			pathFeatures := pathSet.Features()
			if !pathFeatures.IsEmpty() {
				payIntent.destFeatures = pathFeatures.Clone()
			}
		}

		if err := validateDest(payIntent.dest); err != nil {
			return payIntent, err
		}

		// Do bounds checking with the block padding.
		err = routing.ValidateCLTVLimit(
			payIntent.cltvLimit, payIntent.cltvDelta, true,
		)
		if err != nil {
			return payIntent, err
		}

		return payIntent, nil
	}

	// At this point, a destination MUST be specified, so we'll convert it
	// into the proper representation now. The destination will either be
	// encoded as raw bytes, or via a hex string.
	var pubBytes []byte
	if len(rpcPayReq.Dest) != 0 {
		pubBytes = rpcPayReq.Dest
	} else {
		var err error
		pubBytes, err = hex.DecodeString(rpcPayReq.DestString)
		if err != nil {
			return payIntent, err
		}
	}
	if len(pubBytes) != 33 {
		return payIntent, errors.New("invalid key length")
	}
	copy(payIntent.dest[:], pubBytes)

	if err := validateDest(payIntent.dest); err != nil {
		return payIntent, err
	}

	// Payment address may not be needed by legacy invoices.
	if len(rpcPayReq.PaymentAddr) != 0 && len(rpcPayReq.PaymentAddr) != 32 {
		return payIntent, errors.New("invalid payment address length")
	}

	// Set the payment address if it was explicitly defined with the
	// rpcPaymentRequest.
	// Note that the payment address for the payIntent should be nil if none
	// was provided with the rpcPaymentRequest.
	if len(rpcPayReq.PaymentAddr) != 0 {
		var addr [32]byte
		copy(addr[:], rpcPayReq.PaymentAddr)
		payIntent.paymentAddr = fn.Some(addr)
	}

	// Otherwise, If the payment request field was not specified
	// (and a custom route wasn't specified), construct the payment
	// from the other fields.
	payIntent.msat, err = lnrpc.UnmarshallAmt(
		rpcPayReq.Amt, rpcPayReq.AmtMsat,
	)
	if err != nil {
		return payIntent, err
	}

	// Calculate the fee limit that should be used for this payment.
	payIntent.feeLimit = lnrpc.CalculateFeeLimit(
		rpcPayReq.FeeLimit, payIntent.msat,
	)

	if rpcPayReq.FinalCltvDelta != 0 {
		payIntent.cltvDelta = uint16(rpcPayReq.FinalCltvDelta)
	} else {
		// If no final cltv delta is given, assume the default that we
		// use when creating an invoice. We do not assume the default of
		// 9 blocks that is defined in BOLT-11, because this is never
		// enough for other lnd nodes.
		payIntent.cltvDelta = uint16(r.cfg.Bitcoin.TimeLockDelta)
	}

	// Do bounds checking with the block padding so the router isn't left
	// with a zombie payment in case the user messes up.
	err = routing.ValidateCLTVLimit(
		payIntent.cltvLimit, payIntent.cltvDelta, true,
	)
	if err != nil {
		return payIntent, err
	}

	// If the user is manually specifying payment details, then the payment
	// hash may be encoded as a string.
	switch {
	case rpcPayReq.PaymentHashString != "":
		paymentHash, err := hex.DecodeString(
			rpcPayReq.PaymentHashString,
		)
		if err != nil {
			return payIntent, err
		}

		copy(payIntent.rHash[:], paymentHash)

	default:
		copy(payIntent.rHash[:], rpcPayReq.PaymentHash)
	}

	// Unmarshal any custom destination features.
	payIntent.destFeatures = routerrpc.UnmarshalFeatures(
		rpcPayReq.DestFeatures,
	)

	return payIntent, nil
}

type paymentIntentResponse struct {
	Route    *route.Route
	Preimage [32]byte
	Err      error
}

// dispatchPaymentIntent attempts to fully dispatch an RPC payment intent.
// We'll either pass the payment as a whole to the channel router, or give it a
// pre-built route. The first error this method returns denotes if we were
// unable to save the payment. The second error returned denotes if the payment
// didn't succeed.
func (r *rpcServer) dispatchPaymentIntent(ctx context.Context,
	payIntent *rpcPaymentIntent) (*paymentIntentResponse, error) {

	// Construct a payment request to send to the channel router. If the
	// payment is successful, the route chosen will be returned. Otherwise,
	// we'll get a non-nil error.
	var (
		preImage  [32]byte
		route     *route.Route
		routerErr error
	)

	// If a route was specified, then we'll pass the route directly to the
	// router, otherwise we'll create a payment session to execute it.
	if payIntent.route == nil {
		payment := &routing.LightningPayment{
			Target:             payIntent.dest,
			Amount:             payIntent.msat,
			FinalCLTVDelta:     payIntent.cltvDelta,
			FeeLimit:           payIntent.feeLimit,
			CltvLimit:          payIntent.cltvLimit,
			RouteHints:         payIntent.routeHints,
			OutgoingChannelIDs: payIntent.outgoingChannelIDs,
			LastHop:            payIntent.lastHop,
			PaymentRequest:     payIntent.payReq,
			PayAttemptTimeout:  routing.DefaultPayAttemptTimeout,
			DestCustomRecords:  payIntent.destCustomRecords,
			DestFeatures:       payIntent.destFeatures,
			PaymentAddr:        payIntent.paymentAddr,
			Metadata:           payIntent.metadata,
			BlindedPathSet:     payIntent.blindedPathSet,

			// Don't enable multi-part payments on the main rpc.
			// Users need to use routerrpc for that.
			MaxParts: 1,
		}
		err := payment.SetPaymentHash(payIntent.rHash)
		if err != nil {
			return nil, err
		}

		preImage, route, routerErr = r.server.chanRouter.SendPayment(
			ctx, payment,
		)
	} else {
		var attempt *paymentsdb.HTLCAttempt
		attempt, routerErr = r.server.chanRouter.SendToRoute(
			ctx, payIntent.rHash, payIntent.route, nil,
		)

		if routerErr == nil {
			preImage = attempt.Settle.Preimage
		}

		route = payIntent.route
	}

	// If the route failed, then we'll return a nil save err, but a non-nil
	// routing err.
	if routerErr != nil {
		rpcsLog.Warnf("Unable to send payment: %v", routerErr)

		return &paymentIntentResponse{
			Err: routerErr,
		}, nil
	}

	return &paymentIntentResponse{
		Route:    route,
		Preimage: preImage,
	}, nil
}

// sendPayment takes a paymentStream (a source of pre-built routes or payment
// requests) and continually attempt to dispatch payment requests written to
// the write end of the stream. Responses will also be streamed back to the
// client via the write end of the stream. This method is by both SendToRoute
// and SendPayment as the logic is virtually identical.
func (r *rpcServer) sendPayment(stream *paymentStream) error {
	payChan := make(chan *rpcPaymentIntent)
	errChan := make(chan error, 1)

	// We don't allow payments to be sent while the daemon itself is still
	// syncing as we may be trying to sent a payment over a "stale"
	// channel.
	if !r.server.Started() {
		return ErrServerNotActive
	}

	// TODO(roasbeef): check payment filter to see if already used?

	// In order to limit the level of concurrency and prevent a client from
	// attempting to OOM the server, we'll set up a semaphore to create an
	// upper ceiling on the number of outstanding payments.
	const numOutstandingPayments = 2000
	htlcSema := make(chan struct{}, numOutstandingPayments)
	for i := 0; i < numOutstandingPayments; i++ {
		htlcSema <- struct{}{}
	}

	// We keep track of the running goroutines and set up a quit signal we
	// can use to request them to exit if the method returns because of an
	// encountered error.
	var wg sync.WaitGroup
	reqQuit := make(chan struct{})
	defer close(reqQuit)

	// Launch a new goroutine to handle reading new payment requests from
	// the client. This way we can handle errors independently of blocking
	// and waiting for the next payment request to come through.
	// TODO(joostjager): Callers expect result to come in in the same order
	// as the request were sent, but this is far from guarantueed in the
	// code below.
	wg.Add(1)
	go func() {
		defer wg.Done()

		for {
			select {
			case <-reqQuit:
				return

			default:
				// Receive the next pending payment within the
				// stream sent by the client. If we read the
				// EOF sentinel, then the client has closed the
				// stream, and we can exit normally.
				nextPayment, err := stream.recv()
				if err == io.EOF {
					close(payChan)
					return
				} else if err != nil {
					rpcsLog.Errorf("Failed receiving from "+
						"stream: %v", err)

					select {
					case errChan <- err:
					default:
					}
					return
				}

				// Populate the next payment, either from the
				// payment request, or from the explicitly set
				// fields. If the payment proto wasn't well
				// formed, then we'll send an error reply and
				// wait for the next payment.
				payIntent, err := r.extractPaymentIntent(
					nextPayment,
				)
				if err != nil {
					if err := stream.send(&lnrpc.SendResponse{
						PaymentError: err.Error(),
						PaymentHash:  payIntent.rHash[:],
					}); err != nil {
						rpcsLog.Errorf("Failed "+
							"sending on "+
							"stream: %v", err)

						select {
						case errChan <- err:
						default:
						}
						return
					}
					continue
				}

				// If the payment was well formed, then we'll
				// send to the dispatch goroutine, or exit,
				// which ever comes first.
				select {
				case payChan <- &payIntent:
				case <-reqQuit:
					return
				}
			}
		}
	}()

sendLoop:
	for {
		select {

		// If we encounter and error either during sending or
		// receiving, we return directly, closing the stream.
		case err := <-errChan:
			return err

		case <-r.quit:
			return errors.New("rpc server shutting down")

		case payIntent, ok := <-payChan:
			// If the receive loop is done, we break the send loop
			// and wait for the ongoing payments to finish before
			// exiting.
			if !ok {
				break sendLoop
			}

			// We launch a new goroutine to execute the current
			// payment so we can continue to serve requests while
			// this payment is being dispatched.
			wg.Add(1)
			go func(payIntent *rpcPaymentIntent) {
				defer wg.Done()

				// Attempt to grab a free semaphore slot, using
				// a defer to eventually release the slot
				// regardless of payment success.
				select {
				case <-htlcSema:
				case <-reqQuit:
					return
				}
				defer func() {
					htlcSema <- struct{}{}
				}()

				resp, saveErr := r.dispatchPaymentIntent(
					stream.getCtx(), payIntent,
				)

				switch {
				// If we were unable to save the state of the
				// payment, then we'll return the error to the
				// user, and terminate.
				case saveErr != nil:
					rpcsLog.Errorf("Failed dispatching "+
						"payment intent: %v", saveErr)

					select {
					case errChan <- saveErr:
					default:
					}
					return

				// If we receive payment error than, instead of
				// terminating the stream, send error response
				// to the user.
				case resp.Err != nil:
					err := stream.send(&lnrpc.SendResponse{
						PaymentError: resp.Err.Error(),
						PaymentHash:  payIntent.rHash[:],
					})
					if err != nil {
						rpcsLog.Errorf("Failed "+
							"sending error "+
							"response: %v", err)

						select {
						case errChan <- err:
						default:
						}
					}
					return
				}

				backend := r.routerBackend
				marshalledRouted, err := backend.MarshallRoute(
					resp.Route,
				)
				if err != nil {
					errChan <- err
					return
				}

				err = stream.send(&lnrpc.SendResponse{
					PaymentHash:     payIntent.rHash[:],
					PaymentPreimage: resp.Preimage[:],
					PaymentRoute:    marshalledRouted,
				})
				if err != nil {
					rpcsLog.Errorf("Failed sending "+
						"response: %v", err)

					select {
					case errChan <- err:
					default:
					}
					return
				}
			}(payIntent)
		}
	}

	// Wait for all goroutines to finish before closing the stream.
	wg.Wait()
	return nil
}

// SendPaymentSync is the synchronous non-streaming version of SendPayment.
// This RPC is intended to be consumed by clients of the REST proxy.
// Additionally, this RPC expects the destination's public key and the payment
// hash (if any) to be encoded as hex strings.
func (r *rpcServer) SendPaymentSync(ctx context.Context,
	nextPayment *lnrpc.SendRequest) (*lnrpc.SendResponse, error) {

	return r.sendPaymentSync(ctx, &rpcPaymentRequest{
		SendRequest: nextPayment,
	})
}

// SendToRouteSync is the synchronous non-streaming version of SendToRoute.
// This RPC is intended to be consumed by clients of the REST proxy.
// Additionally, this RPC expects the payment hash (if any) to be encoded as
// hex strings.
func (r *rpcServer) SendToRouteSync(ctx context.Context,
	req *lnrpc.SendToRouteRequest) (*lnrpc.SendResponse, error) {

	if req.Route == nil {
		return nil, fmt.Errorf("unable to send, no routes provided")
	}

	paymentRequest, err := r.unmarshallSendToRouteRequest(req)
	if err != nil {
		return nil, err
	}

	return r.sendPaymentSync(ctx, paymentRequest)
}

// sendPaymentSync is the synchronous variant of sendPayment. It will block and
// wait until the payment has been fully completed.
func (r *rpcServer) sendPaymentSync(ctx context.Context,
	nextPayment *rpcPaymentRequest) (*lnrpc.SendResponse, error) {

	// We don't allow payments to be sent while the daemon itself is still
	// syncing as we may be trying to sent a payment over a "stale"
	// channel.
	if !r.server.Started() {
		return nil, ErrServerNotActive
	}

	// First we'll attempt to map the proto describing the next payment to
	// an intent that we can pass to local sub-systems.
	payIntent, err := r.extractPaymentIntent(nextPayment)
	if err != nil {
		return nil, err
	}

	// With the payment validated, we'll now attempt to dispatch the
	// payment.
	resp, saveErr := r.dispatchPaymentIntent(ctx, &payIntent)
	switch {
	case saveErr != nil:
		return nil, saveErr

	case resp.Err != nil:
		return &lnrpc.SendResponse{
			PaymentError: resp.Err.Error(),
			PaymentHash:  payIntent.rHash[:],
		}, nil
	}

	rpcRoute, err := r.routerBackend.MarshallRoute(resp.Route)
	if err != nil {
		return nil, err
	}

	return &lnrpc.SendResponse{
		PaymentHash:     payIntent.rHash[:],
		PaymentPreimage: resp.Preimage[:],
		PaymentRoute:    rpcRoute,
	}, nil
}

// AddInvoice attempts to add a new invoice to the invoice database. Any
// duplicated invoices are rejected, therefore all invoices *must* have a
// unique payment preimage.
func (r *rpcServer) AddInvoice(ctx context.Context,
	invoice *lnrpc.Invoice) (*lnrpc.AddInvoiceResponse, error) {

	var (
		defaultDelta = r.cfg.Bitcoin.TimeLockDelta
		blindCfg     = invoice.BlindedPathConfig
		blind        = invoice.IsBlinded
	)

	globalBlindCfg := r.server.cfg.Routing.BlindedPaths
	blindingRestrictions := &routing.BlindedPathRestrictions{
		MinDistanceFromIntroNode: globalBlindCfg.MinNumRealHops,
		NumHops:                  globalBlindCfg.NumHops,
		MaxNumPaths:              globalBlindCfg.MaxNumPaths,
		NodeOmissionSet:          fn.NewSet[route.Vertex](),
	}

	if blindCfg != nil && !blind {
		return nil, fmt.Errorf("blinded path config provided but " +
			"IsBlinded not set")
	}

	if blind && blindCfg != nil {
		if blindCfg.MinNumRealHops != nil {
			blindingRestrictions.MinDistanceFromIntroNode =
				uint8(*blindCfg.MinNumRealHops)
		}
		if blindCfg.NumHops != nil {
			blindingRestrictions.NumHops = uint8(*blindCfg.NumHops)
		}
		if blindCfg.MaxNumPaths != nil {
			if *blindCfg.MaxNumPaths == 0 {
				return nil, fmt.Errorf("blinded max num " +
					"paths cannot be 0")
			}
			blindingRestrictions.MaxNumPaths =
				uint8(*blindCfg.MaxNumPaths)
		}

		for _, nodeIDBytes := range blindCfg.NodeOmissionList {
			vertex, err := route.NewVertexFromBytes(nodeIDBytes)
			if err != nil {
				return nil, err
			}

			blindingRestrictions.NodeOmissionSet.Add(vertex)
		}

		blindingRestrictions.IncomingChainedChannels = append(
			blindingRestrictions.IncomingChainedChannels,
			blindCfg.IncomingChannelList...,
		)

		numChainedChannels :=
			uint8(len(blindingRestrictions.IncomingChainedChannels))

		// When selecting the blinded incoming channel list parameter
		// the maximum number of hops is implictitly set.
		if numChainedChannels > blindingRestrictions.NumHops {
			rpcsLog.Warnf("Changing the num_blinded_hops "+
				"from (%d) to (%d)",
				blindingRestrictions.NumHops,
				numChainedChannels)

			blindingRestrictions.NumHops =
				numChainedChannels
		}

		// The MinDistanceFromIntroNode must be greater than or equal to
		// the number of hops specified on the chained channels.
		minNumHops := blindingRestrictions.MinDistanceFromIntroNode
		if minNumHops < numChainedChannels {
			// Ensure MinimumPath is at least the size of the
			// chained path to avoid shorter routes being returned
			// by the pathfinder.
			return nil, fmt.Errorf("minimum number of blinded "+
				"path hops (%d) must be greater than or equal "+
				"to the number of hops specified on the "+
				"chained channels (%d)", minNumHops,
				numChainedChannels)
		}

	}

	if blindingRestrictions.MinDistanceFromIntroNode >
		blindingRestrictions.NumHops {

		return nil, fmt.Errorf("the minimum number of real " +
			"hops in a blinded path must be smaller than " +
			"or equal to the number of hops expected to " +
			"be included in each path")
	}

	addInvoiceCfg := &invoicesrpc.AddInvoiceConfig{
		AddInvoice:        r.server.invoices.AddInvoice,
		IsChannelActive:   r.server.htlcSwitch.HasActiveLink,
		ChainParams:       r.cfg.ActiveNetParams.Params,
		NodeSigner:        r.server.nodeSigner,
		DefaultCLTVExpiry: defaultDelta,
		ChanDB:            r.server.chanStateDB,
		Graph:             r.server.graphDB,
		GenInvoiceFeatures: func() *lnwire.FeatureVector {
			v := r.server.featureMgr.Get(feature.SetInvoice)

			if blind {
				// If an invoice includes blinded paths, then a
				// payment address is not required since we use
				// the PathID in the final hop's encrypted data
				// as equivalent to the payment address
				v.Unset(lnwire.PaymentAddrRequired)
				v.Set(lnwire.PaymentAddrOptional)

				// The invoice payer will also need to
				// understand the new BOLT 11 tagged field
				// containing the blinded path, so we switch
				// the bit to required.
				v = feature.SetBit(
					v, lnwire.Bolt11BlindedPathsRequired,
				)
			}

			return v
		},
		GenAmpInvoiceFeatures: func() *lnwire.FeatureVector {
			return r.server.featureMgr.Get(feature.SetInvoiceAmp)
		},
		GetAlias:   r.server.aliasMgr.GetPeerAlias,
		BestHeight: r.server.cc.BestBlockTracker.BestHeight,
		QueryBlindedRoutes: func(amt lnwire.MilliSatoshi) (
			[]*route.Route, error) {

			return r.server.chanRouter.FindBlindedPaths(
				r.selfNode, amt,
				r.server.defaultMC.GetProbability,
				blindingRestrictions,
			)
		},
	}

	value, err := lnrpc.UnmarshallAmt(invoice.Value, invoice.ValueMsat)
	if err != nil {
		return nil, err
	}

	// Convert the passed routing hints to the required format.
	routeHints, err := invoicesrpc.CreateZpay32HopHints(invoice.RouteHints)
	if err != nil {
		return nil, err
	}

	var blindedPathCfg *invoicesrpc.BlindedPathConfig
	if blind {
		bpConfig := r.server.cfg.Routing.BlindedPaths

		blindedPathCfg = &invoicesrpc.BlindedPathConfig{
			RoutePolicyIncrMultiplier: bpConfig.
				PolicyIncreaseMultiplier,
			RoutePolicyDecrMultiplier: bpConfig.
				PolicyDecreaseMultiplier,
			DefaultDummyHopPolicy: &blindedpath.BlindedHopPolicy{
				CLTVExpiryDelta: uint16(defaultDelta),
				FeeRate: uint32(
					r.server.cfg.Bitcoin.FeeRate,
				),
				BaseFee:     r.server.cfg.Bitcoin.BaseFee,
				MinHTLCMsat: r.server.cfg.Bitcoin.MinHTLCIn,

				// MaxHTLCMsat will be calculated on the fly by
				// using the introduction node's channel's
				// capacities.
				MaxHTLCMsat: 0,
			},
			MinNumPathHops: blindingRestrictions.NumHops,
		}
	}

	addInvoiceData := &invoicesrpc.AddInvoiceData{
		Memo:            invoice.Memo,
		Value:           value,
		DescriptionHash: invoice.DescriptionHash,
		Expiry:          invoice.Expiry,
		FallbackAddr:    invoice.FallbackAddr,
		CltvExpiry:      invoice.CltvExpiry,
		Private:         invoice.Private,
		RouteHints:      routeHints,
		Amp:             invoice.IsAmp,
		BlindedPathCfg:  blindedPathCfg,
	}

	if invoice.RPreimage != nil {
		preimage, err := lntypes.MakePreimage(invoice.RPreimage)
		if err != nil {
			return nil, err
		}
		addInvoiceData.Preimage = &preimage
	}

	hash, dbInvoice, err := invoicesrpc.AddInvoice(
		ctx, addInvoiceCfg, addInvoiceData,
	)
	if err != nil {
		return nil, err
	}

	return &lnrpc.AddInvoiceResponse{
		AddIndex:       dbInvoice.AddIndex,
		PaymentRequest: string(dbInvoice.PaymentRequest),
		RHash:          hash[:],
		PaymentAddr:    dbInvoice.Terms.PaymentAddr[:],
	}, nil
}

// LookupInvoice attempts to look up an invoice according to its payment hash.
// The passed payment hash *must* be exactly 32 bytes, if not an error is
// returned.
func (r *rpcServer) LookupInvoice(ctx context.Context,
	req *lnrpc.PaymentHash) (*lnrpc.Invoice, error) {

	var (
		payHash [32]byte
		rHash   []byte
		err     error
	)

	// If the RHash as a raw string was provided, then decode that and use
	// that directly. Otherwise, we use the raw bytes provided.
	if req.RHashStr != "" {
		rHash, err = hex.DecodeString(req.RHashStr)
		if err != nil {
			return nil, err
		}
	} else {
		rHash = req.RHash
	}

	// Ensure that the payment hash is *exactly* 32-bytes.
	if len(rHash) != 0 && len(rHash) != 32 {
		return nil, fmt.Errorf("payment hash must be exactly "+
			"32 bytes, is instead %v", len(rHash))
	}
	copy(payHash[:], rHash)

	rpcsLog.Tracef("[lookupinvoice] searching for invoice %x", payHash[:])

	invoice, err := r.server.invoices.LookupInvoice(ctx, payHash)
	switch {
	case errors.Is(err, invoices.ErrInvoiceNotFound) ||
		errors.Is(err, invoices.ErrNoInvoicesCreated):

		return nil, status.Error(codes.NotFound, err.Error())
	case err != nil:
		return nil, err
	}

	rpcsLog.Tracef("[lookupinvoice] located invoice %v",
		lnutils.SpewLogClosure(invoice))

	rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
		&invoice, r.cfg.ActiveNetParams.Params,
	)
	if err != nil {
		return nil, err
	}

	// Give the aux data parser a chance to format the custom data in the
	// invoice HTLCs.
	err = fn.MapOptionZ(
		r.server.implCfg.AuxDataParser,
		func(parser AuxDataParser) error {
			return parser.InlineParseCustomData(rpcInvoice)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing custom data: %w",
			err)
	}

	return rpcInvoice, nil
}

// ListInvoices returns a list of all the invoices currently stored within the
// database. Any active debug invoices are ignored.
func (r *rpcServer) ListInvoices(ctx context.Context,
	req *lnrpc.ListInvoiceRequest) (*lnrpc.ListInvoiceResponse, error) {

	// If the number of invoices was not specified, then we'll default to
	// returning the latest 100 invoices.
	if req.NumMaxInvoices == 0 {
		req.NumMaxInvoices = 100
	}

	// If both dates are set, we check that the start date is less than the
	// end date, otherwise we'll get an empty result.
	if req.CreationDateStart != 0 && req.CreationDateEnd != 0 {
		if req.CreationDateStart >= req.CreationDateEnd {
			return nil, fmt.Errorf("start date(%v) must be before "+
				"end date(%v)", req.CreationDateStart,
				req.CreationDateEnd)
		}
	}

	// Next, we'll map the proto request into a format that is understood by
	// the database.
	q := invoices.InvoiceQuery{
		IndexOffset:       req.IndexOffset,
		NumMaxInvoices:    req.NumMaxInvoices,
		PendingOnly:       req.PendingOnly,
		Reversed:          req.Reversed,
		CreationDateStart: int64(req.CreationDateStart),
		CreationDateEnd:   int64(req.CreationDateEnd),
	}

	invoiceSlice, err := r.server.invoicesDB.QueryInvoices(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("unable to query invoices: %w", err)
	}

	// Before returning the response, we'll need to convert each invoice
	// into it's proto representation.
	resp := &lnrpc.ListInvoiceResponse{
		Invoices:         make([]*lnrpc.Invoice, len(invoiceSlice.Invoices)),
		FirstIndexOffset: invoiceSlice.FirstIndexOffset,
		LastIndexOffset:  invoiceSlice.LastIndexOffset,
	}
	for i, invoice := range invoiceSlice.Invoices {
		invoice := invoice
		resp.Invoices[i], err = invoicesrpc.CreateRPCInvoice(
			&invoice, r.cfg.ActiveNetParams.Params,
		)
		if err != nil {
			return nil, err
		}

		// Give the aux data parser a chance to format the custom data
		// in the invoice HTLCs.
		err = fn.MapOptionZ(
			r.server.implCfg.AuxDataParser,
			func(parser AuxDataParser) error {
				return parser.InlineParseCustomData(
					resp.Invoices[i],
				)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("error parsing custom data: %w",
				err)
		}
	}

	return resp, nil
}

// SubscribeInvoices returns a uni-directional stream (server -> client) for
// notifying the client of newly added/settled invoices.
func (r *rpcServer) SubscribeInvoices(req *lnrpc.InvoiceSubscription,
	updateStream lnrpc.Lightning_SubscribeInvoicesServer) error {

	invoiceClient, err := r.server.invoices.SubscribeNotifications(
		updateStream.Context(), req.AddIndex, req.SettleIndex,
	)
	if err != nil {
		return err
	}
	defer invoiceClient.Cancel()

	for {
		select {
		case newInvoice := <-invoiceClient.NewInvoices:
			rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
				newInvoice, r.cfg.ActiveNetParams.Params,
			)
			if err != nil {
				return err
			}

			// Give the aux data parser a chance to format the
			// custom data in the invoice HTLCs.
			err = fn.MapOptionZ(
				r.server.implCfg.AuxDataParser,
				func(parser AuxDataParser) error {
					return parser.InlineParseCustomData(
						rpcInvoice,
					)
				},
			)
			if err != nil {
				return fmt.Errorf("error parsing custom data: "+
					"%w", err)
			}

			if err := updateStream.Send(rpcInvoice); err != nil {
				return err
			}

		case settledInvoice := <-invoiceClient.SettledInvoices:
			rpcInvoice, err := invoicesrpc.CreateRPCInvoice(
				settledInvoice, r.cfg.ActiveNetParams.Params,
			)
			if err != nil {
				return err
			}

			// Give the aux data parser a chance to format the
			// custom data in the invoice HTLCs.
			err = fn.MapOptionZ(
				r.server.implCfg.AuxDataParser,
				func(parser AuxDataParser) error {
					return parser.InlineParseCustomData(
						rpcInvoice,
					)
				},
			)
			if err != nil {
				return fmt.Errorf("error parsing custom data: "+
					"%w", err)
			}

			if err := updateStream.Send(rpcInvoice); err != nil {
				return err
			}

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-updateStream.Context().Done():
			if errors.Is(updateStream.Context().Err(), context.Canceled) {
				return nil
			}
			return updateStream.Context().Err()

		case <-r.quit:
			return nil
		}
	}
}

// SubscribeTransactions creates a uni-directional stream (server -> client) in
// which any newly discovered transactions relevant to the wallet are sent
// over.
func (r *rpcServer) SubscribeTransactions(req *lnrpc.GetTransactionsRequest,
	updateStream lnrpc.Lightning_SubscribeTransactionsServer) error {

	txClient, err := r.server.cc.Wallet.SubscribeTransactions()
	if err != nil {
		return err
	}
	defer txClient.Cancel()
	rpcsLog.Infof("New transaction subscription")

	for {
		select {
		case tx := <-txClient.ConfirmedTransactions():
			detail := lnrpc.RPCTransaction(tx)
			if err := updateStream.Send(detail); err != nil {
				return err
			}

		case tx := <-txClient.UnconfirmedTransactions():
			detail := lnrpc.RPCTransaction(tx)
			if err := updateStream.Send(detail); err != nil {
				return err
			}

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-updateStream.Context().Done():
			rpcsLog.Infof("Canceling transaction subscription")
			if errors.Is(updateStream.Context().Err(), context.Canceled) {
				return nil
			}
			return updateStream.Context().Err()

		case <-r.quit:
			return nil
		}
	}
}

// GetTransactions returns a list of describing all the known transactions
// relevant to the wallet.
func (r *rpcServer) GetTransactions(ctx context.Context,
	req *lnrpc.GetTransactionsRequest) (*lnrpc.TransactionDetails, error) {

	// To remain backwards compatible with the old api, default to the
	// special case end height which will return transactions from the start
	// height until the chain tip, including unconfirmed transactions.
	var endHeight = btcwallet.UnconfirmedHeight

	// If the user has provided an end height, we overwrite our default.
	if req.EndHeight != 0 {
		endHeight = req.EndHeight
	}

	txns, firstIdx, lastIdx, err :=
		r.server.cc.Wallet.ListTransactionDetails(
			req.StartHeight, endHeight, req.Account,
			req.IndexOffset, req.MaxTransactions,
		)
	if err != nil {
		return nil, err
	}

	return lnrpc.RPCTransactionDetails(txns, firstIdx, lastIdx), nil
}

// DescribeGraph returns a description of the latest graph state from the PoV
// of the node. The graph information is partitioned into two components: all
// the nodes/vertexes, and all the edges that connect the vertexes themselves.
// As this is a directed graph, the edges also contain the node directional
// specific routing policy which includes: the time lock delta, fee
// information, etc.
func (r *rpcServer) DescribeGraph(ctx context.Context,
	req *lnrpc.ChannelGraphRequest) (*lnrpc.ChannelGraph, error) {

	resp := &lnrpc.ChannelGraph{}
	includeUnannounced := req.IncludeUnannounced

	// Check to see if the cache is already populated, if so then we can
	// just return it directly.
	//
	// TODO(roasbeef): move this to an interceptor level feature?
	graphCacheActive := r.cfg.Caches.RPCGraphCacheDuration != 0
	if graphCacheActive {
		r.graphCache.Lock()
		defer r.graphCache.Unlock()

		if r.describeGraphResp != nil {
			return r.describeGraphResp, nil
		}
	}

	// Obtain the pointer to the global singleton channel graph, this will
	// provide a consistent view of the graph due to bolt db's
	// transactional model.
	graph := r.server.graphDB

	// First iterate through all the known nodes (connected or unconnected
	// within the graph), collating their current state into the RPC
	// response.
	err := graph.ForEachNode(ctx, func(node *models.Node) error {
		lnNode := marshalNode(node)

		resp.Nodes = append(resp.Nodes, lnNode)

		return nil
	}, func() {
		resp.Nodes = nil
	})
	if err != nil {
		return nil, err
	}

	// Next, for each active channel we know of within the graph, create a
	// similar response which details both the edge information as well as
	// the routing policies of th nodes connecting the two edges.
	err = graph.ForEachChannel(ctx, func(edgeInfo *models.ChannelEdgeInfo,
		c1, c2 *models.ChannelEdgePolicy) error {

		// Do not include unannounced channels unless specifically
		// requested. Unannounced channels include both private channels as
		// well as public channels whose authentication proof were not
		// confirmed yet, hence were not announced.
		if !includeUnannounced && edgeInfo.AuthProof == nil {
			return nil
		}

		edge := marshalDBEdge(edgeInfo, c1, c2, req.IncludeAuthProof)
		resp.Edges = append(resp.Edges, edge)

		return nil
	}, func() {
		resp.Edges = nil
	})
	if err != nil && !errors.Is(err, graphdb.ErrGraphNoEdgesFound) {
		return nil, err
	}

	// We still have the mutex held, so we can safely populate the cache
	// now to save on GC churn for this query, but only if the cache isn't
	// disabled.
	if graphCacheActive {
		r.describeGraphResp = resp
	}

	return resp, nil
}

// marshalExtraOpaqueData marshals the given tlv data. If the tlv stream is
// malformed or empty, an empty map is returned. This makes the method safe to
// use on unvalidated data.
func marshalExtraOpaqueData(data []byte) map[uint64][]byte {
	r := bytes.NewReader(data)

	tlvStream, err := tlv.NewStream()
	if err != nil {
		return nil
	}

	// Since ExtraOpaqueData is provided by a potentially malicious peer,
	// pass it into the P2P decoding variant.
	parsedTypes, err := tlvStream.DecodeWithParsedTypesP2P(r)
	if err != nil || len(parsedTypes) == 0 {
		return nil
	}

	records := make(map[uint64][]byte)
	for k, v := range parsedTypes {
		records[uint64(k)] = v
	}

	return records
}

func marshalDBEdge(edgeInfo *models.ChannelEdgeInfo,
	c1, c2 *models.ChannelEdgePolicy,
	includeAuthProof bool) *lnrpc.ChannelEdge {

	// Make sure the policies match the node they belong to. c1 should point
	// to the policy for NodeKey1, and c2 for NodeKey2.
	if c1 != nil && c1.ChannelFlags&lnwire.ChanUpdateDirection == 1 ||
		c2 != nil && c2.ChannelFlags&lnwire.ChanUpdateDirection == 0 {

		c2, c1 = c1, c2
	}

	var lastUpdate int64
	if c1 != nil {
		lastUpdate = c1.LastUpdate.Unix()
	}
	if c2 != nil && c2.LastUpdate.Unix() > lastUpdate {
		lastUpdate = c2.LastUpdate.Unix()
	}

	customRecords := marshalExtraOpaqueData(edgeInfo.ExtraOpaqueData)

	edge := &lnrpc.ChannelEdge{
		ChannelId: edgeInfo.ChannelID,
		ChanPoint: edgeInfo.ChannelPoint.String(),
		// TODO(roasbeef): update should be on edge info itself
		LastUpdate:    uint32(lastUpdate),
		Node1Pub:      hex.EncodeToString(edgeInfo.NodeKey1Bytes[:]),
		Node2Pub:      hex.EncodeToString(edgeInfo.NodeKey2Bytes[:]),
		Capacity:      int64(edgeInfo.Capacity),
		CustomRecords: customRecords,
	}

	if c1 != nil {
		edge.Node1Policy = marshalDBRoutingPolicy(c1)
	}

	if c2 != nil {
		edge.Node2Policy = marshalDBRoutingPolicy(c2)
	}

	// We do not expect to have an AuthProof for private channels and for
	// our own public channels for the time between channel funding and
	// channel announcement.
	if includeAuthProof && edgeInfo.AuthProof != nil {
		edge.AuthProof = &lnrpc.ChannelAuthProof{
			NodeSig1:    edgeInfo.AuthProof.NodeSig1Bytes,
			BitcoinSig1: edgeInfo.AuthProof.BitcoinSig1Bytes,
			NodeSig2:    edgeInfo.AuthProof.NodeSig2Bytes,
			BitcoinSig2: edgeInfo.AuthProof.BitcoinSig2Bytes,
		}
	}

	return edge
}

// marshalPolicyExtraOpaqueData marshals the given tlv data and filters out
// inbound fee record.
func marshalPolicyExtraOpaqueData(data []byte) map[uint64][]byte {
	records := marshalExtraOpaqueData(data)

	// Remove the inbound fee record as we have dedicated fields for it.
	delete(records, uint64(lnwire.FeeRecordType))

	return records
}

func marshalDBRoutingPolicy(
	policy *models.ChannelEdgePolicy) *lnrpc.RoutingPolicy {

	disabled := policy.ChannelFlags&lnwire.ChanUpdateDisabled != 0

	customRecords := marshalPolicyExtraOpaqueData(policy.ExtraOpaqueData)
	inboundFee := policy.InboundFee.UnwrapOr(lnwire.Fee{})

	return &lnrpc.RoutingPolicy{
		TimeLockDelta:    uint32(policy.TimeLockDelta),
		MinHtlc:          int64(policy.MinHTLC),
		MaxHtlcMsat:      uint64(policy.MaxHTLC),
		FeeBaseMsat:      int64(policy.FeeBaseMSat),
		FeeRateMilliMsat: int64(policy.FeeProportionalMillionths),
		Disabled:         disabled,
		LastUpdate:       uint32(policy.LastUpdate.Unix()),
		CustomRecords:    customRecords,

		InboundFeeBaseMsat:      inboundFee.BaseFee,
		InboundFeeRateMilliMsat: inboundFee.FeeRate,
	}
}

// GetNodeMetrics returns all available node metrics calculated from the
// current channel graph.
func (r *rpcServer) GetNodeMetrics(ctx context.Context,
	req *lnrpc.NodeMetricsRequest) (*lnrpc.NodeMetricsResponse, error) {

	// Get requested metric types.
	getCentrality := false
	for _, t := range req.Types {
		if t == lnrpc.NodeMetricType_BETWEENNESS_CENTRALITY {
			getCentrality = true
		}
	}

	// Only centrality can be requested for now.
	if !getCentrality {
		return nil, nil
	}

	resp := &lnrpc.NodeMetricsResponse{
		BetweennessCentrality: make(map[string]*lnrpc.FloatMetric),
	}

	// Obtain the pointer to the global singleton channel graph, this will
	// provide a consistent view of the graph due to bolt db's
	// transactional model.
	graph := r.server.graphDB

	// Calculate betweenness centrality if requested. Note that depending on the
	// graph size, this may take up to a few minutes.
	channelGraph := autopilot.ChannelGraphFromDatabase(graph)
	centralityMetric, err := autopilot.NewBetweennessCentralityMetric(
		runtime.NumCPU(),
	)
	if err != nil {
		return nil, err
	}
	if err := centralityMetric.Refresh(ctx, channelGraph); err != nil {
		return nil, err
	}

	// Fill normalized and non normalized centrality.
	centrality := centralityMetric.GetMetric(true)
	for nodeID, val := range centrality {
		resp.BetweennessCentrality[hex.EncodeToString(nodeID[:])] =
			&lnrpc.FloatMetric{
				NormalizedValue: val,
			}
	}

	centrality = centralityMetric.GetMetric(false)
	for nodeID, val := range centrality {
		resp.BetweennessCentrality[hex.EncodeToString(nodeID[:])].Value = val
	}

	return resp, nil
}

// GetChanInfo returns the latest authenticated network announcement for the
// given channel identified by either its channel ID or a channel outpoint. Both
// uniquely identify the location of transaction's funding output within the
// blockchain. The former is an 8-byte integer, while the latter is a string
// formatted as funding_txid:output_index.
func (r *rpcServer) GetChanInfo(_ context.Context,
	in *lnrpc.ChanInfoRequest) (*lnrpc.ChannelEdge, error) {

	graph := r.server.graphDB

	var (
		edgeInfo     *models.ChannelEdgeInfo
		edge1, edge2 *models.ChannelEdgePolicy
		err          error
	)

	switch {
	case in.ChanId != 0:
		edgeInfo, edge1, edge2, err = graph.FetchChannelEdgesByID(
			in.ChanId,
		)

	case in.ChanPoint != "":
		var chanPoint *wire.OutPoint
		chanPoint, err = wire.NewOutPointFromString(in.ChanPoint)
		if err != nil {
			return nil, err
		}
		edgeInfo, edge1, edge2, err = graph.FetchChannelEdgesByOutpoint(
			chanPoint,
		)

	default:
		return nil, fmt.Errorf("specify either chan_id or chan_point")
	}
	switch {
	case errors.Is(err, graphdb.ErrEdgeNotFound):
		return nil, status.Error(codes.NotFound, err.Error())
	case err != nil:
		return nil, err
	}

	// Convert the database's edge format into the network/RPC edge format
	// which couples the edge itself along with the directional node
	// routing policies of each node involved within the channel.
	channelEdge := marshalDBEdge(
		edgeInfo, edge1, edge2, in.IncludeAuthProof,
	)

	return channelEdge, nil
}

// GetNodeInfo returns the latest advertised and aggregate authenticated
// channel information for the specified node identified by its public key.
func (r *rpcServer) GetNodeInfo(ctx context.Context,
	in *lnrpc.NodeInfoRequest) (*lnrpc.NodeInfo, error) {

	if in.IncludeAuthProof && !in.IncludeChannels {
		return nil, fmt.Errorf("include_auth_proof depends on " +
			"include_channels")
	}

	graph := r.server.graphDB

	// First, parse the hex-encoded public key into a full in-memory public
	// key object we can work with for querying.
	pubKey, err := route.NewVertexFromStr(in.PubKey)
	if err != nil {
		return nil, err
	}

	// With the public key decoded, attempt to fetch the node corresponding
	// to this public key. If the node cannot be found, then an error will
	// be returned.
	node, err := graph.FetchNode(ctx, pubKey)
	switch {
	case errors.Is(err, graphdb.ErrGraphNodeNotFound):
		return nil, status.Error(codes.NotFound, err.Error())
	case err != nil:
		return nil, err
	}

	// With the node obtained, we'll now iterate through all its out going
	// edges to gather some basic statistics about its out going channels.
	var (
		numChannels   uint32
		totalCapacity btcutil.Amount
		channels      []*lnrpc.ChannelEdge
	)

	err = graph.ForEachNodeChannel(
		ctx, node.PubKeyBytes,
		func(edge *models.ChannelEdgeInfo,
			c1, c2 *models.ChannelEdgePolicy) error {

			numChannels++
			totalCapacity += edge.Capacity

			// Only populate the node's channels if the user
			// requested them.
			if in.IncludeChannels {
				// Do not include unannounced channels - private
				// channels or public channels whose
				// authentication proof were not confirmed yet.
				if edge.AuthProof == nil {
					return nil
				}

				// Convert the database's edge format into the
				// network/RPC edge format.
				channelEdge := marshalDBEdge(
					edge, c1, c2, in.IncludeAuthProof,
				)
				channels = append(channels, channelEdge)
			}

			return nil
		}, func() {
			numChannels = 0
			totalCapacity = 0
			channels = nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &lnrpc.NodeInfo{
		Node:          marshalNode(node),
		NumChannels:   numChannels,
		TotalCapacity: int64(totalCapacity),
		Channels:      channels,
	}, nil
}

func marshalNode(node *models.Node) *lnrpc.LightningNode {
	nodeAddrs := make([]*lnrpc.NodeAddress, len(node.Addresses))
	for i, addr := range node.Addresses {
		nodeAddr := &lnrpc.NodeAddress{
			Network: addr.Network(),
			Addr:    addr.String(),
		}
		nodeAddrs[i] = nodeAddr
	}

	features := invoicesrpc.CreateRPCFeatures(node.Features)

	customRecords := marshalExtraOpaqueData(node.ExtraOpaqueData)

	return &lnrpc.LightningNode{
		LastUpdate: uint32(node.LastUpdate.Unix()),
		PubKey:     hex.EncodeToString(node.PubKeyBytes[:]),
		Addresses:  nodeAddrs,
		Alias:      node.Alias.UnwrapOr(""),
		Color: graphdb.EncodeHexColor(
			node.Color.UnwrapOr(color.RGBA{}),
		),
		Features:      features,
		CustomRecords: customRecords,
	}
}

// QueryRoutes attempts to query the daemons' Channel Router for a possible
// route to a target destination capable of carrying a specific amount of
// satoshis within the route's flow. The returned route contains the full
// details required to craft and send an HTLC, also including the necessary
// information that should be present within the Sphinx packet encapsulated
// within the HTLC.
//
// TODO(roasbeef): should return a slice of routes in reality
//   - create separate PR to send based on well formatted route
func (r *rpcServer) QueryRoutes(ctx context.Context,
	in *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse, error) {

	return r.routerBackend.QueryRoutes(ctx, in)
}

// GetNetworkInfo returns some basic stats about the known channel graph from
// the PoV of the node.
func (r *rpcServer) GetNetworkInfo(ctx context.Context,
	_ *lnrpc.NetworkInfoRequest) (*lnrpc.NetworkInfo, error) {

	graph := r.server.graphDB

	var (
		numNodes             uint32
		numChannels          uint32
		maxChanOut           uint32
		totalNetworkCapacity btcutil.Amount
		minChannelSize       btcutil.Amount = math.MaxInt64
		maxChannelSize       btcutil.Amount
		medianChanSize       btcutil.Amount
	)

	// We'll use this map to de-duplicate channels during our traversal.
	// This is needed since channels are directional, so there will be two
	// edges for each channel within the graph.
	seenChans := make(map[uint64]struct{})

	// We also keep a list of all encountered capacities, in order to
	// calculate the median channel size.
	var allChans []btcutil.Amount

	// We'll run through all the known nodes in the within our view of the
	// network, tallying up the total number of nodes, and also gathering
	// each node so we can measure the graph diameter and degree stats
	// below.
	err := graph.ForEachNodeCached(ctx, false, func(ctx context.Context,
		node route.Vertex, _ []net.Addr,
		edges map[uint64]*graphdb.DirectedChannel) error {

		// Increment the total number of nodes with each iteration.
		numNodes++

		// For each channel we'll compute the out degree of each node,
		// and also update our running tallies of the min/max channel
		// capacity, as well as the total channel capacity. We pass
		// through the db transaction from the outer view so we can
		// re-use it within this inner view.
		var outDegree uint32
		for _, edge := range edges {
			// Bump up the out degree for this node for each
			// channel encountered.
			outDegree++

			// If we've already seen this channel, then we'll
			// return early to ensure that we don't double-count
			// stats.
			if _, ok := seenChans[edge.ChannelID]; ok {
				return nil
			}

			// Compare the capacity of this channel against the
			// running min/max to see if we should update the
			// extrema.
			chanCapacity := edge.Capacity
			if chanCapacity < minChannelSize {
				minChannelSize = chanCapacity
			}
			if chanCapacity > maxChannelSize {
				maxChannelSize = chanCapacity
			}

			// Accumulate the total capacity of this channel to the
			// network wide-capacity.
			totalNetworkCapacity += chanCapacity

			numChannels++

			seenChans[edge.ChannelID] = struct{}{}
			allChans = append(allChans, edge.Capacity)
		}

		// Finally, if the out degree of this node is greater than what
		// we've seen so far, update the maxChanOut variable.
		if outDegree > maxChanOut {
			maxChanOut = outDegree
		}

		return nil
	}, func() {
		numChannels = 0
		numNodes = 0
		maxChanOut = 0
		totalNetworkCapacity = 0
		minChannelSize = math.MaxInt64
		maxChannelSize = 0
		clear(allChans)
		clear(seenChans)
	})
	if err != nil {
		return nil, err
	}

	// Query the graph for the current number of zombie channels.
	numZombies, err := graph.NumZombies()
	if err != nil {
		return nil, err
	}

	// Find the median.
	medianChanSize = autopilot.Median(allChans)

	// If we don't have any channels, then reset the minChannelSize to zero
	// to avoid outputting NaN in encoded JSON.
	if numChannels == 0 {
		minChannelSize = 0
	}

	// Graph diameter.
	channelGraph := autopilot.ChannelGraphFromCachedDatabase(graph)
	simpleGraph, err := autopilot.NewSimpleGraph(ctx, channelGraph)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	diameter := simpleGraph.DiameterRadialCutoff()
	rpcsLog.Infof("elapsed time for diameter (%d) calculation: %v", diameter,
		time.Since(start))

	// TODO(roasbeef): also add oldest channel?
	netInfo := &lnrpc.NetworkInfo{
		GraphDiameter:        diameter,
		MaxOutDegree:         maxChanOut,
		AvgOutDegree:         float64(2*numChannels) / float64(numNodes),
		NumNodes:             numNodes,
		NumChannels:          numChannels,
		TotalNetworkCapacity: int64(totalNetworkCapacity),
		AvgChannelSize:       float64(totalNetworkCapacity) / float64(numChannels),

		MinChannelSize:       int64(minChannelSize),
		MaxChannelSize:       int64(maxChannelSize),
		MedianChannelSizeSat: int64(medianChanSize),
		NumZombieChans:       numZombies,
	}

	// Similarly, if we don't have any channels, then we'll also set the
	// average channel size to zero in order to avoid weird JSON encoding
	// outputs.
	if numChannels == 0 {
		netInfo.AvgChannelSize = 0
	}

	return netInfo, nil
}

// StopDaemon will send a shutdown request to the interrupt handler, triggering
// a graceful shutdown of the daemon.
func (r *rpcServer) StopDaemon(_ context.Context,
	_ *lnrpc.StopRequest) (*lnrpc.StopResponse, error) {

	// Before we even consider a shutdown, are we currently in recovery
	// mode? We don't want to allow shutting down during recovery because
	// that would mean the user would have to manually continue the rescan
	// process next time by using `lncli unlock --recovery_window X`
	// otherwise some funds wouldn't be picked up.
	isRecoveryMode, progress, err := r.server.cc.Wallet.GetRecoveryInfo()
	if err != nil {
		return nil, fmt.Errorf("unable to get wallet recovery info: %w",
			err)
	}
	if isRecoveryMode && progress < 1 {
		return nil, fmt.Errorf("wallet recovery in progress, cannot " +
			"shut down, please wait until rescan finishes")
	}

	r.interceptor.RequestShutdown()

	return &lnrpc.StopResponse{
		Status: "shutdown initiated, check logs for progress",
	}, nil
}

// SubscribeChannelGraph launches a streaming RPC that allows the caller to
// receive notifications upon any changes the channel graph topology from the
// review of the responding node. Events notified include: new nodes coming
// online, nodes updating their authenticated attributes, new channels being
// advertised, updates in the routing policy for a directional channel edge,
// and finally when prior channels are closed on-chain.
func (r *rpcServer) SubscribeChannelGraph(req *lnrpc.GraphTopologySubscription,
	updateStream lnrpc.Lightning_SubscribeChannelGraphServer) error {

	// First, we start by subscribing to a new intent to receive
	// notifications from the channel router.
	client, err := r.server.graphDB.SubscribeTopology()
	if err != nil {
		return err
	}

	// Ensure that the resources for the topology update client is cleaned
	// up once either the server, or client exists.
	defer client.Cancel()

	for {
		select {

		// A new update has been sent by the channel router, we'll
		// marshal it into the form expected by the gRPC client, then
		// send it off.
		case topChange, ok := <-client.TopologyChanges:
			// If the second value from the channel read is nil,
			// then this means that the channel router is exiting
			// or the notification client was canceled. So we'll
			// exit early.
			if !ok {
				return errors.New("server shutting down")
			}

			// Convert the struct from the channel router into the
			// form expected by the gRPC service then send it off
			// to the client.
			graphUpdate := marshallTopologyChange(topChange)
			if err := updateStream.Send(graphUpdate); err != nil {
				return err
			}

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline
		// we will return an error.
		case <-updateStream.Context().Done():
			if errors.Is(updateStream.Context().Err(), context.Canceled) {
				return nil
			}
			return updateStream.Context().Err()

		// The server is quitting, so we'll exit immediately. Returning
		// nil will close the clients read end of the stream.
		case <-r.quit:
			return nil
		}
	}
}

// marshallTopologyChange performs a mapping from the topology change struct
// returned by the router to the form of notifications expected by the current
// gRPC service.
func marshallTopologyChange(
	topChange *graphdb.TopologyChange) *lnrpc.GraphTopologyUpdate {

	// encodeKey is a simple helper function that converts a live public
	// key into a hex-encoded version of the compressed serialization for
	// the public key.
	encodeKey := func(k *btcec.PublicKey) string {
		return hex.EncodeToString(k.SerializeCompressed())
	}

	nodeUpdates := make([]*lnrpc.NodeUpdate, len(topChange.NodeUpdates))
	for i, nodeUpdate := range topChange.NodeUpdates {
		nodeAddrs := make(
			[]*lnrpc.NodeAddress, 0, len(nodeUpdate.Addresses),
		)
		for _, addr := range nodeUpdate.Addresses {
			nodeAddr := &lnrpc.NodeAddress{
				Network: addr.Network(),
				Addr:    addr.String(),
			}
			nodeAddrs = append(nodeAddrs, nodeAddr)
		}

		addrs := make([]string, len(nodeUpdate.Addresses))
		for i, addr := range nodeUpdate.Addresses {
			addrs[i] = addr.String()
		}

		nodeUpdates[i] = &lnrpc.NodeUpdate{
			Addresses:     addrs,
			NodeAddresses: nodeAddrs,
			IdentityKey:   encodeKey(nodeUpdate.IdentityKey),
			Alias:         nodeUpdate.Alias,
			Color:         nodeUpdate.Color,
			Features: invoicesrpc.CreateRPCFeatures(
				nodeUpdate.Features,
			),
		}
	}

	channelUpdates := make([]*lnrpc.ChannelEdgeUpdate, len(topChange.ChannelEdgeUpdates))
	for i, channelUpdate := range topChange.ChannelEdgeUpdates {

		customRecords := marshalPolicyExtraOpaqueData(
			channelUpdate.ExtraOpaqueData,
		)
		inboundFee := channelUpdate.InboundFee.UnwrapOr(lnwire.Fee{})

		channelUpdates[i] = &lnrpc.ChannelEdgeUpdate{
			ChanId: channelUpdate.ChanID,
			ChanPoint: &lnrpc.ChannelPoint{
				FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
					FundingTxidBytes: channelUpdate.ChanPoint.Hash[:],
				},
				OutputIndex: channelUpdate.ChanPoint.Index,
			},
			Capacity: int64(channelUpdate.Capacity),
			RoutingPolicy: &lnrpc.RoutingPolicy{
				TimeLockDelta: uint32(
					channelUpdate.TimeLockDelta,
				),
				MinHtlc: int64(
					channelUpdate.MinHTLC,
				),
				MaxHtlcMsat: uint64(
					channelUpdate.MaxHTLC,
				),
				FeeBaseMsat: int64(
					channelUpdate.BaseFee,
				),
				FeeRateMilliMsat: int64(
					channelUpdate.FeeRate,
				),
				Disabled:                channelUpdate.Disabled,
				InboundFeeBaseMsat:      inboundFee.BaseFee,
				InboundFeeRateMilliMsat: inboundFee.FeeRate,
				CustomRecords:           customRecords,
			},
			AdvertisingNode: encodeKey(channelUpdate.AdvertisingNode),
			ConnectingNode:  encodeKey(channelUpdate.ConnectingNode),
		}
	}

	closedChans := make([]*lnrpc.ClosedChannelUpdate, len(topChange.ClosedChannels))
	for i, closedChan := range topChange.ClosedChannels {
		closedChans[i] = &lnrpc.ClosedChannelUpdate{
			ChanId:       closedChan.ChanID,
			Capacity:     int64(closedChan.Capacity),
			ClosedHeight: closedChan.ClosedHeight,
			ChanPoint: &lnrpc.ChannelPoint{
				FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
					FundingTxidBytes: closedChan.ChanPoint.Hash[:],
				},
				OutputIndex: closedChan.ChanPoint.Index,
			},
		}
	}

	return &lnrpc.GraphTopologyUpdate{
		NodeUpdates:    nodeUpdates,
		ChannelUpdates: channelUpdates,
		ClosedChans:    closedChans,
	}
}

// ListPayments returns a list of outgoing payments determined by a paginated
// database query.
func (r *rpcServer) ListPayments(ctx context.Context,
	req *lnrpc.ListPaymentsRequest) (*lnrpc.ListPaymentsResponse, error) {

	// If both dates are set, we check that the start date is less than the
	// end date, otherwise we'll get an empty result.
	if req.CreationDateStart != 0 && req.CreationDateEnd != 0 {
		if req.CreationDateStart >= req.CreationDateEnd {
			return nil, fmt.Errorf("start date(%v) must be before "+
				"end date(%v)", req.CreationDateStart,
				req.CreationDateEnd)
		}
	}

	query := paymentsdb.Query{
		IndexOffset:       req.IndexOffset,
		MaxPayments:       req.MaxPayments,
		Reversed:          req.Reversed,
		IncludeIncomplete: req.IncludeIncomplete,
		CountTotal:        req.CountTotalPayments,
		CreationDateStart: int64(req.CreationDateStart),
		CreationDateEnd:   int64(req.CreationDateEnd),
	}

	// If the maximum number of payments wasn't specified, we default to
	// a reasonable number to prevent resource exhaustion. All of the
	// payments are fetched into memory. Moreover we don't want our daemon
	// to remain stable and do other stuff rather than serving payments.
	//
	// TODO(ziggie): Choose a more specific default value when results of
	// performance testing are available.
	if req.MaxPayments == 0 {
		query.MaxPayments = paymentsdb.DefaultMaxPayments
	}

	paymentsQuerySlice, err := r.server.paymentsDB.QueryPayments(
		ctx, query,
	)
	if err != nil {
		return nil, err
	}

	paymentsResp := &lnrpc.ListPaymentsResponse{
		LastIndexOffset:  paymentsQuerySlice.LastIndexOffset,
		FirstIndexOffset: paymentsQuerySlice.FirstIndexOffset,
		TotalNumPayments: paymentsQuerySlice.TotalCount,
	}

	for _, payment := range paymentsQuerySlice.Payments {
		payment := payment

		rpcPayment, err := r.routerBackend.MarshallPayment(payment)
		if err != nil {
			return nil, err
		}

		paymentsResp.Payments = append(
			paymentsResp.Payments, rpcPayment,
		)
	}

	return paymentsResp, nil
}

// DeleteCanceledInvoice remove a canceled invoice from the database.
func (r *rpcServer) DeleteCanceledInvoice(ctx context.Context,
	req *lnrpc.DelCanceledInvoiceReq) (*lnrpc.DelCanceledInvoiceResp,
	error) {

	if req.InvoiceHash == "" {
		return nil, invoices.ErrNoInvoiceHash
	}

	hash, err := lntypes.MakeHashFromStr(req.InvoiceHash)
	if err != nil {
		return nil, err
	}

	invoice, err := r.server.invoices.LookupInvoice(ctx, hash)
	if err != nil {
		return nil, err
	}

	if invoice.State != invoices.ContractCanceled {
		return nil, invoices.ErrInvoiceNotCanceled
	}

	err = r.server.invoicesDB.DeleteInvoice(ctx,
		[]invoices.InvoiceDeleteRef{
			{
				PayHash:  hash,
				AddIndex: invoice.AddIndex,
			},
		},
	)
	if err != nil {
		return nil, err
	}

	return &lnrpc.DelCanceledInvoiceResp{Status: fmt.Sprintf("canceled "+
		"invoice deleted successfully: invoice hash %v", hash)}, nil
}

// DeletePayment deletes a payment from the DB given its payment hash. If
// failedHtlcsOnly is set, only failed HTLC attempts of the payment will be
// deleted.
func (r *rpcServer) DeletePayment(ctx context.Context,
	req *lnrpc.DeletePaymentRequest) (
	*lnrpc.DeletePaymentResponse, error) {

	hash, err := lntypes.MakeHash(req.PaymentHash)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("[DeletePayment] payment_identifier=%v, "+
		"failed_htlcs_only=%v", hash, req.FailedHtlcsOnly)

	err = r.server.paymentsDB.DeletePayment(ctx, hash, req.FailedHtlcsOnly)
	if err != nil {
		return nil, err
	}

	return &lnrpc.DeletePaymentResponse{
		Status: "payment deleted",
	}, nil
}

// DeleteAllPayments deletes all outgoing payments from DB.
func (r *rpcServer) DeleteAllPayments(ctx context.Context,
	req *lnrpc.DeleteAllPaymentsRequest) (
	*lnrpc.DeleteAllPaymentsResponse, error) {

	switch {
	// Since this is a destructive operation, at least one of the options
	// must be set to true.
	case !req.AllPayments && !req.FailedPaymentsOnly &&
		!req.FailedHtlcsOnly:

		return nil, fmt.Errorf("at least one of the options " +
			"`all_payments`, `failed_payments_only`, or " +
			"`failed_htlcs_only` must be set to true")

	// `all_payments` cannot be true with `failed_payments_only` or
	// `failed_htlcs_only`. `all_payments` includes all records, making
	// these options contradictory.
	case req.AllPayments &&
		(req.FailedPaymentsOnly || req.FailedHtlcsOnly):

		return nil, fmt.Errorf("`all_payments` cannot be set to true " +
			"while either `failed_payments_only` or " +
			"`failed_htlcs_only` is also set to true")
	}

	rpcsLog.Infof("[DeleteAllPayments] failed_payments_only=%v, "+
		"failed_htlcs_only=%v", req.FailedPaymentsOnly,
		req.FailedHtlcsOnly)

	numDeletedPayments, err := r.server.paymentsDB.DeletePayments(
		ctx, req.FailedPaymentsOnly, req.FailedHtlcsOnly,
	)
	if err != nil {
		return nil, err
	}

	return &lnrpc.DeleteAllPaymentsResponse{
		Status: fmt.Sprintf("%v payments deleted, failed_htlcs_only=%v",
			numDeletedPayments, req.FailedHtlcsOnly),
	}, nil
}

// DebugLevel allows a caller to programmatically set the logging verbosity of
// lnd. The logging can be targeted according to a coarse daemon-wide logging
// level, or in a granular fashion to specify the logging for a target
// sub-system.
func (r *rpcServer) DebugLevel(ctx context.Context,
	req *lnrpc.DebugLevelRequest) (*lnrpc.DebugLevelResponse, error) {

	// If show is set, then we simply print out the list of available
	// sub-systems.
	if req.Show {
		return &lnrpc.DebugLevelResponse{
			SubSystems: strings.Join(
				r.cfg.SubLogMgr.SupportedSubsystems(), " ",
			),
		}, nil
	}

	rpcsLog.Infof("[debuglevel] changing debug level to: %v", req.LevelSpec)

	// Otherwise, we'll attempt to set the logging level using the
	// specified level spec.
	err := build.ParseAndSetDebugLevels(req.LevelSpec, r.cfg.SubLogMgr)
	if err != nil {
		return nil, err
	}

	subLoggers := r.cfg.SubLogMgr.SubLoggers()
	// Sort alphabetically by subsystem name.
	var tags []string
	for t := range subLoggers {
		tags = append(tags, t)
	}
	sort.Strings(tags)

	// Create the log levels string.
	var logLevels []string
	for _, t := range tags {
		logLevels = append(logLevels, fmt.Sprintf("%s=%s", t,
			subLoggers[t].Level().String()))
	}
	logLevelsString := strings.Join(logLevels, ", ")

	// Propagate the new config level to the main config struct.
	r.cfg.DebugLevel = logLevelsString

	return &lnrpc.DebugLevelResponse{
		SubSystems: logLevelsString,
	}, nil
}

// DecodePayReq takes an encoded payment request string and attempts to decode
// it, returning a full description of the conditions encoded within the
// payment request.
func (r *rpcServer) DecodePayReq(ctx context.Context,
	req *lnrpc.PayReqString) (*lnrpc.PayReq, error) {

	rpcsLog.Tracef("[decodepayreq] decoding: %v", req.PayReq)

	// Fist we'll attempt to decode the payment request string, if the
	// request is invalid or the checksum doesn't match, then we'll exit
	// here with an error.
	payReq, err := zpay32.Decode(req.PayReq, r.cfg.ActiveNetParams.Params)
	if err != nil {
		return nil, err
	}

	// Let the fields default to empty strings.
	desc := ""
	if payReq.Description != nil {
		desc = *payReq.Description
	}

	descHash := []byte("")
	if payReq.DescriptionHash != nil {
		descHash = payReq.DescriptionHash[:]
	}

	fallbackAddr := ""
	if payReq.FallbackAddr != nil {
		fallbackAddr = payReq.FallbackAddr.String()
	}

	// Expiry time will default to 3600 seconds if not specified
	// explicitly.
	expiry := int64(payReq.Expiry().Seconds())

	// Convert between the `lnrpc` and `routing` types.
	routeHints := invoicesrpc.CreateRPCRouteHints(payReq.RouteHints)

	blindedPaymentPaths, err := invoicesrpc.CreateRPCBlindedPayments(
		payReq.BlindedPaymentPaths,
	)
	if err != nil {
		return nil, err
	}

	var amtSat, amtMsat int64
	if payReq.MilliSat != nil {
		amtSat = int64(payReq.MilliSat.ToSatoshis())
		amtMsat = int64(*payReq.MilliSat)
	}

	// Extract the payment address from the payment request, if present.
	paymentAddr := payReq.PaymentAddr.UnwrapOr([32]byte{})

	dest := payReq.Destination.SerializeCompressed()
	return &lnrpc.PayReq{
		Destination:     hex.EncodeToString(dest),
		PaymentHash:     hex.EncodeToString(payReq.PaymentHash[:]),
		NumSatoshis:     amtSat,
		NumMsat:         amtMsat,
		Timestamp:       payReq.Timestamp.Unix(),
		Description:     desc,
		DescriptionHash: hex.EncodeToString(descHash[:]),
		FallbackAddr:    fallbackAddr,
		Expiry:          expiry,
		CltvExpiry:      int64(payReq.MinFinalCLTVExpiry()),
		RouteHints:      routeHints,
		BlindedPaths:    blindedPaymentPaths,
		PaymentAddr:     paymentAddr[:],
		Features:        invoicesrpc.CreateRPCFeatures(payReq.Features),
	}, nil
}

// feeBase is the fixed point that fee rate computation are performed over.
// Nodes on the network advertise their fee rate using this point as a base.
// This means that the minimal possible fee rate if 1e-6, or 0.000001, or
// 0.0001%.
const feeBase float64 = 1000000

// FeeReport allows the caller to obtain a report detailing the current fee
// schedule enforced by the node globally for each channel.
func (r *rpcServer) FeeReport(ctx context.Context,
	_ *lnrpc.FeeReportRequest) (*lnrpc.FeeReportResponse, error) {

	channelGraph := r.server.graphDB
	selfNode, err := channelGraph.SourceNode(ctx)
	if err != nil {
		return nil, err
	}

	var feeReports []*lnrpc.ChannelFeeReport
	err = channelGraph.ForEachNodeChannel(
		ctx, selfNode.PubKeyBytes,
		func(chanInfo *models.ChannelEdgeInfo,
			edgePolicy, _ *models.ChannelEdgePolicy) error {

			// Self node should always have policies for its
			// channels.
			if edgePolicy == nil {
				return fmt.Errorf("no policy for outgoing "+
					"channel %v ", chanInfo.ChannelID)
			}

			// We'll compute the effective fee rate by converting
			// from a fixed point fee rate to a floating point fee
			// rate. The fee rate field in the database the amount
			// of mSAT charged per 1mil mSAT sent, so will divide by
			// this to get the proper fee rate.
			feeRateFixedPoint :=
				edgePolicy.FeeProportionalMillionths
			feeRate := float64(feeRateFixedPoint) / feeBase

			inboundFee := edgePolicy.InboundFee.UnwrapOr(
				lnwire.Fee{},
			)

			// TODO(roasbeef): also add stats for revenue for each
			// channel
			feeReports = append(feeReports, &lnrpc.ChannelFeeReport{
				ChanId:       chanInfo.ChannelID,
				ChannelPoint: chanInfo.ChannelPoint.String(),
				BaseFeeMsat:  int64(edgePolicy.FeeBaseMSat),
				FeePerMil:    int64(feeRateFixedPoint),
				FeeRate:      feeRate,

				InboundBaseFeeMsat: inboundFee.BaseFee,
				InboundFeePerMil:   inboundFee.FeeRate,
			})

			return nil
		}, func() {
			feeReports = nil
		},
	)
	if err != nil {
		return nil, err
	}

	fwdEventLog := r.server.miscDB.ForwardingLog()

	// computeFeeSum is a helper function that computes the total fees for
	// a particular time slice described by a forwarding event query.
	computeFeeSum := func(query channeldb.ForwardingEventQuery) (lnwire.MilliSatoshi, error) {

		var totalFees lnwire.MilliSatoshi

		// We'll continue to fetch the next query and accumulate the
		// fees until the next query returns no events.
		for {
			timeSlice, err := fwdEventLog.Query(query)
			if err != nil {
				return 0, err
			}

			// If the timeslice is empty, then we'll return as
			// we've retrieved all the entries in this range.
			if len(timeSlice.ForwardingEvents) == 0 {
				break
			}

			// Otherwise, we'll tally up an accumulate the total
			// fees for this time slice.
			for _, event := range timeSlice.ForwardingEvents {
				fee := event.AmtIn - event.AmtOut
				totalFees += fee
			}

			// We'll now take the last offset index returned as
			// part of this response, and modify our query to start
			// at this index. This has a pagination effect in the
			// case that our query bounds has more than 100k
			// entries.
			query.IndexOffset = timeSlice.LastIndexOffset
		}

		return totalFees, nil
	}

	now := time.Now()

	// Before we perform the queries below, we'll instruct the switch to
	// flush any pending events to disk. This ensure we get a complete
	// snapshot at this particular time.
	if err := r.server.htlcSwitch.FlushForwardingEvents(); err != nil {
		return nil, fmt.Errorf("unable to flush forwarding "+
			"events: %v", err)
	}

	// In addition to returning the current fee schedule for each channel.
	// We'll also perform a series of queries to obtain the total fees
	// earned over the past day, week, and month.
	dayQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	dayFees, err := computeFeeSum(dayQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %w", err)
	}

	weekQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24 * 7),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	weekFees, err := computeFeeSum(weekQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %w", err)
	}

	monthQuery := channeldb.ForwardingEventQuery{
		StartTime:    now.Add(-time.Hour * 24 * 30),
		EndTime:      now,
		NumMaxEvents: 1000,
	}
	monthFees, err := computeFeeSum(monthQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve day fees: %w", err)
	}

	return &lnrpc.FeeReportResponse{
		ChannelFees: feeReports,
		DayFeeSum:   uint64(dayFees.ToSatoshis()),
		WeekFeeSum:  uint64(weekFees.ToSatoshis()),
		MonthFeeSum: uint64(monthFees.ToSatoshis()),
	}, nil
}

// minFeeRate is the smallest permitted fee rate within the network. This is
// derived by the fact that fee rates are computed using a fixed point of
// 1,000,000. As a result, the smallest representable fee rate is 1e-6, or
// 0.000001, or 0.0001%.
const minFeeRate = 1e-6

// UpdateChannelPolicy allows the caller to update the channel forwarding policy
// for all channels globally, or a particular channel.
func (r *rpcServer) UpdateChannelPolicy(ctx context.Context,
	req *lnrpc.PolicyUpdateRequest) (*lnrpc.PolicyUpdateResponse, error) {

	var targetChans []wire.OutPoint
	switch scope := req.Scope.(type) {
	// If the request is targeting all active channels, then we don't need
	// target any channels by their channel point.
	case *lnrpc.PolicyUpdateRequest_Global:

	// Otherwise, we're targeting an individual channel by its channel
	// point.
	case *lnrpc.PolicyUpdateRequest_ChanPoint:
		txid, err := lnrpc.GetChanPointFundingTxid(scope.ChanPoint)
		if err != nil {
			return nil, err
		}
		targetChans = append(targetChans, wire.OutPoint{
			Hash:  *txid,
			Index: scope.ChanPoint.OutputIndex,
		})
	default:
		return nil, fmt.Errorf("unknown scope: %v", scope)
	}

	var feeRateFixed uint32

	switch {
	// The request should use either the fee rate in percent, or the new
	// ppm rate, but not both.
	case req.FeeRate != 0 && req.FeeRatePpm != 0:
		errMsg := "cannot set both FeeRate and FeeRatePpm at the " +
			"same time"

		return nil, status.Errorf(codes.InvalidArgument, "%v", errMsg)

	// If the request is using fee_rate.
	case req.FeeRate != 0:
		// As a sanity check, if the fee isn't zero, we'll ensure that
		// the passed fee rate is below 1e-6, or the lowest allowed
		// non-zero fee rate expressible within the protocol.
		if req.FeeRate != 0 && req.FeeRate < minFeeRate {
			return nil, fmt.Errorf("fee rate of %v is too "+
				"small, min fee rate is %v", req.FeeRate,
				minFeeRate)
		}

		// We'll also need to convert the floating point fee rate we
		// accept over RPC to the fixed point rate that we use within
		// the protocol. We do this by multiplying the passed fee rate
		// by the fee base. This gives us the fixed point, scaled by 1
		// million that's used within the protocol.
		//
		// Because of the inaccurate precision of the IEEE 754
		// standard, we need to round the product of feerate and
		// feebase.
		feeRateFixed = uint32(math.Round(req.FeeRate * feeBase))

	// Otherwise, we use the fee_rate_ppm parameter.
	case req.FeeRatePpm != 0:
		feeRateFixed = req.FeeRatePpm
	}

	// We'll also ensure that the user isn't setting a CLTV delta that
	// won't give outgoing HTLCs enough time to fully resolve if needed.
	if req.TimeLockDelta < minTimeLockDelta {
		return nil, fmt.Errorf("time lock delta of %v is too small, "+
			"minimum supported is %v", req.TimeLockDelta,
			minTimeLockDelta)
	} else if req.TimeLockDelta > uint32(MaxTimeLockDelta) {
		return nil, fmt.Errorf("time lock delta of %v is too big, "+
			"maximum supported is %v", req.TimeLockDelta,
			MaxTimeLockDelta)
	}

	// By default, positive inbound fees are rejected.
	if !r.cfg.AcceptPositiveInboundFees && req.InboundFee != nil {
		if req.InboundFee.BaseFeeMsat > 0 {
			return nil, fmt.Errorf("positive values for inbound "+
				"base fee msat are not supported: %v",
				req.InboundFee.BaseFeeMsat)
		}
		if req.InboundFee.FeeRatePpm > 0 {
			return nil, fmt.Errorf("positive values for inbound "+
				"fee rate ppm are not supported: %v",
				req.InboundFee.FeeRatePpm)
		}
	}

	// If no inbound fees have been specified, we indicate with an empty
	// option that the previous inbound fee should be retained during the
	// edge update.
	inboundFee := fn.None[models.InboundFee]()
	if req.InboundFee != nil {
		inboundFee = fn.Some(models.InboundFee{
			Base: req.InboundFee.BaseFeeMsat,
			Rate: req.InboundFee.FeeRatePpm,
		})
	}

	baseFeeMsat := lnwire.MilliSatoshi(req.BaseFeeMsat)
	feeSchema := routing.FeeSchema{
		BaseFee:    baseFeeMsat,
		FeeRate:    feeRateFixed,
		InboundFee: inboundFee,
	}

	maxHtlc := lnwire.MilliSatoshi(req.MaxHtlcMsat)
	var minHtlc *lnwire.MilliSatoshi
	if req.MinHtlcMsatSpecified {
		min := lnwire.MilliSatoshi(req.MinHtlcMsat)
		minHtlc = &min
	}

	chanPolicy := routing.ChannelPolicy{
		FeeSchema:     feeSchema,
		TimeLockDelta: req.TimeLockDelta,
		MaxHTLC:       maxHtlc,
		MinHTLC:       minHtlc,
	}

	rpcsLog.Debugf("[updatechanpolicy] updating channel policy, "+
		"targets=%v, req=%v", lnutils.SpewLogClosure(targetChans),
		lnutils.SpewLogClosure(req))

	// With the scope resolved, we'll now send this to the local channel
	// manager so it can propagate the new policy for our target channel(s).
	failedUpdates, err := r.server.localChanMgr.UpdatePolicy(
		ctx, chanPolicy, req.CreateMissingEdge, targetChans...,
	)
	if err != nil {
		return nil, err
	}

	return &lnrpc.PolicyUpdateResponse{
		FailedUpdates: failedUpdates,
	}, nil
}

// ForwardingHistory allows the caller to query the htlcswitch for a record of
// all HTLC's forwarded within the target time range, and integer offset within
// that time range. If no time-range is specified, then the first chunk of the
// past 24 hrs of forwarding history are returned.

// A list of forwarding events are returned. The size of each forwarding event
// is 40 bytes, and the max message size able to be returned in gRPC is 4 MiB.
// In order to safely stay under this max limit, we'll return 50k events per
// response.  Each response has the index offset of the last entry. The index
// offset can be provided to the request to allow the caller to skip a series
// of records.
func (r *rpcServer) ForwardingHistory(ctx context.Context,
	req *lnrpc.ForwardingHistoryRequest) (*lnrpc.ForwardingHistoryResponse,
	error) {

	// Before we perform the queries below, we'll instruct the switch to
	// flush any pending events to disk. This ensure we get a complete
	// snapshot at this particular time.
	if err := r.server.htlcSwitch.FlushForwardingEvents(); err != nil {
		return nil, fmt.Errorf("unable to flush forwarding "+
			"events: %v", err)
	}

	var (
		startTime, endTime time.Time

		numEvents uint32
	)

	// startTime defaults to the Unix epoch (0 unixtime, or
	// midnight 01-01-1970).
	startTime = time.Unix(int64(req.StartTime), 0)

	// If the end time wasn't specified, assume a default end time of now.
	if req.EndTime == 0 {
		now := time.Now()
		endTime = now
	} else {
		endTime = time.Unix(int64(req.EndTime), 0)
	}

	// If the number of events wasn't specified, then we'll default to
	// returning the last 100 events.
	numEvents = req.NumMaxEvents
	if numEvents == 0 {
		numEvents = 100
	}

	// Create sets of incoming and outgoing channel IDs from the request
	// for faster lookups for filtering.
	incomingChanIDs := fn.NewSet(req.IncomingChanIds...)
	outgoingChanIDs := fn.NewSet(req.OutgoingChanIds...)

	// Next, we'll map the proto request into a format that is understood by
	// the forwarding log.
	eventQuery := channeldb.ForwardingEventQuery{
		StartTime:       startTime,
		EndTime:         endTime,
		IndexOffset:     req.IndexOffset,
		NumMaxEvents:    numEvents,
		IncomingChanIDs: incomingChanIDs,
		OutgoingChanIDs: outgoingChanIDs,
	}
	timeSlice, err := r.server.miscDB.ForwardingLog().Query(eventQuery)
	if err != nil {
		return nil, fmt.Errorf("unable to query forwarding log: %w",
			err)
	}

	// chanToPeerAlias caches previously looked up channel information.
	chanToPeerAlias := make(map[lnwire.ShortChannelID]string)

	// Helper function to extract a peer's node alias given its SCID.
	getRemoteAlias := func(chanID lnwire.ShortChannelID) (string, error) {
		// If we'd previously seen this chanID then return the cached
		// peer alias.
		if peerAlias, ok := chanToPeerAlias[chanID]; ok {
			return peerAlias, nil
		}

		// Else call the server to look up the peer alias.
		edge, err := r.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
			ChanId: chanID.ToUint64(),
		})
		if err != nil {
			return "", err
		}

		remotePub := edge.Node1Pub
		if r.selfNode.String() == edge.Node1Pub {
			remotePub = edge.Node2Pub
		}

		vertex, err := route.NewVertexFromStr(remotePub)
		if err != nil {
			return "", err
		}

		peer, err := r.server.graphDB.FetchNode(ctx, vertex)
		if err != nil {
			return "", err
		}

		// Cache the peer alias.
		chanToPeerAlias[chanID] = peer.Alias.UnwrapOr("")

		return peer.Alias.UnwrapOr(""), nil
	}

	// TODO(roasbeef): add settlement latency?
	//  * use FPE on all records?

	// With the events retrieved, we'll now map them into the proper proto
	// response.
	//
	// TODO(roasbeef): show in ns for the outside?
	fwdingEvents := make(
		[]*lnrpc.ForwardingEvent, len(timeSlice.ForwardingEvents),
	)
	resp := &lnrpc.ForwardingHistoryResponse{
		ForwardingEvents: fwdingEvents,
		LastOffsetIndex:  timeSlice.LastIndexOffset,
	}
	for i, event := range timeSlice.ForwardingEvents {
		amtInMsat := event.AmtIn
		amtOutMsat := event.AmtOut
		feeMsat := event.AmtIn - event.AmtOut

		resp.ForwardingEvents[i] = &lnrpc.ForwardingEvent{
			Timestamp:   uint64(event.Timestamp.Unix()),
			TimestampNs: uint64(event.Timestamp.UnixNano()),
			ChanIdIn:    event.IncomingChanID.ToUint64(),
			ChanIdOut:   event.OutgoingChanID.ToUint64(),
			AmtIn:       uint64(amtInMsat.ToSatoshis()),
			AmtOut:      uint64(amtOutMsat.ToSatoshis()),
			Fee:         uint64(feeMsat.ToSatoshis()),
			FeeMsat:     uint64(feeMsat),
			AmtInMsat:   uint64(amtInMsat),
			AmtOutMsat:  uint64(amtOutMsat),
		}

		// If the incoming htlc id is present, add it to the response.
		event.IncomingHtlcID.WhenSome(func(id uint64) {
			resp.ForwardingEvents[i].IncomingHtlcId = &id
		})

		// If the outgoing htlc id is present, add it to the response.
		event.OutgoingHtlcID.WhenSome(func(id uint64) {
			resp.ForwardingEvents[i].OutgoingHtlcId = &id
		})

		if req.PeerAliasLookup {
			aliasIn, err := getRemoteAlias(event.IncomingChanID)
			if err != nil {
				aliasIn = fmt.Sprintf("unable to lookup peer "+
					"alias: %v", err)
			}
			aliasOut, err := getRemoteAlias(event.OutgoingChanID)
			if err != nil {
				aliasOut = fmt.Sprintf("unable to lookup peer"+
					"alias: %v", err)
			}
			resp.ForwardingEvents[i].PeerAliasIn = aliasIn
			resp.ForwardingEvents[i].PeerAliasOut = aliasOut
		}
	}

	return resp, nil
}

// ExportChannelBackup attempts to return an encrypted static channel backup
// for the target channel identified by it channel point. The backup is
// encrypted with a key generated from the aezeed seed of the user. The
// returned backup can either be restored using the RestoreChannelBackup method
// once lnd is running, or via the InitWallet and UnlockWallet methods from the
// WalletUnlocker service.
func (r *rpcServer) ExportChannelBackup(ctx context.Context,
	in *lnrpc.ExportChannelBackupRequest) (*lnrpc.ChannelBackup, error) {

	// First, we'll convert the lnrpc channel point into a wire.OutPoint
	// that we can manipulate.
	txid, err := lnrpc.GetChanPointFundingTxid(in.ChanPoint)
	if err != nil {
		return nil, err
	}
	chanPoint := wire.OutPoint{
		Hash:  *txid,
		Index: in.ChanPoint.OutputIndex,
	}

	// Next, we'll attempt to fetch a channel backup for this channel from
	// the database. If this channel has been closed, or the outpoint is
	// unknown, then we'll return an error
	unpackedBackup, err := chanbackup.FetchBackupForChan(
		ctx, chanPoint, r.server.chanStateDB, r.server.addrSource,
	)
	if err != nil {
		return nil, err
	}

	// At this point, we have an unpacked backup (plaintext) so we'll now
	// attempt to serialize and encrypt it in order to create a packed
	// backup.
	packedBackups, err := chanbackup.PackStaticChanBackups(
		[]chanbackup.Single{*unpackedBackup},
		r.server.cc.KeyRing,
	)
	if err != nil {
		return nil, fmt.Errorf("packing of back ups failed: %w", err)
	}

	// Before we proceed, we'll ensure that we received a backup for this
	// channel, otherwise, we'll bail out.
	packedBackup, ok := packedBackups[chanPoint]
	if !ok {
		return nil, fmt.Errorf("expected single backup for "+
			"ChannelPoint(%v), got %v", chanPoint,
			len(packedBackup))
	}

	return &lnrpc.ChannelBackup{
		ChanPoint:  in.ChanPoint,
		ChanBackup: packedBackup,
	}, nil
}

// VerifyChanBackup allows a caller to verify the integrity of a channel backup
// snapshot. This method will accept both either a packed Single or a packed
// Multi. Specifying both will result in an error.
func (r *rpcServer) VerifyChanBackup(ctx context.Context,
	in *lnrpc.ChanBackupSnapshot) (*lnrpc.VerifyChanBackupResponse, error) {

	var (
		channels []chanbackup.Single
		err      error
	)
	switch {
	// If neither a Single or Multi has been specified, then we have nothing
	// to verify.
	case in.GetSingleChanBackups() == nil && in.GetMultiChanBackup() == nil:
		return nil, errors.New("either a Single or Multi channel " +
			"backup must be specified")

	// Either a Single or a Multi must be specified, but not both.
	case in.GetSingleChanBackups() != nil && in.GetMultiChanBackup() != nil:
		return nil, errors.New("either a Single or Multi channel " +
			"backup must be specified, but not both")

	// If a Single is specified then we'll only accept one of them to allow
	// the caller to map the valid/invalid state for each individual Single.
	case in.GetSingleChanBackups() != nil:
		chanBackupsProtos := in.GetSingleChanBackups().ChanBackups
		if len(chanBackupsProtos) != 1 {
			return nil, errors.New("only one Single is accepted " +
				"at a time")
		}

		// First, we'll convert the raw byte slice into a type we can
		// work with a bit better.
		chanBackup := chanbackup.PackedSingles(
			[][]byte{chanBackupsProtos[0].ChanBackup},
		)

		// With our PackedSingles created, we'll attempt to unpack the
		// backup. If this fails, then we know the backup is invalid for
		// some reason.
		channels, err = chanBackup.Unpack(r.server.cc.KeyRing)
		if err != nil {
			return nil, fmt.Errorf("invalid single channel "+
				"backup: %v", err)
		}

	case in.GetMultiChanBackup() != nil:
		// We'll convert the raw byte slice into a PackedMulti that we
		// can easily work with.
		packedMultiBackup := in.GetMultiChanBackup().MultiChanBackup
		packedMulti := chanbackup.PackedMulti(packedMultiBackup)

		// We'll now attempt to unpack the Multi. If this fails, then we
		// know it's invalid.
		multi, err := packedMulti.Unpack(r.server.cc.KeyRing)
		if err != nil {
			return nil, fmt.Errorf("invalid multi channel backup: "+
				"%v", err)
		}

		channels = multi.StaticBackups
	}

	return &lnrpc.VerifyChanBackupResponse{
		ChanPoints: fn.Map(channels, func(c chanbackup.Single) string {
			return c.FundingOutpoint.String()
		}),
	}, nil
}

// createBackupSnapshot converts the passed Single backup into a snapshot which
// contains individual packed single backups, as well as a single packed multi
// backup.
func (r *rpcServer) createBackupSnapshot(backups []chanbackup.Single) (
	*lnrpc.ChanBackupSnapshot, error) {

	// Once we have the set of back ups, we'll attempt to pack them all
	// into a series of single channel backups.
	singleChanPackedBackups, err := chanbackup.PackStaticChanBackups(
		backups, r.server.cc.KeyRing,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to pack set of chan "+
			"backups: %v", err)
	}

	// Now that we have our set of single packed backups, we'll morph that
	// into a form that the proto response requires.
	numBackups := len(singleChanPackedBackups)
	singleBackupResp := &lnrpc.ChannelBackups{
		ChanBackups: make([]*lnrpc.ChannelBackup, 0, numBackups),
	}
	for chanPoint, singlePackedBackup := range singleChanPackedBackups {
		txid := chanPoint.Hash
		rpcChanPoint := &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
				FundingTxidBytes: txid[:],
			},
			OutputIndex: chanPoint.Index,
		}

		singleBackupResp.ChanBackups = append(
			singleBackupResp.ChanBackups,
			&lnrpc.ChannelBackup{
				ChanPoint:  rpcChanPoint,
				ChanBackup: singlePackedBackup,
			},
		)
	}

	// In addition, to the set of single chan backups, we'll also create a
	// single multi-channel backup which can be serialized into a single
	// file for safe storage.
	var b bytes.Buffer
	unpackedMultiBackup := chanbackup.Multi{
		StaticBackups: backups,
	}
	err = unpackedMultiBackup.PackToWriter(&b, r.server.cc.KeyRing)
	if err != nil {
		return nil, fmt.Errorf("unable to multi-pack backups: %w", err)
	}

	multiBackupResp := &lnrpc.MultiChanBackup{
		MultiChanBackup: b.Bytes(),
	}
	for _, singleBackup := range singleBackupResp.ChanBackups {
		multiBackupResp.ChanPoints = append(
			multiBackupResp.ChanPoints, singleBackup.ChanPoint,
		)
	}

	return &lnrpc.ChanBackupSnapshot{
		SingleChanBackups: singleBackupResp,
		MultiChanBackup:   multiBackupResp,
	}, nil
}

// ExportAllChannelBackups returns static channel backups for all existing
// channels known to lnd. A set of regular singular static channel backups for
// each channel are returned. Additionally, a multi-channel backup is returned
// as well, which contains a single encrypted blob containing the backups of
// each channel.
func (r *rpcServer) ExportAllChannelBackups(ctx context.Context,
	in *lnrpc.ChanBackupExportRequest) (*lnrpc.ChanBackupSnapshot, error) {

	// First, we'll attempt to read back ups for ALL currently opened
	// channels from disk.
	allUnpackedBackups, err := chanbackup.FetchStaticChanBackups(
		ctx, r.server.chanStateDB, r.server.addrSource,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch all static chan "+
			"backups: %v", err)
	}

	// With the backups assembled, we'll create a full snapshot.
	return r.createBackupSnapshot(allUnpackedBackups)
}

// RestoreChannelBackups accepts a set of singular channel backups, or a single
// encrypted multi-chan backup and attempts to recover any funds remaining
// within the channel. If we're able to unpack the backup, then the new channel
// will be shown under listchannels, as well as pending channels.
func (r *rpcServer) RestoreChannelBackups(ctx context.Context,
	in *lnrpc.RestoreChanBackupRequest) (*lnrpc.RestoreBackupResponse, error) {

	// The server hasn't yet started, so it won't be able to service any of
	// our requests, so we'll bail early here.
	if !r.server.Started() {
		return nil, ErrServerNotActive
	}

	// First, we'll make our implementation of the
	// chanbackup.ChannelRestorer interface which we'll use to properly
	// restore either a set of chanbackup.Single or chanbackup.Multi
	// backups.
	chanRestorer := &chanDBRestorer{
		db:         r.server.chanStateDB,
		secretKeys: r.server.cc.KeyRing,
		chainArb:   r.server.chainArb,
	}

	// We'll accept either a list of Single backups, or a single Multi
	// backup which contains several single backups.
	var (
		numRestored int
		err         error
	)
	switch {
	case in.GetChanBackups() != nil:
		chanBackupsProtos := in.GetChanBackups()

		// Now that we know what type of backup we're working with,
		// we'll parse them all out into a more suitable format.
		packedBackups := make([][]byte, 0, len(chanBackupsProtos.ChanBackups))
		for _, chanBackup := range chanBackupsProtos.ChanBackups {
			packedBackups = append(
				packedBackups, chanBackup.ChanBackup,
			)
		}

		// With our backups obtained, we'll now restore them which will
		// write the new backups to disk, and then attempt to connect
		// out to any peers that we know of which were our prior
		// channel peers.
		numRestored, err = chanbackup.UnpackAndRecoverSingles(
			chanbackup.PackedSingles(packedBackups),
			r.server.cc.KeyRing, chanRestorer, r.server,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to unpack single "+
				"backups: %v", err)
		}

	case in.GetMultiChanBackup() != nil:
		packedMultiBackup := in.GetMultiChanBackup()

		// With our backups obtained, we'll now restore them which will
		// write the new backups to disk, and then attempt to connect
		// out to any peers that we know of which were our prior
		// channel peers.
		packedMulti := chanbackup.PackedMulti(packedMultiBackup)
		numRestored, err = chanbackup.UnpackAndRecoverMulti(
			packedMulti, r.server.cc.KeyRing, chanRestorer,
			r.server,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to unpack chan "+
				"backup: %v", err)
		}
	}

	return &lnrpc.RestoreBackupResponse{
		NumRestored: uint32(numRestored),
	}, nil
}

// SubscribeChannelBackups allows a client to sub-subscribe to the most up to
// date information concerning the state of all channel back ups. Each time a
// new channel is added, we return the new set of channels, along with a
// multi-chan backup containing the backup info for all channels. Each time a
// channel is closed, we send a new update, which contains new new chan back
// ups, but the updated set of encrypted multi-chan backups with the closed
// channel(s) removed.
func (r *rpcServer) SubscribeChannelBackups(req *lnrpc.ChannelBackupSubscription,
	updateStream lnrpc.Lightning_SubscribeChannelBackupsServer) error {

	// First, we'll subscribe to the primary channel notifier so we can
	// obtain events for new pending/opened/closed channels.
	chanSubscription, err := r.server.channelNotifier.SubscribeChannelEvents()
	if err != nil {
		return err
	}

	defer chanSubscription.Cancel()
	for {
		select {
		// A new event has been sent by the channel notifier, we'll
		// assemble, then sling out a new event to the client.
		case e := <-chanSubscription.Updates():
			// TODO(roasbeef): batch dispatch ntnfs

			switch e.(type) {

			// We only care about new/closed channels, so we'll
			// skip any events for active/inactive channels.
			// To make the subscription behave the same way as the
			// synchronous call and the file based backup, we also
			// include pending channels in the update.
			case channelnotifier.ActiveChannelEvent:
				continue
			case channelnotifier.InactiveChannelEvent:
				continue
			case channelnotifier.ActiveLinkEvent:
				continue
			case channelnotifier.InactiveLinkEvent:
				continue
			}

			// Now that we know the channel state has changed,
			// we'll obtains the current set of single channel
			// backups from disk.
			chanBackups, err := chanbackup.FetchStaticChanBackups(
				updateStream.Context(), r.server.chanStateDB,
				r.server.addrSource,
			)
			if err != nil {
				return fmt.Errorf("unable to fetch all "+
					"static chan backups: %v", err)
			}

			// With our backups obtained, we'll pack them into a
			// snapshot and send them back to the client.
			backupSnapshot, err := r.createBackupSnapshot(
				chanBackups,
			)
			if err != nil {
				return err
			}
			err = updateStream.Send(backupSnapshot)
			if err != nil {
				return err
			}

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-updateStream.Context().Done():
			if errors.Is(updateStream.Context().Err(), context.Canceled) {
				return nil
			}
			return updateStream.Context().Err()

		case <-r.quit:
			return nil
		}
	}
}

// ChannelAcceptor dispatches a bi-directional streaming RPC in which
// OpenChannel requests are sent to the client and the client responds with
// a boolean that tells LND whether or not to accept the channel. This allows
// node operators to specify their own criteria for accepting inbound channels
// through a single persistent connection.
func (r *rpcServer) ChannelAcceptor(stream lnrpc.Lightning_ChannelAcceptorServer) error {
	chainedAcceptor := r.chanPredicate

	// Create a new RPCAcceptor which will send requests into the
	// newRequests channel when it receives them.
	rpcAcceptor := chanacceptor.NewRPCAcceptor(
		stream.Recv, stream.Send, r.cfg.AcceptorTimeout,
		r.cfg.ActiveNetParams.Params, r.quit,
	)

	// Add the RPCAcceptor to the ChainedAcceptor and defer its removal.
	id := chainedAcceptor.AddAcceptor(rpcAcceptor)
	defer chainedAcceptor.RemoveAcceptor(id)

	// Run the rpc acceptor, which will accept requests for channel
	// acceptance decisions from our chained acceptor, send them to the
	// channel acceptor and listen for and report responses. This function
	// blocks, and will exit if the rpcserver receives the instruction to
	// shutdown, or the client cancels.
	return rpcAcceptor.Run()
}

// BakeMacaroon allows the creation of a new macaroon with custom read and write
// permissions. No first-party caveats are added since this can be done offline.
// If the --allow-external-permissions flag is set, the RPC will allow
// external permissions that LND is not aware of.
func (r *rpcServer) BakeMacaroon(ctx context.Context,
	req *lnrpc.BakeMacaroonRequest) (*lnrpc.BakeMacaroonResponse, error) {

	// If the --no-macaroons flag is used to start lnd, the macaroon service
	// is not initialized. Therefore we can't bake new macaroons.
	if r.macService == nil {
		return nil, errMacaroonDisabled
	}

	helpMsg := fmt.Sprintf("supported actions are %v, supported entities "+
		"are %v", validActions, validEntities)

	// Don't allow empty permission list as it doesn't make sense to have
	// a macaroon that is not allowed to access any RPC.
	if len(req.Permissions) == 0 {
		return nil, fmt.Errorf("permission list cannot be empty. "+
			"specify at least one action/entity pair. %s", helpMsg)
	}

	// Validate and map permission struct used by gRPC to the one used by
	// the bakery. If the --allow-external-permissions flag is set, we
	// will not validate, but map.
	requestedPermissions := make([]bakery.Op, len(req.Permissions))
	for idx, op := range req.Permissions {
		if req.AllowExternalPermissions {
			requestedPermissions[idx] = bakery.Op{
				Entity: op.Entity,
				Action: op.Action,
			}
			continue
		}

		if !stringInSlice(op.Entity, validEntities) {
			return nil, fmt.Errorf("invalid permission entity. %s",
				helpMsg)
		}

		// Either we have the special entity "uri" which specifies a
		// full gRPC URI or we have one of the pre-defined actions.
		if op.Entity == macaroons.PermissionEntityCustomURI {
			allPermissions := r.interceptorChain.Permissions()
			_, ok := allPermissions[op.Action]
			if !ok {
				return nil, fmt.Errorf("invalid permission " +
					"action, must be an existing URI in " +
					"the format /package.Service/" +
					"MethodName")
			}
		} else if !stringInSlice(op.Action, validActions) {
			return nil, fmt.Errorf("invalid permission action. %s",
				helpMsg)
		}

		requestedPermissions[idx] = bakery.Op{
			Entity: op.Entity,
			Action: op.Action,
		}
	}

	// Convert root key id from uint64 to bytes. Because the
	// DefaultRootKeyID is a digit 0 expressed in a byte slice of a string
	// "0", we will keep the IDs in the same format - all must be numeric,
	// and must be a byte slice of string value of the digit, e.g.,
	// uint64(123) to string(123).
	rootKeyID := []byte(strconv.FormatUint(req.RootKeyId, 10))

	// Bake new macaroon with the given permissions and send it binary
	// serialized and hex encoded to the client.
	newMac, err := r.macService.NewMacaroon(
		ctx, rootKeyID, requestedPermissions...,
	)
	if err != nil {
		return nil, err
	}
	newMacBytes, err := newMac.M().MarshalBinary()
	if err != nil {
		return nil, err
	}
	resp := &lnrpc.BakeMacaroonResponse{}
	resp.Macaroon = hex.EncodeToString(newMacBytes)

	return resp, nil
}

// ListMacaroonIDs returns a list of macaroon root key IDs in use.
func (r *rpcServer) ListMacaroonIDs(ctx context.Context,
	req *lnrpc.ListMacaroonIDsRequest) (
	*lnrpc.ListMacaroonIDsResponse, error) {

	// If the --no-macaroons flag is used to start lnd, the macaroon service
	// is not initialized. Therefore we can't show any IDs.
	if r.macService == nil {
		return nil, errMacaroonDisabled
	}

	rootKeyIDByteSlice, err := r.macService.ListMacaroonIDs(ctx)
	if err != nil {
		return nil, err
	}

	var rootKeyIDs []uint64
	for _, value := range rootKeyIDByteSlice {
		// Convert bytes into uint64.
		id, err := strconv.ParseUint(string(value), 10, 64)
		if err != nil {
			return nil, err
		}

		rootKeyIDs = append(rootKeyIDs, id)
	}

	return &lnrpc.ListMacaroonIDsResponse{RootKeyIds: rootKeyIDs}, nil
}

// DeleteMacaroonID removes a specific macaroon ID.
func (r *rpcServer) DeleteMacaroonID(ctx context.Context,
	req *lnrpc.DeleteMacaroonIDRequest) (
	*lnrpc.DeleteMacaroonIDResponse, error) {

	// If the --no-macaroons flag is used to start lnd, the macaroon service
	// is not initialized. Therefore we can't delete any IDs.
	if r.macService == nil {
		return nil, errMacaroonDisabled
	}

	// Convert root key id from uint64 to bytes. Because the
	// DefaultRootKeyID is a digit 0 expressed in a byte slice of a string
	// "0", we will keep the IDs in the same format - all must be digit, and
	// must be a byte slice of string value of the digit.
	rootKeyID := []byte(strconv.FormatUint(req.RootKeyId, 10))
	deletedIDBytes, err := r.macService.DeleteMacaroonID(ctx, rootKeyID)
	if err != nil {
		return nil, err
	}

	return &lnrpc.DeleteMacaroonIDResponse{
		// If the root key ID doesn't exist, it won't be deleted. We
		// will return a response with deleted = false, otherwise true.
		Deleted: deletedIDBytes != nil,
	}, nil
}

// ListPermissions lists all RPC method URIs and their required macaroon
// permissions to access them.
func (r *rpcServer) ListPermissions(_ context.Context,
	_ *lnrpc.ListPermissionsRequest) (*lnrpc.ListPermissionsResponse,
	error) {

	permissionMap := make(map[string]*lnrpc.MacaroonPermissionList)
	for uri, perms := range r.interceptorChain.Permissions() {
		rpcPerms := make([]*lnrpc.MacaroonPermission, len(perms))
		for idx, perm := range perms {
			rpcPerms[idx] = &lnrpc.MacaroonPermission{
				Entity: perm.Entity,
				Action: perm.Action,
			}
		}
		permissionMap[uri] = &lnrpc.MacaroonPermissionList{
			Permissions: rpcPerms,
		}
	}

	return &lnrpc.ListPermissionsResponse{
		MethodPermissions: permissionMap,
	}, nil
}

// CheckMacaroonPermissions checks whether the provided macaroon contains all
// the provided permissions. If the macaroon is valid (e.g. all caveats are
// satisfied), and all permissions provided in the request are met, then
// this RPC returns true.
func (r *rpcServer) CheckMacaroonPermissions(ctx context.Context,
	req *lnrpc.CheckMacPermRequest) (*lnrpc.CheckMacPermResponse, error) {

	// Sanity-check the input parameters to eliminate impossible
	// combinations.
	switch {
	case len(req.Permissions) > 0 && req.CheckDefaultPermsFromFullMethod:
		return nil, fmt.Errorf("cannot check default permissions " +
			"from full method and from provided permission list " +
			"at the same time")

	case len(req.FullMethod) == 0 && req.CheckDefaultPermsFromFullMethod:
		return nil, fmt.Errorf("cannot check default permissions " +
			"from full method without providing the full method " +
			"name")
	}

	// Turn grpc macaroon permission into bakery.Op for the server to
	// process.
	permissions := make([]bakery.Op, len(req.Permissions))
	for idx, perm := range req.Permissions {
		permissions[idx] = bakery.Op{
			Entity: perm.Entity,
			Action: perm.Action,
		}
	}

	// If the user wants to check the default permissions for the
	// full method, then we'll use the interceptor chain to obtain the
	// default permissions for the full method. This overwrites the
	// user-provided permissions parsed above, but those are required to be
	// empty anyway if the flag is turned on.
	if req.CheckDefaultPermsFromFullMethod {
		allPerms := r.interceptorChain.Permissions()
		methodPerms, ok := allPerms[req.FullMethod]
		if !ok {
			return nil, fmt.Errorf("no permissions found for "+
				"full method %s", req.FullMethod)
		}

		permissions = methodPerms
	}

	err := r.macService.CheckMacAuth(
		ctx, req.Macaroon, permissions, req.FullMethod,
	)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	return &lnrpc.CheckMacPermResponse{
		Valid: true,
	}, nil
}

// FundingStateStep is an advanced funding related call that allows the caller
// to either execute some preparatory steps for a funding workflow, or manually
// progress a funding workflow. The primary way a funding flow is identified is
// via its pending channel ID. As an example, this method can be used to
// specify that we're expecting a funding flow for a particular pending channel
// ID, for which we need to use specific parameters.  Alternatively, this can
// be used to interactively drive PSBT signing for funding for partially
// complete funding transactions.
func (r *rpcServer) FundingStateStep(ctx context.Context,
	in *lnrpc.FundingTransitionMsg) (*lnrpc.FundingStateStepResp, error) {

	var pendingChanID [32]byte
	switch {
	// If this is a message to register a new shim that is an external
	// channel point, then we'll contact the wallet to register this new
	// shim. A user will use this method to register a new channel funding
	// workflow which has already been partially negotiated outside of the
	// core protocol.
	case in.GetShimRegister() != nil &&
		in.GetShimRegister().GetChanPointShim() != nil:

		rpcShimIntent := in.GetShimRegister().GetChanPointShim()

		// Using the rpc shim as a template, we'll construct a new
		// chanfunding.Assembler that is able to express proper
		// formulation of this expected channel.
		shimAssembler, err := newFundingShimAssembler(
			rpcShimIntent, false, r.server.cc.KeyRing,
		)
		if err != nil {
			return nil, err
		}
		req := &chanfunding.Request{
			RemoteAmt: btcutil.Amount(rpcShimIntent.Amt),
		}
		shimIntent, err := shimAssembler.ProvisionChannel(req)
		if err != nil {
			return nil, err
		}

		// Once we have the intent, we'll register it with the wallet.
		// Once we receive an incoming funding request that uses this
		// pending channel ID, then this shim will be dispatched in
		// place of our regular funding workflow.
		copy(pendingChanID[:], rpcShimIntent.PendingChanId)
		err = r.server.cc.Wallet.RegisterFundingIntent(
			pendingChanID, shimIntent,
		)
		if err != nil {
			return nil, err
		}

	// There is no need to register a PSBT shim before opening the channel,
	// even though our RPC message structure allows for it. Inform the user
	// by returning a proper error instead of just doing nothing.
	case in.GetShimRegister() != nil &&
		in.GetShimRegister().GetPsbtShim() != nil:

		return nil, fmt.Errorf("PSBT shim must only be sent when " +
			"opening a channel")

	// If this is a transition to cancel an existing shim, then we'll pass
	// this message along to the wallet, informing it that the intent no
	// longer needs to be considered and should be cleaned up.
	case in.GetShimCancel() != nil:
		rpcsLog.Debugf("Canceling funding shim for pending_id=%x",
			in.GetShimCancel().PendingChanId)

		copy(pendingChanID[:], in.GetShimCancel().PendingChanId)
		err := r.server.cc.Wallet.CancelFundingIntent(pendingChanID)
		if err != nil {
			return nil, err
		}

	// If this is a transition to verify the PSBT for an existing shim,
	// we'll do so and then store the verified PSBT for later so we can
	// compare it to the final, signed one.
	case in.GetPsbtVerify() != nil:
		rpcsLog.Debugf("Verifying PSBT for pending_id=%x",
			in.GetPsbtVerify().PendingChanId)

		copy(pendingChanID[:], in.GetPsbtVerify().PendingChanId)
		packet, err := psbt.NewFromRawBytes(
			bytes.NewReader(in.GetPsbtVerify().FundedPsbt), false,
		)
		if err != nil {
			return nil, fmt.Errorf("error parsing psbt: %w", err)
		}

		err = r.server.cc.Wallet.PsbtFundingVerify(
			pendingChanID, packet, in.GetPsbtVerify().SkipFinalize,
		)
		if err != nil {
			return nil, err
		}

	// If this is a transition to finalize the PSBT funding flow, we compare
	// the final PSBT to the previously verified one and if nothing
	// unexpected was changed, continue the channel opening process.
	case in.GetPsbtFinalize() != nil:
		msg := in.GetPsbtFinalize()
		rpcsLog.Debugf("Finalizing PSBT for pending_id=%x",
			msg.PendingChanId)

		copy(pendingChanID[:], in.GetPsbtFinalize().PendingChanId)

		var (
			packet *psbt.Packet
			rawTx  *wire.MsgTx
			err    error
		)

		// Either the signed PSBT or the raw transaction need to be set
		// but not both at the same time.
		switch {
		case len(msg.SignedPsbt) > 0 && len(msg.FinalRawTx) > 0:
			return nil, fmt.Errorf("cannot set both signed PSBT " +
				"and final raw TX at the same time")

		case len(msg.SignedPsbt) > 0:
			packet, err = psbt.NewFromRawBytes(
				bytes.NewReader(in.GetPsbtFinalize().SignedPsbt),
				false,
			)
			if err != nil {
				return nil, fmt.Errorf("error parsing psbt: %w",
					err)
			}

		case len(msg.FinalRawTx) > 0:
			rawTx = &wire.MsgTx{}
			err = rawTx.Deserialize(bytes.NewReader(msg.FinalRawTx))
			if err != nil {
				return nil, fmt.Errorf("error parsing final "+
					"raw TX: %v", err)
			}

		default:
			return nil, fmt.Errorf("PSBT or raw transaction to " +
				"finalize missing")
		}

		err = r.server.cc.Wallet.PsbtFundingFinalize(
			pendingChanID, packet, rawTx,
		)
		if err != nil {
			return nil, err
		}
	}

	// TODO(roasbeef): extend PendingChannels to also show shims

	// TODO(roasbeef): return resulting state? also add a method to query
	// current state?
	return &lnrpc.FundingStateStepResp{}, nil
}

// RegisterRPCMiddleware adds a new gRPC middleware to the interceptor chain. A
// gRPC middleware is software component external to lnd that aims to add
// additional business logic to lnd by observing/intercepting/validating
// incoming gRPC client requests and (if needed) replacing/overwriting outgoing
// messages before they're sent to the client. When registering the middleware
// must identify itself and indicate what custom macaroon caveats it wants to
// be responsible for. Only requests that contain a macaroon with that specific
// custom caveat are then sent to the middleware for inspection. As a security
// measure, _no_ middleware can intercept requests made with _unencumbered_
// macaroons!
func (r *rpcServer) RegisterRPCMiddleware(
	stream lnrpc.Lightning_RegisterRPCMiddlewareServer) error {

	// This is a security critical functionality and needs to be enabled
	// specifically by the user.
	if !r.cfg.RPCMiddleware.Enable {
		return fmt.Errorf("RPC middleware not enabled in config")
	}

	// When registering a middleware the first message being sent from the
	// middleware must be a registration message containing its name and the
	// custom caveat it wants to register for.
	var (
		registerChan     = make(chan *lnrpc.MiddlewareRegistration, 1)
		registerDoneChan = make(chan struct{})
		errChan          = make(chan error, 1)
	)
	ctxc, cancel := context.WithTimeout(
		stream.Context(), r.cfg.RPCMiddleware.InterceptTimeout,
	)
	defer cancel()

	// Read the first message in a goroutine because the Recv method blocks
	// until the message arrives.
	go func() {
		msg, err := stream.Recv()
		if err != nil {
			errChan <- err

			return
		}

		registerChan <- msg.GetRegister()
	}()

	// Wait for the initial message to arrive or time out if it takes too
	// long.
	var registerMsg *lnrpc.MiddlewareRegistration
	select {
	case registerMsg = <-registerChan:
		if registerMsg == nil {
			return fmt.Errorf("invalid initial middleware " +
				"registration message")
		}

	case err := <-errChan:
		return fmt.Errorf("error receiving initial middleware "+
			"registration message: %v", err)

	case <-ctxc.Done():
		return ctxc.Err()

	case <-r.quit:
		return ErrServerShuttingDown
	}

	// Make sure the registration is valid.
	const nameMinLength = 5
	if len(registerMsg.MiddlewareName) < nameMinLength {
		return fmt.Errorf("invalid middleware name, use descriptive "+
			"name of at least %d characters", nameMinLength)
	}

	readOnly := registerMsg.ReadOnlyMode
	caveatName := registerMsg.CustomMacaroonCaveatName
	switch {
	case readOnly && len(caveatName) > 0:
		return fmt.Errorf("cannot set read-only and custom caveat " +
			"name at the same time")

	case !readOnly && len(caveatName) < nameMinLength:
		return fmt.Errorf("need to set either custom caveat name "+
			"of at least %d characters or read-only mode",
			nameMinLength)
	}

	middleware := rpcperms.NewMiddlewareHandler(
		registerMsg.MiddlewareName,
		caveatName, readOnly, stream.Recv, stream.Send,
		r.cfg.RPCMiddleware.InterceptTimeout,
		r.cfg.ActiveNetParams.Params, r.quit,
	)

	// Add the RPC middleware to the interceptor chain and defer its
	// removal.
	if err := r.interceptorChain.RegisterMiddleware(middleware); err != nil {
		return fmt.Errorf("error registering middleware: %w", err)
	}
	defer r.interceptorChain.RemoveMiddleware(registerMsg.MiddlewareName)

	// Send a message to the client to indicate that the registration has
	// successfully completed.
	regCompleteMsg := &lnrpc.RPCMiddlewareRequest{
		InterceptType: &lnrpc.RPCMiddlewareRequest_RegComplete{
			RegComplete: true,
		},
	}

	// Send the message in a goroutine because the Send method blocks until
	// the message is read by the client.
	go func() {
		err := stream.Send(regCompleteMsg)
		if err != nil {
			errChan <- err
			return
		}

		close(registerDoneChan)
	}()

	select {
	case err := <-errChan:
		return fmt.Errorf("error sending middleware registration "+
			"complete message: %v", err)

	case <-ctxc.Done():
		return ctxc.Err()

	case <-r.quit:
		return ErrServerShuttingDown

	case <-registerDoneChan:
	}

	return middleware.Run()
}

// SendCustomMessage sends a custom peer message.
func (r *rpcServer) SendCustomMessage(ctx context.Context,
	req *lnrpc.SendCustomMessageRequest) (*lnrpc.SendCustomMessageResponse,
	error) {

	peer, err := route.NewVertexFromBytes(req.Peer)
	if err != nil {
		return nil, err
	}

	err = r.server.SendCustomMessage(
		ctx, peer, lnwire.MessageType(req.Type), req.Data,
	)
	switch {
	case errors.Is(err, ErrPeerNotConnected):
		return nil, status.Error(codes.NotFound, err.Error())
	case err != nil:
		return nil, err
	}

	return &lnrpc.SendCustomMessageResponse{
		Status: "message sent successfully",
	}, nil
}

// SubscribeCustomMessages subscribes to a stream of incoming custom peer
// messages.
func (r *rpcServer) SubscribeCustomMessages(
	_ *lnrpc.SubscribeCustomMessagesRequest,
	server lnrpc.Lightning_SubscribeCustomMessagesServer) error {

	client, err := r.server.SubscribeCustomMessages()
	if err != nil {
		return err
	}
	defer client.Cancel()

	for {
		select {
		case <-client.Quit():
			return errors.New("shutdown")

		case <-server.Context().Done():
			return server.Context().Err()

		case update := <-client.Updates():
			customMsg := update.(*CustomMessage)

			err := server.Send(&lnrpc.CustomMessage{
				Peer: customMsg.Peer[:],
				Data: customMsg.Msg.Data,
				Type: uint32(customMsg.Msg.Type),
			})
			if err != nil {
				return err
			}
		}
	}
}

// SendOnionMessage sends a custom peer message.
func (r *rpcServer) SendOnionMessage(ctx context.Context,
	req *lnrpc.SendOnionMessageRequest) (*lnrpc.SendOnionMessageResponse,
	error) {

	// First we'll validate the string passed in within the request to
	// ensure that it's a valid hex-string, and also a valid compressed
	// public key.
	pathKey, err := btcec.ParsePubKey(req.PathKey)
	if err != nil {
		return nil, fmt.Errorf("unable to decode path key bytes: %w",
			err)
	}

	peer, err := route.NewVertexFromBytes(req.Peer)
	if err != nil {
		return nil, err
	}

	err = r.server.SendOnionMessage(ctx, peer, pathKey, req.Onion)
	switch {
	case errors.Is(err, ErrPeerNotConnected):
		return nil, status.Error(codes.NotFound, err.Error())
	case err != nil:
		return nil, err
	}

	return &lnrpc.SendOnionMessageResponse{
		Status: "onion message sent successfully",
	}, nil
}

// SubscribeOnionMessages subscribes to a stream of incoming onion messages.
func (r *rpcServer) SubscribeOnionMessages(
	_ *lnrpc.SubscribeOnionMessagesRequest,
	server lnrpc.Lightning_SubscribeOnionMessagesServer) error {

	client, err := r.server.SubscribeOnionMessages()
	if err != nil {
		return err
	}
	defer client.Cancel()

	for {
		select {
		case <-client.Quit():
			return errors.New("shutdown")

		case <-server.Context().Done():
			return server.Context().Err()

		case update := <-client.Updates():
			oMsg, ok := update.(*onionmessage.OnionMessageUpdate)
			if !ok {
				return fmt.Errorf("onion message update "+
					"failed type assertion: %T", update)
			}

			err := server.Send(&lnrpc.OnionMessage{
				Peer:    oMsg.Peer[:],
				PathKey: oMsg.PathKey[:],
				Onion:   oMsg.OnionBlob,
			})
			if err != nil {
				return err
			}
		}
	}
}

// ListAliases returns the set of all aliases we have ever allocated along with
// their base SCIDs and possibly a separate confirmed SCID in the case of
// zero-conf.
func (r *rpcServer) ListAliases(_ context.Context,
	_ *lnrpc.ListAliasesRequest) (*lnrpc.ListAliasesResponse, error) {

	// Fetch the map of all aliases.
	mapAliases := r.server.aliasMgr.ListAliases()

	// Fill out the response. This does not include the zero-conf confirmed
	// SCID. Doing so would require more database lookups, and it can be
	// cross-referenced with the output of ListChannels/ClosedChannels.
	resp := &lnrpc.ListAliasesResponse{
		AliasMaps: make([]*lnrpc.AliasMap, 0),
	}

	// Now we need to parse the created mappings into an rpc response.
	resp.AliasMaps = lnrpc.MarshalAliasMap(mapAliases)

	return resp, nil
}

// rpcInitiator returns the correct lnrpc initiator for channels where we have
// a record of the opening channel.
func rpcInitiator(isInitiator bool) lnrpc.Initiator {
	if isInitiator {
		return lnrpc.Initiator_INITIATOR_LOCAL
	}

	return lnrpc.Initiator_INITIATOR_REMOTE
}

// chainSyncInfo wraps info about the best block and whether the system is
// synced to that block.
type chainSyncInfo struct {
	// isSynced specifies whether the whole system is considered synced.
	// When true, it means the following subsystems are at the best height
	// reported by the chain backend,
	// - wallet.
	// - channel graph.
	// - blockbeat dispatcher.
	isSynced bool

	// bestHeight is the current height known to the chain backend.
	bestHeight int32

	// blockHash is the hash of the current block known to the chain
	// backend.
	blockHash chainhash.Hash

	// timestamp is the block's timestamp the wallet has synced to.
	timestamp int64
}

// getChainSyncInfo queries the chain backend, the wallet, the channel router
// and the blockbeat dispatcher to determine the best block and whether the
// system is considered synced.
func (r *rpcServer) getChainSyncInfo() (*chainSyncInfo, error) {
	bestHash, bestHeight, err := r.server.cc.ChainIO.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("unable to get best block info: %w", err)
	}

	isSynced, bestHeaderTimestamp, err := r.server.cc.Wallet.IsSynced()
	if err != nil {
		return nil, fmt.Errorf("unable to sync PoV of the wallet "+
			"with current best block in the main chain: %v", err)
	}

	// Create an info to be returned.
	info := &chainSyncInfo{
		isSynced:   isSynced,
		bestHeight: bestHeight,
		blockHash:  *bestHash,
		timestamp:  bestHeaderTimestamp,
	}

	// Exit early if the wallet is not synced.
	if !isSynced {
		rpcsLog.Debugf("Wallet is not synced to height %v yet",
			bestHeight)

		return info, nil
	}

	// If the router does full channel validation, it has a lot of work to
	// do for each block. So it might be possible that it isn't yet up to
	// date with the most recent block, even if the wallet is. This can
	// happen in environments with high CPU load (such as parallel itests).
	// Since the `synced_to_chain` flag in the response of this call is used
	// by many wallets (and also our itests) to make sure everything's up to
	// date, we add the router's state to it. So the flag will only toggle
	// to true once the router was also able to catch up.
	if !r.cfg.Routing.AssumeChannelValid {
		routerHeight := r.server.graphBuilder.SyncedHeight()
		isSynced = uint32(bestHeight) == routerHeight
	}

	// Exit early if the channel graph is not synced.
	if !isSynced {
		rpcsLog.Debugf("Graph is not synced to height %v yet",
			bestHeight)

		return info, nil
	}

	// Given the wallet and the channel router are synced, we now check
	// whether the blockbeat dispatcher is synced.
	height := r.server.blockbeatDispatcher.CurrentHeight()

	// Overwrite isSynced and return.
	info.isSynced = height == bestHeight

	if !info.isSynced {
		rpcsLog.Debugf("Blockbeat is not synced to height %v yet",
			bestHeight)
	}

	return info, nil
}

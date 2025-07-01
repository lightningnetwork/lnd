package chainreg

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainntnfs/bitcoindnotify"
	"github.com/lightningnetwork/lnd/chainntnfs/btcdnotify"
	"github.com/lightningnetwork/lnd/chainntnfs/neutrinonotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/lightningnetwork/lnd/walletunlocker"
)

// Config houses necessary fields that a chainControl instance needs to
// function.
type Config struct {
	// Bitcoin defines settings for the Bitcoin chain.
	Bitcoin *lncfg.Chain

	// HeightHintCacheQueryDisable is a boolean that disables height hint
	// queries if true.
	HeightHintCacheQueryDisable bool

	// NeutrinoMode defines settings for connecting to a neutrino
	// light-client.
	NeutrinoMode *lncfg.Neutrino

	// BitcoindMode defines settings for connecting to a bitcoind node.
	BitcoindMode *lncfg.Bitcoind

	// BtcdMode defines settings for connecting to a btcd node.
	BtcdMode *lncfg.Btcd

	// HeightHintDB is a pointer to the database that stores the height
	// hints.
	HeightHintDB kvdb.Backend

	// ChanStateDB is a pointer to the database that stores the channel
	// state.
	ChanStateDB *channeldb.ChannelStateDB

	// AuxLeafStore is an optional store that can be used to store auxiliary
	// leaves for certain custom channel types.
	AuxLeafStore fn.Option[lnwallet.AuxLeafStore]

	// AuxSigner is an optional signer that can be used to sign auxiliary
	// leaves for certain custom channel types.
	AuxSigner fn.Option[lnwallet.AuxSigner]

	// BlockCache is the main cache for storing block information.
	BlockCache *blockcache.BlockCache

	// WalletUnlockParams are the parameters that were used for unlocking
	// the main wallet.
	WalletUnlockParams *walletunlocker.WalletUnlockParams

	// NeutrinoCS is a pointer to a neutrino ChainService. Must be non-nil
	// if using neutrino.
	NeutrinoCS *neutrino.ChainService

	// ActiveNetParams details the current chain we are on.
	ActiveNetParams BitcoinNetParams

	// Deprecated: Use Fee.URL. FeeURL defines the URL for fee estimation
	// we will use. This field is optional.
	FeeURL string

	// Fee defines settings for the web fee estimator. This field is
	// optional.
	Fee *lncfg.Fee

	// Dialer is a function closure that will be used to establish outbound
	// TCP connections to Bitcoin peers in the event of a pruned block being
	// requested.
	Dialer chain.Dialer
}

const (
	// DefaultBitcoinMinHTLCInMSat is the default smallest value htlc this
	// node will accept. This value is proposed in the channel open sequence
	// and cannot be changed during the life of the channel. It is 1 msat by
	// default to allow maximum flexibility in deciding what size payments
	// to forward.
	//
	// All forwarded payments are subjected to the min htlc constraint of
	// the routing policy of the outgoing channel. This implicitly controls
	// the minimum htlc value on the incoming channel too.
	DefaultBitcoinMinHTLCInMSat = lnwire.MilliSatoshi(1)

	// DefaultBitcoinMinHTLCOutMSat is the default minimum htlc value that
	// we require for sending out htlcs. Our channel peer may have a lower
	// min htlc channel parameter, but we - by default - don't forward
	// anything under the value defined here.
	DefaultBitcoinMinHTLCOutMSat = lnwire.MilliSatoshi(1000)

	// DefaultBitcoinBaseFeeMSat is the default forwarding base fee.
	DefaultBitcoinBaseFeeMSat = lnwire.MilliSatoshi(1000)

	// DefaultBitcoinFeeRate is the default forwarding fee rate.
	DefaultBitcoinFeeRate = lnwire.MilliSatoshi(1)

	// DefaultBitcoinTimeLockDelta is the default forwarding time lock
	// delta.
	DefaultBitcoinTimeLockDelta = 80

	// DefaultBitcoinStaticFeePerKW is the fee rate of 50 sat/vbyte
	// expressed in sat/kw.
	DefaultBitcoinStaticFeePerKW = chainfee.SatPerKWeight(12500)

	// DefaultBitcoinStaticMinRelayFeeRate is the min relay fee used for
	// static estimators.
	DefaultBitcoinStaticMinRelayFeeRate = chainfee.FeePerKwFloor

	// DefaultMinOutboundPeers is the min number of connected
	// outbound peers the chain backend should have to maintain a
	// healthy connection to the network.
	DefaultMinOutboundPeers = 6
)

// PartialChainControl contains all the primary interfaces of the chain control
// that can be purely constructed from the global configuration. No wallet
// instance is required for constructing this partial state.
type PartialChainControl struct {
	// Cfg is the configuration that was used to create the partial chain
	// control.
	Cfg *Config

	// HealthCheck is a function which can be used to send a low-cost, fast
	// query to the chain backend to ensure we still have access to our
	// node.
	HealthCheck func() error

	// FeeEstimator is used to estimate an optimal fee for transactions
	// important to us.
	FeeEstimator chainfee.Estimator

	// ChainNotifier is used to receive blockchain events that we are
	// interested in.
	ChainNotifier chainntnfs.ChainNotifier

	// BestBlockTracker is used to maintain a view of the global
	// chain state that changes over time
	BestBlockTracker *chainntnfs.BestBlockTracker

	// MempoolNotifier is used to watch for spending events happened in
	// mempool.
	MempoolNotifier chainntnfs.MempoolWatcher

	// ChainView is used in the router for maintaining an up-to-date graph.
	ChainView chainview.FilteredChainView

	// ChainSource is the primary chain interface. This is used to operate
	// the wallet and do things such as rescanning, sending transactions,
	// notifications for received funds, etc.
	ChainSource chain.Interface

	// RoutingPolicy is the routing policy we have decided to use.
	RoutingPolicy models.ForwardingPolicy

	// MinHtlcIn is the minimum HTLC we will accept.
	MinHtlcIn lnwire.MilliSatoshi
}

// ChainControl couples the three primary interfaces lnd utilizes for a
// particular chain together. A single ChainControl instance will exist for all
// the chains lnd is currently active on.
type ChainControl struct {
	// PartialChainControl is the part of the chain control that was
	// initialized purely from the configuration and doesn't contain any
	// wallet related elements.
	*PartialChainControl

	// ChainIO represents an abstraction over a source that can query the
	// blockchain.
	ChainIO lnwallet.BlockChainIO

	// Signer is used to provide signatures over things like transactions.
	Signer input.Signer

	// KeyRing represents a set of keys that we have the private keys to.
	KeyRing keychain.SecretKeyRing

	// Wc is an abstraction over some basic wallet commands. This base set
	// of commands will be provided to the Wallet *LightningWallet raw
	// pointer below.
	Wc lnwallet.WalletController

	// MsgSigner is used to sign arbitrary messages.
	MsgSigner lnwallet.MessageSigner

	// Wallet is our LightningWallet that also contains the abstract Wc
	// above. This wallet handles all of the lightning operations.
	Wallet *lnwallet.LightningWallet
}

// NewPartialChainControl creates a new partial chain control that contains all
// the parts that can be purely constructed from the passed in global
// configuration and doesn't need any wallet instance yet.
//
//nolint:ll
func NewPartialChainControl(cfg *Config) (*PartialChainControl, func(), error) {
	cc := &PartialChainControl{
		Cfg: cfg,
		RoutingPolicy: models.ForwardingPolicy{
			MinHTLCOut:    cfg.Bitcoin.MinHTLCOut,
			BaseFee:       cfg.Bitcoin.BaseFee,
			FeeRate:       cfg.Bitcoin.FeeRate,
			TimeLockDelta: cfg.Bitcoin.TimeLockDelta,
		},
		MinHtlcIn: cfg.Bitcoin.MinHTLCIn,
		FeeEstimator: chainfee.NewStaticEstimator(
			DefaultBitcoinStaticFeePerKW,
			DefaultBitcoinStaticMinRelayFeeRate,
		),
	}

	var err error
	heightHintCacheConfig := channeldb.CacheConfig{
		QueryDisable: cfg.HeightHintCacheQueryDisable,
	}
	if cfg.HeightHintCacheQueryDisable {
		log.Infof("Height Hint Cache Queries disabled")
	}

	// Initialize the height hint cache within the chain directory.
	hintCache, err := channeldb.NewHeightHintCache(
		heightHintCacheConfig, cfg.HeightHintDB,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to initialize height hint "+
			"cache: %v", err)
	}

	// Map the deprecated feeurl flag to fee.url.
	if cfg.FeeURL != "" {
		if cfg.Fee.URL != "" {
			return nil, nil, errors.New("fee.url and " +
				"feeurl are mutually exclusive")
		}

		cfg.Fee.URL = cfg.FeeURL
	}

	// If spv mode is active, then we'll be using a distinct set of
	// chainControl interfaces that interface directly with the p2p network
	// of the selected chain.
	switch cfg.Bitcoin.Node {
	case "neutrino":
		// We'll create ChainNotifier and FilteredChainView instances,
		// along with the wallet's ChainSource, which are all backed by
		// the neutrino light client.
		cc.ChainNotifier = neutrinonotify.New(
			cfg.NeutrinoCS, hintCache, hintCache, cfg.BlockCache,
		)
		cc.ChainView, err = chainview.NewCfFilteredChainView(
			cfg.NeutrinoCS, cfg.BlockCache,
		)
		if err != nil {
			return nil, nil, err
		}

		cc.ChainSource = chain.NewNeutrinoClient(
			cfg.ActiveNetParams.Params, cfg.NeutrinoCS,
		)

		// Get our best block as a health check.
		cc.HealthCheck = func() error {
			_, _, err := cc.ChainSource.GetBestBlock()
			return err
		}

	case "bitcoind":
		bitcoindMode := cfg.BitcoindMode

		// Otherwise, we'll be speaking directly via RPC and ZMQ to a
		// bitcoind node. If the specified host for the btcd RPC
		// server already has a port specified, then we use that
		// directly. Otherwise, we assume the default port according to
		// the selected chain parameters.
		var bitcoindHost string
		if strings.Contains(bitcoindMode.RPCHost, ":") {
			bitcoindHost = bitcoindMode.RPCHost
		} else {
			// The RPC ports specified in chainparams.go assume
			// btcd, which picks a different port so that btcwallet
			// can use the same RPC port as bitcoind. We convert
			// this back to the btcwallet/bitcoind port.
			rpcPort, err := strconv.Atoi(cfg.ActiveNetParams.RPCPort)
			if err != nil {
				return nil, nil, err
			}
			rpcPort -= 2
			bitcoindHost = fmt.Sprintf("%v:%d",
				bitcoindMode.RPCHost, rpcPort)
			if cfg.Bitcoin.RegTest || cfg.Bitcoin.SigNet {
				conn, err := net.Dial("tcp", bitcoindHost)
				if err != nil || conn == nil {
					switch {
					case cfg.Bitcoin.RegTest:
						rpcPort = 18443
					case cfg.Bitcoin.SigNet:
						rpcPort = 38332
					}
					bitcoindHost = fmt.Sprintf("%v:%d",
						bitcoindMode.RPCHost,
						rpcPort)
				} else {
					conn.Close()
				}
			}
		}

		bitcoindCfg := &chain.BitcoindConfig{
			ChainParams:        cfg.ActiveNetParams.Params,
			Host:               bitcoindHost,
			User:               bitcoindMode.RPCUser,
			Pass:               bitcoindMode.RPCPass,
			Dialer:             cfg.Dialer,
			PrunedModeMaxPeers: bitcoindMode.PrunedNodeMaxPeers,
		}

		if bitcoindMode.RPCPolling {
			bitcoindCfg.PollingConfig = &chain.PollingConfig{
				BlockPollingInterval:    bitcoindMode.BlockPollingInterval,
				TxPollingInterval:       bitcoindMode.TxPollingInterval,
				TxPollingIntervalJitter: lncfg.DefaultTxPollingJitter,
			}
		} else {
			bitcoindCfg.ZMQConfig = &chain.ZMQConfig{
				ZMQBlockHost:           bitcoindMode.ZMQPubRawBlock,
				ZMQTxHost:              bitcoindMode.ZMQPubRawTx,
				ZMQReadDeadline:        bitcoindMode.ZMQReadDeadline,
				MempoolPollingInterval: bitcoindMode.TxPollingInterval,
				PollingIntervalJitter:  lncfg.DefaultTxPollingJitter,
			}
		}

		// Establish the connection to bitcoind and create the clients
		// required for our relevant subsystems.
		bitcoindConn, err := chain.NewBitcoindConn(bitcoindCfg)
		if err != nil {
			return nil, nil, err
		}

		if err := bitcoindConn.Start(); err != nil {
			return nil, nil, fmt.Errorf("unable to connect to "+
				"bitcoind: %v", err)
		}

		chainNotifier := bitcoindnotify.New(
			bitcoindConn, cfg.ActiveNetParams.Params, hintCache,
			hintCache, cfg.BlockCache,
		)

		cc.ChainNotifier = chainNotifier
		cc.MempoolNotifier = chainNotifier

		cc.ChainView = chainview.NewBitcoindFilteredChainView(
			bitcoindConn, cfg.BlockCache,
		)
		cc.ChainSource = bitcoindConn.NewBitcoindClient()

		// Initialize config to connect to bitcoind RPC.
		rpcConfig := &rpcclient.ConnConfig{
			Host:                 bitcoindHost,
			User:                 bitcoindMode.RPCUser,
			Pass:                 bitcoindMode.RPCPass,
			DisableConnectOnNew:  true,
			DisableAutoReconnect: false,
			DisableTLS:           true,
			HTTPPostMode:         true,
		}

		// If feeurl is not provided, use bitcoind's fee estimator.
		if cfg.Fee.URL == "" {
			log.Infof("Initializing bitcoind backed fee estimator "+
				"in %s mode", bitcoindMode.EstimateMode)

			// Finally, we'll re-initialize the fee estimator, as
			// if we're using bitcoind as a backend, then we can
			// use live fee estimates, rather than a statically
			// coded value.
			fallBackFeeRate := chainfee.SatPerKVByte(25 * 1000)
			cc.FeeEstimator, err = chainfee.NewBitcoindEstimator(
				*rpcConfig, bitcoindMode.EstimateMode,
				fallBackFeeRate.FeePerKWeight(),
			)
			if err != nil {
				return nil, nil, err
			}
		}

		// We need to use some apis that are not exposed by btcwallet,
		// for a health check function so we create an ad-hoc bitcoind
		// connection.
		chainConn, err := rpcclient.New(rpcConfig, nil)
		if err != nil {
			return nil, nil, err
		}

		// Before we continue any further, we'll ensure that the
		// backend understands Taproot. If not, then all the default
		// features can't be used.
		if !backendSupportsTaproot(chainConn) {
			return nil, nil, fmt.Errorf("node backend does not " +
				"support taproot")
		}

		// The api we will use for our health check depends on the
		// bitcoind version.
		cmd, ver, err := getBitcoindHealthCheckCmd(chainConn)
		if err != nil {
			return nil, nil, err
		}

		// If the getzmqnotifications api is available (was added in
		// version 0.17.0) we make sure lnd subscribes to the correct
		// zmq events. We do this to avoid a situation in which we are
		// not notified of new transactions or blocks.
		if ver >= 170000 && !bitcoindMode.RPCPolling {
			zmqPubRawBlockURL, err := url.Parse(bitcoindMode.ZMQPubRawBlock)
			if err != nil {
				return nil, nil, err
			}
			zmqPubRawTxURL, err := url.Parse(bitcoindMode.ZMQPubRawTx)
			if err != nil {
				return nil, nil, err
			}

			// Fetch all active zmq notifications from the bitcoind client.
			resp, err := chainConn.RawRequest("getzmqnotifications", nil)
			if err != nil {
				return nil, nil, err
			}

			zmq := []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			}{}

			if err = json.Unmarshal([]byte(resp), &zmq); err != nil {
				return nil, nil, err
			}

			pubRawBlockActive := false
			pubRawTxActive := false

			for i := range zmq {
				if zmq[i].Type == "pubrawblock" {
					url, err := url.Parse(zmq[i].Address)
					if err != nil {
						return nil, nil, err
					}
					if url.Port() != zmqPubRawBlockURL.Port() {
						log.Warnf(
							"unable to subscribe to zmq block events on "+
								"%s (bitcoind is running on %s)",
							zmqPubRawBlockURL.Host,
							url.Host,
						)
					}
					pubRawBlockActive = true
				}
				if zmq[i].Type == "pubrawtx" {
					url, err := url.Parse(zmq[i].Address)
					if err != nil {
						return nil, nil, err
					}
					if url.Port() != zmqPubRawTxURL.Port() {
						log.Warnf(
							"unable to subscribe to zmq tx events on "+
								"%s (bitcoind is running on %s)",
							zmqPubRawTxURL.Host,
							url.Host,
						)
					}
					pubRawTxActive = true
				}
			}

			// Return an error if raw tx or raw block notification over
			// zmq is inactive.
			if !pubRawBlockActive {
				return nil, nil, errors.New(
					"block notification over zmq is inactive on " +
						"bitcoind",
				)
			}
			if !pubRawTxActive {
				return nil, nil, errors.New(
					"tx notification over zmq is inactive on " +
						"bitcoind",
				)
			}
		}

		cc.HealthCheck = func() error {
			_, err := chainConn.RawRequest(cmd, nil)
			if err != nil {
				return err
			}

			// On local test networks we usually don't have multiple
			// chain backend peers, so we can skip
			// the checkOutboundPeers test.
			if cfg.Bitcoin.IsLocalNetwork() {
				return nil
			}

			// Make sure the bitcoind chain backend maintains a
			// healthy connection to the network by checking the
			// number of outbound peers.
			return checkOutboundPeers(chainConn)
		}

	case "btcd":
		// Otherwise, we'll be speaking directly via RPC to a node.
		//
		// So first we'll load btcd's TLS cert for the RPC
		// connection. If a raw cert was specified in the config, then
		// we'll set that directly. Otherwise, we attempt to read the
		// cert from the path specified in the config.
		var (
			rpcCert  []byte
			btcdMode = cfg.BtcdMode
		)
		if btcdMode.RawRPCCert != "" {
			rpcCert, err = hex.DecodeString(btcdMode.RawRPCCert)
			if err != nil {
				return nil, nil, err
			}
		} else {
			certFile, err := os.Open(btcdMode.RPCCert)
			if err != nil {
				return nil, nil, err
			}
			rpcCert, err = io.ReadAll(certFile)
			if err != nil {
				return nil, nil, err
			}
			if err := certFile.Close(); err != nil {
				return nil, nil, err
			}
		}

		// If the specified host for the btcd RPC server already
		// has a port specified, then we use that directly. Otherwise,
		// we assume the default port according to the selected chain
		// parameters.
		var btcdHost string
		if strings.Contains(btcdMode.RPCHost, ":") {
			btcdHost = btcdMode.RPCHost
		} else {
			btcdHost = fmt.Sprintf("%v:%v", btcdMode.RPCHost,
				cfg.ActiveNetParams.RPCPort)
		}

		btcdUser := btcdMode.RPCUser
		btcdPass := btcdMode.RPCPass
		rpcConfig := &rpcclient.ConnConfig{
			Host:                 btcdHost,
			Endpoint:             "ws",
			User:                 btcdUser,
			Pass:                 btcdPass,
			Certificates:         rpcCert,
			DisableTLS:           false,
			DisableConnectOnNew:  true,
			DisableAutoReconnect: false,
		}

		chainNotifier, err := btcdnotify.New(
			rpcConfig, cfg.ActiveNetParams.Params, hintCache,
			hintCache, cfg.BlockCache,
		)
		if err != nil {
			return nil, nil, err
		}

		cc.ChainNotifier = chainNotifier
		cc.MempoolNotifier = chainNotifier

		// Finally, we'll create an instance of the default chain view
		// to be used within the routing layer.
		cc.ChainView, err = chainview.NewBtcdFilteredChainView(
			*rpcConfig, cfg.BlockCache,
		)
		if err != nil {
			log.Errorf("unable to create chain view: %v", err)
			return nil, nil, err
		}

		// Create a special websockets rpc client for btcd which will be
		// used by the wallet for notifications, calls, etc.
		chainRPC, err := chain.NewRPCClient(
			cfg.ActiveNetParams.Params, btcdHost, btcdUser,
			btcdPass, rpcCert, false, 20,
		)
		if err != nil {
			return nil, nil, err
		}

		// Before we continue any further, we'll ensure that the
		// backend understands Taproot. If not, then all the default
		// features can't be used.
		restConfCopy := *rpcConfig
		restConfCopy.Endpoint = ""
		restConfCopy.HTTPPostMode = true
		chainConn, err := rpcclient.New(&restConfCopy, nil)
		if err != nil {
			return nil, nil, err
		}
		if !backendSupportsTaproot(chainConn) {
			return nil, nil, fmt.Errorf("node backend does not " +
				"support taproot")
		}

		cc.ChainSource = chainRPC

		// Use a query for our best block as a health check.
		cc.HealthCheck = func() error {
			_, _, err := cc.ChainSource.GetBestBlock()
			if err != nil {
				return err
			}

			// On local test networks we usually don't have multiple
			// chain backend peers, so we can skip
			// the checkOutboundPeers test.
			if cfg.Bitcoin.IsLocalNetwork() {
				return nil
			}

			// Make sure the btcd chain backend maintains a
			// healthy connection to the network by checking the
			// number of outbound peers.
			return checkOutboundPeers(chainRPC.Client)
		}

		// If feeurl is not provided, use btcd's fee estimator.
		if cfg.Fee.URL == "" {
			log.Info("Initializing btcd backed fee estimator")

			// Finally, we'll re-initialize the fee estimator, as
			// if we're using btcd as a backend, then we can use
			// live fee estimates, rather than a statically coded
			// value.
			fallBackFeeRate := chainfee.SatPerKVByte(25 * 1000)
			cc.FeeEstimator, err = chainfee.NewBtcdEstimator(
				*rpcConfig, fallBackFeeRate.FeePerKWeight(),
			)
			if err != nil {
				return nil, nil, err
			}
		}

	case "nochainbackend":
		backend := &NoChainBackend{}
		source := &NoChainSource{
			BestBlockTime: time.Now(),
		}

		cc.ChainNotifier = backend
		cc.ChainView = backend
		cc.FeeEstimator = backend

		cc.ChainSource = source
		cc.HealthCheck = func() error {
			return nil
		}

	default:
		return nil, nil, fmt.Errorf("unknown node type: %s",
			cfg.Bitcoin.Node)
	}

	cc.BestBlockTracker =
		chainntnfs.NewBestBlockTracker(cc.ChainNotifier)

	switch {
	// If the fee URL isn't set, and the user is running mainnet, then
	// we'll return an error to instruct them to set a proper fee
	// estimator.
	case cfg.Fee.URL == "" && cfg.Bitcoin.MainNet &&
		cfg.Bitcoin.Node == "neutrino":

		return nil, nil, fmt.Errorf("--fee.url parameter required " +
			"when running neutrino on mainnet")

	// Override default fee estimator if an external service is specified.
	case cfg.Fee.URL != "":
		// Do not cache fees on regtest to make it easier to execute
		// manual or automated test cases.
		cacheFees := !cfg.Bitcoin.RegTest

		log.Infof("Using external fee estimator %v: cached=%v: "+
			"min update timeout=%v, max update timeout=%v",
			cfg.Fee.URL, cacheFees, cfg.Fee.MinUpdateTimeout,
			cfg.Fee.MaxUpdateTimeout)

		cc.FeeEstimator, err = chainfee.NewWebAPIEstimator(
			chainfee.SparseConfFeeSource{
				URL: cfg.Fee.URL,
			},
			!cacheFees,
			cfg.Fee.MinUpdateTimeout,
			cfg.Fee.MaxUpdateTimeout,
		)
		if err != nil {
			return nil, nil, err
		}
	}

	ccCleanup := func() {
		if cc.FeeEstimator != nil {
			if err := cc.FeeEstimator.Stop(); err != nil {
				log.Errorf("Failed to stop feeEstimator: %v",
					err)
			}
		}
	}

	// Start fee estimator.
	if err := cc.FeeEstimator.Start(); err != nil {
		return nil, nil, err
	}

	return cc, ccCleanup, nil
}

// NewChainControl attempts to create a ChainControl instance according
// to the parameters in the passed configuration. Currently three
// branches of ChainControl instances exist: one backed by a running btcd
// full-node, another backed by a running bitcoind full-node, and the other
// backed by a running neutrino light client instance. When running with a
// neutrino light client instance, `neutrinoCS` must be non-nil.
func NewChainControl(walletConfig lnwallet.Config,
	msgSigner lnwallet.MessageSigner,
	pcc *PartialChainControl) (*ChainControl, func(), error) {

	cc := &ChainControl{
		PartialChainControl: pcc,
		MsgSigner:           msgSigner,
		Signer:              walletConfig.Signer,
		ChainIO:             walletConfig.ChainIO,
		Wc:                  walletConfig.WalletController,
		KeyRing:             walletConfig.SecretKeyRing,
	}

	ccCleanup := func() {
		if cc.Wallet != nil {
			if err := cc.Wallet.Shutdown(); err != nil {
				log.Errorf("Failed to shutdown wallet: %v", err)
			}
		}
	}

	lnWallet, err := lnwallet.NewLightningWallet(walletConfig)
	if err != nil {
		return nil, ccCleanup, fmt.Errorf("unable to create wallet: %w",
			err)
	}
	if err := lnWallet.Startup(); err != nil {
		return nil, ccCleanup, fmt.Errorf("unable to create wallet: %w",
			err)
	}

	log.Info("LightningWallet opened")
	cc.Wallet = lnWallet

	return cc, ccCleanup, nil
}

// getBitcoindHealthCheckCmd queries bitcoind for its version to decide which
// api we should use for our health check. We prefer to use the uptime
// command, because it has no locking and is an inexpensive call, which was
// added in version 0.15. If we are on an earlier version, we fallback to using
// getblockchaininfo.
func getBitcoindHealthCheckCmd(client *rpcclient.Client) (string, int64, error) {
	// Query bitcoind to get our current version.
	resp, err := client.RawRequest("getnetworkinfo", nil)
	if err != nil {
		return "", 0, err
	}

	// Parse the response to retrieve bitcoind's version.
	info := struct {
		Version int64 `json:"version"`
	}{}
	if err := json.Unmarshal(resp, &info); err != nil {
		return "", 0, err
	}

	// Bitcoind returns a single value representing the semantic version:
	// 1000000 * CLIENT_VERSION_MAJOR + 10000 * CLIENT_VERSION_MINOR
	// + 100 * CLIENT_VERSION_REVISION + 1 * CLIENT_VERSION_BUILD
	//
	// The uptime call was added in version 0.15.0, so we return it for
	// any version value >= 150000, as per the above calculation.
	if info.Version >= 150000 {
		return "uptime", info.Version, nil
	}

	return "getblockchaininfo", info.Version, nil
}

var (
	// BitcoinTestnetGenesis is the genesis hash of Bitcoin's testnet
	// chain.
	BitcoinTestnetGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0x43, 0x49, 0x7f, 0xd7, 0xf8, 0x26, 0x95, 0x71,
		0x08, 0xf4, 0xa3, 0x0f, 0xd9, 0xce, 0xc3, 0xae,
		0xba, 0x79, 0x97, 0x20, 0x84, 0xe9, 0x0e, 0xad,
		0x01, 0xea, 0x33, 0x09, 0x00, 0x00, 0x00, 0x00,
	})

	// BitcoinTestnet4Genesis is the genesis hash of Bitcoin's testnet4
	// chain.
	BitcoinTestnet4Genesis = chainhash.Hash([chainhash.HashSize]byte{
		0x43, 0xf0, 0x8b, 0xda, 0xb0, 0x50, 0xe3, 0x5b,
		0x56, 0x7c, 0x86, 0x4b, 0x91, 0xf4, 0x7f, 0x50,
		0xae, 0x72, 0x5a, 0xe2, 0xde, 0x53, 0xbc, 0xfb,
		0xba, 0xf2, 0x84, 0xda, 0x00, 0x00, 0x00, 0x00,
	})

	// BitcoinSignetGenesis is the genesis hash of Bitcoin's signet chain.
	BitcoinSignetGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0xf6, 0x1e, 0xee, 0x3b, 0x63, 0xa3, 0x80, 0xa4,
		0x77, 0xa0, 0x63, 0xaf, 0x32, 0xb2, 0xbb, 0xc9,
		0x7c, 0x9f, 0xf9, 0xf0, 0x1f, 0x2c, 0x42, 0x25,
		0xe9, 0x73, 0x98, 0x81, 0x08, 0x00, 0x00, 0x00,
	})

	// BitcoinMainnetGenesis is the genesis hash of Bitcoin's main chain.
	BitcoinMainnetGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0x6f, 0xe2, 0x8c, 0x0a, 0xb6, 0xf1, 0xb3, 0x72,
		0xc1, 0xa6, 0xa2, 0x46, 0xae, 0x63, 0xf7, 0x4f,
		0x93, 0x1e, 0x83, 0x65, 0xe1, 0x5a, 0x08, 0x9c,
		0x68, 0xd6, 0x19, 0x00, 0x00, 0x00, 0x00, 0x00,
	})

	// ChainDNSSeeds is a map of a chain's hash to the set of DNS seeds
	// that will be use to bootstrap peers upon first startup.
	//
	// The first item in the array is the primary host we'll use to attempt
	// the SRV lookup we require. If we're unable to receive a response
	// over UDP, then we'll fall back to manual TCP resolution. The second
	// item in the array is a special A record that we'll query in order to
	// receive the IP address of the current authoritative DNS server for
	// the network seed.
	//
	// TODO(roasbeef): extend and collapse these and chainparams.go into
	// struct like chaincfg.Params.
	ChainDNSSeeds = map[chainhash.Hash][][2]string{
		BitcoinMainnetGenesis: {
			{
				"nodes.lightning.wiki",
				"soa.nodes.lightning.wiki",
			},
			{
				"lseed.bitcoinstats.com",
			},
		},

		BitcoinTestnetGenesis: {
			{
				"test.nodes.lightning.wiki",
				"soa.nodes.lightning.wiki",
			},
		},

		BitcoinTestnet4Genesis: {
			{
				"test4.nodes.lightning.wiki",
				"soa.nodes.lightning.wiki",
			},
		},

		BitcoinSignetGenesis: {
			{
				"signet.nodes.lightning.wiki",
				"soa.nodes.lightning.wiki",
			},
		},
	}
)

// checkOutboundPeers checks the number of outbound peers connected to the
// provided RPC client. If the number of outbound peers is below 6, a warning
// is logged. This function is intended to ensure that the chain backend
// maintains a healthy connection to the network.
func checkOutboundPeers(client *rpcclient.Client) error {
	peers, err := client.GetPeerInfo()
	if err != nil {
		return err
	}

	var outboundPeers int
	for _, peer := range peers {
		if !peer.Inbound {
			outboundPeers++
		}
	}

	if outboundPeers < DefaultMinOutboundPeers {
		log.Warnf("The chain backend has an insufficient number "+
			"of connected outbound peers (%d connected, expected "+
			"minimum is %d) which can be a security issue. "+
			"Connect to more trusted nodes manually if necessary.",
			outboundPeers, DefaultMinOutboundPeers)
	}

	return nil
}

package chainreg

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
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
	"github.com/lightningnetwork/lnd/htlcswitch"
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

	// Litecoin defines settings for the Litecoin chain.
	Litecoin *lncfg.Chain

	// PrimaryChain is a function that returns our primary chain via its
	// ChainCode.
	PrimaryChain func() ChainCode

	// HeightHintCacheQueryDisable is a boolean that disables height hint
	// queries if true.
	HeightHintCacheQueryDisable bool

	// NeutrinoMode defines settings for connecting to a neutrino
	// light-client.
	NeutrinoMode *lncfg.Neutrino

	// BitcoindMode defines settings for connecting to a bitcoind node.
	BitcoindMode *lncfg.Bitcoind

	// LitecoindMode defines settings for connecting to a litecoind node.
	LitecoindMode *lncfg.Bitcoind

	// BtcdMode defines settings for connecting to a btcd node.
	BtcdMode *lncfg.Btcd

	// LtcdMode defines settings for connecting to an ltcd node.
	LtcdMode *lncfg.Btcd

	// HeightHintDB is a pointer to the database that stores the height
	// hints.
	HeightHintDB kvdb.Backend

	// ChanStateDB is a pointer to the database that stores the channel
	// state.
	ChanStateDB *channeldb.ChannelStateDB

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

	// FeeURL defines the URL for fee estimation we will use. This field is
	// optional.
	FeeURL string

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

	DefaultLitecoinMinHTLCInMSat  = lnwire.MilliSatoshi(1)
	DefaultLitecoinMinHTLCOutMSat = lnwire.MilliSatoshi(1000)
	DefaultLitecoinBaseFeeMSat    = lnwire.MilliSatoshi(1000)
	DefaultLitecoinFeeRate        = lnwire.MilliSatoshi(1)
	DefaultLitecoinTimeLockDelta  = 576
	DefaultLitecoinDustLimit      = btcutil.Amount(54600)

	// DefaultBitcoinStaticFeePerKW is the fee rate of 50 sat/vbyte
	// expressed in sat/kw.
	DefaultBitcoinStaticFeePerKW = chainfee.SatPerKWeight(12500)

	// DefaultBitcoinStaticMinRelayFeeRate is the min relay fee used for
	// static estimators.
	DefaultBitcoinStaticMinRelayFeeRate = chainfee.FeePerKwFloor

	// DefaultLitecoinStaticFeePerKW is the fee rate of 200 sat/vbyte
	// expressed in sat/kw.
	DefaultLitecoinStaticFeePerKW = chainfee.SatPerKWeight(50000)

	// BtcToLtcConversionRate is a fixed ratio used in order to scale up
	// payments when running on the Litecoin chain.
	BtcToLtcConversionRate = 60
)

// DefaultLtcChannelConstraints is the default set of channel constraints that
// are meant to be used when initially funding a Litecoin channel.
var DefaultLtcChannelConstraints = channeldb.ChannelConstraints{
	DustLimit:        DefaultLitecoinDustLimit,
	MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
}

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
	RoutingPolicy htlcswitch.ForwardingPolicy

	// MinHtlcIn is the minimum HTLC we will accept.
	MinHtlcIn lnwire.MilliSatoshi

	// ChannelConstraints is the set of default constraints that will be
	// used for any incoming or outgoing channel reservation requests.
	ChannelConstraints channeldb.ChannelConstraints
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

// GenDefaultBtcConstraints generates the default set of channel constraints
// that are to be used when funding a Bitcoin channel.
func GenDefaultBtcConstraints() channeldb.ChannelConstraints {
	// We use the dust limit for the maximally sized witness program with
	// a 40-byte data push.
	dustLimit := lnwallet.DustLimitForSize(input.UnknownWitnessSize)

	return channeldb.ChannelConstraints{
		DustLimit:        dustLimit,
		MaxAcceptedHtlcs: input.MaxHTLCNumber / 2,
	}
}

// NewPartialChainControl creates a new partial chain control that contains all
// the parts that can be purely constructed from the passed in global
// configuration and doesn't need any wallet instance yet.
//
//nolint:lll
func NewPartialChainControl(cfg *Config) (*PartialChainControl, func(), error) {
	// Set the RPC config from the "home" chain. Multi-chain isn't yet
	// active, so we'll restrict usage to a particular chain for now.
	homeChainConfig := cfg.Bitcoin
	if cfg.PrimaryChain() == LitecoinChain {
		homeChainConfig = cfg.Litecoin
	}
	log.Infof("Primary chain is set to: %v", cfg.PrimaryChain())

	cc := &PartialChainControl{
		Cfg: cfg,
	}

	switch cfg.PrimaryChain() {
	case BitcoinChain:
		cc.RoutingPolicy = htlcswitch.ForwardingPolicy{
			MinHTLCOut:    cfg.Bitcoin.MinHTLCOut,
			BaseFee:       cfg.Bitcoin.BaseFee,
			FeeRate:       cfg.Bitcoin.FeeRate,
			TimeLockDelta: cfg.Bitcoin.TimeLockDelta,
		}
		cc.MinHtlcIn = cfg.Bitcoin.MinHTLCIn
		cc.FeeEstimator = chainfee.NewStaticEstimator(
			DefaultBitcoinStaticFeePerKW,
			DefaultBitcoinStaticMinRelayFeeRate,
		)
	case LitecoinChain:
		cc.RoutingPolicy = htlcswitch.ForwardingPolicy{
			MinHTLCOut:    cfg.Litecoin.MinHTLCOut,
			BaseFee:       cfg.Litecoin.BaseFee,
			FeeRate:       cfg.Litecoin.FeeRate,
			TimeLockDelta: cfg.Litecoin.TimeLockDelta,
		}
		cc.MinHtlcIn = cfg.Litecoin.MinHTLCIn
		cc.FeeEstimator = chainfee.NewStaticEstimator(
			DefaultLitecoinStaticFeePerKW, 0,
		)
	default:
		return nil, nil, fmt.Errorf("default routing policy for chain "+
			"%v is unknown", cfg.PrimaryChain())
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

	// If spv mode is active, then we'll be using a distinct set of
	// chainControl interfaces that interface directly with the p2p network
	// of the selected chain.
	switch homeChainConfig.Node {
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

		// Map the deprecated neutrino feeurl flag to the general fee
		// url.
		if cfg.NeutrinoMode.FeeURL != "" {
			if cfg.FeeURL != "" {
				return nil, nil, errors.New("feeurl and " +
					"neutrino.feeurl are mutually " +
					"exclusive")
			}

			cfg.FeeURL = cfg.NeutrinoMode.FeeURL
		}

		cc.ChainSource = chain.NewNeutrinoClient(
			cfg.ActiveNetParams.Params, cfg.NeutrinoCS,
		)

		// Get our best block as a health check.
		cc.HealthCheck = func() error {
			_, _, err := cc.ChainSource.GetBestBlock()
			return err
		}

	case "bitcoind", "litecoind":
		var bitcoindMode *lncfg.Bitcoind
		switch {
		case cfg.Bitcoin.Active:
			bitcoindMode = cfg.BitcoindMode
		case cfg.Litecoin.Active:
			bitcoindMode = cfg.LitecoindMode
		}
		// Otherwise, we'll be speaking directly via RPC and ZMQ to a
		// bitcoind node. If the specified host for the btcd/ltcd RPC
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
			if (cfg.Bitcoin.Active &&
				(cfg.Bitcoin.RegTest || cfg.Bitcoin.SigNet)) ||
				(cfg.Litecoin.Active && cfg.Litecoin.RegTest) {

				conn, err := net.Dial("tcp", bitcoindHost)
				if err != nil || conn == nil {
					switch {
					case cfg.Bitcoin.Active && cfg.Bitcoin.RegTest:
						rpcPort = 18443
					case cfg.Litecoin.Active && cfg.Litecoin.RegTest:
						rpcPort = 19443
					case cfg.Bitcoin.Active && cfg.Bitcoin.SigNet:
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

		// If we're not in regtest mode, then we'll attempt to use a
		// proper fee estimator for testnet.
		rpcConfig := &rpcclient.ConnConfig{
			Host:                 bitcoindHost,
			User:                 bitcoindMode.RPCUser,
			Pass:                 bitcoindMode.RPCPass,
			DisableConnectOnNew:  true,
			DisableAutoReconnect: false,
			DisableTLS:           true,
			HTTPPostMode:         true,
		}
		if cfg.Bitcoin.Active && !cfg.Bitcoin.RegTest {
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
		} else if cfg.Litecoin.Active && !cfg.Litecoin.RegTest {
			log.Infof("Initializing litecoind backed fee "+
				"estimator in %s mode",
				bitcoindMode.EstimateMode)

			// Finally, we'll re-initialize the fee estimator, as
			// if we're using litecoind as a backend, then we can
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
			return err
		}

	case "btcd", "ltcd":
		// Otherwise, we'll be speaking directly via RPC to a node.
		//
		// So first we'll load btcd/ltcd's TLS cert for the RPC
		// connection. If a raw cert was specified in the config, then
		// we'll set that directly. Otherwise, we attempt to read the
		// cert from the path specified in the config.
		var btcdMode *lncfg.Btcd
		switch {
		case cfg.Bitcoin.Active:
			btcdMode = cfg.BtcdMode
		case cfg.Litecoin.Active:
			btcdMode = cfg.LtcdMode
		}
		var rpcCert []byte
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
			rpcCert, err = ioutil.ReadAll(certFile)
			if err != nil {
				return nil, nil, err
			}
			if err := certFile.Close(); err != nil {
				return nil, nil, err
			}
		}

		// If the specified host for the btcd/ltcd RPC server already
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
			return err
		}

		// If we're not in simnet or regtest mode, then we'll attempt
		// to use a proper fee estimator for testnet.
		if !cfg.Bitcoin.SimNet && !cfg.Litecoin.SimNet &&
			!cfg.Bitcoin.RegTest && !cfg.Litecoin.RegTest {

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
			homeChainConfig.Node)
	}

	switch {
	// If the fee URL isn't set, and the user is running mainnet, then
	// we'll return an error to instruct them to set a proper fee
	// estimator.
	case cfg.FeeURL == "" && cfg.Bitcoin.MainNet &&
		homeChainConfig.Node == "neutrino":

		return nil, nil, fmt.Errorf("--feeurl parameter required " +
			"when running neutrino on mainnet")

	// Override default fee estimator if an external service is specified.
	case cfg.FeeURL != "":
		// Do not cache fees on regtest to make it easier to execute
		// manual or automated test cases.
		cacheFees := !cfg.Bitcoin.RegTest

		log.Infof("Using external fee estimator %v: cached=%v",
			cfg.FeeURL, cacheFees)

		cc.FeeEstimator = chainfee.NewWebAPIEstimator(
			chainfee.SparseConfFeeSource{
				URL: cfg.FeeURL,
			},
			!cacheFees,
		)
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

	// Select the default channel constraints for the primary chain.
	cc.ChannelConstraints = GenDefaultBtcConstraints()
	if cfg.PrimaryChain() == LitecoinChain {
		cc.ChannelConstraints = DefaultLtcChannelConstraints
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
		return nil, ccCleanup, fmt.Errorf("unable to create wallet: %v",
			err)
	}
	if err := lnWallet.Startup(); err != nil {
		return nil, ccCleanup, fmt.Errorf("unable to create wallet: %v",
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

	// LitecoinTestnetGenesis is the genesis hash of Litecoin's testnet4
	// chain.
	LitecoinTestnetGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0xa0, 0x29, 0x3e, 0x4e, 0xeb, 0x3d, 0xa6, 0xe6,
		0xf5, 0x6f, 0x81, 0xed, 0x59, 0x5f, 0x57, 0x88,
		0x0d, 0x1a, 0x21, 0x56, 0x9e, 0x13, 0xee, 0xfd,
		0xd9, 0x51, 0x28, 0x4b, 0x5a, 0x62, 0x66, 0x49,
	})

	// LitecoinMainnetGenesis is the genesis hash of Litecoin's main chain.
	LitecoinMainnetGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0xe2, 0xbf, 0x04, 0x7e, 0x7e, 0x5a, 0x19, 0x1a,
		0xa4, 0xef, 0x34, 0xd3, 0x14, 0x97, 0x9d, 0xc9,
		0x98, 0x6e, 0x0f, 0x19, 0x25, 0x1e, 0xda, 0xba,
		0x59, 0x40, 0xfd, 0x1f, 0xe3, 0x65, 0xa7, 0x12,
	})

	// chainMap is a simple index that maps a chain's genesis hash to the
	// ChainCode enum for that chain.
	chainMap = map[chainhash.Hash]ChainCode{
		BitcoinTestnetGenesis:  BitcoinChain,
		LitecoinTestnetGenesis: LitecoinChain,

		BitcoinMainnetGenesis:  BitcoinChain,
		LitecoinMainnetGenesis: LitecoinChain,
	}

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
				"nodes.lightning.directory",
				"soa.nodes.lightning.directory",
			},
			{
				"lseed.bitcoinstats.com",
			},
		},

		BitcoinTestnetGenesis: {
			{
				"test.nodes.lightning.directory",
				"soa.nodes.lightning.directory",
			},
		},

		BitcoinSignetGenesis: {
			{
				"ln.signet.secp.tech",
			},
		},

		LitecoinMainnetGenesis: {
			{
				"ltc.nodes.lightning.directory",
				"soa.nodes.lightning.directory",
			},
		},
	}
)

// ChainRegistry keeps track of the current chains.
type ChainRegistry struct {
	sync.RWMutex

	activeChains map[ChainCode]*ChainControl
	netParams    map[ChainCode]*BitcoinNetParams

	primaryChain ChainCode
}

// NewChainRegistry creates a new ChainRegistry.
func NewChainRegistry() *ChainRegistry {
	return &ChainRegistry{
		activeChains: make(map[ChainCode]*ChainControl),
		netParams:    make(map[ChainCode]*BitcoinNetParams),
	}
}

// RegisterChain assigns an active ChainControl instance to a target chain
// identified by its ChainCode.
func (c *ChainRegistry) RegisterChain(newChain ChainCode,
	cc *ChainControl) {

	c.Lock()
	c.activeChains[newChain] = cc
	c.Unlock()
}

// LookupChain attempts to lookup an active ChainControl instance for the
// target chain.
func (c *ChainRegistry) LookupChain(targetChain ChainCode) (
	*ChainControl, bool) {

	c.RLock()
	cc, ok := c.activeChains[targetChain]
	c.RUnlock()
	return cc, ok
}

// LookupChainByHash attempts to look up an active ChainControl which
// corresponds to the passed genesis hash.
func (c *ChainRegistry) LookupChainByHash(
	chainHash chainhash.Hash) (*ChainControl, bool) {

	c.RLock()
	defer c.RUnlock()

	targetChain, ok := chainMap[chainHash]
	if !ok {
		return nil, ok
	}

	cc, ok := c.activeChains[targetChain]
	return cc, ok
}

// RegisterPrimaryChain sets a target chain as the "home chain" for lnd.
func (c *ChainRegistry) RegisterPrimaryChain(cc ChainCode) {
	c.Lock()
	defer c.Unlock()

	c.primaryChain = cc
}

// PrimaryChain returns the primary chain for this running lnd instance. The
// primary chain is considered the "home base" while the other registered
// chains are treated as secondary chains.
func (c *ChainRegistry) PrimaryChain() ChainCode {
	c.RLock()
	defer c.RUnlock()

	return c.primaryChain
}

// ActiveChains returns a slice containing the active chains.
func (c *ChainRegistry) ActiveChains() []ChainCode {
	c.RLock()
	defer c.RUnlock()

	chains := make([]ChainCode, 0, len(c.activeChains))
	for activeChain := range c.activeChains {
		chains = append(chains, activeChain)
	}

	return chains
}

// NumActiveChains returns the total number of active chains.
func (c *ChainRegistry) NumActiveChains() uint32 {
	c.RLock()
	defer c.RUnlock()

	return uint32(len(c.activeChains))
}

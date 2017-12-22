package main

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainntnfs/bitcoindnotify"
	"github.com/lightningnetwork/lnd/chainntnfs/btcdnotify"
	"github.com/lightningnetwork/lnd/chainntnfs/neutrinonotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/chain"
	"github.com/roasbeef/btcwallet/walletdb"
)

// defaultChannelConstraints is the default set of channel constraints that are
// meant to be used when initially funding a channel.
//
// TODO(roasbeef): have one for both chains
// TODO(halseth): make configurable at startup?
var defaultChannelConstraints = channeldb.ChannelConstraints{
	DustLimit:        lnwallet.DefaultDustLimit(),
	MaxAcceptedHtlcs: lnwallet.MaxHTLCNumber / 2,
}

// chainCode is an enum-like structure for keeping track of the chains
// currently supported within lnd.
type chainCode uint32

const (
	// bitcoinChain is Bitcoin's testnet chain.
	bitcoinChain chainCode = iota

	// litecoinChain is Litecoin's testnet chain.
	litecoinChain
)

// String returns a string representation of the target chainCode.
func (c chainCode) String() string {
	switch c {
	case bitcoinChain:
		return "bitcoin"
	case litecoinChain:
		return "litecoin"
	default:
		return "kekcoin"
	}
}

// chainControl couples the three primary interfaces lnd utilizes for a
// particular chain together. A single chainControl instance will exist for all
// the chains lnd is currently active on.
type chainControl struct {
	chainIO lnwallet.BlockChainIO

	feeEstimator lnwallet.FeeEstimator

	signer lnwallet.Signer

	msgSigner lnwallet.MessageSigner

	chainNotifier chainntnfs.ChainNotifier

	chainView chainview.FilteredChainView

	wallet *lnwallet.LightningWallet

	routingPolicy htlcswitch.ForwardingPolicy
}

// newChainControlFromConfig attempts to create a chainControl instance
// according to the parameters in the passed lnd configuration. Currently two
// branches of chainControl instances exist: one backed by a running btcd
// full-node, and the other backed by a running neutrino light client instance.
func newChainControlFromConfig(cfg *config, chanDB *channeldb.DB,
	privateWalletPw, publicWalletPw []byte) (*chainControl, func(), error) {

	// Set the RPC config from the "home" chain. Multi-chain isn't yet
	// active, so we'll restrict usage to a particular chain for now.
	homeChainConfig := cfg.Bitcoin
	if registeredChains.PrimaryChain() == litecoinChain {
		homeChainConfig = cfg.Litecoin
	}
	ltndLog.Infof("Primary chain is set to: %v",
		registeredChains.PrimaryChain())

	cc := &chainControl{}

	switch registeredChains.PrimaryChain() {
	case bitcoinChain:
		cc.routingPolicy = htlcswitch.ForwardingPolicy{
			MinHTLC:       cfg.Bitcoin.MinHTLC,
			BaseFee:       cfg.Bitcoin.BaseFee,
			FeeRate:       cfg.Bitcoin.FeeRate,
			TimeLockDelta: cfg.Bitcoin.TimeLockDelta,
		}
		cc.feeEstimator = lnwallet.StaticFeeEstimator{
			FeeRate: 50,
		}
	case litecoinChain:
		cc.routingPolicy = htlcswitch.ForwardingPolicy{
			MinHTLC:       cfg.Litecoin.MinHTLC,
			BaseFee:       cfg.Litecoin.BaseFee,
			FeeRate:       cfg.Litecoin.FeeRate,
			TimeLockDelta: cfg.Litecoin.TimeLockDelta,
		}
		cc.feeEstimator = lnwallet.StaticFeeEstimator{
			FeeRate: 100,
		}
	default:
		return nil, nil, fmt.Errorf("Default routing policy for "+
			"chain %v is unknown", registeredChains.PrimaryChain())
	}

	walletConfig := &btcwallet.Config{
		PrivatePass:  privateWalletPw,
		PublicPass:   publicWalletPw,
		DataDir:      homeChainConfig.ChainDir,
		NetParams:    activeNetParams.Params,
		FeeEstimator: cc.feeEstimator,
	}

	var (
		err          error
		cleanUp      func()
		btcdConn     *chain.RPCClient
		bitcoindConn *chain.BitcoindClient
	)

	// If spv mode is active, then we'll be using a distinct set of
	// chainControl interfaces that interface directly with the p2p network
	// of the selected chain.
	switch homeChainConfig.Node {
	case "neutrino":
		// First we'll open the database file for neutrino, creating
		// the database if needed.
		dbName := filepath.Join(cfg.DataDir, "neutrino.db")
		nodeDatabase, err := walletdb.Create("bdb", dbName)
		if err != nil {
			return nil, nil, err
		}

		// With the database open, we can now create an instance of the
		// neutrino light client. We pass in relevant configuration
		// parameters required.
		config := neutrino.Config{
			DataDir:      cfg.DataDir,
			Database:     nodeDatabase,
			ChainParams:  *activeNetParams.Params,
			AddPeers:     cfg.NeutrinoMode.AddPeers,
			ConnectPeers: cfg.NeutrinoMode.ConnectPeers,
		}
		neutrino.WaitForMoreCFHeaders = time.Second * 1
		neutrino.MaxPeers = 8
		neutrino.BanDuration = 5 * time.Second
		svc, err := neutrino.NewChainService(config)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to create neutrino: %v", err)
		}
		svc.Start()

		// Next we'll create the instances of the ChainNotifier and
		// FilteredChainView interface which is backed by the neutrino
		// light client.
		cc.chainNotifier, err = neutrinonotify.New(svc)
		if err != nil {
			return nil, nil, err
		}
		cc.chainView, err = chainview.NewCfFilteredChainView(svc)
		if err != nil {
			return nil, nil, err
		}

		// Finally, we'll set the chain source for btcwallet, and
		// create our clean up function which simply closes the
		// database.
		walletConfig.ChainSource = chain.NewNeutrinoClient(svc)
		cleanUp = func() {
			defer nodeDatabase.Close()
		}
	case "bitcoind":
		// Otherwise, we'll be speaking directly via RPC and ZMQ to a
		// bitcoind node. If the specified host for the btcd/ltcd RPC
		// server already has a port specified, then we use that
		// directly. Otherwise, we assume the default port according to
		// the selected chain parameters.
		var bitcoindHost string
		if strings.Contains(cfg.BitcoindMode.RPCHost, ":") {
			bitcoindHost = cfg.BitcoindMode.RPCHost
		} else {
			// The RPC ports specified in chainparams.go assume
			// btcd, which picks a different port so that btcwallet
			// can use the same RPC port as bitcoind. We convert
			// this back to the btcwallet/bitcoind port.
			rpcPort, err := strconv.Atoi(activeNetParams.rpcPort)
			if err != nil {
				return nil, nil, err
			}
			rpcPort -= 2
			bitcoindHost = fmt.Sprintf("%v:%d",
				cfg.BitcoindMode.RPCHost, rpcPort)
			if cfg.Bitcoin.RegTest {
				conn, err := net.Dial("tcp", bitcoindHost)
				if err != nil || conn == nil {
					rpcPort = 18443
					bitcoindHost = fmt.Sprintf("%v:%d",
						cfg.BitcoindMode.RPCHost,
						rpcPort)
				} else {
					conn.Close()
				}
			}
		}

		bitcoindUser := cfg.BitcoindMode.RPCUser
		bitcoindPass := cfg.BitcoindMode.RPCPass
		rpcConfig := &rpcclient.ConnConfig{
			Host:                 bitcoindHost,
			User:                 bitcoindUser,
			Pass:                 bitcoindPass,
			DisableConnectOnNew:  true,
			DisableAutoReconnect: false,
			DisableTLS:           true,
			HTTPPostMode:         true,
		}
		cc.chainNotifier, err = bitcoindnotify.New(rpcConfig,
			cfg.BitcoindMode.ZMQPath, *activeNetParams.Params)
		if err != nil {
			return nil, nil, err
		}

		// Next, we'll create an instance of the bitcoind chain view to
		// be used within the routing layer.
		cc.chainView, err = chainview.NewBitcoindFilteredChainView(
			*rpcConfig, cfg.BitcoindMode.ZMQPath,
			*activeNetParams.Params)
		if err != nil {
			srvrLog.Errorf("unable to create chain view: %v", err)
			return nil, nil, err
		}

		// Create a special rpc+ZMQ client for bitcoind which will be
		// used by the wallet for notifications, calls, etc.
		bitcoindConn, err = chain.NewBitcoindClient(
			activeNetParams.Params, bitcoindHost, bitcoindUser,
			bitcoindPass, cfg.BitcoindMode.ZMQPath,
			time.Millisecond*100)
		if err != nil {
			return nil, nil, err
		}

		walletConfig.ChainSource = bitcoindConn

		// If we're not in regtest mode, then we'll attempt to use a
		// proper fee estimator for testnet.
		if !cfg.Bitcoin.RegTest {
			ltndLog.Infof("Initializing bitcoind backed fee estimator")

			// Finally, we'll re-initialize the fee estimator, as
			// if we're using bitcoind as a backend, then we can
			// use live fee estimates, rather than a statically
			// coded value.
			fallBackFeeRate := btcutil.Amount(25)
			cc.feeEstimator, err = lnwallet.NewBitcoindFeeEstimator(
				*rpcConfig, fallBackFeeRate,
			)
			if err != nil {
				return nil, nil, err
			}
			if err := cc.feeEstimator.Start(); err != nil {
				return nil, nil, err
			}
		}
	case "btcd":
		// Otherwise, we'll be speaking directly via RPC to a node.
		//
		// So first we'll load btcd/ltcd's TLS cert for the RPC
		// connection. If a raw cert was specified in the config, then
		// we'll set that directly. Otherwise, we attempt to read the
		// cert from the path specified in the config.
		var btcdMode *btcdConfig
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
				activeNetParams.rpcPort)
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
		cc.chainNotifier, err = btcdnotify.New(rpcConfig)
		if err != nil {
			return nil, nil, err
		}

		// Finally, we'll create an instance of the default chain view to be
		// used within the routing layer.
		cc.chainView, err = chainview.NewBtcdFilteredChainView(*rpcConfig)
		if err != nil {
			srvrLog.Errorf("unable to create chain view: %v", err)
			return nil, nil, err
		}

		// Create a special websockets rpc client for btcd which will be used
		// by the wallet for notifications, calls, etc.
		chainRPC, err := chain.NewRPCClient(activeNetParams.Params, btcdHost,
			btcdUser, btcdPass, rpcCert, false, 20)
		if err != nil {
			return nil, nil, err
		}

		walletConfig.ChainSource = chainRPC
		btcdConn = chainRPC

		// If we're not in simnet or regtest mode, then we'll attempt
		// to use a proper fee estimator for testnet.
		if !cfg.Bitcoin.SimNet && !cfg.Litecoin.SimNet &&
			!cfg.Bitcoin.RegTest && !cfg.Litecoin.RegTest {

			ltndLog.Infof("Initializing btcd backed fee estimator")

			// Finally, we'll re-initialize the fee estimator, as
			// if we're using btcd as a backend, then we can use
			// live fee estimates, rather than a statically coded
			// value.
			fallBackFeeRate := btcutil.Amount(25)
			cc.feeEstimator, err = lnwallet.NewBtcdFeeEstimator(
				*rpcConfig, fallBackFeeRate,
			)
			if err != nil {
				return nil, nil, err
			}
			if err := cc.feeEstimator.Start(); err != nil {
				return nil, nil, err
			}
		}
	default:
		return nil, nil, fmt.Errorf("unknown node type: %s",
			homeChainConfig.Node)
	}

	wc, err := btcwallet.New(*walletConfig)
	if err != nil {
		fmt.Printf("unable to create wallet controller: %v\n", err)
		return nil, nil, err
	}

	cc.msgSigner = wc
	cc.signer = wc
	cc.chainIO = wc

	// Create, and start the lnwallet, which handles the core payment
	// channel logic, and exposes control via proxy state machines.
	walletCfg := lnwallet.Config{
		Database:           chanDB,
		Notifier:           cc.chainNotifier,
		WalletController:   wc,
		Signer:             cc.signer,
		FeeEstimator:       cc.feeEstimator,
		ChainIO:            cc.chainIO,
		DefaultConstraints: defaultChannelConstraints,
		NetParams:          *activeNetParams.Params,
	}
	wallet, err := lnwallet.NewLightningWallet(walletCfg)
	if err != nil {
		fmt.Printf("unable to create wallet: %v\n", err)
		return nil, nil, err
	}
	if err := wallet.Startup(); err != nil {
		fmt.Printf("unable to start wallet: %v\n", err)
		return nil, nil, err
	}

	ltndLog.Info("LightningWallet opened")

	cc.wallet = wallet

	// As a final check, if we're using the RPC backend, we'll ensure that
	// the btcd node has the txindex set. Atm, this is required in order to
	// properly perform historical confirmation+spend dispatches.
	if homeChainConfig.Node != "neutrino" {
		// In order to check to see if we have the txindex up to date
		// and active, we'll try to fetch the first transaction in the
		// latest block via the index. If this doesn't succeed, then we
		// know it isn't active (or just not yet up to date).
		bestHash, _, err := cc.chainIO.GetBestBlock()
		if err != nil {
			return nil, nil, fmt.Errorf("unable to get current "+
				"best hash: %v", err)
		}
		bestBlock, err := cc.chainIO.GetBlock(bestHash)
		if err != nil {
			return nil, nil, fmt.Errorf("unable to get current "+
				"block hash: %v", err)
		}

		firstTxHash := bestBlock.Transactions[0].TxHash()
		switch homeChainConfig.Node {
		case "btcd":
			_, err = btcdConn.GetRawTransaction(&firstTxHash)
		case "bitcoind":
			_, err = bitcoindConn.GetRawTransactionVerbose(&firstTxHash)
		}
		if err != nil {
			// If the node doesn't have the txindex set, then we'll
			// halt startup, as we can't proceed in this state.
			return nil, nil, fmt.Errorf("%s detected to not "+
				"have --txindex active, cannot proceed",
				homeChainConfig.Node)
		}
	}

	return cc, cleanUp, nil
}

var (
	// bitcoinGenesis is the genesis hash of Bitcoin's testnet chain.
	bitcoinGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0x43, 0x49, 0x7f, 0xd7, 0xf8, 0x26, 0x95, 0x71,
		0x08, 0xf4, 0xa3, 0x0f, 0xd9, 0xce, 0xc3, 0xae,
		0xba, 0x79, 0x97, 0x20, 0x84, 0xe9, 0x0e, 0xad,
		0x01, 0xea, 0x33, 0x09, 0x00, 0x00, 0x00, 0x00,
	})

	// litecoinGenesis is the genesis hash of Litecoin's testnet4 chain.
	litecoinGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0xa0, 0x29, 0x3e, 0x4e, 0xeb, 0x3d, 0xa6, 0xe6,
		0xf5, 0x6f, 0x81, 0xed, 0x59, 0x5f, 0x57, 0x88,
		0x0d, 0x1a, 0x21, 0x56, 0x9e, 0x13, 0xee, 0xfd,
		0xd9, 0x51, 0x28, 0x4b, 0x5a, 0x62, 0x66, 0x49,
	})

	// chainMap is a simple index that maps a chain's genesis hash to the
	// chainCode enum for that chain.
	chainMap = map[chainhash.Hash]chainCode{
		bitcoinGenesis:  bitcoinChain,
		litecoinGenesis: litecoinChain,
	}

	// reverseChainMap is the inverse of the chainMap above: it maps the
	// chain enum for a chain to its genesis hash.
	reverseChainMap = map[chainCode]chainhash.Hash{
		bitcoinChain:  bitcoinGenesis,
		litecoinChain: litecoinGenesis,
	}

	// chainDNSSeeds is a map of a chain's hash to the set of DNS seeds
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
	// struct like chaincfg.Params
	chainDNSSeeds = map[chainhash.Hash][][2]string{
		bitcoinGenesis: {
			{
				"nodes.lightning.directory",
				"soa.nodes.lightning.directory",
			},
		},
	}
)

// chainRegistry keeps track of the current chains
type chainRegistry struct {
	sync.RWMutex

	activeChains map[chainCode]*chainControl
	netParams    map[chainCode]*bitcoinNetParams

	primaryChain chainCode
}

// newChainRegistry creates a new chainRegistry.
func newChainRegistry() *chainRegistry {
	return &chainRegistry{
		activeChains: make(map[chainCode]*chainControl),
		netParams:    make(map[chainCode]*bitcoinNetParams),
	}
}

// RegisterChain assigns an active chainControl instance to a target chain
// identified by its chainCode.
func (c *chainRegistry) RegisterChain(newChain chainCode, cc *chainControl) {
	c.Lock()
	c.activeChains[newChain] = cc
	c.Unlock()
}

// LookupChain attempts to lookup an active chainControl instance for the
// target chain.
func (c *chainRegistry) LookupChain(targetChain chainCode) (*chainControl, bool) {
	c.RLock()
	cc, ok := c.activeChains[targetChain]
	c.RUnlock()
	return cc, ok
}

// LookupChainByHash attempts to look up an active chainControl which
// corresponds to the passed genesis hash.
func (c *chainRegistry) LookupChainByHash(chainHash chainhash.Hash) (*chainControl, bool) {
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
func (c *chainRegistry) RegisterPrimaryChain(cc chainCode) {
	c.Lock()
	defer c.Unlock()

	c.primaryChain = cc
}

// PrimaryChain returns the primary chain for this running lnd instance. The
// primary chain is considered the "home base" while the other registered
// chains are treated as secondary chains.
func (c *chainRegistry) PrimaryChain() chainCode {
	c.RLock()
	defer c.RUnlock()

	return c.primaryChain
}

// ActiveChains returns the total number of active chains.
func (c *chainRegistry) ActiveChains() []chainCode {
	c.RLock()
	defer c.RUnlock()

	chains := make([]chainCode, 0, len(c.activeChains))
	for activeChain := range c.activeChains {
		chains = append(chains, activeChain)
	}

	return chains
}

// NumActiveChains returns the total number of active chains.
func (c *chainRegistry) NumActiveChains() uint32 {
	c.RLock()
	defer c.RUnlock()

	return uint32(len(c.activeChains))
}

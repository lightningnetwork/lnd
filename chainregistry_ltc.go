// +build !noltc

package main

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/chainntnfs/bitcoindnotify"
	"github.com/lightningnetwork/lnd/chainntnfs/btcdnotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/chainview"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/rpcclient"
	"github.com/roasbeef/btcwallet/chain"
)

const (
	defaultLitecoinMinHTLCMSat   = lnwire.MilliSatoshi(1000)
	defaultLitecoinBaseFeeMSat   = lnwire.MilliSatoshi(1000)
	defaultLitecoinFeeRate       = lnwire.MilliSatoshi(1)
	defaultLitecoinTimeLockDelta = 576
	defaultLitecoinStaticFeeRate = lnwallet.SatPerVByte(200)
	defaultLitecoinDustLimit     = btcutil.Amount(54600)
)

// defaultLtcChannelConstraints is the default set of channel constraints that are
// meant to be used when initially funding a Litecoin channel.
var defaultLtcChannelConstraints = channeldb.ChannelConstraints{
	DustLimit:        defaultLitecoinDustLimit,
	MaxAcceptedHtlcs: lnwallet.MaxHTLCNumber / 2,
}

var (
	// litecoinTestnetGenesis is the genesis hash of Litecoin's testnet4
	// chain.
	litecoinTestnetGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0xa0, 0x29, 0x3e, 0x4e, 0xeb, 0x3d, 0xa6, 0xe6,
		0xf5, 0x6f, 0x81, 0xed, 0x59, 0x5f, 0x57, 0x88,
		0x0d, 0x1a, 0x21, 0x56, 0x9e, 0x13, 0xee, 0xfd,
		0xd9, 0x51, 0x28, 0x4b, 0x5a, 0x62, 0x66, 0x49,
	})

	// litecoinMainnetGenesis is the genesis hash of Litecoin's main chain.
	litecoinMainnetGenesis = chainhash.Hash([chainhash.HashSize]byte{
		0xe2, 0xbf, 0x04, 0x7e, 0x7e, 0x5a, 0x19, 0x1a,
		0xa4, 0xef, 0x34, 0xd3, 0x14, 0x97, 0x9d, 0xc9,
		0x98, 0x6e, 0x0f, 0x19, 0x25, 0x1e, 0xda, 0xba,
		0x59, 0x40, 0xfd, 0x1f, 0xe3, 0x65, 0xa7, 0x12,
	})
)

func newLitecoinChainControlFromConfig(cfg *config, chanDB *channeldb.DB,
	privateWalletPw, publicWalletPw []byte, birthday time.Time,
	recoveryWindow uint32) (*chainControl, func(), error) {

	// Set the RPC config from the "home" chain. Multi-chain isn't yet
	// active, so we'll restrict usage to a particular chain for now.
	homeChainConfig := cfg.Litecoin

	ltndLog.Infof("Primary chain is set to: %v",
		registeredChains.PrimaryChain())

	cc := &chainControl{}

	cc.routingPolicy = htlcswitch.ForwardingPolicy{
		MinHTLC:       cfg.Litecoin.MinHTLC,
		BaseFee:       cfg.Litecoin.BaseFee,
		FeeRate:       cfg.Litecoin.FeeRate,
		TimeLockDelta: cfg.Litecoin.TimeLockDelta,
	}
	cc.feeEstimator = lnwallet.StaticFeeEstimator{
		FeeRate: defaultLitecoinStaticFeeRate,
	}

	walletConfig := &btcwallet.Config{
		PrivatePass:    privateWalletPw,
		PublicPass:     publicWalletPw,
		Birthday:       birthday,
		RecoveryWindow: recoveryWindow,
		DataDir:        homeChainConfig.ChainDir,
		NetParams:      activeNetParams.Params,
		FeeEstimator:   cc.feeEstimator,
		CoinType:       activeNetParams.CoinType,
	}

	var (
		err          error
		cleanUp      func()
		bitcoindConn *chain.BitcoindClient
	)

	// If spv mode is active, then we'll be using a distinct set of
	// chainControl interfaces that interface directly with the p2p network
	// of the selected chain.
	switch homeChainConfig.Node {
	case "litecoind":
		bitcoindMode := cfg.LitecoindMode

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
			rpcPort, err := strconv.Atoi(activeNetParams.rpcPort)
			if err != nil {
				return nil, nil, err
			}
			rpcPort -= 2
			bitcoindHost = fmt.Sprintf("%v:%d",
				bitcoindMode.RPCHost, rpcPort)
			if cfg.Bitcoin.Active && cfg.Bitcoin.RegTest {
				conn, err := net.Dial("tcp", bitcoindHost)
				if err != nil || conn == nil {
					rpcPort = 18443
					bitcoindHost = fmt.Sprintf("%v:%d",
						bitcoindMode.RPCHost,
						rpcPort)
				} else {
					conn.Close()
				}
			}
		}

		bitcoindUser := bitcoindMode.RPCUser
		bitcoindPass := bitcoindMode.RPCPass

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
			bitcoindMode.ZMQPath, *activeNetParams.Params)
		if err != nil {
			return nil, nil, err
		}

		// Next, we'll create an instance of the bitcoind chain view to
		// be used within the routing layer.
		cc.chainView, err = chainview.NewBitcoindFilteredChainView(
			*rpcConfig, bitcoindMode.ZMQPath,
			*activeNetParams.Params)
		if err != nil {
			srvrLog.Errorf("unable to create chain view: %v", err)
			return nil, nil, err
		}

		// Create a special rpc+ZMQ client for bitcoind which will be
		// used by the wallet for notifications, calls, etc.
		bitcoindConn, err = chain.NewBitcoindClient(
			activeNetParams.Params, bitcoindHost, bitcoindUser,
			bitcoindPass, bitcoindMode.ZMQPath,
			time.Millisecond*100)
		if err != nil {
			return nil, nil, err
		}

		walletConfig.ChainSource = bitcoindConn

		// If we're not in regtest mode, then we'll attempt to use a
		// proper fee estimator for testnet.
		if cfg.Litecoin.Active {
			ltndLog.Infof("Initializing litecoind backed fee estimator")

			// Finally, we'll re-initialize the fee estimator, as
			// if we're using litecoind as a backend, then we can
			// use live fee estimates, rather than a statically
			// coded value.
			fallBackFeeRate := lnwallet.SatPerVByte(25)
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
	case "btcd", "ltcd":
		// Otherwise, we'll be speaking directly via RPC to a node.
		//
		// So first we'll load btcd/ltcd's TLS cert for the RPC
		// connection. If a raw cert was specified in the config, then
		// we'll set that directly. Otherwise, we attempt to read the
		// cert from the path specified in the config.
		btcdMode := cfg.LtcdMode

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

		// If we're not in simnet or regtest mode, then we'll attempt
		// to use a proper fee estimator for testnet.
		if !cfg.Litecoin.SimNet && !cfg.Litecoin.RegTest {

			ltndLog.Infof("Initializing btcd backed fee estimator")

			// Finally, we'll re-initialize the fee estimator, as
			// if we're using btcd as a backend, then we can use
			// live fee estimates, rather than a statically coded
			// value.
			fallBackFeeRate := lnwallet.SatPerVByte(25)
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
	}

	wc, err := btcwallet.New(*walletConfig)
	if err != nil {
		fmt.Printf("unable to create wallet controller: %v\n", err)
		return nil, nil, err
	}

	cc.msgSigner = wc
	cc.signer = wc
	cc.chainIO = wc

	// Select the default channel constraints for the primary chain.
	channelConstraints := defaultLtcChannelConstraints

	keyRing := keychain.NewBtcWalletKeyRing(
		wc.InternalWallet(), activeNetParams.CoinType,
	)

	// Create, and start the lnwallet, which handles the core payment
	// channel logic, and exposes control via proxy state machines.
	walletCfg := lnwallet.Config{
		Database:           chanDB,
		Notifier:           cc.chainNotifier,
		WalletController:   wc,
		Signer:             cc.signer,
		FeeEstimator:       cc.feeEstimator,
		SecretKeyRing:      keyRing,
		ChainIO:            cc.chainIO,
		DefaultConstraints: channelConstraints,
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

	return cc, cleanUp, nil
}

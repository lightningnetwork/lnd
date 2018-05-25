// +build !noltc

package main

import (
	"fmt"
	"path/filepath"

	"github.com/roasbeef/btcutil"
)

var (
	defaultLtcdDir         = btcutil.AppDataDir("ltcd", false)
	defaultLtcdRPCCertFile = filepath.Join(defaultLtcdDir, "rpc.cert")

	defaultLitecoindDir = btcutil.AppDataDir("litecoin", false)
)

func init() {

	err := registeredCoins.RegisterCoin(litecoinChain, &coinControl{
		DefaultConfig:          ltcDefaultConfig,
		RegisterChain:          ltcRegisterChain,
		IsTestNet:              ltcIsTestNet,
		ChainControlFromConfig: newLitecoinChainControlFromConfig,
	})
	if err != nil {
		panic(err)
	}
}

// ltcDefaultConfig applies the default chain configuration for litecoin.
func ltcDefaultConfig(cfg *config) error {
	cfg.Litecoin = &chainConfig{
		MinHTLC:       defaultLitecoinMinHTLCMSat,
		BaseFee:       defaultLitecoinBaseFeeMSat,
		FeeRate:       defaultLitecoinFeeRate,
		TimeLockDelta: defaultLitecoinTimeLockDelta,
		Node:          "ltcd",
	}

	cfg.LtcdMode = &btcdConfig{
		Dir:        defaultLtcdDir,
		RPCHost:    defaultRPCHost,
		RPCCert:    defaultLtcdRPCCertFile,
		DaemonName: "ltcd",
		File:       "ltcd",
	}

	cfg.LitecoindMode = &bitcoindConfig{
		Dir:        defaultLitecoindDir,
		RPCHost:    defaultRPCHost,
		DaemonName: "litecoind",
		File:       "litecoin",
	}

	return nil
}

func ltcRegisterChain(cfg *config, funcName string) (*config, error) {
	if cfg.Litecoin.SimNet {
		str := "%s: simnet mode for litecoin not currently supported"
		return nil, fmt.Errorf(str, funcName)
	}
	if cfg.Litecoin.RegTest {
		str := "%s: regnet mode for litecoin not currently supported"
		return nil, fmt.Errorf(str, funcName)
	}

	if cfg.Litecoin.TimeLockDelta < minTimeLockDelta {
		return nil, fmt.Errorf("timelockdelta must be at least %v",
			minTimeLockDelta)
	}

	// Multiple networks can't be selected simultaneously.  Count
	// number of network flags passed; assign active network params
	// while we're at it.
	numNets := 0
	if cfg.Litecoin.MainNet {
		numNets++
	}
	if cfg.Litecoin.TestNet3 {
		numNets++
	}

	if numNets > 1 {
		str := "%s: The mainnet, testnet, and simnet params " +
			"can't be used together -- choose one of the " +
			"three"
		err := fmt.Errorf(str, funcName)
		return nil, err
	}

	// The target network must be provided, otherwise, we won't
	// know how to initialize the daemon.
	if numNets == 0 {
		str := "%s: either --litecoin.mainnet, or " +
			"litecoin.testnet must be specified"
		err := fmt.Errorf(str, funcName)
		return nil, err
	}

	// The litecoin chain is the current active chain. However
	// throughout the codebase we required chaincfg.Params. So as a
	// temporary hack, we'll mutate the default net params for
	// bitcoin with the litecoin specific information.
	applyLitecoinParams(&activeNetParams, cfg)

	switch cfg.Litecoin.Node {
	case "ltcd":
		err := parseRPCParams(cfg.Litecoin, cfg.LtcdMode,
			litecoinChain, funcName)
		if err != nil {
			err := fmt.Errorf("unable to load RPC "+
				"credentials for ltcd: %v", err)
			return nil, err
		}
	case "litecoind":
		if cfg.Litecoin.SimNet {
			return nil, fmt.Errorf("%s: litecoind does not "+
				"support simnet", funcName)
		}
		err := parseRPCParams(cfg.Litecoin, cfg.LitecoindMode,
			litecoinChain, funcName)
		if err != nil {
			err := fmt.Errorf("unable to load RPC "+
				"credentials for litecoind: %v", err)
			return nil, err
		}
	default:
		str := "%s: only ltcd and litecoind mode supported for " +
			"litecoin at this time"
		return nil, fmt.Errorf(str, funcName)
	}

	cfg.Litecoin.ChainDir = filepath.Join(cfg.DataDir,
		defaultChainSubDirname,
		litecoinChain.String())

	// Finally we'll register the litecoin chain as our current
	// primary chain.
	registeredChains.RegisterPrimaryChain(litecoinChain)

	return cfg, nil
}

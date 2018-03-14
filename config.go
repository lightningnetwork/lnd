// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2017 The Lightning Network Developers

package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	flags "github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd/brontide"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/torsvc"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

const (
	defaultConfigFilename     = "lnd.conf"
	defaultDataDirname        = "data"
	defaultChainSubDirname    = "chain"
	defaultGraphSubDirname    = "graph"
	defaultTLSCertFilename    = "tls.cert"
	defaultTLSKeyFilename     = "tls.key"
	defaultAdminMacFilename   = "admin.macaroon"
	defaultReadMacFilename    = "readonly.macaroon"
	defaultLogLevel           = "info"
	defaultLogDirname         = "logs"
	defaultLogFilename        = "lnd.log"
	defaultRPCPort            = 10009
	defaultRESTPort           = 8080
	defaultPeerPort           = 9735
	defaultRPCHost            = "localhost"
	defaultMaxPendingChannels = 1
	defaultNoEncryptWallet    = false
	defaultTrickleDelay       = 30 * 1000

	defaultBroadcastDelta = 10

	// minTimeLockDelta is the minimum timelock we require for incoming
	// HTLCs on our channels.
	minTimeLockDelta = 4

	defaultAlias = ""
	defaultColor = "#3399FF"
)

var (
	defaultLndDir       = btcutil.AppDataDir("lnd", false)
	defaultConfigFile   = filepath.Join(defaultLndDir, defaultConfigFilename)
	defaultDataDir      = filepath.Join(defaultLndDir, defaultDataDirname)
	defaultTLSCertPath  = filepath.Join(defaultLndDir, defaultTLSCertFilename)
	defaultTLSKeyPath   = filepath.Join(defaultLndDir, defaultTLSKeyFilename)
	defaultAdminMacPath = filepath.Join(defaultLndDir, defaultAdminMacFilename)
	defaultReadMacPath  = filepath.Join(defaultLndDir, defaultReadMacFilename)
	defaultLogDir       = filepath.Join(defaultLndDir, defaultLogDirname)

	defaultBtcdDir         = btcutil.AppDataDir("btcd", false)
	defaultBtcdRPCCertFile = filepath.Join(defaultBtcdDir, "rpc.cert")

	defaultLtcdDir         = btcutil.AppDataDir("ltcd", false)
	defaultLtcdRPCCertFile = filepath.Join(defaultLtcdDir, "rpc.cert")

	defaultBitcoindDir  = btcutil.AppDataDir("bitcoin", false)
	defaultLitecoindDir = btcutil.AppDataDir("litecoin", false)
)

type chainConfig struct {
	Active   bool   `long:"active" description:"If the chain should be active or not."`
	ChainDir string `long:"chaindir" description:"The directory to store the chain's data within."`

	Node string `long:"node" description:"The blockchain interface to use." choice:"btcd" choice:"bitcoind" choice:"neutrino" choice:"ltcd" choice:"litecoind"`

	TestNet3 bool `long:"testnet" description:"Use the test network"`
	SimNet   bool `long:"simnet" description:"Use the simulation test network"`
	RegTest  bool `long:"regtest" description:"Use the regression test network"`

	DefaultNumChanConfs int                 `long:"defaultchanconfs" description:"The default number of confirmations a channel must have before it's considered open. If this is not set, we will scale the value according to the channel size."`
	DefaultRemoteDelay  int                 `long:"defaultremotedelay" description:"The default number of blocks we will require our channel counterparty to wait before accessing its funds in case of unilateral close. If this is not set, we will scale the value according to the channel size."`
	MinHTLC             lnwire.MilliSatoshi `long:"minhtlc" description:"The smallest HTLC we are willing to forward on our channels, in millisatoshi"`
	BaseFee             lnwire.MilliSatoshi `long:"basefee" description:"The base fee in millisatoshi we will charge for forwarding payments on our channels"`
	FeeRate             lnwire.MilliSatoshi `long:"feerate" description:"The fee rate used when forwarding payments on our channels. The total fee charged is basefee + (amount * feerate / 1000000), where amount is the forwarded amount."`
	TimeLockDelta       uint32              `long:"timelockdelta" description:"The CLTV delta we will subtract from a forwarded HTLC's timelock value"`
}

type neutrinoConfig struct {
	AddPeers     []string      `short:"a" long:"addpeer" description:"Add a peer to connect with at startup"`
	ConnectPeers []string      `long:"connect" description:"Connect only to the specified peers at startup"`
	MaxPeers     int           `long:"maxpeers" description:"Max number of inbound and outbound peers"`
	BanDuration  time.Duration `long:"banduration" description:"How long to ban misbehaving peers.  Valid time units are {s, m, h}.  Minimum 1 second"`
	BanThreshold uint32        `long:"banthreshold" description:"Maximum allowed ban score before disconnecting and banning misbehaving peers."`
}

type btcdConfig struct {
	Dir        string `long:"dir" description:"The base directory that contains the node's data, logs, configuration file, etc."`
	RPCHost    string `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser    string `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass    string `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	RPCCert    string `long:"rpccert" description:"File containing the daemon's certificate file"`
	RawRPCCert string `long:"rawrpccert" description:"The raw bytes of the daemon's PEM-encoded certificate chain which will be used to authenticate the RPC connection."`
}

type bitcoindConfig struct {
	Dir     string `long:"dir" description:"The base directory that contains the node's data, logs, configuration file, etc."`
	RPCHost string `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser string `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass string `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	ZMQPath string `long:"zmqpath" description:"The path to the ZMQ socket providing at least raw blocks. Raw transactions can be handled as well."`
}

type autoPilotConfig struct {
	// TODO(roasbeef): add
	Active      bool    `long:"active" description:"If the autopilot agent should be active or not."`
	MaxChannels int     `long:"maxchannels" description:"The maximum number of channels that should be created"`
	Allocation  float64 `long:"allocation" description:"The percentage of total funds that should be committed to automatic channel establishment"`
}

type torConfig struct {
	Socks           string `long:"socks" description:"The port that Tor's exposed SOCKS5 proxy is listening on. Using Tor allows outbound-only connections (listening will be disabled) -- NOTE port must be between 1024 and 65535"`
	DNS             string `long:"dns" description:"The DNS server as IP:PORT that Tor will use for SRV queries - NOTE must have TCP resolution enabled"`
	StreamIsolation bool   `long:"streamisolation" description:"Enable Tor stream isolation by randomizing user credentials for each connection."`
}

// config defines the configuration options for lnd.
//
// See loadConfig for further details regarding the configuration
// loading+parsing process.
type config struct {
	ShowVersion bool `short:"V" long:"version" description:"Display version information and exit"`

	LndDir       string `long:"lnddir" description:"The base directory that contains lnd's data, logs, configuration file, etc."`
	ConfigFile   string `long:"C" long:"configfile" description:"Path to configuration file"`
	DataDir      string `short:"b" long:"datadir" description:"The directory to store lnd's data within"`
	TLSCertPath  string `long:"tlscertpath" description:"Path to write the TLS certificate for lnd's RPC and REST services"`
	TLSKeyPath   string `long:"tlskeypath" description:"Path to write the TLS private key for lnd's RPC and REST services"`
	TLSExtraIP   string `long:"tlsextraip" description:"Adds an extra ip to the generated certificate"`
	NoMacaroons  bool   `long:"no-macaroons" description:"Disable macaroon authentication"`
	AdminMacPath string `long:"adminmacaroonpath" description:"Path to write the admin macaroon for lnd's RPC and REST services if it doesn't exist"`
	ReadMacPath  string `long:"readonlymacaroonpath" description:"Path to write the read-only macaroon for lnd's RPC and REST services if it doesn't exist"`
	LogDir       string `long:"logdir" description:"Directory to log output."`

	RPCListeners  []string `long:"rpclisten" description:"Add an interface/port to listen for RPC connections"`
	RESTListeners []string `long:"restlisten" description:"Add an interface/port to listen for REST connections"`
	Listeners     []string `long:"listen" description:"Add an interface/port to listen for peer connections"`
	DisableListen bool     `long:"nolisten" description:"Disable listening for incoming peer connections"`
	ExternalIPs   []string `long:"externalip" description:"Add an ip:port to the list of local addresses we claim to listen on to peers. If a port is not specified, the default (9735) will be used regardless of other parameters"`

	DebugLevel string `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`

	CPUProfile string `long:"cpuprofile" description:"Write CPU profile to the specified file"`

	Profile string `long:"profile" description:"Enable HTTP profiling on given port -- NOTE port must be between 1024 and 65535"`

	DebugHTLC          bool `long:"debughtlc" description:"Activate the debug htlc mode. With the debug HTLC mode, all payments sent use a pre-determined R-Hash. Additionally, all HTLCs sent to a node with the debug HTLC R-Hash are immediately settled in the next available state transition."`
	HodlHTLC           bool `long:"hodlhtlc" description:"Activate the hodl HTLC mode.  With hodl HTLC mode, all incoming HTLCs will be accepted by the receiving node, but no attempt will be made to settle the payment with the sender."`
	UnsafeDisconnect   bool `long:"unsafe-disconnect" description:"Allows the rpcserver to intentionally disconnect from peers with open channels. USED FOR TESTING ONLY."`
	UnsafeReplay       bool `long:"unsafe-replay" description:"Causes a link to replay the adds on its commitment txn after starting up, this enables testing of the sphinx replay logic."`
	MaxPendingChannels int  `long:"maxpendingchannels" description:"The maximum number of incoming pending channels permitted per peer."`

	Bitcoin      *chainConfig    `group:"Bitcoin" namespace:"bitcoin"`
	BtcdMode     *btcdConfig     `group:"btcd" namespace:"btcd"`
	BitcoindMode *bitcoindConfig `group:"bitcoind" namespace:"bitcoind"`
	NeutrinoMode *neutrinoConfig `group:"neutrino" namespace:"neutrino"`

	Litecoin      *chainConfig    `group:"Litecoin" namespace:"litecoin"`
	LtcdMode      *btcdConfig     `group:"ltcd" namespace:"ltcd"`
	LitecoindMode *bitcoindConfig `group:"litecoind" namespace:"litecoind"`

	Autopilot *autoPilotConfig `group:"autopilot" namespace:"autopilot"`

	Tor *torConfig `group:"Tor" namespace:"tor"`

	NoNetBootstrap bool `long:"nobootstrap" description:"If true, then automatic network bootstrapping will not be attempted."`

	NoEncryptWallet bool `long:"noencryptwallet" description:"If set, wallet will be encrypted using the default passphrase."`

	TrickleDelay int `long:"trickledelay" description:"Time in milliseconds between each release of announcements to the network"`

	Alias string `long:"alias" description:"The node alias. Used as a moniker by peers and intelligence services"`
	Color string `long:"color" description:"The color of the node in hex format (i.e. '#3399FF'). Used to customize node appearance in intelligence services"`

	net torsvc.Net
}

// loadConfig initializes and parses the config using a config file and command
// line options.
//
// The configuration proceeds as follows:
// 	1) Start with a default config with sane settings
// 	2) Pre-parse the command line to check for an alternative config file
// 	3) Load configuration file overwriting defaults with any specified options
// 	4) Parse CLI options and overwrite/add any specified options
func loadConfig() (*config, error) {
	defaultCfg := config{
		LndDir:       defaultLndDir,
		ConfigFile:   defaultConfigFile,
		DataDir:      defaultDataDir,
		DebugLevel:   defaultLogLevel,
		TLSCertPath:  defaultTLSCertPath,
		TLSKeyPath:   defaultTLSKeyPath,
		AdminMacPath: defaultAdminMacPath,
		ReadMacPath:  defaultReadMacPath,
		LogDir:       defaultLogDir,
		Bitcoin: &chainConfig{
			MinHTLC:       defaultBitcoinMinHTLCMSat,
			BaseFee:       defaultBitcoinBaseFeeMSat,
			FeeRate:       defaultBitcoinFeeRate,
			TimeLockDelta: defaultBitcoinTimeLockDelta,
			Node:          "btcd",
		},
		BtcdMode: &btcdConfig{
			Dir:     defaultBtcdDir,
			RPCHost: defaultRPCHost,
			RPCCert: defaultBtcdRPCCertFile,
		},
		BitcoindMode: &bitcoindConfig{
			Dir:     defaultBitcoindDir,
			RPCHost: defaultRPCHost,
		},
		Litecoin: &chainConfig{
			MinHTLC:       defaultLitecoinMinHTLCMSat,
			BaseFee:       defaultLitecoinBaseFeeMSat,
			FeeRate:       defaultLitecoinFeeRate,
			TimeLockDelta: defaultLitecoinTimeLockDelta,
			Node:          "ltcd",
		},
		LtcdMode: &btcdConfig{
			Dir:     defaultLtcdDir,
			RPCHost: defaultRPCHost,
			RPCCert: defaultLtcdRPCCertFile,
		},
		LitecoindMode: &bitcoindConfig{
			Dir:     defaultLitecoindDir,
			RPCHost: defaultRPCHost,
		},
		MaxPendingChannels: defaultMaxPendingChannels,
		NoEncryptWallet:    defaultNoEncryptWallet,
		Autopilot: &autoPilotConfig{
			MaxChannels: 5,
			Allocation:  0.6,
		},
		TrickleDelay: defaultTrickleDelay,
		Alias:        defaultAlias,
		Color:        defaultColor,
	}

	// Pre-parse the command line options to pick up an alternative config
	// file.
	preCfg := defaultCfg
	if _, err := flags.Parse(&preCfg); err != nil {
		return nil, err
	}

	// Show the version and exit if the version flag was specified.
	appName := filepath.Base(os.Args[0])
	appName = strings.TrimSuffix(appName, filepath.Ext(appName))
	usageMessage := fmt.Sprintf("Use %s -h to show usage", appName)
	if preCfg.ShowVersion {
		fmt.Println(appName, "version", version())
		os.Exit(0)
	}

	// If the provided lnd directory is not the default, we'll modify the
	// path to all of the files and directories that will live within it.
	lndDir := cleanAndExpandPath(preCfg.LndDir)
	if lndDir != defaultLndDir {
		defaultCfg.ConfigFile = filepath.Join(lndDir, defaultConfigFilename)
		defaultCfg.DataDir = filepath.Join(lndDir, defaultDataDirname)
		defaultCfg.TLSCertPath = filepath.Join(lndDir, defaultTLSCertFilename)
		defaultCfg.TLSKeyPath = filepath.Join(lndDir, defaultTLSKeyFilename)
		defaultCfg.AdminMacPath = filepath.Join(lndDir, defaultAdminMacFilename)
		defaultCfg.ReadMacPath = filepath.Join(lndDir, defaultReadMacFilename)
		defaultCfg.LogDir = filepath.Join(lndDir, defaultLogDirname)
	}

	// Create the lnd directory if it doesn't already exist.
	funcName := "loadConfig"
	if err := os.MkdirAll(lndDir, 0700); err != nil {
		// Show a nicer error message if it's because a symlink is
		// linked to a directory that does not exist (probably because
		// it's not mounted).
		if e, ok := err.(*os.PathError); ok && os.IsExist(err) {
			if link, lerr := os.Readlink(e.Path); lerr == nil {
				str := "is symlink %s -> %s mounted?"
				err = fmt.Errorf(str, e.Path, link)
			}
		}

		str := "%s: Failed to create lnd directory: %v"
		err := fmt.Errorf(str, funcName, err)
		fmt.Fprintln(os.Stderr, err)
		return nil, err
	}

	// Next, load any additional configuration options from the file.
	var configFileError error
	cfg := defaultCfg
	configFile := cleanAndExpandPath(preCfg.ConfigFile)
	if err := flags.IniParse(configFile, &cfg); err != nil {
		configFileError = err
	}

	// Finally, parse the remaining command line options again to ensure
	// they take precedence.
	if _, err := flags.Parse(&cfg); err != nil {
		return nil, err
	}

	// As soon as we're done parsing configuration options, ensure all paths
	// to directories and files are cleaned and expanded before attempting
	// to use them later on.
	cfg.DataDir = cleanAndExpandPath(cfg.DataDir)
	cfg.TLSCertPath = cleanAndExpandPath(cfg.TLSCertPath)
	cfg.TLSKeyPath = cleanAndExpandPath(cfg.TLSKeyPath)
	cfg.AdminMacPath = cleanAndExpandPath(cfg.AdminMacPath)
	cfg.ReadMacPath = cleanAndExpandPath(cfg.ReadMacPath)
	cfg.LogDir = cleanAndExpandPath(cfg.LogDir)
	cfg.BtcdMode.Dir = cleanAndExpandPath(cfg.BtcdMode.Dir)
	cfg.LtcdMode.Dir = cleanAndExpandPath(cfg.LtcdMode.Dir)
	cfg.BitcoindMode.Dir = cleanAndExpandPath(cfg.BitcoindMode.Dir)
	cfg.LitecoindMode.Dir = cleanAndExpandPath(cfg.LitecoindMode.Dir)

	// Setup dial and DNS resolution functions depending on the specified
	// options. The default is to use the standard golang "net" package
	// functions. When Tor's proxy is specified, the dial function is set to
	// the proxy specific dial function and the DNS resolution functions use
	// Tor.
	cfg.net = &torsvc.RegularNet{}
	if cfg.Tor.Socks != "" && cfg.Tor.DNS != "" {
		// Validate Tor port number
		torport, err := strconv.Atoi(cfg.Tor.Socks)
		if err != nil || torport < 1024 || torport > 65535 {
			str := "%s: The tor socks5 port must be between 1024 and 65535"
			err := fmt.Errorf(str, funcName)
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintln(os.Stderr, usageMessage)
			return nil, err
		}

		// If ExternalIPs is set, throw an error since we cannot
		// listen for incoming connections via Tor's SOCKS5 proxy.
		if len(cfg.ExternalIPs) != 0 {
			str := "%s: Cannot set externalip flag with proxy flag - " +
				"cannot listen for incoming connections via Tor's " +
				"socks5 proxy"
			err := fmt.Errorf(str, funcName)
			return nil, err
		}

		cfg.net = &torsvc.TorProxyNet{
			TorDNS:          cfg.Tor.DNS,
			TorSocks:        cfg.Tor.Socks,
			StreamIsolation: cfg.Tor.StreamIsolation,
		}

		// If we are using Tor, since we only want connections routed
		// through Tor, listening is disabled.
		cfg.DisableListen = true

	} else if cfg.Tor.Socks != "" || cfg.Tor.DNS != "" {
		// Both TorSocks and TorDNS must be set.
		str := "%s: Both the tor.socks and the tor.dns flags must be set" +
			"to properly route connections and avoid DNS leaks while" +
			"using Tor"
		err := fmt.Errorf(str, funcName)
		return nil, err
	}

	switch {
	// At this moment, multiple active chains are not supported.
	case cfg.Litecoin.Active && cfg.Bitcoin.Active:
		str := "%s: Currently both Bitcoin and Litecoin cannot be " +
			"active together"
		return nil, fmt.Errorf(str, funcName)

	// Either Bitcoin must be active, or Litecoin must be active.
	// Otherwise, we don't know which chain we're on.
	case !cfg.Bitcoin.Active && !cfg.Litecoin.Active:
		return nil, fmt.Errorf("%s: either bitcoin.active or "+
			"litecoin.active must be set to 1 (true)", funcName)

	case cfg.Litecoin.Active:
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

		// The litecoin chain is the current active chain. However
		// throughout the codebase we required chaincfg.Params. So as a
		// temporary hack, we'll mutate the default net params for
		// bitcoin with the litecoin specific information.
		paramCopy := bitcoinTestNetParams
		applyLitecoinParams(&paramCopy)
		activeNetParams = paramCopy

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

	case cfg.Bitcoin.Active:
		// Multiple networks can't be selected simultaneously.  Count
		// number of network flags passed; assign active network params
		// while we're at it.
		numNets := 0
		if cfg.Bitcoin.TestNet3 {
			numNets++
			activeNetParams = bitcoinTestNetParams
		}
		if cfg.Bitcoin.RegTest {
			numNets++
			activeNetParams = regTestNetParams
		}
		if cfg.Bitcoin.SimNet {
			numNets++
			activeNetParams = bitcoinSimNetParams
		}
		if numNets > 1 {
			str := "%s: The testnet, segnet, and simnet params can't be " +
				"used together -- choose one of the three"
			err := fmt.Errorf(str, funcName)
			return nil, err
		}

		if cfg.Bitcoin.TimeLockDelta < minTimeLockDelta {
			return nil, fmt.Errorf("timelockdelta must be at least %v",
				minTimeLockDelta)
		}

		switch cfg.Bitcoin.Node {
		case "btcd":
			err := parseRPCParams(cfg.Bitcoin, cfg.BtcdMode,
				bitcoinChain, funcName)
			if err != nil {
				err := fmt.Errorf("unable to load RPC "+
					"credentials for btcd: %v", err)
				return nil, err
			}
		case "bitcoind":
			if cfg.Bitcoin.SimNet {
				return nil, fmt.Errorf("%s: bitcoind does not "+
					"support simnet", funcName)
			}
			err := parseRPCParams(cfg.Bitcoin, cfg.BitcoindMode,
				bitcoinChain, funcName)
			if err != nil {
				err := fmt.Errorf("unable to load RPC "+
					"credentials for bitcoind: %v", err)
				return nil, err
			}
		case "neutrino":
			// No need to get RPC parameters.
		default:
			str := "%s: only btcd, bitcoind, and neutrino mode " +
				"supported for bitcoin at this time"
			return nil, fmt.Errorf(str, funcName)
		}

		cfg.Bitcoin.ChainDir = filepath.Join(cfg.DataDir,
			defaultChainSubDirname,
			bitcoinChain.String())

		// Finally we'll register the bitcoin chain as our current
		// primary chain.
		registeredChains.RegisterPrimaryChain(bitcoinChain)
	}

	// Validate profile port number.
	if cfg.Profile != "" {
		profilePort, err := strconv.Atoi(cfg.Profile)
		if err != nil || profilePort < 1024 || profilePort > 65535 {
			str := "%s: The profile port must be between 1024 and 65535"
			err := fmt.Errorf(str, funcName)
			fmt.Fprintln(os.Stderr, err)
			fmt.Fprintln(os.Stderr, usageMessage)
			return nil, err
		}
	}

	// At this point, we'll save the base data directory in order to ensure
	// we don't store the macaroon database within any of the chain
	// namespaced directories.
	macaroonDatabaseDir = cfg.DataDir

	// If a custom macaroon directory wasn't specified and the data
	// directory has changed from the default path, then we'll also update
	// the path for the macaroons to be generated.
	if cfg.DataDir != defaultDataDir && cfg.AdminMacPath == defaultAdminMacPath {
		cfg.AdminMacPath = filepath.Join(cfg.DataDir, defaultAdminMacFilename)
	}
	if cfg.DataDir != defaultDataDir && cfg.ReadMacPath == defaultReadMacPath {
		cfg.ReadMacPath = filepath.Join(cfg.DataDir, defaultReadMacFilename)
	}

	// Append the network type to the log directory so it is "namespaced"
	// per network in the same fashion as the data directory.
	cfg.LogDir = filepath.Join(cfg.LogDir,
		registeredChains.PrimaryChain().String(),
		normalizeNetwork(activeNetParams.Name))

	// Initialize logging at the default logging level.
	initLogRotator(filepath.Join(cfg.LogDir, defaultLogFilename))

	// Parse, validate, and set debug log level(s).
	if err := parseAndSetDebugLevels(cfg.DebugLevel); err != nil {
		err := fmt.Errorf("%s: %v", funcName, err.Error())
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usageMessage)
		return nil, err
	}

	// At least one RPCListener is required.
	if len(cfg.RPCListeners) == 0 {
		addr := fmt.Sprintf("localhost:%d", defaultRPCPort)
		cfg.RPCListeners = append(cfg.RPCListeners, addr)
	}

	// Listen on the default interface/port if no REST listeners were
	// specified.
	if len(cfg.RESTListeners) == 0 {
		addr := fmt.Sprintf("localhost:%d", defaultRESTPort)
		cfg.RESTListeners = append(cfg.RESTListeners, addr)
	}

	// Listen on the default interface/port if no listeners were specified.
	if len(cfg.Listeners) == 0 {
		addr := fmt.Sprintf(":%d", defaultPeerPort)
		cfg.Listeners = append(cfg.Listeners, addr)
	}

	// For each of the RPC listeners (REST+gRPC), we'll ensure that users
	// have specified a safe combo for authentication. If not, we'll bail
	// out with an error.
	err := enforceSafeAuthentication(cfg.RPCListeners, !cfg.NoMacaroons)
	if err != nil {
		return nil, err
	}
	err = enforceSafeAuthentication(cfg.RESTListeners, !cfg.NoMacaroons)
	if err != nil {
		return nil, err
	}

	// Remove all Listeners if listening is disabled.
	if cfg.DisableListen {
		cfg.Listeners = nil
	}

	// Add default port to all RPC listener addresses if needed and remove
	// duplicate addresses.
	cfg.RPCListeners = normalizeAddresses(cfg.RPCListeners,
		strconv.Itoa(defaultRPCPort))

	// Add default port to all REST listener addresses if needed and remove
	// duplicate addresses.
	cfg.RESTListeners = normalizeAddresses(cfg.RESTListeners,
		strconv.Itoa(defaultRESTPort))

	// Add default port to all listener addresses if needed and remove
	// duplicate addresses.
	cfg.Listeners = normalizeAddresses(cfg.Listeners,
		strconv.Itoa(defaultPeerPort))

	// Warn about missing config file only after all other configuration is
	// done.  This prevents the warning on help messages and invalid
	// options.  Note this should go directly before the return.
	if configFileError != nil {
		ltndLog.Warnf("%v", configFileError)
	}

	return &cfg, nil
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
// This function is taken from https://github.com/btcsuite/btcd
func cleanAndExpandPath(path string) string {
	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		var homeDir string

		user, err := user.Current()
		if err == nil {
			homeDir = user.HomeDir
		} else {
			homeDir = os.Getenv("HOME")
		}

		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but the variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}

// parseAndSetDebugLevels attempts to parse the specified debug level and set
// the levels accordingly. An appropriate error is returned if anything is
// invalid.
func parseAndSetDebugLevels(debugLevel string) error {
	// When the specified string doesn't have any delimiters, treat it as
	// the log level for all subsystems.
	if !strings.Contains(debugLevel, ",") && !strings.Contains(debugLevel, "=") {
		// Validate debug log level.
		if !validLogLevel(debugLevel) {
			str := "The specified debug level [%v] is invalid"
			return fmt.Errorf(str, debugLevel)
		}

		// Change the logging level for all subsystems.
		setLogLevels(debugLevel)

		return nil
	}

	// Split the specified string into subsystem/level pairs while detecting
	// issues and update the log levels accordingly.
	for _, logLevelPair := range strings.Split(debugLevel, ",") {
		if !strings.Contains(logLevelPair, "=") {
			str := "The specified debug level contains an invalid " +
				"subsystem/level pair [%v]"
			return fmt.Errorf(str, logLevelPair)
		}

		// Extract the specified subsystem and log level.
		fields := strings.Split(logLevelPair, "=")
		subsysID, logLevel := fields[0], fields[1]

		// Validate subsystem.
		if _, exists := subsystemLoggers[subsysID]; !exists {
			str := "The specified subsystem [%v] is invalid -- " +
				"supported subsystems %v"
			return fmt.Errorf(str, subsysID, supportedSubsystems())
		}

		// Validate log level.
		if !validLogLevel(logLevel) {
			str := "The specified debug level [%v] is invalid"
			return fmt.Errorf(str, logLevel)
		}

		setLogLevel(subsysID, logLevel)
	}

	return nil
}

// validLogLevel returns whether or not logLevel is a valid debug log level.
func validLogLevel(logLevel string) bool {
	switch logLevel {
	case "trace":
		fallthrough
	case "debug":
		fallthrough
	case "info":
		fallthrough
	case "warn":
		fallthrough
	case "error":
		fallthrough
	case "critical":
		return true
	}
	return false
}

// supportedSubsystems returns a sorted slice of the supported subsystems for
// logging purposes.
func supportedSubsystems() []string {
	// Convert the subsystemLoggers map keys to a slice.
	subsystems := make([]string, 0, len(subsystemLoggers))
	for subsysID := range subsystemLoggers {
		subsystems = append(subsystems, subsysID)
	}

	// Sort the subsystems for stable display.
	sort.Strings(subsystems)
	return subsystems
}

// noiseDial is a factory function which creates a connmgr compliant dialing
// function by returning a closure which includes the server's identity key.
func noiseDial(idPriv *btcec.PrivateKey) func(net.Addr) (net.Conn, error) {
	return func(a net.Addr) (net.Conn, error) {
		lnAddr := a.(*lnwire.NetAddress)
		return brontide.Dial(idPriv, lnAddr, cfg.net.Dial)
	}
}

func parseRPCParams(cConfig *chainConfig, nodeConfig interface{}, net chainCode,
	funcName string) error {

	// First, we'll check our node config to make sure the RPC parameters
	// were set correctly. We'll also determine the path to the conf file
	// depending on the backend node.
	var daemonName, confDir, confFile string
	switch conf := nodeConfig.(type) {
	case *btcdConfig:
		// If both RPCUser and RPCPass are set, we assume those
		// credentials are good to use.
		if conf.RPCUser != "" && conf.RPCPass != "" {
			return nil
		}
		// If only ONE of RPCUser or RPCPass is set, we assume the
		// user did that unintentionally.
		if conf.RPCUser != "" || conf.RPCPass != "" {
			return fmt.Errorf("please set both or neither of " +
				"btcd.rpcuser and btcd.rpcpass")
		}

		switch net {
		case bitcoinChain:
			daemonName = "btcd"
			confDir = conf.Dir
			confFile = "btcd"
		case litecoinChain:
			daemonName = "ltcd"
			confDir = conf.Dir
			confFile = "ltcd"
		}
	case *bitcoindConfig:
		// If all of RPCUser, RPCPass, and ZMQPath are set, we assume
		// those parameters are good to use.
		if conf.RPCUser != "" && conf.RPCPass != "" && conf.ZMQPath != "" {
			return nil
		}
		// If only one or two of the parameters are set, we assume the
		// user did that unintentionally.
		if conf.RPCUser != "" || conf.RPCPass != "" || conf.ZMQPath != "" {
			return fmt.Errorf("please set all or none of " +
				"bitcoind.rpcuser, bitcoind.rpcpass, and " +
				"bitcoind.zmqpath")
		}

		switch net {
		case bitcoinChain:
			daemonName = "bitcoind"
			confDir = conf.Dir
			confFile = "bitcoin"
		case litecoinChain:
			daemonName = "litecoind"
			confDir = conf.Dir
			confFile = "litecoin"
		}
	}

	// If we're in simnet mode, then the running btcd instance won't read
	// the RPC credentials from the configuration. So if lnd wasn't
	// specified the parameters, then we won't be able to start.
	if cConfig.SimNet {
		str := "%v: rpcuser and rpcpass must be set to your btcd " +
			"node's RPC parameters for simnet mode"
		return fmt.Errorf(str, funcName)
	}

	fmt.Println("Attempting automatic RPC configuration to " + daemonName)

	confFile = filepath.Join(confDir, fmt.Sprintf("%v.conf", confFile))
	switch cConfig.Node {
	case "btcd", "ltcd":
		nConf := nodeConfig.(*btcdConfig)
		rpcUser, rpcPass, err := extractBtcdRPCParams(confFile)
		if err != nil {
			return fmt.Errorf("unable to extract RPC credentials:"+
				" %v, cannot start w/o RPC connection",
				err)
		}
		nConf.RPCUser, nConf.RPCPass = rpcUser, rpcPass
	case "bitcoind", "litecoind":
		nConf := nodeConfig.(*bitcoindConfig)
		rpcUser, rpcPass, zmqPath, err := extractBitcoindRPCParams(confFile)
		if err != nil {
			return fmt.Errorf("unable to extract RPC credentials:"+
				" %v, cannot start w/o RPC connection",
				err)
		}
		nConf.RPCUser, nConf.RPCPass, nConf.ZMQPath = rpcUser, rpcPass, zmqPath
	}

	fmt.Printf("Automatically obtained %v's RPC credentials\n", daemonName)
	return nil
}

// extractBtcdRPCParams attempts to extract the RPC credentials for an existing
// btcd instance. The passed path is expected to be the location of btcd's
// application data directory on the target system.
func extractBtcdRPCParams(btcdConfigPath string) (string, string, error) {
	// First, we'll open up the btcd configuration file found at the target
	// destination.
	btcdConfigFile, err := os.Open(btcdConfigPath)
	if err != nil {
		return "", "", err
	}
	defer btcdConfigFile.Close()

	// With the file open extract the contents of the configuration file so
	// we can attempt to locate the RPC credentials.
	configContents, err := ioutil.ReadAll(btcdConfigFile)
	if err != nil {
		return "", "", err
	}

	// Attempt to locate the RPC user using a regular expression. If we
	// don't have a match for our regular expression then we'll exit with
	// an error.
	rpcUserRegexp, err := regexp.Compile(`(?m)^\s*rpcuser=([^\s]+)`)
	if err != nil {
		return "", "", err
	}
	userSubmatches := rpcUserRegexp.FindSubmatch(configContents)
	if userSubmatches == nil {
		return "", "", fmt.Errorf("unable to find rpcuser in config")
	}

	// Similarly, we'll use another regular expression to find the set
	// rpcpass (if any). If we can't find the pass, then we'll exit with an
	// error.
	rpcPassRegexp, err := regexp.Compile(`(?m)^\s*rpcpass=([^\s]+)`)
	if err != nil {
		return "", "", err
	}
	passSubmatches := rpcPassRegexp.FindSubmatch(configContents)
	if passSubmatches == nil {
		return "", "", fmt.Errorf("unable to find rpcuser in config")
	}

	return string(userSubmatches[1]), string(passSubmatches[1]), nil
}

// extractBitcoindParams attempts to extract the RPC credentials for an
// existing bitcoind node instance. The passed path is expected to be the
// location of bitcoind's bitcoin.conf on the target system. The routine looks
// for a cookie first, optionally following the datadir configuration option in
// the bitcoin.conf. If it doesn't find one, it looks for rpcuser/rpcpassword.
func extractBitcoindRPCParams(bitcoindConfigPath string) (string, string, string, error) {

	// First, we'll open up the bitcoind configuration file found at the
	// target destination.
	bitcoindConfigFile, err := os.Open(bitcoindConfigPath)
	if err != nil {
		return "", "", "", err
	}
	defer bitcoindConfigFile.Close()

	// With the file open extract the contents of the configuration file so
	// we can attempt to locate the RPC credentials.
	configContents, err := ioutil.ReadAll(bitcoindConfigFile)
	if err != nil {
		return "", "", "", err
	}

	// First, we look for the ZMQ path for raw blocks. If raw transactions
	// are sent over this interface, we can also get unconfirmed txs.
	zmqPathRE, err := regexp.Compile(`(?m)^\s*zmqpubrawblock=([^\s]+)`)
	if err != nil {
		return "", "", "", err
	}
	zmqPathSubmatches := zmqPathRE.FindSubmatch(configContents)
	if len(zmqPathSubmatches) < 2 {
		return "", "", "", fmt.Errorf("unable to find zmqpubrawblock in config")
	}

	// Next, we'll try to find an auth cookie. We need to detect the chain
	// by seeing if one is specified in the configuration file.
	dataDir := path.Dir(bitcoindConfigPath)
	dataDirRE, err := regexp.Compile(`(?m)^\s*datadir=([^\s]+)`)
	if err != nil {
		return "", "", "", err
	}
	dataDirSubmatches := dataDirRE.FindSubmatch(configContents)
	if dataDirSubmatches != nil {
		dataDir = string(dataDirSubmatches[1])
	}

	chainDir := "/"
	netRE, err := regexp.Compile(`(?m)^\s*(testnet|regtest)=[\d]+`)
	if err != nil {
		return "", "", "", err
	}
	netSubmatches := netRE.FindSubmatch(configContents)
	if netSubmatches != nil {
		switch string(netSubmatches[1]) {
		case "testnet":
			chainDir = "/testnet3/"
		case "regtest":
			chainDir = "/regtest/"
		}
	}

	cookie, err := ioutil.ReadFile(dataDir + chainDir + ".cookie")
	if err == nil {
		splitCookie := strings.Split(string(cookie), ":")
		if len(splitCookie) == 2 {
			return splitCookie[0], splitCookie[1],
				string(zmqPathSubmatches[1]), nil
		}
	}

	// We didn't find a cookie, so we attempt to locate the RPC user using
	// a regular expression. If we  don't have a match for our regular
	// expression then we'll exit with an error.
	rpcUserRegexp, err := regexp.Compile(`(?m)^\s*rpcuser=([^\s]+)`)
	if err != nil {
		return "", "", "", err
	}
	userSubmatches := rpcUserRegexp.FindSubmatch(configContents)
	if userSubmatches == nil {
		return "", "", "", fmt.Errorf("unable to find rpcuser in config")
	}

	// Similarly, we'll use another regular expression to find the set
	// rpcpass (if any). If we can't find the pass, then we'll exit with an
	// error.
	rpcPassRegexp, err := regexp.Compile(`(?m)^\s*rpcpassword=([^\s]+)`)
	if err != nil {
		return "", "", "", err
	}
	passSubmatches := rpcPassRegexp.FindSubmatch(configContents)
	if passSubmatches == nil {
		return "", "", "", fmt.Errorf("unable to find rpcpassword in config")
	}

	return string(userSubmatches[1]), string(passSubmatches[1]),
		string(zmqPathSubmatches[1]), nil
}

// normalizeAddresses returns a new slice with all the passed addresses
// normalized with the given default port and all duplicates removed.
func normalizeAddresses(addrs []string, defaultPort string) []string {
	result := make([]string, 0, len(addrs))
	seen := map[string]struct{}{}
	for _, addr := range addrs {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, defaultPort)
		}
		if _, ok := seen[addr]; !ok {
			result = append(result, addr)
			seen[addr] = struct{}{}
		}
	}
	return result
}

// enforceSafeAuthentication enforces "safe" authentication taking into account
// the interfaces that the RPC servers are listening on, and if macaroons are
// activated or not. To project users from using dangerous config combinations,
// we'll prevent disabling authentication if the sever is listening on a public
// interface.
func enforceSafeAuthentication(addrs []string, macaroonsActive bool) error {
	isLoopback := func(addr string) bool {
		loopBackAddrs := []string{"localhost", "127.0.0.1"}
		for _, loopback := range loopBackAddrs {
			if strings.Contains(addr, loopback) {
				return true
			}
		}

		return false
	}

	// We'll now examine all addresses that this RPC server is listening
	// on. If it's a localhost address, we'll skip it, otherwise, we'll
	// return an error if macaroons are inactive.
	for _, addr := range addrs {
		if isLoopback(addr) {
			continue
		}

		if !macaroonsActive {
			return fmt.Errorf("Detected RPC server listening on "+
				"publicly reachable interface %v with "+
				"authentication disabled! Refusing to start with "+
				"--no-macaroons specified.", addr)
		}
	}

	return nil
}

// normalizeNetwork returns the common name of a network type used to create
// file paths. This allows differently versioned networks to use the same path.
func normalizeNetwork(network string) string {
	if strings.HasPrefix(network, "testnet") {
		return "testnet"
	}

	return network
}

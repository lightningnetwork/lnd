package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"
	flags "github.com/btcsuite/go-flags"
)

const (
	defaultConfigFilename = "lnd.conf"
	defaultDataDirname    = "data"
	defaultLogLevel       = "info"
	defaultLogDirname     = "logs"
	defaultLogFilename    = "btcd.log"
	defaultRPCPort        = 10009
	defaultSPVMode        = false
	defaultPeerPort       = 10011
	defaultRPCHost        = "localhost:18334"
	defaultRPCUser        = "user"
	defaultRPCPass        = "passwd"
	defaultSPVHostAdr     = "localhost:18333"
)

var (
	lndHomeDir        = btcutil.AppDataDir("lnd", false)
	defaultConfigFile = filepath.Join(lndHomeDir, defaultConfigFilename)
	defaultDataDir    = filepath.Join(lndHomeDir, defaultDataDirname)
	defaultLogDir     = filepath.Join(lndHomeDir, defaultLogDirname)

	// activeNetParams is a pointer to the parameters specific to the
	// currently active bitcoin network.
	//activeNetParams = &chaincfg.SegNetParams
	activeNetParams = &chaincfg.TestNet3Params

	btcdHomeDir        = btcutil.AppDataDir("btcd", false)
	defaultRPCKeyFile  = filepath.Join(btcdHomeDir, "rpc.key")
	defaultRPCCertFile = filepath.Join(btcdHomeDir, "rpc.cert")
)

// config defines the configuratino options for lnd.
//
// See loadConfig for further details regarding the configuration
// loading+parsing process.
type config struct {
	ShowVersion bool `short:"V" long:"version" description:"Display version information and exit"`

	ConfigFile string `long:"C" long:"configfile" description:"Path to configuration file"`
	DataDir    string `short:"b" long:"datadir" description:"The directory to store lnd's data within"`
	LogDir     string `long:"logdir" description:"Directory to log output."`

	Listeners   []string `long:"listen" description:"Add an interface/port to listen for connections (default all interfaces port: 8333, testnet: 18333)"`
	ExternalIPs []string `long:"externalip" description:"Add an ip to the list of local addresses we claim to listen on to peers"`

	DebugLevel string `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`

	PeerPort int    `long:"peerport" description:"The port to listen on for incoming p2p connections"`
	RPCPort  int    `long:"rpcport" description:"The port for the rpc server"`
	SPVMode  bool   `long:"spv" description:"assert to enter spv wallet mode"`
	RPCHost  string `long:"btcdhost" description:"The btcd rpc listening address. "`
	RPCUser  string `short:"u" long:"rpcuser" description:"Username for RPC connections"`
	RPCPass  string `short:"P" long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`

	RPCCert    string `long:"rpccert" description:"File containing btcd's certificate file"`
	RPCKey     string `long:"rpckey" description:"File containing btcd's certificate key"`
	SPVHostAdr string `long:"spvhostadr" description:"Address of full bitcoin node. It is used in SPV mode."`
	TestNet3   bool   `long:"testnet" description:"Use the test network"`
	SimNet     bool   `long:"simnet" description:"Use the simulation test network"`
	SegNet     bool   `long:"segnet" description:"Use the segragated witness test network"`
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
		ConfigFile: defaultConfigFile,
		DataDir:    defaultDataDir,
		DebugLevel: defaultLogLevel,
		LogDir:     defaultLogDir,
		PeerPort:   defaultPeerPort,
		RPCPort:    defaultRPCPort,
		SPVMode:    defaultSPVMode,
		RPCHost:    defaultRPCHost,
		RPCUser:    defaultRPCUser,
		RPCPass:    defaultRPCPass,
		RPCCert:    defaultRPCCertFile,
		RPCKey:     defaultRPCKeyFile,
		SPVHostAdr: defaultSPVHostAdr,
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

	// Create the home directory if it doesn't already exist.
	funcName := "loadConfig"
	if err := os.MkdirAll(lndHomeDir, 0700); err != nil {
		// Show a nicer error message if it's because a symlink is
		// linked to a directory that does not exist (probably because
		// it's not mounted).
		if e, ok := err.(*os.PathError); ok && os.IsExist(err) {
			if link, lerr := os.Readlink(e.Path); lerr == nil {
				str := "is symlink %s -> %s mounted?"
				err = fmt.Errorf(str, e.Path, link)
			}
		}

		str := "%s: Failed to create home directory: %v"
		err := fmt.Errorf(str, funcName, err)
		fmt.Fprintln(os.Stderr, err)
		return nil, err
	}

	// Next, load any additional configuration options from the file.
	cfg := defaultCfg
	if err := flags.IniParse(preCfg.ConfigFile, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}

	// Finally, parse the remaining command line options again to ensure
	// they take precedence.
	if _, err := flags.Parse(&cfg); err != nil {
		return nil, err
	}

	// Multiple networks can't be selected simultaneously.
	// Count number of network flags passed; assign active network params
	// while we're at it
	numNets := 0
	if cfg.TestNet3 {
		numNets++
		activeNetParams = &chaincfg.TestNet3Params
	}
	if cfg.SegNet {
		numNets++
		activeNetParams = &chaincfg.SegNetParams
	}
	if cfg.SimNet {
		numNets++
		activeNetParams = &chaincfg.SimNetParams
	}
	if numNets > 1 {
		str := "%s: The testnet, segnet, and simnet params can't be " +
			"used together -- choose one of the three"
		err := fmt.Errorf(str, funcName)
		return nil, err
	}

	// Append the network type to the data directory so it is "namespaced"
	// per network.  In addition to the block database, there are other
	// pieces of data that are saved to disk such as address manager state.
	// All data is specific to a network, so namespacing the data directory
	// means each individual piece of serialized data does not have to
	// worry about changing names per network and such.
	cfg.DataDir = cleanAndExpandPath(cfg.DataDir)
	cfg.DataDir = filepath.Join(cfg.DataDir, activeNetParams.Name)

	// Append the network type to the log directory so it is "namespaced"
	// per network in the same fashion as the data directory.
	cfg.LogDir = cleanAndExpandPath(cfg.LogDir)
	cfg.LogDir = filepath.Join(cfg.LogDir, activeNetParams.Name)

	// Initialize logging at the default logging level.
	initSeelogLogger(filepath.Join(cfg.LogDir, defaultLogFilename))
	setLogLevels(defaultLogLevel)

	// Parse, validate, and set debug log level(s).
	if err := parseAndSetDebugLevels(cfg.DebugLevel); err != nil {
		err := fmt.Errorf("%s: %v", funcName, err.Error())
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, usageMessage)
		return nil, err
	}

	// TODO(roasbeef): logging
	return &cfg, nil
}

// cleanAndExpandPath expands environment variables and leading ~ in the
// passed path, cleans the result, and returns it.
func cleanAndExpandPath(path string) string {
	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		homeDir := filepath.Dir(lndHomeDir)
		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but they variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}

// parseAndSetDebugLevels attempts to parse the specified debug level and set
// the levels accordingly.  An appropriate error is returned if anything is
// invalid.
func parseAndSetDebugLevels(debugLevel string) error {
	// When the specified string doesn't have any delimters, treat it as
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
				"supported subsytems %v"
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

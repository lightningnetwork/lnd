// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Copyright (C) 2015-2017 The Lightning Network Developers

package config

import (
	"net"
	"time"

	"github.com/lightningnetwork/lnd/htlcswitch/hodl"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
)

// Chain contains the configuration settings for the current chain
type Chain struct {
	Active   bool   `long:"active" description:"If the chain should be active or not."`
	ChainDir string `long:"chaindir" description:"The directory to store the chain's data within."`

	Node string `long:"node" description:"The blockchain interface to use." choice:"btcd" choice:"bitcoind" choice:"neutrino" choice:"ltcd" choice:"litecoind"`

	MainNet  bool `long:"mainnet" description:"Use the main network"`
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

// Neutrino contains the configuration settings for the Neutrino client
type Neutrino struct {
	AddPeers     []string      `short:"a" long:"addpeer" description:"Add a peer to connect with at startup"`
	ConnectPeers []string      `long:"connect" description:"Connect only to the specified peers at startup"`
	MaxPeers     int           `long:"maxpeers" description:"Max number of inbound and outbound peers"`
	BanDuration  time.Duration `long:"banduration" description:"How long to ban misbehaving peers.  Valid time units are {s, m, h}.  Minimum 1 second"`
	BanThreshold uint32        `long:"banthreshold" description:"Maximum allowed ban score before disconnecting and banning misbehaving peers."`
}

// Btcd contains the configuration settings for the Btcd client
type Btcd struct {
	Dir        string `long:"dir" description:"The base directory that contains the node's data, logs, configuration file, etc."`
	RPCHost    string `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser    string `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass    string `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	RPCCert    string `long:"rpccert" description:"File containing the daemon's certificate file"`
	RawRPCCert string `long:"rawrpccert" description:"The raw bytes of the daemon's PEM-encoded certificate chain which will be used to authenticate the RPC connection."`
}

// Bitcoind contains the configuration settings for the Bitcoind client
type Bitcoind struct {
	Dir            string `long:"dir" description:"The base directory that contains the node's data, logs, configuration file, etc."`
	RPCHost        string `long:"rpchost" description:"The daemon's rpc listening address. If a port is omitted, then the default port for the selected chain parameters will be used."`
	RPCUser        string `long:"rpcuser" description:"Username for RPC connections"`
	RPCPass        string `long:"rpcpass" default-mask:"-" description:"Password for RPC connections"`
	ZMQPubRawBlock string `long:"zmqpubrawblock" description:"The address listening for ZMQ connections to deliver raw block notifications"`
	ZMQPubRawTx    string `long:"zmqpubrawtx" description:"The address listening for ZMQ connections to deliver raw transaction notifications"`
}

// AutoPilot contains the configuration settings for the AutoPilot feature
type AutoPilot struct {
	Active         bool    `long:"active" description:"If the autopilot agent should be active or not."`
	MaxChannels    int     `long:"maxchannels" description:"The maximum number of channels that should be created"`
	Allocation     float64 `long:"allocation" description:"The percentage of total funds that should be committed to automatic channel establishment"`
	MinChannelSize int64   `long:"minchansize" description:"The smallest channel that the autopilot agent should create"`
	MaxChannelSize int64   `long:"maxchansize" description:"The largest channel that the autopilot agent should create"`
}

// Tor contains the configuration settings for the configured Tor connection
type Tor struct {
	Active           bool   `long:"active" description:"Allow outbound and inbound connections to be routed through Tor"`
	SOCKS            string `long:"socks" description:"The host:port that Tor's exposed SOCKS5 proxy is listening on"`
	DNS              string `long:"dns" description:"The DNS server as host:port that Tor will use for SRV queries - NOTE must have TCP resolution enabled"`
	StreamIsolation  bool   `long:"streamisolation" description:"Enable Tor stream isolation by randomizing user credentials for each connection."`
	Control          string `long:"control" description:"The host:port that Tor is listening on for Tor control connections"`
	V2               bool   `long:"v2" description:"Automatically set up a v2 onion service to listen for inbound connections"`
	V2PrivateKeyPath string `long:"v2privatekeypath" description:"The path to the private key of the onion service being created"`
	V3               bool   `long:"v3" description:"Use a v3 onion service to listen for inbound connections"`
}

// Config defines the configuration options for lnd.
//
// See loadConfig for further details regarding the configuration
// loading+parsing process.
type Config struct {
	ShowVersion bool `short:"V" long:"version" description:"Display version information and exit"`

	LndDir         string `long:"lnddir" description:"The base directory that contains lnd's data, logs, configuration file, etc."`
	ConfigFile     string `long:"C" long:"configfile" description:"Path to configuration file"`
	DataDir        string `short:"b" long:"datadir" description:"The directory to store lnd's data within"`
	TLSCertPath    string `long:"tlscertpath" description:"Path to write the TLS certificate for lnd's RPC and REST services"`
	TLSKeyPath     string `long:"tlskeypath" description:"Path to write the TLS private key for lnd's RPC and REST services"`
	TLSExtraIP     string `long:"tlsextraip" description:"Adds an extra ip to the generated certificate"`
	TLSExtraDomain string `long:"tlsextradomain" description:"Adds an extra domain to the generated certificate"`
	NoMacaroons    bool   `long:"no-macaroons" description:"Disable macaroon authentication"`
	AdminMacPath   string `long:"adminmacaroonpath" description:"Path to write the admin macaroon for lnd's RPC and REST services if it doesn't exist"`
	ReadMacPath    string `long:"readonlymacaroonpath" description:"Path to write the read-only macaroon for lnd's RPC and REST services if it doesn't exist"`
	InvoiceMacPath string `long:"invoicemacaroonpath" description:"Path to the invoice-only macaroon for lnd's RPC and REST services if it doesn't exist"`
	LogDir         string `long:"logdir" description:"Directory to log output."`
	MaxLogFiles    int    `long:"maxlogfiles" description:"Maximum logfiles to keep (0 for no rotation)"`
	MaxLogFileSize int    `long:"maxlogfilesize" description:"Maximum logfile size in MB"`

	// We'll parse these 'raw' string arguments into real net.Addrs in the
	// loadConfig function. We need to expose the 'raw' strings so the
	// command line library can access them.
	// Only the parsed net.Addrs should be used!
	RawRPCListeners  []string `long:"rpclisten" description:"Add an interface/port/socket to listen for RPC connections"`
	RawRESTListeners []string `long:"restlisten" description:"Add an interface/port/socket to listen for REST connections"`
	RawListeners     []string `long:"listen" description:"Add an interface/port to listen for peer connections"`
	RawExternalIPs   []string `long:"externalip" description:"Add an ip:port to the list of local addresses we claim to listen on to peers. If a port is not specified, the default (9735) will be used regardless of other parameters"`
	RPCListeners     []net.Addr
	RESTListeners    []net.Addr
	Listeners        []net.Addr
	ExternalIPs      []net.Addr
	DisableListen    bool `long:"nolisten" description:"Disable listening for incoming peer connections"`
	NAT              bool `long:"nat" description:"Toggle NAT traversal support (using either UPnP or NAT-PMP) to automatically advertise your external IP address to the network -- NOTE this does not support devices behind multiple NATs"`

	DebugLevel string `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`

	CPUProfile string `long:"cpuprofile" description:"Write CPU profile to the specified file"`

	Profile string `long:"profile" description:"Enable HTTP profiling on given port -- NOTE port must be between 1024 and 65535"`

	DebugHTLC          bool `long:"debughtlc" description:"Activate the debug htlc mode. With the debug HTLC mode, all payments sent use a pre-determined R-Hash. Additionally, all HTLCs sent to a node with the debug HTLC R-Hash are immediately settled in the next available state transition."`
	UnsafeDisconnect   bool `long:"unsafe-disconnect" description:"Allows the rpcserver to intentionally disconnect from peers with open channels. USED FOR TESTING ONLY."`
	UnsafeReplay       bool `long:"unsafe-replay" description:"Causes a link to replay the adds on its commitment txn after starting up, this enables testing of the sphinx replay logic."`
	MaxPendingChannels int  `long:"maxpendingchannels" description:"The maximum number of incoming pending channels permitted per peer."`

	Bitcoin      *Chain    `group:"Bitcoin" namespace:"bitcoin"`
	BtcdMode     *Btcd     `group:"btcd" namespace:"btcd"`
	BitcoindMode *Bitcoind `group:"bitcoind" namespace:"bitcoind"`
	NeutrinoMode *Neutrino `group:"neutrino" namespace:"neutrino"`

	Litecoin      *Chain    `group:"Litecoin" namespace:"litecoin"`
	LtcdMode      *Btcd     `group:"ltcd" namespace:"ltcd"`
	LitecoindMode *Bitcoind `group:"litecoind" namespace:"litecoind"`

	Autopilot *AutoPilot `group:"Autopilot" namespace:"autopilot"`

	Tor *Tor `group:"Tor" namespace:"tor"`

	Hodl *hodl.Config `group:"hodl" namespace:"hodl"`

	NoNetBootstrap bool `long:"nobootstrap" description:"If true, then automatic network bootstrapping will not be attempted."`

	NoEncryptWallet bool `long:"noencryptwallet" description:"If set, wallet will be encrypted using the default passphrase."`

	TrickleDelay int `long:"trickledelay" description:"Time in milliseconds between each release of announcements to the network"`

	Alias       string `long:"alias" description:"The node alias. Used as a moniker by peers and intelligence services"`
	Color       string `long:"color" description:"The color of the node in hex format (i.e. '#3399FF'). Used to customize node appearance in intelligence services"`
	MinChanSize int64  `long:"minchansize" description:"The smallest channel size (in satoshis) that we should accept. Incoming channels smaller than this will be rejected"`

	NoChanUpdates bool `long:"nochanupdates" description:"If specified, lnd will not request real-time channel updates from connected peers. This option should be used by routing nodes to save bandwidth."`

	Net tor.Net
}

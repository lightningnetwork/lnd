package lnwallet

import (
	"path/filepath"

	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcutil"
)

var (
	// TODO(roasbeef): lnwallet config file
	lnwalletHomeDir = btcutil.AppDataDir("lnwallet", false)
	defaultDataDir  = lnwalletHomeDir

	defaultLogFilename = "lnwallet.log"
	defaultLogDirname  = "logs"
	defaultLogDir      = filepath.Join(lnwalletHomeDir, defaultLogDirname)

	btcdHomeDir        = btcutil.AppDataDir("btcd", false)
	btcdHomedirCAFile  = filepath.Join(btcdHomeDir, "rpc.cert")
	defaultRPCKeyFile  = filepath.Join(lnwalletHomeDir, "rpc.key")
	defaultRPCCertFile = filepath.Join(lnwalletHomeDir, "rpc.cert")

	// defaultPubPassphrase is the default public wallet passphrase which is
	// used when the user indicates they do not want additional protection
	// provided by having all public data in the wallet encrypted by a
	// passphrase only known to them.
	defaultPubPassphrase = []byte("public")

	walletDbName = "lnwallet.db"
)

// Config...
type Config struct {
	DataDir string
	LogDir  string

	DebugLevel string

	RpcHost  string // localhost:18334
	RpcUser  string
	RpcPass  string
	RpcNoTLS bool

	RPCCert string
	RPCKey  string

	CACert []byte

	PrivatePass []byte
	PublicPass  []byte
	HdSeed      []byte

	// Which bitcoin network are we using?
	NetParams *chaincfg.Params
}

// setDefaults...
func setDefaults(confg *Config) {
}

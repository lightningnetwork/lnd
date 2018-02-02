package walletunlocker

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcwallet/wallet"
	"golang.org/x/net/context"
)

// WalletInitMsg is a message sent to the UnlockerService when a user wishes to
// set up the internal wallet for the first time. The user MUST provide a
// passphrase, but is also able to provide their own source of entropy. If
// provided, then this source of entropy will be used to generate the wallet's
// HD seed. Otherwise, the wallet will generate one itself.
type WalletInitMsg struct {
	// Passphrase is the passphrase that will be used to encrypt the wallet
	// itself. This MUST be at least 8 characters.
	Passphrase []byte

	// WalletSeed is the deciphered cipher seed that the wallet should use
	// to initialize itself.
	WalletSeed *aezeed.CipherSeed
}

// UnlockerService implements the WalletUnlocker service used to provide lnd
// with a password for wallet encryption at startup. Additionally, during
// initial setup, users can provide their own source of entropy which will be
// used to generate the seed that's ultimately used within the wallet.
type UnlockerService struct {
	// InitMsgs is a channel that carries all wallet init messages.
	InitMsgs chan *WalletInitMsg

	// UnlockPasswords is a channel where passwords provided by the rpc
	// client to be used to unlock and decrypt an existing wallet will be
	// sent.
	UnlockPasswords chan []byte

	chainDir  string
	netParams *chaincfg.Params
	authSvc   *macaroons.Service
}

// New creates and returns a new UnlockerService.
func New(authSvc *macaroons.Service, chainDir string,
	params *chaincfg.Params) *UnlockerService {

	return &UnlockerService{
		InitMsgs:        make(chan *WalletInitMsg, 1),
		UnlockPasswords: make(chan []byte, 1),
		chainDir:        chainDir,
		netParams:       params,
	}
}

// GenSeed is the first method that should be used to instantiate a new lnd
// instance. This method allows a caller to generate a new aezeed cipher seed
// given an optional passphrase. If provided, the passphrase will be necessary
// to decrypt the cipherseed to expose the internal wallet seed.
//
// Once the cipherseed is obtained and verified by the user, the InitWallet
// method should be used to commit the newly generated seed, and create the
// wallet.
func (u *UnlockerService) GenSeed(ctx context.Context,
	in *lnrpc.GenSeedRequest) (*lnrpc.GenSeedResponse, error) {

	// Before we start, we'll ensure that the wallet hasn't already created
	// so we don't show a *new* seed to the user if one already exists.
	netDir := btcwallet.NetworkDir(u.chainDir, u.netParams)
	loader := wallet.NewLoader(u.netParams, netDir)
	walletExists, err := loader.WalletExists()
	if err != nil {
		return nil, err
	}
	if walletExists {
		return nil, fmt.Errorf("wallet already exists")
	}

	var entropy [aezeed.EntropySize]byte

	switch {
	// If the user provided any entropy, then we'll make sure it's sized
	// properly.
	case len(in.SeedEntropy) != 0 && len(in.SeedEntropy) != aezeed.EntropySize:
		return nil, fmt.Errorf("incorrect entropy length: expected "+
			"16 bytes, instead got %v bytes", len(in.SeedEntropy))

	// If the user provided the correct number of bytes, then we'll copy it
	// over into our buffer for usage.
	case len(in.SeedEntropy) == aezeed.EntropySize:
		copy(entropy[:], in.SeedEntropy[:])

	// Otherwise, we'll generate a fresh new set of bytes to use as entropy
	// to generate the seed.
	default:
		if _, err := rand.Read(entropy[:]); err != nil {
			return nil, err
		}
	}

	// Now that we have our set of entropy, we'll create a new cipher seed
	// instance.
	//
	// TODO(roasbeef): should use current keychain version here
	cipherSeed, err := aezeed.New(0, &entropy, time.Now())
	if err != nil {
		return nil, err
	}

	// With our raw cipher seed obtained, we'll convert it into an encoded
	// mnemonic using the user specified pass phrase.
	mnemonic, err := cipherSeed.ToMnemonic(in.AezeedPassphrase)
	if err != nil {
		return nil, err
	}

	// Additionally, we'll also obtain the raw enciphered cipher seed as
	// well to return to the user.
	encipheredSeed, err := cipherSeed.Encipher(in.AezeedPassphrase)
	if err != nil {
		return nil, err
	}

	return &lnrpc.GenSeedResponse{
		CipherSeedMnemonic: []string(mnemonic[:]),
		EncipheredSeed:     encipheredSeed[:],
	}, nil
}

// InitWallet is used when lnd is starting up for the first time to fully
// initialize the daemon and its internal wallet. At the very least a wallet
// password must be provided. This will be used to encrypt sensitive material
// on disk.
//
// In the case of a recovery scenario, the user can also specify their aezeed
// mnemonic and passphrase. If set, then the daemon will use this prior state
// to initialize its internal wallet.
//
// Alternatively, this can be used along with the GenSeed RPC to obtain a
// seed, then present it to the user. Once it has been verified by the user,
// the seed can be fed into this RPC in order to commit the new wallet.
func (u *UnlockerService) InitWallet(ctx context.Context,
	in *lnrpc.InitWalletRequest) (*lnrpc.InitWalletResponse, error) {

	// Require the provided password to have a length of at least 8
	// characters.
	password := in.WalletPassword
	if len(password) < 8 {
		return nil, fmt.Errorf("password must have " +
			"at least 8 characters")
	}

	// We'll then open up the directory that will be used to store the
	// wallet's files so we can check if the wallet already exists.
	netDir := btcwallet.NetworkDir(u.chainDir, u.netParams)
	loader := wallet.NewLoader(u.netParams, netDir)

	walletExists, err := loader.WalletExists()
	if err != nil {
		return nil, err
	}

	// If the wallet already exists, then we'll exit early as we can't
	// create the wallet if it already exists!
	if walletExists {
		return nil, fmt.Errorf("wallet already exists")
	}

	// At this point, we know that the wallet doesn't already exist. So
	// we'll map the user provided aezeed and passphrase into a decoded
	// cipher seed instance.
	var mnemonic aezeed.Mnemonic
	copy(mnemonic[:], in.CipherSeedMnemonic[:])

	// If we're unable to map it back into the ciphertext, then either the
	// mnemonic is wrong, or the passphrase is wrong.
	cipherSeed, err := mnemonic.ToCipherSeed(in.AezeedPassphrase)
	if err != nil {
		return nil, err
	}

	// Attempt to create a password for the macaroon service.
	if u.authSvc != nil {
		err = u.authSvc.CreateUnlock(&password)
		if err != nil {
			return nil, fmt.Errorf("unable to create/unlock "+
				"macaroon store: %v", err)
		}
	}

	// With the cipher seed deciphered, and the auth service created, we'll
	// now send over the wallet password and the seed. This will allow the
	// daemon to initialize itself and startup.
	initMsg := &WalletInitMsg{
		Passphrase: password,
		WalletSeed: cipherSeed,
	}

	u.InitMsgs <- initMsg

	return &lnrpc.InitWalletResponse{}, nil
}

// UnlockWallet sends the password provided by the incoming UnlockWalletRequest
// over the UnlockPasswords channel in case it successfully decrypts an
// existing wallet found in the chain's wallet database directory.
func (u *UnlockerService) UnlockWallet(ctx context.Context,
	in *lnrpc.UnlockWalletRequest) (*lnrpc.UnlockWalletResponse, error) {

	netDir := btcwallet.NetworkDir(u.chainDir, u.netParams)
	loader := wallet.NewLoader(u.netParams, netDir)

	// Check if wallet already exists.
	walletExists, err := loader.WalletExists()
	if err != nil {
		return nil, err
	}

	if !walletExists {
		// Cannot unlock a wallet that does not exist!
		return nil, fmt.Errorf("wallet not found")
	}

	// Try opening the existing wallet with the provided password.
	_, err = loader.OpenExistingWallet(in.WalletPassword, false)
	if err != nil {
		// Could not open wallet, most likely this means that provided
		// password was incorrect.
		return nil, err
	}

	// We successfully opened the wallet, but we'll need to unload it to
	// make sure lnd can open it later.
	if err := loader.UnloadWallet(); err != nil {
		// TODO: not return error here?
		return nil, err
	}

	// Attempt to create a password for the macaroon service.
	if u.authSvc != nil {
		err = u.authSvc.CreateUnlock(&in.WalletPassword)
		if err != nil {
			return nil, fmt.Errorf("unable to create/unlock "+
				"macaroon store: %v", err)
		}
	}

	// At this point we was able to open the existing wallet with the
	// provided password. We send the password over the UnlockPasswords
	// channel, such that it can be used by lnd to open the wallet.
	u.UnlockPasswords <- in.WalletPassword

	return &lnrpc.UnlockWalletResponse{}, nil
}

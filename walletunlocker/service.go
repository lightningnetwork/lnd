package walletunlocker

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcwallet/wallet"
	"golang.org/x/net/context"
	"gopkg.in/macaroon-bakery.v1/bakery"
)

// UnlockerService implements the WalletUnlocker service used to provide lnd
// with a password for wallet encryption at startup.
type UnlockerService struct {
	// CreatePasswords is a channel where passwords provided by the rpc
	// client to be used to initially create and encrypt a wallet will
	// be sent.
	CreatePasswords chan []byte

	// UnlockPasswords is a channel where passwords provided by the rpc
	// client to be used to unlock and decrypt an existing wallet will
	// be sent.
	UnlockPasswords chan []byte

	// authSvc is the authentication/authorization service backed by
	// macaroons.
	authSvc *bakery.Service

	chainDir  string
	netParams *chaincfg.Params
}

// New creates and returns a new UnlockerService.
func New(authSvc *bakery.Service, chainDir string,
	params *chaincfg.Params) *UnlockerService {
	return &UnlockerService{
		CreatePasswords: make(chan []byte, 1),
		UnlockPasswords: make(chan []byte, 1),
		authSvc:         authSvc,
		chainDir:        chainDir,
		netParams:       params,
	}
}

// CreateWallet will read the password provided in the CreateWalletRequest
// and send it over the CreatePasswords channel in case no wallet already
// exist in the chain's wallet database directory.
func (u *UnlockerService) CreateWallet(ctx context.Context,
	in *lnrpc.CreateWalletRequest) (*lnrpc.CreateWalletResponse, error) {

	// Check macaroon to see if this is allowed.
	if u.authSvc != nil {
		if err := macaroons.ValidateMacaroon(ctx, "createwallet",
			u.authSvc); err != nil {
			return nil, err
		}
	}

	netDir := btcwallet.NetworkDir(u.chainDir, u.netParams)
	loader := wallet.NewLoader(u.netParams, netDir)

	// Check if wallet already exists.
	walletExists, err := loader.WalletExists()
	if err != nil {
		return nil, err
	}

	if walletExists {
		// Cannot create wallet if it already exists!
		return nil, fmt.Errorf("wallet already exists")
	}

	// We send the password over the CreatePasswords channel, such that it
	// can be used by lnd to open or create the wallet.
	u.CreatePasswords <- in.Password

	return &lnrpc.CreateWalletResponse{}, nil
}

// UnlockWallet sends the password provided by the incoming UnlockWalletRequest
// over the UnlockPasswords channel in case it successfully decrypts an existing
// wallet found in the chain's wallet database directory.
func (u *UnlockerService) UnlockWallet(ctx context.Context,
	in *lnrpc.UnlockWalletRequest) (*lnrpc.UnlockWalletResponse, error) {

	// Check macaroon to see if this is allowed.
	if u.authSvc != nil {
		if err := macaroons.ValidateMacaroon(ctx, "unlockwallet",
			u.authSvc); err != nil {
			return nil, err
		}
	}

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
	_, err = loader.OpenExistingWallet(in.Password, false)
	if err != nil {
		// Could not open wallet, most likely this means that
		// provided password was incorrect.
		return nil, err
	}

	// We successfully opened the wallet, but we'll need to unload
	// it to make sure lnd can open it later.
	if err := loader.UnloadWallet(); err != nil {
		// TODO: not return error here?
		return nil, err
	}

	// At this point we was able to open the existing wallet with the
	// provided password. We send the password over the UnlockPasswords
	// channel, such that it can be used by lnd to open the wallet.
	u.UnlockPasswords <- in.Password

	return &lnrpc.UnlockWalletResponse{}, nil
}

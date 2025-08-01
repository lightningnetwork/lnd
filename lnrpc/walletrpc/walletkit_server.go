//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/sweep"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

var (
	// macaroonOps are the set of capabilities that our minted macaroon (if
	// it doesn't already exist) will have.
	macaroonOps = []bakery.Op{
		{
			Entity: "address",
			Action: "write",
		},
		{
			Entity: "address",
			Action: "read",
		},
		{
			Entity: "onchain",
			Action: "write",
		},
		{
			Entity: "onchain",
			Action: "read",
		},
	}

	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/walletrpc.WalletKit/DeriveNextKey": {{
			Entity: "address",
			Action: "read",
		}},
		"/walletrpc.WalletKit/DeriveKey": {{
			Entity: "address",
			Action: "read",
		}},
		"/walletrpc.WalletKit/NextAddr": {{
			Entity: "address",
			Action: "read",
		}},
		"/walletrpc.WalletKit/GetTransaction": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/PublishTransaction": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/SendOutputs": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/EstimateFee": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/PendingSweeps": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/BumpFee": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/BumpForceCloseFee": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/ListSweeps": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/LabelTransaction": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/LeaseOutput": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/ReleaseOutput": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/ListLeases": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/ListUnspent": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/ListAddresses": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/SignMessageWithAddr": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/VerifyMessageWithAddr": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/FundPsbt": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/SignPsbt": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/FinalizePsbt": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/ListAccounts": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/RequiredReserve": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/walletrpc.WalletKit/ImportAccount": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/ImportPublicKey": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/ImportTapscript": {{
			Entity: "onchain",
			Action: "write",
		}},
		"/walletrpc.WalletKit/RemoveTransaction": {{
			Entity: "onchain",
			Action: "write",
		}},
	}

	// DefaultWalletKitMacFilename is the default name of the wallet kit
	// macaroon that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultWalletKitMacFilename = "walletkit.macaroon"

	// allWitnessTypes is a mapping between the witness types defined in the
	// `input` package, and the witness types in the protobuf definition.
	// This map is necessary because the native enum and the protobuf enum
	// are numbered differently. The protobuf enum cannot be renumbered
	// because this would break backwards compatibility with older clients,
	// and the native enum cannot be renumbered because it is stored in the
	// watchtower and BreachArbitrator databases.
	//
	//nolint:ll
	allWitnessTypes = map[input.WitnessType]WitnessType{
		input.CommitmentTimeLock:                           WitnessType_COMMITMENT_TIME_LOCK,
		input.CommitmentNoDelay:                            WitnessType_COMMITMENT_NO_DELAY,
		input.CommitmentRevoke:                             WitnessType_COMMITMENT_REVOKE,
		input.HtlcOfferedRevoke:                            WitnessType_HTLC_OFFERED_REVOKE,
		input.HtlcAcceptedRevoke:                           WitnessType_HTLC_ACCEPTED_REVOKE,
		input.HtlcOfferedTimeoutSecondLevel:                WitnessType_HTLC_OFFERED_TIMEOUT_SECOND_LEVEL,
		input.HtlcAcceptedSuccessSecondLevel:               WitnessType_HTLC_ACCEPTED_SUCCESS_SECOND_LEVEL,
		input.HtlcOfferedRemoteTimeout:                     WitnessType_HTLC_OFFERED_REMOTE_TIMEOUT,
		input.HtlcAcceptedRemoteSuccess:                    WitnessType_HTLC_ACCEPTED_REMOTE_SUCCESS,
		input.HtlcSecondLevelRevoke:                        WitnessType_HTLC_SECOND_LEVEL_REVOKE,
		input.WitnessKeyHash:                               WitnessType_WITNESS_KEY_HASH,
		input.NestedWitnessKeyHash:                         WitnessType_NESTED_WITNESS_KEY_HASH,
		input.CommitmentAnchor:                             WitnessType_COMMITMENT_ANCHOR,
		input.HtlcOfferedTimeoutSecondLevelInputConfirmed:  WitnessType_HTLC_OFFERED_TIMEOUT_SECOND_LEVEL_INPUT_CONFIRMED,
		input.HtlcAcceptedSuccessSecondLevelInputConfirmed: WitnessType_HTLC_ACCEPTED_SUCCESS_SECOND_LEVEL_INPUT_CONFIRMED,
		input.CommitSpendNoDelayTweakless:                  WitnessType_COMMITMENT_NO_DELAY_TWEAKLESS,
		input.CommitmentToRemoteConfirmed:                  WitnessType_COMMITMENT_TO_REMOTE_CONFIRMED,
		input.LeaseCommitmentTimeLock:                      WitnessType_LEASE_COMMITMENT_TIME_LOCK,
		input.LeaseCommitmentToRemoteConfirmed:             WitnessType_LEASE_COMMITMENT_TO_REMOTE_CONFIRMED,
		input.LeaseHtlcOfferedTimeoutSecondLevel:           WitnessType_LEASE_HTLC_OFFERED_TIMEOUT_SECOND_LEVEL,
		input.LeaseHtlcAcceptedSuccessSecondLevel:          WitnessType_LEASE_HTLC_ACCEPTED_SUCCESS_SECOND_LEVEL,
		input.TaprootPubKeySpend:                           WitnessType_TAPROOT_PUB_KEY_SPEND,
		input.TaprootLocalCommitSpend:                      WitnessType_TAPROOT_LOCAL_COMMIT_SPEND,
		input.TaprootRemoteCommitSpend:                     WitnessType_TAPROOT_REMOTE_COMMIT_SPEND,
		input.TaprootAnchorSweepSpend:                      WitnessType_TAPROOT_ANCHOR_SWEEP_SPEND,
		input.TaprootHtlcOfferedTimeoutSecondLevel:         WitnessType_TAPROOT_HTLC_OFFERED_TIMEOUT_SECOND_LEVEL,
		input.TaprootHtlcAcceptedSuccessSecondLevel:        WitnessType_TAPROOT_HTLC_ACCEPTED_SUCCESS_SECOND_LEVEL,
		input.TaprootHtlcSecondLevelRevoke:                 WitnessType_TAPROOT_HTLC_SECOND_LEVEL_REVOKE,
		input.TaprootHtlcAcceptedRevoke:                    WitnessType_TAPROOT_HTLC_ACCEPTED_REVOKE,
		input.TaprootHtlcOfferedRevoke:                     WitnessType_TAPROOT_HTLC_OFFERED_REVOKE,
		input.TaprootHtlcOfferedRemoteTimeout:              WitnessType_TAPROOT_HTLC_OFFERED_REMOTE_TIMEOUT,
		input.TaprootHtlcLocalOfferedTimeout:               WitnessType_TAPROOT_HTLC_LOCAL_OFFERED_TIMEOUT,
		input.TaprootHtlcAcceptedRemoteSuccess:             WitnessType_TAPROOT_HTLC_ACCEPTED_REMOTE_SUCCESS,
		input.TaprootHtlcAcceptedLocalSuccess:              WitnessType_TAPROOT_HTLC_ACCEPTED_LOCAL_SUCCESS,
		input.TaprootCommitmentRevoke:                      WitnessType_TAPROOT_COMMITMENT_REVOKE,
	}
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	WalletKitServer
}

// WalletKit is a sub-RPC server that exposes a tool kit which allows clients
// to execute common wallet operations. This includes requesting new addresses,
// keys (for contracts!), and publishing transactions.
type WalletKit struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	UnimplementedWalletKitServer

	cfg *Config
}

// A compile time check to ensure that WalletKit fully implements the
// WalletKitServer gRPC service.
var _ WalletKitServer = (*WalletKit)(nil)

// New creates a new instance of the WalletKit sub-RPC server.
func New(cfg *Config) (*WalletKit, lnrpc.MacaroonPerms, error) {
	// If the path of the wallet kit macaroon wasn't specified, then we'll
	// assume that it's found at the default network directory.
	if cfg.WalletKitMacPath == "" {
		cfg.WalletKitMacPath = filepath.Join(
			cfg.NetworkDir, DefaultWalletKitMacFilename,
		)
	}

	// Now that we know the full path of the wallet kit macaroon, we can
	// check to see if we need to create it or not. If stateless_init is set
	// then we don't write the macaroons.
	macFilePath := cfg.WalletKitMacPath
	if cfg.MacService != nil && !cfg.MacService.StatelessInit &&
		!lnrpc.FileExists(macFilePath) {

		log.Infof("Baking macaroons for WalletKit RPC Server at: %v",
			macFilePath)

		// At this point, we know that the wallet kit macaroon doesn't
		// yet, exist, so we need to create it with the help of the
		// main macaroon service.
		walletKitMac, err := cfg.MacService.NewMacaroon(
			context.Background(), macaroons.DefaultRootKeyID,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		walletKitMacBytes, err := walletKitMac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = os.WriteFile(macFilePath, walletKitMacBytes, 0644)
		if err != nil {
			_ = os.Remove(macFilePath)
			return nil, nil, err
		}
	}

	walletKit := &WalletKit{
		cfg: cfg,
	}

	return walletKit, macPermissions, nil
}

// Start launches any helper goroutines required for the sub-server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (w *WalletKit) Start() error {
	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (w *WalletKit) Stop() error {
	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (w *WalletKit) Name() string {
	return SubServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until this
// is called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterWalletKitServer(grpcServer, r)

	log.Debugf("WalletKit RPC server successfully registered with " +
		"root gRPC server")

	return nil
}

// RegisterWithRestServer will be called by the root REST mux to direct a sub
// RPC server to register itself with the main REST mux server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRestServer(ctx context.Context,
	mux *runtime.ServeMux, dest string, opts []grpc.DialOption) error {

	// We make sure that we register it with the main REST server to ensure
	// all our methods are routed properly.
	err := RegisterWalletKitHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register WalletKit REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("WalletKit REST server successfully registered with " +
		"root REST server")
	return nil
}

// CreateSubServer populates the subserver's dependencies using the passed
// SubServerConfigDispatcher. This method should fully initialize the
// sub-server instance, making it ready for action. It returns the macaroon
// permissions that the sub-server wishes to pass on to the root server for all
// methods routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) CreateSubServer(configRegistry lnrpc.SubServerConfigDispatcher) (
	lnrpc.SubServer, lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.WalletKitServer = subServer
	return subServer, macPermissions, nil
}

// internalScope returns the internal key scope.
func (w *WalletKit) internalScope() waddrmgr.KeyScope {
	return waddrmgr.KeyScope{
		Purpose: keychain.BIP0043Purpose,
		Coin:    w.cfg.ChainParams.HDCoinType,
	}
}

// ListUnspent returns useful information about each unspent output owned by
// the wallet, as reported by the underlying `ListUnspentWitness`; the
// information returned is: outpoint, amount in satoshis, address, address
// type, scriptPubKey in hex and number of confirmations. The result is
// filtered to contain outputs whose number of confirmations is between a
// minimum and maximum number of confirmations specified by the user.
func (w *WalletKit) ListUnspent(ctx context.Context,
	req *ListUnspentRequest) (*ListUnspentResponse, error) {

	// Force min_confs and max_confs to be zero if unconfirmed_only is
	// true.
	if req.UnconfirmedOnly && (req.MinConfs != 0 || req.MaxConfs != 0) {
		return nil, fmt.Errorf("min_confs and max_confs must be zero " +
			"if unconfirmed_only is true")
	}

	// When unconfirmed_only is inactive and max_confs is zero (default
	// values), we will override max_confs to be a MaxInt32, in order
	// to return all confirmed and unconfirmed utxos as a default response.
	if req.MaxConfs == 0 && !req.UnconfirmedOnly {
		req.MaxConfs = math.MaxInt32
	}

	// Validate the confirmation arguments.
	minConfs, maxConfs, err := lnrpc.ParseConfs(req.MinConfs, req.MaxConfs)
	if err != nil {
		return nil, err
	}

	// With our arguments validated, we'll query the internal wallet for
	// the set of UTXOs that match our query.
	//
	// We'll acquire the global coin selection lock to ensure there aren't
	// any other concurrent processes attempting to lock any UTXOs which may
	// be shown available to us.
	var utxos []*lnwallet.Utxo
	err = w.cfg.CoinSelectionLocker.WithCoinSelectLock(func() error {
		utxos, err = w.cfg.Wallet.ListUnspentWitness(
			minConfs, maxConfs, req.Account,
		)

		return err
	})
	if err != nil {
		return nil, err
	}

	rpcUtxos, err := lnrpc.MarshalUtxos(utxos, w.cfg.ChainParams)
	if err != nil {
		return nil, err
	}

	return &ListUnspentResponse{
		Utxos: rpcUtxos,
	}, nil
}

// LeaseOutput locks an output to the given ID, preventing it from being
// available for any future coin selection attempts. The absolute time of the
// lock's expiration is returned. The expiration of the lock can be extended by
// successive invocations of this call. Outputs can be unlocked before their
// expiration through `ReleaseOutput`.
//
// If the output is not known, wtxmgr.ErrUnknownOutput is returned. If the
// output has already been locked to a different ID, then
// wtxmgr.ErrOutputAlreadyLocked is returned.
func (w *WalletKit) LeaseOutput(ctx context.Context,
	req *LeaseOutputRequest) (*LeaseOutputResponse, error) {

	if len(req.Id) != 32 {
		return nil, errors.New("id must be 32 random bytes")
	}
	var lockID wtxmgr.LockID
	copy(lockID[:], req.Id)

	// Don't allow ID's of 32 bytes, but all zeros.
	if lockID == (wtxmgr.LockID{}) {
		return nil, errors.New("id must be 32 random bytes")
	}

	// Don't allow our internal ID to be used externally for locking. Only
	// unlocking is allowed.
	if lockID == chanfunding.LndInternalLockID {
		return nil, errors.New("reserved id cannot be used")
	}

	op, err := UnmarshallOutPoint(req.Outpoint)
	if err != nil {
		return nil, err
	}

	// Use the specified lock duration or fall back to the default.
	duration := chanfunding.DefaultLockDuration
	if req.ExpirationSeconds != 0 {
		duration = time.Duration(req.ExpirationSeconds) * time.Second
	}

	// Acquire the global coin selection lock to ensure there aren't any
	// other concurrent processes attempting to lease the same UTXO.
	var expiration time.Time
	err = w.cfg.CoinSelectionLocker.WithCoinSelectLock(func() error {
		expiration, err = w.cfg.Wallet.LeaseOutput(
			lockID, *op, duration,
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &LeaseOutputResponse{
		Expiration: uint64(expiration.Unix()),
	}, nil
}

// ReleaseOutput unlocks an output, allowing it to be available for coin
// selection if it remains unspent. The ID should match the one used to
// originally lock the output.
func (w *WalletKit) ReleaseOutput(ctx context.Context,
	req *ReleaseOutputRequest) (*ReleaseOutputResponse, error) {

	if len(req.Id) != 32 {
		return nil, errors.New("id must be 32 random bytes")
	}
	var lockID wtxmgr.LockID
	copy(lockID[:], req.Id)

	op, err := UnmarshallOutPoint(req.Outpoint)
	if err != nil {
		return nil, err
	}

	// Acquire the global coin selection lock to maintain consistency as
	// it's acquired when we initially leased the output.
	err = w.cfg.CoinSelectionLocker.WithCoinSelectLock(func() error {
		return w.cfg.Wallet.ReleaseOutput(lockID, *op)
	})
	if err != nil {
		return nil, err
	}

	return &ReleaseOutputResponse{
		Status: fmt.Sprintf("output %v released", op.String()),
	}, nil
}

// ListLeases returns a list of all currently locked utxos.
func (w *WalletKit) ListLeases(ctx context.Context,
	req *ListLeasesRequest) (*ListLeasesResponse, error) {

	leases, err := w.cfg.Wallet.ListLeasedOutputs()
	if err != nil {
		return nil, err
	}

	return &ListLeasesResponse{
		LockedUtxos: marshallLeases(leases),
	}, nil
}

// DeriveNextKey attempts to derive the *next* key within the key family
// (account in BIP43) specified. This method should return the next external
// child within this branch.
func (w *WalletKit) DeriveNextKey(ctx context.Context,
	req *KeyReq) (*signrpc.KeyDescriptor, error) {

	nextKeyDesc, err := w.cfg.KeyRing.DeriveNextKey(
		keychain.KeyFamily(req.KeyFamily),
	)
	if err != nil {
		return nil, err
	}

	return &signrpc.KeyDescriptor{
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(nextKeyDesc.Family),
			KeyIndex:  int32(nextKeyDesc.Index),
		},
		RawKeyBytes: nextKeyDesc.PubKey.SerializeCompressed(),
	}, nil
}

// DeriveKey attempts to derive an arbitrary key specified by the passed
// KeyLocator.
func (w *WalletKit) DeriveKey(ctx context.Context,
	req *signrpc.KeyLocator) (*signrpc.KeyDescriptor, error) {

	keyDesc, err := w.cfg.KeyRing.DeriveKey(keychain.KeyLocator{
		Family: keychain.KeyFamily(req.KeyFamily),
		Index:  uint32(req.KeyIndex),
	})
	if err != nil {
		return nil, err
	}

	return &signrpc.KeyDescriptor{
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(keyDesc.Family),
			KeyIndex:  int32(keyDesc.Index),
		},
		RawKeyBytes: keyDesc.PubKey.SerializeCompressed(),
	}, nil
}

// NextAddr returns the next unused address within the wallet.
func (w *WalletKit) NextAddr(ctx context.Context,
	req *AddrRequest) (*AddrResponse, error) {

	account := lnwallet.DefaultAccountName
	if req.Account != "" {
		account = req.Account
	}

	addrType := lnwallet.WitnessPubKey
	switch req.Type {
	case AddressType_NESTED_WITNESS_PUBKEY_HASH:
		addrType = lnwallet.NestedWitnessPubKey

	case AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:
		return nil, fmt.Errorf("invalid address type for next "+
			"address: %v", req.Type)

	case AddressType_TAPROOT_PUBKEY:
		addrType = lnwallet.TaprootPubkey
	}

	addr, err := w.cfg.Wallet.NewAddress(addrType, req.Change, account)
	if err != nil {
		return nil, err
	}

	return &AddrResponse{
		Addr: addr.String(),
	}, nil
}

// GetTransaction returns a transaction from the wallet given its hash.
func (w *WalletKit) GetTransaction(_ context.Context,
	req *GetTransactionRequest) (*lnrpc.Transaction, error) {

	// If the client doesn't specify a hash, then there's nothing to
	// return.
	if req.Txid == "" {
		return nil, fmt.Errorf("must provide a transaction hash")
	}

	txHash, err := chainhash.NewHashFromStr(req.Txid)
	if err != nil {
		return nil, err
	}

	res, err := w.cfg.Wallet.GetTransactionDetails(txHash)
	if err != nil {
		return nil, err
	}

	return lnrpc.RPCTransaction(res), nil
}

// Attempts to publish the passed transaction to the network. Once this returns
// without an error, the wallet will continually attempt to re-broadcast the
// transaction on start up, until it enters the chain.
func (w *WalletKit) PublishTransaction(ctx context.Context,
	req *Transaction) (*PublishResponse, error) {

	switch {
	// If the client doesn't specify a transaction, then there's nothing to
	// publish.
	case len(req.TxHex) == 0:
		return nil, fmt.Errorf("must provide a transaction to " +
			"publish")
	}

	tx := &wire.MsgTx{}
	txReader := bytes.NewReader(req.TxHex)
	if err := tx.Deserialize(txReader); err != nil {
		return nil, err
	}

	label, err := labels.ValidateAPI(req.Label)
	if err != nil {
		return nil, err
	}

	err = w.cfg.Wallet.PublishTransaction(tx, label)
	if err != nil {
		return nil, err
	}

	return &PublishResponse{}, nil
}

// RemoveTransaction attempts to remove the transaction and all of its
// descendants resulting from further spends of the outputs of the provided
// transaction id.
// NOTE: We do not remove the transaction from the rebroadcaster which might
// run in the background rebroadcasting not yet confirmed transactions. We do
// not have access to the rebroadcaster here nor should we. This command is not
// a way to remove transactions from the network. It is a way to shortcircuit
// wallet utxo housekeeping while transactions are still unconfirmed and we know
// that a transaction will never confirm because a replacement already pays
// higher fees.
func (w *WalletKit) RemoveTransaction(_ context.Context,
	req *GetTransactionRequest) (*RemoveTransactionResponse, error) {

	// If the client doesn't specify a hash, then there's nothing to
	// return.
	if req.Txid == "" {
		return nil, fmt.Errorf("must provide a transaction hash")
	}

	txHash, err := chainhash.NewHashFromStr(req.Txid)
	if err != nil {
		return nil, err
	}

	// Query the tx store of our internal wallet for the specified
	// transaction.
	res, err := w.cfg.Wallet.GetTransactionDetails(txHash)
	if err != nil {
		return nil, fmt.Errorf("transaction with txid=%v not found "+
			"in the internal wallet store", txHash)
	}

	// Only allow unconfirmed transactions to be removed because as soon
	// as a transaction is confirmed it will be evaluated by the wallet
	// again and the wallet state would be updated in case the user had
	// removed the transaction accidentally.
	if res.NumConfirmations > 0 {
		return nil, fmt.Errorf("transaction with txid=%v is already "+
			"confirmed (numConfs=%d) cannot be removed", txHash,
			res.NumConfirmations)
	}

	tx := &wire.MsgTx{}
	txReader := bytes.NewReader(res.RawTx)
	if err := tx.Deserialize(txReader); err != nil {
		return nil, err
	}

	err = w.cfg.Wallet.RemoveDescendants(tx)
	if err != nil {
		return nil, err
	}

	return &RemoveTransactionResponse{
		Status: "Successfully removed transaction",
	}, nil
}

// SendOutputs is similar to the existing sendmany call in Bitcoind, and allows
// the caller to create a transaction that sends to several outputs at once.
// This is ideal when wanting to batch create a set of transactions.
func (w *WalletKit) SendOutputs(ctx context.Context,
	req *SendOutputsRequest) (*SendOutputsResponse, error) {

	switch {
	// If the client didn't specify any outputs to create, then  we can't
	// proceed .
	case len(req.Outputs) == 0:
		return nil, fmt.Errorf("must specify at least one output " +
			"to create")
	}

	// Before we can request this transaction to be created, we'll need to
	// amp the protos back into the format that the internal wallet will
	// recognize.
	var totalOutputValue int64
	outputsToCreate := make([]*wire.TxOut, 0, len(req.Outputs))
	for _, output := range req.Outputs {
		outputsToCreate = append(outputsToCreate, &wire.TxOut{
			Value:    output.Value,
			PkScript: output.PkScript,
		})
		totalOutputValue += output.Value
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the transaction should satisfy.
	minConfs, err := lnrpc.ExtractMinConfs(
		req.MinConfs, req.SpendUnconfirmed,
	)
	if err != nil {
		return nil, err
	}

	// Before sending out funds we need to ensure that the remainder of our
	// wallet funds would cover for the anchor reserve requirement. We'll
	// also take unconfirmed funds into account.
	walletBalance, err := w.cfg.Wallet.ConfirmedBalance(
		0, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}

	// We'll get the currently required reserve amount.
	reserve, err := w.RequiredReserve(ctx, &RequiredReserveRequest{})
	if err != nil {
		return nil, err
	}

	// Then we check if our current wallet balance undershoots the required
	// reserve if we'd send out the outputs specified in the request.
	if int64(walletBalance)-totalOutputValue < reserve.RequiredReserve {
		return nil, ErrInsufficientReserve
	}

	label, err := labels.ValidateAPI(req.Label)
	if err != nil {
		return nil, err
	}

	coinSelectionStrategy, err := lnrpc.UnmarshallCoinSelectionStrategy(
		req.CoinSelectionStrategy, w.cfg.CoinSelectionStrategy,
	)
	if err != nil {
		return nil, err
	}

	// Now that we have the outputs mapped and checked for the reserve
	// requirement, we can request that the wallet attempts to create this
	// transaction.
	tx, err := w.cfg.Wallet.SendOutputs(
		nil, outputsToCreate, chainfee.SatPerKWeight(req.SatPerKw),
		minConfs, label, coinSelectionStrategy,
	)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		return nil, err
	}

	return &SendOutputsResponse{
		RawTx: b.Bytes(),
	}, nil
}

// EstimateFee attempts to query the internal fee estimator of the wallet to
// determine the fee (in sat/kw) to attach to a transaction in order to achieve
// the confirmation target.
func (w *WalletKit) EstimateFee(ctx context.Context,
	req *EstimateFeeRequest) (*EstimateFeeResponse, error) {

	// A confirmation target of zero or lower doesn't make any sense.
	if req.ConfTarget <= 0 {
		return nil, fmt.Errorf("confirmation target must be greater " +
			"than 0")
	}

	satPerKw, err := w.cfg.FeeEstimator.EstimateFeePerKW(
		uint32(req.ConfTarget),
	)
	if err != nil {
		return nil, err
	}

	relayFeePerKw := w.cfg.FeeEstimator.RelayFeePerKW()

	return &EstimateFeeResponse{
		SatPerKw:            int64(satPerKw),
		MinRelayFeeSatPerKw: int64(relayFeePerKw),
	}, nil
}

// PendingSweeps returns lists of on-chain outputs that lnd is currently
// attempting to sweep within its central batching engine. Outputs with similar
// fee rates are batched together in order to sweep them within a single
// transaction. The fee rate of each sweeping transaction is determined by
// taking the average fee rate of all the outputs it's trying to sweep.
func (w *WalletKit) PendingSweeps(ctx context.Context,
	in *PendingSweepsRequest) (*PendingSweepsResponse, error) {

	// Retrieve all of the outputs the UtxoSweeper is currently trying to
	// sweep.
	inputsMap, err := w.cfg.Sweeper.PendingInputs()
	if err != nil {
		return nil, err
	}

	// Convert them into their respective RPC format.
	rpcPendingSweeps := make([]*PendingSweep, 0, len(inputsMap))
	for _, inp := range inputsMap {
		witnessType, ok := allWitnessTypes[inp.WitnessType]
		if !ok {
			return nil, fmt.Errorf("unhandled witness type %v for "+
				"input %v", inp.WitnessType, inp.OutPoint)
		}

		op := lnrpc.MarshalOutPoint(&inp.OutPoint)
		amountSat := uint32(inp.Amount)
		satPerVbyte := uint64(inp.LastFeeRate.FeePerVByte())
		broadcastAttempts := uint32(inp.BroadcastAttempts)

		// Get the requested starting fee rate, if set.
		startingFeeRate := fn.MapOptionZ(
			inp.Params.StartingFeeRate,
			func(feeRate chainfee.SatPerKWeight) uint64 {
				return uint64(feeRate.FeePerVByte())
			})

		ps := &PendingSweep{
			Outpoint:             op,
			WitnessType:          witnessType,
			AmountSat:            amountSat,
			SatPerVbyte:          satPerVbyte,
			BroadcastAttempts:    broadcastAttempts,
			Immediate:            inp.Params.Immediate,
			Budget:               uint64(inp.Params.Budget),
			DeadlineHeight:       inp.DeadlineHeight,
			RequestedSatPerVbyte: startingFeeRate,
			MaturityHeight:       inp.MaturityHeight,
		}
		rpcPendingSweeps = append(rpcPendingSweeps, ps)
	}

	return &PendingSweepsResponse{
		PendingSweeps: rpcPendingSweeps,
	}, nil
}

// UnmarshallOutPoint converts an outpoint from its lnrpc type to its canonical
// type.
func UnmarshallOutPoint(op *lnrpc.OutPoint) (*wire.OutPoint, error) {
	if op == nil {
		return nil, fmt.Errorf("empty outpoint provided")
	}

	var hash chainhash.Hash
	switch {
	// Return an error if both txid fields are unpopulated.
	case len(op.TxidBytes) == 0 && len(op.TxidStr) == 0:
		return nil, fmt.Errorf("TxidBytes and TxidStr are both " +
			"unspecified")

	// The hash was provided as raw bytes.
	case len(op.TxidBytes) != 0:
		copy(hash[:], op.TxidBytes)

	// The hash was provided as a hex-encoded string.
	case len(op.TxidStr) != 0:
		h, err := chainhash.NewHashFromStr(op.TxidStr)
		if err != nil {
			return nil, err
		}
		hash = *h
	}

	return &wire.OutPoint{
		Hash:  hash,
		Index: op.OutputIndex,
	}, nil
}

// validateBumpFeeRequest makes sure the deprecated fields are not used when
// the new fields are set.
func validateBumpFeeRequest(in *BumpFeeRequest, estimator chainfee.Estimator) (
	fn.Option[chainfee.SatPerKWeight], bool, error) {

	// Get the specified fee rate if set.
	satPerKwOpt := fn.None[chainfee.SatPerKWeight]()

	// We only allow using either the deprecated field or the new field.
	switch {
	case in.SatPerByte != 0 && in.SatPerVbyte != 0:
		return satPerKwOpt, false, fmt.Errorf("either SatPerByte or " +
			"SatPerVbyte should be set, but not both")

	case in.SatPerByte != 0:
		satPerKw := chainfee.SatPerVByte(
			in.SatPerByte,
		).FeePerKWeight()
		satPerKwOpt = fn.Some(satPerKw)

	case in.SatPerVbyte != 0:
		satPerKw := chainfee.SatPerVByte(
			in.SatPerVbyte,
		).FeePerKWeight()
		satPerKwOpt = fn.Some(satPerKw)
	}

	// We make sure either the conf target or the exact fee rate is
	// specified for the starting fee of the fee function.
	if in.TargetConf != 0 && !satPerKwOpt.IsNone() {
		return satPerKwOpt, false,
			fmt.Errorf("either TargetConf or SatPerVbyte should " +
				"be set, to specify the starting fee rate of " +
				"the fee function")
	}

	// In case the user specified a conf target, we estimate the fee rate
	// for the given target using the provided estimator.
	if in.TargetConf != 0 {
		startingFeeRate, err := estimator.EstimateFeePerKW(
			in.TargetConf,
		)
		if err != nil {
			return satPerKwOpt, false, fmt.Errorf("unable to "+
				"estimate fee rate for target conf %d: %w",
				in.TargetConf, err)
		}

		// Set the starting fee rate to the estimated fee rate.
		satPerKwOpt = fn.Some(startingFeeRate)
	}

	var immediate bool
	switch {
	case in.Force && in.Immediate:
		return satPerKwOpt, false, fmt.Errorf("either Force or " +
			"Immediate should be set, but not both")

	case in.Force:
		immediate = in.Force

	case in.Immediate:
		immediate = in.Immediate
	}

	if in.DeadlineDelta != 0 && in.Budget == 0 {
		return satPerKwOpt, immediate, fmt.Errorf("budget must be " +
			"set if deadline-delta is set")
	}

	return satPerKwOpt, immediate, nil
}

// prepareSweepParams creates the sweep params to be used for the sweeper. It
// returns the new params and a bool indicating whether this is an existing
// input.
func (w *WalletKit) prepareSweepParams(in *BumpFeeRequest,
	op wire.OutPoint, currentHeight int32) (sweep.Params, bool, error) {

	// Return an error if the bump fee request is invalid.
	feeRate, immediate, err := validateBumpFeeRequest(
		in, w.cfg.FeeEstimator,
	)
	if err != nil {
		return sweep.Params{}, false, err
	}

	// Get the current pending inputs.
	inputMap, err := w.cfg.Sweeper.PendingInputs()
	if err != nil {
		return sweep.Params{}, false, fmt.Errorf("unable to get "+
			"pending inputs: %w", err)
	}

	// Find the pending input.
	//
	// TODO(yy): act differently based on the state of the input?
	inp, ok := inputMap[op]

	if !ok {
		// NOTE: if this input doesn't exist and the new budget is not
		// specified, the params would have a zero budget.
		params := sweep.Params{
			Immediate:       immediate,
			StartingFeeRate: feeRate,
			Budget:          btcutil.Amount(in.Budget),
		}

		if in.DeadlineDelta != 0 {
			params.DeadlineHeight = fn.Some(
				int32(in.DeadlineDelta) + currentHeight,
			)
		}

		return params, ok, nil
	}

	// Find the existing budget used for this input. Note that this value
	// must be greater than zero.
	budget := inp.Params.Budget

	// Set the new budget if specified. If a new deadline delta is
	// specified we also require the budget value which is checked in the
	// validateBumpFeeRequest function.
	if in.Budget != 0 {
		budget = btcutil.Amount(in.Budget)
	}

	// For an existing input, we assign it first, then overwrite it if
	// a deadline is requested.
	deadline := inp.Params.DeadlineHeight

	// Set the deadline if it was specified.
	//
	// TODO(yy): upgrade `falafel` so we can make this field optional. Atm
	// we cannot distinguish between user's not setting the field and
	// setting it to 0.
	if in.DeadlineDelta != 0 {
		deadline = fn.Some(int32(in.DeadlineDelta) + currentHeight)
	}

	startingFeeRate := inp.Params.StartingFeeRate

	// We only set the starting fee rate if it was specified else we keep
	// the existing one.
	if feeRate.IsSome() {
		startingFeeRate = feeRate
	}

	// Prepare the new sweep params.
	//
	// NOTE: if this input doesn't exist and the new budget is not
	// specified, the params would have a zero budget.
	params := sweep.Params{
		Immediate:       immediate,
		DeadlineHeight:  deadline,
		StartingFeeRate: startingFeeRate,
		Budget:          budget,
	}

	log.Infof("[BumpFee]: bumping fee for existing input=%v, old "+
		"params=%v, new params=%v", op, inp.Params, params)

	return params, ok, nil
}

// BumpFee allows bumping the fee rate of an arbitrary input. A fee preference
// can be expressed either as a specific fee rate or a delta of blocks in which
// the output should be swept on-chain within. If a fee preference is not
// explicitly specified, then an error is returned. The status of the input
// sweep can be checked through the PendingSweeps RPC.
func (w *WalletKit) BumpFee(ctx context.Context,
	in *BumpFeeRequest) (*BumpFeeResponse, error) {

	// Parse the outpoint from the request.
	op, err := UnmarshallOutPoint(in.Outpoint)
	if err != nil {
		return nil, err
	}

	// Get the current height so we can calculate the deadline height.
	_, currentHeight, err := w.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve current height: %w",
			err)
	}

	// We now create a new sweeping params and update it in the sweeper.
	// This will complicate the RBF conditions if this input has already
	// been offered to sweeper before and it has already been included in a
	// tx with other inputs. If this is the case, two results are possible:
	// - either this input successfully RBFed the existing tx, or,
	// - the budget of this input was not enough to RBF the existing tx.
	params, existing, err := w.prepareSweepParams(in, *op, currentHeight)
	if err != nil {
		return nil, err
	}

	// If this input exists, we will update its params.
	if existing {
		_, err = w.cfg.Sweeper.UpdateParams(*op, params)
		if err != nil {
			return nil, err
		}

		return &BumpFeeResponse{
			Status: "Successfully registered rbf-tx with sweeper",
		}, nil
	}

	// Otherwise, create a new sweeping request for this input.
	err = w.sweepNewInput(op, uint32(currentHeight), params)
	if err != nil {
		return nil, err
	}

	return &BumpFeeResponse{
		Status: "Successfully registered CPFP-tx with the sweeper",
	}, nil
}

// getWaitingCloseChannel returns the waiting close channel in case it does
// exist in the underlying channel state database.
func (w *WalletKit) getWaitingCloseChannel(
	chanPoint wire.OutPoint) (*channeldb.OpenChannel, error) {

	// Fetch all channels, which still have their commitment transaction not
	// confirmed (waiting close channels).
	chans, err := w.cfg.ChanStateDB.FetchWaitingCloseChannels()
	if err != nil {
		return nil, err
	}

	channel := fn.Find(chans, func(c *channeldb.OpenChannel) bool {
		return c.FundingOutpoint == chanPoint
	})

	return channel.UnwrapOrErr(errors.New("channel not found"))
}

// BumpForceCloseFee bumps the fee rate of an unconfirmed anchor channel. It
// updates the new fee rate parameters with the sweeper subsystem. Additionally
// it will try to create anchor cpfp transactions for all possible commitment
// transactions (local, remote, remote-dangling) so depending on which
// commitment is in the local mempool only one of them will succeed in being
// broadcasted.
func (w *WalletKit) BumpForceCloseFee(_ context.Context,
	in *BumpForceCloseFeeRequest) (*BumpForceCloseFeeResponse, error) {

	if in.ChanPoint == nil {
		return nil, fmt.Errorf("no chan_point provided")
	}

	lnrpcOutpoint, err := lnrpc.GetChannelOutPoint(in.ChanPoint)
	if err != nil {
		return nil, err
	}

	outPoint, err := UnmarshallOutPoint(lnrpcOutpoint)
	if err != nil {
		return nil, err
	}

	// Get the relevant channel if it is in the waiting close state.
	channel, err := w.getWaitingCloseChannel(*outPoint)
	if err != nil {
		return nil, err
	}

	if !channel.ChanType.HasAnchors() {
		return nil, fmt.Errorf("not able to bump the fee of a " +
			"non-anchor channel")
	}

	// Match pending sweeps with commitments of the channel for which a bump
	// is requested. Depending on the commitment state when force closing
	// the channel we might have up to 3 commitments to consider when
	// bumping the fee.
	commitSet := fn.NewSet[chainhash.Hash]()

	if channel.LocalCommitment.CommitTx != nil {
		localTxID := channel.LocalCommitment.CommitTx.TxHash()
		commitSet.Add(localTxID)
	}

	if channel.RemoteCommitment.CommitTx != nil {
		remoteTxID := channel.RemoteCommitment.CommitTx.TxHash()
		commitSet.Add(remoteTxID)
	}

	// Check whether there was a dangling commitment at the time the channel
	// was force closed.
	remoteCommitDiff, err := channel.RemoteCommitChainTip()
	if err != nil && !errors.Is(err, channeldb.ErrNoPendingCommit) {
		return nil, err
	}

	if remoteCommitDiff != nil {
		hash := remoteCommitDiff.Commitment.CommitTx.TxHash()
		commitSet.Add(hash)
	}

	// Retrieve all of the outputs the UtxoSweeper is currently trying to
	// sweep.
	inputsMap, err := w.cfg.Sweeper.PendingInputs()
	if err != nil {
		return nil, err
	}

	// Get the current height so we can calculate the deadline height.
	_, currentHeight, err := w.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve current height: %w",
			err)
	}

	pendingSweeps := slices.Collect(maps.Values(inputsMap))

	// Discard everything except for the anchor sweeps.
	anchors := fn.Filter(
		pendingSweeps,
		func(sweep *sweep.PendingInputResponse) bool {
			// Only filter for anchor inputs because these are the
			// only inputs which can be used to bump a closed
			// unconfirmed commitment transaction.
			isCommitAnchor := sweep.WitnessType ==
				input.CommitmentAnchor
			isTaprootSweepSpend := sweep.WitnessType ==
				input.TaprootAnchorSweepSpend
			if !isCommitAnchor && !isTaprootSweepSpend {
				return false
			}

			return commitSet.Contains(sweep.OutPoint.Hash)
		},
	)

	if len(anchors) == 0 {
		return nil, fmt.Errorf("unable to find pending anchor outputs")
	}

	// Filter all relevant anchor sweeps and update the sweep request.
	for _, anchor := range anchors {
		// Anchor cpfp bump request are predictable because they are
		// swept separately hence not batched with other sweeps (they
		// are marked with the exclusive group flag). Bumping the fee
		// rate does not create any conflicting fee bump conditions.
		// Either the rbf requirements are met or the bump is rejected
		// by the mempool rules.
		params, existing, err := w.prepareSweepParams(
			&BumpFeeRequest{
				Outpoint:      lnrpcOutpoint,
				TargetConf:    in.TargetConf,
				SatPerVbyte:   in.StartingFeerate,
				Immediate:     in.Immediate,
				Budget:        in.Budget,
				DeadlineDelta: in.DeadlineDelta,
			}, anchor.OutPoint, currentHeight,
		)
		if err != nil {
			return nil, err
		}

		// There might be the case when an anchor sweep is confirmed
		// between fetching the pending sweeps and preparing the sweep
		// params. We log this case and proceed.
		if !existing {
			log.Errorf("Sweep anchor input(%v) not known to the " +
				"sweeper subsystem")
			continue
		}

		_, err = w.cfg.Sweeper.UpdateParams(anchor.OutPoint, params)
		if err != nil {
			return nil, err
		}
	}

	return &BumpForceCloseFeeResponse{
		Status: "Successfully registered anchor-cpfp transaction to" +
			"bump channel force close transaction",
	}, nil
}

// sweepNewInput handles the case where an input is seen the first time by the
// sweeper. It will fetch the output from the wallet and construct an input and
// offer it to the sweeper.
//
// NOTE: if the budget is not set, the default budget ratio is used.
func (w *WalletKit) sweepNewInput(op *wire.OutPoint, currentHeight uint32,
	params sweep.Params) error {

	log.Debugf("Attempting to sweep outpoint %s", op)

	// Since the sweeper is not aware of the input, we'll assume the user
	// is attempting to bump an unconfirmed transaction's fee rate by
	// sweeping an output within it under control of the wallet with a
	// higher fee rate. In this case, this will be a CPFP.
	//
	// We'll gather all of the information required by the UtxoSweeper in
	// order to sweep the output.
	utxo, err := w.cfg.Wallet.FetchOutpointInfo(op)
	if err != nil {
		return err
	}

	// We're only able to bump the fee of unconfirmed transactions.
	if utxo.Confirmations > 0 {
		return errors.New("unable to bump fee of a confirmed " +
			"transaction")
	}

	// TODO(ziggie): The budget value should ideally only be set for CPFP
	// requests because for RBF requests we should have already registered
	// the input including the budget value in the first place. However it
	// might not be set and then depending on the deadline delta fee
	// estimations might become too aggressive. So need to evaluate whether
	// we set a default value here, make it configurable or fail request
	// in that case.
	if params.Budget == 0 {
		params.Budget = utxo.Value.MulF64(
			contractcourt.DefaultBudgetRatio,
		)

		log.Warnf("[BumpFee]: setting default budget value of %v for "+
			"input=%v, which will be used for the maximum fee "+
			"rate estimation (budget was not specified)",
			params.Budget, op)
	}

	signDesc := &input.SignDescriptor{
		Output: &wire.TxOut{
			PkScript: utxo.PkScript,
			Value:    int64(utxo.Value),
		},
		HashType: txscript.SigHashAll,
	}

	var witnessType input.WitnessType
	switch utxo.AddressType {
	case lnwallet.WitnessPubKey:
		witnessType = input.WitnessKeyHash
	case lnwallet.NestedWitnessPubKey:
		witnessType = input.NestedWitnessKeyHash
	case lnwallet.TaprootPubkey:
		witnessType = input.TaprootPubKeySpend
		signDesc.HashType = txscript.SigHashDefault
	default:
		return fmt.Errorf("unknown input witness %v", op)
	}

	log.Infof("[BumpFee]: bumping fee for new input=%v, params=%v", op,
		params)

	inp := input.NewBaseInput(op, witnessType, signDesc, currentHeight)
	if _, err = w.cfg.Sweeper.SweepInput(inp, params); err != nil {
		return err
	}

	return nil
}

// ListSweeps returns a list of the sweeps that our node has published.
func (w *WalletKit) ListSweeps(ctx context.Context,
	in *ListSweepsRequest) (*ListSweepsResponse, error) {

	sweeps, err := w.cfg.Sweeper.ListSweeps()
	if err != nil {
		return nil, err
	}

	sweepTxns := make(map[string]bool)
	for _, sweep := range sweeps {
		sweepTxns[sweep.String()] = true
	}

	// Some of our sweeps could have been replaced by fee, or dropped out
	// of the mempool. Here, we lookup our wallet transactions so that we
	// can match our list of sweeps against the list of transactions that
	// the wallet is still tracking. Sweeps are currently always swept to
	// the default wallet account.
	txns, firstIdx, lastIdx, err := w.cfg.Wallet.ListTransactionDetails(
		in.StartHeight, btcwallet.UnconfirmedHeight,
		lnwallet.DefaultAccountName, 0, 0,
	)
	if err != nil {
		return nil, err
	}

	var (
		txids     []string
		txDetails []*lnwallet.TransactionDetail
	)

	for _, tx := range txns {
		_, ok := sweepTxns[tx.Hash.String()]
		if !ok {
			continue
		}

		// Add the txid or full tx details depending on whether we want
		// verbose output or not.
		if in.Verbose {
			txDetails = append(txDetails, tx)
		} else {
			txids = append(txids, tx.Hash.String())
		}
	}

	if in.Verbose {
		return &ListSweepsResponse{
			Sweeps: &ListSweepsResponse_TransactionDetails{
				TransactionDetails: lnrpc.RPCTransactionDetails(
					txDetails, firstIdx, lastIdx,
				),
			},
		}, nil
	}

	return &ListSweepsResponse{
		Sweeps: &ListSweepsResponse_TransactionIds{
			TransactionIds: &ListSweepsResponse_TransactionIDs{
				TransactionIds: txids,
			},
		},
	}, nil
}

// LabelTransaction adds a label to a transaction.
func (w *WalletKit) LabelTransaction(ctx context.Context,
	req *LabelTransactionRequest) (*LabelTransactionResponse, error) {

	// Check that the label provided in non-zero.
	if len(req.Label) == 0 {
		return nil, ErrZeroLabel
	}

	// Validate the length of the non-zero label. We do not need to use the
	// label returned here, because the original is non-zero so will not
	// be replaced.
	if _, err := labels.ValidateAPI(req.Label); err != nil {
		return nil, err
	}

	hash, err := chainhash.NewHash(req.Txid)
	if err != nil {
		return nil, err
	}

	err = w.cfg.Wallet.LabelTransaction(*hash, req.Label, req.Overwrite)

	return &LabelTransactionResponse{
		Status: fmt.Sprintf("transaction label '%s' added", req.Label),
	}, err
}

// FundPsbt creates a fully populated PSBT that contains enough inputs to fund
// the outputs specified in the template. There are three ways a user can
// specify what we call the template (a list of inputs and outputs to use in the
// PSBT): Either as a PSBT packet directly with no coin selection (using the
// legacy "psbt" field), a PSBT with advanced coin selection support (using the
// new "coin_select" field) or as a raw RPC message (using the "raw" field).
// The legacy "psbt" and "raw" modes, the following restrictions apply:
//  1. If there are no inputs specified in the template, coin selection is
//     performed automatically.
//  2. If the template does contain any inputs, it is assumed that full coin
//     selection happened externally and no additional inputs are added. If the
//     specified inputs aren't enough to fund the outputs with the given fee
//     rate, an error is returned.
//
// The new "coin_select" mode does not have these restrictions and allows the
// user to specify a PSBT with inputs and outputs and still perform coin
// selection on top of that.
// For all modes this RPC requires any inputs that are specified to be locked by
// the user (if they belong to this node in the first place).
// After either selecting or verifying the inputs, all input UTXOs are locked
// with an internal app ID. A custom address type for change can be specified
// for default accounts and single imported public keys (only P2TR for now).
// Otherwise, P2WPKH will be used by default. No custom address type should be
// provided for custom accounts as we will always generate the change address
// using the coin selection key scope.
//
// NOTE: If this method returns without an error, it is the caller's
// responsibility to either spend the locked UTXOs (by finalizing and then
// publishing the transaction) or to unlock/release the locked UTXOs in case of
// an error on the caller's side.
func (w *WalletKit) FundPsbt(_ context.Context,
	req *FundPsbtRequest) (*FundPsbtResponse, error) {

	coinSelectionStrategy, err := lnrpc.UnmarshallCoinSelectionStrategy(
		req.CoinSelectionStrategy, w.cfg.CoinSelectionStrategy,
	)
	if err != nil {
		return nil, err
	}

	// Determine the desired transaction fee.
	var feeSatPerKW chainfee.SatPerKWeight
	switch {
	// Estimate the fee by the target number of blocks to confirmation.
	case req.GetTargetConf() != 0:
		targetConf := req.GetTargetConf()
		if targetConf < 1 {
			return nil, fmt.Errorf("confirmation target must be " +
				"greater than 0")
		}

		feeSatPerKW, err = w.cfg.FeeEstimator.EstimateFeePerKW(
			targetConf,
		)
		if err != nil {
			return nil, fmt.Errorf("could not estimate fee: %w",
				err)
		}

	// Convert the fee to sat/kW from the specified sat/vByte.
	case req.GetSatPerVbyte() != 0:
		feeSatPerKW = chainfee.SatPerKVByte(
			req.GetSatPerVbyte() * 1000,
		).FeePerKWeight()

	case req.GetSatPerKw() != 0:
		feeSatPerKW = chainfee.SatPerKWeight(req.GetSatPerKw())

	default:
		return nil, fmt.Errorf("fee definition missing, need to " +
			"specify either target_conf, sat_per_vbyte or " +
			"sat_per_kw")
	}

	// Then, we'll extract the minimum number of confirmations that each
	// output we use to fund the transaction should satisfy.
	minConfs, err := lnrpc.ExtractMinConfs(
		req.GetMinConfs(), req.GetSpendUnconfirmed(),
	)
	if err != nil {
		return nil, err
	}

	// We'll assume the PSBT will be funded by the default account unless
	// otherwise specified.
	account := lnwallet.DefaultAccountName
	if req.Account != "" {
		account = req.Account
	}

	var customLockID *wtxmgr.LockID
	if len(req.CustomLockId) > 0 {
		lockID := wtxmgr.LockID{}
		if len(req.CustomLockId) != len(lockID) {
			return nil, fmt.Errorf("custom lock ID must be " +
				"exactly 32 bytes")
		}

		copy(lockID[:], req.CustomLockId)
		customLockID = &lockID
	}

	var customLockDuration time.Duration
	if req.LockExpirationSeconds != 0 {
		customLockDuration = time.Duration(req.LockExpirationSeconds) *
			time.Second
	}

	// There are three ways a user can specify what we call the template (a
	// list of inputs and outputs to use in the PSBT): Either as a PSBT
	// packet directly with no coin selection, a PSBT with coin selection or
	// as a special RPC message. Find out which one the user wants to use,
	// they are mutually exclusive.
	switch {
	// The template is specified as a PSBT. All we have to do is parse it.
	case req.GetPsbt() != nil:
		r := bytes.NewReader(req.GetPsbt())
		packet, err := psbt.NewFromRawBytes(r, false)
		if err != nil {
			return nil, fmt.Errorf("could not parse PSBT: %w", err)
		}

		// Run the actual funding process now, using the internal
		// wallet.
		return w.fundPsbtInternalWallet(
			account, keyScopeFromChangeAddressType(req.ChangeType),
			packet, minConfs, feeSatPerKW, coinSelectionStrategy,
			customLockID, customLockDuration,
		)

	// The template is specified as a PSBT with the intention to perform
	// coin selection even if inputs are already present.
	case req.GetCoinSelect() != nil:
		coinSelectRequest := req.GetCoinSelect()
		r := bytes.NewReader(coinSelectRequest.Psbt)
		packet, err := psbt.NewFromRawBytes(r, false)
		if err != nil {
			return nil, fmt.Errorf("could not parse PSBT: %w", err)
		}

		numOutputs := int32(len(packet.UnsignedTx.TxOut))
		if numOutputs == 0 {
			return nil, fmt.Errorf("no outputs specified in " +
				"template")
		}

		outputSum := int64(0)
		for _, txOut := range packet.UnsignedTx.TxOut {
			outputSum += txOut.Value
		}
		if outputSum <= 0 {
			return nil, fmt.Errorf("output sum must be positive")
		}

		var (
			changeIndex int32 = -1
			changeType  chanfunding.ChangeAddressType
		)
		switch t := coinSelectRequest.ChangeOutput.(type) {
		// The user wants to use an existing output as change output.
		case *PsbtCoinSelect_ExistingOutputIndex:
			if t.ExistingOutputIndex < 0 ||
				t.ExistingOutputIndex >= numOutputs {

				return nil, fmt.Errorf("change output index "+
					"out of range: %d",
					t.ExistingOutputIndex)
			}

			changeIndex = t.ExistingOutputIndex

			changeOut := packet.UnsignedTx.TxOut[changeIndex]
			_, err := txscript.ParsePkScript(changeOut.PkScript)
			if err != nil {
				return nil, fmt.Errorf("error parsing change "+
					"script: %w", err)
			}

			changeType = chanfunding.ExistingChangeAddress

		// The user wants to use a new output as change output.
		case *PsbtCoinSelect_Add:
			// We already set the change index to -1 above to
			// indicate no change output should be used if possible
			// or a new one should be created if needed. So we only
			// need to parse the type of change output we want to
			// create.
			switch req.ChangeType {
			case ChangeAddressType_CHANGE_ADDRESS_TYPE_P2TR:
				changeType = chanfunding.P2TRChangeAddress

			default:
				changeType = chanfunding.P2WKHChangeAddress
			}

		default:
			return nil, fmt.Errorf("unknown change output type")
		}

		maxFeeRatio := chanfunding.DefaultMaxFeeRatio

		if req.MaxFeeRatio != 0 {
			maxFeeRatio = req.MaxFeeRatio
		}

		// Run the actual funding process now, using the channel funding
		// coin selection algorithm.
		return w.fundPsbtCoinSelect(
			account, changeIndex, packet, minConfs, changeType,
			feeSatPerKW, coinSelectionStrategy, maxFeeRatio,
			customLockID, customLockDuration,
		)

	// The template is specified as a RPC message. We need to create a new
	// PSBT and copy the RPC information over.
	case req.GetRaw() != nil:
		tpl := req.GetRaw()

		txOut := make([]*wire.TxOut, 0, len(tpl.Outputs))
		for addrStr, amt := range tpl.Outputs {
			addr, err := btcutil.DecodeAddress(
				addrStr, w.cfg.ChainParams,
			)
			if err != nil {
				return nil, fmt.Errorf("error parsing address "+
					"%s for network %s: %v", addrStr,
					w.cfg.ChainParams.Name, err)
			}

			if !addr.IsForNet(w.cfg.ChainParams) {
				return nil, fmt.Errorf("address is not for %s",
					w.cfg.ChainParams.Name)
			}

			pkScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return nil, fmt.Errorf("error getting pk "+
					"script for address %s: %w", addrStr,
					err)
			}

			txOut = append(txOut, &wire.TxOut{
				Value:    int64(amt),
				PkScript: pkScript,
			})
		}

		txIn := make([]*wire.OutPoint, len(tpl.Inputs))
		for idx, in := range tpl.Inputs {
			op, err := UnmarshallOutPoint(in)
			if err != nil {
				return nil, fmt.Errorf("error parsing "+
					"outpoint: %w", err)
			}
			txIn[idx] = op
		}

		sequences := make([]uint32, len(txIn))
		packet, err := psbt.New(txIn, txOut, 2, 0, sequences)
		if err != nil {
			return nil, fmt.Errorf("could not create PSBT: %w", err)
		}

		// Run the actual funding process now, using the internal
		// wallet.
		return w.fundPsbtInternalWallet(
			account, keyScopeFromChangeAddressType(req.ChangeType),
			packet, minConfs, feeSatPerKW, coinSelectionStrategy,
			customLockID, customLockDuration,
		)

	default:
		return nil, fmt.Errorf("transaction template missing, need " +
			"to specify either PSBT or raw TX template")
	}
}

// fundPsbtInternalWallet uses the "old" PSBT funding method of the internal
// wallet that does not allow specifying custom inputs while selecting coins.
func (w *WalletKit) fundPsbtInternalWallet(account string,
	keyScope *waddrmgr.KeyScope, packet *psbt.Packet, minConfs int32,
	feeSatPerKW chainfee.SatPerKWeight, strategy base.CoinSelectionStrategy,
	customLockID *wtxmgr.LockID, customLockDuration time.Duration) (
	*FundPsbtResponse, error) {

	// The RPC parsing part is now over. Several of the following operations
	// require us to hold the global coin selection lock, so we do the rest
	// of the tasks while holding the lock. The result is a list of locked
	// UTXOs.
	var response *FundPsbtResponse
	err := w.cfg.CoinSelectionLocker.WithCoinSelectLock(func() error {
		// In case the user did specify inputs, we need to make sure
		// they are known to us, still unspent and not yet locked.
		if len(packet.UnsignedTx.TxIn) > 0 {
			// Get a list of all unspent witness outputs.
			utxos, err := w.cfg.Wallet.ListUnspentWitness(
				minConfs, defaultMaxConf, account,
			)
			if err != nil {
				return err
			}

			// filterFn makes sure utxos which are unconfirmed and
			// still used by the sweeper are not used.
			filterFn := func(u *lnwallet.Utxo) bool {
				// Confirmed utxos are always allowed.
				if u.Confirmations > 0 {
					return true
				}

				// Unconfirmed utxos in use by the sweeper are
				// not stable to use because they can be
				// replaced.
				if w.cfg.Sweeper.IsSweeperOutpoint(u.OutPoint) {
					log.Warnf("Cannot use unconfirmed "+
						"utxo=%v because it is "+
						"unstable and could be "+
						"replaced", u.OutPoint)

					return false
				}

				return true
			}

			eligibleUtxos := fn.Filter(utxos, filterFn)

			// Validate all inputs against our known list of UTXOs
			// now.
			err = verifyInputsUnspent(
				packet.UnsignedTx.TxIn, eligibleUtxos,
			)
			if err != nil {
				return err
			}
		}

		// currentHeight is needed to determine whether the internal
		// wallet utxo is still unconfirmed.
		_, currentHeight, err := w.cfg.Chain.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to retrieve current "+
				"height: %v", err)
		}

		// restrictUnstableUtxos is a filter function which disallows
		// the usage of unconfirmed outputs published (still in use) by
		// the sweeper.
		restrictUnstableUtxos := func(utxo wtxmgr.Credit) bool {
			// Wallet utxos which are unmined have a height
			// of -1.
			if utxo.Height != -1 && utxo.Height <= currentHeight {
				// Confirmed utxos are always allowed.
				return true
			}

			// Utxos used by the sweeper are not used for
			// channel openings.
			allowed := !w.cfg.Sweeper.IsSweeperOutpoint(
				utxo.OutPoint,
			)
			if !allowed {
				log.Warnf("Cannot use unconfirmed "+
					"utxo=%v because it is "+
					"unstable and could be "+
					"replaced", utxo.OutPoint)
			}

			return allowed
		}

		// We made sure the input from the user is as sane as possible.
		// We can now ask the wallet to fund the TX. This will not yet
		// lock any coins but might still change the wallet DB by
		// generating a new change address.
		changeIndex, err := w.cfg.Wallet.FundPsbt(
			packet, minConfs, feeSatPerKW, account, keyScope,
			strategy, restrictUnstableUtxos,
		)
		if err != nil {
			return fmt.Errorf("wallet couldn't fund PSBT: %w", err)
		}

		// Now we have obtained a set of coins that can be used to fund
		// the TX. Let's lock them to be sure they aren't spent by the
		// time the PSBT is published. This is the action we do here
		// that could cause an error. Therefore, if some of the UTXOs
		// cannot be locked, the rollback of the other's locks also
		// happens in this function. If we ever need to do more after
		// this function, we need to extract the rollback needs to be
		// extracted into a defer.
		outpoints := make([]wire.OutPoint, len(packet.UnsignedTx.TxIn))
		for i, txIn := range packet.UnsignedTx.TxIn {
			outpoints[i] = txIn.PreviousOutPoint
		}

		response, err = w.lockAndCreateFundingResponse(
			packet, outpoints, changeIndex, customLockID,
			customLockDuration,
		)

		return err
	})
	if err != nil {
		return nil, err
	}

	return response, nil
}

// fundPsbtCoinSelect uses the "new" PSBT funding method using the channel
// funding coin selection algorithm that allows specifying custom inputs while
// selecting coins.
func (w *WalletKit) fundPsbtCoinSelect(account string, changeIndex int32,
	packet *psbt.Packet, minConfs int32,
	changeType chanfunding.ChangeAddressType,
	feeRate chainfee.SatPerKWeight, strategy base.CoinSelectionStrategy,
	maxFeeRatio float64, customLockID *wtxmgr.LockID,
	customLockDuration time.Duration) (*FundPsbtResponse, error) {

	// We want to make sure we don't select any inputs that are already
	// specified in the template. To do that, we require those inputs to
	// either not belong to this lnd at all or to be already locked through
	// a manual lock call by the user. Either way, they should not appear in
	// the list of unspent outputs.
	err := w.assertNotAvailable(packet.UnsignedTx.TxIn, minConfs, account)
	if err != nil {
		return nil, err
	}

	// In case the user just specified the input outpoints of UTXOs we own,
	// the fee estimation below will error out because the UTXO information
	// is missing. We need to fetch the UTXO information from the wallet
	// and add it to the PSBT. We ignore inputs we don't actually know as
	// they could belong to another wallet.
	err = w.cfg.Wallet.DecorateInputs(packet, false)
	if err != nil {
		return nil, fmt.Errorf("error decorating inputs: %w", err)
	}

	// Before we select anything, we need to calculate the input, output and
	// current weight amounts. While doing that we also ensure the PSBT has
	// all the required information we require at this step.
	var (
		inputSum, outputSum btcutil.Amount
		estimator           input.TxWeightEstimator
	)
	for i := range packet.Inputs {
		in := packet.Inputs[i]

		err := btcwallet.EstimateInputWeight(&in, &estimator)
		if err != nil {
			return nil, fmt.Errorf("error estimating input "+
				"weight: %w", err)
		}

		inputSum += btcutil.Amount(in.WitnessUtxo.Value)
	}
	for i := range packet.UnsignedTx.TxOut {
		out := packet.UnsignedTx.TxOut[i]

		estimator.AddOutput(out.PkScript)
		outputSum += btcutil.Amount(out.Value)
	}

	// The amount we want to fund is the total output sum plus the current
	// fee estimate, minus the sum of any already specified inputs. Since we
	// pass the estimator of the current transaction into the coin selection
	// algorithm, we don't need to subtract the fees here.
	fundingAmount := outputSum - inputSum

	var changeDustLimit btcutil.Amount
	switch changeType {
	case chanfunding.P2TRChangeAddress:
		changeDustLimit = lnwallet.DustLimitForSize(input.P2TRSize)

	case chanfunding.P2WKHChangeAddress:
		changeDustLimit = lnwallet.DustLimitForSize(input.P2WPKHSize)

	case chanfunding.ExistingChangeAddress:
		changeOut := packet.UnsignedTx.TxOut[changeIndex]
		changeDustLimit = lnwallet.DustLimitForSize(
			len(changeOut.PkScript),
		)
	}

	// Do we already have enough inputs specified to pay for the TX as it
	// is? In that case we only need to allocate any change, if there is
	// any.
	packetFeeNoChange := feeRate.FeeForWeight(estimator.Weight())
	if inputSum >= outputSum+packetFeeNoChange {
		// Calculate the packet's fee with a change output so, so we can
		// let the coin selection algorithm decide whether to use a
		// change output or not.
		switch changeType {
		case chanfunding.P2TRChangeAddress:
			estimator.AddP2TROutput()

		case chanfunding.P2WKHChangeAddress:
			estimator.AddP2WKHOutput()
		}
		packetFeeWithChange := feeRate.FeeForWeight(estimator.Weight())

		changeAmt, needMore, err := chanfunding.CalculateChangeAmount(
			inputSum, outputSum, packetFeeNoChange,
			packetFeeWithChange, changeDustLimit, changeType,
			maxFeeRatio,
		)
		if err != nil {
			return nil, fmt.Errorf("error calculating change "+
				"amount: %w", err)
		}

		// We shouldn't get into this branch if the input sum isn't
		// enough to pay for the current package without a change
		// output. So this should never be non-zero.
		if needMore != 0 {
			return nil, fmt.Errorf("internal error with change " +
				"amount calculation")
		}

		if changeAmt > 0 {
			changeIndex, err = w.handleChange(
				packet, changeIndex, int64(changeAmt),
				changeType, account,
			)
			if err != nil {
				return nil, fmt.Errorf("error handling change "+
					"amount: %w", err)
			}
		}

		// We're done. Let's serialize and return the updated package.
		return w.lockAndCreateFundingResponse(
			packet, nil, changeIndex, customLockID,
			customLockDuration,
		)
	}

	// The RPC parsing part is now over. Several of the following operations
	// require us to hold the global coin selection lock, so we do the rest
	// of the tasks while holding the lock. The result is a list of locked
	// UTXOs.
	var response *FundPsbtResponse
	err = w.cfg.CoinSelectionLocker.WithCoinSelectLock(func() error {
		// Get a list of all unspent witness outputs.
		utxos, err := w.cfg.Wallet.ListUnspentWitness(
			minConfs, defaultMaxConf, account,
		)
		if err != nil {
			return err
		}

		coins := make([]base.Coin, len(utxos))
		for i, utxo := range utxos {
			coins[i] = base.Coin{
				TxOut: wire.TxOut{
					Value:    int64(utxo.Value),
					PkScript: utxo.PkScript,
				},
				OutPoint: utxo.OutPoint,
			}
		}

		selectedCoins, changeAmount, err := chanfunding.CoinSelect(
			feeRate, fundingAmount, changeDustLimit, coins,
			strategy, estimator, changeType, maxFeeRatio,
		)
		if err != nil {
			return fmt.Errorf("error selecting coins: %w", err)
		}

		if changeAmount > 0 {
			changeIndex, err = w.handleChange(
				packet, changeIndex, int64(changeAmount),
				changeType, account,
			)
			if err != nil {
				return fmt.Errorf("error handling change "+
					"amount: %w", err)
			}
		}

		addedOutpoints := make([]wire.OutPoint, len(selectedCoins))
		for i := range selectedCoins {
			coin := selectedCoins[i]
			addedOutpoints[i] = coin.OutPoint

			packet.UnsignedTx.TxIn = append(
				packet.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: coin.OutPoint,
				},
			)
			packet.Inputs = append(packet.Inputs, psbt.PInput{
				WitnessUtxo: &coin.TxOut,
			})
		}

		// Now that we've added the bare TX inputs, we also need to add
		// the more verbose input information to the packet, so a future
		// signer doesn't need to do any lookups. We skip any inputs
		// that our wallet doesn't own.
		err = w.cfg.Wallet.DecorateInputs(packet, false)
		if err != nil {
			return fmt.Errorf("error decorating inputs: %w", err)
		}

		response, err = w.lockAndCreateFundingResponse(
			packet, addedOutpoints, changeIndex, customLockID,
			customLockDuration,
		)

		return err
	})
	if err != nil {
		return nil, err
	}

	return response, nil
}

// assertNotAvailable makes sure the specified inputs either don't belong to
// this node or are already locked by the user.
func (w *WalletKit) assertNotAvailable(inputs []*wire.TxIn, minConfs int32,
	account string) error {

	return w.cfg.CoinSelectionLocker.WithCoinSelectLock(func() error {
		// Get a list of all unspent witness outputs.
		utxos, err := w.cfg.Wallet.ListUnspentWitness(
			minConfs, defaultMaxConf, account,
		)
		if err != nil {
			return fmt.Errorf("error fetching UTXOs: %w", err)
		}

		// We'll now check that none of the inputs specified in the
		// template are available to us. That means they either don't
		// belong to us or are already locked by the user.
		for _, txIn := range inputs {
			for _, utxo := range utxos {
				if txIn.PreviousOutPoint == utxo.OutPoint {
					return fmt.Errorf("input %v is not "+
						"locked", txIn.PreviousOutPoint)
				}
			}
		}

		return nil
	})
}

// lockAndCreateFundingResponse locks the given outpoints and creates a funding
// response with the serialized PSBT, the change index and the locked UTXOs.
func (w *WalletKit) lockAndCreateFundingResponse(packet *psbt.Packet,
	newOutpoints []wire.OutPoint, changeIndex int32,
	customLockID *wtxmgr.LockID, customLockDuration time.Duration) (
	*FundPsbtResponse, error) {

	// Make sure we can properly serialize the packet. If this goes wrong
	// then something isn't right with the inputs, and we probably shouldn't
	// try to lock any of them.
	var buf bytes.Buffer
	err := packet.Serialize(&buf)
	if err != nil {
		return nil, fmt.Errorf("error serializing funded PSBT: %w", err)
	}

	locks, err := lockInputs(
		w.cfg.Wallet, newOutpoints, customLockID, customLockDuration,
	)
	if err != nil {
		return nil, fmt.Errorf("could not lock inputs: %w", err)
	}

	// Convert the lock leases to the RPC format.
	rpcLocks := marshallLeases(locks)

	return &FundPsbtResponse{
		FundedPsbt:        buf.Bytes(),
		ChangeOutputIndex: changeIndex,
		LockedUtxos:       rpcLocks,
	}, nil
}

// handleChange is a closure that either adds the non-zero change amount to an
// existing output or creates a change output. The function returns the new
// change output index if a new change output was added.
func (w *WalletKit) handleChange(packet *psbt.Packet, changeIndex int32,
	changeAmount int64, changeType chanfunding.ChangeAddressType,
	changeAccount string) (int32, error) {

	// Does an existing output get the change?
	if changeIndex >= 0 {
		changeOut := packet.UnsignedTx.TxOut[changeIndex]
		changeOut.Value += changeAmount

		return changeIndex, nil
	}

	// The user requested a new change output.
	addrType := addrTypeFromChangeAddressType(changeType)
	changeAddr, err := w.cfg.Wallet.NewAddress(
		addrType, true, changeAccount,
	)
	if err != nil {
		return 0, fmt.Errorf("could not derive change address: %w", err)
	}

	changeScript, err := txscript.PayToAddrScript(changeAddr)
	if err != nil {
		return 0, fmt.Errorf("could not derive change script: %w", err)
	}

	// We need to add the derivation info for the change address in case it
	// is a P2TR address. This is mostly to prove it's a bare BIP-0086
	// address, which is required for some protocols (such as Taproot
	// Assets).
	pOut := psbt.POutput{}
	_, isTaprootChangeAddr := changeAddr.(*btcutil.AddressTaproot)
	if isTaprootChangeAddr {
		changeAddrInfo, err := w.cfg.Wallet.AddressInfo(changeAddr)
		if err != nil {
			return 0, fmt.Errorf("could not get address info: %w",
				err)
		}

		deriv, trDeriv, _, err := btcwallet.Bip32DerivationFromAddress(
			changeAddrInfo,
		)
		if err != nil {
			return 0, fmt.Errorf("could not get derivation info: "+
				"%w", err)
		}

		pOut.TaprootInternalKey = trDeriv.XOnlyPubKey
		pOut.Bip32Derivation = []*psbt.Bip32Derivation{deriv}
		pOut.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{
			trDeriv,
		}
	}

	newChangeIndex := int32(len(packet.Outputs))
	packet.UnsignedTx.TxOut = append(
		packet.UnsignedTx.TxOut, &wire.TxOut{
			Value:    changeAmount,
			PkScript: changeScript,
		},
	)
	packet.Outputs = append(packet.Outputs, pOut)

	return newChangeIndex, nil
}

// marshallLeases converts the lock leases to the RPC format.
func marshallLeases(locks []*base.ListLeasedOutputResult) []*UtxoLease {
	rpcLocks := make([]*UtxoLease, len(locks))
	for idx, lock := range locks {
		lock := lock

		rpcLocks[idx] = &UtxoLease{
			Id:         lock.LockID[:],
			Outpoint:   lnrpc.MarshalOutPoint(&lock.Outpoint),
			Expiration: uint64(lock.Expiration.Unix()),
			PkScript:   lock.PkScript,
			Value:      uint64(lock.Value),
		}
	}

	return rpcLocks
}

// keyScopeFromChangeAddressType maps a ChangeAddressType from protobuf to a
// KeyScope. If the type is ChangeAddressType_CHANGE_ADDRESS_TYPE_UNSPECIFIED,
// it returns nil.
func keyScopeFromChangeAddressType(
	changeAddressType ChangeAddressType) *waddrmgr.KeyScope {

	switch changeAddressType {
	case ChangeAddressType_CHANGE_ADDRESS_TYPE_P2TR:
		return &waddrmgr.KeyScopeBIP0086

	default:
		return nil
	}
}

// addrTypeFromChangeAddressType maps a chanfunding.ChangeAddressType to the
// lnwallet.AddressType.
func addrTypeFromChangeAddressType(
	changeAddressType chanfunding.ChangeAddressType) lnwallet.AddressType {

	switch changeAddressType {
	case chanfunding.P2TRChangeAddress:
		return lnwallet.TaprootPubkey

	default:
		return lnwallet.WitnessPubKey
	}
}

// SignPsbt expects a partial transaction with all inputs and outputs fully
// declared and tries to sign all unsigned inputs that have all required fields
// (UTXO information, BIP32 derivation information, witness or sig scripts)
// set.
// If no error is returned, the PSBT is ready to be given to the next signer or
// to be finalized if lnd was the last signer.
//
// NOTE: This RPC only signs inputs (and only those it can sign), it does not
// perform any other tasks (such as coin selection, UTXO locking or
// input/output/fee value validation, PSBT finalization). Any input that is
// incomplete will be skipped.
func (w *WalletKit) SignPsbt(_ context.Context, req *SignPsbtRequest) (
	*SignPsbtResponse, error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(req.FundedPsbt), false,
	)
	if err != nil {
		log.Debugf("Error parsing PSBT: %v, raw input: %x", err,
			req.FundedPsbt)
		return nil, fmt.Errorf("error parsing PSBT: %w", err)
	}

	// Before we attempt to sign the packet, ensure that every input either
	// has a witness UTXO, or a non witness UTXO.
	for idx := range packet.UnsignedTx.TxIn {
		in := packet.Inputs[idx]

		// Doesn't have either a witness or non witness UTXO so we need
		// to exit here as otherwise signing will fail.
		if in.WitnessUtxo == nil && in.NonWitnessUtxo == nil {
			return nil, fmt.Errorf("input (index=%v) doesn't "+
				"specify any UTXO info", idx)
		}
	}

	// Let the wallet do the heavy lifting. This will sign all inputs that
	// we have the UTXO for. If some inputs can't be signed and don't have
	// witness data attached, they will just be skipped.
	signedInputs, err := w.cfg.Wallet.SignPsbt(packet)
	if err != nil {
		return nil, fmt.Errorf("error signing PSBT: %w", err)
	}

	// Serialize the signed PSBT in both the packet and wire format.
	var signedPsbtBytes bytes.Buffer
	err = packet.Serialize(&signedPsbtBytes)
	if err != nil {
		return nil, fmt.Errorf("error serializing PSBT: %w", err)
	}

	return &SignPsbtResponse{
		SignedPsbt:   signedPsbtBytes.Bytes(),
		SignedInputs: signedInputs,
	}, nil
}

// FinalizePsbt expects a partial transaction with all inputs and outputs fully
// declared and tries to sign all inputs that belong to the wallet. Lnd must be
// the last signer of the transaction. That means, if there are any unsigned
// non-witness inputs or inputs without UTXO information attached or inputs
// without witness data that do not belong to lnd's wallet, this method will
// fail. If no error is returned, the PSBT is ready to be extracted and the
// final TX within to be broadcast.
//
// NOTE: This method does NOT publish the transaction once finalized. It is the
// caller's responsibility to either publish the transaction on success or
// unlock/release any locked UTXOs in case of an error in this method.
func (w *WalletKit) FinalizePsbt(_ context.Context,
	req *FinalizePsbtRequest) (*FinalizePsbtResponse, error) {

	// We'll assume the PSBT was funded by the default account unless
	// otherwise specified.
	account := lnwallet.DefaultAccountName
	if req.Account != "" {
		account = req.Account
	}

	// Parse the funded PSBT.
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(req.FundedPsbt), false,
	)
	if err != nil {
		return nil, fmt.Errorf("error parsing PSBT: %w", err)
	}

	// The only check done at this level is to validate that the PSBT is
	// not complete. The wallet performs all other checks.
	if packet.IsComplete() {
		return nil, fmt.Errorf("PSBT is already fully signed")
	}

	// Let the wallet do the heavy lifting. This will sign all inputs that
	// we have the UTXO for. If some inputs can't be signed and don't have
	// witness data attached, this will fail.
	err = w.cfg.Wallet.FinalizePsbt(packet, account)
	if err != nil {
		return nil, fmt.Errorf("error finalizing PSBT: %w", err)
	}

	var (
		finalPsbtBytes bytes.Buffer
		finalTxBytes   bytes.Buffer
	)

	// Serialize the finalized PSBT in both the packet and wire format.
	err = packet.Serialize(&finalPsbtBytes)
	if err != nil {
		return nil, fmt.Errorf("error serializing PSBT: %w", err)
	}
	finalTx, err := psbt.Extract(packet)
	if err != nil {
		return nil, fmt.Errorf("unable to extract final TX: %w", err)
	}
	err = finalTx.Serialize(&finalTxBytes)
	if err != nil {
		return nil, fmt.Errorf("error serializing final TX: %w", err)
	}

	return &FinalizePsbtResponse{
		SignedPsbt: finalPsbtBytes.Bytes(),
		RawFinalTx: finalTxBytes.Bytes(),
	}, nil
}

// marshalWalletAccount converts the properties of an account into its RPC
// representation.
func marshalWalletAccount(internalScope waddrmgr.KeyScope,
	account *waddrmgr.AccountProperties) (*Account, error) {

	var addrType AddressType
	switch account.KeyScope {
	case waddrmgr.KeyScopeBIP0049Plus:
		// No address schema present represents the traditional BIP-0049
		// address derivation scheme.
		if account.AddrSchema == nil {
			addrType = AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH
			break
		}

		switch *account.AddrSchema {
		case waddrmgr.KeyScopeBIP0049AddrSchema:
			addrType = AddressType_NESTED_WITNESS_PUBKEY_HASH

		case waddrmgr.ScopeAddrMap[waddrmgr.KeyScopeBIP0049Plus]:
			addrType = AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH

		default:
			return nil, fmt.Errorf("unsupported address schema %v",
				*account.AddrSchema)
		}

	case waddrmgr.KeyScopeBIP0084:
		addrType = AddressType_WITNESS_PUBKEY_HASH

	case waddrmgr.KeyScopeBIP0086:
		addrType = AddressType_TAPROOT_PUBKEY

	case internalScope:
		addrType = AddressType_WITNESS_PUBKEY_HASH

	default:
		return nil, fmt.Errorf("account %v has unsupported "+
			"key scope %v", account.AccountName, account.KeyScope)
	}

	rpcAccount := &Account{
		Name:             account.AccountName,
		AddressType:      addrType,
		ExternalKeyCount: account.ExternalKeyCount,
		InternalKeyCount: account.InternalKeyCount,
		WatchOnly:        account.IsWatchOnly,
	}

	// The remaining fields can only be done on accounts other than the
	// default imported one existing within each key scope.
	if account.AccountName != waddrmgr.ImportedAddrAccountName {
		nonHardenedIndex := account.AccountPubKey.ChildIndex() -
			hdkeychain.HardenedKeyStart
		rpcAccount.ExtendedPublicKey = account.AccountPubKey.String()
		if account.MasterKeyFingerprint != 0 {
			var mkfp [4]byte
			binary.BigEndian.PutUint32(
				mkfp[:], account.MasterKeyFingerprint,
			)
			rpcAccount.MasterKeyFingerprint = mkfp[:]
		}
		rpcAccount.DerivationPath = fmt.Sprintf("%v/%v'",
			account.KeyScope, nonHardenedIndex)
	}

	return rpcAccount, nil
}

// marshalWalletAddressList converts the list of address into its RPC
// representation.
func marshalWalletAddressList(w *WalletKit, account *waddrmgr.AccountProperties,
	addressList []lnwallet.AddressProperty) (*AccountWithAddresses, error) {

	// Get the RPC representation of account.
	rpcAccount, err := marshalWalletAccount(
		w.internalScope(), account,
	)
	if err != nil {
		return nil, err
	}

	addresses := make([]*AddressProperty, len(addressList))
	for idx, addr := range addressList {
		var pubKeyBytes []byte
		if addr.PublicKey != nil {
			pubKeyBytes = addr.PublicKey.SerializeCompressed()
		}
		addresses[idx] = &AddressProperty{
			Address:        addr.Address,
			IsInternal:     addr.Internal,
			Balance:        int64(addr.Balance),
			DerivationPath: addr.DerivationPath,
			PublicKey:      pubKeyBytes,
		}
	}

	rpcAddressList := &AccountWithAddresses{
		Name:           rpcAccount.Name,
		AddressType:    rpcAccount.AddressType,
		DerivationPath: rpcAccount.DerivationPath,
		Addresses:      addresses,
	}

	return rpcAddressList, nil
}

// ListAccounts retrieves all accounts belonging to the wallet by default. A
// name and key scope filter can be provided to filter through all of the wallet
// accounts and return only those matching.
func (w *WalletKit) ListAccounts(ctx context.Context,
	req *ListAccountsRequest) (*ListAccountsResponse, error) {

	// Map the supported address types into their corresponding key scope.
	var keyScopeFilter *waddrmgr.KeyScope
	switch req.AddressType {
	case AddressType_UNKNOWN:
		break

	case AddressType_WITNESS_PUBKEY_HASH:
		keyScope := waddrmgr.KeyScopeBIP0084
		keyScopeFilter = &keyScope

	case AddressType_NESTED_WITNESS_PUBKEY_HASH,
		AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:

		keyScope := waddrmgr.KeyScopeBIP0049Plus
		keyScopeFilter = &keyScope

	case AddressType_TAPROOT_PUBKEY:
		keyScope := waddrmgr.KeyScopeBIP0086
		keyScopeFilter = &keyScope

	default:
		return nil, fmt.Errorf("unhandled address type %v",
			req.AddressType)
	}

	accounts, err := w.cfg.Wallet.ListAccounts(req.Name, keyScopeFilter)
	if err != nil {
		return nil, err
	}

	rpcAccounts := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		// Don't include the default imported accounts created by the
		// wallet in the response if they don't have any keys imported.
		if account.AccountName == waddrmgr.ImportedAddrAccountName &&
			account.ImportedKeyCount == 0 {

			continue
		}

		rpcAccount, err := marshalWalletAccount(
			w.internalScope(), account,
		)
		if err != nil {
			return nil, err
		}
		rpcAccounts = append(rpcAccounts, rpcAccount)
	}

	return &ListAccountsResponse{Accounts: rpcAccounts}, nil
}

// RequiredReserve returns the minimum amount of satoshis that should be
// kept in the wallet in order to fee bump anchor channels if necessary.
// The value scales with the number of public anchor channels but is
// capped at a maximum.
func (w *WalletKit) RequiredReserve(ctx context.Context,
	req *RequiredReserveRequest) (*RequiredReserveResponse, error) {

	numAnchorChans, err := w.cfg.CurrentNumAnchorChans()
	if err != nil {
		return nil, err
	}

	additionalChans := req.AdditionalPublicChannels
	totalChans := uint32(numAnchorChans) + additionalChans
	reserved := w.cfg.Wallet.RequiredReserve(totalChans)

	return &RequiredReserveResponse{
		RequiredReserve: int64(reserved),
	}, nil
}

// ListAddresses retrieves all the addresses along with their balance. An
// account name filter can be provided to filter through all of the
// wallet accounts and return the addresses of only those matching.
func (w *WalletKit) ListAddresses(ctx context.Context,
	req *ListAddressesRequest) (*ListAddressesResponse, error) {

	addressLists, err := w.cfg.Wallet.ListAddresses(
		req.AccountName,
		req.ShowCustomAccounts,
	)
	if err != nil {
		return nil, err
	}

	// Create a slice of accounts from addressLists map.
	accounts := make([]*waddrmgr.AccountProperties, 0, len(addressLists))
	for account := range addressLists {
		accounts = append(accounts, account)
	}

	// Sort the accounts by derivation path.
	sort.Slice(accounts, func(i, j int) bool {
		scopeI := accounts[i].KeyScope
		scopeJ := accounts[j].KeyScope
		if scopeI.Purpose == scopeJ.Purpose {
			if scopeI.Coin == scopeJ.Coin {
				acntNumI := accounts[i].AccountNumber
				acntNumJ := accounts[j].AccountNumber
				return acntNumI < acntNumJ
			}

			return scopeI.Coin < scopeJ.Coin
		}

		return scopeI.Purpose < scopeJ.Purpose
	})

	rpcAddressLists := make([]*AccountWithAddresses, 0, len(addressLists))
	for _, account := range accounts {
		addressList := addressLists[account]
		rpcAddressList, err := marshalWalletAddressList(
			w, account, addressList,
		)
		if err != nil {
			return nil, err
		}

		rpcAddressLists = append(rpcAddressLists, rpcAddressList)
	}

	return &ListAddressesResponse{
		AccountWithAddresses: rpcAddressLists,
	}, nil
}

// parseAddrType parses an address type from its RPC representation to a
// *waddrmgr.AddressType.
func parseAddrType(addrType AddressType,
	required bool) (*waddrmgr.AddressType, error) {

	switch addrType {
	case AddressType_UNKNOWN:
		if required {
			return nil, fmt.Errorf("an address type must be " +
				"specified")
		}
		return nil, nil

	case AddressType_WITNESS_PUBKEY_HASH:
		addrTyp := waddrmgr.WitnessPubKey
		return &addrTyp, nil

	case AddressType_NESTED_WITNESS_PUBKEY_HASH:
		addrTyp := waddrmgr.NestedWitnessPubKey
		return &addrTyp, nil

	case AddressType_HYBRID_NESTED_WITNESS_PUBKEY_HASH:
		addrTyp := waddrmgr.WitnessPubKey
		return &addrTyp, nil

	case AddressType_TAPROOT_PUBKEY:
		addrTyp := waddrmgr.TaprootPubKey
		return &addrTyp, nil

	default:
		return nil, fmt.Errorf("unhandled address type %v", addrType)
	}
}

// msgSignaturePrefix is a prefix used to prevent inadvertently signing a
// transaction or a signature. It is prepended in front of the message and
// follows the same standard as bitcoin core and btcd.
const msgSignaturePrefix = "Bitcoin Signed Message:\n"

// SignMessageWithAddr signs a message with the private key of the provided
// address. The address needs to belong to the lnd wallet.
func (w *WalletKit) SignMessageWithAddr(_ context.Context,
	req *SignMessageWithAddrRequest) (*SignMessageWithAddrResponse, error) {

	addr, err := btcutil.DecodeAddress(req.Addr, w.cfg.ChainParams)
	if err != nil {
		return nil, fmt.Errorf("unable to decode address: %w", err)
	}

	if !addr.IsForNet(w.cfg.ChainParams) {
		return nil, fmt.Errorf("encoded address is for "+
			"the wrong network %s", req.Addr)
	}

	// Fetch address infos from own wallet and check whether it belongs
	// to the lnd wallet.
	managedAddr, err := w.cfg.Wallet.AddressInfo(addr)
	if err != nil {
		return nil, fmt.Errorf("address could not be found in the "+
			"wallet database: %w", err)
	}

	// Verifying by checking the interface type that the wallet knows about
	// the public and private keys so it can sign the message with the
	// private key of this address.
	pubKey, ok := managedAddr.(waddrmgr.ManagedPubKeyAddress)
	if !ok {
		return nil, fmt.Errorf("private key to address is unknown")
	}

	digest, err := doubleHashMessage(msgSignaturePrefix, string(req.Msg))
	if err != nil {
		return nil, err
	}

	// For all address types (P2WKH, NP2WKH,P2TR) the ECDSA compact signing
	// algorithm is used. For P2TR addresses this represents a special case.
	// ECDSA is used to create a compact signature which makes the public
	// key of the signature recoverable. For Schnorr no known compact
	// signing algorithm exists yet.
	privKey, err := pubKey.PrivKey()
	if err != nil {
		return nil, fmt.Errorf("no private key could be "+
			"fetched from wallet database: %w", err)
	}

	sigBytes := ecdsa.SignCompact(privKey, digest, pubKey.Compressed())

	// Bitcoin signatures are base64 encoded (being compatible with
	// bitcoin-core and btcd).
	sig := base64.StdEncoding.EncodeToString(sigBytes)

	return &SignMessageWithAddrResponse{
		Signature: sig,
	}, nil
}

// VerifyMessageWithAddr verifies a signature on a message with a provided
// address, it checks both the validity of the signature itself and then
// verifies whether the signature corresponds to the public key of the
// provided address. There is no dependence on the private key of the address
// therefore also external addresses are allowed to verify signatures.
// Supported address types are P2PKH, P2WKH, NP2WKH, P2TR.
func (w *WalletKit) VerifyMessageWithAddr(_ context.Context,
	req *VerifyMessageWithAddrRequest) (*VerifyMessageWithAddrResponse,
	error) {

	sig, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		return nil, fmt.Errorf("malformed base64 encoding of "+
			"the signature: %w", err)
	}

	digest, err := doubleHashMessage(msgSignaturePrefix, string(req.Msg))
	if err != nil {
		return nil, err
	}

	pk, wasCompressed, err := ecdsa.RecoverCompact(sig, digest)
	if err != nil {
		return nil, fmt.Errorf("unable to recover public key "+
			"from compact signature: %w", err)
	}

	var serializedPubkey []byte
	if wasCompressed {
		serializedPubkey = pk.SerializeCompressed()
	} else {
		serializedPubkey = pk.SerializeUncompressed()
	}

	addr, err := btcutil.DecodeAddress(req.Addr, w.cfg.ChainParams)
	if err != nil {
		return nil, fmt.Errorf("unable to decode address: %w", err)
	}

	if !addr.IsForNet(w.cfg.ChainParams) {
		return nil, fmt.Errorf("encoded address is for"+
			"the wrong network %s", req.Addr)
	}

	var (
		address    btcutil.Address
		pubKeyHash = btcutil.Hash160(serializedPubkey)
	)

	// Ensure the address is one of the supported types.
	switch addr.(type) {
	case *btcutil.AddressPubKeyHash:
		address, err = btcutil.NewAddressPubKeyHash(
			pubKeyHash, w.cfg.ChainParams,
		)
		if err != nil {
			return nil, err
		}

	case *btcutil.AddressWitnessPubKeyHash:
		address, err = btcutil.NewAddressWitnessPubKeyHash(
			pubKeyHash, w.cfg.ChainParams,
		)
		if err != nil {
			return nil, err
		}

	case *btcutil.AddressScriptHash:
		// Check if address is a Nested P2WKH (NP2WKH).
		address, err = btcutil.NewAddressWitnessPubKeyHash(
			pubKeyHash, w.cfg.ChainParams,
		)
		if err != nil {
			return nil, err
		}

		witnessScript, err := txscript.PayToAddrScript(address)
		if err != nil {
			return nil, err
		}

		address, err = btcutil.NewAddressScriptHashFromHash(
			btcutil.Hash160(witnessScript), w.cfg.ChainParams,
		)
		if err != nil {
			return nil, err
		}

	case *btcutil.AddressTaproot:
		// Only addresses without a tapscript are allowed because
		// the verification is using the internal key.
		tapKey := txscript.ComputeTaprootKeyNoScript(pk)
		address, err = btcutil.NewAddressTaproot(
			schnorr.SerializePubKey(tapKey),
			w.cfg.ChainParams,
		)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unsupported address type")
	}

	return &VerifyMessageWithAddrResponse{
		Valid:  req.Addr == address.EncodeAddress(),
		Pubkey: serializedPubkey,
	}, nil
}

// ImportAccount imports an account backed by an account extended public key.
// The master key fingerprint denotes the fingerprint of the root key
// corresponding to the account public key (also known as the key with
// derivation path m/). This may be required by some hardware wallets for proper
// identification and signing.
//
// The address type can usually be inferred from the key's version, but may be
// required for certain keys to map them into the proper scope.
//
// For BIP-0044 keys, an address type must be specified as we intend to not
// support importing BIP-0044 keys into the wallet using the legacy
// pay-to-pubkey-hash (P2PKH) scheme. A nested witness address type will force
// the standard BIP-0049 derivation scheme, while a witness address type will
// force the standard BIP-0084 derivation scheme.
//
// For BIP-0049 keys, an address type must also be specified to make a
// distinction between the standard BIP-0049 address schema (nested witness
// pubkeys everywhere) and our own BIP-0049Plus address schema (nested pubkeys
// externally, witness pubkeys internally).
func (w *WalletKit) ImportAccount(_ context.Context,
	req *ImportAccountRequest) (*ImportAccountResponse, error) {

	accountPubKey, err := hdkeychain.NewKeyFromString(req.ExtendedPublicKey)
	if err != nil {
		return nil, err
	}

	var mkfp uint32
	switch len(req.MasterKeyFingerprint) {
	// No master key fingerprint provided, which is fine as it's not
	// required.
	case 0:
	// Expected length.
	case 4:
		mkfp = binary.BigEndian.Uint32(req.MasterKeyFingerprint)
	default:
		return nil, errors.New("invalid length for master key " +
			"fingerprint, expected 4 bytes in big-endian")
	}

	addrType, err := parseAddrType(req.AddressType, false)
	if err != nil {
		return nil, err
	}

	accountProps, extAddrs, intAddrs, err := w.cfg.Wallet.ImportAccount(
		req.Name, accountPubKey, mkfp, addrType, req.DryRun,
	)
	if err != nil {
		return nil, err
	}

	rpcAccount, err := marshalWalletAccount(w.internalScope(), accountProps)
	if err != nil {
		return nil, err
	}

	resp := &ImportAccountResponse{Account: rpcAccount}
	if !req.DryRun {
		return resp, nil
	}

	resp.DryRunExternalAddrs = make([]string, len(extAddrs))
	for i := 0; i < len(extAddrs); i++ {
		resp.DryRunExternalAddrs[i] = extAddrs[i].String()
	}
	resp.DryRunInternalAddrs = make([]string, len(intAddrs))
	for i := 0; i < len(intAddrs); i++ {
		resp.DryRunInternalAddrs[i] = intAddrs[i].String()
	}

	return resp, nil
}

// ImportPublicKey imports a single derived public key into the wallet. The
// address type can usually be inferred from the key's version, but in the case
// of legacy versions (xpub, tpub), an address type must be specified as we
// intend to not support importing BIP-44 keys into the wallet using the legacy
// pay-to-pubkey-hash (P2PKH) scheme. For Taproot keys, this will only watch
// the BIP-0086 style output script. Use ImportTapscript for more advanced key
// spend or script spend outputs.
func (w *WalletKit) ImportPublicKey(_ context.Context,
	req *ImportPublicKeyRequest) (*ImportPublicKeyResponse, error) {

	var (
		pubKey *btcec.PublicKey
		err    error
	)
	switch req.AddressType {
	case AddressType_TAPROOT_PUBKEY:
		pubKey, err = schnorr.ParsePubKey(req.PublicKey)

	default:
		pubKey, err = btcec.ParsePubKey(req.PublicKey)
	}
	if err != nil {
		return nil, err
	}

	addrType, err := parseAddrType(req.AddressType, true)
	if err != nil {
		return nil, err
	}

	if err := w.cfg.Wallet.ImportPublicKey(pubKey, *addrType); err != nil {
		return nil, err
	}

	return &ImportPublicKeyResponse{
		Status: fmt.Sprintf("public key %x imported",
			pubKey.SerializeCompressed()),
	}, nil
}

// ImportTapscript imports a Taproot script and internal key and adds the
// resulting Taproot output key as a watch-only output script into the wallet.
// For BIP-0086 style Taproot keys (no root hash commitment and no script spend
// path) use ImportPublicKey.
//
// NOTE: Taproot keys imported through this RPC currently _cannot_ be used for
// funding PSBTs. Only tracking the balance and UTXOs is currently supported.
func (w *WalletKit) ImportTapscript(_ context.Context,
	req *ImportTapscriptRequest) (*ImportTapscriptResponse, error) {

	internalKey, err := schnorr.ParsePubKey(req.InternalPublicKey)
	if err != nil {
		return nil, fmt.Errorf("error parsing internal key: %w", err)
	}

	var tapscript *waddrmgr.Tapscript
	switch {
	case req.GetFullTree() != nil:
		tree := req.GetFullTree()
		leaves := make([]txscript.TapLeaf, len(tree.AllLeaves))
		for idx, leaf := range tree.AllLeaves {
			leaves[idx] = txscript.TapLeaf{
				LeafVersion: txscript.TapscriptLeafVersion(
					leaf.LeafVersion,
				),
				Script: leaf.Script,
			}
		}

		tapscript = input.TapscriptFullTree(internalKey, leaves...)

	case req.GetPartialReveal() != nil:
		partialReveal := req.GetPartialReveal()
		if partialReveal.RevealedLeaf == nil {
			return nil, fmt.Errorf("missing revealed leaf")
		}

		revealedLeaf := txscript.TapLeaf{
			LeafVersion: txscript.TapscriptLeafVersion(
				partialReveal.RevealedLeaf.LeafVersion,
			),
			Script: partialReveal.RevealedLeaf.Script,
		}
		if len(partialReveal.FullInclusionProof)%32 != 0 {
			return nil, fmt.Errorf("invalid inclusion proof "+
				"length, expected multiple of 32, got %d",
				len(partialReveal.FullInclusionProof)%32)
		}

		tapscript = input.TapscriptPartialReveal(
			internalKey, revealedLeaf,
			partialReveal.FullInclusionProof,
		)

	case req.GetRootHashOnly() != nil:
		rootHash := req.GetRootHashOnly()
		if len(rootHash) == 0 {
			return nil, fmt.Errorf("missing root hash")
		}

		tapscript = input.TapscriptRootHashOnly(internalKey, rootHash)

	case req.GetFullKeyOnly():
		tapscript = input.TapscriptFullKeyOnly(internalKey)

	default:
		return nil, fmt.Errorf("invalid script")
	}

	taprootScope := waddrmgr.KeyScopeBIP0086
	addr, err := w.cfg.Wallet.ImportTaprootScript(taprootScope, tapscript)
	if err != nil {
		return nil, fmt.Errorf("error importing script into wallet: %w",
			err)
	}

	return &ImportTapscriptResponse{
		P2TrAddress: addr.Address().String(),
	}, nil
}

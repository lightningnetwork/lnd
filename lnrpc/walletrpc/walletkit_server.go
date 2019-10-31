// +build walletrpc

package walletrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/sweep"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize as the name of our
	subServerName = "WalletKitRPC"
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
	}

	// DefaultWalletKitMacFilename is the default name of the wallet kit
	// macaroon that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultWalletKitMacFilename = "walletkit.macaroon"
)

// WalletKit is a sub-RPC server that exposes a tool kit which allows clients
// to execute common wallet operations. This includes requesting new addresses,
// keys (for contracts!), and publishing transactions.
type WalletKit struct {
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
	// check to see if we need to create it or not.
	macFilePath := cfg.WalletKitMacPath
	if !lnrpc.FileExists(macFilePath) && cfg.MacService != nil {
		log.Infof("Baking macaroons for WalletKit RPC Server at: %v",
			macFilePath)

		// At this point, we know that the wallet kit macaroon doesn't
		// yet, exist, so we need to create it with the help of the
		// main macaroon service.
		walletKitMac, err := cfg.MacService.Oven.NewMacaroon(
			context.Background(), bakery.LatestVersion, nil,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		walletKitMacBytes, err := walletKitMac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = ioutil.WriteFile(macFilePath, walletKitMacBytes, 0644)
		if err != nil {
			os.Remove(macFilePath)
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
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until this
// is called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (w *WalletKit) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterWalletKitServer(grpcServer, w)

	log.Debugf("WalletKit RPC server successfully registered with " +
		"root gRPC server")

	return nil
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

	addr, err := w.cfg.Wallet.NewAddress(lnwallet.WitnessPubKey, false)
	if err != nil {
		return nil, err
	}

	return &AddrResponse{
		Addr: addr.String(),
	}, nil
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

	err := w.cfg.Wallet.PublishTransaction(tx)
	if err != nil {
		return nil, err
	}

	return &PublishResponse{}, nil
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
	outputsToCreate := make([]*wire.TxOut, 0, len(req.Outputs))
	for _, output := range req.Outputs {
		outputsToCreate = append(outputsToCreate, &wire.TxOut{
			Value:    output.Value,
			PkScript: output.PkScript,
		})
	}

	// Now that we have the outputs mapped, we can request that the wallet
	// attempt to create this transaction.
	tx, err := w.cfg.Wallet.SendOutputs(
		outputsToCreate, chainfee.SatPerKWeight(req.SatPerKw),
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

	switch {
	// A confirmation target of zero doesn't make any sense. Similarly, we
	// reject confirmation targets of 1 as they're unreasonable.
	case req.ConfTarget == 0 || req.ConfTarget == 1:
		return nil, fmt.Errorf("confirmation target must be greater " +
			"than 1")
	}

	satPerKw, err := w.cfg.FeeEstimator.EstimateFeePerKW(
		uint32(req.ConfTarget),
	)
	if err != nil {
		return nil, err
	}

	return &EstimateFeeResponse{
		SatPerKw: int64(satPerKw),
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
	pendingInputs, err := w.cfg.Sweeper.PendingInputs()
	if err != nil {
		return nil, err
	}

	// Convert them into their respective RPC format.
	rpcPendingSweeps := make([]*PendingSweep, 0, len(pendingInputs))
	for _, pendingInput := range pendingInputs {
		var witnessType WitnessType
		switch pendingInput.WitnessType {
		case input.CommitmentTimeLock:
			witnessType = WitnessType_COMMITMENT_TIME_LOCK
		case input.CommitmentNoDelay:
			witnessType = WitnessType_COMMITMENT_NO_DELAY
		case input.CommitmentRevoke:
			witnessType = WitnessType_COMMITMENT_REVOKE
		case input.HtlcOfferedRevoke:
			witnessType = WitnessType_HTLC_OFFERED_REVOKE
		case input.HtlcAcceptedRevoke:
			witnessType = WitnessType_HTLC_ACCEPTED_REVOKE
		case input.HtlcOfferedTimeoutSecondLevel:
			witnessType = WitnessType_HTLC_OFFERED_TIMEOUT_SECOND_LEVEL
		case input.HtlcAcceptedSuccessSecondLevel:
			witnessType = WitnessType_HTLC_ACCEPTED_SUCCESS_SECOND_LEVEL
		case input.HtlcOfferedRemoteTimeout:
			witnessType = WitnessType_HTLC_OFFERED_REMOTE_TIMEOUT
		case input.HtlcAcceptedRemoteSuccess:
			witnessType = WitnessType_HTLC_ACCEPTED_REMOTE_SUCCESS
		case input.HtlcSecondLevelRevoke:
			witnessType = WitnessType_HTLC_SECOND_LEVEL_REVOKE
		case input.WitnessKeyHash:
			witnessType = WitnessType_WITNESS_KEY_HASH
		case input.NestedWitnessKeyHash:
			witnessType = WitnessType_NESTED_WITNESS_KEY_HASH
		default:
			log.Warnf("Unhandled witness type %v for input %v",
				pendingInput.WitnessType, pendingInput.OutPoint)
		}

		op := &lnrpc.OutPoint{
			TxidBytes:   pendingInput.OutPoint.Hash[:],
			OutputIndex: pendingInput.OutPoint.Index,
		}
		amountSat := uint32(pendingInput.Amount)
		satPerByte := uint32(pendingInput.LastFeeRate.FeePerKVByte() / 1000)
		broadcastAttempts := uint32(pendingInput.BroadcastAttempts)
		nextBroadcastHeight := uint32(pendingInput.NextBroadcastHeight)

		rpcPendingSweeps = append(rpcPendingSweeps, &PendingSweep{
			Outpoint:            op,
			WitnessType:         witnessType,
			AmountSat:           amountSat,
			SatPerByte:          satPerByte,
			BroadcastAttempts:   broadcastAttempts,
			NextBroadcastHeight: nextBroadcastHeight,
		})
	}

	return &PendingSweepsResponse{
		PendingSweeps: rpcPendingSweeps,
	}, nil
}

// unmarshallOutPoint converts an outpoint from its lnrpc type to its canonical
// type.
func unmarshallOutPoint(op *lnrpc.OutPoint) (*wire.OutPoint, error) {
	if op == nil {
		return nil, fmt.Errorf("empty outpoint provided")
	}

	var hash chainhash.Hash
	switch {
	case len(op.TxidBytes) == 0 && len(op.TxidStr) == 0:
		fallthrough

	case len(op.TxidBytes) != 0 && len(op.TxidStr) != 0:
		return nil, fmt.Errorf("either TxidBytes or TxidStr must be " +
			"specified, but not both")

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

// BumpFee allows bumping the fee rate of an arbitrary input. A fee preference
// can be expressed either as a specific fee rate or a delta of blocks in which
// the output should be swept on-chain within. If a fee preference is not
// explicitly specified, then an error is returned. The status of the input
// sweep can be checked through the PendingSweeps RPC.
func (w *WalletKit) BumpFee(ctx context.Context,
	in *BumpFeeRequest) (*BumpFeeResponse, error) {

	// Parse the outpoint from the request.
	op, err := unmarshallOutPoint(in.Outpoint)
	if err != nil {
		return nil, err
	}

	// Construct the request's fee preference.
	satPerKw := chainfee.SatPerKVByte(in.SatPerByte * 1000).FeePerKWeight()
	feePreference := sweep.FeePreference{
		ConfTarget: uint32(in.TargetConf),
		FeeRate:    satPerKw,
	}

	// We'll attempt to bump the fee of the input through the UtxoSweeper.
	// If it is currently attempting to sweep the input, then it'll simply
	// bump its fee, which will result in a replacement transaction (RBF)
	// being broadcast. If it is not aware of the input however,
	// lnwallet.ErrNotMine is returned.
	_, err = w.cfg.Sweeper.BumpFee(*op, feePreference)
	switch err {
	case nil:
		return &BumpFeeResponse{}, nil
	case lnwallet.ErrNotMine:
		break
	default:
		return nil, err
	}

	// Since we're unable to perform a bump through RBF, we'll assume the
	// user is attempting to bump an unconfirmed transaction's fee rate by
	// sweeping an output within it under control of the wallet with a
	// higher fee rate, essentially performing a Child-Pays-For-Parent
	// (CPFP).
	//
	// We'll gather all of the information required by the UtxoSweeper in
	// order to sweep the output.
	utxo, err := w.cfg.Wallet.FetchInputInfo(op)
	if err != nil {
		return nil, err
	}

	// We're only able to bump the fee of unconfirmed transactions.
	if utxo.Confirmations > 0 {
		return nil, errors.New("unable to bump fee of a confirmed " +
			"transaction")
	}

	var witnessType input.WitnessType
	switch utxo.AddressType {
	case lnwallet.WitnessPubKey:
		witnessType = input.WitnessKeyHash
	case lnwallet.NestedWitnessPubKey:
		witnessType = input.NestedWitnessKeyHash
	default:
		return nil, fmt.Errorf("unknown input witness %v", op)
	}

	signDesc := &input.SignDescriptor{
		Output: &wire.TxOut{
			PkScript: utxo.PkScript,
			Value:    int64(utxo.Value),
		},
		HashType: txscript.SigHashAll,
	}

	// We'll use the current height as the height hint since we're dealing
	// with an unconfirmed transaction.
	_, currentHeight, err := w.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve current height: %v",
			err)
	}

	input := input.NewBaseInput(op, witnessType, signDesc, uint32(currentHeight))
	if _, err = w.cfg.Sweeper.SweepInput(input, feePreference); err != nil {
		return nil, err
	}

	return &BumpFeeResponse{}, nil
}

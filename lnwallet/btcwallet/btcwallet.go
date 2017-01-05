package btcwallet

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"github.com/roasbeef/btcwallet/chain"
	"github.com/roasbeef/btcwallet/waddrmgr"
	base "github.com/roasbeef/btcwallet/wallet"
	"github.com/roasbeef/btcwallet/walletdb"
)

const (
	defaultAccount = uint32(waddrmgr.DefaultAccountNum)
)

var (
	lnNamespace = []byte("ln")
	rootKey     = []byte("ln-root")
)

// BtcWallet is an implementation of the lnwallet.WalletController interface
// backed by an active instance of btcwallet. At the time of the writing of
// this documentation, this implementation requires a full btcd node to
// operate.
type BtcWallet struct {
	// wallet is an active instance of btcwallet.
	wallet *base.Wallet

	// rpc is an an active RPC connection to btcd full-node.
	rpc *chain.RPCClient

	// lnNamespace is a namespace within btcwallet's walletdb used to store
	// persistent state required by the WalletController interface but not
	// natively supported by btcwallet.
	lnNamespace walletdb.Namespace

	netParams *chaincfg.Params

	// utxoCache is a cache used to speed up repeated calls to
	// FetchInputInfo.
	utxoCache map[wire.OutPoint]*wire.TxOut
	cacheMtx  sync.RWMutex
}

// A compile time check to ensure that BtcWallet implements the
// WalletController interface.
var _ lnwallet.WalletController = (*BtcWallet)(nil)

// New returns a new fully initialized instance of BtcWallet given a valid
// configuration struct.
func New(cfg *Config) (*BtcWallet, error) {
	// Ensure the wallet exists or create it when the create flag is set.
	netDir := networkDir(cfg.DataDir, cfg.NetParams)

	var pubPass []byte
	if cfg.PublicPass == nil {
		pubPass = defaultPubPassphrase
	} else {
		pubPass = cfg.PublicPass
	}

	loader := base.NewLoader(cfg.NetParams, netDir)
	walletExists, err := loader.WalletExists()
	if err != nil {
		return nil, err
	}

	var wallet *base.Wallet
	if !walletExists {
		// Wallet has never been created, perform initial set up.
		wallet, err = loader.CreateNewWallet(pubPass, cfg.PrivatePass,
			cfg.HdSeed)
		if err != nil {
			return nil, err
		}
	} else {
		// Wallet has been created and been initialized at this point, open it
		// along with all the required DB namepsaces, and the DB itself.
		wallet, err = loader.OpenExistingWallet(pubPass, false)
		if err != nil {
			return nil, err
		}
	}

	if err := wallet.Manager.Unlock(cfg.PrivatePass); err != nil {
		return nil, err
	}

	// Create a special websockets rpc client for btcd which will be used
	// by the wallet for notifications, calls, etc.
	rpcc, err := chain.NewRPCClient(cfg.NetParams, cfg.RpcHost,
		cfg.RpcUser, cfg.RpcPass, cfg.CACert, false, 20)
	if err != nil {
		return nil, err
	}

	db := wallet.Database()
	walletNamespace, err := db.Namespace(lnNamespace)
	if err != nil {
		return nil, err
	}

	return &BtcWallet{
		wallet:      wallet,
		rpc:         rpcc,
		lnNamespace: walletNamespace,
		netParams:   cfg.NetParams,
		utxoCache:   make(map[wire.OutPoint]*wire.TxOut),
	}, nil
}

// Start initializes the underlying rpc connection, the wallet itself, and
// begins syncing to the current available blockchain state.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) Start() error {
	// Establish an RPC connection in additino to starting the goroutines
	// in the underlying wallet.
	if err := b.rpc.Start(); err != nil {
		return err
	}

	// Start the underlying btcwallet core.
	b.wallet.Start()

	// Pass the rpc client into the wallet so it can sync up to the
	// current main chain.
	b.wallet.SynchronizeRPC(b.rpc)

	return nil
}

// Stop signals the wallet for shutdown. Shutdown may entail closing
// any active sockets, database handles, stopping goroutines, etc.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) Stop() error {
	b.wallet.Stop()

	b.wallet.WaitForShutdown()

	b.rpc.Shutdown()

	return nil
}

// ConfirmedBalance returns the sum of all the wallet's unspent outputs that
// have at least confs confirmations. If confs is set to zero, then all unspent
// outputs, including those currently in the mempool will be included in the
// final sum.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ConfirmedBalance(confs int32, witness bool) (btcutil.Amount, error) {
	var balance btcutil.Amount

	if witness {
		witnessOutputs, err := b.ListUnspentWitness(confs)
		if err != nil {
			return 0, err
		}

		for _, witnessOutput := range witnessOutputs {
			balance += witnessOutput.Value
		}
	} else {
		outputSum, err := b.wallet.CalculateBalance(confs)
		if err != nil {
			return 0, err
		}

		balance = outputSum
	}

	return balance, nil
}

// NewAddress returns the next external or internal address for the wallet
// dicatated by the value of the `change` paramter. If change is true, then an
// internal address will be returned, otherwise an external address should be
// returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) NewAddress(t lnwallet.AddressType, change bool) (btcutil.Address, error) {
	var addrType waddrmgr.AddressType

	switch t {
	case lnwallet.WitnessPubKey:
		addrType = waddrmgr.WitnessPubKey
	case lnwallet.NestedWitnessPubKey:
		addrType = waddrmgr.NestedWitnessPubKey
	case lnwallet.PubKeyHash:
		addrType = waddrmgr.PubKeyHash
	default:
		return nil, fmt.Errorf("unknown address type")
	}

	if change {
		return b.wallet.NewAddress(defaultAccount, addrType)
	} else {
		return b.wallet.NewChangeAddress(defaultAccount, addrType)
	}
}

// GetPrivKey retrives the underlying private key associated with the passed
// address. If the we're unable to locate the proper private key, then a
// non-nil error will be returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) GetPrivKey(a btcutil.Address) (*btcec.PrivateKey, error) {
	// Using the ID address, request the private key coresponding to the
	// address from the wallet's address manager.
	walletAddr, err := b.wallet.Manager.Address(a)
	if err != nil {
		return nil, err
	}

	return walletAddr.(waddrmgr.ManagedPubKeyAddress).PrivKey()
}

// NewRawKey retrieves the next key within our HD key-chain for use within as a
// multi-sig key within the funding transaction, or within the commitment
// transaction's outputs.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) NewRawKey() (*btcec.PublicKey, error) {
	nextAddr, err := b.wallet.Manager.NextExternalAddresses(defaultAccount,
		1, waddrmgr.WitnessPubKey)
	if err != nil {
		return nil, err
	}

	pkAddr := nextAddr[0].(waddrmgr.ManagedPubKeyAddress)

	return pkAddr.PubKey(), nil
}

// FetchRootKey returns a root key which is meanted to be used as an initial
// seed/salt to generate any Lightning specific secrets.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) FetchRootKey() (*btcec.PrivateKey, error) {
	// Fetch the root address hash from the database, this is persisted
	// locally within the database, then used to obtain the key from the
	// wallet based on the address hash.
	var rootAddrHash []byte
	if err := b.lnNamespace.Update(func(tx walletdb.Tx) error {
		rootBucket := tx.RootBucket()

		rootAddrHash = rootBucket.Get(rootKey)
		return nil
	}); err != nil {
		return nil, err
	}

	if rootAddrHash == nil {
		// Otherwise, we need to generate a fresh address from the
		// wallet, then stores it's hash160 within the database so we
		// can look up the exact key later.
		rootAddr, err := b.wallet.Manager.NextExternalAddresses(defaultAccount,
			1, waddrmgr.WitnessPubKey)
		if err != nil {
			return nil, err
		}

		if err := b.lnNamespace.Update(func(tx walletdb.Tx) error {
			rootBucket := tx.RootBucket()

			rootAddrHash = rootAddr[0].Address().ScriptAddress()
			if err := rootBucket.Put(rootKey, rootAddrHash); err != nil {
				return err
			}

			return nil
		}); err != nil {
			return nil, err
		}
	}

	// With the root address hash obtained, generate the corresponding
	// address, then retrieve the managed address from the wallet which
	// will allow us to obtain the private key.
	rootAddr, err := btcutil.NewAddressWitnessPubKeyHash(rootAddrHash,
		b.netParams)
	if err != nil {
		return nil, err
	}
	walletAddr, err := b.wallet.Manager.Address(rootAddr)
	if err != nil {
		return nil, err
	}

	return walletAddr.(waddrmgr.ManagedPubKeyAddress).PrivKey()
}

// SendOutputs funds, signs, and broadcasts a Bitcoin transaction paying out to
// the specified outputs. In the case the wallet has insufficient funds, or the
// outputs are non-standard, a non-nil error will be be returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SendOutputs(outputs []*wire.TxOut) (*chainhash.Hash, error) {
	return b.wallet.SendOutputs(outputs, defaultAccount, 1)
}

// LockOutpoint marks an outpoint as locked meaning it will no longer be deemed
// as eligible for coin selection. Locking outputs are utilized in order to
// avoid race conditions when selecting inputs for usage when funding a
// channel.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) LockOutpoint(o wire.OutPoint) {
	b.wallet.LockOutpoint(o)
}

// UnlockOutpoint unlocks an previously locked output, marking it eligible for
// coin seleciton.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) UnlockOutpoint(o wire.OutPoint) {
	b.wallet.UnlockOutpoint(o)
}

// ListUnspentWitness returns a slice of all the unspent outputs the wallet
// controls which pay to witness programs either directly or indirectly.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListUnspentWitness(minConfs int32) ([]*lnwallet.Utxo, error) {
	// First, grab all the unfiltered currently unspent outputs.
	maxConfs := int32(math.MaxInt32)
	unspentOutputs, err := b.wallet.ListUnspent(minConfs, maxConfs, nil)
	if err != nil {
		return nil, err
	}

	// Next, we'll run through all the regular outputs, only saving those
	// which are p2wkh outputs or a p2wsh output nested within a p2sh output.
	witnessOutputs := make([]*lnwallet.Utxo, 0, len(unspentOutputs))
	for _, output := range unspentOutputs {
		pkScript, err := hex.DecodeString(output.ScriptPubKey)
		if err != nil {
			return nil, err
		}

		// TODO(roasbeef): this assumes all p2sh outputs returned by
		// the wallet are nested p2sh...
		if txscript.IsPayToWitnessPubKeyHash(pkScript) ||
			txscript.IsPayToScriptHash(pkScript) {
			txid, err := chainhash.NewHashFromStr(output.TxID)
			if err != nil {
				return nil, err
			}

			utxo := &lnwallet.Utxo{
				Value: btcutil.Amount(output.Amount * 1e8),
				OutPoint: wire.OutPoint{
					Hash:  *txid,
					Index: output.Vout,
				},
			}
			witnessOutputs = append(witnessOutputs, utxo)
		}

	}

	return witnessOutputs, nil
}

// PublishTransaction performs cursory validation (dust checks, etc), then
// finally broadcasts the passed transaction to the Bitcoin network.
func (b *BtcWallet) PublishTransaction(tx *wire.MsgTx) error {
	return b.wallet.PublishTransaction(tx)
}

// extractBalanceDelta extracts the net balance delta from the PoV of the
// wallet given a TransactionSummary.
func extractBalanceDelta(txSummary base.TransactionSummary) (btcutil.Amount, error) {
	tx := wire.NewMsgTx(1)
	txReader := bytes.NewReader(txSummary.Transaction)
	if err := tx.Deserialize(txReader); err != nil {
		return -1, nil
	}

	// For each input we debit the wallet's outflow for this transaction,
	// and for each output we credit the wallet's inflow for this
	// transaction.
	var balanceDelta btcutil.Amount
	for _, input := range txSummary.MyInputs {
		balanceDelta -= input.PreviousAmount
	}
	for _, output := range txSummary.MyOutputs {
		balanceDelta += btcutil.Amount(tx.TxOut[output.Index].Value)
	}

	return balanceDelta, nil
}

// minedTransactionsToDetails is a helper function which converts a summary
// information about mined transactions to a TransactionDetail.
func minedTransactionsToDetails(currentHeight int32,
	block base.Block) ([]*lnwallet.TransactionDetail, error) {

	details := make([]*lnwallet.TransactionDetail, 0, len(block.Transactions))
	for _, tx := range block.Transactions {
		txDetail := &lnwallet.TransactionDetail{
			Hash:             *tx.Hash,
			NumConfirmations: currentHeight - block.Height + 1,
			BlockHash:        block.Hash,
			BlockHeight:      block.Height,
			Timestamp:        block.Timestamp,
			TotalFees:        int64(tx.Fee),
		}

		balanceDelta, err := extractBalanceDelta(tx)
		if err != nil {
			return nil, err
		}
		txDetail.Value = balanceDelta

		details = append(details, txDetail)
	}

	return details, nil
}

// unminedTransactionsToDetail is a helper funciton which converts a summary
// for a unconfirmed transaction to a transaction detail.
func unminedTransactionsToDetail(summary base.TransactionSummary) (*lnwallet.TransactionDetail, error) {
	txDetail := &lnwallet.TransactionDetail{
		Hash:      *summary.Hash,
		TotalFees: int64(summary.Fee),
		Timestamp: summary.Timestamp,
	}

	balanceDelta, err := extractBalanceDelta(summary)
	if err != nil {
		return nil, err
	}
	txDetail.Value = balanceDelta

	return txDetail, nil
}

// ListTransactionDetails returns a list of all transactions which are
// relevant to the wallet.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListTransactionDetails() ([]*lnwallet.TransactionDetail, error) {
	// Grab the best block the wallet knows of, we'll use this to calculate
	// # of confirmations shortly below.
	bestBlock := b.wallet.Manager.SyncedTo()
	currentHeight := bestBlock.Height

	// TODO(roasbeef): can replace with start "wallet birthday"
	start := base.NewBlockIdentifierFromHeight(0)
	stop := base.NewBlockIdentifierFromHeight(bestBlock.Height)
	txns, err := b.wallet.GetTransactions(start, stop, nil)
	if err != nil {
		return nil, err
	}

	txDetails := make([]*lnwallet.TransactionDetail, 0,
		len(txns.MinedTransactions)+len(txns.UnminedTransactions))

	// For both confirmed and unconfirme dtransactions, create a
	// TransactionDetail which re-packages the data returned by the base
	// wallet.
	for _, blockPackage := range txns.MinedTransactions {
		details, err := minedTransactionsToDetails(currentHeight, blockPackage)
		if err != nil {
			return nil, err
		}

		txDetails = append(txDetails, details...)
	}
	for _, tx := range txns.UnminedTransactions {
		detail, err := unminedTransactionsToDetail(tx)
		if err != nil {
			return nil, err
		}

		txDetails = append(txDetails, detail)
	}

	return txDetails, nil
}

// txSubscriptionClient encapsulates the transaction notification client from
// the base wallet. Notifications received from the client will be proxied over
// two distinct channels.
type txSubscriptionClient struct {
	txClient base.TransactionNotificationsClient

	confirmed   chan *lnwallet.TransactionDetail
	unconfirmed chan *lnwallet.TransactionDetail

	w *base.Wallet

	wg   sync.WaitGroup
	quit chan struct{}
}

// ConfirmedTransactions returns a channel which will be sent on as new
// relevant transactions are confirmed.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) ConfirmedTransactions() chan *lnwallet.TransactionDetail {
	return t.confirmed
}

// UnconfirmedTransactions returns a channel which will be sent on as
// new relevant transactions are seen within the network.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) UnconfirmedTransactions() chan *lnwallet.TransactionDetail {
	return t.unconfirmed
}

// Cancel finalizes the subscription, cleaning up any resources allocated.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) Cancel() {
	close(t.quit)
	t.wg.Wait()

	t.txClient.Done()
}

// notificationProxier proxies the notifications received by the underlying
// wallet's notification client to a higher-level TransactionSubscription
// client.
func (t *txSubscriptionClient) notificationProxier() {
out:
	for {
		select {
		case txNtfn := <-t.txClient.C:
			// TODO(roasbeef): handle detached blocks
			currentHeight := t.w.Manager.SyncedTo().Height

			// Launch a goroutine to re-package and send
			// notifications for any newly confirmed transactions.
			go func() {
				for _, block := range txNtfn.AttachedBlocks {
					details, err := minedTransactionsToDetails(currentHeight, block)
					if err != nil {
						continue
					}

					for _, d := range details {
						select {
						case t.confirmed <- d:
						case <-t.quit:
							return
						}
					}
				}

			}()

			// Launch a goroutine to re-package and send
			// notifications for any newly unconfirmed transactions.
			go func() {
				for _, tx := range txNtfn.UnminedTransactions {
					detail, err := unminedTransactionsToDetail(tx)
					if err != nil {
						continue
					}

					select {
					case t.unconfirmed <- detail:
					case <-t.quit:
						return
					}
				}
			}()
		case <-t.quit:
			break out
		}
	}

	t.wg.Done()
}

// SubscribeTransactions returns a TransactionSubscription client which
// is capable of receiving async notifications as new transactions
// related to the wallet are seen within the network, or found in
// blocks.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SubscribeTransactions() (lnwallet.TransactionSubscription, error) {
	walletClient := b.wallet.NtfnServer.TransactionNotifications()

	txClient := &txSubscriptionClient{
		txClient:    walletClient,
		confirmed:   make(chan *lnwallet.TransactionDetail),
		unconfirmed: make(chan *lnwallet.TransactionDetail),
		w:           b.wallet,
		quit:        make(chan struct{}),
	}
	txClient.wg.Add(1)
	go txClient.notificationProxier()

	return txClient, nil
}

// IsSynced returns a boolean indicating if from the PoV of the wallet,
// it has fully synced to the current best block in the main chain.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) IsSynced() (bool, error) {
	// Grab the best chain state the wallet is currently aware of.
	syncState := b.wallet.Manager.SyncedTo()

	// Next, query the btcd node to grab the info about the tip of the main
	// chain.
	bestHash, bestHeight, err := b.rpc.GetBestBlock()
	if err != nil {
		return false, err
	}

	// If the wallet hasn't yet fully synced to the node's best chain tip,
	// then we're not yet fully synced.
	if syncState.Height < bestHeight {
		return false, nil
	}

	// If the wallet is on par with the current best chain tip, then we
	// still may not yet be synced as the btcd node may still be catching
	// up to the main chain. So we'll grab the block header in order to
	// make a guess based on the current time stamp.
	blockHeader, err := b.rpc.GetBlockHeader(bestHash)
	if err != nil {
		return false, err
	}

	// If the timestamp no the best header is more than 2 hours in the
	// past, then we're not yet synced.
	minus24Hours := time.Now().Add(-2 * time.Hour)
	return !blockHeader.Timestamp.Before(minus24Hours), nil
}

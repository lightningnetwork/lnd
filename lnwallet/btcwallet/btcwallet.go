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
	lnNamespace          = []byte("ln")
	rootKey              = []byte("ln-root")
	waddrmgrNamespaceKey = []byte("waddrmgr")
)

// BtcWallet is an implementation of the lnwallet.WalletController interface
// backed by an active instance of btcwallet. At the time of the writing of
// this documentation, this implementation requires a full btcd node to
// operate.
type BtcWallet struct {
	// wallet is an active instance of btcwallet.
	wallet *base.Wallet

	chain chain.Interface

	db walletdb.DB

	cfg *Config

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
func New(cfg Config) (*BtcWallet, error) {
	// Ensure the wallet exists or create it when the create flag is set.
	netDir := NetworkDir(cfg.DataDir, cfg.NetParams)

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
		// Wallet has been created and been initialized at this point,
		// open it along with all the required DB namepsaces, and the
		// DB itself.
		wallet, err = loader.OpenExistingWallet(pubPass, false)
		if err != nil {
			return nil, err
		}
	}

	// Create a bucket within the wallet's database dedicated to storing
	// our LN specific data.
	db := wallet.Database()
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		_, err := tx.CreateTopLevelBucket(lnNamespace)
		if err != nil && err != walletdb.ErrBucketExists {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &BtcWallet{
		cfg:       &cfg,
		wallet:    wallet,
		db:        db,
		chain:     cfg.ChainSource,
		netParams: cfg.NetParams,
		utxoCache: make(map[wire.OutPoint]*wire.TxOut),
	}, nil
}

// BackEnd returns the underlying ChainService's name as a string.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) BackEnd() string {
	if b.chain != nil {
		return b.chain.BackEnd()
	}

	return ""
}

// Start initializes the underlying rpc connection, the wallet itself, and
// begins syncing to the current available blockchain state.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) Start() error {
	// Establish an RPC connection in addition to starting the goroutines
	// in the underlying wallet.
	if err := b.chain.Start(); err != nil {
		return err
	}

	// Start the underlying btcwallet core.
	b.wallet.Start()

	// Pass the rpc client into the wallet so it can sync up to the
	// current main chain.
	b.wallet.SynchronizeRPC(b.chain)

	if err := b.wallet.Unlock(b.cfg.PrivatePass, nil); err != nil {
		return err
	}

	return nil
}

// Stop signals the wallet for shutdown. Shutdown may entail closing
// any active sockets, database handles, stopping goroutines, etc.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) Stop() error {
	b.wallet.Stop()

	b.wallet.WaitForShutdown()

	b.chain.Stop()

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
// dictated by the value of the `change` parameter. If change is true, then an
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
		return b.wallet.NewChangeAddress(defaultAccount, addrType)
	}

	return b.wallet.NewAddress(defaultAccount, addrType)
}

// GetPrivKey retrieves the underlying private key associated with the passed
// address. If the we're unable to locate the proper private key, then a
// non-nil error will be returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) GetPrivKey(a btcutil.Address) (*btcec.PrivateKey, error) {
	// Using the ID address, request the private key corresponding to the
	// address from the wallet's address manager.
	return b.wallet.PrivKeyForAddress(a)
}

// NewRawKey retrieves the next key within our HD key-chain for use within as a
// multi-sig key within the funding transaction, or within the commitment
// transaction's outputs.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) NewRawKey() (*btcec.PublicKey, error) {
	addr, err := b.wallet.NewAddress(defaultAccount,
		waddrmgr.WitnessPubKey)
	if err != nil {
		return nil, err
	}

	return b.wallet.PubKeyForAddress(addr)
}

// FetchRootKey returns a root key which is intended to be used as an initial
// seed/salt to generate any Lightning specific secrets.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) FetchRootKey() (*btcec.PrivateKey, error) {
	// Fetch the root address hash from the database, this is persisted
	// locally within the database, then used to obtain the key from the
	// wallet based on the address hash.
	var rootAddrHash []byte
	if err := walletdb.View(b.db, func(tx walletdb.ReadTx) error {
		lnBucket := tx.ReadBucket(lnNamespace)

		rootAddrHash = lnBucket.Get(rootKey)
		return nil
	}); err != nil {
		return nil, err
	}

	if rootAddrHash == nil {
		// Otherwise, we need to generate a fresh address from the
		// wallet, then stores it's hash160 within the database so we
		// can look up the exact key later.
		if err := walletdb.Update(b.db, func(tx walletdb.ReadWriteTx) error {
			addrmgrNs := tx.ReadWriteBucket(waddrmgrNamespaceKey)
			addrs, err := b.wallet.Manager.NextExternalAddresses(addrmgrNs,
				defaultAccount, 1, waddrmgr.WitnessPubKey)
			if err != nil {
				return err
			}
			rootAddr := addrs[0].Address()

			lnBucket := tx.ReadWriteBucket(lnNamespace)

			rootAddrHash = rootAddr.ScriptAddress()
			return lnBucket.Put(rootKey, rootAddrHash)
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

	priv, err := b.wallet.PrivKeyForAddress(rootAddr)
	if err != nil {
		return nil, err
	}

	return priv, nil
}

// SendOutputs funds, signs, and broadcasts a Bitcoin transaction paying out to
// the specified outputs. In the case the wallet has insufficient funds, or the
// outputs are non-standard, a non-nil error will be be returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SendOutputs(outputs []*wire.TxOut,
	feeSatPerByte btcutil.Amount) (*chainhash.Hash, error) {

	// The fee rate is passed in using units of sat/byte, so we'll scale
	// this up to sat/KB as the SendOutputs method requires this unit.
	feeSatPerKB := feeSatPerByte * 1024

	return b.wallet.SendOutputs(outputs, defaultAccount, 1, feeSatPerKB)
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
// coin selection.
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

		var addressType lnwallet.AddressType
		if txscript.IsPayToWitnessPubKeyHash(pkScript) {
			addressType = lnwallet.WitnessPubKey
		} else if txscript.IsPayToScriptHash(pkScript) {
			// TODO(roasbeef): This assumes all p2sh outputs returned by the
			// wallet are nested p2pkh. We can't check the redeem script because
			// the btcwallet service does not include it.
			addressType = lnwallet.NestedWitnessPubKey
		}

		if addressType == lnwallet.WitnessPubKey ||
			addressType == lnwallet.NestedWitnessPubKey {

			txid, err := chainhash.NewHashFromStr(output.TxID)
			if err != nil {
				return nil, err
			}

			utxo := &lnwallet.Utxo{
				AddressType: addressType,
				Value:       btcutil.Amount(output.Amount * 1e8),
				PkScript:    pkScript,
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
func extractBalanceDelta(
	txSummary base.TransactionSummary,
	tx *wire.MsgTx,
) (btcutil.Amount, error) {
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
func minedTransactionsToDetails(
	currentHeight int32,
	block base.Block,
	chainParams *chaincfg.Params,
) ([]*lnwallet.TransactionDetail, error) {

	details := make([]*lnwallet.TransactionDetail, 0, len(block.Transactions))
	for _, tx := range block.Transactions {
		wireTx := &wire.MsgTx{}
		txReader := bytes.NewReader(tx.Transaction)

		if err := wireTx.Deserialize(txReader); err != nil {
			return nil, err
		}

		var destAddresses []btcutil.Address
		for _, txOut := range wireTx.TxOut {
			_, outAddresses, _, err :=
				txscript.ExtractPkScriptAddrs(txOut.PkScript, chainParams)
			if err != nil {
				return nil, err
			}

			destAddresses = append(destAddresses, outAddresses...)
		}

		txDetail := &lnwallet.TransactionDetail{
			Hash:             *tx.Hash,
			NumConfirmations: currentHeight - block.Height + 1,
			BlockHash:        block.Hash,
			BlockHeight:      block.Height,
			Timestamp:        block.Timestamp,
			TotalFees:        int64(tx.Fee),
			DestAddresses:    destAddresses,
		}

		balanceDelta, err := extractBalanceDelta(tx, wireTx)
		if err != nil {
			return nil, err
		}
		txDetail.Value = balanceDelta

		details = append(details, txDetail)
	}

	return details, nil
}

// unminedTransactionsToDetail is a helper function which converts a summary
// for a unconfirmed transaction to a transaction detail.
func unminedTransactionsToDetail(
	summary base.TransactionSummary,
) (*lnwallet.TransactionDetail, error) {
	wireTx := &wire.MsgTx{}
	txReader := bytes.NewReader(summary.Transaction)

	if err := wireTx.Deserialize(txReader); err != nil {
		return nil, err
	}

	txDetail := &lnwallet.TransactionDetail{
		Hash:      *summary.Hash,
		TotalFees: int64(summary.Fee),
		Timestamp: summary.Timestamp,
	}

	balanceDelta, err := extractBalanceDelta(summary, wireTx)
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

	// For both confirmed and unconfirmed transactions, create a
	// TransactionDetail which re-packages the data returned by the base
	// wallet.
	for _, blockPackage := range txns.MinedTransactions {
		details, err := minedTransactionsToDetails(currentHeight, blockPackage, b.netParams)
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
					details, err := minedTransactionsToDetails(currentHeight, block, t.w.ChainParams())
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

	var (
		bestHash   *chainhash.Hash
		bestHeight int32
		err        error
	)

	// Next, query the chain backend to grab the info about the tip of the
	// main chain.
	bestHash, bestHeight, err = b.cfg.ChainSource.GetBestBlock()
	if err != nil {
		return false, err
	}

	// If the wallet hasn't yet fully synced to the node's best chain tip,
	// then we're not yet fully synced.
	if syncState.Height < bestHeight {
		return false, nil
	}

	// If the wallet is on par with the current best chain tip, then we
	// still may not yet be synced as the chain backend may still be
	// catching up to the main chain. So we'll grab the block header in
	// order to make a guess based on the current time stamp.
	blockHeader, err := b.cfg.ChainSource.GetBlockHeader(bestHash)
	if err != nil {
		return false, err
	}

	// If the timestamp no the best header is more than 2 hours in the
	// past, then we're not yet synced.
	minus24Hours := time.Now().Add(-2 * time.Hour)
	return !blockHeader.Timestamp.Before(minus24Hours), nil
}

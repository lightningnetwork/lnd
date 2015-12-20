package lnwallet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
	"github.com/btcsuite/btcutil/txsort"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	btcwallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	msgBufferSize = 100
)

var (
	// Namespace bucket keys.
	lightningNamespaceKey = []byte("ln-wallet")
	waddrmgrNamespaceKey  = []byte("waddrmgr")
	wtxmgrNamespaceKey    = []byte("wtxmgr")

	openChannelBucket   = []byte("o-chans")
	closedChannelBucket = []byte("c-chans")
	fundingTxKey        = []byte("funding")

	// Error types
	ErrInsufficientFunds = errors.New("not enough available outputs to " +
		"create funding transaction")

	// Which bitcoin network are we using?
	ActiveNetParams = &chaincfg.TestNet3Params

	endian = binary.BigEndian
)

type FundingType uint16

const (
	// Use SegWit, assumes CSV+CLTV
	SEGWIT FundingType = iota

	// Use SIGHASH_NOINPUT, assumes CSV+CLTV
	SIGHASH

	// Use CSV without reserve
	CSV

	// Use CSV with reserve
	// Reserve is a permanent amount of funds locked and the capacity.
	CSV_RESERVE

	// CLTV with reserve.
	CLTV_RESERVE
)

// initFundingReserveReq...
type initFundingReserveMsg struct {
	fundingAmount btcutil.Amount
	fundingType   FundingType
	minFeeRate    btcutil.Amount

	// TODO(roasbeef): optional reserve for CLTV, etc.

	// Insuffcient funds etc..
	err chan error // Buffered

	resp chan *ChannelReservation // Buffered
}

// FundingReserveCancelMsg...
type fundingReserveCancelMsg struct {
	pendingFundingID uint64

	// Buffered, used for optionally synchronization.
	err chan error // Buffered
}

// addCounterPartySigsMsg...
type addCounterPartyFundsMsg struct {
	pendingFundingID uint64

	// TODO(roasbeef): Should also carry SPV proofs in we're in SPV mode
	theirInputs        []*wire.TxIn
	theirChangeOutputs []*wire.TxOut
	theirKey           *btcec.PublicKey

	err chan error // Buffered
}

// partiallySignedFundingState...
type partiallySignedFundingState struct {
	// In order of sorted inputs that are ours. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	OurSigs [][]byte

	NormalizedTxID wire.ShaHash
}

// addCounterPartySigsMsg...
type addCounterPartySigsMsg struct {
	pendingFundingID uint64

	// Should be order of sorted inputs that are theirs. Sorting is done in accordance
	// to BIP-69: https://github.com/bitcoin/bips/blob/master/bip-0069.mediawiki.
	theirSigs [][]byte

	err chan error // Buffered
}

// FundingCompleteResp...
type finalizedFundingState struct {
	FundingTxId           wire.ShaHash
	NormalizedFundingTXID wire.ShaHash

	CompletedFundingTx *btcutil.Tx
}

// LightningWallet....
// responsible for internal global (from the point of view of a user/node)
// channel state. Requests to modify this state come in via messages over
// channels, same with replies.
// Embedded wallet backed by boltdb...
type LightningWallet struct {
	// TODO(roasbeef): add btcwallet/chain for notifications initially, then
	// abstract out in order to accomodate zeroMQ/BitcoinCore
	lmtx sync.RWMutex

	db walletdb.DB

	// A namespace within boltdb reserved for ln-based wallet meta-data.
	// TODO(roasbeef): which possible other namespaces are relevant?
	lnNamespace walletdb.Namespace

	wallet *btcwallet.Wallet
	rpc    *chain.Client

	msgChan chan interface{}

	// TODO(roasbeef): zombie garbage collection routine to solve
	// lost-object/starvation problem/attack.
	limboMtx      sync.RWMutex
	nextFundingID uint64
	fundingLimbo  map[uint64]*ChannelReservation

	started int32

	shutdown int32

	quit chan struct{}

	wg sync.WaitGroup

	// TODO(roasbeef): handle wallet lock/unlock
}

// NewLightningWallet...
// TODO(roasbeef): fin...add config
func NewLightningWallet(privWalletPass, pubWalletPass, hdSeed []byte, dataDir string) (*LightningWallet, error) {
	// Ensure the wallet exists or create it when the create flag is set.
	netDir := networkDir(dataDir, ActiveNetParams)
	dbPath := filepath.Join(netDir, walletDbName)

	var pubPass []byte
	if pubWalletPass == nil {
		pubPass = defaultPubPassphrase
	} else {
		pubPass = pubWalletPass
	}

	// Wallet has never been created, perform initial set up.
	if !fileExists(dbPath) {
		// Ensure the data directory for the network exists.
		if err := checkCreateDir(netDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return nil, err
		}

		// Attempt to create  a new wallet
		if err := createWallet(privWalletPass, pubPass, hdSeed, dbPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return nil, err
		}
	}

	// Wallet has been created and been initialized at this point, open it
	// along with all the required DB namepsaces, and the DB itself.
	wallet, db, err := openWallet(pubPass, netDir)
	if err != nil {
		return nil, err
	}

	// Create a special namespace for our unique payment channel related
	// meta-data.
	lnNamespace, err := db.Namespace(lightningNamespaceKey)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): logging

	return &LightningWallet{
		db:          db,
		wallet:      wallet,
		lnNamespace: lnNamespace,
		msgChan:     make(chan interface{}, msgBufferSize),
		// TODO(roasbeef): make this atomic.Uint32 instead? Which is
		// faster, locks or CAS? I'm guessing CAS because assembly:
		//  * https://golang.org/src/sync/atomic/asm_amd64.s
		nextFundingID: 0,
		fundingLimbo:  make(map[uint64]*ChannelReservation),
		quit:          make(chan struct{}),
	}, nil
}

// Start...
func (l *LightningWallet) Start() error {
	// Already started?
	if atomic.AddInt32(&l.started, 1) != 1 {
		return nil
	}
	// TODO(roasbeef): config...

	rpcc, err := chain.NewClient(ActiveNetParams,
		"host", "user", "pass", []byte("cert"), true)
	if err != nil {
		return err
	}

	// Start the goroutines in the underlying wallet.
	l.rpc = rpcc
	l.wallet.Start(rpcc)

	l.wg.Add(1)
	// TODO(roasbeef): multiple request handlers?
	go l.requestHandler()

	return nil
}

// Stop...
func (l *LightningWallet) Stop() error {
	if atomic.AddInt32(&l.shutdown, 1) != 1 {
		return nil
	}

	l.wallet.Stop()

	close(l.quit)
	l.wg.Wait()
	return nil
}

// requestHandler....
func (l *LightningWallet) requestHandler() {
out:
	for {
		select {
		case m := <-l.msgChan:
			switch msg := m.(type) {
			case *initFundingReserveMsg:
				l.handleFundingReserveRequest(msg)
			case *fundingReserveCancelMsg:
				l.handleFundingCancelRequest(msg)
			case *addCounterPartyFundsMsg:
				l.handleFundingCounterPartyFunds(msg)
			case *addCounterPartySigsMsg:
				l.handleFundingCounterPartySigs(msg)
			}
		case <-l.quit:
			// TODO: do some clean up
			break out
		}
	}

	l.wg.Done()
}

// RequestFundingReservation...
func (l *LightningWallet) InitChannelReservation(a btcutil.Amount, t FundingType) (*ChannelReservation, error) {
	errChan := make(chan error, 1)
	respChan := make(chan *ChannelReservation, 1)

	l.msgChan <- &initFundingReserveMsg{
		fundingAmount: a,
		fundingType:   t,
		err:           errChan,
		resp:          respChan,
	}

	return <-respChan, <-errChan
}

// handleFundingReserveRequest...
func (l *LightningWallet) handleFundingReserveRequest(req *initFundingReserveMsg) {
	// Create a limbo and record entry for this newly pending funding request.
	l.limboMtx.Lock()

	id := l.nextFundingID
	reservation := newChannelReservation(req.fundingType, req.fundingAmount, req.minFeeRate, l, id)
	l.nextFundingID++
	l.fundingLimbo[id] = reservation

	l.limboMtx.Unlock()

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	reservation.Lock()
	defer reservation.Unlock()

	// Find all unlocked unspent outputs with greater than 6 confirmations.
	maxConfs := int32(math.MaxInt32)
	unspentOutputs, err := l.wallet.ListUnspent(6, maxConfs, nil)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}

	// Convert the outputs to coins for coin selection below.
	coins, err := outputsToCoins(unspentOutputs)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}

	// Peform coin selection over our available, unlocked unspent outputs
	// in order to find enough coins to meet the funding amount requirements.
	//
	// TODO(roasbeef): Should extend coinset with optimal coin selection
	// heuristics for our use case.
	// TODO(roasbeef): factor in fees..
	// TODO(roasbeef): possibly integrate the fee prediction project? if
	// results hold up...
	// NOTE: this current selection assumes "priority" is still a thing.
	selector := &coinset.MaxValueAgeCoinSelector{
		MaxInputs:       10,
		MinChangeAmount: 10000,
	}
	selectedCoins, err := selector.CoinSelect(req.fundingAmount, coins)
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}

	// Lock the selected coins. These coins are now "reserved", this
	// prevents concurrent funding requests from referring to and this
	// double-spending the same set of coins.
	reservation.ourInputs = make([]*wire.TxIn, len(selectedCoins.Coins()))
	for i, coin := range selectedCoins.Coins() {
		txout := wire.NewOutPoint(coin.Hash(), coin.Index())
		l.wallet.LockOutpoint(*txout)

		// Empty sig script, we'll actually sign if this reservation is
		// queued up to be completed (the other side accepts).
		outPoint := wire.NewOutPoint(coin.Hash(), coin.Index())
		reservation.ourInputs[i] = wire.NewTxIn(outPoint, nil)
	}

	// Create some possibly neccessary change outputs.
	selectedTotalValue := coinset.NewCoinSet(selectedCoins.Coins()).TotalValue()
	reservation.ourChange = make([]*wire.TxOut, 0, len(selectedCoins.Coins()))
	if selectedTotalValue > req.fundingAmount {
		// Change is necessary. Query for an available change address to
		// send the remainder to.
		changeAmount := selectedTotalValue - req.fundingAmount
		addrs, err := l.wallet.Manager.NextInternalAddresses(waddrmgr.DefaultAccountNum, 1)
		if err != nil {
			req.err <- err
			req.resp <- nil
			return
		}
		changeAddrScript, err := txscript.PayToAddrScript(addrs[0].Address())
		if err != nil {
			req.err <- err
			req.resp <- nil
			return
		}
		// TODO(roasbeef): re-enable after tests are connected to real node.
		//   * or the change to btcwallet is made to reverse the dependancy
		//     between chain-client and wallet.
		//changeAddr, err := l.wallet.NewChangeAddress(waddrmgr.DefaultAccountNum)

		reservation.ourChange = append(reservation.ourChange,
			wire.NewTxOut(int64(changeAmount), changeAddrScript))
	}

	// TODO(roasbeef): re-calculate fees here to minFeePerKB, may need more inputs

	multiSigKey, err := l.getNextMultiSigKey()
	if err != nil {
		req.err <- err
		req.resp <- nil
		return
	}

	reservation.partialState.multiSigKey = multiSigKey

	// Funding reservation request succesfully handled. The funding inputs
	// will be marked as unavailable until the reservation is either
	// completed, or cancecled.
	req.resp <- reservation
	req.err <- nil
}

// handleFundingReserveCancel...
func (l *LightningWallet) handleFundingCancelRequest(req *fundingReserveCancelMsg) {
	// TODO(roasbeef): holding lock too long
	// RLOCK?
	l.limboMtx.Lock()
	defer l.limboMtx.Unlock()

	pendingReservation, ok := l.fundingLimbo[req.pendingFundingID]
	if !ok {
		// TODO(roasbeef): make new error, "unkown funding state" or something
		req.err <- fmt.Errorf("attempted to cancel non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Mark all previously locked outpoints as usuable for future funding
	// requests.
	for _, unusedInput := range pendingReservation.ourInputs {
		l.wallet.UnlockOutpoint(unusedInput.PreviousOutPoint)
	}

	// TODO(roasbeef): is it even worth it to keep track of unsed keys?

	// TODO(roasbeef): Is it possible to mark the unused change also as
	// available?

	delete(l.fundingLimbo, req.pendingFundingID)

	req.err <- nil
}

// handleFundingCounterPartyFunds...
func (l *LightningWallet) handleFundingCounterPartyFunds(req *addCounterPartyFundsMsg) {
	l.limboMtx.Lock()
	pendingReservation, ok := l.fundingLimbo[req.pendingFundingID]
	l.limboMtx.Unlock()
	if !ok {
		req.err <- fmt.Errorf("attempted to update non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Create a blank, fresh transaction. Soon to be a complete funding
	// transaction which will allow opening a lightning channel.
	pendingReservation.partialState.fundingTx = wire.NewMsgTx()

	// First, add all multi-party inputs to the transaction
	// TODO(roasbeef); handle case that tx doesn't exist, fake input
	// TODO(roasbeef): validate SPV proof from other side if in SPV mode.
	//  * actually, pure SPV would need fraud proofs right? must prove input
	//    is unspent
	//  * or, something like getutxo?
	for _, ourInput := range pendingReservation.ourInputs {
		pendingReservation.partialState.fundingTx.AddTxIn(ourInput)
	}
	pendingReservation.theirInputs = req.theirInputs
	for _, theirInput := range pendingReservation.theirInputs {
		pendingReservation.partialState.fundingTx.AddTxIn(theirInput)
	}

	// Next, add all multi-party outputs to the transaction. This includes
	// change outputs for both side.
	for _, ourChangeOutput := range pendingReservation.ourChange {
		pendingReservation.partialState.fundingTx.AddTxOut(ourChangeOutput)
	}
	pendingReservation.theirChange = req.theirChangeOutputs
	for _, theirChangeOutput := range pendingReservation.theirChange {
		pendingReservation.partialState.fundingTx.AddTxOut(theirChangeOutput)
	}

	// Finally, add the 2-of-2 multi-sig output which will set up the lightning
	// channel.
	// TODO(roasbeef): Cannonical sorting of keys here?
	//  * also, also this is currently bare-multi sig, keep this for network
	//    transparency or switch to P2SH?
	keys := make([]*btcutil.AddressPubKey, 2)
	ourKey := pendingReservation.partialState.multiSigKey.PubKey().SerializeCompressed()
	keys[0], _ = btcutil.NewAddressPubKey(ourKey, ActiveNetParams)
	pendingReservation.theirMultiSigKey = req.theirKey
	keys[1], _ = btcutil.NewAddressPubKey(pendingReservation.theirMultiSigKey.SerializeCompressed(), ActiveNetParams)
	multiSigScript, err := txscript.MultiSigScript(keys, 2)
	if err != nil {
		req.err <- err
		return
	}
	multiSigOut := wire.NewTxOut(int64(pendingReservation.partialState.capacity),
		multiSigScript)
	pendingReservation.partialState.fundingTx.AddTxOut(multiSigOut)

	// Sort the transaction. Since both side agree to a cannonical
	// ordering, by sorting we no longer need to send the entire
	// transaction. Only signatures will be exchanged.
	txsort.InPlaceSort(pendingReservation.partialState.fundingTx)

	// Now that the transaction has been cannonically sorted, compute the
	// normalized transation ID before we attach our signatures.
	// TODO(roasbeef): this isn't the normalized txid, this isn't recursive...
	// pendingReservation.normalizedTxID = pendingReservation.fundingTx.TxSha()

	// Now, sign all inputs that are ours, collecting the signatures in
	// order of the inputs.
	pendingReservation.ourFundingSigs = make([][]byte, 0, len(pendingReservation.ourInputs))
	for i, txIn := range pendingReservation.partialState.fundingTx.TxIn {
		// Does the wallet know about the txin?
		txDetail, _ := l.wallet.TxStore.TxDetails(&txIn.PreviousOutPoint.Hash)
		if txDetail == nil {
			continue
		}

		// Is this our txin? TODO(roasbeef): assumes all inputs are P2PKH...
		prevIndex := txIn.PreviousOutPoint.Index
		prevOut := txDetail.TxRecord.MsgTx.TxOut[prevIndex]
		_, addrs, _, _ := txscript.ExtractPkScriptAddrs(prevOut.PkScript, ActiveNetParams)
		apkh, ok := addrs[0].(*btcutil.AddressPubKeyHash)
		if !ok {
			req.err <- btcwallet.ErrUnsupportedTransactionType
			return
		}

		ai, err := l.wallet.Manager.Address(apkh)
		if err != nil {
			req.err <- fmt.Errorf("cannot get address info: %v", err)
			return
		}
		pka := ai.(waddrmgr.ManagedPubKeyAddress)
		privkey, err := pka.PrivKey()
		if err != nil {
			req.err <- fmt.Errorf("cannot get private key: %v", err)
			return
		}

		sigscript, err := txscript.SignatureScript(pendingReservation.partialState.fundingTx, i,
			prevOut.PkScript, txscript.SigHashAll, privkey,
			ai.Compressed())
		if err != nil {
			req.err <- fmt.Errorf("cannot create sigscript: %s", err)
			return
		}

		pendingReservation.partialState.fundingTx.TxIn[i].SignatureScript = sigscript
		pendingReservation.ourFundingSigs = append(pendingReservation.ourFundingSigs, sigscript)
	}

	req.err <- nil
}

// handleFundingCounterPartySigs...
func (l *LightningWallet) handleFundingCounterPartySigs(msg *addCounterPartySigsMsg) {
	l.limboMtx.RLock()
	pendingReservation, ok := l.fundingLimbo[msg.pendingFundingID]
	l.limboMtx.RUnlock()
	if !ok {
		msg.err <- fmt.Errorf("attempted to update non-existant funding state")
		return
	}

	// Grab the mutex on the ChannelReservation to ensure thead-safety
	pendingReservation.Lock()
	defer pendingReservation.Unlock()

	// Now we can complete the funding transaction by adding their
	// signatures to their inputs.
	pendingReservation.theirFundingSigs = msg.theirSigs
	fundingTx := pendingReservation.partialState.fundingTx
	for i, txin := range fundingTx.TxIn {
		if txin.SignatureScript == nil {
			txin.SignatureScript = pendingReservation.theirFundingSigs[i]

			/*// Fetch the alleged previous output along with the
			// pkscript referenced by this input.
			prevOut := txin.PreviousOutPoint
			output, err := l.rpc.GetTxOut(&prevOut.Hash, prevOut.Index, false)
			if err != nil {
				// TODO(roasbeef): do this at the start to avoid wasting out time?
				//  8 or a set of nodes "we" run with exposed unauthenticated RPC?
				msg.err <- err
				return
			}
			pkscript, err := hex.DecodeString(output.ScriptPubKey.Hex)
			if err != nil {
				msg.err <- err
				return
			}

			// Ensure that the signature is valid.
			vm, err := txscript.NewEngine(pkscript,
				fundingTx, i, txscript.StandardVerifyFlags, nil)
			if err != nil {
				// TODO(roasbeef): cancel at this stage if invalid sigs?
				msg.err <- fmt.Errorf("cannot create script engine: %s", err)
				return
			}
			if err = vm.Execute(); err != nil {
				msg.err <- fmt.Errorf("cannot validate transaction: %s", err)
				return
			}*/
		}
	}

	// Funding complete, this entry can be removed from limbo.
	l.limboMtx.Lock()
	delete(l.fundingLimbo, pendingReservation.reservationID)
	// TODO(roasbeef): unlock outputs here, Store.InsertTx will handle marking
	// input in unconfirmed tx, so future coin selects don't pick it up
	//  * also record location of change address so can use AddCredit
	l.limboMtx.Unlock()

	// Add the complete funding transaction to the DB, in it's open bucket
	// which will be used for the lifetime of this channel.
	writeErr := l.lnNamespace.Update(func(tx walletdb.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		// TODO(roasbeef): CHECKSUMS, REDUNDANCY, etc etc.
		rootBucket := tx.RootBucket()
		openChanBucket, err := rootBucket.CreateBucketIfNotExists(openChannelBucket)
		if err != nil {
			return err
		}

		// Create a new sub-bucket within the open channel bucket
		// specifically for this channel.
		// TODO(roasbeef): should def be indexed by LNID, cuz mal etc.
		txID := pendingReservation.partialState.fundingTx.TxSha()
		chanBucket, err := openChanBucket.CreateBucketIfNotExists(txID.Bytes())
		if err != nil {
			return err
		}

		// TODO(roasbeef): sync.Pool of buffers in the future.
		var buf bytes.Buffer
		fundingTx.Serialize(&buf)
		return chanBucket.Put(fundingTxKey, buf.Bytes())
	})

	// TODO(roasbeef): broadcast now?
	//  * create goroutine, listens on blockconnected+blockdisconnected channels
	//  * after six blocks, then will create an LightningChannel struct and
	//    send over reservation.
	//  * will need a multi-plexer to fan out, to listen on ListenConnectedBlocks
	//    * should prob be a separate struct/modele
	//  * use NotifySpent in order to catch non-cooperative spends of revoked
	//    commitment txns. Hmm using p2sh or bare multi-sig?
	msg.err <- writeErr
}

// nextMultiSigKey...
// TODO(roasbeef): on shutdown, write state of pending keys, then read back?
func (l *LightningWallet) getNextMultiSigKey() (*btcec.PrivateKey, error) {
	l.lmtx.Lock()
	defer l.lmtx.Unlock()

	nextAddr, err := l.wallet.Manager.NextExternalAddresses(waddrmgr.DefaultAccountNum, 1)
	if err != nil {
		return nil, err
	}

	pkAddr := nextAddr[0].(waddrmgr.ManagedPubKeyAddress)

	return pkAddr.PrivKey()
}

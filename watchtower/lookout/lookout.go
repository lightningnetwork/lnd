package lookout

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/watchtower/blob"
)

// ErrLookoutExiting is an error that is returned when the lookout server is
// in the process of shutting down.
var ErrLookoutExiting = errors.New("lookout server is shutting down")

// Config houses the Lookout's required resources to properly fulfill it's duty,
// including block fetching, querying accepted state updates, and construction
// and publication of justice transactions.
type Config struct {
	// DB provides persistent access to the watchtower's accepted state
	// updates such that they can be queried as new blocks arrive from the
	// network.
	DB DB

	// EpochRegistrar supports the ability to register for events corresponding to
	// newly created blocks.
	EpochRegistrar EpochRegistrar

	// BlockFetcher supports the ability to fetch blocks from the backend or
	// network.
	BlockFetcher BlockFetcher

	// Punisher handles the responsibility of crafting and broadcasting
	// justice transaction for any breached transactions.
	Punisher Punisher

	// MinBackoff is the minimum amount of time to back-off before
	// re-attempting to fetch a block.
	MinBackoff time.Duration

	// MaxBackoff is the maximum amount of time to back-off before
	// re-attempting to fetch a block.
	MaxBackoff time.Duration

	// MaxNumRetries is the maximum number of times that we should
	// re-attempt fetching a block before moving on.
	MaxNumRetries int
}

// Lookout will check any incoming blocks against the transactions found in the
// database, and in case of matches send the information needed to create a
// penalty transaction to the punisher.
type Lookout struct {
	started  int32 // atomic
	shutdown int32 // atomic

	cfg *Config

	wg   sync.WaitGroup
	quit chan struct{}
}

// New constructs a new Lookout from the given LookoutConfig.
func New(cfg *Config) *Lookout {
	return &Lookout{
		cfg:  cfg,
		quit: make(chan struct{}),
	}
}

// Start safely spins up the Lookout and begins monitoring for breaches.
func (l *Lookout) Start() error {
	if !atomic.CompareAndSwapInt32(&l.started, 0, 1) {
		return nil
	}

	log.Infof("Starting lookout")

	startEpoch, err := l.cfg.DB.GetLookoutTip()
	if err != nil {
		return err
	}

	if startEpoch == nil {
		log.Infof("Starting lookout from chain tip")
	} else {
		log.Infof("Starting lookout from epoch(height=%d hash=%v)",
			startEpoch.Height, startEpoch.Hash)
	}

	events, err := l.cfg.EpochRegistrar.RegisterBlockEpochNtfn(startEpoch)
	if err != nil {
		log.Errorf("Unable to register for block epochs: %v", err)
		return err
	}

	l.wg.Add(1)
	go l.watchBlocks(events)

	log.Infof("Lookout started successfully")

	return nil
}

// Stop safely shuts down the Lookout.
func (l *Lookout) Stop() error {
	if !atomic.CompareAndSwapInt32(&l.shutdown, 0, 1) {
		return nil
	}

	log.Infof("Stopping lookout")

	close(l.quit)
	l.wg.Wait()

	log.Infof("Lookout stopped successfully")

	return nil
}

// fetchBlockWithRetries attempts to fetch a block from the blockchain using
// its hash. If it fails to fetch the block, it will back-off and retry up to
// MaxNumRetries times.
func (l *Lookout) fetchBlockWithRetries(hash *chainhash.Hash) (*wire.MsgBlock,
	error) {

	backoff := l.cfg.MinBackoff

	updateBackoff := func() {
		backoff *= 2
		if backoff > l.cfg.MaxBackoff {
			backoff = l.cfg.MaxBackoff
		}
	}

	var attempt int
	for {
		attempt++

		block, err := l.cfg.BlockFetcher.GetBlock(hash)
		if err == nil {
			return block, nil
		}

		if attempt > l.cfg.MaxNumRetries {
			return nil, fmt.Errorf("failed to fetch block %s "+
				"after %d attempts: %v", hash, attempt, err)
		}

		log.Errorf("Failed to fetch block %s (attempt %d): %v. "+
			"Retrying in %v seconds", hash, attempt, err,
			backoff.Seconds())

		select {
		case <-time.After(backoff):
		case <-l.quit:
			return nil, ErrLookoutExiting
		}

		updateBackoff()
	}
}

// watchBlocks serially pulls incoming epochs from the epoch source and searches
// our accepted state updates for any breached transactions. If any are found,
// we will attempt to decrypt the state updates' encrypted blobs and exact
// justice for the victim.
//
// This method MUST be run as a goroutine.
func (l *Lookout) watchBlocks(epochs *chainntnfs.BlockEpochEvent) {
	defer l.wg.Done()
	defer epochs.Cancel()

	for {
		select {
		case epoch := <-epochs.Epochs:
			log.Debugf("Fetching block for (height=%d, hash=%s)",
				epoch.Height, epoch.Hash)

			// Fetch the full block corresponding to the newly
			// arriving epoch from the backend.
			block, err := l.fetchBlockWithRetries(epoch.Hash)
			if err != nil {
				log.Errorf("Unable to fetch block for "+
					"(height=%x, hash=%s): %v",
					epoch.Height, epoch.Hash, err)
				continue
			}

			// Process the block to see if it contains any breaches
			// that we are monitoring on behalf of our clients.
			err = l.processEpoch(epoch, block)
			if err != nil {
				log.Errorf("Unable to process %v: %v",
					epoch, err)
			}

		case <-l.quit:
			return
		}
	}
}

// processEpoch accepts an Epoch and queries the database for any matching state
// updates for the confirmed transactions. If any are found, the lookout
// responds by attempting to decrypt the encrypted blob and publishing the
// justice transaction.
func (l *Lookout) processEpoch(epoch *chainntnfs.BlockEpoch,
	block *wire.MsgBlock) error {

	numTxnsInBlock := len(block.Transactions)

	log.Debugf("Scanning %d transaction in block (height=%d, hash=%s) "+
		"for breaches", numTxnsInBlock, epoch.Height, epoch.Hash)

	// Iterate over the transactions contained in the block, deriving a
	// breach hint for each transaction and constructing an index mapping
	// the hint back to it's original transaction.
	hintToTx := make(map[blob.BreachHint]*wire.MsgTx, numTxnsInBlock)
	txHints := make([]blob.BreachHint, 0, numTxnsInBlock)
	for _, tx := range block.Transactions {
		hash := tx.TxHash()
		hint := blob.NewBreachHintFromHash(&hash)

		txHints = append(txHints, hint)
		hintToTx[hint] = tx.Copy()
	}

	// Query the database to see if any of the breach hints cause a match
	// with any of our accepted state updates.
	matches, err := l.cfg.DB.QueryMatches(txHints)
	if err != nil {
		return err
	}

	// No matches were found, we are done.
	if len(matches) == 0 {
		log.Debugf("No breaches found in (height=%d, hash=%s)",
			epoch.Height, epoch.Hash)
		return nil
	}

	breachCountStr := "breach"
	if len(matches) > 1 {
		breachCountStr = "breaches"
	}

	log.Infof("Found %d %s in (height=%d, hash=%s)",
		len(matches), breachCountStr, epoch.Height, epoch.Hash)

	// For each match, use our index to retrieve the original transaction,
	// which corresponds to the breaching commitment transaction. If the
	// decryption succeeds, we will accumulate the assembled justice
	// descriptors in a single slice
	var successes []*JusticeDescriptor
	for _, match := range matches {
		commitTx := hintToTx[match.Hint]
		log.Infof("Dispatching punisher for client %s, breach-txid=%s",
			match.ID, commitTx.TxHash())

		// The decryption key for the state update should be the full
		// txid of the breaching commitment transaction.
		// The decryption key for the state update should be computed as
		//   key = SHA256(txid || txid).
		breachTxID := commitTx.TxHash()
		breachKey := blob.NewBreachKeyFromHash(&breachTxID)

		// Now, decrypt the blob of justice that we received in the
		// state update. This will contain all information required to
		// sweep the breached commitment outputs.
		justiceKit, err := blob.Decrypt(
			breachKey, match.EncryptedBlob,
			match.SessionInfo.Policy.BlobType,
		)
		if err != nil {
			// If the decryption fails, this implies either that the
			// client sent an invalid blob, or that the breach hint
			// caused a match on the txid, but this isn't actually
			// the right transaction.
			log.Debugf("Unable to decrypt blob for client %s, "+
				"breach-txid %s: %v", match.ID,
				commitTx.TxHash(), err)
			continue
		}

		justiceDesc := &JusticeDescriptor{
			BreachedCommitTx: commitTx,
			SessionInfo:      match.SessionInfo,
			JusticeKit:       justiceKit,
		}
		successes = append(successes, justiceDesc)
	}

	// TODO(conner): mark successfully decrypted blob so that we can
	// reliably rebroadcast on startup

	// Now, we'll dispatch a punishment for each successful match in
	// parallel. This will assemble the justice transaction for each and
	// watch for their confirmation on chain.
	for _, justiceDesc := range successes {
		l.wg.Add(1)
		go l.dispatchPunisher(justiceDesc)
	}

	return l.cfg.DB.SetLookoutTip(epoch)
}

// dispatchPunisher accepts a justice descriptor corresponding to a successfully
// decrypted blob.  The punisher will then construct the witness scripts and
// witness stacks for the breached outputs. If construction of the justice
// transaction is successful, it will be published to the network to retrieve
// the funds and claim the watchtower's reward.
//
// This method MUST be run as a goroutine.
func (l *Lookout) dispatchPunisher(desc *JusticeDescriptor) {
	defer l.wg.Done()

	// Give the justice descriptor to the punisher to construct and publish
	// the justice transaction. The lookout's quit channel is provided so
	// that long-running tasks that watch for on-chain events can be
	// canceled during shutdown since this method is waitgrouped.
	err := l.cfg.Punisher.Punish(desc, l.quit)
	if err != nil {
		log.Errorf("Unable to punish breach-txid %s for %s: %v",
			desc.BreachedCommitTx.TxHash(), desc.SessionInfo.ID,
			err)
		return
	}

	log.Infof("Punishment for client %s with breach-txid=%s dispatched",
		desc.SessionInfo.ID, desc.BreachedCommitTx.TxHash())
}

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// preschoolBucket stores outputs from commitment transactions that
	// have been broadcast, but not yet confirmed. This set of outputs is
	// persisted in case the system is shut down between the time when the
	// commitment has been broadcast and the time the transaction has been
	// confirmed on the blockchain.
	// TODO(roasbeef): modify schema later to be:
	//  * chanPoint ->
	//               {outpoint1} -> info
	//               {outpoint2} -> info
	preschoolBucket = []byte("psc")

	// preschoolIndex is an index that maps original chanPoint that created
	// the channel to all the active time-locked outpoints for that
	// channel.
	preschoolIndex = []byte("preschool-index")

	// kindergartenBucket stores outputs from commitment transactions that
	// have received an initial confirmation, but which aren't yet
	// spendable because they require additional confirmations enforced by
	// CheckSequenceVerify. Once required additional confirmations have
	// been reported, a sweep transaction will be created to move the funds
	// out of these outputs. After a further six confirmations have been
	// reported, the outputs will be deleted from this bucket. The purpose
	// of this additional wait time is to ensure that a block
	// reorganization doesn't result in the sweep transaction getting
	// re-organized out of the chain.
	// TODO(roasbeef): modify schema later to be:
	//   * height ->
	//              {chanPoint} -> info
	kindergartenBucket = []byte("kdg")

	// contractIndex is an index that maps a contract's channel point to
	// the current information pertaining to the maturity of outputs within
	// that contract. Items are inserted into this index once they've been
	// accepted to pre-school and deleted after the output has been fully
	// swept.
	//
	// mapping: chanPoint -> graduationHeight || byte-offset-in-kindergartenBucket
	contractIndex = []byte("contract-index")

	// lastGraduatedHeightKey is used to persist the last block height that
	// has been checked for graduating outputs. When the nursery is
	// restarted, lastGraduatedHeightKey is used to determine the point
	// from which it's necessary to catch up.
	lastGraduatedHeightKey = []byte("lgh")

	byteOrder = binary.BigEndian
)

var (
	// ErrContractNotFound is returned when the nursery is unable to
	// retreive information about a queried contract.
	ErrContractNotFound = fmt.Errorf("unable to locate contract")
)

// utxoNursery is a system dedicated to incubating time-locked outputs created
// by the broadcast of a commitment transaction either by us, or the remote
// peer. The nursery accepts outputs and "incubates" them until they've reached
// maturity, then sweep the outputs into the source wallet. An output is
// considered mature after the relative time-lock within the pkScript has
// passed. As outputs reach their maturity age, they're swept in batches into
// the source wallet, returning the outputs so they can be used within future
// channels, or regular Bitcoin transactions.
type utxoNursery struct {
	sync.RWMutex

	notifier chainntnfs.ChainNotifier
	wallet   *lnwallet.LightningWallet

	db *channeldb.DB

	requests chan *incubationRequest

	started uint32
	stopped uint32
	quit    chan struct{}
	wg      sync.WaitGroup
}

// newUtxoNursery creates a new instance of the utxoNursery from a
// ChainNotifier and LightningWallet instance.
func newUtxoNursery(db *channeldb.DB, notifier chainntnfs.ChainNotifier,
	wallet *lnwallet.LightningWallet) *utxoNursery {

	return &utxoNursery{
		notifier: notifier,
		wallet:   wallet,
		requests: make(chan *incubationRequest),
		db:       db,
		quit:     make(chan struct{}),
	}
}

// Start launches all goroutines the utxoNursery needs to properly carry out
// its duties.
func (u *utxoNursery) Start() error {
	if !atomic.CompareAndSwapUint32(&u.started, 0, 1) {
		return nil
	}

	utxnLog.Tracef("Starting UTXO nursery")

	// Query the database for the most recently processed block. We'll use
	// this to strict the search space when asking for confirmation
	// notifications, and also to scan the chain to graduate now mature
	// outputs.
	var lastGraduatedHeight uint32
	err := u.db.View(func(tx *bolt.Tx) error {
		kgtnBucket := tx.Bucket(kindergartenBucket)
		if kgtnBucket == nil {
			return nil
		}
		heightBytes := kgtnBucket.Get(lastGraduatedHeightKey)
		if heightBytes == nil {
			return nil
		}

		lastGraduatedHeight = byteOrder.Uint32(heightBytes)
		return nil
	})
	if err != nil {
		return err
	}

	if err := u.reloadPreschool(lastGraduatedHeight); err != nil {
		return err
	}

	// Register with the notifier to receive notifications for each newly
	// connected block. We register during startup to ensure that no blocks
	// are missed while we are handling blocks that were missed during the
	// time the UTXO nursery was unavailable.
	newBlockChan, err := u.notifier.RegisterBlockEpochNtfn()
	if err != nil {
		return err
	}
	if err := u.catchUpKindergarten(lastGraduatedHeight); err != nil {
		return err
	}

	u.wg.Add(1)
	go u.incubator(newBlockChan, lastGraduatedHeight)

	return nil
}

// reloadPreschool re-initializes the chain notifier with all of the outputs
// that had been saved to the "preschool" database bucket prior to shutdown.
func (u *utxoNursery) reloadPreschool(heightHint uint32) error {
	return u.db.View(func(tx *bolt.Tx) error {
		psclBucket := tx.Bucket(preschoolBucket)
		if psclBucket == nil {
			return nil
		}

		return psclBucket.ForEach(func(outputBytes, kidBytes []byte) error {
			var psclOutput kidOutput
			err := psclOutput.Decode(bytes.NewBuffer(kidBytes))
			if err != nil {
				return err
			}

			sourceTxid := psclOutput.OutPoint().Hash

			confChan, err := u.notifier.RegisterConfirmationsNtfn(
				&sourceTxid, 1, heightHint,
			)
			if err != nil {
				return err
			}

			utxnLog.Infof("Preschool outpoint %v re-registered for confirmation "+
				"notification.", psclOutput.OutPoint())
			go psclOutput.waitForPromotion(u.db, confChan)
			return nil
		})
	})
}

// catchUpKindergarten handles the graduation of kindergarten outputs from
// blocks that were missed while the UTXO Nursery was down or offline.
// graduateMissedBlocks is called during the startup of the UTXO Nursery.
func (u *utxoNursery) catchUpKindergarten(lastGraduatedHeight uint32) error {
	// Get the most recently mined block
	_, bestHeight, err := u.wallet.Cfg.ChainIO.GetBestBlock()
	if err != nil {
		return err
	}

	// If we haven't yet seen any registered force closes, or we're already
	// caught up with the current best chain, then we can exit early.
	if lastGraduatedHeight == 0 || uint32(bestHeight) == lastGraduatedHeight {
		return nil
	}

	utxnLog.Infof("Processing outputs from missed blocks. Starting with "+
		"blockHeight: %v, to current blockHeight: %v", lastGraduatedHeight,
		bestHeight)

	// Loop through and check for graduating outputs at each of the missed
	// block heights.
	for graduationHeight := lastGraduatedHeight + 1; graduationHeight <= uint32(bestHeight); graduationHeight++ {
		utxnLog.Debugf("Attempting to graduate outputs at height=%v",
			graduationHeight)

		if err := u.graduateKindergarten(graduationHeight); err != nil {
			return err
		}
	}

	utxnLog.Infof("UTXO Nursery is now fully synced")

	return nil
}

// Stop gracefully shuts down any lingering goroutines launched during normal
// operation of the utxoNursery.
func (u *utxoNursery) Stop() error {
	if !atomic.CompareAndSwapUint32(&u.stopped, 0, 1) {
		return nil
	}

	utxnLog.Infof("UTXO nursery shutting down")

	close(u.quit)
	u.wg.Wait()

	return nil
}

// incubationRequest is a request to the utxoNursery to incubate a set of
// outputs until their mature, finally sweeping them into the wallet once
// available.
type incubationRequest struct {
	outputs []*kidOutput
}

// incubateOutputs sends a request to utxoNursery to incubate the outputs
// defined within the summary of a closed channel. Individually, as all outputs
// reach maturity they'll be swept back into the wallet.
func (u *utxoNursery) IncubateOutputs(closeSummary *lnwallet.ForceCloseSummary) {
	var incReq incubationRequest

	// It could be that our to-self output was below the dust limit. In
	// that case the SignDescriptor would be nil and we would not have that
	// output to incubate.
	if closeSummary.SelfOutputSignDesc != nil {
		selfOutput := makeKidOutput(
			&closeSummary.SelfOutpoint,
			&closeSummary.ChanPoint,
			closeSummary.SelfOutputMaturity,
			lnwallet.CommitmentTimeLock,
			closeSummary.SelfOutputSignDesc,
		)

		incReq.outputs = append(incReq.outputs, &selfOutput)
	}

	// If there are no outputs to incubate, there is nothing to send to the
	// request channel.
	if len(incReq.outputs) != 0 {
		u.requests <- &incReq
	}
}

// incubator is tasked with watching over all outputs from channel closes as
// they transition from being broadcast (at which point they move into the
// "preschool state"), then confirmed and waiting for the necessary number of
// blocks to be confirmed (as specified as kidOutput.blocksToMaturity and
// enforced by CheckSequenceVerify). When the necessary block height has been
// reached, the output has "matured" and the waitForGraduation function will
// generate a sweep transaction to move funds from the commitment transaction
// into the user's wallet.
func (u *utxoNursery) incubator(newBlockChan *chainntnfs.BlockEpochEvent,
	startingHeight uint32) {

	defer u.wg.Done()
	defer newBlockChan.Cancel()

	currentHeight := startingHeight
out:
	for {
		select {

		case preschoolRequest := <-u.requests:
			utxnLog.Infof("Incubating %v new outputs",
				len(preschoolRequest.outputs))

			for _, output := range preschoolRequest.outputs {
				// We'll skip any zero value'd outputs as this
				// indicates we don't have a settled balance
				// within the commitment transaction.
				if output.Amount() == 0 {
					continue
				}

				sourceTxid := output.OutPoint().Hash

				if err := output.enterPreschool(u.db); err != nil {
					utxnLog.Errorf("unable to add kidOutput to preschool: %v, %v ",
						output, err)
					continue
				}

				// Register for a notification that will
				// trigger graduation from preschool to
				// kindergarten when the channel close
				// transaction has been confirmed.
				confChan, err := u.notifier.RegisterConfirmationsNtfn(
					&sourceTxid, 1, currentHeight,
				)
				if err != nil {
					utxnLog.Errorf("unable to register output for confirmation: %v",
						sourceTxid)
					continue
				}

				// Launch a dedicated goroutine that will move
				// the output from the preschool bucket to the
				// kindergarten bucket once the channel close
				// transaction has been confirmed.
				go output.waitForPromotion(u.db, confChan)
			}

		case epoch, ok := <-newBlockChan.Epochs:
			// If the epoch channel has been closed, then the
			// ChainNotifier is exiting which means the daemon is
			// as well. Therefore, we exit early also in order to
			// ensure the daemon shuts down gracefully, yet
			// swiftly.
			if !ok {
				return
			}

			// TODO(roasbeef): if the BlockChainIO is rescanning
			// will give stale data

			// A new block has just been connected to the main
			// chain which means we might be able to graduate some
			// outputs out of the kindergarten bucket. Graduation
			// entails successfully sweeping a time-locked output.
			height := uint32(epoch.Height)
			currentHeight = height
			if err := u.graduateKindergarten(height); err != nil {
				utxnLog.Errorf("error while graduating "+
					"kindergarten outputs: %v", err)
			}

		case <-u.quit:
			break out
		}
	}
}

// contractMaturityReport is a report that details the maturity progress of a
// particular force closed contract.
type contractMaturityReport struct {
	// chanPoint is the channel point of the original contract that is now
	// awaiting maturity within the utxoNursery.
	chanPoint wire.OutPoint

	// limboBalance is the total number of frozen coins within this
	// contract.
	limboBalance btcutil.Amount

	// confirmationHeight is the block height that this output originally
	// confirmed at.
	confirmationHeight uint32

	// maturityRequirement is the input age required for this output to
	// reach maturity.
	maturityRequirement uint32

	// maturityHeight is the absolute block height that this output will
	// mature at.
	maturityHeight uint32
}

// NurseryReport attempts to return a nursery report stored for the target
// outpoint. A nursery report details the maturity/sweeping progress for a
// contract that was previously force closed. If a report entry for the target
// chanPoint is unable to be constructed, then an error will be returned.
func (u *utxoNursery) NurseryReport(chanPoint *wire.OutPoint) (*contractMaturityReport, error) {
	var report *contractMaturityReport
	if err := u.db.View(func(tx *bolt.Tx) error {
		// First we'll examine the preschool bucket as the target
		// contract may not yet have been confirmed.
		psclBucket := tx.Bucket(preschoolBucket)
		if psclBucket == nil {
			return nil
		}
		psclIndex := tx.Bucket(preschoolIndex)
		if psclIndex == nil {
			return nil
		}

		var b bytes.Buffer
		if err := writeOutpoint(&b, chanPoint); err != nil {
			return err
		}
		chanPointBytes := b.Bytes()

		var outputReader *bytes.Reader

		// If the target contract hasn't been confirmed yet, then we
		// can just construct the report from this information.
		if outPoint := psclIndex.Get(chanPointBytes); outPoint != nil {
			// The channel entry hasn't yet been fully confirmed
			// yet, so we'll dig into the preschool bucket to fetch
			// the channel information.
			outputBytes := psclBucket.Get(outPoint)
			if outputBytes == nil {
				return nil
			}

			outputReader = bytes.NewReader(outputBytes)
		} else {
			// Otherwise, we'll have to consult out contract index,
			// so fetch that bucket as well as the kindergarten
			// bucket.
			indexBucket := tx.Bucket(contractIndex)
			if indexBucket == nil {
				return fmt.Errorf("contract not found, " +
					"contract index not populated")
			}
			kgtnBucket := tx.Bucket(kindergartenBucket)
			if kgtnBucket == nil {
				return fmt.Errorf("contract not found, " +
					"kindergarten bucket not populated")
			}

			// Attempt to query the index to see if we have an
			// entry for this particular contract.
			indexInfo := indexBucket.Get(chanPointBytes)
			if indexInfo == nil {
				return ErrContractNotFound
			}

			// If an entry is found, then using the height store in
			// the first 4 bytes, we'll fetch the height that this
			// entry matures at.
			height := indexInfo[:4]
			heightRow := kgtnBucket.Get(height)
			if heightRow == nil {
				return ErrContractNotFound
			}

			// Once we have the entry itself, we'll slice of the
			// last for bytes so we can seek into this row to fetch
			// the contract's information.
			offset := byteOrder.Uint32(indexInfo[4:])
			outputReader = bytes.NewReader(heightRow[offset:])
		}

		// With the proper set of bytes received, we'll deserialize the
		// information for this immature output.
		var immatureOutput kidOutput
		if err := immatureOutput.Decode(outputReader); err != nil {
			return err
		}

		// TODO(roasbeef): should actually be list of outputs
		report = &contractMaturityReport{
			chanPoint:           *chanPoint,
			limboBalance:        immatureOutput.Amount(),
			maturityRequirement: immatureOutput.BlocksToMaturity(),
		}

		// If the confirmation height is set, then this means the
		// contract has been confirmed, and we know the final maturity
		// height.
		if immatureOutput.ConfHeight() != 0 {
			report.confirmationHeight = immatureOutput.ConfHeight()
			report.maturityHeight = (immatureOutput.BlocksToMaturity() +
				immatureOutput.ConfHeight())
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return report, nil
}

// enterPreschool is the first stage in the process of transferring funds from
// a force closed channel into the user's wallet. When an output is in the
// "preschool" stage, the daemon is waiting for the initial confirmation of the
// commitment transaction.
func (k *kidOutput) enterPreschool(db *channeldb.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		psclBucket, err := tx.CreateBucketIfNotExists(preschoolBucket)
		if err != nil {
			return err
		}
		psclIndex, err := tx.CreateBucketIfNotExists(preschoolIndex)
		if err != nil {
			return err
		}

		// Once we have the buckets we can insert the raw bytes of the
		// immature outpoint into the preschool bucket.
		var outpointBytes bytes.Buffer
		if err := writeOutpoint(&outpointBytes, k.OutPoint()); err != nil {
			return err
		}
		var kidBytes bytes.Buffer
		if err := k.Encode(&kidBytes); err != nil {
			return err
		}
		err = psclBucket.Put(outpointBytes.Bytes(), kidBytes.Bytes())
		if err != nil {
			return err
		}

		// Additionally, we'll populate the preschool index so we can
		// track all the immature outpoints for a particular channel's
		// chanPoint.
		var b bytes.Buffer
		err = writeOutpoint(&b, k.OriginChanPoint())
		if err != nil {
			return err
		}
		err = psclIndex.Put(b.Bytes(), outpointBytes.Bytes())
		if err != nil {
			return err
		}

		utxnLog.Infof("Outpoint %v now in preschool, waiting for "+
			"initial confirmation", k.OutPoint())

		return nil
	})
}

// waitForPromotion is intended to be run as a goroutine that will wait until a
// channel force close commitment transaction has been included in a confirmed
// block. Once the transaction has been confirmed (as reported by the Chain
// Notifier), waitForPromotion will delete the output from the "preschool"
// database bucket and atomically add it to the "kindergarten" database bucket.
// This is the second step in the output incubation process.
func (k *kidOutput) waitForPromotion(db *channeldb.DB, confChan *chainntnfs.ConfirmationEvent) {
	txConfirmation, ok := <-confChan.Confirmed
	if !ok {
		utxnLog.Errorf("notification chan "+
			"closed, can't advance output %v", k.OutPoint())
		return
	}

	utxnLog.Infof("Outpoint %v confirmed in block %v moving to kindergarten",
		k.OutPoint(), txConfirmation.BlockHeight)

	k.SetConfHeight(txConfirmation.BlockHeight)

	// The following block deletes a kidOutput from the preschool database
	// bucket and adds it to the kindergarten database bucket which is
	// keyed by block height. Keys and values are serialized into byte
	// array form prior to database insertion.
	err := db.Update(func(tx *bolt.Tx) error {
		var originPoint bytes.Buffer
		if err := writeOutpoint(&originPoint, k.OriginChanPoint()); err != nil {
			return err
		}

		psclBucket := tx.Bucket(preschoolBucket)
		if psclBucket == nil {
			return errors.New("unable to open preschool bucket")
		}
		psclIndex := tx.Bucket(preschoolIndex)
		if psclIndex == nil {
			return errors.New("unable to open preschool index")
		}

		// Now that the entry has been confirmed, in order to move it
		// along in the maturity pipeline we first delete the entry
		// from the preschool bucket, as well as the secondary index.
		var outpointBytes bytes.Buffer
		if err := writeOutpoint(&outpointBytes, k.OutPoint()); err != nil {
			return err
		}
		if err := psclBucket.Delete(outpointBytes.Bytes()); err != nil {
			utxnLog.Errorf("unable to delete kindergarten output from "+
				"preschool bucket: %v", k.OutPoint())
			return err
		}
		if err := psclIndex.Delete(originPoint.Bytes()); err != nil {
			utxnLog.Errorf("unable to delete kindergarten output from "+
				"preschool index: %v", k.OutPoint())
			return err
		}

		// Next, fetch the kindergarten bucket. This output will remain
		// in this bucket until it's fully mature.
		kgtnBucket, err := tx.CreateBucketIfNotExists(kindergartenBucket)
		if err != nil {
			return err
		}

		maturityHeight := k.ConfHeight() + k.BlocksToMaturity()

		heightBytes := make([]byte, 4)
		byteOrder.PutUint32(heightBytes, maturityHeight)

		// If there're any existing outputs for this particular block
		// height target, then we'll append this new output to the
		// serialized list of outputs.
		var existingOutputs []byte
		if results := kgtnBucket.Get(heightBytes); results != nil {
			existingOutputs = results
		}

		// We'll grab the output's offset in the value for its maturity
		// height so we can add this to the contract index.
		outputOffset := len(existingOutputs)

		b := bytes.NewBuffer(existingOutputs)
		if err := k.Encode(b); err != nil {
			return err
		}
		if err := kgtnBucket.Put(heightBytes, b.Bytes()); err != nil {
			return err
		}

		// Finally, we'll insert a new entry into the contract index.
		// The entry itself consists of 4 bytes for the height, and 4
		// bytes for the offset within the value for the height.
		var indexEntry [4 + 4]byte
		copy(indexEntry[:4], heightBytes)
		byteOrder.PutUint32(indexEntry[4:], uint32(outputOffset))

		indexBucket, err := tx.CreateBucketIfNotExists(contractIndex)
		if err != nil {
			return err
		}
		err = indexBucket.Put(originPoint.Bytes(), indexEntry[:])
		if err != nil {
			return err
		}

		utxnLog.Infof("Outpoint %v now in kindergarten, will mature "+
			"at height %v (delay of %v)", k.OutPoint(),
			maturityHeight, k.BlocksToMaturity())
		return nil
	})
	if err != nil {
		utxnLog.Errorf("unable to move kid output from preschool bucket "+
			"to kindergarten bucket: %v", err)
	}
}

// graduateKindergarten handles the steps invoked with moving funds from a
// force close commitment transaction into a user's wallet after the output
// from the commitment transaction has become spendable. graduateKindergarten
// is called both when a new block notification has been received and also at
// startup in order to process graduations from blocks missed while the UTXO
// nursery was offline.
// TODO(roasbeef): single db transaction for the below
func (u *utxoNursery) graduateKindergarten(blockHeight uint32) error {
	// First fetch the set of outputs that we can "graduate" at this
	// particular block height. We can graduate an output once we've
	// reached its height maturity.
	kgtnOutputs, err := fetchGraduatingOutputs(u.db, u.wallet, blockHeight)
	if err != nil {
		return err
	}

	// If we're able to graduate any outputs, then create a single
	// transaction which sweeps them all into the wallet.
	if len(kgtnOutputs) > 0 {
		err := sweepGraduatingOutputs(u.wallet, kgtnOutputs)
		if err != nil {
			return err
		}

		// Now that the sweeping transaction has been broadcast, for
		// each of the immature outputs, we'll mark them as being fully
		// closed within the database.
		for _, closedChan := range kgtnOutputs {
			err := u.db.MarkChanFullyClosed(closedChan.OriginChanPoint())
			if err != nil {
				return err
			}
		}
	}

	// Using a re-org safety margin of 6-blocks, delete any outputs which
	// have graduated 6 blocks ago.
	deleteHeight := blockHeight - 6
	if err := deleteGraduatedOutputs(u.db, deleteHeight); err != nil {
		return err
	}

	// Finally, record the last height at which we graduated outputs so we
	// can reconcile our state with that of the main-chain during restarts.
	return putLastHeightGraduated(u.db, blockHeight)
}

// fetchGraduatingOutputs checks the "kindergarten" database bucket whenever a
// new block is received in order to determine if commitment transaction
// outputs have become newly spendable. If fetchGraduatingOutputs finds outputs
// that are ready for "graduation," it passes them on to be swept.  This is the
// third step in the output incubation process.
func fetchGraduatingOutputs(db *channeldb.DB, wallet *lnwallet.LightningWallet,
	blockHeight uint32) ([]*kidOutput, error) {

	var results []byte
	if err := db.View(func(tx *bolt.Tx) error {
		// A new block has just been connected, check to see if we have
		// any new outputs that can be swept into the wallet.
		kgtnBucket := tx.Bucket(kindergartenBucket)
		if kgtnBucket == nil {
			return nil
		}

		heightBytes := make([]byte, 4)
		byteOrder.PutUint32(heightBytes, blockHeight)

		results = kgtnBucket.Get(heightBytes)
		return nil
	}); err != nil {
		return nil, err
	}

	// If no time-locked outputs can be swept at this point, then we can
	// exit early.
	if len(results) == 0 {
		return nil, nil
	}

	// Otherwise, we deserialize the list of kid outputs into their full
	// forms.
	kgtnOutputs, err := deserializeKidList(bytes.NewReader(results))
	if err != nil {
		utxnLog.Errorf("error while deserializing list of kidOutputs: %v", err)
	}

	// For each of the outputs, we also generate its proper witness
	// function based on its witness type. This varies if the output is on
	// our commitment transaction or theirs, and also if it's an HTLC
	// output or not.
	for _, kgtnOutput := range kgtnOutputs {
		kgtnOutput.witnessFunc = kgtnOutput.witnessType.GenWitnessFunc(
			wallet.Cfg.Signer, kgtnOutput.SignDesc())
	}

	utxnLog.Infof("New block: height=%v, sweeping %v mature outputs",
		blockHeight, len(kgtnOutputs))

	return kgtnOutputs, nil
}

// sweepGraduatingOutputs generates and broadcasts the transaction that
// transfers control of funds from a channel commitment transaction to the
// user's wallet.
func sweepGraduatingOutputs(wallet *lnwallet.LightningWallet, kgtnOutputs []*kidOutput) error {
	// Create a transaction which sweeps all the newly mature outputs into
	// a output controlled by the wallet.
	// TODO(roasbeef): can be more intelligent about buffering outputs to
	// be more efficient on-chain.
	sweepTx, err := createSweepTx(wallet, kgtnOutputs)
	if err != nil {
		// TODO(roasbeef): retry logic?
		utxnLog.Errorf("unable to create sweep tx: %v", err)
		return err
	}

	utxnLog.Infof("Sweeping %v time-locked outputs "+
		"with sweep tx (txid=%v): %v", len(kgtnOutputs),
		sweepTx.TxHash(),
		newLogClosure(func() string {
			return spew.Sdump(sweepTx)
		}))

	// With the sweep transaction fully signed, broadcast the transaction
	// to the network. Additionally, we can stop tracking these outputs as
	// they've just been swept.
	if err := wallet.PublishTransaction(sweepTx); err != nil {
		utxnLog.Errorf("unable to broadcast sweep tx: %v, %v",
			err, spew.Sdump(sweepTx))
		return err
	}

	return nil
}

// createSweepTx creates a final sweeping transaction with all witnesses in
// place for all inputs. The created transaction has a single output sending
// all the funds back to the source wallet.
func createSweepTx(wallet *lnwallet.LightningWallet,
	matureOutputs []*kidOutput) (*wire.MsgTx, error) {

	pkScript, err := newSweepPkScript(wallet)
	if err != nil {
		return nil, err
	}

	var totalSum btcutil.Amount
	for _, o := range matureOutputs {
		totalSum += o.Amount()
	}

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    int64(totalSum - 5000),
	})
	for _, utxo := range matureOutputs {
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *utxo.OutPoint(),
			// TODO(roasbeef): assumes pure block delays
			Sequence: utxo.BlocksToMaturity(),
		})
	}

	// TODO(roasbeef): insert fee calculation
	//  * remove hardcoded fee above

	// With all the inputs in place, use each output's unique witness
	// function to generate the final witness required for spending.
	hashCache := txscript.NewTxSigHashes(sweepTx)
	for i, txIn := range sweepTx.TxIn {
		witness, err := matureOutputs[i].witnessFunc(sweepTx, hashCache, i)
		if err != nil {
			return nil, err
		}

		txIn.Witness = witness
	}

	return sweepTx, nil
}

// deleteGraduatedOutputs removes outputs from the kindergarten database bucket
// when six blockchain confirmations have passed since the outputs were swept.
// We wait for six confirmations to ensure that the outputs will be swept if a
// chain reorganization occurs. This is the final step in the output incubation
// process.
func deleteGraduatedOutputs(db *channeldb.DB, deleteHeight uint32) error {
	return db.Update(func(tx *bolt.Tx) error {
		kgtnBucket := tx.Bucket(kindergartenBucket)
		if kgtnBucket == nil {
			return nil
		}

		heightBytes := make([]byte, 4)
		byteOrder.PutUint32(heightBytes, deleteHeight)
		results := kgtnBucket.Get(heightBytes)
		if results == nil {
			return nil
		}

		// Delete the row for this height within the kindergarten bucket.k
		if err := kgtnBucket.Delete(heightBytes); err != nil {
			return err
		}

		sweptOutputs, err := deserializeKidList(bytes.NewBuffer(results))
		if err != nil {
			return err
		}
		utxnLog.Infof("Deleting %v swept outputs from kindergarten bucket "+
			"at block height: %v", len(sweptOutputs), deleteHeight)

		// Additionally, for each output that has now been fully swept,
		// we'll also remove the index entry for that output.
		indexBucket := tx.Bucket(contractIndex)
		if indexBucket == nil {
			return nil
		}
		for _, sweptOutput := range sweptOutputs {
			var chanPoint bytes.Buffer
			err := writeOutpoint(&chanPoint, sweptOutput.OriginChanPoint())
			if err != nil {
				return err
			}

			if err := indexBucket.Delete(chanPoint.Bytes()); err != nil {
				return err
			}
		}

		return nil
	})
}

// putLastHeightGraduated persists the most recently processed blockheight
// to the database. This blockheight is used during restarts to determine if
// blocks were missed while the UTXO Nursery was offline.
func putLastHeightGraduated(db *channeldb.DB, blockheight uint32) error {
	return db.Update(func(tx *bolt.Tx) error {
		kgtnBucket, err := tx.CreateBucketIfNotExists(kindergartenBucket)
		if err != nil {
			return nil
		}

		heightBytes := make([]byte, 4)
		byteOrder.PutUint32(heightBytes, blockheight)
		return kgtnBucket.Put(lastGraduatedHeightKey, heightBytes)
	})
}

// newSweepPkScript creates a new public key script which should be used to
// sweep any time-locked, or contested channel funds into the wallet.
// Specifically, the script generated is a version 0,
// pay-to-witness-pubkey-hash (p2wkh) output.
func newSweepPkScript(wallet lnwallet.WalletController) ([]byte, error) {
	sweepAddr, err := wallet.NewAddress(lnwallet.WitnessPubKey, false)
	if err != nil {
		return nil, err
	}

	return txscript.PayToAddrScript(sweepAddr)
}

// deserializedKidList takes a sequence of serialized kid outputs and returns a
// slice of kidOutput structs.
func deserializeKidList(r io.Reader) ([]*kidOutput, error) {
	var kidOutputs []*kidOutput

	for {
		var kid = &kidOutput{}
		if err := kid.Decode(r); err != nil {
			if err == io.EOF {
				break
			} else {
				return nil, err
			}
		}
		kidOutputs = append(kidOutputs, kid)
	}

	return kidOutputs, nil
}

// CsvSpendableOutput is a SpendableOutput that contains all of the information
// necessary to construct, sign, and sweep an output locked with a CSV delay.
type CsvSpendableOutput interface {
	SpendableOutput

	// ConfHeight returns the height at which this output was confirmed.
	// A zero value indicates that the output has not been confirmed.
	ConfHeight() uint32

	// SetConfHeight marks the height at which the output is confirmed in
	// the chain.
	SetConfHeight(height uint32)

	// BlocksToMaturity returns the relative timelock, as a number of
	// blocks, that must be built on top of the confirmation height before
	// the output can be spent.
	BlocksToMaturity() uint32

	// OriginChanPoint returns the outpoint of the channel from which this
	// output is derived.
	OriginChanPoint() *wire.OutPoint
}

// babyOutput is an HTLC output that is in the earliest stage of upbringing.
// Each babyOutput carries a presigned timeout transction, which should be
// broadcast at the appropriate CLTV expiry, and its future kidOutput self. If
// all goes well, and the timeout transaction is successfully confirmed, the
// the now-mature kidOutput will be unwrapped and continue its journey through
// the nursery.
type babyOutput struct {
	kidOutput

	expiry    uint32
	timeoutTx *wire.MsgTx
}

// makeBabyOutput constructs baby output the wraps a future kidOutput. The
// provided sign descriptors and witness types will be used once the output
// reaches the delay and claim stage.
func makeBabyOutput(outpoint, originChanPoint *wire.OutPoint,
	blocksToMaturity uint32, witnessType lnwallet.WitnessType,
	htlcResolution *lnwallet.OutgoingHtlcResolution) babyOutput {

	kid := makeKidOutput(outpoint, originChanPoint,
		blocksToMaturity, witnessType,
		&htlcResolution.SweepSignDesc)

	return babyOutput{
		kidOutput: kid,
		expiry:    htlcResolution.Expiry,
		timeoutTx: htlcResolution.SignedTimeoutTx,
	}
}

// Encode writes the baby output to the given io.Writer.
func (bo *babyOutput) Encode(w io.Writer) error {
	var scratch [4]byte
	byteOrder.PutUint32(scratch[:], bo.expiry)
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := bo.timeoutTx.Serialize(w); err != nil {
		return err
	}

	return bo.kidOutput.Encode(w)
}

// Decode reconstructs a baby output using the provide io.Reader.
func (bo *babyOutput) Decode(r io.Reader) error {
	var scratch [4]byte
	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	bo.expiry = byteOrder.Uint32(scratch[:])

	bo.timeoutTx = new(wire.MsgTx)
	if err := bo.timeoutTx.Deserialize(r); err != nil {
		return err
	}

	return bo.kidOutput.Decode(r)
}

// kidOutput represents an output that's waiting for a required blockheight
// before its funds will be available to be moved into the user's wallet.  The
// struct includes a WitnessGenerator closure which will be used to generate
// the witness required to sweep the output once it's mature.
//
// TODO(roasbeef): rename to immatureOutput?
type kidOutput struct {
	breachedOutput

	originChanPoint wire.OutPoint

	// TODO(roasbeef): using block timeouts everywhere currently, will need
	// to modify logic later to account for MTP based timeouts.
	blocksToMaturity uint32
	confHeight       uint32
}

func makeKidOutput(outpoint, originChanPoint *wire.OutPoint,
	blocksToMaturity uint32, witnessType lnwallet.WitnessType,
	signDescriptor *lnwallet.SignDescriptor) kidOutput {

	return kidOutput{
		breachedOutput: makeBreachedOutput(
			outpoint, witnessType, signDescriptor,
		),
		originChanPoint:  *originChanPoint,
		blocksToMaturity: blocksToMaturity,
	}
}

func (k *kidOutput) OriginChanPoint() *wire.OutPoint {
	return &k.originChanPoint
}

func (k *kidOutput) BlocksToMaturity() uint32 {
	return k.blocksToMaturity
}

func (k *kidOutput) SetConfHeight(height uint32) {
	k.confHeight = height
}

func (k *kidOutput) ConfHeight() uint32 {
	return k.confHeight
}

// Encode converts a KidOutput struct into a form suitable for on-disk database
// storage. Note that the signDescriptor struct field is included so that the
// output's witness can be generated by createSweepTx() when the output becomes
// spendable.
func (k *kidOutput) Encode(w io.Writer) error {
	var scratch [8]byte
	byteOrder.PutUint64(scratch[:], uint64(k.Amount()))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := writeOutpoint(w, k.OutPoint()); err != nil {
		return err
	}
	if err := writeOutpoint(w, k.OriginChanPoint()); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], k.BlocksToMaturity())
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], k.ConfHeight())
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint16(scratch[:2], uint16(k.WitnessType()))
	if _, err := w.Write(scratch[:2]); err != nil {
		return err
	}

	return lnwallet.WriteSignDescriptor(w, k.SignDesc())
}

// Decode takes a byte array representation of a kidOutput and converts it to an
// struct. Note that the witnessFunc method isn't added during deserialization
// and must be added later based on the value of the witnessType field.
func (k *kidOutput) Decode(r io.Reader) error {
	var scratch [8]byte

	if _, err := r.Read(scratch[:]); err != nil {
		return err
	}
	k.amt = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if err := readOutpoint(io.LimitReader(r, 40), &k.outpoint); err != nil {
		return err
	}

	err := readOutpoint(io.LimitReader(r, 40), &k.originChanPoint)
	if err != nil {
		return err
	}

	if _, err := r.Read(scratch[:4]); err != nil {
		return err
	}
	k.blocksToMaturity = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(scratch[:4]); err != nil {
		return err
	}
	k.confHeight = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(scratch[:2]); err != nil {
		return err
	}
	k.witnessType = lnwallet.WitnessType(byteOrder.Uint16(scratch[:2]))

	return lnwallet.ReadSignDescriptor(r, &k.signDesc)
}

// TODO(bvu): copied from channeldb, remove repetition
func writeOutpoint(w io.Writer, o *wire.OutPoint) error {
	// TODO(roasbeef): make all scratch buffers on the stack
	scratch := make([]byte, 4)

	// TODO(roasbeef): write raw 32 bytes instead of wasting the extra
	// byte.
	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch, o.Index)
	_, err := w.Write(scratch)
	return err
}

// TODO(bvu): copied from channeldb, remove repetition
func readOutpoint(r io.Reader, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	txid, err := wire.ReadVarBytes(r, 0, 32, "prevout")
	if err != nil {
		return err
	}
	copy(o.Hash[:], txid)

	if _, err := r.Read(scratch); err != nil {
		return err
	}
	o.Index = byteOrder.Uint32(scratch)

	return nil
}

func writeTxOut(w io.Writer, txo *wire.TxOut) error {
	scratch := make([]byte, 8)

	byteOrder.PutUint64(scratch, uint64(txo.Value))
	if _, err := w.Write(scratch); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, txo.PkScript); err != nil {
		return err
	}

	return nil
}

func readTxOut(r io.Reader, txo *wire.TxOut) error {
	scratch := make([]byte, 8)

	if _, err := r.Read(scratch); err != nil {
		return err
	}
	txo.Value = int64(byteOrder.Uint64(scratch))

	pkScript, err := wire.ReadVarBytes(r, 0, 80, "pkScript")
	if err != nil {
		return err
	}
	txo.PkScript = pkScript

	return nil
}

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/boltdb/bolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/roasbeef/btcd/btcec"
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
	preschoolBucket = []byte("psc")

	// kindergartenBucket stores outputs from commitment transactions that
	// have received an initial confirmation, but which aren't yet
	// spendable because they require additional confirmations enforced by
	// Check Sequence Verify. Once required additional confirmations have
	// been reported, a sweep transaction will be created to move the funds
	// out of these outputs. After a further six confirmations have been
	// reported, the outputs will be deleted from this bucket. The purpose
	// of this additional wait time is to ensure that a block
	// reorganization doesn't result in the sweep transaction getting
	// re-organized out of the chain.
	kindergartenBucket = []byte("kdg")

	// lastGraduatedHeightKey is used to persist the last blockheight that
	// has been checked for graduating outputs. When the nursery is
	// restarted, lastGraduatedHeightKey is used to determine the point
	// from which it's necessary to catch up.
	lastGraduatedHeightKey = []byte("lgh")

	byteOrder = binary.BigEndian
)

// witnessType determines how an output's witness will be generated. The
// default commitmentTimeLock type will generate a witness that will allow
// spending of a time-locked transaction enforced by CheckSequenceVerify.
type witnessType uint16

const (
	commitmentTimeLock witnessType = 0
)

// witnessGenerator represents a function which is able to generate the final
// witness for a particular public key script. This function acts as an
// abstraction layer, hiding the details of the underlying script from the
// utxoNursery.
type witnessGenerator func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
	inputIndex int) ([][]byte, error)

// generateFunc will return the witnessGenerator function that a kidOutput uses
// to generate the witness for a sweep transaction. Currently there is only one
// witnessType but this will be expanded.
func (wt witnessType) generateFunc(signer *lnwallet.Signer,
	descriptor *lnwallet.SignDescriptor) witnessGenerator {

	switch wt {
	case commitmentTimeLock:
		return func(tx *wire.MsgTx, hc *txscript.TxSigHashes,
			inputIndex int) ([][]byte, error) {

			desc := descriptor
			desc.SigHashes = hc
			desc.InputIndex = inputIndex

			return lnwallet.CommitSpendTimeout(*signer, desc, tx)
		}
	}

	return nil
}

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

	if err := u.reloadPreschool(); err != nil {
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
	if err := u.catchUpKindergarten(); err != nil {
		return err
	}

	u.wg.Add(1)
	go u.incubator(newBlockChan)

	return nil
}

// reloadPreschool re-initializes the chain notifier with all of the outputs
// that had been saved to the "preschool" database bucket prior to shutdown.
func (u *utxoNursery) reloadPreschool() error {
	return u.db.View(func(tx *bolt.Tx) error {
		psclBucket := tx.Bucket(preschoolBucket)
		if psclBucket == nil {
			return nil
		}

		return psclBucket.ForEach(func(outputBytes, kidBytes []byte) error {
			psclOutput, err := deserializeKidOutput(bytes.NewBuffer(kidBytes))

			outpoint := psclOutput.outPoint
			sourceTxid := outpoint.Hash

			confChan, err := u.notifier.RegisterConfirmationsNtfn(&sourceTxid, 1)
			if err != nil {
				return err
			}

			utxnLog.Infof("Preschool outpoint %v re-registered for confirmation "+
				"notification.", psclOutput.outPoint)
			go psclOutput.waitForPromotion(u.db, confChan)
			return nil
		})
	})
}

// catchUpKindergarten handles the graduation of kindergarten outputs from
// blocks that were missed while the UTXO Nursery was down or offline.
// graduateMissedBlocks is called during the startup of the UTXO Nursery.
func (u *utxoNursery) catchUpKindergarten() error {
	var lastGraduatedHeight uint32

	// Query the database for the most recently processed block
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

	// Get the most recently mined block
	_, bestHeight, err := u.wallet.ChainIO.GetBestBlock()
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

// kidOutput represents an output that's waiting for a required blockheight
// before its funds will be available to be moved into the user's wallet.  The
// struct includes a witnessGenerator closure which will be used to generate
// the witness required to sweep the output once it's mature.
type kidOutput struct {
	amt      btcutil.Amount
	outPoint wire.OutPoint

	witnessFunc witnessGenerator

	// TODO(roasbeef): using block timeouts everywhere currently, will need
	// to modify logic later to account for MTP based timeouts.
	blocksToMaturity uint32
	confHeight       uint32

	signDescriptor *lnwallet.SignDescriptor
	witnessType    witnessType
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
func (u *utxoNursery) incubateOutputs(closeSummary *lnwallet.ForceCloseSummary) {
	outputAmt := btcutil.Amount(closeSummary.SelfOutputSignDesc.Output.Value)
	selfOutput := &kidOutput{
		amt:              outputAmt,
		outPoint:         closeSummary.SelfOutpoint,
		blocksToMaturity: closeSummary.SelfOutputMaturity,
		signDescriptor:   closeSummary.SelfOutputSignDesc,
		witnessType:      commitmentTimeLock,
	}

	u.requests <- &incubationRequest{
		outputs: []*kidOutput{selfOutput},
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
func (u *utxoNursery) incubator(newBlockChan *chainntnfs.BlockEpochEvent) {
	defer u.wg.Done()

out:
	for {
		select {
		case preschoolRequest := <-u.requests:
			utxnLog.Infof("Incubating %v new outputs",
				len(preschoolRequest.outputs))

			for _, output := range preschoolRequest.outputs {
				sourceTxid := output.outPoint.Hash

				if err := output.enterPreschool(u.db); err != nil {
					utxnLog.Errorf("unable to add kidOutput to preschool: %v, %v ",
						output, err)
					continue
				}

				// Register for a notification that will
				// trigger graduation from preschool to
				// kindergarten when the channel close
				// transaction has been confirmed.
				confChan, err := u.notifier.RegisterConfirmationsNtfn(&sourceTxid, 1)
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

			// A new block has just been connected to the main
			// chain which means we might be able to graduate some
			// outputs out of the kindergarten bucket. Graduation
			// entails successfully sweeping a time-locked output.
			height := uint32(epoch.Height)
			if err := u.graduateKindergarten(height); err != nil {
				utxnLog.Errorf("error while graduating "+
					"kindergarten outputs: %v", err)
			}

		case <-u.quit:
			break out
		}
	}
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

		var outpointBytes bytes.Buffer
		if err := writeOutpoint(&outpointBytes, &k.outPoint); err != nil {
			return err
		}

		var kidBytes bytes.Buffer
		if err := serializeKidOutput(&kidBytes, k); err != nil {
			return err
		}

		if err := psclBucket.Put(outpointBytes.Bytes(), kidBytes.Bytes()); err != nil {
			return err
		}

		utxnLog.Infof("Outpoint %v now in preschool, waiting for "+
			"initial confirmation", k.outPoint)

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
			"closed, can't advance output %v", k.outPoint)
		return
	}

	utxnLog.Infof("Outpoint %v confirmed in block %v moving to kindergarten",
		k.outPoint, txConfirmation.BlockHeight)

	k.confHeight = uint32(txConfirmation.BlockHeight)

	// The following block deletes a kidOutput from the preschool database
	// bucket and adds it to the kindergarten database bucket which is
	// keyed by block height. Keys and values are serialized into byte
	// array form prior to database insertion.
	err := db.Update(func(tx *bolt.Tx) error {
		psclBucket := tx.Bucket(preschoolBucket)
		if psclBucket == nil {
			return errors.New("unable to open preschool bucket")
		}

		var outpointBytes bytes.Buffer
		if err := writeOutpoint(&outpointBytes, &k.outPoint); err != nil {
			return err
		}
		if err := psclBucket.Delete(outpointBytes.Bytes()); err != nil {
			utxnLog.Errorf("unable to delete kindergarten output from "+
				"preschool bucket: %v", k.outPoint)
			return err
		}

		kgtnBucket, err := tx.CreateBucketIfNotExists(kindergartenBucket)
		if err != nil {
			return err
		}

		maturityHeight := k.confHeight + k.blocksToMaturity

		heightBytes := make([]byte, 4)
		byteOrder.PutUint32(heightBytes, uint32(maturityHeight))

		// If there're any existing outputs for this particular block
		// height target, then we'll append this new output to the
		// serialized list of outputs.
		var existingOutputs []byte
		if results := kgtnBucket.Get(heightBytes); results != nil {
			existingOutputs = results
		}

		b := bytes.NewBuffer(existingOutputs)
		if err := serializeKidOutput(b, k); err != nil {
			return err
		}
		if err := kgtnBucket.Put(heightBytes, b.Bytes()); err != nil {
			return err
		}

		utxnLog.Infof("Outpoint %v now in kindergarten, will mature "+
			"at height %v (delay of %v)", k.outPoint,
			maturityHeight, k.blocksToMaturity)
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
		if err := sweepGraduatingOutputs(u.wallet, kgtnOutputs); err != nil {
			return err
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

	// If no time-locked outputs can be sweeped at this point, ten we can
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
		kgtnOutput.witnessFunc = kgtnOutput.witnessType.generateFunc(
			&wallet.Signer, kgtnOutput.signDescriptor,
		)
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
		"with sweep tx: %v", len(kgtnOutputs),
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
		totalSum += o.amt
	}

	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxOut(&wire.TxOut{
		PkScript: pkScript,
		Value:    int64(totalSum - 5000),
	})
	for _, utxo := range matureOutputs {
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: utxo.outPoint,
			// TODO(roasbeef): assumes pure block delays
			Sequence: utxo.blocksToMaturity,
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
	err := db.Update(func(tx *bolt.Tx) error {
		kgtnBucket := tx.Bucket(kindergartenBucket)
		if kgtnBucket == nil {
			return nil
		}

		heightBytes := make([]byte, 4)
		byteOrder.PutUint32(heightBytes, uint32(deleteHeight))
		results := kgtnBucket.Get(heightBytes)
		if results == nil {
			return nil
		}

		sweptOutputs, err := deserializeKidList(bytes.NewBuffer(results))
		if err != nil {
			return err
		}

		if err := kgtnBucket.Delete(heightBytes); err != nil {
			return err
		}

		utxnLog.Infof("Deleting %v swept outputs from kindergarten bucket "+
			"at block height: %v", len(sweptOutputs), deleteHeight)

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// putLastHeightGraduated persists the most recently processed blockheight
// to the database. This blockheight is used during restarts to determine if
// blocks were missed while the UTXO Nursery was offline.
func putLastHeightGraduated(db *channeldb.DB, blockheight uint32) error {
	err := db.Update(func(tx *bolt.Tx) error {
		kgtnBucket, err := tx.CreateBucketIfNotExists(kindergartenBucket)
		if err != nil {
			return nil
		}

		heightBytes := make([]byte, 4)
		byteOrder.PutUint32(heightBytes, blockheight)
		if err := kgtnBucket.Put(lastGraduatedHeightKey, heightBytes); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
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
		kidOutput, err := deserializeKidOutput(r)
		if err != nil {
			if err == io.EOF {
				break
			} else {
				return nil, err
			}
		}
		kidOutputs = append(kidOutputs, kidOutput)
	}

	return kidOutputs, nil
}

// serializeKidOutput converts a KidOutput struct into a form
// suitable for on-disk database storage. Note that the signDescriptor
// struct field is included so that the output's witness can be generated
// by createSweepTx() when the output becomes spendable.
func serializeKidOutput(w io.Writer, kid *kidOutput) error {
	var scratch [8]byte
	byteOrder.PutUint64(scratch[:], uint64(kid.amt))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	if err := writeOutpoint(w, &kid.outPoint); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], kid.blocksToMaturity)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], kid.confHeight)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	byteOrder.PutUint16(scratch[:2], uint16(kid.witnessType))
	if _, err := w.Write(scratch[:2]); err != nil {
		return err
	}

	serializedPubKey := kid.signDescriptor.PubKey.SerializeCompressed()
	if err := wire.WriteVarBytes(w, 0, serializedPubKey); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, kid.signDescriptor.PrivateTweak); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(w, 0, kid.signDescriptor.WitnessScript); err != nil {
		return err
	}

	if err := writeTxOut(w, kid.signDescriptor.Output); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], uint32(kid.signDescriptor.HashType))
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	return nil
}

// deserializeKidOutput takes a byte array representation of a kidOutput
// and converts it to an struct. Note that the witnessFunc method isn't added
// during deserialization and must be added later based on the value of the
// witnessType field.
func deserializeKidOutput(r io.Reader) (*kidOutput, error) {
	scratch := make([]byte, 8)

	kid := &kidOutput{}

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	kid.amt = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if err := readOutpoint(io.LimitReader(r, 40), &kid.outPoint); err != nil {
		return nil, err
	}

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	kid.blocksToMaturity = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	kid.confHeight = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(scratch[:2]); err != nil {
		return nil, err
	}
	kid.witnessType = witnessType(byteOrder.Uint16(scratch[:2]))

	kid.signDescriptor = &lnwallet.SignDescriptor{}

	descKeyBytes, err := wire.ReadVarBytes(r, 0, 34, "descKeyBytes")
	if err != nil {
		return nil, err
	}

	descKey, err := btcec.ParsePubKey(descKeyBytes, btcec.S256())
	if err != nil {
		return nil, err
	}
	kid.signDescriptor.PubKey = descKey

	descPrivateTweak, err := wire.ReadVarBytes(r, 0, 32, "privateTweak")
	if err != nil {
		return nil, err
	}
	kid.signDescriptor.PrivateTweak = descPrivateTweak

	descWitnessScript, err := wire.ReadVarBytes(r, 0, 100, "witnessScript")
	if err != nil {
		return nil, err
	}
	kid.signDescriptor.WitnessScript = descWitnessScript

	descTxOut := &wire.TxOut{}
	if err := readTxOut(r, descTxOut); err != nil {
		return nil, err
	}
	kid.signDescriptor.Output = descTxOut

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	kid.signDescriptor.HashType = txscript.SigHashType(byteOrder.Uint32(scratch[:4]))

	return kid, nil
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
	if _, err := w.Write(scratch); err != nil {
		return err
	}

	return nil
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

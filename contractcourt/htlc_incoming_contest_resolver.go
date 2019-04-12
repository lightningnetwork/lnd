package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// htlcIncomingContestResolver is a ContractResolver that's able to resolve an
// incoming HTLC that is still contested. An HTLC is still contested, if at the
// time of commitment broadcast, we don't know of the preimage for it yet, and
// it hasn't expired. In this case, we can resolve the HTLC if we learn of the
// preimage, otherwise the remote party will sweep it after it expires.
type htlcIncomingContestResolver struct {
	// htlcExpiry is the absolute expiry of this incoming HTLC. We use this
	// value to determine if we can exit early as if the HTLC times out,
	// before we learn of the preimage then we can't claim it on chain
	// successfully.
	htlcExpiry uint32

	// htlcResolution is the incoming HTLC resolution for this HTLC. It
	// contains everything we need to properly resolve this HTLC.
	htlcResolution lnwallet.IncomingHtlcResolution

	// outputIncubating returns true if we've sent the output to the output
	// incubator (utxo nursery).
	outputIncubating bool

	// resolved reflects if the contract has been fully resolved or not.
	resolved bool

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	broadcastHeight uint32

	// payHash is the payment hash of the original HTLC extended to us.
	payHash lntypes.Hash

	// sweepTx will be non-nil if we've already crafted a transaction to
	// sweep a direct HTLC output. This is only a concern if we're sweeping
	// from the commitment transaction of the remote party.
	//
	// TODO(roasbeef): send off to utxobundler
	sweepTx *wire.MsgTx

	// htlcAmt is the original amount of the htlc, not taking into
	// account any fees that may have to be paid if it goes on chain.
	htlcAmt lnwire.MilliSatoshi

	ResolverKit
}

// Resolve attempts to resolve this contract. As we don't yet know of the
// preimage for the contract, we'll wait for one of two things to happen:
//
//   1. We learn of the preimage! In this case, we can sweep the HTLC incoming
//      and ensure that if this was a multi-hop HTLC we are made whole. In this
//      case, an additional ContractResolver will be returned to finish the
//      job.
//
//   2. The HTLC expires. If this happens, then the contract is fully resolved
//      as we have no remaining actions left at our disposal.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) Resolve() (ContractResolver, error) {
	// If we're already full resolved, then we don't have anything further
	// to do.
	if h.resolved {
		return nil, nil
	}

	// Get current block height for checking htlc cltv and csv expiry.
	_, currentHeight, err := h.ChainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// If the HTLC hasn't expired yet, then we may still be able to claim
	// it if we learn of the pre-image, so we'll subscribe to the preimage
	// database to see if it turns up, or the HTLC times out.
	//
	// NOTE: This is done BEFORE opportunistically querying the db, to
	// ensure the preimage can't be delivered between querying and
	// registering for the preimage subscription.
	preimageSubscription := h.PreimageDB.SubscribeUpdates()
	blockEpochs, err := h.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		preimageSubscription.CancelSubscription()
		blockEpochs.Cancel()
	}()

	// Create a buffered hodl chan to prevent deadlock.
	hodlChan := make(chan interface{}, 1)

	// Notify registry that we are potentially settling as exit hop
	// on-chain, so that we will get a hodl event when a corresponding hodl
	// invoice is settled.
	event, err := h.Registry.NotifyExitHopHtlc(h.payHash, h.htlcAmt, hodlChan)
	if err != nil && err != channeldb.ErrInvoiceNotFound {
		return nil, err
	}
	defer h.Registry.HodlUnsubscribeAll(hodlChan)

	// If the htlc can be settled directly, we can progress to the inner
	// resolver immediately.
	if event != nil && event.Preimage != nil {
		return nil, h.resolveSuccess(
			*event.Preimage, currentHeight, blockEpochs,
		)
	}

	// With the epochs and preimage subscriptions initialized, we'll query
	// to see if we already know the preimage.
	preimage, ok := h.PreimageDB.LookupPreimage(h.payHash)
	if ok {
		// If we do, then this means we can claim the HTLC!  However,
		// we don't know how to ourselves, so we'll return our inner
		// resolver which has the knowledge to do so.
		return nil, h.resolveSuccess(
			preimage, currentHeight, blockEpochs,
		)
	}

	checkTimeout := func() bool {
		if currentHeight < int32(h.htlcExpiry) {
			return false
		}

		// TODO(roasbeef): should also somehow check if outgoing is
		// resolved or not
		//  * may need to hook into the circuit map
		//  * can't timeout before the outgoing has been

		log.Infof("%T(%v): HTLC has timed out (expiry=%v, height=%v), "+
			"abandoning", h, h.htlcResolution.ClaimOutpoint,
			h.htlcExpiry, currentHeight)

		return true
	}

	// If we're past the point of expiry of the HTLC, then at this point
	// the sender can sweep it, so we'll end our lifetime.
	if checkTimeout() {
		h.resolved = true
		return nil, h.Checkpoint(h)
	}

	for {

		select {
		case preimage := <-preimageSubscription.WitnessUpdates:
			// Check to see if this preimage matches our htlc.
			if !preimage.Matches(h.payHash) {
				continue
			}

			// We've learned of the preimage and this information
			// has been added to our inner resolver. We return it so
			// it can continue contract resolution.

			return nil, h.resolveSuccess(
				preimage, currentHeight, blockEpochs,
			)

		case hodlItem := <-hodlChan:
			hodlEvent := hodlItem.(invoices.HodlEvent)

			// If hodl invoice is canceled, abandon resolver.
			if hodlEvent.Preimage == nil {
				log.Infof("%T(%v): Invoice has been canceled "+
					"(expiry=%v, height=%v), abandoning", h,
					h.htlcResolution.ClaimOutpoint,
					h.htlcExpiry, currentHeight)
				h.resolved = true
				return nil, h.Checkpoint(h)
			}

			return nil, h.resolveSuccess(
				*hodlEvent.Preimage, currentHeight, blockEpochs,
			)

		case newBlock, ok := <-blockEpochs.Epochs:
			if !ok {
				return nil, fmt.Errorf("quitting")
			}

			currentHeight = int32(newBlock.Height)

			// If this new height expires the HTLC, then this means
			// we never found out the preimage, so we can mark
			// resolved and
			// exit.
			if checkTimeout() {
				h.resolved = true
				return nil, h.Checkpoint(h)
			}

		case <-h.Quit:
			return nil, fmt.Errorf("resolver stopped")
		}
	}
}

// resolveSuccess continues contract resolution from the point the preimage is
// known.
func (h *htlcIncomingContestResolver) resolveSuccess(
	preimage lntypes.Preimage, currentHeight int32,
	blockEpochs *chainntnfs.BlockEpochEvent) error {

	log.Infof("%T(%v): settle htlc on-chain using preimage=%v", h,
		h.htlcResolution.ClaimOutpoint, preimage)

	// If this our commitment transaction, then we'll need to
	// populate the witness for the second-level HTLC transaction.
	if h.htlcResolution.SignedSuccessTx != nil {
		// Within the witness for the success transaction, the
		// preimage is the 4th element as it looks like:
		//
		//  * <sender sig> <recvr sig> <preimage> <witness script>
		//
		// We'll populate it within the witness, as since this
		// was a "contest" resolver, we didn't yet know of the
		// preimage.
		h.htlcResolution.SignedSuccessTx.TxIn[0].Witness[3] = preimage[:]

		return h.resolveLocal(currentHeight, blockEpochs)
	}

	return h.resolveRemote(preimage)
}

// resolveLocal handles the resolution in case we published the commit tx.
func (h *htlcIncomingContestResolver) resolveLocal(currentHeight int32,
	blockEpochs *chainntnfs.BlockEpochEvent) error {

	successTx := h.htlcResolution.SignedSuccessTx

	log.Infof("%T(%x): broadcasting second-layer success tx: %v",
		h, h.payHash[:], spew.Sdump(successTx))

	// We'll now broadcast the second layer transaction so we can kick off
	// the claiming process. In case of a publish error, continue because it
	// may be that we already published the tx in a previous run.
	err := h.PublishTx(successTx)
	if err != nil {
		log.Warnf("%T(%x): cannot publish success tx: %v", h,
			h.payHash[:], err)
	}

	// Wait for confirmation of the success tx.
	log.Infof("%T(%x): waiting for commit output to be spent",
		h, h.payHash[:])

	outpointToWatch, scriptToWatch, err := h.chainDetailsToWatch()
	if err != nil {
		return err
	}
	spendNtfn, err := h.Notifier.RegisterSpendNtfn(
		outpointToWatch, scriptToWatch, h.broadcastHeight,
	)
	if err != nil {
		return err
	}

	var (
		spend *chainntnfs.SpendDetail
		ok    bool
	)
	select {
	case spend, ok = <-spendNtfn.Spend:
		if !ok {
			return fmt.Errorf("quitting")
		}

	case <-h.Quit:
		return fmt.Errorf("quitting")
	}

	successTxHash := successTx.TxHash()
	if *spend.SpenderTxHash != successTxHash {
		log.Infof("%T(%x): commit output spent by remote party tx %v "+
			"at height %v, abandoning", h, h.payHash[:],
			*spend.SpenderTxHash, spend.SpendingHeight)

		h.resolved = true
		return h.Checkpoint(h)
	}

	log.Infof("%T(%x): commit output spent by second-layer "+
		"success tx at height %v", h, h.payHash[:],
		*spend.SpenderTxHash, spend.SpendingHeight)

	// Wait for csv delay
	maturityHeight := spend.SpendingHeight + int32(h.htlcResolution.CsvDelay)

	log.Infof("%T(%x): waiting for success tx output to mature at height "+
		"%v", h, h.payHash[:], *spend.SpenderTxHash, maturityHeight)

	for currentHeight < maturityHeight {
		select {
		case newBlock, ok := <-blockEpochs.Epochs:
			if !ok {
				return fmt.Errorf("quitting")
			}

			currentHeight = int32(newBlock.Height)
		case <-h.Quit:
			return fmt.Errorf("quitting")
		}
	}

	log.Infof("%T(%x): success tx output is mature", h, h.payHash[:])

	inp := input.NewBaseInput(&h.htlcResolution.ClaimOutpoint,
		input.HtlcAcceptedSuccessSecondLevel,
		&h.htlcResolution.SweepSignDesc,
		h.broadcastHeight,
	)

	return h.sweep(inp)
}

func (h *htlcIncomingContestResolver) sweep(inp input.Input) error {
	resultChan, err := h.Sweeper.SweepInput(inp)
	if err != nil {
		return err
	}

	select {
	case _, ok := <-resultChan:
		// TODO: Handle result (remote spend possibility)
		if !ok {
			return fmt.Errorf("quitting")
		}

	case <-h.Quit:
		return fmt.Errorf("quitting")
	}

	// With the HTLC claimed, we can attempt to settle its corresponding
	// invoice if we were the original destination. As the htlc is already
	// settled at this point, we don't need to read on the hodl
	// channel.
	//
	// TODO: Passing MaxInt32 to circumvent the expiry is not the
	// way to do this.
	hodlChan := make(chan interface{}, 1)
	_, err = h.Registry.NotifyExitHopHtlc(
		h.payHash, h.htlcAmt, hodlChan,
	)
	if err != nil && err != channeldb.ErrInvoiceNotFound {
		log.Errorf("Unable to settle invoice with payment "+
			"hash %x: %v", h.payHash, err)
	}

	h.resolved = true
	return h.Checkpoint(h)
}

// resolveLocal handles the resolution in case the remote party published the
// commit tx.
func (h *htlcIncomingContestResolver) resolveRemote(preimage lntypes.Preimage) error {
	inp := input.MakeHtlcSucceedInput(
		&h.htlcResolution.ClaimOutpoint,
		&h.htlcResolution.SweepSignDesc,
		preimage[:],
		h.broadcastHeight,
	)

	return h.sweep(&inp)
}

// report returns a report on the resolution state of the contract.
func (h *htlcIncomingContestResolver) report() *ContractReport {
	// TODO: Report stage 2

	// No locking needed as these values are read-only.
	finalAmt := h.htlcAmt.ToSatoshis()
	if h.htlcResolution.SignedSuccessTx != nil {
		finalAmt = btcutil.Amount(
			h.htlcResolution.SignedSuccessTx.TxOut[0].Value,
		)
	}

	return &ContractReport{
		Outpoint:       h.htlcResolution.ClaimOutpoint,
		Incoming:       true,
		Amount:         finalAmt,
		MaturityHeight: h.htlcExpiry,
		LimboBalance:   finalAmt,
		Stage:          1,
	}
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) Stop() {
	close(h.Quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) IsResolved() bool {
	return h.resolved
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) Encode(w io.Writer) error {
	// We'll first write out the one field unique to this resolver.
	if err := binary.Write(w, endian, h.htlcExpiry); err != nil {
		return err
	}

	// Encode our inner HTLC resolution.
	if err := encodeIncomingResolution(w, &h.htlcResolution); err != nil {
		return err
	}

	// Next, we'll write out the fields that are specified to the contract
	// resolver.
	if err := binary.Write(w, endian, h.outputIncubating); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.resolved); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.broadcastHeight); err != nil {
		return err
	}
	if _, err := w.Write(h.payHash[:]); err != nil {
		return err
	}

	return nil
}

// Decode attempts to decode an encoded ContractResolver from the passed Reader
// instance, returning an active ContractResolver instance.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) Decode(r io.Reader) error {
	// We'll first read the one field unique to this resolver.
	if err := binary.Read(r, endian, &h.htlcExpiry); err != nil {
		return err
	}

	// Decode our inner HTLC resolution.
	if err := decodeIncomingResolution(r, &h.htlcResolution); err != nil {
		return err
	}

	// Next, we'll read all the fields that are specified to the contract
	// resolver.
	if err := binary.Read(r, endian, &h.outputIncubating); err != nil {
		return err
	}
	if err := binary.Read(r, endian, &h.resolved); err != nil {
		return err
	}
	if err := binary.Read(r, endian, &h.broadcastHeight); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, h.payHash[:]); err != nil {
		return err
	}

	return nil
}

// AttachResolverKit should be called once a resolved is successfully decoded
// from its stored format. This struct delivers a generic tool kit that
// resolvers need to complete their duty.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) AttachResolverKit(r ResolverKit) {
	h.ResolverKit = r
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) ResolverKey() []byte {
	// The primary key for this resolver will be the outpoint of the HTLC
	// on the commitment transaction itself. If this is our commitment,
	// then the output can be found within the signed success tx,
	// otherwise, it's just the ClaimOutpoint.
	var op wire.OutPoint
	if h.htlcResolution.SignedSuccessTx != nil {
		op = h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint
	} else {
		op = h.htlcResolution.ClaimOutpoint
	}

	key := newResolverID(op)
	return key[:]
}

// chainDetailsToWatch returns the output and script which we use to watch for
// spends from the direct HTLC output on the commitment transaction.
func (h *htlcIncomingContestResolver) chainDetailsToWatch() (*wire.OutPoint,
	[]byte, error) {

	// If there's no success transaction, then the claim output is the
	// output directly on the commitment transaction, so we'll just use
	// that.
	if h.htlcResolution.SignedSuccessTx == nil {
		outPointToWatch := h.htlcResolution.ClaimOutpoint
		scriptToWatch := h.htlcResolution.SweepSignDesc.Output.PkScript

		return &outPointToWatch, scriptToWatch, nil
	}

	// If this is the remote party's commitment, then we'll need to grab
	// watch the output that our success transaction points to. We can
	// directly grab the outpoint, then also extract the witness script
	// (the last element of the witness stack) to re-construct the pkScript
	// we need to watch.
	outPointToWatch := h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint
	witness := h.htlcResolution.SignedSuccessTx.TxIn[0].Witness
	scriptToWatch, err := input.WitnessScriptHash(witness[len(witness)-1])
	if err != nil {
		return nil, nil, err
	}

	return &outPointToWatch, scriptToWatch, nil
}

// A compile time assertion to ensure htlcIncomingContestResolver meets the
// ContractResolver interface.
var _ ContractResolver = (*htlcIncomingContestResolver)(nil)

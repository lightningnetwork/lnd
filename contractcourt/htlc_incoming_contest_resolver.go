package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sweep"
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

	// We'll first check if this HTLC has been timed out, if so, we can
	// return now and mark ourselves as resolved.
	_, currentHeight, err := h.ChainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// If we're past the point of expiry of the HTLC, then at this point
	// the sender can sweep it, so we'll end our lifetime.
	if uint32(currentHeight) >= h.htlcExpiry {
		// TODO(roasbeef): should also somehow check if outgoing is
		// resolved or not
		//  * may need to hook into the circuit map
		//  * can't timeout before the outgoing has been

		log.Infof("%T(%v): HTLC has timed out (expiry=%v, height=%v), "+
			"abandoning", h, h.htlcResolution.ClaimOutpoint,
			h.htlcExpiry, currentHeight)
		h.resolved = true
		return nil, h.Checkpoint(h)
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
		return nil, h.resolveSuccess(*event.Preimage)
	}

	// With the epochs and preimage subscriptions initialized, we'll query
	// to see if we already know the preimage.
	preimage, ok := h.PreimageDB.LookupPreimage(h.payHash)
	if ok {
		// If we do, then this means we can claim the HTLC!  However,
		// we don't know how to ourselves, so we'll return our inner
		// resolver which has the knowledge to do so.
		return nil, h.resolveSuccess(preimage)
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

			return nil, h.resolveSuccess(preimage)

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

			return nil, h.resolveSuccess(*hodlEvent.Preimage)

		case newBlock, ok := <-blockEpochs.Epochs:
			if !ok {
				return nil, fmt.Errorf("quitting")
			}

			// If this new height expires the HTLC, then this means
			// we never found out the preimage, so we can mark
			// resolved and
			// exit.
			newHeight := uint32(newBlock.Height)
			if newHeight >= h.htlcExpiry {
				log.Infof("%T(%v): HTLC has timed out "+
					"(expiry=%v, height=%v), abandoning", h,
					h.htlcResolution.ClaimOutpoint,
					h.htlcExpiry, currentHeight)
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
	preimage lntypes.Preimage) error {

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

		return h.resolveLocal()
	}

	return h.resolveRemote(preimage)
}

// resolveLocal handles the resolution in case we published the commit tx.
func (h *htlcIncomingContestResolver) resolveLocal() error {
	log.Infof("%T(%x): broadcasting second-layer transition tx: %v",
		h, h.payHash[:], spew.Sdump(h.htlcResolution.SignedSuccessTx))

	// We'll now broadcast the second layer transaction so we can kick off
	// the claiming process.
	//
	// TODO(roasbeef): after changing sighashes send to tx bundler
	err := h.PublishTx(h.htlcResolution.SignedSuccessTx)
	if err != nil {
		return err
	}

	// Otherwise, this is an output on our commitment transaction. In this
	// case, we'll send it to the incubator, but only if we haven't already
	// done so.
	if !h.outputIncubating {
		log.Infof("%T(%x): incubating incoming htlc output",
			h, h.payHash[:])

		err := h.IncubateOutputs(
			h.ChanPoint, nil, nil, &h.htlcResolution,
			h.broadcastHeight,
		)
		if err != nil {
			return err
		}

		h.outputIncubating = true

		if err := h.Checkpoint(h); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return err
		}
	}

	// To wrap this up, we'll wait until the second-level transaction has
	// been spent, then fully resolve the contract.
	spendNtfn, err := h.Notifier.RegisterSpendNtfn(
		&h.htlcResolution.ClaimOutpoint,
		h.htlcResolution.SweepSignDesc.Output.PkScript,
		h.broadcastHeight,
	)
	if err != nil {
		return err
	}

	log.Infof("%T(%x): waiting for second-level HTLC output to be spent "+
		"after csv_delay=%v", h, h.payHash[:], h.htlcResolution.CsvDelay)

	select {
	case _, ok := <-spendNtfn.Spend:
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
	// If we don't already have the sweep transaction constructed,
	// we'll do so and broadcast it.
	if h.sweepTx == nil {
		log.Infof("%T(%x): crafting sweep tx for "+
			"incoming+remote htlc confirmed", h,
			h.payHash[:])

		// Before we can craft out sweeping transaction, we
		// need to create an input which contains all the items
		// required to add this input to a sweeping transaction,
		// and generate a witness.
		inp := input.MakeHtlcSucceedInput(
			&h.htlcResolution.ClaimOutpoint,
			&h.htlcResolution.SweepSignDesc,
			preimage[:],
			h.broadcastHeight,
		)

		// With the input created, we can now generate the full
		// sweep transaction, that we'll use to move these
		// coins back into the backing wallet.
		//
		// TODO: Set tx lock time to current block height
		// instead of zero. Will be taken care of once sweeper
		// implementation is complete.
		//
		// TODO: Use time-based sweeper and result chan.
		var err error
		h.sweepTx, err = h.Sweeper.CreateSweepTx(
			[]input.Input{&inp},
			sweep.FeePreference{
				ConfTarget: sweepConfTarget,
			}, 0,
		)
		if err != nil {
			return err
		}

		log.Infof("%T(%x): crafted sweep tx=%v", h,
			h.payHash[:], spew.Sdump(h.sweepTx))

		// With the sweep transaction signed, we'll now
		// Checkpoint our state.
		if err := h.Checkpoint(h); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return err
		}
	}

	// Regardless of whether an existing transaction was found or newly
	// constructed, we'll broadcast the sweep transaction to the
	// network.
	err := h.PublishTx(h.sweepTx)
	if err != nil {
		log.Infof("%T(%x): unable to publish tx: %v",
			h, h.payHash[:], err)
		return err
	}

	// With the sweep transaction broadcast, we'll wait for its
	// confirmation.
	sweepTXID := h.sweepTx.TxHash()
	sweepScript := h.sweepTx.TxOut[0].PkScript
	confNtfn, err := h.Notifier.RegisterConfirmationsNtfn(
		&sweepTXID, sweepScript, 1, h.broadcastHeight,
	)
	if err != nil {
		return err
	}

	log.Infof("%T(%x): waiting for sweep tx (txid=%v) to be "+
		"confirmed", h, h.payHash[:], sweepTXID)

	select {
	case _, ok := <-confNtfn.Confirmed:
		if !ok {
			return fmt.Errorf("quitting")
		}

	case <-h.Quit:
		return fmt.Errorf("quitting")
	}

	// With the HTLC claimed, we can attempt to settle its
	// corresponding invoice if we were the original destination. As
	// the htlc is already settled at this point, we don't need to
	// read on the hodl channel.
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

	// Once the transaction has received a sufficient number of
	// confirmations, we'll mark ourselves as fully resolved and exit.
	h.resolved = true
	return h.Checkpoint(h)
}

// report returns a report on the resolution state of the contract.
func (h *htlcIncomingContestResolver) report() *ContractReport {
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

// A compile time assertion to ensure htlcIncomingContestResolver meets the
// ContractResolver interface.
var _ ContractResolver = (*htlcIncomingContestResolver)(nil)

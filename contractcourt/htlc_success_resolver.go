package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sweep"
)

// htlcSuccessResolver is a resolver that's capable of sweeping an incoming
// HTLC output on-chain. If this is the remote party's commitment, we'll sweep
// it directly from the commitment output *immediately*. If this is our
// commitment, we'll first broadcast the success transaction, then send it to
// the incubator for sweeping. That's it, no need to send any clean up
// messages.
//
// TODO(roasbeef): don't need to broadcast?
type htlcSuccessResolver struct {
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
	payHash [32]byte

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

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) ResolverKey() []byte {
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

// Resolve attempts to resolve an unresolved incoming HTLC that we know the
// preimage to. If the HTLC is on the commitment of the remote party, then
// we'll simply sweep it directly. Otherwise, we'll hand this off to the utxo
// nursery to do its duty.
//
// TODO(roasbeef): create multi to batch
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if h.resolved {
		return nil, nil
	}

	// If we don't have a success transaction, then this means that this is
	// an output on the remote party's commitment transaction.
	if h.htlcResolution.SignedSuccessTx == nil {
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
				h.htlcResolution.Preimage[:],
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
				return nil, err
			}

			log.Infof("%T(%x): crafted sweep tx=%v", h,
				h.payHash[:], spew.Sdump(h.sweepTx))

			// With the sweep transaction signed, we'll now
			// Checkpoint our state.
			if err := h.Checkpoint(h); err != nil {
				log.Errorf("unable to Checkpoint: %v", err)
				return nil, err
			}
		}

		// Regardless of whether an existing transaction was found or newly
		// constructed, we'll broadcast the sweep transaction to the
		// network.
		err := h.PublishTx(h.sweepTx)
		if err != nil && err != lnwallet.ErrDoubleSpend {
			log.Infof("%T(%x): unable to publish tx: %v",
				h, h.payHash[:], err)
			return nil, err
		}

		// With the sweep transaction broadcast, we'll wait for its
		// confirmation.
		sweepTXID := h.sweepTx.TxHash()
		sweepScript := h.sweepTx.TxOut[0].PkScript
		confNtfn, err := h.Notifier.RegisterConfirmationsNtfn(
			&sweepTXID, sweepScript, 1, h.broadcastHeight,
		)
		if err != nil {
			return nil, err
		}

		log.Infof("%T(%x): waiting for sweep tx (txid=%v) to be "+
			"confirmed", h, h.payHash[:], sweepTXID)

		select {
		case _, ok := <-confNtfn.Confirmed:
			if !ok {
				return nil, fmt.Errorf("quitting")
			}

		case <-h.Quit:
			return nil, fmt.Errorf("quitting")
		}

		// With the HTLC claimed, we can attempt to settle its
		// corresponding invoice if we were the original destination.
		err = h.Registry.SettleInvoice(h.payHash, h.htlcAmt)
		if err != nil && err != channeldb.ErrInvoiceNotFound {
			log.Errorf("Unable to settle invoice with payment "+
				"hash %x: %v", h.payHash, err)
		}

		// Once the transaction has received a sufficient number of
		// confirmations, we'll mark ourselves as fully resolved and exit.
		h.resolved = true
		return nil, h.Checkpoint(h)
	}

	log.Infof("%T(%x): broadcasting second-layer transition tx: %v",
		h, h.payHash[:], spew.Sdump(h.htlcResolution.SignedSuccessTx))

	// We'll now broadcast the second layer transaction so we can kick off
	// the claiming process.
	//
	// TODO(roasbeef): after changing sighashes send to tx bundler
	err := h.PublishTx(h.htlcResolution.SignedSuccessTx)
	if err != nil && err != lnwallet.ErrDoubleSpend {
		return nil, err
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
			return nil, err
		}

		h.outputIncubating = true

		if err := h.Checkpoint(h); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return nil, err
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
		return nil, err
	}

	log.Infof("%T(%x): waiting for second-level HTLC output to be spent "+
		"after csv_delay=%v", h, h.payHash[:], h.htlcResolution.CsvDelay)

	select {
	case _, ok := <-spendNtfn.Spend:
		if !ok {
			return nil, fmt.Errorf("quitting")
		}

	case <-h.Quit:
		return nil, fmt.Errorf("quitting")
	}

	// With the HTLC claimed, we can attempt to settle its corresponding
	// invoice if we were the original destination.
	err = h.Registry.SettleInvoice(h.payHash, h.htlcAmt)
	if err != nil && err != channeldb.ErrInvoiceNotFound {
		log.Errorf("Unable to settle invoice with payment "+
			"hash %x: %v", h.payHash, err)
	}

	h.resolved = true
	return nil, h.Checkpoint(h)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Stop() {
	close(h.Quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) IsResolved() bool {
	return h.resolved
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Encode(w io.Writer) error {
	// First we'll encode our inner HTLC resolution.
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
func (h *htlcSuccessResolver) Decode(r io.Reader) error {
	// First we'll decode our inner HTLC resolution.
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
func (h *htlcSuccessResolver) AttachResolverKit(r ResolverKit) {
	h.ResolverKit = r
}

// A compile time assertion to ensure htlcSuccessResolver meets the
// ContractResolver interface.
var _ ContractResolver = (*htlcSuccessResolver)(nil)

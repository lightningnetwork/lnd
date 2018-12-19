package contractcourt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/lightningnetwork/lnd/sweep"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	endian = binary.BigEndian
)

const (
	// sweepConfTarget is the default number of blocks that we'll use as a
	// confirmation target when sweeping.
	sweepConfTarget = 6
)

// ContractResolver is an interface which packages a state machine which is
// able to carry out the necessary steps required to fully resolve a Bitcoin
// contract on-chain. Resolvers are fully encodable to ensure callers are able
// to persist them properly. A resolver may produce another resolver in the
// case that claiming an HTLC is a multi-stage process. In this case, we may
// partially resolve the contract, then persist, and set up for an additional
// resolution.
type ContractResolver interface {
	// ResolverKey returns an identifier which should be globally unique
	// for this particular resolver within the chain the original contract
	// resides within.
	ResolverKey() []byte

	// Resolve instructs the contract resolver to resolve the output
	// on-chain. Once the output has been *fully* resolved, the function
	// should return immediately with a nil ContractResolver value for the
	// first return value.  In the case that the contract requires further
	// resolution, then another resolve is returned.
	//
	// NOTE: This function MUST be run as a goroutine.
	Resolve() (ContractResolver, error)

	// IsResolved returns true if the stored state in the resolve is fully
	// resolved. In this case the target output can be forgotten.
	IsResolved() bool

	// Encode writes an encoded version of the ContractResolver into the
	// passed Writer.
	Encode(w io.Writer) error

	// Decode attempts to decode an encoded ContractResolver from the
	// passed Reader instance, returning an active ContractResolver
	// instance.
	Decode(r io.Reader) error

	// AttachResolverKit should be called once a resolved is successfully
	// decoded from its stored format. This struct delivers a generic tool
	// kit that resolvers need to complete their duty.
	AttachResolverKit(ResolverKit)

	// Stop signals the resolver to cancel any current resolution
	// processes, and suspend.
	Stop()
}

// ResolverKit is meant to be used as a mix-in struct to be embedded within a
// given ContractResolver implementation. It contains all the items that a
// resolver requires to carry out its duties.
type ResolverKit struct {
	// ChannelArbitratorConfig contains all the interfaces and closures
	// required for the resolver to interact with outside sub-systems.
	ChannelArbitratorConfig

	// Checkpoint allows a resolver to check point its state. This function
	// should write the state of the resolver to persistent storage, and
	// return a non-nil error upon success.
	Checkpoint func(ContractResolver) error

	Quit chan struct{}
}

// htlcTimeoutResolver is a ContractResolver that's capable of resolving an
// outgoing HTLC. The HTLC may be on our commitment transaction, or on the
// commitment transaction of the remote party. An output on our commitment
// transaction is considered fully resolved once the second-level transaction
// has been confirmed (and reached a sufficient depth). An output on the
// commitment transaction of the remote party is resolved once we detect a
// spend of the direct HTLC output using the timeout clause.
type htlcTimeoutResolver struct {
	// htlcResolution contains all the information required to properly
	// resolve this outgoing HTLC.
	htlcResolution lnwallet.OutgoingHtlcResolution

	// outputIncubating returns true if we've sent the output to the output
	// incubator (utxo nursery).
	outputIncubating bool

	// resolved reflects if the contract has been fully resolved or not.
	resolved bool

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	//
	// TODO(roasbeef): wrap above into definite resolution embedding?
	broadcastHeight uint32

	// htlcIndex is the index of this HTLC within the trace of the
	// additional commitment state machine.
	htlcIndex uint64

	ResolverKit
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) ResolverKey() []byte {
	// The primary key for this resolver will be the outpoint of the HTLC
	// on the commitment transaction itself. If this is our commitment,
	// then the output can be found within the signed timeout tx,
	// otherwise, it's just the ClaimOutpoint.
	var op wire.OutPoint
	if h.htlcResolution.SignedTimeoutTx != nil {
		op = h.htlcResolution.SignedTimeoutTx.TxIn[0].PreviousOutPoint
	} else {
		op = h.htlcResolution.ClaimOutpoint
	}

	key := newResolverID(op)
	return key[:]
}

// Resolve kicks off full resolution of an outgoing HTLC output. If it's our
// commitment, it isn't resolved until we see the second level HTLC txn
// confirmed. If it's the remote party's commitment, we don't resolve until we
// see a direct sweep via the timeout clause.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if h.resolved {
		return nil, nil
	}

	// If we haven't already sent the output to the utxo nursery, then
	// we'll do so now.
	if !h.outputIncubating {
		log.Tracef("%T(%v): incubating htlc output", h,
			h.htlcResolution.ClaimOutpoint)

		err := h.IncubateOutputs(
			h.ChanPoint, nil, &h.htlcResolution, nil,
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

	// waitForOutputResolution waits for the HTLC output to be fully
	// resolved. The output is considered fully resolved once it has been
	// spent, and the spending transaction has been fully confirmed.
	waitForOutputResolution := func() error {
		// We first need to register to see when the HTLC output itself
		// has been spent by a confirmed transaction.
		spendNtfn, err := h.Notifier.RegisterSpendNtfn(
			&h.htlcResolution.ClaimOutpoint,
			h.htlcResolution.SweepSignDesc.Output.PkScript,
			h.broadcastHeight,
		)
		if err != nil {
			return err
		}

		select {
		case _, ok := <-spendNtfn.Spend:
			if !ok {
				return fmt.Errorf("notifier quit")
			}

		case <-h.Quit:
			return fmt.Errorf("quitting")
		}

		return nil
	}

	// With the output sent to the nursery, we'll now wait until the output
	// has been fully resolved before sending the clean up message.
	//
	// TODO(roasbeef): need to be able to cancel nursery?
	//  * if they pull on-chain while we're waiting

	// If we don't have a second layer transaction, then this is a remote
	// party's commitment, so we'll watch for a direct spend.
	if h.htlcResolution.SignedTimeoutTx == nil {
		// We'll block until: the HTLC output has been spent, and the
		// transaction spending that output is sufficiently confirmed.
		log.Infof("%T(%v): waiting for nursery to spend CLTV-locked "+
			"output", h, h.htlcResolution.ClaimOutpoint)
		if err := waitForOutputResolution(); err != nil {
			return nil, err
		}
	} else {
		// Otherwise, this is our commitment, so we'll watch for the
		// second-level transaction to be sufficiently confirmed.
		secondLevelTXID := h.htlcResolution.SignedTimeoutTx.TxHash()
		sweepScript := h.htlcResolution.SignedTimeoutTx.TxOut[0].PkScript
		confNtfn, err := h.Notifier.RegisterConfirmationsNtfn(
			&secondLevelTXID, sweepScript, 1, h.broadcastHeight,
		)
		if err != nil {
			return nil, err
		}

		log.Infof("%T(%v): waiting second-level tx (txid=%v) to be "+
			"fully confirmed", h, h.htlcResolution.ClaimOutpoint,
			secondLevelTXID)

		select {
		case _, ok := <-confNtfn.Confirmed:
			if !ok {
				return nil, fmt.Errorf("quitting")
			}

		case <-h.Quit:
			return nil, fmt.Errorf("quitting")
		}
	}

	// TODO(roasbeef): need to watch for remote party sweeping with pre-image?
	//  * have another waiting on spend above, will check the type, if it's
	//    pre-image, then we'll cancel, and send a clean up back with
	//    pre-image, also add to preimage cache

	log.Infof("%T(%v): resolving htlc with incoming fail msg, fully "+
		"confirmed", h, h.htlcResolution.ClaimOutpoint)

	// At this point, the second-level transaction is sufficiently
	// confirmed, or a transaction directly spending the output is.
	// Therefore, we can now send back our clean up message.
	failureMsg := &lnwire.FailPermanentChannelFailure{}
	if err := h.DeliverResolutionMsg(ResolutionMsg{
		SourceChan: h.ShortChanID,
		HtlcIndex:  h.htlcIndex,
		Failure:    failureMsg,
	}); err != nil {
		return nil, err
	}

	// Finally, if this was an output on our commitment transaction, we'll
	// for the second-level HTLC output to be spent, and for that
	// transaction itself to confirm.
	if h.htlcResolution.SignedTimeoutTx != nil {
		log.Infof("%T(%v): waiting for nursery to spend CSV delayed "+
			"output", h, h.htlcResolution.ClaimOutpoint)
		if err := waitForOutputResolution(); err != nil {
			return nil, err
		}
	}

	// With the clean up message sent, we'll now mark the contract
	// resolved, and wait.
	h.resolved = true
	return nil, h.Checkpoint(h)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Stop() {
	close(h.Quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) IsResolved() bool {
	return h.resolved
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Encode(w io.Writer) error {
	// First, we'll write out the relevant fields of the
	// OutgoingHtlcResolution to the writer.
	if err := encodeOutgoingResolution(w, &h.htlcResolution); err != nil {
		return err
	}

	// With that portion written, we can now write out the fields specific
	// to the resolver itself.
	if err := binary.Write(w, endian, h.outputIncubating); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.resolved); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.broadcastHeight); err != nil {
		return err
	}

	if err := binary.Write(w, endian, h.htlcIndex); err != nil {
		return err
	}

	return nil
}

// Decode attempts to decode an encoded ContractResolver from the passed Reader
// instance, returning an active ContractResolver instance.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Decode(r io.Reader) error {
	// First, we'll read out all the mandatory fields of the
	// OutgoingHtlcResolution that we store.
	if err := decodeOutgoingResolution(r, &h.htlcResolution); err != nil {
		return err
	}

	// With those fields read, we can now read back the fields that are
	// specific to the resolver itself.
	if err := binary.Read(r, endian, &h.outputIncubating); err != nil {
		return err
	}
	if err := binary.Read(r, endian, &h.resolved); err != nil {
		return err
	}
	if err := binary.Read(r, endian, &h.broadcastHeight); err != nil {
		return err
	}

	if err := binary.Read(r, endian, &h.htlcIndex); err != nil {
		return err
	}

	return nil
}

// AttachResolverKit should be called once a resolved is successfully decoded
// from its stored format. This struct delivers a generic tool kit that
// resolvers need to complete their duty.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) AttachResolverKit(r ResolverKit) {
	h.ResolverKit = r
}

// A compile time assertion to ensure htlcTimeoutResolver meets the
// ContractResolver interface.
var _ ContractResolver = (*htlcTimeoutResolver)(nil)

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
			input := sweep.MakeHtlcSucceedInput(
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
				[]sweep.Input{&input}, sweepConfTarget, 0,
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

// htlcOutgoingContestResolver is a ContractResolver that's able to resolve an
// outgoing HTLC that is still contested. An HTLC is still contested, if at the
// time that we broadcast the commitment transaction, it isn't able to be fully
// resolved. In this case, we'll either wait for the HTLC to timeout, or for
// us to learn of the preimage.
type htlcOutgoingContestResolver struct {
	// htlcTimeoutResolver is the inner solver that this resolver may turn
	// into. This only happens if the HTLC expires on-chain.
	htlcTimeoutResolver
}

// Resolve commences the resolution of this contract. As this contract hasn't
// yet timed out, we'll wait for one of two things to happen
//
//   1. The HTLC expires. In this case, we'll sweep the funds and send a clean
//      up cancel message to outside sub-systems.
//
//   2. The remote party sweeps this HTLC on-chain, in which case we'll add the
//      pre-image to our global cache, then send a clean up settle message
//      backwards.
//
// When either of these two things happens, we'll create a new resolver which
// is able to handle the final resolution of the contract. We're only the pivot
// point.
func (h *htlcOutgoingContestResolver) Resolve() (ContractResolver, error) {
	// If we're already full resolved, then we don't have anything further
	// to do.
	if h.resolved {
		return nil, nil
	}

	// claimCleanUp is a helper function that's called once the HTLC output
	// is spent by the remote party. It'll extract the preimage, add it to
	// the global cache, and finally send the appropriate clean up message.
	claimCleanUp := func(commitSpend *chainntnfs.SpendDetail) (ContractResolver, error) {
		// Depending on if this is our commitment or not, then we'll be
		// looking for a different witness pattern.
		spenderIndex := commitSpend.SpenderInputIndex
		spendingInput := commitSpend.SpendingTx.TxIn[spenderIndex]

		log.Infof("%T(%v): extracting preimage! remote party spent "+
			"HTLC with tx=%v", h, h.htlcResolution.ClaimOutpoint,
			spew.Sdump(commitSpend.SpendingTx))

		// If this is the remote party's commitment, then we'll be
		// looking for them to spend using the second-level success
		// transaction.
		var preimage [32]byte
		if h.htlcResolution.SignedTimeoutTx == nil {
			// The witness stack when the remote party sweeps the
			// output to them looks like:
			//
			//  * <sender sig> <recvr sig> <preimage> <witness script>
			copy(preimage[:], spendingInput.Witness[3])
		} else {
			// Otherwise, they'll be spending directly from our
			// commitment output. In which case the witness stack
			// looks like:
			//
			//  * <sig> <preimage> <witness script>
			copy(preimage[:], spendingInput.Witness[1])
		}

		log.Infof("%T(%v): extracting preimage=%x from on-chain "+
			"spend!", h, h.htlcResolution.ClaimOutpoint, preimage[:])

		// With the preimage obtained, we can now add it to the global
		// cache.
		if err := h.PreimageDB.AddPreimage(preimage[:]); err != nil {
			log.Errorf("%T(%v): unable to add witness to cache",
				h, h.htlcResolution.ClaimOutpoint)
		}

		// Finally, we'll send the clean up message, mark ourselves as
		// resolved, then exit.
		if err := h.DeliverResolutionMsg(ResolutionMsg{
			SourceChan: h.ShortChanID,
			HtlcIndex:  h.htlcIndex,
			PreImage:   &preimage,
		}); err != nil {
			return nil, err
		}
		h.resolved = true
		return nil, h.Checkpoint(h)
	}

	// Otherwise, we'll watch for two external signals to decide if we'll
	// morph into another resolver, or fully resolve the contract.

	// The output we'll be watching for is the *direct* spend from the HTLC
	// output. If this isn't our commitment transaction, it'll be right on
	// the resolution. Otherwise, we fetch this pointer from the input of
	// the time out transaction.
	var (
		outPointToWatch wire.OutPoint
		scriptToWatch   []byte
		err             error
	)
	if h.htlcResolution.SignedTimeoutTx == nil {
		outPointToWatch = h.htlcResolution.ClaimOutpoint
		scriptToWatch = h.htlcResolution.SweepSignDesc.Output.PkScript
	} else {
		// If this is the remote party's commitment, then we'll need to
		// grab watch the output that our timeout transaction points
		// to. We can directly grab the outpoint, then also extract the
		// witness script (the last element of the witness stack) to
		// re-construct the pkScipt we need to watch.
		outPointToWatch = h.htlcResolution.SignedTimeoutTx.TxIn[0].PreviousOutPoint
		witness := h.htlcResolution.SignedTimeoutTx.TxIn[0].Witness
		scriptToWatch, err = lnwallet.WitnessScriptHash(
			witness[len(witness)-1],
		)
		if err != nil {
			return nil, err
		}
	}

	// First, we'll register for a spend notification for this output. If
	// the remote party sweeps with the pre-image, we'll be notified.
	spendNtfn, err := h.Notifier.RegisterSpendNtfn(
		&outPointToWatch, scriptToWatch, h.broadcastHeight,
	)
	if err != nil {
		return nil, err
	}

	// We'll quickly check to see if the output has already been spent.
	select {
	// If the output has already been spent, then we can stop early and
	// sweep the pre-image from the output.
	case commitSpend, ok := <-spendNtfn.Spend:
		if !ok {
			return nil, fmt.Errorf("quitting")
		}

		// TODO(roasbeef): Checkpoint?
		return claimCleanUp(commitSpend)

	// If it hasn't, then we'll watch for both the expiration, and the
	// sweeping out this output.
	default:
	}

	// We'll check the current height, if the HTLC has already expired,
	// then we'll morph immediately into a resolver that can sweep the
	// HTLC.
	//
	// TODO(roasbeef): use grace period instead?
	_, currentHeight, err := h.ChainIO.GetBestBlock()
	if err != nil {
		return nil, err
	}

	// If the current height is >= expiry-1, then a spend will be valid to
	// be included in the next block, and we can immediately return the
	// resolver.
	//
	// TODO(joostjager): Statement above may not be valid. For CLTV locks,
	// the expiry value is the last _invalid_ block. The likely reason that
	// this does not create a problem, is that utxonursery is checking the
	// expiry again (in the proper way). Same holds for minus one operation
	// below.
	//
	// Source:
	// https://github.com/btcsuite/btcd/blob/991d32e72fe84d5fbf9c47cd604d793a0cd3a072/blockchain/validate.go#L154

	if uint32(currentHeight) >= h.htlcResolution.Expiry-1 {
		log.Infof("%T(%v): HTLC has expired (height=%v, expiry=%v), "+
			"transforming into timeout resolver", h,
			h.htlcResolution.ClaimOutpoint, currentHeight,
			h.htlcResolution.Expiry)
		return &h.htlcTimeoutResolver, nil
	}

	// If we reach this point, then we can't fully act yet, so we'll await
	// either of our signals triggering: the HTLC expires, or we learn of
	// the preimage.
	blockEpochs, err := h.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return nil, err
	}
	defer blockEpochs.Cancel()

	for {
		select {

		// A new block has arrived, we'll check to see if this leads to
		// HTLC expiration.
		case newBlock, ok := <-blockEpochs.Epochs:
			if !ok {
				return nil, fmt.Errorf("quitting")
			}

			// If this new height expires the HTLC, then we can
			// exit early and create a resolver that's capable of
			// handling the time locked output.
			newHeight := uint32(newBlock.Height)
			if newHeight >= h.htlcResolution.Expiry-1 {
				log.Infof("%T(%v): HTLC has expired "+
					"(height=%v, expiry=%v), transforming "+
					"into timeout resolver", h,
					h.htlcResolution.ClaimOutpoint,
					newHeight, h.htlcResolution.Expiry)
				return &h.htlcTimeoutResolver, nil
			}

		// The output has been spent! This means the preimage has been
		// revealed on-chain.
		case commitSpend, ok := <-spendNtfn.Spend:
			if !ok {
				return nil, fmt.Errorf("quitting")
			}

			// The only way this output can be spent by the remote
			// party is by revealing the preimage. So we'll perform
			// our duties to clean up the contract once it has been
			// claimed.
			return claimCleanUp(commitSpend)

		case <-h.Quit:
			return nil, fmt.Errorf("resolver cancelled")
		}
	}
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcOutgoingContestResolver) Stop() {
	close(h.Quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcOutgoingContestResolver) IsResolved() bool {
	return h.resolved
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcOutgoingContestResolver) Encode(w io.Writer) error {
	return h.htlcTimeoutResolver.Encode(w)
}

// Decode attempts to decode an encoded ContractResolver from the passed Reader
// instance, returning an active ContractResolver instance.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcOutgoingContestResolver) Decode(r io.Reader) error {
	return h.htlcTimeoutResolver.Decode(r)
}

// AttachResolverKit should be called once a resolved is successfully decoded
// from its stored format. This struct delivers a generic tool kit that
// resolvers need to complete their duty.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcOutgoingContestResolver) AttachResolverKit(r ResolverKit) {
	h.ResolverKit = r
}

// A compile time assertion to ensure htlcOutgoingContestResolver meets the
// ContractResolver interface.
var _ ContractResolver = (*htlcOutgoingContestResolver)(nil)

// htlcIncomingContestResolver is a ContractResolver that's able to resolve an
// incoming HTLC that is still contested. An HTLC is still contested, if at the
// time of commitment broadcast, we don't know of the preimage for it yet, and
// it hasn't expired. In this case, we can resolve the HTLC if we learn of the
// preimage, otherwise the remote party will sweep it after it expires.
//
// TODO(roasbeef): just embed the other resolver?
type htlcIncomingContestResolver struct {
	// htlcExpiry is the absolute expiry of this incoming HTLC. We use this
	// value to determine if we can exit early as if the HTLC times out,
	// before we learn of the preimage then we can't claim it on chain
	// successfully.
	htlcExpiry uint32

	// htlcSuccessResolver is the inner resolver that may be utilized if we
	// learn of the preimage.
	htlcSuccessResolver
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

	// applyPreimage is a helper function that will populate our internal
	// resolver with the preimage we learn of. This should be called once
	// the preimage is revealed so the inner resolver can properly complete
	// its duties.
	applyPreimage := func(preimage []byte) {
		copy(h.htlcResolution.Preimage[:], preimage)

		log.Infof("%T(%v): extracted preimage=%x from beacon!", h,
			h.htlcResolution.ClaimOutpoint, preimage[:])

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
		}

		copy(h.htlcResolution.Preimage[:], preimage[:])
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

	// With the epochs and preimage subscriptions initialized, we'll query
	// to see if we already know the preimage.
	preimage, ok := h.PreimageDB.LookupPreimage(h.payHash[:])
	if ok {
		// If we do, then this means we can claim the HTLC!  However,
		// we don't know how to ourselves, so we'll return our inner
		// resolver which has the knowledge to do so.
		applyPreimage(preimage[:])
		return &h.htlcSuccessResolver, nil
	}

	for {

		select {
		case preimage := <-preimageSubscription.WitnessUpdates:
			// If this isn't our preimage, then we'll continue
			// onwards.
			newHash := sha256.Sum256(preimage)
			preimageMatches := bytes.Equal(newHash[:], h.payHash[:])
			if !preimageMatches {
				continue
			}

			// Otherwise, we've learned of the preimage! We'll add
			// this information to our inner resolver, then return
			// it so it can continue contract resolution.
			applyPreimage(preimage)
			return &h.htlcSuccessResolver, nil

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

	// Then we'll write out our internal resolver.
	return h.htlcSuccessResolver.Encode(w)
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

	// Then we'll decode our internal resolver.
	return h.htlcSuccessResolver.Decode(r)
}

// AttachResolverKit should be called once a resolved is successfully decoded
// from its stored format. This struct delivers a generic tool kit that
// resolvers need to complete their duty.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcIncomingContestResolver) AttachResolverKit(r ResolverKit) {
	h.ResolverKit = r
}

// A compile time assertion to ensure htlcIncomingContestResolver meets the
// ContractResolver interface.
var _ ContractResolver = (*htlcIncomingContestResolver)(nil)

// commitSweepResolver is a resolver that will attempt to sweep the commitment
// output paying to us, in the case that the remote party broadcasts their
// version of the commitment transaction. We can sweep this output immediately,
// as it doesn't have a time-lock delay.
type commitSweepResolver struct {
	// commitResolution contains all data required to successfully sweep
	// this HTLC on-chain.
	commitResolution lnwallet.CommitOutputResolution

	// resolved reflects if the contract has been fully resolved or not.
	resolved bool

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	broadcastHeight uint32

	// chanPoint is the channel point of the original contract.
	chanPoint wire.OutPoint

	// sweepTx is the fully signed transaction which when broadcast, will
	// sweep the commitment output into an output under control by the
	// source wallet.
	sweepTx *wire.MsgTx

	ResolverKit
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
func (c *commitSweepResolver) ResolverKey() []byte {
	key := newResolverID(c.commitResolution.SelfOutPoint)
	return key[:]
}

// Resolve instructs the contract resolver to resolve the output on-chain. Once
// the output has been *fully* resolved, the function should return immediately
// with a nil ContractResolver value for the first return value.  In the case
// that the contract requires further resolution, then another resolve is
// returned.
//
// NOTE: This function MUST be run as a goroutine.
func (c *commitSweepResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if c.resolved {
		return nil, nil
	}

	// First, we'll register for a notification once the commitment output
	// itself has been confirmed.
	//
	// TODO(roasbeef): instead sweep asap if remote commit? yeh
	commitTXID := c.commitResolution.SelfOutPoint.Hash
	sweepScript := c.commitResolution.SelfOutputSignDesc.Output.PkScript
	confNtfn, err := c.Notifier.RegisterConfirmationsNtfn(
		&commitTXID, sweepScript, 1, c.broadcastHeight,
	)
	if err != nil {
		return nil, err
	}

	log.Debugf("%T(%v): waiting for commit tx to confirm", c, c.chanPoint)

	select {
	case _, ok := <-confNtfn.Confirmed:
		if !ok {
			return nil, fmt.Errorf("quitting")
		}

	case <-c.Quit:
		return nil, fmt.Errorf("quitting")
	}

	// TODO(roasbeef): checkpoint tx confirmed?

	// We're dealing with our commitment transaction if the delay on the
	// resolution isn't zero.
	isLocalCommitTx := c.commitResolution.MaturityDelay != 0

	switch {
	// If the sweep transaction isn't already generated, and the remote
	// party broadcast the commitment transaction then we'll create it now.
	case c.sweepTx == nil && !isLocalCommitTx:
		// As we haven't already generated the sweeping transaction,
		// we'll now craft an input with all the information required
		// to create a fully valid sweeping transaction to recover
		// these coins.
		input := sweep.MakeBaseInput(
			&c.commitResolution.SelfOutPoint,
			lnwallet.CommitmentNoDelay,
			&c.commitResolution.SelfOutputSignDesc,
			c.broadcastHeight,
		)

		// With out input constructed, we'll now request that the
		// sweeper construct a valid sweeping transaction for this
		// input.
		//
		// TODO: Set tx lock time to current block height instead of
		// zero. Will be taken care of once sweeper implementation is
		// complete.
		//
		// TODO: Use time-based sweeper and result chan.
		c.sweepTx, err = c.Sweeper.CreateSweepTx(
			[]sweep.Input{&input}, sweepConfTarget, 0,
		)
		if err != nil {
			return nil, err
		}

		log.Infof("%T(%v): sweeping commit output with tx=%v", c,
			c.chanPoint, spew.Sdump(c.sweepTx))

		// With the sweep transaction constructed, we'll now Checkpoint
		// our state.
		if err := c.Checkpoint(c); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return nil, err
		}

		// With the sweep transaction checkpointed, we'll now publish
		// the transaction. Upon restart, the resolver will immediately
		// take the case below since the sweep tx is checkpointed.
		err := c.PublishTx(c.sweepTx)
		if err != nil && err != lnwallet.ErrDoubleSpend {
			log.Errorf("%T(%v): unable to publish sweep tx: %v",
				c, c.chanPoint, err)
			return nil, err
		}

	// If the sweep transaction has been generated, and the remote party
	// broadcast the commit transaction, we'll republish it for reliability
	// to ensure it confirms. The resolver will enter this case after
	// checkpointing in the case above, ensuring we reliably on restarts.
	case c.sweepTx != nil && !isLocalCommitTx:
		err := c.PublishTx(c.sweepTx)
		if err != nil && err != lnwallet.ErrDoubleSpend {
			log.Errorf("%T(%v): unable to publish sweep tx: %v",
				c, c.chanPoint, err)
			return nil, err
		}

	// Otherwise, this is our commitment transaction, So we'll obtain the
	// sweep transaction once the commitment output has been spent.
	case c.sweepTx == nil && isLocalCommitTx:
		// Otherwise, if we're dealing with our local commitment
		// transaction, then the output we need to sweep has been sent
		// to the nursery for incubation. In this case, we'll wait
		// until the commitment output has been spent.
		spendNtfn, err := c.Notifier.RegisterSpendNtfn(
			&c.commitResolution.SelfOutPoint,
			c.commitResolution.SelfOutputSignDesc.Output.PkScript,
			c.broadcastHeight,
		)
		if err != nil {
			return nil, err
		}

		log.Infof("%T(%v): waiting for commit output to be swept", c,
			c.chanPoint)

		select {
		case commitSpend, ok := <-spendNtfn.Spend:
			if !ok {
				return nil, fmt.Errorf("quitting")
			}

			// Once we detect the commitment output has been spent,
			// we'll extract the spending transaction itself, as we
			// now consider this to be our sweep transaction.
			c.sweepTx = commitSpend.SpendingTx

			log.Infof("%T(%v): commit output swept by txid=%v",
				c, c.chanPoint, c.sweepTx.TxHash())

			if err := c.Checkpoint(c); err != nil {
				log.Errorf("unable to Checkpoint: %v", err)
				return nil, err
			}
		case <-c.Quit:
			return nil, fmt.Errorf("quitting")
		}
	}

	log.Infof("%T(%v): waiting for commit sweep txid=%v conf", c, c.chanPoint,
		c.sweepTx.TxHash())

	// Now we'll wait until the sweeping transaction has been fully
	// confirmed.  Once it's confirmed, we can mark this contract resolved.
	sweepTXID := c.sweepTx.TxHash()
	sweepingScript := c.sweepTx.TxOut[0].PkScript
	confNtfn, err = c.Notifier.RegisterConfirmationsNtfn(
		&sweepTXID, sweepingScript, 1, c.broadcastHeight,
	)
	if err != nil {
		return nil, err
	}
	select {
	case confInfo, ok := <-confNtfn.Confirmed:
		if !ok {
			return nil, fmt.Errorf("quitting")
		}

		log.Infof("ChannelPoint(%v) commit tx is fully resolved, at height: %v",
			c.chanPoint, confInfo.BlockHeight)

	case <-c.Quit:
		return nil, fmt.Errorf("quitting")
	}

	// Once the transaction has received a sufficient number of
	// confirmations, we'll mark ourselves as fully resolved and exit.
	c.resolved = true
	return nil, c.Checkpoint(c)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) Stop() {
	close(c.Quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) IsResolved() bool {
	return c.resolved
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) Encode(w io.Writer) error {
	if err := encodeCommitResolution(w, &c.commitResolution); err != nil {
		return err
	}

	if err := binary.Write(w, endian, c.resolved); err != nil {
		return err
	}
	if err := binary.Write(w, endian, c.broadcastHeight); err != nil {
		return err
	}
	if _, err := w.Write(c.chanPoint.Hash[:]); err != nil {
		return err
	}
	err := binary.Write(w, endian, c.chanPoint.Index)
	if err != nil {
		return err
	}

	if c.sweepTx != nil {
		return c.sweepTx.Serialize(w)
	}

	return nil
}

// Decode attempts to decode an encoded ContractResolver from the passed Reader
// instance, returning an active ContractResolver instance.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) Decode(r io.Reader) error {
	if err := decodeCommitResolution(r, &c.commitResolution); err != nil {
		return err
	}

	if err := binary.Read(r, endian, &c.resolved); err != nil {
		return err
	}
	if err := binary.Read(r, endian, &c.broadcastHeight); err != nil {
		return err
	}
	_, err := io.ReadFull(r, c.chanPoint.Hash[:])
	if err != nil {
		return err
	}
	err = binary.Read(r, endian, &c.chanPoint.Index)
	if err != nil {
		return err
	}

	txBytes, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	if len(txBytes) == 0 {
		return nil
	}

	txReader := bytes.NewReader(txBytes)
	tx := &wire.MsgTx{}
	if err := tx.Deserialize(txReader); err != nil {
		return nil
	}

	c.sweepTx = tx
	return nil
}

// AttachResolverKit should be called once a resolved is successfully decoded
// from its stored format. This struct delivers a generic tool kit that
// resolvers need to complete their duty.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) AttachResolverKit(r ResolverKit) {
	c.ResolverKit = r
}

// A compile time assertion to ensure commitSweepResolver meets the
// ContractResolver interface.
var _ ContractResolver = (*commitSweepResolver)(nil)

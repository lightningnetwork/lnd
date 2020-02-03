package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
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

	// htlc contains information on the htlc that we are resolving on-chain.
	htlc channeldb.HTLC

	// currentReport stores the current state of the resolver for reporting
	// over the rpc interface.
	currentReport ContractReport

	// reportLock prevents concurrent access to the resolver report.
	reportLock sync.Mutex

	contractResolverKit
}

// newSuccessResolver instanties a new htlc success resolver.
func newSuccessResolver(res lnwallet.IncomingHtlcResolution,
	broadcastHeight uint32, htlc channeldb.HTLC,
	resCfg ResolverConfig) *htlcSuccessResolver {

	r := &htlcSuccessResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
		htlcResolution:      res,
		broadcastHeight:     broadcastHeight,
		htlc:                htlc,
	}

	r.initReport()

	return r
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
// preimage to. If the HTLC is on the commitment of the remote party, then we'll
// simply sweep it directly. Otherwise, we'll hand this off to the utxo nursery
// to do its duty. There is no need to make a call to the invoice registry
// anymore. Every HTLC has already passed through the incoming contest resolver
// and in there the invoice was already marked as settled.
//
// TODO(roasbeef): create multi to batch
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Resolve() (ContractResolver, error) {
	var sweepInput input.Input

	// If we don't have a success transaction, then this means that this is
	// an output on the remote party's commitment transaction.
	if h.htlcResolution.SignedSuccessTx != nil {
		log.Infof("%T(%x): broadcasting second-layer transition tx: %v",
			h, h.htlc.RHash[:], spew.Sdump(h.htlcResolution.SignedSuccessTx))

		// We'll now broadcast the second layer transaction so we can kick off
		// the claiming process.
		//
		// TODO(roasbeef): after changing sighashes send to tx bundler
		err := h.PublishTx(h.htlcResolution.SignedSuccessTx)
		if err != nil {
			return nil, err
		}

		// Wait for success tx to confirm.
		confHeight, err := h.waitForSecondLevelConf(1)
		if err != nil {
			return nil, err
		}

		// Update reported maturity height and advance to stage two
		// (waiting for csv lock).
		h.reportLock.Lock()
		h.currentReport.MaturityHeight =
			confHeight + h.htlcResolution.CsvDelay - 1
		h.currentReport.Stage = 2
		h.reportLock.Unlock()

		// Wait for csv lock to expire.
		_, err = h.waitForSecondLevelConf(h.htlcResolution.CsvDelay)
		if err != nil {
			return nil, err
		}

		// Create final input for the sweeper.
		sweepInput = input.NewCsvInput(
			&h.htlcResolution.ClaimOutpoint,
			input.HtlcAcceptedSuccessSecondLevel,
			&h.htlcResolution.SweepSignDesc,
			h.broadcastHeight, h.htlcResolution.CsvDelay,
		)
	} else {
		// Create input to sweep htlc from commitment tx.
		inp := input.MakeHtlcSucceedInput(
			&h.htlcResolution.ClaimOutpoint,
			&h.htlcResolution.SweepSignDesc,
			h.htlcResolution.Preimage[:],
			h.broadcastHeight,
		)
		sweepInput = &inp
	}

	log.Infof("%T(%x): sweeping output for "+
		"incoming+remote htlc confirmed", h,
		h.htlc.RHash[:])

	// Offer the created input to the sweeper.
	resultChan, err := h.Sweeper.SweepInput(
		sweepInput,
		sweep.Params{
			Fee: sweep.FeePreference{
				ConfTarget: sweepConfTarget,
			},
		},
	)
	if err != nil {
		return nil, err
	}

	// Wait for the sweep result.
	select {
	case result, ok := <-resultChan:
		if !ok {
			return nil, errResolverShuttingDown
		}
		if result.Err != nil {
			return nil, result.Err
		}

		log.Infof("%T(%x): sweep tx (txid=%v) confirmed",
			h, h.htlc.RHash[:], result.Tx)

	case <-h.quit:
		return nil, errResolverShuttingDown
	}

	// Funds have been swept and balance is no longer in limbo.
	h.reportLock.Lock()
	h.currentReport.RecoveredBalance = h.currentReport.LimboBalance
	h.currentReport.LimboBalance = 0
	h.reportLock.Unlock()

	// Once the transaction has received a sufficient number of
	// confirmations, we'll mark ourselves as fully resolved and exit.
	h.resolved = true
	return nil, nil
}

func (h *htlcSuccessResolver) waitForSecondLevelConf(confDepth uint32) (
	uint32, error) {

	txID := h.htlcResolution.SignedSuccessTx.TxHash()
	pkScript := h.htlcResolution.SignedSuccessTx.TxOut[0].PkScript

	confChan, err := h.Notifier.RegisterConfirmationsNtfn(
		&txID, pkScript, confDepth, h.broadcastHeight,
	)
	if err != nil {
		return 0, err
	}
	defer confChan.Cancel()

	select {
	case conf, ok := <-confChan.Confirmed:
		if !ok {
			return 0, fmt.Errorf("cannot get confirmation "+
				"for commit tx %v", txID)
		}

		return conf.BlockHeight, nil

	case <-h.quit:
		return 0, errResolverShuttingDown
	}
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Stop() {
	close(h.quit)
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

	// This was previously the resolved state of the resolver. Write a dummy
	// value, because the resolver is stateless.
	if err := binary.Write(w, endian, false); err != nil {
		return err
	}

	if err := binary.Write(w, endian, h.broadcastHeight); err != nil {
		return err
	}
	if _, err := w.Write(h.htlc.RHash[:]); err != nil {
		return err
	}

	return nil
}

// newSuccessResolverFromReader attempts to decode an encoded ContractResolver
// from the passed Reader instance, returning an active ContractResolver
// instance.
func newSuccessResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*htlcSuccessResolver, error) {

	h := &htlcSuccessResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
	}

	// First we'll decode our inner HTLC resolution.
	if err := decodeIncomingResolution(r, &h.htlcResolution); err != nil {
		return nil, err
	}

	// Next, we'll read all the fields that are specified to the contract
	// resolver.
	if err := binary.Read(r, endian, &h.outputIncubating); err != nil {
		return nil, err
	}

	// Read a dummy byte that previously stored the resolved state.
	var dummy bool
	if err := binary.Read(r, endian, &dummy); err != nil {
		return nil, err
	}

	if err := binary.Read(r, endian, &h.broadcastHeight); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, h.htlc.RHash[:]); err != nil {
		return nil, err
	}

	h.initReport()

	return h, nil
}

// Supplement adds additional information to the resolver that is required
// before Resolve() is called.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcSuccessResolver) Supplement(htlc channeldb.HTLC) {
	h.htlc = htlc
}

// HtlcPoint returns the htlc's outpoint on the commitment tx.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcSuccessResolver) HtlcPoint() wire.OutPoint {
	return h.htlcResolution.HtlcPoint()
}

// report returns a report on the resolution state of the contract.
func (h *htlcSuccessResolver) report() *ContractReport {
	h.reportLock.Lock()
	defer h.reportLock.Unlock()

	copy := h.currentReport
	return &copy
}

// initReport initializes the pending channels report for this resolver.
func (h *htlcSuccessResolver) initReport() {
	amt := btcutil.Amount(
		h.htlcResolution.SweepSignDesc.Output.Value,
	)

	// Set the initial report. Because we resolve via the success path, we
	// don't need to wait for the htlc to expire and can start in stage 2
	// right away.
	h.currentReport = ContractReport{
		Outpoint:         h.htlcResolution.ClaimOutpoint,
		Type:             ReportOutputIncomingHtlc,
		Amount:           amt,
		LimboBalance:     amt,
		RecoveredBalance: 0,
		Stage:            1,
	}
}

// A compile time assertion to ensure htlcSuccessResolver meets the
// ContractResolver interface.
var _ htlcContractResolver = (*htlcSuccessResolver)(nil)

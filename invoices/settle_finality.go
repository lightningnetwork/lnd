package invoices

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
)

// DefaultSettleReconcileInterval is the default interval at which the
// registry checks whether htlcs in HtlcStatePendingSettle reached a final
// outcome that it wasn't notified of.
const DefaultSettleReconcileInterval = 5 * time.Minute

const (
	// minFinalizeRetryDelay is the delay before the first retry of an
	// outcome that couldn't be written to the invoice database.
	minFinalizeRetryDelay = time.Second

	// maxFinalizeRetryDelay caps the backoff between retries of outcomes
	// that couldn't be written to the invoice database.
	maxFinalizeRetryDelay = time.Minute
)

// FinalHtlcOutcome describes the final outcome of an htlc.
type FinalHtlcOutcome struct {
	// Key identifies the htlc.
	Key CircuitKey

	// Settled is true if the htlc was settled, and false if it was failed
	// back or reclaimed by the payer on chain.
	Settled bool

	// Offchain is true if the htlc was resolved off chain.
	Offchain bool
}

// FinalHtlcSource provides channel outcomes used to finalize ordinary invoice
// payments. Their preimages are released before settlement is irrevocable,
// so the registry needs this evidence before reporting final credit.
type FinalHtlcSource interface {
	// SubscribeFinalHtlcs returns a channel that delivers the final
	// outcome of htlcs as they become known, and a function to cancel the
	// subscription. Outcomes that became known before the subscription
	// was established are not delivered.
	SubscribeFinalHtlcs() (<-chan FinalHtlcOutcome, func(), error)

	// LookupFinalHtlcs returns the recorded final outcome of those of the
	// given htlcs that have one. Htlcs without a recorded outcome are
	// omitted from the result. This is a cheap point lookup per htlc.
	LookupFinalHtlcs(keys []CircuitKey) (map[CircuitKey]FinalHtlcOutcome,
		error)

	// InferFinalHtlcs derives the final outcome of those of the given
	// htlcs whose outcome follows from the channel state, for htlcs
	// without a recorded outcome. This reads all channels during startup
	// and nonempty periodic passes to recover channel-only completion.
	InferFinalHtlcs(keys []CircuitKey) (map[CircuitKey]FinalHtlcOutcome,
		error)
}

// trackPendingSettles adds all htlcs of the invoice that are in
// HtlcStatePendingSettle to the set of htlcs that wait for their final
// outcome.
//
// NOTE: Must be called with the registry lock held.
func (i *InvoiceRegistry) trackPendingSettles(hash lntypes.Hash,
	invoice *Invoice) {

	for key, htlc := range invoice.Htlcs {
		if htlc.State != HtlcStatePendingSettle {
			continue
		}

		i.pendingSettles[key] = hash
	}
}

// finalizeHtlc records the final outcome of an htlc in HtlcStatePendingSettle.
// Outcomes of htlcs that aren't pending are ignored, which includes htlcs that
// don't pay to an invoice.
func (i *InvoiceRegistry) finalizeHtlc(ctx context.Context,
	outcome FinalHtlcOutcome) error {

	// Most outcomes are for forwarded htlcs, so check without blocking
	// invoice processing first.
	i.RLock()
	_, ok := i.pendingSettles[outcome.Key]
	i.RUnlock()
	if !ok {
		return nil
	}

	i.Lock()
	defer i.Unlock()

	hash, ok := i.pendingSettles[outcome.Key]
	if !ok {
		return nil
	}

	ref := InvoiceRefByHash(hash)

	var finalized bool
	invoice, err := i.idb.UpdateInvoice(
		ctx, ref, nil,
		func(inv *Invoice) (*InvoiceUpdateDesc, error) {
			htlc, ok := inv.Htlcs[outcome.Key]
			if !ok {
				return nil, fmt.Errorf("htlc %v not found on "+
					"invoice %v", outcome.Key, ref)
			}

			// The htlc may have been finalized already, for
			// example if its outcome was delivered twice.
			if htlc.State != HtlcStatePendingSettle {
				return nil, nil
			}
			finalized = true

			return &InvoiceUpdateDesc{
				UpdateType: FinalizeHTLCsUpdate,
				FinalizeHtlcs: map[CircuitKey]bool{
					outcome.Key: outcome.Settled,
				},
			}, nil
		},
	)
	if err != nil {
		return fmt.Errorf("unable to finalize htlc %v on invoice %v: "+
			"%w", outcome.Key, ref, err)
	}

	delete(i.pendingSettles, outcome.Key)

	if !finalized {
		return nil
	}

	if !outcome.Settled {
		log.Warnf("Invoice%v: htlc %v released its preimage but was "+
			"not settled (offchain=%v), invoice state is now %v, "+
			"amt_paid=%v", ref, outcome.Key,
			outcome.Offchain, invoice.State, invoice.AmtPaid)
	} else {
		log.Debugf("Invoice%v: htlc %v settled (offchain=%v), "+
			"invoice state is now %v", ref, outcome.Key,
			outcome.Offchain, invoice.State)
	}

	i.notifyClients(hash, invoice, nil)

	return nil
}

// reconcilePendingSettles recovers ordinary outcomes missed by the live
// subscription. A completed channel is only fallback evidence: re-read the
// records after scanning so a concurrent durable failure overrides inferred
// success before any invoice is credited.
func (i *InvoiceRegistry) reconcilePendingSettles(ctx context.Context) error {
	i.RLock()
	keys := make([]CircuitKey, 0, len(i.pendingSettles))
	for key := range i.pendingSettles {
		keys = append(keys, key)
	}
	i.RUnlock()

	// Most ticks have no pending work, so avoid channel scans while idle.
	if len(keys) == 0 {
		return nil
	}

	source := i.cfg.FinalHtlcSource
	recorded, err := source.LookupFinalHtlcs(keys)
	if err != nil {
		return err
	}

	// Own the merged result even when a source has no recorded outcomes.
	outcomes := make(map[CircuitKey]FinalHtlcOutcome, len(recorded))
	maps.Copy(outcomes, recorded)

	var remaining []CircuitKey
	for _, key := range keys {
		if _, ok := outcomes[key]; !ok {
			remaining = append(remaining, key)
		}
	}

	if len(remaining) > 0 {
		inferred, err := source.InferFinalHtlcs(remaining)
		if err != nil {
			return err
		}

		// A resolver persists its outcome before closing the channel.
		// If that happened during the scan, the record is stronger
		// evidence than the now-resolved channel summary. On a read
		// failure, leave every candidate pending instead of crediting
		// it.
		recorded, err := source.LookupFinalHtlcs(remaining)
		if err != nil {
			return err
		}

		maps.Copy(outcomes, inferred)
		maps.Copy(outcomes, recorded)
	}

	log.Debugf("Reconciled %d of %d htlcs pending settlement",
		len(outcomes), len(keys))

	// A failed invoice write must not hold up other outcomes. The failed
	// key remains tracked and will be retried by the next reconciliation.
	var errs []error
	for _, outcome := range outcomes {
		if err := i.finalizeHtlc(ctx, outcome); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// startSettleFinalizer subscribes to final htlc outcomes, loads all htlcs
// that wait for their final outcome and reconciles them. The subscription is
// established before the lookup, so that no outcome can be missed in between.
//
// A failure to reconcile on start doesn't fail the start of the registry.
// Pending work is retried periodically, including channel-only completion
// that becomes inferable after startup without a per-HTLC notification.
func (i *InvoiceRegistry) startSettleFinalizer(ctx context.Context,
	pendingInvoices map[lntypes.Hash]Invoice) error {

	if i.cfg.FinalHtlcSource == nil {
		log.Warnf("No final htlc source configured, invoices will " +
			"remain in PendingSettle")

		return nil
	}

	outcomes, cancel, err := i.cfg.FinalHtlcSource.SubscribeFinalHtlcs()
	if err != nil {
		return fmt.Errorf("unable to subscribe to final htlc "+
			"outcomes: %w", err)
	}

	i.Lock()
	for hash, invoice := range pendingInvoices {
		i.trackPendingSettles(hash, &invoice)
	}
	i.Unlock()

	interval := i.cfg.SettleReconcileInterval
	if interval == 0 {
		interval = DefaultSettleReconcileInterval
	}

	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		defer cancel()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Outcomes that couldn't be written, for example because the
		// invoice database was unavailable, are retried with backoff
		// rather than waiting for the next reconciliation.
		retries := make(map[CircuitKey]FinalHtlcOutcome)
		retryDelay := minFinalizeRetryDelay
		var retryTimer <-chan time.Time

		finalize := func(outcome FinalHtlcOutcome) {
			err := i.finalizeHtlc(ctx, outcome)
			if err == nil {
				delete(retries, outcome.Key)
				return
			}

			log.Errorf("Unable to finalize htlc, retrying in %v: "+
				"%v", retryDelay, err)

			retries[outcome.Key] = outcome
			if retryTimer == nil {
				retryTimer = time.After(retryDelay)
			}
		}

		for {
			select {
			case outcome, ok := <-outcomes:
				if !ok {
					log.Errorf("Final htlc outcome " +
						"subscription closed")

					return
				}

				finalize(outcome)

			case <-retryTimer:
				retryTimer = nil
				pending := maps.Clone(retries)
				for _, outcome := range pending {
					finalize(outcome)
				}

				// Back off while writes keep failing.
				if len(retries) == 0 {
					retryDelay = minFinalizeRetryDelay
				} else {
					retryDelay = min(
						2*retryDelay,
						maxFinalizeRetryDelay,
					)
				}

			// Live events and failed-write retries cover normal
			// delivery. Channel-only completions, including breach
			// cleanup, also need inference after an otherwise
			// successful startup.
			case <-ticker.C:
				err := i.reconcilePendingSettles(ctx)
				if err != nil {
					log.Errorf("Unable to reconcile htlcs "+
						"pending settlement: %v", err)
				}

			case <-i.quit:
				return
			}
		}
	}()

	err = i.reconcilePendingSettles(ctx)
	if err != nil {
		log.Errorf("Unable to reconcile htlcs pending settlement on "+
			"start, retrying every %v: %v", interval, err)
	}

	return nil
}

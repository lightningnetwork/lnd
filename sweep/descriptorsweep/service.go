package descriptorsweep

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/descriptors"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/tlv"
)

func (s *Service) verifyBindings(desc *descriptors.Descriptor,
	bindings []KeyBinding) error {

	keys := desc.Keys()
	if len(bindings) != len(keys) {
		return fmt.Errorf("descriptor has %d keys, got %d bindings",
			len(keys), len(bindings))
	}

	remaining := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		remaining[key] = struct{}{}
	}
	for _, binding := range bindings {
		if _, ok := remaining[binding.DescriptorKey]; !ok {
			return fmt.Errorf("unknown or duplicate descriptor key %q",
				binding.DescriptorKey)
		}

		derived, err := s.cfg.KeyRing.DeriveKey(binding.KeyLocator)
		if err != nil {
			return fmt.Errorf("derive key %q: %w", binding.DescriptorKey, err)
		}
		if derived.PubKey == nil {
			return fmt.Errorf("derived key %q has no public key",
				binding.DescriptorKey)
		}

		want, err := descriptorPubKey(binding.DescriptorKey)
		if err != nil {
			return fmt.Errorf("descriptor key %q: %w",
				binding.DescriptorKey, err)
		}
		if !bytes.Equal(want, derived.PubKey.SerializeCompressed()) {
			return fmt.Errorf("descriptor key %q does not match locator",
				binding.DescriptorKey)
		}

		delete(remaining, binding.DescriptorKey)
	}

	return nil
}

func descriptorPubKey(key string) ([]byte, error) {
	// Fixed-index MVP bindings intentionally only accept a raw compressed key.
	// Extended keys and origin paths are range-capable and need a derivation
	// aware binding format before they can be safely accepted.
	if len(key) != 66 {
		return nil, errors.New("only raw compressed public keys are supported")
	}
	raw, err := hex.DecodeString(key)
	if err != nil {
		return nil, err
	}
	pubKey, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, err
	}
	return pubKey.SerializeCompressed(), nil
}

func rejectTimeLocks(desc *descriptors.Descriptor) error {
	timelocks, err := desc.PotentialTimelocks()
	if err != nil {
		return fmt.Errorf("inspect descriptor timelocks: %w", err)
	}
	for _, timelock := range timelocks {
		switch timelock.Type {
		case descriptors.TimelockTypeAbsolute:
			if timelock.Value >= txscript.LockTimeThreshold {
				return errors.New("time-based CLTV is not supported")
			}

		case descriptors.TimelockTypeRelative:
			if timelock.Value&wire.SequenceLockTimeIsSeconds != 0 {
				return errors.New("time-based CSV is not supported")
			}
		}
	}
	return nil
}

func (s *Service) resume(id RegistrationID) error {
	select {
	case <-s.quit:
		return nil
	default:
	}

	s.mu.RLock()
	stored, ok := s.records[id]
	if !ok {
		s.mu.RUnlock()
		return ErrNotFound
	}
	record := stored.snapshot()
	blockScan := stored.BlockScan
	s.mu.RUnlock()

	switch record.Status {
	case StatusSwept, StatusFailed:
		return nil
	case StatusSweeping:
		// SweepInput is intentionally idempotent for an already-known
		// outpoint. Rebuild the exact frozen input after restart.
		err := s.restoreSweep(id)
		if err == nil || isRetryable(err) {
			return err
		}
		return deterministic(err)
	default:
		if record.OutPoint != nil {
			return s.trySweep(id)
		}
		if blockScan {
			s.mu.RLock()
			bestHeight := s.bestHeight
			s.mu.RUnlock()
			return s.scanMatureBlocks(id, bestHeight)
		}
		return s.watchOutput(id)
	}
}

// waitForReady defers all notifier calls until the daemon has started both the
// notifier and sweeper. RegisterBlockEpochNtfn is retried because Start has
// already returned by this point and a transient notifier error must not leave
// durable registrations inert until the next daemon restart.
func (s *Service) waitForReady() {
	defer s.wg.Done()

	select {
	case <-s.cfg.Ready:
	case <-s.quit:
		return
	}

	var epochs *chainntnfs.BlockEpochEvent
	for {
		select {
		case <-s.quit:
			return
		default:
		}

		var err error
		epochs, err = s.cfg.Notifier.RegisterBlockEpochNtfn(nil)
		if err == nil {
			// A nil best block asks the notifier to send its current tip
			// immediately. Consume that tip before restoring registrations so
			// CLTV/CSV scheduling and frozen fee deadlines never start from
			// height zero after a restart.
			for {
				select {
				case epoch, ok := <-epochs.Epochs:
					if !ok {
						epochs.Cancel()
						err = errors.New("block epoch stream closed before current tip")
						break
					}
					if epoch == nil || epoch.Height < 0 {
						continue
					}

					s.mu.Lock()
					s.bestHeight = uint32(epoch.Height)
					s.mu.Unlock()
					err = nil

				case <-s.quit:
					epochs.Cancel()
					return
				}
				break
			}
			if err == nil {
				break
			}
		}

		select {
		case <-time.After(time.Second):
		case <-s.quit:
			return
		}
	}

	// Keep notifierReady false while draining registrations. Register and
	// AddPreimage only persist and mark the ID pending in that state. Taking
	// and clearing pending under the same lock used to publish readiness
	// ensures data added while an ID is being resumed triggers another pass.
	for {
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.notifierReady = true
			s.mu.Unlock()
			break
		}
		ids := make([]RegistrationID, 0, len(s.pending))
		for id := range s.pending {
			ids = append(ids, id)
			delete(s.pending, id)
		}
		s.mu.Unlock()

		for _, id := range ids {
			if err := s.resume(id); err != nil {
				s.handleRegistrationError(id, err)
			}
		}
	}

	s.consumeEpochs(epochs)
}

func (s *Service) restoreSweep(id RegistrationID) error {
	s.mu.RLock()
	record, ok := s.records[id]
	if !ok {
		s.mu.RUnlock()
		return ErrNotFound
	}
	if record.OutPoint == nil {
		s.mu.RUnlock()
		return deterministic(errors.New(
			"frozen descriptor sweep has no outpoint",
		))
	}
	frozen := record.cloneForInput()
	s.mu.RUnlock()

	desc, err := descriptors.NewDescriptor(frozen.CanonicalDescriptor)
	if err != nil {
		return deterministic(err)
	}
	assets := makeFrozenAssets(frozen)
	plan, err := desc.PlanAt(0, frozen.DerivationIndex, assets)
	if err != nil {
		return deterministic(fmt.Errorf(
			"restore frozen descriptor plan: %w", err,
		))
	}
	constraints := plan.TxConstraints()
	if constraints.MinTxVersion > 2 {
		return deterministic(fmt.Errorf(
			"frozen path requires transaction version %d",
			constraints.MinTxVersion,
		))
	}
	if !sameOptionalUint32(constraints.AbsoluteLocktime,
		frozen.PlanLocktime) || !sameOptionalUint32(
		constraints.RelativeLocktime, frozen.PlanSequence,
	) {

		return deterministic(errors.New(
			"restored descriptor plan changed frozen branch",
		))
	}

	inp, err := newDescriptorInput(desc, plan, frozen)
	if err != nil {
		return deterministic(err)
	}
	params := sweep.Params{
		Budget:    frozen.Budget,
		Immediate: frozen.Immediate,
	}
	if frozen.HasStartingFeeRate {
		params.StartingFeeRate = fn.Some(frozen.StartingFeeRate)
	}
	if frozen.PlanDeadlineHeight != nil {
		params.DeadlineHeight = fn.Some(*frozen.PlanDeadlineHeight)
	}
	result, err := s.cfg.Sweeper.SweepInput(inp, params)
	if err != nil {
		return retryablef("restore descriptor sweep input: %w", err)
	}
	s.launch(func() { s.consumeSweepResult(id, result) })
	return nil
}

func (s *Service) watchOutput(id RegistrationID) error {
	select {
	case <-s.quit:
		return nil
	default:
	}

	// Serialize installation per registration. In particular this makes the
	// transition from the initial readiness drain to live registration
	// idempotent even when both paths race for the same ID.
	s.mu.Lock()
	record, ok := s.records[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if _, ok := s.watches[id]; ok {
		s.mu.Unlock()
		return nil
	}
	pkScript := append([]byte(nil), record.PkScript...)
	minConfs, heightHint := record.MinConfs, record.WatchHeight
	if heightHint == 0 {
		heightHint = record.HeightHint
	}

	event, err := s.cfg.Notifier.RegisterConfirmationsNtfn(
		nil, pkScript, minConfs, heightHint, chainntnfs.WithIncludeBlock(),
	)
	if err != nil {
		s.mu.Unlock()
		return retryablef("register descriptor confirmation: %w", err)
	}

	_, err = s.updateRecordLocked(id, func(next *storedRecord) error {
		next.Status = StatusWatching
		return nil
	})
	if err != nil {
		s.mu.Unlock()
		event.Cancel()
		return err
	}
	s.watches[id] = event.Cancel
	s.wg.Add(1)
	s.mu.Unlock()

	go s.consumeConfirmation(id, event)
	return nil
}

func (s *Service) consumeConfirmation(id RegistrationID,
	event *chainntnfs.ConfirmationEvent) {

	defer s.wg.Done()
	select {
	case conf, ok := <-event.Confirmed:
		if !ok || conf == nil {
			s.detachWatch(id)
			s.handleRegistrationError(id, retryable(errors.New(
				"descriptor confirmation stream closed",
			)))
			return
		}
		if err := s.outputConfirmed(id, conf); err != nil {
			s.handleRegistrationError(id, err)
		}

	case <-s.quit:
		return
	}
}

func (s *Service) outputConfirmed(id RegistrationID,
	conf *chainntnfs.TxConfirmation) error {

	s.mu.Lock()
	record, ok := s.records[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if cancel, ok := s.watches[id]; ok {
		cancel()
		delete(s.watches, id)
	}

	if conf.Block == nil {
		s.mu.Unlock()
		return retryable(errors.New(
			"descriptor confirmation did not include its block",
		))
	}
	match, err := findExactOutput(
		conf.Block, record.PkScript, int64(record.ExpectedValue),
	)
	if err != nil {
		s.mu.Unlock()
		return deterministic(err)
	}
	if match == nil {
		if conf.BlockHeight == math.MaxUint32 {
			s.mu.Unlock()
			return deterministic(errors.New(
				"descriptor watch height overflow",
			))
		}

		// A script-only notifier may first report an output with a value
		// chosen by an unrelated party. Advance past that block and persist
		// the scan cursor; re-registering the same script can replay the
		// notifier's cached match forever.
		bestHeight := s.bestHeight
		_, err := s.updateRecordLocked(id, func(next *storedRecord) error {
			next.WatchHeight = conf.BlockHeight + 1
			next.BlockScan = true
			next.Status = StatusWatching
			return nil
		})
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()

		return s.scanMatureBlocks(id, bestHeight)
	}

	txid := match.tx.TxHash()
	op := wire.OutPoint{Hash: txid, Index: match.outputIndex}
	_, err = s.updateRecordLocked(id, func(next *storedRecord) error {
		next.OutPoint = &op
		next.Value = match.tx.TxOut[match.outputIndex].Value
		next.ConfirmationHeight = conf.BlockHeight
		next.BlockScan = false
		next.Status = StatusFound
		return nil
	})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	return s.trySweep(id)
}

func (s *Service) detachWatch(id RegistrationID) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cancel, ok := s.watches[id]; ok {
		cancel()
		delete(s.watches, id)
	}
}

type exactOutputMatch struct {
	tx          *wire.MsgTx
	outputIndex uint32
}

func findExactOutput(block *wire.MsgBlock, pkScript []byte,
	expectedValue int64) (*exactOutputMatch, error) {

	if block == nil {
		return nil, errors.New("descriptor confirmation did not include its block")
	}

	var match *exactOutputMatch
	for _, tx := range block.Transactions {
		if tx == nil {
			continue
		}
		for outputIndex, output := range tx.TxOut {
			if output == nil || output.Value != expectedValue ||
				!bytes.Equal(output.PkScript, pkScript) {

				continue
			}
			if match != nil {
				return nil, errors.New("confirmed block has multiple exact descriptor outputs")
			}
			match = &exactOutputMatch{
				tx:          tx,
				outputIndex: uint32(outputIndex),
			}
		}
	}

	return match, nil
}

func (s *Service) scanMatureBlocks(id RegistrationID,
	bestHeight uint32) error {
	select {
	case <-s.quit:
		return nil
	default:
	}

	// Readiness restore and block epochs can overlap briefly. Serializing the
	// range scan prevents duplicate block work and duplicate sweeper offers.
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	s.mu.RLock()
	record, ok := s.records[id]
	if !ok {
		s.mu.RUnlock()
		return ErrNotFound
	}
	if !record.BlockScan || record.Status == StatusFailed ||
		record.Status == StatusSwept || record.OutPoint != nil {

		s.mu.RUnlock()
		return nil
	}
	if bestHeight+1 < record.MinConfs {
		s.mu.RUnlock()
		return nil
	}
	startHeight := record.WatchHeight
	matureThrough := bestHeight - (record.MinConfs - 1)
	pkScript := append([]byte(nil), record.PkScript...)
	expectedValue := int64(record.ExpectedValue)
	s.mu.RUnlock()

	if startHeight > matureThrough {
		return nil
	}

	for height := startHeight; height <= matureThrough; height++ {
		blockHash, err := s.cfg.BlockSource.GetBlockHash(int64(height))
		if err != nil {
			return retryablef("get descriptor scan block %d hash: %w",
				height, err)
		}
		block, err := s.cfg.BlockSource.GetBlock(blockHash)
		if err != nil {
			return retryablef("get descriptor scan block %d: %w",
				height, err)
		}
		match, err := findExactOutput(block, pkScript, expectedValue)
		if err != nil {
			return deterministic(err)
		}

		s.mu.Lock()
		record, ok := s.records[id]
		if !ok {
			s.mu.Unlock()
			return ErrNotFound
		}
		if !record.BlockScan || record.Status == StatusFailed ||
			record.Status == StatusSwept || record.OutPoint != nil {

			s.mu.Unlock()
			return nil
		}

		if match == nil {
			if height == math.MaxUint32 {
				s.mu.Unlock()
				return deterministic(errors.New(
					"descriptor scan height overflow",
				))
			}
			_, err := s.updateRecordLocked(
				id, func(next *storedRecord) error {
					next.WatchHeight = height + 1
					return nil
				},
			)
			if err != nil {
				s.mu.Unlock()
				return err
			}
			s.mu.Unlock()
			continue
		}

		txid := match.tx.TxHash()
		op := wire.OutPoint{Hash: txid, Index: match.outputIndex}
		_, err = s.updateRecordLocked(id, func(next *storedRecord) error {
			next.OutPoint = &op
			next.Value = match.tx.TxOut[match.outputIndex].Value
			next.ConfirmationHeight = height
			next.BlockScan = false
			next.Status = StatusFound
			return nil
		})
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()

		return s.trySweep(id)
	}

	return nil
}

func (s *Service) consumeEpochs(event *chainntnfs.BlockEpochEvent) {
	defer func() { event.Cancel() }()

	for {
		select {
		case epoch, ok := <-event.Epochs:
			if !ok {
				event.Cancel()
				replacement := s.reconnectEpochs()
				if replacement == nil {
					return
				}
				event = replacement
				continue
			}
			if epoch == nil || epoch.Height < 0 {
				continue
			}
			s.mu.Lock()
			s.bestHeight = uint32(epoch.Height)
			ids := make([]RegistrationID, 0, len(s.records))
			scanIDs := make([]RegistrationID, 0, len(s.records))
			for id, record := range s.records {
				if record.BlockScan && record.Status != StatusFailed &&
					record.Status != StatusSwept {

					scanIDs = append(scanIDs, id)
				}
				if record.OutPoint != nil &&
					record.Status != StatusSwept &&
					record.Status != StatusSweeping &&
					record.Status != StatusFailed {

					ids = append(ids, id)
				}
			}
			s.mu.Unlock()
			for _, id := range scanIDs {
				if err := s.scanMatureBlocks(
					id, uint32(epoch.Height),
				); err != nil {

					s.handleRegistrationError(id, err)
				}
			}
			for _, id := range ids {
				if err := s.trySweep(id); err != nil {
					s.handleRegistrationError(id, err)
				}
			}

		case <-s.quit:
			return
		}
	}
}

func (s *Service) reconnectEpochs() *chainntnfs.BlockEpochEvent {
	backoff, maximum := s.retryBounds()
	for {
		select {
		case <-s.quit:
			return nil
		default:
		}

		event, err := s.cfg.Notifier.RegisterBlockEpochNtfn(nil)
		if err == nil {
			return event
		}

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-s.quit:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		}
		if backoff < maximum {
			backoff *= 2
			if backoff > maximum {
				backoff = maximum
			}
		}
	}
}

func (s *Service) trySweep(id RegistrationID) error {
	select {
	case <-s.quit:
		return nil
	default:
	}

	s.mu.Lock()
	record, ok := s.records[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if record.OutPoint == nil || record.Status == StatusSweeping ||
		record.Status == StatusSwept || record.Status == StatusFailed {

		s.mu.Unlock()
		return nil
	}

	desc, err := descriptors.NewDescriptor(record.CanonicalDescriptor)
	if err != nil {
		s.mu.Unlock()
		return deterministic(err)
	}

	// Prefer an immediately satisfiable branch without making any timeout
	// available. This makes a supplied preimage win over an already-mature
	// timeout even when the timeout witness is smaller. Only when no such
	// branch exists do we expose mature CLTV/CSV candidates to the planner.
	assets := makeAssets(record, 0)
	plan, err := desc.PlanAt(0, record.DerivationIndex, assets)
	if err != nil {
		assets = makeAssets(record, s.bestHeight)
		plan, err = desc.PlanAt(0, record.DerivationIndex, assets)
	}
	if err != nil {
		_, storeErr := s.updateRecordLocked(
			id, func(next *storedRecord) error {
				next.Status = StatusWaiting
				return nil
			},
		)
		s.mu.Unlock()
		return storeErr
	}

	constraints := plan.TxConstraints()
	if constraints.MinTxVersion > 2 {
		s.mu.Unlock()
		return deterministic(fmt.Errorf(
			"selected path requires transaction version %d",
			constraints.MinTxVersion,
		))
	}
	if constraints.AbsoluteLocktime != nil &&
		*constraints.AbsoluteLocktime >= txscript.LockTimeThreshold {

		s.mu.Unlock()
		return deterministic(errors.New("selected path has time-based CLTV"))
	}
	if constraints.RelativeLocktime != nil &&
		*constraints.RelativeLocktime&wire.SequenceLockTimeIsSeconds != 0 {

		s.mu.Unlock()
		return deterministic(errors.New("selected path has time-based CSV"))
	}

	// Freeze all witness material and the exact branch before giving the
	// input to UtxoSweeper. The only mutable object after this point is the
	// durable lifecycle record, never the input implementation.
	frozen := record.cloneForInput()
	bestHeight := s.bestHeight
	var deadline *int32
	if record.DeadlineDelta > 0 {
		if bestHeight > math.MaxInt32-record.DeadlineDelta {
			s.mu.Unlock()
			return deterministic(errors.New(
				"selected sweep deadline exceeds maximum block height",
			))
		}
		value := int32(bestHeight + record.DeadlineDelta)
		deadline = &value
	}
	next, err := s.updateRecordLocked(id, func(next *storedRecord) error {
		next.PlanLocktime = cloneUint32(constraints.AbsoluteLocktime)
		next.PlanSequence = cloneUint32(constraints.RelativeLocktime)
		next.PlanDeadlineHeight = cloneInt32(deadline)
		next.Status = StatusSweeping
		return nil
	})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	frozen = next.cloneForInput()
	s.mu.Unlock()

	inp, err := newDescriptorInput(desc, plan, frozen)
	if err != nil {
		return deterministic(err)
	}
	params := sweep.Params{
		Budget:    frozen.Budget,
		Immediate: frozen.Immediate,
	}
	if frozen.HasStartingFeeRate {
		params.StartingFeeRate = fn.Some(frozen.StartingFeeRate)
	}
	if frozen.PlanDeadlineHeight != nil {
		params.DeadlineHeight = fn.Some(*frozen.PlanDeadlineHeight)
	}

	result, err := s.cfg.Sweeper.SweepInput(inp, params)
	if err != nil {
		return retryablef("offer descriptor sweep input: %w", err)
	}

	s.launch(func() { s.consumeSweepResult(id, result) })
	return nil
}

func (s *Service) consumeSweepResult(id RegistrationID,
	result <-chan sweep.Result) {

	defer s.wg.Done()
	select {
	case sweepResult, ok := <-result:
		if !ok {
			s.handleRegistrationError(id, retryable(errors.New(
				"descriptor sweep result stream closed",
			)))
			return
		}
		var persistErr error
		if sweepResult.Err != nil {
			// A sweeper result can represent a transient publisher or
			// backend failure. Re-offer the exact frozen branch unless the
			// input was definitively spent by somebody else.
			if errors.Is(sweepResult.Err, sweep.ErrRemoteSpend) ||
				errors.Is(sweepResult.Err, sweep.ErrExclusiveGroupSpend) {

				s.failDurably(id, sweepResult.Err)
				return
			}
			persistErr = retryablef(
				"descriptor sweep result: %w", sweepResult.Err,
			)
		} else {
			persistSuccess := func() error {
				return s.persistTransition(id, func(next *storedRecord) error {
					next.Status = StatusSwept
					next.Error = ""
					if sweepResult.Tx != nil {
						txid := sweepResult.Tx.TxHash()
						next.SweepTxID = &txid
					}
					return nil
				})
			}
			persistErr = persistSuccess()
			if persistErr != nil {
				s.scheduleRetry(
					retryKey{id: id, kind: "persist-success"},
					persistSuccess, nil,
				)
				return
			}
		}
		if persistErr != nil {
			s.handleRegistrationError(id, persistErr)
		}

	case <-s.quit:
		return
	}
}

func makeAssets(record *storedRecord, bestHeight uint32) descriptors.Assets {
	availableKeys := make(map[string]struct{}, len(record.KeyBindings))
	for _, binding := range record.KeyBindings {
		availableKeys[binding.DescriptorKey] = struct{}{}
	}

	// Candidate lock values expose every currently mature branch to PlanAt.
	// The returned Plan.TxConstraints then freezes only the selected branch.
	var absolute, relative *uint32
	if bestHeight > 0 {
		absolute = &bestHeight
	}
	if record.ConfirmationHeight > 0 && bestHeight+1 >=
		record.ConfirmationHeight {

		value := bestHeight + 1 - record.ConfirmationHeight
		relative = &value
	}

	return descriptors.Assets{
		LookupEcdsaSig: func(key string) bool {
			_, ok := availableKeys[key]
			return ok
		},
		LookupPreimage: func(hashFunc string, hash []byte) bool {
			_, ok := record.Preimages[preimageKey(hashFunc, hash)]
			return ok
		},
		AbsoluteLocktime: absolute,
		RelativeLocktime: relative,
	}
}

func makeFrozenAssets(record *storedRecord) descriptors.Assets {
	assets := makeAssets(record, 0)
	assets.AbsoluteLocktime = cloneUint32(record.PlanLocktime)
	assets.RelativeLocktime = cloneUint32(record.PlanSequence)
	return assets
}

func sameOptionalUint32(a, b *uint32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func preimageKey(hashFunc string, hash []byte) string {
	return hashFunc + ":" + hex.EncodeToString(hash)
}

func (r *storedRecord) cloneForInput() *storedRecord {
	result := *r
	result.Record = *r.snapshot()
	result.Preimages = make(map[string][]byte, len(r.Preimages))
	for key, preimage := range r.Preimages {
		result.Preimages[key] = append([]byte(nil), preimage...)
	}
	result.PlanLocktime = cloneUint32(r.PlanLocktime)
	result.PlanSequence = cloneUint32(r.PlanSequence)
	result.PlanDeadlineHeight = cloneInt32(r.PlanDeadlineHeight)
	return &result
}

func cloneUint32(value *uint32) *uint32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneInt32(value *int32) *int32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type descriptorInput struct {
	op           wire.OutPoint
	signDesc     input.SignDescriptor
	witnessType  *descriptorWitnessType
	heightHint   uint32
	sequence     uint32
	locktime     *uint32
	confirmation uint32
	preimage     fn.Option[lntypes.Preimage]
}

func newDescriptorInput(desc *descriptors.Descriptor, plan *descriptors.Plan,
	record *storedRecord) (*descriptorInput, error) {

	if record.OutPoint == nil {
		return nil, errors.New("descriptor input has no outpoint")
	}
	witnessType, err := newDescriptorWitnessType(desc, plan, record)
	if err != nil {
		return nil, err
	}

	return &descriptorInput{
		op: *record.OutPoint,
		signDesc: input.SignDescriptor{
			WitnessScript: append([]byte(nil), record.WitnessScript...),
			Output: &wire.TxOut{
				Value:    record.Value,
				PkScript: append([]byte(nil), record.PkScript...),
			},
			HashType:   txscript.SigHashAll,
			SignMethod: input.WitnessV0SignMethod,
		},
		witnessType:  witnessType,
		heightHint:   record.ConfirmationHeight,
		sequence:     valueOrZero(record.PlanSequence),
		locktime:     cloneUint32(record.PlanLocktime),
		confirmation: record.ConfirmationHeight,
		preimage:     firstPreimage(record.Preimages),
	}, nil
}

func firstPreimage(preimages map[string][]byte) fn.Option[lntypes.Preimage] {
	for _, raw := range preimages {
		var preimage lntypes.Preimage
		copy(preimage[:], raw)
		return fn.Some(preimage)
	}
	return fn.None[lntypes.Preimage]()
}

func valueOrZero(value *uint32) uint32 {
	if value == nil {
		return 0
	}
	return *value
}

func (i *descriptorInput) OutPoint() wire.OutPoint    { return i.op }
func (i *descriptorInput) RequiredTxOut() *wire.TxOut { return nil }
func (i *descriptorInput) RequiredLockTime() (uint32, bool) {
	if i.locktime == nil {
		return 0, false
	}
	return *i.locktime, true
}
func (i *descriptorInput) WitnessType() input.WitnessType  { return i.witnessType }
func (i *descriptorInput) SignDesc() *input.SignDescriptor { return &i.signDesc }
func (i *descriptorInput) CraftInputScript(signer input.Signer,
	tx *wire.MsgTx, hashes *txscript.TxSigHashes,
	fetcher txscript.PrevOutputFetcher, index int) (*input.Script, error) {

	i.signDesc.SigHashes = hashes
	i.signDesc.PrevOutputFetcher = fetcher
	i.signDesc.InputIndex = index
	return i.witnessType.craft(signer, &i.signDesc, tx, index)
}
func (i *descriptorInput) BlocksToMaturity() uint32    { return i.sequence }
func (i *descriptorInput) HeightHint() uint32          { return i.heightHint }
func (i *descriptorInput) UnconfParent() *input.TxInfo { return nil }
func (i *descriptorInput) ResolutionBlob() fn.Option[tlv.Blob] {
	return fn.None[tlv.Blob]()
}
func (i *descriptorInput) Preimage() fn.Option[lntypes.Preimage] {
	return i.preimage
}

type descriptorWitnessType struct {
	desc          *descriptors.Descriptor
	plan          *descriptors.Plan
	bindings      map[string]keychain.KeyDescriptor
	preimages     map[string][]byte
	witnessScript []byte
	witnessSize   lntypes.WeightUnit
}

// IsWitnessType reports whether a sweeper witness belongs to this service.
// WalletKit uses this to render custom pending inputs without failing its
// standard witness enum conversion.
func IsWitnessType(witness input.WitnessType) bool {
	_, ok := witness.(*descriptorWitnessType)
	return ok
}

func newDescriptorWitnessType(desc *descriptors.Descriptor,
	plan *descriptors.Plan, record *storedRecord) (*descriptorWitnessType, error) {

	bindings := make(map[string]keychain.KeyDescriptor, len(record.KeyBindings))
	for _, binding := range record.KeyBindings {
		pubKeyBytes, err := descriptorPubKey(binding.DescriptorKey)
		if err != nil {
			return nil, err
		}
		pubKey, err := btcec.ParsePubKey(pubKeyBytes)
		if err != nil {
			return nil, err
		}
		bindings[binding.DescriptorKey] = keychain.KeyDescriptor{
			KeyLocator: binding.KeyLocator,
			PubKey:     pubKey,
		}
	}

	maxWeight, err := desc.MaxWeightToSatisfy()
	if err != nil {
		return nil, err
	}
	return &descriptorWitnessType{
		desc:          desc,
		plan:          plan,
		bindings:      bindings,
		preimages:     record.Preimages,
		witnessScript: append([]byte(nil), record.WitnessScript...),
		// MaxWeightToSatisfy is relative to an empty witness. lnd expects
		// the complete serialized witness, including the element-count byte.
		witnessSize: lntypes.WeightUnit(maxWeight + 1),
	}, nil
}

func (w *descriptorWitnessType) String() string { return "descriptor-wsh" }
func (w *descriptorWitnessType) WitnessGenerator(signer input.Signer,
	desc *input.SignDescriptor) input.WitnessGenerator {

	return func(tx *wire.MsgTx, _ *txscript.TxSigHashes,
		index int) (*input.Script, error) {

		return w.craft(signer, desc, tx, index)
	}
}
func (w *descriptorWitnessType) SizeUpperBound() (lntypes.WeightUnit, bool,
	error) {

	return w.witnessSize, false, nil
}
func (w *descriptorWitnessType) AddWeightEstimation(
	estimator *input.TxWeightEstimator) error {

	estimator.AddWitnessInput(w.witnessSize)
	return nil
}

func (w *descriptorWitnessType) craft(signer input.Signer,
	signDesc *input.SignDescriptor, tx *wire.MsgTx,
	index int) (*input.Script, error) {

	satisfier := &descriptors.Satisfier{
		LookupEcdsaSig: func(key string) ([]byte, bool) {
			keyDesc, ok := w.bindings[key]
			if !ok {
				return nil, false
			}
			local := *signDesc
			local.KeyDesc = keyDesc
			local.InputIndex = index
			signature, err := signer.SignOutputRaw(tx, &local)
			if err != nil {
				return nil, false
			}
			serialized := signature.Serialize()
			serialized = append(serialized, byte(local.HashType))
			return serialized, true
		},
		LookupPreimage: func(hashFunc string, hash []byte) ([]byte, bool) {
			preimage, ok := w.preimages[preimageKey(hashFunc, hash)]
			return append([]byte(nil), preimage...), ok
		},
	}

	result, err := w.plan.Satisfy(satisfier)
	if err != nil {
		return nil, err
	}
	witness := append(wire.TxWitness{}, result.Witness...)
	witness = append(witness, append([]byte(nil), w.witnessScript...))

	return &input.Script{
		Witness:   witness,
		SigScript: result.ScriptSig,
	}, nil
}

var _ input.Input = (*descriptorInput)(nil)
var _ input.WitnessType = (*descriptorWitnessType)(nil)

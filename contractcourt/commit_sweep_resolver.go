package contractcourt

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
)

const (
	// commitOutputConfTarget is the default confirmation target we'll use
	// for sweeps of commit outputs that belong to us.
	commitOutputConfTarget = 6
)

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
			return nil, errResolverShuttingDown
		}

	case <-c.Quit:
		return nil, errResolverShuttingDown
	}

	// We're dealing with our commitment transaction if the delay on the
	// resolution isn't zero.
	isLocalCommitTx := c.commitResolution.MaturityDelay != 0

	if !isLocalCommitTx {
		// There're two types of commitments, those that have tweaks
		// for the remote key (us in this case), and those that don't.
		// We'll rely on the presence of the commitment tweak to to
		// discern which type of commitment this is.
		var witnessType input.WitnessType
		if c.commitResolution.SelfOutputSignDesc.SingleTweak == nil {
			witnessType = input.CommitSpendNoDelayTweakless
		} else {
			witnessType = input.CommitmentNoDelay
		}

		// We'll craft an input with all the information required for
		// the sweeper to create a fully valid sweeping transaction to
		// recover these coins.
		inp := input.MakeBaseInput(
			&c.commitResolution.SelfOutPoint,
			witnessType,
			&c.commitResolution.SelfOutputSignDesc,
			c.broadcastHeight,
		)

		// With our input constructed, we'll now offer it to the
		// sweeper.
		log.Infof("%T(%v): sweeping commit output", c, c.chanPoint)

		feePref := sweep.FeePreference{ConfTarget: commitOutputConfTarget}
		resultChan, err := c.Sweeper.SweepInput(&inp, feePref)
		if err != nil {
			log.Errorf("%T(%v): unable to sweep input: %v",
				c, c.chanPoint, err)

			return nil, err
		}

		// Sweeper is going to join this input with other inputs if
		// possible and publish the sweep tx. When the sweep tx
		// confirms, it signals us through the result channel with the
		// outcome. Wait for this to happen.
		select {
		case sweepResult := <-resultChan:
			if sweepResult.Err != nil {
				log.Errorf("%T(%v): unable to sweep input: %v",
					c, c.chanPoint, sweepResult.Err)

				return nil, sweepResult.Err
			}

			log.Infof("ChannelPoint(%v) commit tx is fully resolved by "+
				"sweep tx: %v", c.chanPoint, sweepResult.Tx.TxHash())
		case <-c.Quit:
			return nil, errResolverShuttingDown
		}

		c.resolved = true
		return nil, c.Checkpoint(c)
	}

	// Otherwise we are dealing with a local commitment transaction and the
	// output we need to sweep has been sent to the nursery for incubation.
	// In this case, we'll wait until the commitment output has been spent.
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

	var sweepTx *wire.MsgTx
	select {
	case commitSpend, ok := <-spendNtfn.Spend:
		if !ok {
			return nil, errResolverShuttingDown
		}

		// Once we detect the commitment output has been spent,
		// we'll extract the spending transaction itself, as we
		// now consider this to be our sweep transaction.
		sweepTx = commitSpend.SpendingTx

		log.Infof("%T(%v): commit output swept by txid=%v",
			c, c.chanPoint, sweepTx.TxHash())

		if err := c.Checkpoint(c); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return nil, err
		}
	case <-c.Quit:
		return nil, errResolverShuttingDown
	}

	log.Infof("%T(%v): waiting for commit sweep txid=%v conf", c, c.chanPoint,
		sweepTx.TxHash())

	// Now we'll wait until the sweeping transaction has been fully
	// confirmed.  Once it's confirmed, we can mark this contract resolved.
	sweepTXID := sweepTx.TxHash()
	sweepingScript := sweepTx.TxOut[0].PkScript
	confNtfn, err = c.Notifier.RegisterConfirmationsNtfn(
		&sweepTXID, sweepingScript, 1, c.broadcastHeight,
	)
	if err != nil {
		return nil, err
	}
	select {
	case confInfo, ok := <-confNtfn.Confirmed:
		if !ok {
			return nil, errResolverShuttingDown
		}

		log.Infof("ChannelPoint(%v) commit tx is fully resolved, at height: %v",
			c.chanPoint, confInfo.BlockHeight)

	case <-c.Quit:
		return nil, errResolverShuttingDown
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

	// Previously a sweep tx was serialized at this point. Refactoring
	// removed this, but keep in mind that this data may still be present in
	// the database.

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

	// Previously a sweep tx was deserialized at this point. Refactoring
	// removed this, but keep in mind that this data may still be present in
	// the database.

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

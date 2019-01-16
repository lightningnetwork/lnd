package contractcourt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
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
			[]sweep.Input{&input},
			sweep.FeePreference{
				ConfTarget: sweepConfTarget,
			}, 0,
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

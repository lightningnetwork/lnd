package funding

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
)

// PublisherCfg is used to configure the Publisher.
type PublisherCfg struct {
	// TxSanityCheck is a context free, static check that verifies some
	// basic transaction rules. If an error is returned, it should mean that
	// transaction would fail consensus rules.
	TxSanityCheck func(tx *wire.MsgTx) error

	// TxStandardnessCheck checks the transaction against some of the main,
	// static standardness rules. It is a context check. Failing this check
	// means that the transaction would likely fail policy rules but not
	// consensus.
	TxStandardnessCheck func(tx *wire.MsgTx) error

	// TestMempoolAccept can be set if the chain backend has a mempool
	// available. This check uses the context of the current mempool and
	// UTXO set.
	TestMempoolAccept func(*wire.MsgTx) error

	// Publish will be used to broadcast the transaction to the network
	// if it passes all the prior checks.
	Publish func(*wire.MsgTx, string) error
}

// Publisher handles checking the validity of a transaction in various ways
// before attempting to broadcast it. It wraps the errors for each check so
// that decisions can be made based on the type of check that the transaction
// may have failed on.
type Publisher struct {
	cfg *PublisherCfg
}

// NewPublisher constructs a new Publisher instance given the passed config.
func NewPublisher(cfg *PublisherCfg) *Publisher {
	return &Publisher{
		cfg: cfg,
	}
}

// CheckAndPublish does various checks of the transaction before attempting
// to publish it.
func (p *Publisher) CheckAndPublish(tx *wire.MsgTx, label string) error {
	// First, we do a static sanity check of the transaction. If this fails,
	// the transaction would not pass consensus rules.
	if p.cfg.TxSanityCheck != nil {
		err := p.cfg.TxSanityCheck(tx)
		if err != nil {
			return &ErrSanity{err}
		}
	}

	// Next, we do a static standardness check of the transaction. If this
	// fails, we can assume that it will be rejected by most mempools but
	// there is still a chance that the transaction will eventually make it
	// onto the blockchain if it was externally published to a mining
	// accelerator. So we can only safely forget about this transaction if
	// we know that it was constructed internally.
	if p.cfg.TxStandardnessCheck != nil {
		err := p.cfg.TxStandardnessCheck(tx)
		if err != nil {
			return &ErrStandardness{err}
		}
	}

	// Now, if our chain backend has a mempool instance, we can use it to
	// check if the transaction would be accepted. An error here doesn't
	// necessarily mean that the transaction would not be accepted to other
	// mempools though (for example, maybe the parent of the transaction has
	// been evicted from this mempool but not others). If we get an error
	// here, we can only safely forget about this transaction if we know
	// that it was constructed internally.
	if p.cfg.TestMempoolAccept != nil {
		err := p.cfg.TestMempoolAccept(tx)
		if err != nil {
			return &ErrMempoolTestAccept{err}
		}
	}

	// Finally, we can attempt to broadcast this transaction. If we get an
	// error here even after performing all the prior sanity checks, then
	// something strange is happening. We should continue to monitor this
	// transaction and its inputs.
	err := p.cfg.Publish(tx, label)
	if err != nil {
		return &ErrPublish{err}
	}

	return nil
}

// ErrSanity is the error returned if the sanity check failed.
type ErrSanity struct {
	err error
}

func (e *ErrSanity) Error() string {
	return fmt.Sprintf("tx sanity check error: %v", e.err)
}

// Unwrap returns the underlying error returned from the sanity check function.
func (e *ErrSanity) Unwrap() error {
	return e.err
}

// ErrStandardness is the error returned if the standardness check failed.
type ErrStandardness struct {
	err error
}

func (e *ErrStandardness) Error() string {
	return fmt.Sprintf("tx standardness check error: %v", e.err)
}

// Unwrap returns the underlying error returned from the standardness check
// function.
func (e *ErrStandardness) Unwrap() error {
	return e.err
}

// ErrMempoolTestAccept is the error returned if the testmempoolaccept check
// failed.
type ErrMempoolTestAccept struct {
	err error
}

func (e *ErrMempoolTestAccept) Error() string {
	return fmt.Sprintf("test mempool accept error: %v", e.err)
}

// Unwrap returns the underlying error returned from the testmempoolaccept
// check.
func (e *ErrMempoolTestAccept) Unwrap() error {
	return e.err
}

// ErrPublish is the error returned if the tx broadcast failed.
type ErrPublish struct {
	err error
}

func (e *ErrPublish) Error() string {
	return fmt.Sprintf("tx publish error: %v", e.err)
}

// Unwrap returns the underlying error returned from the publish call.
func (e *ErrPublish) Unwrap() error {
	return e.err
}

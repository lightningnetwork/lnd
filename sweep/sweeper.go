package sweep

import (
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// UtxoSweeper provides the functionality to generate sweep txes. The plan is
// to extend UtxoSweeper in the future to also manage the actual sweeping
// process by itself.
type UtxoSweeper struct {
	cfg *UtxoSweeperConfig
}

// UtxoSweeperConfig contains dependencies of UtxoSweeper.
type UtxoSweeperConfig struct {
	// GenSweepScript generates a P2WKH script belonging to the wallet
	// where funds can be swept.
	GenSweepScript func() ([]byte, error)

	// Estimator is used when crafting sweep transactions to estimate the
	// necessary fee relative to the expected size of the sweep
	// transaction.
	Estimator lnwallet.FeeEstimator

	// Signer is used by the sweeper to generate valid witnesses at the
	// time the incubated outputs need to be spent.
	Signer lnwallet.Signer
}

// New returns a new UtxoSweeper instance.
func New(cfg *UtxoSweeperConfig) *UtxoSweeper {
	return &UtxoSweeper{
		cfg: cfg,
	}
}

// CreateSweepTx accepts a list of inputs and signs and generates a txn that
// spends from them. This method also makes an accurate fee estimate before
// generating the required witnesses.
//
// The created transaction has a single output sending all the funds back to
// the source wallet, after accounting for the fee estimate.
//
// The value of currentBlockHeight argument will be set as the tx locktime.
// This function assumes that all CLTV inputs will be unlocked after
// currentBlockHeight. Reasons not to use the maximum of all actual CLTV expiry
// values of the inputs:
//
// - Make handling re-orgs easier.
// - Thwart future possible fee sniping attempts.
// - Make us blend in with the bitcoind wallet.
func (s *UtxoSweeper) CreateSweepTx(inputs []Input, confTarget uint32,
	currentBlockHeight uint32) (*wire.MsgTx, error) {

	// Generate the receiving script to which the funds will be swept.
	pkScript, err := s.cfg.GenSweepScript()
	if err != nil {
		return nil, err
	}

	// Using the txn weight estimate, compute the required txn fee.
	feePerKw, err := s.cfg.Estimator.EstimateFeePerKW(confTarget)
	if err != nil {
		return nil, err
	}

	return createSweepTx(
		inputs, pkScript, currentBlockHeight, feePerKw, s.cfg.Signer,
	)
}

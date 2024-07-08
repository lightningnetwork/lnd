package blob

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// CommitmentType characterises the various properties of the breach commitment
// transaction.
type CommitmentType uint8

const (
	// LegacyCommitment represents a legacy commitment transaction where
	// anchor outputs are not yet used and so the to_remote output is just
	// a regular but tweaked P2WKH.
	LegacyCommitment CommitmentType = iota

	// LegacyTweaklessCommitment is similar to the LegacyCommitment with the
	// added detail of the to_remote output not being tweaked.
	LegacyTweaklessCommitment

	// AnchorCommitment represents the commitment transaction of an
	// anchor channel. The key differences are that the to_remote is
	// encumbered by a 1 block CSV and so is thus a P2WSH output.
	AnchorCommitment

	// TaprootCommitment represents the commitment transaction of a simple
	// taproot channel.
	TaprootCommitment
)

// ToLocalInput constructs the input that will be used to spend the to_local
// output.
func (c CommitmentType) ToLocalInput(info *lnwallet.BreachRetribution) (
	input.Input, error) {

	witnessType, err := c.ToLocalWitnessType()
	if err != nil {
		return nil, err
	}

	return input.NewBaseInput(
		&info.RemoteOutpoint, witnessType, info.RemoteOutputSignDesc, 0,
	), nil
}

// ToRemoteInput constructs the input that will be used to spend the to_remote
// output.
func (c CommitmentType) ToRemoteInput(info *lnwallet.BreachRetribution) (
	input.Input, error) {

	witnessType, err := c.ToRemoteWitnessType()
	if err != nil {
		return nil, err
	}

	switch c {
	case LegacyCommitment, LegacyTweaklessCommitment:
		return input.NewBaseInput(
			&info.LocalOutpoint, witnessType,
			info.LocalOutputSignDesc, 0,
		), nil

	case AnchorCommitment, TaprootCommitment:
		// Anchor and Taproot channels have a CSV-encumbered to-remote
		// output. We'll construct a CSV input and assign the proper CSV
		// delay of 1.
		return input.NewCsvInput(
			&info.LocalOutpoint, witnessType,
			info.LocalOutputSignDesc, 0, 1,
		), nil

	default:
		return nil, fmt.Errorf("unknown commitment type: %v", c)
	}
}

// ToLocalWitnessType is the input type of the to_local output.
func (c CommitmentType) ToLocalWitnessType() (input.WitnessType, error) {
	switch c {
	case LegacyTweaklessCommitment, LegacyCommitment, AnchorCommitment:
		return input.CommitmentRevoke, nil

	case TaprootCommitment:
		return input.TaprootCommitmentRevoke, nil

	default:
		return nil, fmt.Errorf("unknown commitment type: %v", c)
	}
}

// ToRemoteWitnessType is the input type of the to_remote output.
func (c CommitmentType) ToRemoteWitnessType() (input.WitnessType, error) {
	switch c {
	case LegacyTweaklessCommitment:
		return input.CommitSpendNoDelayTweakless, nil

	case LegacyCommitment:
		return input.CommitmentNoDelay, nil

	case AnchorCommitment:
		return input.CommitmentToRemoteConfirmed, nil

	case TaprootCommitment:
		return input.TaprootRemoteCommitSpend, nil

	default:
		return nil, fmt.Errorf("unknown commitment type: %v", c)
	}
}

// ToRemoteWitnessSize is the size of the witness that will be required to spend
// the to_remote output.
func (c CommitmentType) ToRemoteWitnessSize() (lntypes.WeightUnit, error) {
	switch c {
	// Legacy channels (both tweaked and non-tweaked) spend from P2WKH
	// output.
	case LegacyTweaklessCommitment, LegacyCommitment:
		return input.P2WKHWitnessSize, nil

	// Anchor channels spend a to-remote confirmed P2WSH output.
	case AnchorCommitment:
		return input.ToRemoteConfirmedWitnessSize, nil

	// Taproot channels spend a confirmed P2SH output.
	case TaprootCommitment:
		return input.TaprootToRemoteWitnessSize, nil

	default:
		return 0, fmt.Errorf("unknown commitment type: %v", c)
	}
}

// ToLocalWitnessSize is the size of the witness that will be required to spend
// the to_local output.
func (c CommitmentType) ToLocalWitnessSize() (lntypes.WeightUnit, error) {
	switch c {
	// An older ToLocalPenaltyWitnessSize constant used to underestimate the
	// size by one byte. The difference in weight can cause different output
	// values on the sweep transaction, so we mimic the original bug and
	// create signatures using the original weight estimate.
	case LegacyTweaklessCommitment, LegacyCommitment:
		return input.ToLocalPenaltyWitnessSize - 1, nil

	case AnchorCommitment:
		return input.ToLocalPenaltyWitnessSize, nil

	case TaprootCommitment:
		return input.TaprootToLocalRevokeWitnessSize, nil

	default:
		return 0, fmt.Errorf("unknown commitment type: %v", c)
	}
}

// ParseRawSig parses a wire.TxWitness and creates an lnwire.Sig.
func (c CommitmentType) ParseRawSig(witness wire.TxWitness) (lnwire.Sig,
	error) {

	// Check that the witness has at least one item since this is required
	// for all commitment types to follow.
	if len(witness) < 1 {
		return lnwire.Sig{}, fmt.Errorf("the witness should have at " +
			"least one element")
	}

	// Check that the first witness element is non-nil. This is to ensure
	// that the witness length checks below do not panic.
	if witness[0] == nil {
		return lnwire.Sig{}, fmt.Errorf("the first witness element " +
			"should not be nil")
	}

	switch c {
	case LegacyCommitment, LegacyTweaklessCommitment, AnchorCommitment:
		// Parse the DER-encoded signature from the first position of
		// the resulting witness. We trim an extra byte to remove the
		// sighash flag.
		rawSignature := witness[0][:len(witness[0])-1]

		// Re-encode the DER signature into a fixed-size 64 byte
		// signature.
		return lnwire.NewSigFromECDSARawSignature(rawSignature)

	case TaprootCommitment:
		rawSignature := witness[0]
		if len(rawSignature) > 64 {
			rawSignature = witness[0][:len(witness[0])-1]
		}

		// Re-encode the schnorr signature into a fixed-size 64 byte
		// signature.
		return lnwire.NewSigFromSchnorrRawSignature(rawSignature)

	default:
		return lnwire.Sig{}, fmt.Errorf("unknown commitment type: %v",
			c)
	}
}

// NewJusticeKit can be used to construct a new JusticeKit depending on the
// CommitmentType.
func (c CommitmentType) NewJusticeKit(sweepScript []byte,
	breachInfo *lnwallet.BreachRetribution, withToRemote bool) (JusticeKit,
	error) {

	switch c {
	case LegacyCommitment, LegacyTweaklessCommitment:
		return newLegacyJusticeKit(
			sweepScript, breachInfo, withToRemote,
		), nil

	case AnchorCommitment:
		return newAnchorJusticeKit(
			sweepScript, breachInfo, withToRemote,
		), nil

	case TaprootCommitment:
		return newTaprootJusticeKit(
			sweepScript, breachInfo, withToRemote,
		)

	default:
		return nil, fmt.Errorf("unknown commitment type: %v", c)
	}
}

// EmptyJusticeKit returns the appropriate empty justice kit for the given
// CommitmentType.
func (c CommitmentType) EmptyJusticeKit() (JusticeKit, error) {
	switch c {
	case LegacyTweaklessCommitment, LegacyCommitment:
		return &legacyJusticeKit{}, nil

	case AnchorCommitment:
		return &anchorJusticeKit{
			legacyJusticeKit: legacyJusticeKit{},
		}, nil

	case TaprootCommitment:
		return &taprootJusticeKit{}, nil

	default:
		return nil, fmt.Errorf("unknown commitment type: %v", c)
	}
}

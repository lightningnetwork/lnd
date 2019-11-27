package lnwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

type ScriptInfo struct {
	WitnessScript []byte
	PkScript      []byte
}

type CommitmentType interface {
	CommitScriptToRemote(csvTimeout uint32, key *btcec.PublicKey) ([]byte, []byte, error)

	// TODO: can return nothing
	CommitScriptAnchor(key *btcec.PublicKey) ([]ScriptInfo, error)

	CommitWeight() int64

	CommitFee(feePerKw chainfee.SatPerKWeight, numHtlcs int64) btcutil.Amount

	HtlcSigHash() txscript.SigHashType
}

func CommitmentFromChanType(t channeldb.ChannelType) (CommitmentType, error) {
	switch t {
	case channeldb.SingleFunderBit:
		return &SingleFunder{}, nil
	}

	return nil, fmt.Errorf("unknown type")
}

type SingleFunder struct{}

var _ CommitmentType = (*SingleFunder)(nil)

func (s *SingleFunder) CommitScriptToRemote(csvTimeout uint32, key *btcec.PublicKey) ([]byte, []byte, error) {
	pkScript, err := input.CommitScriptUnencumbered(key)
	if err != nil {
		return nil, nil, err
	}

	return pkScript, pkScript, nil

}

func (s *SingleFunder) CommitScriptAnchor(key *btcec.PublicKey) ([]ScriptInfo, error) {
	return nil, nil
}

func (s *SingleFunder) CommitWeight() int64 {
	return 0
}

func (s *SingleFunder) CommitFee(feePerKw chainfee.SatPerKWeight, numHtlcs int64) btcutil.Amount {
	return 0
}

func (s *SingleFunder) HtlcSigHash() txscript.SigHashType {
	return txscript.SigHashAll
}

type Anchor struct{}

var _ CommitmentType = (*Anchor)(nil)

func (a *Anchor) CommitScriptToRemote(csvTimeout uint32, key *btcec.PublicKey) ([]byte, []byte, error) {
	pkScript, err := input.CommitScriptToRemote(
		csvTimeout, key,
	)
	if err != nil {
		return nil, nil, err
	}
	scriptHash, err := input.WitnessScriptHash(pkScript)
	if err != nil {
		return nil, nil, err
	}

	return pkScript, scriptHash, nil
}

func (a *Anchor) CommitScriptAnchor(key *btcec.PublicKey) ([]ScriptInfo, error) {
	// Similarly, the anchor going to us will be the anchor output
	// spendable by the remote anchor key.
	localAnchorScript, err := input.CommitScriptAnchor(
		key,
	)
	if err != nil {
		return nil, err
	}

	localAnchorScriptHash, err := input.WitnessScriptHash(localAnchorScript)
	if err != nil {
		return nil, err
	}

	return []ScriptInfo{
		{
			WitnessScript: localAnchorScript,
			PkScript:      localAnchorScriptHash,
		},
	}, nil
}

func (a *Anchor) CommitWeight() int64 {
	return 0
}

func (a *Anchor) CommitFee(feePerKw chainfee.SatPerKWeight, numHtlcs int64) btcutil.Amount {
	totalCommitWeight := input.CommitWeight + (input.HTLCWeight * numHtlcs)
	commitFee := feePerKw.FeeForWeight(totalCommitWeight)
	return commitFee
}

func (a *Anchor) HtlcSigHash() txscript.SigHashType {
	return txscript.SigHashSingle | txscript.SigHashAnyOneCanPay
}

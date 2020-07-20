package chanfunding

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/psbt"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// PsbtState is a type for the state of the PSBT intent state machine.
type PsbtState uint8

const (
	// PsbtShimRegistered denotes a channel funding process has started with
	// a PSBT shim attached. This is the default state for a PsbtIntent. We
	// don't use iota here because the values have to be in sync with the
	// RPC constants.
	PsbtShimRegistered PsbtState = 1

	// PsbtOutputKnown denotes that the local and remote peer have
	// negotiated the multisig keys to be used as the channel funding output
	// and therefore the PSBT funding process can now start.
	PsbtOutputKnown PsbtState = 2

	// PsbtVerified denotes that a potential PSBT has been presented to the
	// intent and passed all checks. The verified PSBT can be given to a/the
	// signer(s).
	PsbtVerified PsbtState = 3

	// PsbtFinalized denotes that a fully signed PSBT has been given to the
	// intent that looks identical to the previously verified transaction
	// but has all witness data added and is therefore completely signed.
	PsbtFinalized PsbtState = 4

	// PsbtFundingTxCompiled denotes that the PSBT processed by this intent
	// has been successfully converted into a protocol transaction. It is
	// not yet completely certain that the resulting transaction will be
	// published because the commitment transactions between the channel
	// peers first need to be counter signed. But the job of the intent is
	// hereby completed.
	PsbtFundingTxCompiled PsbtState = 5

	// PsbtInitiatorCanceled denotes that the user has canceled the intent.
	PsbtInitiatorCanceled PsbtState = 6

	// PsbtResponderCanceled denotes that the remote peer has canceled the
	// funding, likely due to a timeout.
	PsbtResponderCanceled PsbtState = 7
)

// String returns a string representation of the PsbtState.
func (s PsbtState) String() string {
	switch s {
	case PsbtShimRegistered:
		return "shim_registered"

	case PsbtOutputKnown:
		return "output_known"

	case PsbtVerified:
		return "verified"

	case PsbtFinalized:
		return "finalized"

	case PsbtFundingTxCompiled:
		return "funding_tx_compiled"

	case PsbtInitiatorCanceled:
		return "user_canceled"

	case PsbtResponderCanceled:
		return "remote_canceled"

	default:
		return fmt.Sprintf("<unknown(%d)>", s)
	}
}

var (
	// ErrRemoteCanceled is the error that is returned to the user if the
	// funding flow was canceled by the remote peer.
	ErrRemoteCanceled = errors.New("remote canceled funding, possibly " +
		"timed out")

	// ErrUserCanceled is the error that is returned through the PsbtReady
	// channel if the user canceled the funding flow.
	ErrUserCanceled = errors.New("user canceled funding")
)

// PsbtIntent is an intent created by the PsbtAssembler which represents a
// funding output to be created by a PSBT. This might be used when a hardware
// wallet, or a channel factory is the entity crafting the funding transaction,
// and not lnd.
type PsbtIntent struct {
	// ShimIntent is the wrapped basic intent that contains common fields
	// we also use in the PSBT funding case.
	ShimIntent

	// State is the current state the intent state machine is in.
	State PsbtState

	// BasePsbt is the user-supplied base PSBT the channel output should be
	// added to. If this is nil we will create a new, empty PSBT as the base
	// for the funding transaction.
	BasePsbt *psbt.Packet

	// PendingPsbt is the parsed version of the current PSBT. This can be
	// in two stages: If the user has not yet provided any PSBT, this is
	// nil. Once the user sends us an unsigned funded PSBT, we verify that
	// we have a valid transaction that sends to the channel output PK
	// script and has an input large enough to pay for it. We keep this
	// verified but not yet signed version around until the fully signed
	// transaction is submitted by the user. At that point we make sure the
	// inputs and outputs haven't changed to what was previously verified.
	// Only witness data should be added after the verification process.
	PendingPsbt *psbt.Packet

	// PsbtReady is an error channel the funding manager will listen for
	// a signal about the PSBT being ready to continue the funding flow. In
	// the normal, happy flow, this channel is only ever closed. If a
	// non-nil error is sent through the channel, the funding flow will be
	// canceled.
	//
	// NOTE: This channel must always be buffered.
	PsbtReady chan error

	// signalPsbtReady is a Once guard to make sure the PsbtReady channel is
	// only closed exactly once.
	signalPsbtReady sync.Once

	// netParams are the network parameters used to encode the P2WSH funding
	// address.
	netParams *chaincfg.Params
}

// BindKeys sets both the remote and local node's keys that will be used for the
// channel funding multisig output.
func (i *PsbtIntent) BindKeys(localKey *keychain.KeyDescriptor,
	remoteKey *btcec.PublicKey) {

	i.localKey = localKey
	i.remoteKey = remoteKey
	i.State = PsbtOutputKnown
}

// FundingParams returns the parameters that are necessary to start funding the
// channel output this intent was created for. It returns the P2WSH funding
// address, the exact funding amount and a PSBT packet that contains exactly one
// output that encodes the previous two parameters.
func (i *PsbtIntent) FundingParams() (btcutil.Address, int64, *psbt.Packet,
	error) {

	if i.State != PsbtOutputKnown {
		return nil, 0, nil, fmt.Errorf("invalid state, got %v "+
			"expected %v", i.State, PsbtOutputKnown)
	}

	// The funding output needs to be known already at this point, which
	// means we need to have the local and remote multisig keys bound
	// already.
	witnessScript, out, err := i.FundingOutput()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("unable to create funding "+
			"output: %v", err)
	}
	witnessScriptHash := sha256.Sum256(witnessScript)

	// Encode the address in the human readable bech32 format.
	addr, err := btcutil.NewAddressWitnessScriptHash(
		witnessScriptHash[:], i.netParams,
	)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("unable to encode address: %v",
			err)
	}

	// We'll also encode the address/amount in a machine readable raw PSBT
	// format. If the user supplied a base PSBT, we'll add the output to
	// that one, otherwise we'll create a new one.
	packet := i.BasePsbt
	if packet == nil {
		packet, err = psbt.New(nil, nil, 2, 0, nil)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("unable to create "+
				"PSBT: %v", err)
		}
	}
	packet.UnsignedTx.TxOut = append(packet.UnsignedTx.TxOut, out)
	packet.Outputs = append(packet.Outputs, psbt.POutput{})
	return addr, out.Value, packet, nil
}

// Verify makes sure the PSBT that is given to the intent has an output that
// sends to the channel funding multisig address with the correct amount. A
// simple check that at least a single input has been specified is performed.
func (i *PsbtIntent) Verify(packet *psbt.Packet) error {
	if packet == nil {
		return fmt.Errorf("PSBT is nil")
	}
	if i.State != PsbtOutputKnown {
		return fmt.Errorf("invalid state. got %v expected %v", i.State,
			PsbtOutputKnown)
	}

	// Try to locate the channel funding multisig output.
	_, expectedOutput, err := i.FundingOutput()
	if err != nil {
		return fmt.Errorf("funding output cannot be created: %v", err)
	}
	outputFound := false
	outputSum := int64(0)
	for _, out := range packet.UnsignedTx.TxOut {
		outputSum += out.Value
		if txOutsEqual(out, expectedOutput) {
			outputFound = true
		}
	}
	if !outputFound {
		return fmt.Errorf("funding output not found in PSBT")
	}

	// At least one input needs to be specified and it must be large enough
	// to pay for all outputs. We don't want to dive into fee estimation
	// here so we just assume that if the input amount exceeds the output
	// amount, the chosen fee is sufficient.
	if len(packet.UnsignedTx.TxIn) == 0 {
		return fmt.Errorf("PSBT has no inputs")
	}
	sum, err := sumUtxoInputValues(packet)
	if err != nil {
		return fmt.Errorf("error determining input sum: %v", err)
	}
	if sum <= outputSum {
		return fmt.Errorf("input amount sum must be larger than " +
			"output amount sum")
	}

	i.PendingPsbt = packet
	i.State = PsbtVerified
	return nil
}

// Finalize makes sure the final PSBT that is given to the intent is fully valid
// and signed but still contains the same UTXOs and outputs as the pending
// transaction we previously verified. If everything checks out, the funding
// manager is informed that the channel can now be opened and the funding
// transaction be broadcast.
func (i *PsbtIntent) Finalize(packet *psbt.Packet) error {
	if packet == nil {
		return fmt.Errorf("PSBT is nil")
	}
	if i.State != PsbtVerified {
		return fmt.Errorf("invalid state. got %v expected %v", i.State,
			PsbtVerified)
	}

	// Make sure the PSBT itself thinks it's finalized and ready to be
	// broadcast.
	err := psbt.MaybeFinalizeAll(packet)
	if err != nil {
		return fmt.Errorf("error finalizing PSBT: %v", err)
	}
	_, err = psbt.Extract(packet)
	if err != nil {
		return fmt.Errorf("unable to extract funding TX: %v", err)
	}

	// Do a basic check that this is still the same PSBT that we verified in
	// the previous step. This is to protect the user from unwanted
	// modifications. We only check the outputs and previous outpoints of
	// the inputs of the wire transaction because the fields in the PSBT
	// part are allowed to change.
	if i.PendingPsbt == nil {
		return fmt.Errorf("PSBT was not verified first")
	}
	err = verifyOutputsEqual(
		packet.UnsignedTx.TxOut, i.PendingPsbt.UnsignedTx.TxOut,
	)
	if err != nil {
		return fmt.Errorf("outputs differ from verified PSBT: %v", err)
	}
	err = verifyInputPrevOutpointsEqual(
		packet.UnsignedTx.TxIn, i.PendingPsbt.UnsignedTx.TxIn,
	)
	if err != nil {
		return fmt.Errorf("inputs differ from verified PSBT: %v", err)
	}

	// As far as we can tell, this PSBT is ok to be used as a funding
	// transaction.
	i.PendingPsbt = packet
	i.State = PsbtFinalized

	// Signal the funding manager that it can now finally continue with its
	// funding flow as the PSBT is now ready to be converted into a real
	// transaction and be published.
	i.signalPsbtReady.Do(func() {
		close(i.PsbtReady)
	})
	return nil
}

// CompileFundingTx finalizes the previously verified PSBT and returns the
// extracted binary serialized transaction from it. It also prepares the channel
// point for which this funding intent was initiated for.
func (i *PsbtIntent) CompileFundingTx() (*wire.MsgTx, error) {
	if i.State != PsbtFinalized {
		return nil, fmt.Errorf("invalid state. got %v expected %v",
			i.State, PsbtFinalized)
	}

	// Make sure the PSBT can be finalized and extracted.
	err := psbt.MaybeFinalizeAll(i.PendingPsbt)
	if err != nil {
		return nil, fmt.Errorf("error finalizing PSBT: %v", err)
	}
	fundingTx, err := psbt.Extract(i.PendingPsbt)
	if err != nil {
		return nil, fmt.Errorf("unable to extract funding TX: %v", err)
	}

	// Identify our funding outpoint now that we know everything's ready.
	_, txOut, err := i.FundingOutput()
	if err != nil {
		return nil, fmt.Errorf("cannot get funding output: %v", err)
	}
	ok, idx := input.FindScriptOutputIndex(fundingTx, txOut.PkScript)
	if !ok {
		return nil, fmt.Errorf("funding output not found in PSBT")
	}
	i.chanPoint = &wire.OutPoint{
		Hash:  fundingTx.TxHash(),
		Index: idx,
	}
	i.State = PsbtFundingTxCompiled

	return fundingTx, nil
}

// RemoteCanceled informs the listener of the PSBT ready channel that the
// funding has been canceled by the remote peer and that we can no longer
// continue with it.
func (i *PsbtIntent) RemoteCanceled() {
	log.Debugf("PSBT funding intent canceled by remote, state=%v", i.State)
	i.signalPsbtReady.Do(func() {
		i.PsbtReady <- ErrRemoteCanceled
		i.State = PsbtResponderCanceled
	})
	i.ShimIntent.Cancel()
}

// Cancel allows the caller to cancel a funding Intent at any time. This will
// return make sure the channel funding flow with the remote peer is failed and
// any reservations are canceled.
//
// NOTE: Part of the chanfunding.Intent interface.
func (i *PsbtIntent) Cancel() {
	log.Debugf("PSBT funding intent canceled, state=%v", i.State)
	i.signalPsbtReady.Do(func() {
		i.PsbtReady <- ErrUserCanceled
		i.State = PsbtInitiatorCanceled
	})
	i.ShimIntent.Cancel()
}

// PsbtAssembler is a type of chanfunding.Assembler wherein the funding
// transaction is constructed outside of lnd by using partially signed bitcoin
// transactions (PSBT).
type PsbtAssembler struct {
	// fundingAmt is the total amount of coins in the funding output.
	fundingAmt btcutil.Amount

	// basePsbt is the user-supplied base PSBT the channel output should be
	// added to.
	basePsbt *psbt.Packet

	// netParams are the network parameters used to encode the P2WSH funding
	// address.
	netParams *chaincfg.Params

	// shouldPublish specifies if the assembler should publish the
	// transaction once the channel funding has completed.
	shouldPublish bool
}

// NewPsbtAssembler creates a new CannedAssembler from the material required
// to construct a funding output and channel point. An optional base PSBT can
// be supplied which will be used to add the channel output to instead of
// creating a new one.
func NewPsbtAssembler(fundingAmt btcutil.Amount, basePsbt *psbt.Packet,
	netParams *chaincfg.Params, shouldPublish bool) *PsbtAssembler {

	return &PsbtAssembler{
		fundingAmt:    fundingAmt,
		basePsbt:      basePsbt,
		netParams:     netParams,
		shouldPublish: shouldPublish,
	}
}

// ProvisionChannel creates a new ShimIntent given the passed funding Request.
// The returned intent is immediately able to provide the channel point and
// funding output as they've already been created outside lnd.
//
// NOTE: This method satisfies the chanfunding.Assembler interface.
func (p *PsbtAssembler) ProvisionChannel(req *Request) (Intent, error) {
	// We'll exit out if this field is set as the funding transaction will
	// be assembled externally, so we don't influence coin selection.
	if req.SubtractFees {
		return nil, fmt.Errorf("SubtractFees not supported for PSBT")
	}

	intent := &PsbtIntent{
		ShimIntent: ShimIntent{
			localFundingAmt: p.fundingAmt,
		},
		State:     PsbtShimRegistered,
		BasePsbt:  p.basePsbt,
		PsbtReady: make(chan error, 1),
		netParams: p.netParams,
	}

	// A simple sanity check to ensure the provisioned request matches the
	// re-made shim intent.
	if req.LocalAmt+req.RemoteAmt != p.fundingAmt {
		return nil, fmt.Errorf("intent doesn't match PSBT "+
			"assembler: local_amt=%v, remote_amt=%v, funding_amt=%v",
			req.LocalAmt, req.RemoteAmt, p.fundingAmt)
	}

	return intent, nil
}

// ShouldPublishFundingTx is a method of the assembler that signals if the
// funding transaction should be published after the channel negotiations are
// completed with the remote peer.
//
// NOTE: This method is a part of the ConditionalPublishAssembler interface.
func (p *PsbtAssembler) ShouldPublishFundingTx() bool {
	return p.shouldPublish
}

// A compile-time assertion to ensure PsbtAssembler meets the
// ConditionalPublishAssembler interface.
var _ ConditionalPublishAssembler = (*PsbtAssembler)(nil)

// sumUtxoInputValues tries to extract the sum of all inputs specified in the
// UTXO fields of the PSBT. An error is returned if an input is specified that
// does not contain any UTXO information.
func sumUtxoInputValues(packet *psbt.Packet) (int64, error) {
	// We take the TX ins of the unsigned TX as the truth for how many
	// inputs there should be, as the fields in the extra data part of the
	// PSBT can be empty.
	if len(packet.UnsignedTx.TxIn) != len(packet.Inputs) {
		return 0, fmt.Errorf("TX input length doesn't match PSBT " +
			"input length")
	}
	inputSum := int64(0)
	for idx, in := range packet.Inputs {
		switch {
		case in.WitnessUtxo != nil:
			// Witness UTXOs only need to reference the TxOut.
			inputSum += in.WitnessUtxo.Value

		case in.NonWitnessUtxo != nil:
			// Non-witness UTXOs reference to the whole transaction
			// the UTXO resides in.
			utxOuts := in.NonWitnessUtxo.TxOut
			txIn := packet.UnsignedTx.TxIn[idx]
			inputSum += utxOuts[txIn.PreviousOutPoint.Index].Value

		default:
			return 0, fmt.Errorf("input %d has no UTXO information",
				idx)
		}
	}
	return inputSum, nil
}

// txOutsEqual returns true if two transaction outputs are equal.
func txOutsEqual(out1, out2 *wire.TxOut) bool {
	if out1 == nil || out2 == nil {
		return out1 == out2
	}
	return out1.Value == out2.Value &&
		bytes.Equal(out1.PkScript, out2.PkScript)
}

// verifyOutputsEqual verifies that the two slices of transaction outputs are
// deep equal to each other. We do the length check and manual loop to provide
// better error messages to the user than just returning "not equal".
func verifyOutputsEqual(outs1, outs2 []*wire.TxOut) error {
	if len(outs1) != len(outs2) {
		return fmt.Errorf("number of outputs are different")
	}
	for idx, out := range outs1 {
		// There is a byte slice in the output so we can't use the
		// equality operator.
		if !txOutsEqual(out, outs2[idx]) {
			return fmt.Errorf("output %d is different", idx)
		}
	}
	return nil
}

// verifyInputPrevOutpointsEqual verifies that the previous outpoints of the
// two slices of transaction inputs are deep equal to each other. We do the
// length check and manual loop to provide better error messages to the user
// than just returning "not equal".
func verifyInputPrevOutpointsEqual(ins1, ins2 []*wire.TxIn) error {
	if len(ins1) != len(ins2) {
		return fmt.Errorf("number of inputs are different")
	}
	for idx, in := range ins1 {
		if in.PreviousOutPoint != ins2[idx].PreviousOutPoint {
			return fmt.Errorf("previous outpoint of input %d is "+
				"different", idx)
		}
	}
	return nil
}

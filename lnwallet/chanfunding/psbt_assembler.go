package chanfunding

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
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

	// FinalTX is the final, signed and ready to be published wire format
	// transaction. This is only set after the PsbtFinalize step was
	// completed successfully.
	FinalTX *wire.MsgTx

	// PsbtReady is an error channel the funding manager will listen for
	// a signal about the PSBT being ready to continue the funding flow. In
	// the normal, happy flow, this channel is only ever closed. If a
	// non-nil error is sent through the channel, the funding flow will be
	// canceled.
	//
	// NOTE: This channel must always be buffered.
	PsbtReady chan error

	// shouldPublish specifies if the intent assumes its assembler should
	// publish the transaction once the channel funding has completed. If
	// this is set to false then the finalize step can be skipped.
	shouldPublish bool

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

// BindTapscriptRoot takes an optional tapscript root and binds it to the
// underlying funding intent. This only applies to musig2 channels, and will be
// used to make the musig2 funding output.
func (i *PsbtIntent) BindTapscriptRoot(root fn.Option[chainhash.Hash]) {
	i.tapscriptRoot = root
}

// FundingParams returns the parameters that are necessary to start funding the
// channel output this intent was created for. It returns the P2WSH funding
// address, the exact funding amount and a PSBT packet that contains exactly one
// output that encodes the previous two parameters.
func (i *PsbtIntent) FundingParams() (address.Address, int64, *psbt.Packet,
	error) {

	if i.State != PsbtOutputKnown {
		return nil, 0, nil, fmt.Errorf("invalid state, got %v "+
			"expected %v", i.State, PsbtOutputKnown)
	}

	// The funding output needs to be known already at this point, which
	// means we need to have the local and remote multisig keys bound
	// already.
	_, out, err := i.FundingOutput()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("unable to create funding "+
			"output: %v", err)
	}

	script, err := txscript.ParsePkScript(out.PkScript)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("unable to parse funding "+
			"output script: %w", err)
	}

	// Encode the address in the human-readable bech32 format.
	addr, err := script.Address(i.netParams)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("unable to encode address: %w",
			err)
	}

	// We'll also encode the address/amount in a machine-readable raw PSBT
	// format. If the user supplied a base PSBT, we'll add the output to
	// that one, otherwise we'll create a new one.
	packet := i.BasePsbt
	if packet == nil {
		packet, err = psbt.New(nil, nil, 2, 0, nil)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("unable to create "+
				"PSBT: %w", err)
		}
	}
	packet.UnsignedTx.TxOut = append(packet.UnsignedTx.TxOut, out)

	var pOut psbt.POutput

	// If this is a MuSig2 channel, we also need to communicate the internal
	// key to the caller. Otherwise, they cannot verify the construction of
	// the P2TR output script.
	pOut.TaprootInternalKey = fn.MapOptionZ(
		i.TaprootInternalKey(), schnorr.SerializePubKey,
	)

	packet.Outputs = append(packet.Outputs, pOut)

	return addr, out.Value, packet, nil
}

// Verify makes sure the PSBT that is given to the intent has an output that
// sends to the channel funding multisig address with the correct amount. A
// simple check that at least a single input has been specified is performed.
func (i *PsbtIntent) Verify(packet *psbt.Packet, skipFinalize bool) error {
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
		return fmt.Errorf("funding output cannot be created: %w", err)
	}
	outputFound := false
	outputSum := int64(0)
	for _, out := range packet.UnsignedTx.TxOut {
		outputSum += out.Value
		if psbt.TxOutsEqual(out, expectedOutput) {
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
	sum, err := psbt.SumUtxoInputValues(packet)
	if err != nil {
		return fmt.Errorf("error determining input sum: %w", err)
	}
	if sum <= outputSum {
		return fmt.Errorf("input amount sum must be larger than " +
			"output amount sum")
	}

	// To avoid possible malleability, all inputs to a funding transaction
	// must be SegWit spends.
	err = verifyAllInputsSegWit(packet.UnsignedTx.TxIn, packet.Inputs)
	if err != nil {
		return fmt.Errorf("cannot use TX for channel funding, "+
			"not all inputs are SegWit spends, risk of "+
			"malleability: %v", err)
	}

	// In case we aren't going to publish any transaction, we now have
	// everything we need and can skip the Finalize step.
	i.PendingPsbt = packet
	if !i.shouldPublish && skipFinalize {
		i.FinalTX = packet.UnsignedTx
		i.State = PsbtFinalized

		// Signal the funding manager that it can now continue with its
		// funding flow as the PSBT is now complete .
		i.signalPsbtReady.Do(func() {
			close(i.PsbtReady)
		})

		return nil
	}

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
		return fmt.Errorf("error finalizing PSBT: %w", err)
	}
	rawTx, err := psbt.Extract(packet)
	if err != nil {
		return fmt.Errorf("unable to extract funding TX: %w", err)
	}

	return i.FinalizeRawTX(rawTx)
}

// FinalizeRawTX makes sure the final raw transaction that is given to the
// intent is fully valid and signed but still contains the same UTXOs and
// outputs as the pending transaction we previously verified. If everything
// checks out, the funding manager is informed that the channel can now be
// opened and the funding transaction be broadcast.
func (i *PsbtIntent) FinalizeRawTX(rawTx *wire.MsgTx) error {
	if rawTx == nil {
		return fmt.Errorf("raw transaction is nil")
	}
	if i.State != PsbtVerified {
		return fmt.Errorf("invalid state. got %v expected %v", i.State,
			PsbtVerified)
	}

	// Do a basic check that this is still the same TX that we verified in
	// the previous step. This is to protect the user from unwanted
	// modifications. We only check the outputs and previous outpoints of
	// the inputs of the wire transaction because the fields in the PSBT
	// part are allowed to change.
	if i.PendingPsbt == nil {
		return fmt.Errorf("PSBT was not verified first")
	}
	err := psbt.VerifyOutputsEqual(
		rawTx.TxOut, i.PendingPsbt.UnsignedTx.TxOut,
	)
	if err != nil {
		return fmt.Errorf("outputs differ from verified PSBT: %w", err)
	}
	err = psbt.VerifyInputPrevOutpointsEqual(
		rawTx.TxIn, i.PendingPsbt.UnsignedTx.TxIn,
	)
	if err != nil {
		return fmt.Errorf("inputs differ from verified PSBT: %w", err)
	}

	// We also check that we have a signed TX. This is only necessary if the
	// FinalizeRawTX is called directly with a wire format TX instead of
	// extracting the TX from a PSBT.
	err = verifyInputsSigned(rawTx.TxIn)
	if err != nil {
		return fmt.Errorf("inputs not signed: %w", err)
	}

	// As far as we can tell, this TX is ok to be used as a funding
	// transaction.
	i.State = PsbtFinalized
	i.FinalTX = rawTx

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

	// Identify our funding outpoint now that we know everything's ready.
	_, txOut, err := i.FundingOutput()
	if err != nil {
		return nil, fmt.Errorf("cannot get funding output: %w", err)
	}
	ok, idx := input.FindScriptOutputIndex(i.FinalTX, txOut.PkScript)
	if !ok {
		return nil, fmt.Errorf("funding output not found in PSBT")
	}
	i.chanPoint = &wire.OutPoint{
		Hash:  i.FinalTX.TxHash(),
		Index: idx,
	}
	i.State = PsbtFundingTxCompiled

	return i.FinalTX, nil
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

// Inputs returns all inputs to the final funding transaction that we know
// about. These are only known after the PSBT has been verified.
func (i *PsbtIntent) Inputs() []wire.OutPoint {
	var inputs []wire.OutPoint

	switch i.State {
	// We return the inputs to the pending psbt.
	case PsbtVerified:
		for _, in := range i.PendingPsbt.UnsignedTx.TxIn {
			inputs = append(inputs, in.PreviousOutPoint)
		}

	// We return the inputs to the final funding tx.
	case PsbtFinalized, PsbtFundingTxCompiled:
		for _, in := range i.FinalTX.TxIn {
			inputs = append(inputs, in.PreviousOutPoint)
		}

	// In all other states we cannot know the inputs to the funding tx, and
	// return an empty list.
	default:
	}

	return inputs
}

// Outputs returns all outputs of the final funding transaction that we
// know about. These are only known after the PSBT has been verified.
func (i *PsbtIntent) Outputs() []*wire.TxOut {
	switch i.State {
	// We return the outputs of the pending psbt.
	case PsbtVerified:
		return i.PendingPsbt.UnsignedTx.TxOut

	// We return the outputs of the final funding tx.
	case PsbtFinalized, PsbtFundingTxCompiled:
		return i.FinalTX.TxOut

	// In all other states we cannot know the final outputs, and return an
	// empty list.
	default:
		return nil
	}
}

// ShouldPublishFundingTX returns true if the intent assumes that its assembler
// should publish the funding TX once the funding negotiation is complete.
func (i *PsbtIntent) ShouldPublishFundingTX() bool {
	return i.shouldPublish
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
	// We'll exit out if SubtractFees is set as the funding transaction will
	// be assembled externally, so we don't influence coin selection.
	if req.SubtractFees {
		return nil, fmt.Errorf("SubtractFees not supported for PSBT")
	}

	// We'll exit out if FundUpToMaxAmt or MinFundAmt is set as the funding
	// transaction will be assembled externally, so we don't influence coin
	// selection.
	if req.FundUpToMaxAmt != 0 || req.MinFundAmt != 0 {
		return nil, fmt.Errorf("FundUpToMaxAmt and MinFundAmt not " +
			"supported for PSBT")
	}

	intent := &PsbtIntent{
		ShimIntent: ShimIntent{
			localFundingAmt: p.fundingAmt,
			musig2:          req.Musig2,
			tapscriptRoot:   req.TapscriptRoot,
		},
		State:         PsbtShimRegistered,
		BasePsbt:      p.basePsbt,
		PsbtReady:     make(chan error, 1),
		shouldPublish: p.shouldPublish,
		netParams:     p.netParams,
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

// verifyInputsSigned verifies that the given list of inputs is non-empty and
// that all the inputs either contain a script signature or a witness stack.
func verifyInputsSigned(ins []*wire.TxIn) error {
	if len(ins) == 0 {
		return fmt.Errorf("no inputs in transaction")
	}
	for idx, in := range ins {
		if len(in.SignatureScript) == 0 && len(in.Witness) == 0 {
			return fmt.Errorf("input %d has no signature data "+
				"attached", idx)
		}
	}
	return nil
}

// verifyAllInputsSegWit makes sure all inputs to a transaction are SegWit
// spends. This is a bit tricky because the PSBT spec doesn't require the
// WitnessUtxo field to be set. Therefore if only a NonWitnessUtxo is given, we
// need to look at it and make sure it's either a witness pkScript or a nested
// SegWit spend.
func verifyAllInputsSegWit(txIns []*wire.TxIn, ins []psbt.PInput) error {
	for idx, in := range ins {
		switch {
		// The optimal case is that the witness UTXO is set explicitly.
		case in.WitnessUtxo != nil:

		// Only the non witness UTXO field is set, we need to inspect it
		// to make sure it's not P2PKH or bare P2SH.
		case in.NonWitnessUtxo != nil:
			utxo := in.NonWitnessUtxo
			txIn := txIns[idx]
			txOut := utxo.TxOut[txIn.PreviousOutPoint.Index]

			if !txscript.IsWitnessProgram(txOut.PkScript) &&
				!txscript.IsWitnessProgram(in.RedeemScript) {

				return fmt.Errorf("input %d is non-SegWit "+
					"spend or missing redeem script", idx)
			}

		// This should've already been caught by a previous check but we
		// keep it in for completeness' sake.
		default:
			return fmt.Errorf("input %d has no UTXO information",
				idx)
		}
	}

	return nil
}

// txWithFinalSigScripts returns a copy of the unsigned transaction of the given
// PSBT in which every input has the signature script it must have once it is
// signed. Signing only adds witness data on top of that, and witness data is
// not part of the TXID. So the returned transaction has the TXID of the signed
// transaction. An error is returned if the final signature script of an input
// can't be known before the input is signed.
func txWithFinalSigScripts(packet *psbt.Packet) (*wire.MsgTx, error) {
	txIns := packet.UnsignedTx.TxIn
	if len(packet.Inputs) != len(txIns) {
		return nil, fmt.Errorf("PSBT has %d inputs, but its "+
			"transaction has %d", len(packet.Inputs), len(txIns))
	}

	tx := packet.UnsignedTx.Copy()
	for idx, txIn := range tx.TxIn {
		sigScript, err := finalSigScript(txIn, &packet.Inputs[idx])
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", idx, err)
		}

		// We set the signature script of every input and clear its
		// witness, so the result doesn't depend on what the unsigned
		// transaction contains.
		txIn.SignatureScript = sigScript
		txIn.Witness = nil
	}

	return tx, nil
}

// finalSigScript returns the signature script that the given input must have
// once it is signed. By consensus (BIP-141), the signature script of a native
// SegWit input must be empty, and the one of a nested SegWit input must be
// exactly one push of its redeem script. The signature script of any other
// input depends on its signature, so an error is returned for it.
func finalSigScript(txIn *wire.TxIn, pIn *psbt.PInput) ([]byte, error) {
	pkScript, err := prevOutPkScript(txIn, pIn)
	if err != nil {
		return nil, err
	}

	var sigScript []byte
	switch {
	// A native SegWit input keeps its empty signature script.
	case txscript.IsWitnessProgram(pkScript):

	// A nested SegWit input pushes its redeem script, which must be a
	// witness program that hashes to the P2SH script of the output.
	case txscript.IsPayToScriptHash(pkScript):
		sigScript, err = nestedSegWitSigScript(
			pkScript, pIn.RedeemScript,
		)
		if err != nil {
			return nil, err
		}

	default:
		return nil, errors.New("not a SegWit spend")
	}

	// If the PSBT already contains a final signature script for the input,
	// it must be the one we expect. Otherwise, the signed transaction would
	// have a different TXID.
	if pIn.FinalScriptSig != nil &&
		!bytes.Equal(pIn.FinalScriptSig, sigScript) {

		return nil, errors.New("final signature script is not the " +
			"expected one for the input type")
	}

	return sigScript, nil
}

// nestedSegWitSigScript returns the signature script of an input that spends
// the given P2SH output with the given redeem script, which must be a witness
// program. The signature script is exactly one push of the redeem script.
func nestedSegWitSigScript(pkScript, redeemScript []byte) ([]byte, error) {
	if len(redeemScript) == 0 {
		return nil, errors.New("P2SH input has no redeem script " +
			"(finalizing a PSBT may remove it)")
	}

	if !txscript.IsWitnessProgram(redeemScript) {
		return nil, errors.New("P2SH input is not nested SegWit, its " +
			"redeem script is not a witness program")
	}

	p2shScript, err := input.GenerateP2SH(redeemScript)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(p2shScript, pkScript) {
		return nil, errors.New("redeem script does not match the " +
			"P2SH script of the input")
	}

	return txscript.NewScriptBuilder().AddData(redeemScript).Script()
}

// prevOutPkScript returns the pkScript of the output that the given input
// spends, as described by the UTXO fields of the PSBT input. A non-witness UTXO
// must be the transaction the input spends from, so its hash is checked against
// the outpoint of the input. If both UTXO fields are set, they must describe
// the same output.
func prevOutPkScript(txIn *wire.TxIn, pIn *psbt.PInput) ([]byte, error) {
	var nonWitnessUtxo *wire.TxOut
	if pIn.NonWitnessUtxo != nil {
		prevOut := txIn.PreviousOutPoint
		if pIn.NonWitnessUtxo.TxHash() != prevOut.Hash {
			return nil, errors.New("non-witness UTXO is not the " +
				"transaction the input spends from")
		}

		// We compare as uint64, because converting the index to int
		// would make a large index negative on 32-bit platforms.
		prevTxOuts := pIn.NonWitnessUtxo.TxOut
		if uint64(prevOut.Index) >= uint64(len(prevTxOuts)) {
			return nil, fmt.Errorf("non-witness UTXO has no "+
				"output %d", prevOut.Index)
		}

		nonWitnessUtxo = prevTxOuts[prevOut.Index]
	}

	switch {
	case pIn.WitnessUtxo != nil && nonWitnessUtxo != nil:
		if !psbt.TxOutsEqual(pIn.WitnessUtxo, nonWitnessUtxo) {
			return nil, errors.New("witness UTXO and non-witness " +
				"UTXO differ")
		}

		return pIn.WitnessUtxo.PkScript, nil

	case pIn.WitnessUtxo != nil:
		return pIn.WitnessUtxo.PkScript, nil

	case nonWitnessUtxo != nil:
		return nonWitnessUtxo.PkScript, nil

	default:
		return nil, errors.New("no UTXO information")
	}
}

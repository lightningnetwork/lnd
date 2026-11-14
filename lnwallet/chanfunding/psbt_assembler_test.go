package chanfunding

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

var (
	localPrivkey                  = []byte{1, 2, 3, 4, 5, 6}
	remotePrivkey                 = []byte{6, 5, 4, 3, 2, 1}
	chanCapacity   btcutil.Amount = 644000
	params                        = chaincfg.RegressionNetParams
	defaultTimeout                = 50 * time.Millisecond
)

// TestPsbtIntent tests the basic happy path of the PSBT assembler and intent.
func TestPsbtIntent(t *testing.T) {
	t.Parallel()

	// Create a simple assembler and ask it to provision a channel to get
	// the funding intent.
	a := NewPsbtAssembler(chanCapacity, nil, &params, true)
	intent, err := a.ProvisionChannel(&Request{LocalAmt: chanCapacity})
	require.NoError(t, err, "error provisioning channel")
	psbtIntent, ok := intent.(*PsbtIntent)
	if !ok {
		t.Fatalf("intent was not a PsbtIntent")
	}
	if psbtIntent.State != PsbtShimRegistered {
		t.Fatalf("unexpected state. got %d wanted %d", psbtIntent.State,
			PsbtShimRegistered)
	}

	// The first step with the intent is that the funding manager starts
	// negotiating with the remote peer and they accept. By accepting, they
	// send over their multisig key that's going to be used for the funding
	// output. With that known, we can start crafting a PSBT.
	_, localPubkey := btcec.PrivKeyFromBytes(localPrivkey)
	_, remotePubkey := btcec.PrivKeyFromBytes(remotePrivkey)
	psbtIntent.BindKeys(
		&keychain.KeyDescriptor{PubKey: localPubkey}, remotePubkey,
	)
	if psbtIntent.State != PsbtOutputKnown {
		t.Fatalf("unexpected state. got %d wanted %d", psbtIntent.State,
			PsbtOutputKnown)
	}

	// Make sure the output script address is correct.
	script, _, err := input.GenFundingPkScript(
		localPubkey.SerializeCompressed(),
		remotePubkey.SerializeCompressed(), int64(chanCapacity),
	)
	require.NoError(t, err, "error calculating script")
	witnessScriptHash := sha256.Sum256(script)
	addr, err := address.NewAddressWitnessScriptHash(
		witnessScriptHash[:], &params,
	)
	require.NoError(t, err, "unable to encode address")
	fundingAddr, amt, pendingPsbt, err := psbtIntent.FundingParams()
	require.NoError(t, err, "unable to get funding params")
	if addr.EncodeAddress() != fundingAddr.EncodeAddress() {
		t.Fatalf("unexpected address. got %s wanted %s", fundingAddr,
			addr)
	}
	if amt != int64(chanCapacity) {
		t.Fatalf("unexpected amount. got %d wanted %d", amt,
			chanCapacity)
	}

	// Parse and check the returned PSBT packet.
	if pendingPsbt == nil {
		t.Fatalf("expected pending PSBT to be returned")
	}
	if len(pendingPsbt.UnsignedTx.TxOut) != 1 {
		t.Fatalf("unexpected number of outputs. got %d wanted %d",
			len(pendingPsbt.UnsignedTx.TxOut), 1)
	}
	txOut := pendingPsbt.UnsignedTx.TxOut[0]
	if !bytes.Equal(txOut.PkScript[2:], witnessScriptHash[:]) {
		t.Fatalf("unexpected PK script in output. got %x wanted %x",
			txOut.PkScript[2:], witnessScriptHash)
	}
	if txOut.Value != int64(chanCapacity) {
		t.Fatalf("unexpected value in output. got %d wanted %d",
			txOut.Value, chanCapacity)
	}

	// Add an input to the pending TX to simulate it being funded.
	pendingPsbt.UnsignedTx.TxIn = []*wire.TxIn{
		{PreviousOutPoint: wire.OutPoint{Index: 0}},
	}
	pendingPsbt.Inputs = []psbt.PInput{
		{WitnessUtxo: &wire.TxOut{Value: int64(chanCapacity + 1)}},
	}

	// Verify the dummy PSBT with the intent.
	err = psbtIntent.Verify(pendingPsbt, false)
	require.NoError(t, err, "error verifying pending PSBT")
	if psbtIntent.State != PsbtVerified {
		t.Fatalf("unexpected state. got %d wanted %d", psbtIntent.State,
			PsbtVerified)
	}

	// Add some fake witness data to the transaction so it thinks it's
	// signed.
	pendingPsbt.Inputs[0].WitnessUtxo = &wire.TxOut{
		Value:    int64(chanCapacity) * 2,
		PkScript: []byte{99, 99, 99},
	}
	pendingPsbt.Inputs[0].FinalScriptSig = []byte{88, 88, 88}
	pendingPsbt.Inputs[0].FinalScriptWitness = []byte{2, 0, 0}

	// If we call Finalize, the intent will signal to the funding manager
	// that it can continue with the funding flow. We want to make sure
	// the signal arrives.
	var wg sync.WaitGroup
	errChan := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case err := <-psbtIntent.PsbtReady:
			errChan <- err

		case <-time.After(defaultTimeout):
			errChan <- fmt.Errorf("timed out")
		}
	}()
	err = psbtIntent.Finalize(pendingPsbt)
	require.NoError(t, err, "error finalizing pending PSBT")
	wg.Wait()

	// We should have a nil error in our channel now.
	err = <-errChan
	require.NoError(t, err, "unexpected error after finalize")
	if psbtIntent.State != PsbtFinalized {
		t.Fatalf("unexpected state. got %d wanted %d", psbtIntent.State,
			PsbtFinalized)
	}

	// Make sure the funding transaction can be compiled.
	_, err = psbtIntent.CompileFundingTx()
	require.NoError(t, err, "error compiling funding TX from PSBT")
	if psbtIntent.State != PsbtFundingTxCompiled {
		t.Fatalf("unexpected state. got %d wanted %d", psbtIntent.State,
			PsbtFundingTxCompiled)
	}
}

// TestPsbtIntentBasePsbt tests that a channel funding output can be appended to
// a given base PSBT in the funding flow.
func TestPsbtIntentBasePsbt(t *testing.T) {
	t.Parallel()

	// First create a dummy PSBT with a single output.
	pendingPsbt, err := psbt.New(
		[]*wire.OutPoint{{}}, []*wire.TxOut{
			{Value: 999, PkScript: []byte{99, 88, 77}},
		}, 2, 0, []uint32{0},
	)
	if err != nil {
		t.Fatalf("unable to create dummy PSBT")
	}

	// Generate the funding multisig keys and the address so we can compare
	// it to the output of the intent.
	_, localPubkey := btcec.PrivKeyFromBytes(localPrivkey)
	_, remotePubkey := btcec.PrivKeyFromBytes(remotePrivkey)
	// Make sure the output script address is correct.
	script, _, err := input.GenFundingPkScript(
		localPubkey.SerializeCompressed(),
		remotePubkey.SerializeCompressed(), int64(chanCapacity),
	)
	require.NoError(t, err, "error calculating script")
	witnessScriptHash := sha256.Sum256(script)
	addr, err := address.NewAddressWitnessScriptHash(
		witnessScriptHash[:], &params,
	)
	require.NoError(t, err, "unable to encode address")

	// Now as the next step, create a new assembler/intent pair with a base
	// PSBT to see that we can add an additional output to it.
	a := NewPsbtAssembler(chanCapacity, pendingPsbt, &params, true)
	intent, err := a.ProvisionChannel(&Request{LocalAmt: chanCapacity})
	require.NoError(t, err, "error provisioning channel")
	psbtIntent, ok := intent.(*PsbtIntent)
	if !ok {
		t.Fatalf("intent was not a PsbtIntent")
	}
	psbtIntent.BindKeys(
		&keychain.KeyDescriptor{PubKey: localPubkey}, remotePubkey,
	)
	newAddr, amt, twoOutPsbt, err := psbtIntent.FundingParams()
	require.NoError(t, err, "unable to get funding params")
	if addr.EncodeAddress() != newAddr.EncodeAddress() {
		t.Fatalf("unexpected address. got %s wanted %s", newAddr,
			addr)
	}
	if amt != int64(chanCapacity) {
		t.Fatalf("unexpected amount. got %d wanted %d", amt,
			chanCapacity)
	}
	if len(twoOutPsbt.UnsignedTx.TxOut) != 2 {
		t.Fatalf("unexpected number of outputs. got %d wanted %d",
			len(twoOutPsbt.UnsignedTx.TxOut), 2)
	}
	if len(twoOutPsbt.UnsignedTx.TxIn) != 1 {
		t.Fatalf("unexpected number of inputs. got %d wanted %d",
			len(twoOutPsbt.UnsignedTx.TxIn), 1)
	}
	txOld := pendingPsbt.UnsignedTx
	txNew := twoOutPsbt.UnsignedTx
	prevoutEqual := reflect.DeepEqual(
		txOld.TxIn[0].PreviousOutPoint, txNew.TxIn[0].PreviousOutPoint,
	)
	if !prevoutEqual {
		t.Fatalf("inputs changed. got %s wanted %s",
			spew.Sdump(txOld.TxIn[0].PreviousOutPoint),
			spew.Sdump(txNew.TxIn[0].PreviousOutPoint))
	}
	if !reflect.DeepEqual(txOld.TxOut[0], txNew.TxOut[0]) {
		t.Fatalf("existing output changed. got %v wanted %v",
			txOld.TxOut[0], txNew.TxOut[0])
	}
}

// TestPsbtVerify tests the PSBT verification process more deeply than just
// the happy path.
func TestPsbtVerify(t *testing.T) {
	t.Parallel()

	// Signing an input that spends a native SegWit output doesn't change
	// the TXID of the transaction.
	_, inputPubKey := btcec.PrivKeyFromBytes(localPrivkey)
	nativePkScript, err := input.WitnessPubKeyHash(
		inputPubKey.SerializeCompressed(),
	)
	require.NoError(t, err)

	testCases := []struct {
		name          string
		expectedErr   string
		shouldPublish bool
		doVerify      func(int64, *psbt.Packet, *PsbtIntent) error
	}{
		{
			name:          "nil packet",
			expectedErr:   "PSBT is nil",
			shouldPublish: true,
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				return i.Verify(nil, false)
			},
		},
		{
			name:          "wrong state",
			shouldPublish: true,
			expectedErr: "invalid state. got user_canceled " +
				"expected output_known",
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				i.State = PsbtInitiatorCanceled
				return i.Verify(p, false)
			},
		},
		{
			name:          "output not found, value wrong",
			shouldPublish: true,
			expectedErr:   "funding output not found in PSBT",
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				p.UnsignedTx.TxOut[0].Value = 123
				return i.Verify(p, false)
			},
		},
		{
			name:          "output not found, pk script wrong",
			shouldPublish: true,
			expectedErr:   "funding output not found in PSBT",
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				p.UnsignedTx.TxOut[0].PkScript = []byte{1, 2, 3}
				return i.Verify(p, false)
			},
		},
		{
			name:          "no inputs",
			shouldPublish: true,
			expectedErr:   "PSBT has no inputs",
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				return i.Verify(p, false)
			},
		},
		{
			name:          "input(s) too small",
			shouldPublish: true,
			expectedErr: "input amount sum must be larger than " +
				"output amount sum",
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				p.UnsignedTx.TxIn = []*wire.TxIn{{}}
				p.Inputs = []psbt.PInput{{
					WitnessUtxo: &wire.TxOut{
						Value: int64(chanCapacity),
					},
				}}
				return i.Verify(p, false)
			},
		},
		{
			name:          "missing witness-utxo field",
			shouldPublish: true,
			expectedErr: "cannot use TX for channel funding, not " +
				"all inputs are SegWit spends, risk of " +
				"malleability: input 1 is non-SegWit spend " +
				"or missing redeem script",
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				txOut := &wire.TxOut{
					Value: int64(chanCapacity/2) + 1,
				}
				p.UnsignedTx.TxIn = []*wire.TxIn{
					{},
					{
						PreviousOutPoint: wire.OutPoint{
							Index: 0,
						},
					},
				}
				p.Inputs = []psbt.PInput{
					{
						WitnessUtxo: txOut,
					},
					{
						NonWitnessUtxo: &wire.MsgTx{
							TxOut: []*wire.TxOut{
								txOut,
							},
						},
					},
				}
				return i.Verify(p, false)
			},
		},
		{
			name:          "skip verify",
			shouldPublish: false,
			expectedErr:   "",
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				txOut := &wire.TxOut{
					Value:    int64(chanCapacity/2) + 1,
					PkScript: nativePkScript,
				}
				p.UnsignedTx.TxIn = []*wire.TxIn{
					{},
					{
						PreviousOutPoint: wire.OutPoint{
							Index: 0,
						},
					},
				}
				p.Inputs = []psbt.PInput{
					{
						WitnessUtxo: txOut,
					},
					{
						WitnessUtxo: txOut,
					},
				}

				if err := i.Verify(p, true); err != nil {
					return err
				}

				finalTxid := i.FinalTX.TxHash()
				if finalTxid != p.UnsignedTx.TxHash() {
					return fmt.Errorf("expected final TX " +
						"to have the unsigned TXID")
				}
				if i.State != PsbtFinalized {
					return fmt.Errorf("expected state to " +
						"be finalized")
				}

				select {
				case <-i.PsbtReady:

				case <-time.After(50 * time.Millisecond):
					return fmt.Errorf("expected PSBT " +
						"ready to be signaled")
				}

				return nil
			},
		},
		{
			name:          "input correct",
			shouldPublish: true,
			expectedErr:   "",
			doVerify: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				txOut := &wire.TxOut{
					Value: int64(chanCapacity/2) + 1,
				}
				p.UnsignedTx.TxIn = []*wire.TxIn{
					{},
					{
						PreviousOutPoint: wire.OutPoint{
							Index: 0,
						},
					},
				}
				p.Inputs = []psbt.PInput{
					{
						WitnessUtxo: txOut,
					},
					{
						WitnessUtxo: txOut,
					}}
				return i.Verify(p, false)
			},
		},
	}

	// Loop through all our test cases.
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Each case gets its own intent, so no case can leave
			// state behind for the next one, such as a closed
			// PsbtReady channel. Then we create a new pending PSBT
			// that we can manipulate.
			psbtIntent := newTestPsbtIntent(t, tc.shouldPublish)
			_, amt, pendingPsbt, err := psbtIntent.FundingParams()
			require.NoError(t, err, "unable to get funding params")

			err = tc.doVerify(amt, pendingPsbt, psbtIntent)
			if tc.expectedErr != "" {
				require.EqualError(t, err, tc.expectedErr)

				return
			}

			require.NoError(t, err)
		})
	}
}

// newTestPsbtIntent returns a PSBT intent for a channel of the test capacity,
// with the test keys bound so that its funding output is known.
func newTestPsbtIntent(t *testing.T, shouldPublish bool) *PsbtIntent {
	t.Helper()

	a := NewPsbtAssembler(chanCapacity, nil, &params, shouldPublish)
	intent, err := a.ProvisionChannel(&Request{LocalAmt: chanCapacity})
	require.NoError(t, err, "error provisioning channel")

	psbtIntent, ok := intent.(*PsbtIntent)
	require.True(t, ok, "unexpected intent type %T", intent)

	// Bind our test keys to get the funding parameters.
	_, localPubkey := btcec.PrivKeyFromBytes(localPrivkey)
	_, remotePubkey := btcec.PrivKeyFromBytes(remotePrivkey)
	psbtIntent.BindKeys(
		&keychain.KeyDescriptor{PubKey: localPubkey}, remotePubkey,
	)

	return psbtIntent
}

// TestPsbtFinalize tests the PSBT finalization process more deeply than just
// the happy path.
func TestPsbtFinalize(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		expectedErr string
		doFinalize  func(int64, *psbt.Packet, *PsbtIntent) error
	}{
		{
			name:        "nil packet",
			expectedErr: "PSBT is nil",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				return i.Finalize(nil)
			},
		},
		{
			name: "wrong state",
			expectedErr: "invalid state. got user_canceled " +
				"expected verified",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				i.State = PsbtInitiatorCanceled
				return i.Finalize(p)
			},
		},
		{
			name:        "not verified first",
			expectedErr: "PSBT was not verified first",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				i.State = PsbtVerified
				i.PendingPsbt = nil
				return i.Finalize(p)
			},
		},
		{
			name: "output value changed",
			expectedErr: "outputs differ from verified PSBT: " +
				"output 0 is different",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				p.UnsignedTx.TxOut[0].Value = 123
				return i.Finalize(p)
			},
		},
		{
			name: "output pk script changed",
			expectedErr: "outputs differ from verified PSBT: " +
				"output 0 is different",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				p.UnsignedTx.TxOut[0].PkScript = []byte{3, 2, 1}
				return i.Finalize(p)
			},
		},
		{
			name: "input previous outpoint index changed",
			expectedErr: "inputs differ from verified PSBT: " +
				"previous outpoint of input 0 is different",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				p.UnsignedTx.TxIn[0].PreviousOutPoint.Index = 0
				return i.Finalize(p)
			},
		},
		{
			name: "input previous outpoint hash changed",
			expectedErr: "inputs differ from verified PSBT: " +
				"previous outpoint of input 0 is different",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				prevout := &p.UnsignedTx.TxIn[0].PreviousOutPoint
				prevout.Hash = chainhash.Hash{77, 88, 99, 11}
				return i.Finalize(p)
			},
		},
		{
			name:        "raw tx - nil transaction",
			expectedErr: "raw transaction is nil",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				return i.FinalizeRawTX(nil)
			},
		},
		{
			name: "raw tx - no witness data in raw tx",
			expectedErr: "inputs not signed: input 0 has no " +
				"signature data attached",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				rawTx, err := psbt.Extract(p)
				require.NoError(t, err)
				rawTx.TxIn[0].Witness = nil

				return i.FinalizeRawTX(rawTx)
			},
		},
		{
			name:        "happy path",
			expectedErr: "",
			doFinalize: func(amt int64, p *psbt.Packet,
				i *PsbtIntent) error {

				err := i.Finalize(p)
				require.NoError(t, err)

				require.Equal(t, PsbtFinalized, i.State)
				require.NotNil(t, i.FinalTX)

				return nil
			},
		},
	}

	// Create a simple assembler and ask it to provision a channel to get
	// the funding intent.
	a := NewPsbtAssembler(chanCapacity, nil, &params, true)
	intent, err := a.ProvisionChannel(&Request{LocalAmt: chanCapacity})
	require.NoError(t, err, "error provisioning channel")
	psbtIntent := intent.(*PsbtIntent)

	// Bind our test keys to get the funding parameters.
	_, localPubkey := btcec.PrivKeyFromBytes(localPrivkey)
	_, remotePubkey := btcec.PrivKeyFromBytes(remotePrivkey)
	psbtIntent.BindKeys(
		&keychain.KeyDescriptor{PubKey: localPubkey}, remotePubkey,
	)

	// Loop through all our test cases.
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset the state from a previous test and create a new
			// pending PSBT that we can manipulate.
			psbtIntent.State = PsbtOutputKnown
			_, amt, pendingPsbt, err := psbtIntent.FundingParams()
			if err != nil {
				t.Fatalf("unable to get funding params: %v", err)
			}

			// We need to have a simulated transaction here that is
			// fully funded and signed.
			pendingPsbt.UnsignedTx.TxIn = []*wire.TxIn{{
				PreviousOutPoint: wire.OutPoint{
					Index: 1,
					Hash:  chainhash.Hash{1, 2, 3},
				},
			}}
			pendingPsbt.Inputs = []psbt.PInput{{
				WitnessUtxo: &wire.TxOut{
					Value:    int64(chanCapacity) + 1,
					PkScript: []byte{1, 2, 3},
				},
				FinalScriptWitness: []byte{0x01, 0x00},
			}}
			err = psbtIntent.Verify(pendingPsbt, false)
			if err != nil {
				t.Fatalf("error verifying PSBT: %v", err)
			}

			// Deep clone the PSBT so we don't modify the pending
			// one that was registered during Verify.
			pendingPsbt = clonePsbt(t, pendingPsbt)

			err = tc.doFinalize(amt, pendingPsbt, psbtIntent)
			if (err == nil && tc.expectedErr != "") ||
				(err != nil && err.Error() != tc.expectedErr) {

				t.Fatalf("unexpected error, got '%v' wanted "+
					"'%v'", err, tc.expectedErr)
			}
		})
	}
}

func TestVerifyAllInputsSegWit(t *testing.T) {
	testCases := []struct {
		name       string
		packet     string
		shouldFail bool
	}{{
		// Electrum takes the spec literally and does not include the
		// WitnessUtxo field in the PSBT.
		name: "Electrum np2wkh spend",
		packet: "cHNidP8BAFMCAAAAAcptbRo6Ez2R8WclfL4IAW+XEWIl8sNOHnw" +
			"eXfeU3dTwAQAAAAD9////Ab26GQAAAAAAF6kUf+wXsa7Z4ejieW" +
			"CSdKaGFwJmfbyHQyMeAAABAOACAAAAAAEBiS1A2NE4KW00wLQ1B" +
			"9R05jFMINs6egYFNL/5vLseeXMBAAAAAP7///8CNl1m1AEAAAAX" +
			"qRR/7Bexrtnh6OJ5YJJ0poYXAmZ9vIdDuxkAAAAAABepFMD8Ngx" +
			"k/Z7M5nJM2ltpszQZ4KGahwJHMEQCIAD4EIg16hjWEnEf2pilRF" +
			"UmWSk5UsMD7tTrR2FbFCurAiAiNvqpFQSr/FCCXknOV1HzeDVxe" +
			"B9gXeDZ62tmZv04/AEhAxGTJ9E/ImiOp2Ti3w/l/JFHa95RvXCB" +
			"UXFKifC/7f9dQyMeAAEEFgAU9pqV6WX0D/HEMfdw9yzU30MmkV4" +
			"iBgP4kZAAjhAKuixOLJczugvMmlS0xxPc2eD+4IJPHVOeRgxGbt" +
			"z0AAAAAAAAAAAAAA==",
	}, {
		name: "Electrum p2wkh spend",
		packet: "cHNidP8BAFMCAAAAASaMwGRzF7QWjHMX9WaGyUp7f3jznuNigHF" +
			"4l57hW0CgAAAAAAD9////AU+6GQAAAAAAF6kUf+wXsa7Z4ejieW" +
			"CSdKaGFwJmfbyHRSMeAAABANYCAAAAAAEBym1tGjoTPZHxZyV8v" +
			"ggBb5cRYiXyw04efB5d95Td1PABAAAAFxYAFPaalell9A/xxDH3" +
			"cPcs1N9DJpFe/f///wG+uhkAAAAAABYAFPaalell9A/xxDH3cPc" +
			"s1N9DJpFeAkcwRAIgRwfN0qpcM5DHR+YrgXFxhEi6F0qTf4dmQc" +
			"c1cu3TnnICIH5wc0kkMbHZ2mqZUFEITRUXExR7US3wohctQK1lf" +
			"q/oASED+JGQAI4QCrosTiyXM7oLzJpUtMcT3Nng/uCCTx1TnkZF" +
			"Ix4AIgYD+JGQAI4QCrosTiyXM7oLzJpUtMcT3Nng/uCCTx1TnkY" +
			"MRm7c9AAAAAAAAAAAAAA=",
	}, {
		name: "Bitcoind p2wkh spend",
		packet: "cHNidP8BAHICAAAAAeEw64k3k+YEk5EgNTytvkGlpiF6iMSsm0o" +
			"dCSm03dt7AAAAAAD+////AsSvp8oAAAAAFgAUzWRW/Ccwf/Cosy" +
			"dy0uYe5fa/Gb0A4fUFAAAAABepFMD8Ngxk/Z7M5nJM2ltpszQZ4" +
			"KGahwAAAAAAAQBxAgAAAAEtMrUZuqjRoaSEMU9sc7pS2zKlS8X3" +
			"/CUaENX1IHOICgEAAAAA/v///wLcm53QAAAAABYAFB1zJOaGOpk" +
			"NHVkIguiZ2Zh46dlmAGXNHQAAAAAWABTbjTIbrBMD+sWxE6mahK" +
			"tKGa//O1wAAAABAR/cm53QAAAAABYAFB1zJOaGOpkNHVkIguiZ2" +
			"Zh46dlmIgYDjVRSrQ7d5Rld1nDBTY/uA7Xp+SKUfaFpB0SQiM8O" +
			"3usQBp+HNQAAAIABAACADwAAgAAiAgJ9LOXb4MwHRoq31B9fQ2U" +
			"0K+Uj8BBJzy+MWc0OOOVuQxAGn4c1AAAAgAEAAIARAACAAAA=",
	}, {
		name: "Bitcoind p2pkh spend",
		packet: "cHNidP8BAHICAAAAAdp3QE/zv3Q7RhkAR0JyzBWLttUqRHJ" +
			"pAYmHLIqJzGXOAQAAAAD/////AoDw+gIAAAAAF6kUwPw2DGT9ns" +
			"zmckzaW2mzNBngoZqHUN/6AgAAAAAWABRLzlCRi8BozkTClLcIN" +
			"tnz8zYTtAAAAAAAAQB0AgAAAAHhMOuJN5PmBJORIDU8rb5BpaYh" +
			"eojErJtKHQkptN3bewAAAAAA/v///wKcr6fKAAAAABYAFOvePfh" +
			"lztKeDuQRgcGsHLS5BPhTAOH1BQAAAAAZdqkUt9QziR33rxhwcg" +
			"ipzwrDti93vTWIrAAAAAAiBgNKzHC4j7KaSKeH9RYx3Ur3dHL5w" +
			"YRjXpOAi3SI12WNFxAGn4c1AAAAgAAAAIAGAACAAAAiAgK8Fq3O" +
			"5nnASvhn9LJMIJOkGBMYQFd5DcbvOCbBCX/VBRAGn4c1AAAAgAE" +
			"AAIAVAACAAA==",
		shouldFail: true,
	}, {
		name: "Bitcoind p2sh multisig spend",
		packet: "cHNidP8BAHICAAAAAQvO6z5f4wghsQ2c5+Zcw2qdZ4FOYkyWBFe" +
			"U/jiIKcwdAAAAAAD/////AkTc+gIAAAAAFgAU2kWEYfMLfgwVQ0" +
			"2wJwFNsOmorBWA8PoCAAAAABepFMD8Ngxk/Z7M5nJM2ltpszQZ4" +
			"KGahwAAAAAAAQByAgAAAAHad0BP8790O0YZAEdCcswVi7bVKkRy" +
			"aQGJhyyKicxlzgAAAAAA/v///wIA4fUFAAAAABepFJdSg2Xdeo3" +
			"mYbTqbcZnZIH8oWbPh4TDscQAAAAAFgAUs5d3GhxrF5Zdi8fQHy" +
			"A05BSZr4t3AAAAAQRHUSEDdy41G190ATg/VnhXHE4dufESLLl53" +
			"RewoYB2ZYRJ/4AhA6MR4qMgHUkIyqhXW0jEECV8cHg/DCiuLgUk" +
			"YvQeub1zUq4AAAA=",
		shouldFail: true,
	}}

	for _, tc := range testCases {

		t.Run(tc.name, func(t *testing.T) {
			r := strings.NewReader(tc.packet)
			packet, err := psbt.NewFromRawBytes(r, true)
			require.NoError(t, err)

			txIns := packet.UnsignedTx.TxIn
			ins := packet.Inputs

			err = verifyAllInputsSegWit(txIns, ins)
			if tc.shouldFail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// clonePsbt creates a clone of a PSBT packet by serializing then de-serializing
// it.
func clonePsbt(t *testing.T, p *psbt.Packet) *psbt.Packet {
	var buf bytes.Buffer
	err := p.Serialize(&buf)
	require.NoError(t, err, "error serializing PSBT")
	newPacket, err := psbt.NewFromRawBytes(&buf, false)
	require.NoError(t, err, "error unserializing PSBT")
	return newPacket
}

// TestTxWithFinalSigScripts makes sure that the final signature script of every
// input is filled in, and that a PSBT is rejected if the final signature script
// of an input can't be known before the input is signed.
func TestTxWithFinalSigScripts(t *testing.T) {
	t.Parallel()

	_, pubKey := btcec.PrivKeyFromBytes(localPrivkey)
	_, otherPubKey := btcec.PrivKeyFromBytes(remotePrivkey)
	pubKeyBytes := pubKey.SerializeCompressed()

	p2wkh, err := input.WitnessPubKeyHash(pubKeyBytes)
	require.NoError(t, err)

	multiSigScript, err := input.GenMultiSigScript(
		pubKeyBytes, otherPubKey.SerializeCompressed(),
	)
	require.NoError(t, err)

	p2wsh, err := input.WitnessScriptHash(multiSigScript)
	require.NoError(t, err)

	p2tr, err := txscript.PayToTaprootScript(pubKey)
	require.NoError(t, err)

	np2wkh, err := input.GenerateP2SH(p2wkh)
	require.NoError(t, err)

	np2wsh, err := input.GenerateP2SH(p2wsh)
	require.NoError(t, err)

	p2pkh, err := input.GenerateP2PKH(pubKeyBytes)
	require.NoError(t, err)

	bareP2SH, err := input.GenerateP2SH(multiSigScript)
	require.NoError(t, err)

	// pushData returns a script that pushes the given data. A push of up
	// to 75 bytes is the length of the data, followed by the data.
	pushData := func(data []byte) []byte {
		return append([]byte{byte(len(data))}, data...)
	}

	// newPacket returns a PSBT with an input for each of the given output
	// scripts. Each input spends output 1 of its own previous transaction,
	// and has both UTXO fields set.
	newPacket := func(pkScripts [][]byte) *psbt.Packet {
		tx := wire.NewMsgTx(2)
		inputs := make([]psbt.PInput, 0, len(pkScripts))
		for idx, pkScript := range pkScripts {
			prevTx := wire.NewMsgTx(2)
			prevTx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Index: uint32(idx),
				},
			})
			prevTx.AddTxOut(&wire.TxOut{
				Value:    1000,
				PkScript: p2wkh,
			})
			prevTx.AddTxOut(&wire.TxOut{
				Value:    100_000,
				PkScript: pkScript,
			})

			tx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Hash:  prevTx.TxHash(),
					Index: 1,
				},
			})
			inputs = append(inputs, psbt.PInput{
				WitnessUtxo: &wire.TxOut{
					Value:    100_000,
					PkScript: pkScript,
				},
				NonWitnessUtxo: prevTx,
			})
		}
		tx.AddTxOut(&wire.TxOut{Value: 50_000, PkScript: p2tr})

		return &psbt.Packet{
			UnsignedTx: tx,
			Inputs:     inputs,
			Outputs:    make([]psbt.POutput, len(tx.TxOut)),
		}
	}

	// The following functions change the inputs of a PSBT.
	noWitnessUtxo := func(p *psbt.Packet) {
		for idx := range p.Inputs {
			p.Inputs[idx].WitnessUtxo = nil
		}
	}
	noNonWitnessUtxo := func(p *psbt.Packet) {
		for idx := range p.Inputs {
			p.Inputs[idx].NonWitnessUtxo = nil
		}
	}
	redeemScripts := func(scripts ...[]byte) func(*psbt.Packet) {
		return func(p *psbt.Packet) {
			for idx, script := range scripts {
				p.Inputs[idx].RedeemScript = script
			}
		}
	}
	finalScriptSig := func(script []byte) func(*psbt.Packet) {
		return func(p *psbt.Packet) {
			p.Inputs[0].FinalScriptSig = script
		}
	}

	testCases := []struct {
		name string

		// pkScripts are the scripts of the outputs that the inputs
		// spend.
		pkScripts [][]byte

		// modify holds the changes to apply to the PSBT before the
		// test.
		modify []func(*psbt.Packet)

		// sigScripts are the expected signature scripts of the inputs.
		sigScripts [][]byte

		// expectedErr is a part of the expected error, if any.
		expectedErr string
	}{{
		name:       "native SegWit, both UTXO fields",
		pkScripts:  [][]byte{p2wkh, p2wsh, p2tr},
		sigScripts: [][]byte{nil, nil, nil},
	}, {
		name:       "native SegWit, witness UTXO only",
		pkScripts:  [][]byte{p2wkh, p2wsh, p2tr},
		modify:     []func(*psbt.Packet){noNonWitnessUtxo},
		sigScripts: [][]byte{nil, nil, nil},
	}, {
		name:       "native SegWit, non-witness UTXO only",
		pkScripts:  [][]byte{p2wkh, p2wsh, p2tr},
		modify:     []func(*psbt.Packet){noWitnessUtxo},
		sigScripts: [][]byte{nil, nil, nil},
	}, {
		name:      "nested SegWit, both UTXO fields",
		pkScripts: [][]byte{np2wkh, np2wsh},
		modify: []func(*psbt.Packet){
			redeemScripts(p2wkh, p2wsh),
		},
		sigScripts: [][]byte{pushData(p2wkh), pushData(p2wsh)},
	}, {
		name:      "nested SegWit, witness UTXO only",
		pkScripts: [][]byte{np2wkh, np2wsh},
		modify: []func(*psbt.Packet){
			redeemScripts(p2wkh, p2wsh), noNonWitnessUtxo,
		},
		sigScripts: [][]byte{pushData(p2wkh), pushData(p2wsh)},
	}, {
		// Electrum leaves out the witness UTXO.
		name:      "nested SegWit, non-witness UTXO only",
		pkScripts: [][]byte{np2wkh, np2wsh},
		modify: []func(*psbt.Packet){
			redeemScripts(p2wkh, p2wsh), noWitnessUtxo,
		},
		sigScripts: [][]byte{pushData(p2wkh), pushData(p2wsh)},
	}, {
		name:      "native and nested SegWit",
		pkScripts: [][]byte{p2tr, np2wkh, p2wkh, np2wsh},
		modify: []func(*psbt.Packet){
			redeemScripts(nil, p2wkh, nil, p2wsh),
		},
		sigScripts: [][]byte{
			nil, pushData(p2wkh), nil, pushData(p2wsh),
		},
	}, {
		name:      "matching final signature script",
		pkScripts: [][]byte{np2wkh},
		modify: []func(*psbt.Packet){
			redeemScripts(p2wkh), finalScriptSig(pushData(p2wkh)),
		},
		sigScripts: [][]byte{pushData(p2wkh)},
	}, {
		name:        "nested SegWit without redeem script",
		pkScripts:   [][]byte{p2wkh, np2wkh},
		expectedErr: "input 1: P2SH input has no redeem script",
	}, {
		// A finalizer may remove the redeem script, and only keep the
		// witness UTXO and the final scripts.
		name:      "finalized nested SegWit",
		pkScripts: [][]byte{np2wkh},
		modify: []func(*psbt.Packet){
			noNonWitnessUtxo, finalScriptSig(pushData(p2wkh)),
		},
		expectedErr: "input 0: P2SH input has no redeem script",
	}, {
		name:      "redeem script not a witness program",
		pkScripts: [][]byte{bareP2SH},
		modify: []func(*psbt.Packet){
			redeemScripts(multiSigScript),
		},
		expectedErr: "input 0: P2SH input is not nested SegWit",
	}, {
		name:      "redeem script of another output",
		pkScripts: [][]byte{np2wkh},
		modify: []func(*psbt.Packet){
			redeemScripts(p2wsh),
		},
		expectedErr: "input 0: redeem script does not match",
	}, {
		name:        "P2PKH in witness UTXO",
		pkScripts:   [][]byte{p2pkh},
		modify:      []func(*psbt.Packet){noNonWitnessUtxo},
		expectedErr: "input 0: not a SegWit spend",
	}, {
		name:        "bare P2SH in witness UTXO",
		pkScripts:   [][]byte{bareP2SH},
		modify:      []func(*psbt.Packet){noNonWitnessUtxo},
		expectedErr: "input 0: P2SH input has no redeem script",
	}, {
		name:        "empty script in witness UTXO",
		pkScripts:   [][]byte{nil},
		modify:      []func(*psbt.Packet){noNonWitnessUtxo},
		expectedErr: "input 0: not a SegWit spend",
	}, {
		name:      "native SegWit with final signature script",
		pkScripts: [][]byte{p2wkh},
		modify: []func(*psbt.Packet){
			finalScriptSig(pushData(p2wkh)),
		},
		expectedErr: "input 0: final signature script is not",
	}, {
		name:      "nested SegWit with other final signature script",
		pkScripts: [][]byte{np2wkh},
		modify: []func(*psbt.Packet){
			redeemScripts(p2wkh), finalScriptSig(pushData(p2wsh)),
		},
		expectedErr: "input 0: final signature script is not",
	}, {
		name:      "non-witness UTXO of another transaction",
		pkScripts: [][]byte{p2wkh},
		modify: []func(*psbt.Packet){
			func(p *psbt.Packet) {
				txIn := p.UnsignedTx.TxIn[0]
				txIn.PreviousOutPoint.Hash = chainhash.Hash{1}
			},
		},
		expectedErr: "input 0: non-witness UTXO is not the " +
			"transaction",
	}, {
		name:      "non-witness UTXO without spent output",
		pkScripts: [][]byte{p2wkh},
		modify: []func(*psbt.Packet){
			func(p *psbt.Packet) {
				txIn := p.UnsignedTx.TxIn[0]
				txIn.PreviousOutPoint.Index = 2
			},
		},
		expectedErr: "input 0: non-witness UTXO has no output 2",
	}, {
		// Converted to a 32-bit int, this index would be negative.
		name:      "non-witness UTXO without spent output, high index",
		pkScripts: [][]byte{p2wkh},
		modify: []func(*psbt.Packet){
			func(p *psbt.Packet) {
				txIn := p.UnsignedTx.TxIn[0]
				txIn.PreviousOutPoint.Index = 1 << 31
			},
		},
		expectedErr: "input 0: non-witness UTXO has no output " +
			"2147483648",
	}, {
		name:      "UTXO fields with different scripts",
		pkScripts: [][]byte{p2wkh},
		modify: []func(*psbt.Packet){
			func(p *psbt.Packet) {
				p.Inputs[0].WitnessUtxo.PkScript = p2tr
			},
		},
		expectedErr: "input 0: witness UTXO and non-witness UTXO " +
			"differ",
	}, {
		name:      "UTXO fields with different values",
		pkScripts: [][]byte{p2wkh},
		modify: []func(*psbt.Packet){
			func(p *psbt.Packet) {
				p.Inputs[0].WitnessUtxo.Value++
			},
		},
		expectedErr: "input 0: witness UTXO and non-witness UTXO " +
			"differ",
	}, {
		name:      "no UTXO information",
		pkScripts: [][]byte{p2wkh},
		modify: []func(*psbt.Packet){
			noWitnessUtxo, noNonWitnessUtxo,
		},
		expectedErr: "input 0: no UTXO information",
	}, {
		name:      "PSBT inputs don't match transaction inputs",
		pkScripts: [][]byte{p2wkh, p2wkh},
		modify: []func(*psbt.Packet){
			func(p *psbt.Packet) {
				p.Inputs = p.Inputs[:1]
			},
		},
		expectedErr: "PSBT has 1 inputs, but its transaction has 2",
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			packet := newPacket(tc.pkScripts)
			for _, modify := range tc.modify {
				modify(packet)
			}

			// The PSBT itself must not be changed, so we compare
			// its serialization before and after.
			var before bytes.Buffer
			require.NoError(t, packet.Serialize(&before))

			tx, err := txWithFinalSigScripts(packet)

			var after bytes.Buffer
			require.NoError(t, packet.Serialize(&after))
			require.Equal(t, before.Bytes(), after.Bytes())

			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				require.Nil(t, tx)

				return
			}

			require.NoError(t, err)
			require.Len(t, tx.TxIn, len(tc.sigScripts))

			// Apart from the signature scripts, the transaction
			// must be the unsigned one. As it has no witness data,
			// its TXID covers all other fields.
			expectedTx := packet.UnsignedTx.Copy()
			for idx, txIn := range tx.TxIn {
				require.Equal(
					t, tc.sigScripts[idx],
					txIn.SignatureScript, "input %d", idx,
				)
				require.Empty(t, txIn.Witness, "input %d", idx)

				expectedTx.TxIn[idx].SignatureScript =
					tc.sigScripts[idx]
			}
			require.Equal(t, expectedTx.TxHash(), tx.TxHash())
		})
	}
}

// testSpendType is the type of an output that a test transaction spends.
type testSpendType uint8

const (
	// testSpendP2WKH is a native P2WKH output.
	testSpendP2WKH testSpendType = iota

	// testSpendNP2WKH is a P2WKH output nested in P2SH.
	testSpendNP2WKH

	// testSpendNP2WSH is a P2WSH output nested in P2SH. Its witness script
	// is a 1-of-1 multisig script, because the PSBT finalizer of btcd only
	// builds witnesses of multisig scripts for P2WSH.
	testSpendNP2WSH
)

// testSpend is an output that pays to the test key, together with everything
// that is needed to sign for it.
type testSpend struct {
	// txOut is the output that is spent.
	txOut *wire.TxOut

	// redeemScript is the redeem script of a nested SegWit output.
	redeemScript []byte

	// witnessScript is the witness script of a P2WSH output.
	witnessScript []byte

	// signScript is the script that the signature commits to.
	signScript []byte
}

// newTestSpend returns an output of the given type that pays to the key of
// localPrivkey. Its value is enough to fund a test channel on its own.
func newTestSpend(t *testing.T, spendType testSpendType) *testSpend {
	t.Helper()

	_, pubKey := btcec.PrivKeyFromBytes(localPrivkey)
	pubKeyBytes := pubKey.SerializeCompressed()

	var (
		spend          testSpend
		witnessProgram []byte
		err            error
	)
	switch spendType {
	case testSpendP2WKH, testSpendNP2WKH:
		witnessProgram, err = input.WitnessPubKeyHash(pubKeyBytes)
		require.NoError(t, err)

		spend.signScript = witnessProgram

	case testSpendNP2WSH:
		spend.witnessScript, err = txscript.NewScriptBuilder().
			AddOp(txscript.OP_1).AddData(pubKeyBytes).
			AddOp(txscript.OP_1).AddOp(txscript.OP_CHECKMULTISIG).
			Script()
		require.NoError(t, err)

		witnessProgram, err = input.WitnessScriptHash(
			spend.witnessScript,
		)
		require.NoError(t, err)

		spend.signScript = spend.witnessScript
	}

	pkScript := witnessProgram
	if spendType != testSpendP2WKH {
		spend.redeemScript = witnessProgram

		pkScript, err = input.GenerateP2SH(witnessProgram)
		require.NoError(t, err)
	}

	spend.txOut = &wire.TxOut{
		Value:    int64(chanCapacity) + 10_000,
		PkScript: pkScript,
	}

	return &spend
}

// addTestSpends adds an input to the PSBT for each of the given outputs. Each
// input spends its own previous transaction and has both UTXO fields set, like
// the inputs of a PSBT that lnd's wallet funds.
func addTestSpends(packet *psbt.Packet, spends []*testSpend) {
	for idx, spend := range spends {
		prevTx := wire.NewMsgTx(2)
		prevTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Index: uint32(idx)},
		})
		prevTx.AddTxOut(spend.txOut)

		packet.UnsignedTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{Hash: prevTx.TxHash()},
		})
		packet.Inputs = append(packet.Inputs, psbt.PInput{
			NonWitnessUtxo: prevTx,
			WitnessUtxo: &wire.TxOut{
				Value:    spend.txOut.Value,
				PkScript: spend.txOut.PkScript,
			},
			RedeemScript:  spend.redeemScript,
			WitnessScript: spend.witnessScript,
		})
	}
}

// testPrevOutFetcher returns a fetcher for the outputs that the inputs of the
// PSBT spend. Every input must have a witness UTXO.
func testPrevOutFetcher(packet *psbt.Packet) *txscript.MultiPrevOutFetcher {
	prevOuts := make(map[wire.OutPoint]*wire.TxOut)
	for idx, txIn := range packet.UnsignedTx.TxIn {
		prevOuts[txIn.PreviousOutPoint] = packet.Inputs[idx].WitnessUtxo
	}

	return txscript.NewMultiPrevOutFetcher(prevOuts)
}

// signTestSpends adds a signature of the test key to every input of the PSBT.
// The inputs must spend the given outputs, in that order.
func signTestSpends(t *testing.T, packet *psbt.Packet, spends []*testSpend) {
	t.Helper()

	privKey, pubKey := btcec.PrivKeyFromBytes(localPrivkey)

	tx := packet.UnsignedTx
	sigHashes := txscript.NewTxSigHashes(tx, testPrevOutFetcher(packet))
	for idx, spend := range spends {
		sig, err := txscript.RawTxInWitnessSignature(
			tx, sigHashes, idx, spend.txOut.Value,
			spend.signScript, txscript.SigHashAll, privKey,
		)
		require.NoError(t, err)

		packet.Inputs[idx].PartialSigs = []*psbt.PartialSig{{
			PubKey:    pubKey.SerializeCompressed(),
			Signature: sig,
		}}
	}
}

// extractSignedTx finalizes the signed PSBT with the finalizer of btcd,
// extracts the signed transaction, and checks that all its inputs are valid.
func extractSignedTx(t *testing.T, packet *psbt.Packet) *wire.MsgTx {
	t.Helper()

	require.NoError(t, psbt.MaybeFinalizeAll(packet))

	tx, err := psbt.Extract(packet)
	require.NoError(t, err)

	fetcher := testPrevOutFetcher(packet)
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	for idx, txIn := range tx.TxIn {
		prevOut := fetcher.FetchPrevOutput(txIn.PreviousOutPoint)
		vm, err := txscript.NewEngine(
			prevOut.PkScript, tx, idx,
			txscript.StandardVerifyFlags, nil, sigHashes,
			prevOut.Value, fetcher,
		)
		require.NoError(t, err)
		require.NoError(t, vm.Execute(), "input %d", idx)
	}

	return tx
}

// requirePsbtReady checks whether the PsbtReady channel of the intent was
// closed, which tells the funding manager to continue.
func requirePsbtReady(t *testing.T, intent *PsbtIntent, ready bool) {
	t.Helper()

	select {
	case _, ok := <-intent.PsbtReady:
		require.True(t, ready, "PSBT ready was signaled")
		require.False(t, ok, "PSBT ready channel was not closed")

	default:
		require.False(t, ready, "PSBT ready was not signaled")
	}
}

// TestPsbtVerifySkipFinalize makes sure that a channel that skips the PSBT
// finalize step is keyed to the TXID of the signed transaction. Signing a
// nested SegWit input adds a signature script, so that TXID is not the one of
// the unsigned transaction.
func TestPsbtVerifySkipFinalize(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		spendTypes []testSpendType
	}{{
		name:       "P2SH-P2WKH",
		spendTypes: []testSpendType{testSpendNP2WKH},
	}, {
		name:       "P2SH-P2WSH",
		spendTypes: []testSpendType{testSpendNP2WSH},
	}, {
		name: "native and nested SegWit",
		spendTypes: []testSpendType{
			testSpendP2WKH, testSpendNP2WKH, testSpendNP2WSH,
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			intent := newTestPsbtIntent(t, false)
			_, _, packet, err := intent.FundingParams()
			require.NoError(t, err)

			spends := make([]*testSpend, 0, len(tc.spendTypes))
			for _, spendType := range tc.spendTypes {
				spends = append(
					spends, newTestSpend(t, spendType),
				)
			}
			addTestSpends(packet, spends)

			require.NoError(t, intent.Verify(packet, true))
			require.Equal(t, PsbtFinalized, intent.State)
			requirePsbtReady(t, intent, true)

			_, err = intent.CompileFundingTx()
			require.NoError(t, err)

			chanPoint, err := intent.ChanPoint()
			require.NoError(t, err)

			// Sign a copy of the PSBT like a wallet would. The
			// channel must use the TXID of the signed transaction,
			// which differs from the unsigned TXID.
			signPacket := clonePsbt(t, packet)
			signTestSpends(t, signPacket, spends)
			signedTx := extractSignedTx(t, signPacket)

			require.Equal(t, signedTx.TxHash(), chanPoint.Hash)
			require.NotEqual(
				t, packet.UnsignedTx.TxHash(), chanPoint.Hash,
			)
		})
	}
}

// TestPsbtVerifySkipFinalizeReject makes sure that a PSBT is rejected if the
// TXID of the signed transaction can't be known when the finalize step is
// skipped, and that the intent is left as it was. The user can then retry with
// the same PSBT and the finalize step, or fix the PSBT and skip the finalize
// step again.
func TestPsbtVerifySkipFinalizeReject(t *testing.T) {
	t.Parallel()

	// rejectPsbt returns an intent that rejected a PSBT in the verify step
	// with skip_finalize, that PSBT, and the output its only input spends.
	rejectPsbt := func(t *testing.T) (*PsbtIntent, *psbt.Packet,
		*testSpend) {

		t.Helper()

		intent := newTestPsbtIntent(t, false)
		_, _, packet, err := intent.FundingParams()
		require.NoError(t, err)

		// Without the redeem script, the signature script of the
		// nested SegWit input can't be known.
		spend := newTestSpend(t, testSpendNP2WKH)
		addTestSpends(packet, []*testSpend{spend})
		packet.Inputs[0].RedeemScript = nil

		err = intent.Verify(packet, true)
		require.ErrorContains(t, err, "cannot skip finalize step")
		require.ErrorContains(
			t, err, "input 0: P2SH input has no redeem script",
		)

		require.Equal(t, PsbtOutputKnown, intent.State)
		require.Nil(t, intent.PendingPsbt)
		require.Nil(t, intent.FinalTX)
		requirePsbtReady(t, intent, false)

		return intent, packet, spend
	}

	// The same PSBT can be used with the finalize step.
	t.Run("retry with finalize step", func(t *testing.T) {
		t.Parallel()

		intent, packet, _ := rejectPsbt(t)

		require.NoError(t, intent.Verify(packet, false))
		require.Equal(t, PsbtVerified, intent.State)
		requirePsbtReady(t, intent, false)
	})

	// With the redeem script added back, the finalize step can be skipped
	// again, and the channel uses the TXID of the signed transaction.
	t.Run("retry with redeem script", func(t *testing.T) {
		t.Parallel()

		intent, packet, spend := rejectPsbt(t)
		packet.Inputs[0].RedeemScript = spend.redeemScript

		require.NoError(t, intent.Verify(packet, true))
		require.Equal(t, PsbtFinalized, intent.State)
		requirePsbtReady(t, intent, true)

		_, err := intent.CompileFundingTx()
		require.NoError(t, err)

		chanPoint, err := intent.ChanPoint()
		require.NoError(t, err)

		signPacket := clonePsbt(t, packet)
		signTestSpends(t, signPacket, []*testSpend{spend})
		signedTx := extractSignedTx(t, signPacket)
		require.Equal(t, signedTx.TxHash(), chanPoint.Hash)
	})
}

// TestPsbtVerifySkipFinalizeBatch makes sure that all channels of a batch that
// skip the PSBT finalize step use the TXID of the signed transaction, and that
// they don't change the PSBT they share.
func TestPsbtVerifySkipFinalizeBatch(t *testing.T) {
	t.Parallel()

	// The second channel has another remote key, so it gets its own
	// funding output.
	intent1 := newTestPsbtIntent(t, false)
	intent2 := newTestPsbtIntent(t, false)
	_, localPubkey := btcec.PrivKeyFromBytes(localPrivkey)
	_, otherPubkey := btcec.PrivKeyFromBytes([]byte{7, 8, 9})
	intent2.BindKeys(
		&keychain.KeyDescriptor{PubKey: localPubkey}, otherPubkey,
	)

	_, _, packet, err := intent1.FundingParams()
	require.NoError(t, err)

	_, fundingOutput2, err := intent2.FundingOutput()
	require.NoError(t, err)

	packet.UnsignedTx.AddTxOut(fundingOutput2)
	packet.Outputs = append(packet.Outputs, psbt.POutput{})

	spends := []*testSpend{
		newTestSpend(t, testSpendP2WKH),
		newTestSpend(t, testSpendNP2WKH),
	}
	addTestSpends(packet, spends)

	var before bytes.Buffer
	require.NoError(t, packet.Serialize(&before))

	require.NoError(t, intent1.Verify(packet, true))
	require.NoError(t, intent2.Verify(packet, true))

	var after bytes.Buffer
	require.NoError(t, packet.Serialize(&after))
	require.Equal(t, before.Bytes(), after.Bytes())

	_, err = intent1.CompileFundingTx()
	require.NoError(t, err)

	_, err = intent2.CompileFundingTx()
	require.NoError(t, err)

	chanPoint1, err := intent1.ChanPoint()
	require.NoError(t, err)

	chanPoint2, err := intent2.ChanPoint()
	require.NoError(t, err)

	signPacket := clonePsbt(t, packet)
	signTestSpends(t, signPacket, spends)
	signedTx := extractSignedTx(t, signPacket)

	require.Equal(t, signedTx.TxHash(), chanPoint1.Hash)
	require.Equal(t, signedTx.TxHash(), chanPoint2.Hash)
	require.NotEqual(t, chanPoint1.Index, chanPoint2.Index)

	// The intents keep copies of the transaction, so changing the PSBT
	// afterwards doesn't change them.
	packet.UnsignedTx.LockTime = 1
	packet.UnsignedTx.TxIn[0].Sequence = 1
	require.Equal(t, chanPoint1.Hash, intent1.FinalTX.TxHash())
	require.Equal(t, chanPoint2.Hash, intent2.FinalTX.TxHash())
}

// TestPsbtFinalizeNested makes sure that a channel that goes through the PSBT
// finalize step is keyed to the TXID of the signed transaction if it spends
// nested SegWit inputs, whether the channel publishes the transaction or not.
func TestPsbtFinalizeNested(t *testing.T) {
	t.Parallel()

	for _, shouldPublish := range []bool{true, false} {
		name := fmt.Sprintf("publish=%v", shouldPublish)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			intent := newTestPsbtIntent(t, shouldPublish)
			_, _, packet, err := intent.FundingParams()
			require.NoError(t, err)

			spends := []*testSpend{
				newTestSpend(t, testSpendNP2WKH),
				newTestSpend(t, testSpendNP2WSH),
			}
			addTestSpends(packet, spends)

			require.NoError(t, intent.Verify(packet, false))
			require.Equal(t, PsbtVerified, intent.State)

			// Sign a copy of the PSBT like a wallet would, and let
			// the intent finalize it.
			signPacket := clonePsbt(t, packet)
			signTestSpends(t, signPacket, spends)
			require.NoError(t, intent.Finalize(signPacket))
			requirePsbtReady(t, intent, true)

			_, err = intent.CompileFundingTx()
			require.NoError(t, err)

			chanPoint, err := intent.ChanPoint()
			require.NoError(t, err)

			signedTx := extractSignedTx(t, signPacket)
			require.Equal(t, signedTx.TxHash(), chanPoint.Hash)
			require.NotEqual(
				t, packet.UnsignedTx.TxHash(), chanPoint.Hash,
			)
		})
	}
}

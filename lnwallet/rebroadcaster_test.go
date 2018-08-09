package lnwallet

import (
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/keychain"
)

type emptyMockSigner struct{}

var _ Signer = (*emptyMockSigner)(nil)

func (s *emptyMockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *SignDescriptor) ([]byte, error) {

	return nil, nil
}

func (s *emptyMockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *SignDescriptor) (*InputScript, error) {

	return nil, nil
}

// TestRebroadcasterFeeBump ensures that the rebroadcaster can properly bump a
// transaction's current fee until it is deemed sufficient to be broadcast to
// the network. It should be able to bump the fee successfully without exceeding
// MaxAttempts and MaxFee.
func TestRebroadcasterFeeBump(t *testing.T) {
	t.Parallel()

	// We'll start off by making a copy of the transaction to prevent
	// mutating the state of the global variable.
	tx := testTx.Copy()

	// Calculate the current fee of the transaction.
	outputAmt := btcutil.Amount(tx.TxOut[0].Value)
	inputAmt := outputAmt + btcutil.SatoshiPerBitcoin
	fee := inputAmt - outputAmt

	// We'll assume that the current fee is insufficient and that a
	// sufficient fee should be 1.5x more.
	sufficientFee := fee + (btcutil.SatoshiPerBitcoin / 2)

	// Recreate the sign descriptor needed in order to re-sign this
	// transaction.
	testPrivKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)
	testPubKey := testPrivKey.PubKey()
	signDesc := &WitnessSignDescriptor{
		WitnessType: P2WPKH,
		SignDescriptor: &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: testPubKey,
			},
			SigHashes: txscript.NewTxSigHashes(tx),
		},
	}
	signDescs := []*WitnessSignDescriptor{signDesc}

	// We'll now set our constraints on the rebroadcaster. The fee will
	// increase by 10% at every iteration. It should be able to reach the
	// sufficientFee without exceeding maxAttempts and maxFee.
	const bumpPercentage = 10
	const maxAttempts = 10
	maxFee := 2 * fee

	cfg := TxRebroadcasterCfg{
		Tx:                tx,
		ChangeOutputIndex: 0,
		Signer:            &emptyMockSigner{},
		WitnessSignDescs:  signDescs,
		BumpPercentage:    bumpPercentage,
		MaxAttempts:       maxAttempts,
		MaxFee:            maxFee,
		DustLimit:         DefaultDustLimit(),
		FetchInputAmount: func(*wire.OutPoint) (btcutil.Amount, error) {
			return inputAmt, nil
		},
		Broadcast: func(tx *wire.MsgTx) error {
			// Calculate the fee for the current version of the
			// transaction being broadcast and check if it's still
			// not enough.
			fee := inputAmt - btcutil.Amount(tx.TxOut[0].Value)
			if fee < sufficientFee {
				return ErrInsufficientFee
			}
			return nil
		},
	}

	rebroadcaster, err := NewTxRebroadcaster(cfg)
	if err != nil {
		t.Fatalf("unable to create tx rebroadcaster: %v", err)
	}

	// Kick off the rebroadcaster for this transaction.
	errChan := rebroadcaster.Rebroadcast(nil)
	select {
	case err = <-errChan:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// The transaction should have been rebroadcasted successfully.
	if err != nil {
		t.Fatalf("received unexpected error: %v", err)
	}

	// Ensure that the last fee used is above the sufficientFee and below
	// the maxFee.
	lastFee := rebroadcaster.LastFeeUsed()
	if lastFee < sufficientFee {
		t.Fatalf("expected last fee used to be greater than %v, got %v",
			sufficientFee, lastFee)
	}
	if lastFee > maxFee {
		t.Fatalf("expected last fee used to be less than max %v, got %v",
			maxFee, lastFee)
	}
}

// TestRebroadcasterMaxFee ensures that the rebroadcaster respects its MaxFee
// limit by halting the rebroadcast of the transaction after exceeding the
// maximum allowed fee.
func TestRebroadcasterMaxFee(t *testing.T) {
	t.Parallel()

	// We'll start off by making a copy of the transaction to prevent
	// mutating the state of the global variable.
	tx := testTx.Copy()

	// Calculate the current fee of the transaction.
	outputAmt := btcutil.Amount(tx.TxOut[0].Value)
	inputAmt := outputAmt + btcutil.SatoshiPerBitcoin
	fee := btcutil.Amount(inputAmt - outputAmt)

	// Recreate the sign descriptor needed in order to re-sign this
	// transaction.
	testPrivKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)
	testPubKey := testPrivKey.PubKey()
	signDesc := &WitnessSignDescriptor{
		WitnessType: P2WPKH,
		SignDescriptor: &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: testPubKey,
			},
			SigHashes: txscript.NewTxSigHashes(tx),
		},
	}
	signDescs := []*WitnessSignDescriptor{signDesc}

	// Set the constraints on the rebroadcaster. We should continue to
	// rebroadcast the transaction until we've reached the max fee, which
	// should then trigger the rebroadcaster to halt.
	const bumpPercentage = 10
	maxFee := fee + (btcutil.SatoshiPerBitcoin / 2)

	cfg := TxRebroadcasterCfg{
		Tx:                tx,
		ChangeOutputIndex: 0,
		Signer:            &emptyMockSigner{},
		WitnessSignDescs:  signDescs,
		BumpPercentage:    bumpPercentage,
		MaxFee:            maxFee,
		DustLimit:         DefaultDustLimit(),
		FetchInputAmount: func(*wire.OutPoint) (btcutil.Amount, error) {
			return inputAmt, nil
		},
		// Broadcast will always return ErrInsufficientFee in order to
		// keep bumping up the fee and rebroadcasting until reaching the
		// maximum fee allowed.
		Broadcast: func(*wire.MsgTx) error {
			return ErrInsufficientFee
		},
	}

	rebroadcaster, err := NewTxRebroadcaster(cfg)
	if err != nil {
		t.Fatalf("unable to create tx rebroadcaster: %v", err)
	}

	// Kick off the rebroadcaster for this transaction.
	errChan := rebroadcaster.Rebroadcast(nil)
	select {
	case err = <-errChan:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// The rebroadcaster should halt indicating that it reached its maximum
	// fee allowed while attempting to rebroadcast the transaction.
	if err != ErrRebroadcastMaxFee {
		t.Fatalf("expected ErrRebroadcastMaxFee, got %v", err)
	}

	// Ensure the last fee used is below the maximum fee allowed.
	lastFee := rebroadcaster.LastFeeUsed()
	if lastFee > maxFee {
		t.Fatalf("expected last fee used to be less than %v, got %v",
			cfg.MaxFee, lastFee)
	}
}

// TestRebroadcasterMaxAttempts ensures that the rebroadcaster respects its
// MaxAttempts limit by halting the rebroadcast of the transaction after
// exceeding the maximum allowed number of attempts.
func TestRebroadcasterMaxAttempts(t *testing.T) {
	t.Parallel()

	// We'll start off by making a copy of the transaction to prevent
	// mutating the state of the global variable.
	tx := testTx.Copy()

	// We'll assume that the input amount is the output's + 1 BTC,
	// indicating there is a 1 BTC fee on the transaction.
	outputAmt := btcutil.Amount(tx.TxOut[0].Value)
	inputAmt := outputAmt + btcutil.SatoshiPerBitcoin

	// Recreate the sign descriptor needed in order to re-sign this
	// transaction.
	testPrivKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)
	testPubKey := testPrivKey.PubKey()
	signDesc := &WitnessSignDescriptor{
		WitnessType: P2WPKH,
		SignDescriptor: &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: testPubKey,
			},
			SigHashes: txscript.NewTxSigHashes(tx),
		},
	}
	signDescs := []*WitnessSignDescriptor{signDesc}

	// The rebroadcaster should not attempt to rebroadcast this transaction
	// more than 10 times.
	const maxAttempts = 10

	cfg := TxRebroadcasterCfg{
		Tx:                tx,
		ChangeOutputIndex: 0,
		Signer:            &emptyMockSigner{},
		WitnessSignDescs:  signDescs,
		BumpPercentage:    10,
		MaxAttempts:       maxAttempts,
		DustLimit:         DefaultDustLimit(),
		MaxFee:            btcutil.Amount(outputAmt),
		FetchInputAmount: func(*wire.OutPoint) (btcutil.Amount, error) {
			return inputAmt, nil
		},
		// Broadcast will always return ErrInsufficientFee in order to
		// keep bumping up the fee and rebroadcasting until reaching the
		// maximum number of attempts allowed.
		Broadcast: func(*wire.MsgTx) error {
			return ErrInsufficientFee
		},
	}

	rebroadcaster, err := NewTxRebroadcaster(cfg)
	if err != nil {
		t.Fatalf("unable to create tx rebroadcaster: %v", err)
	}

	// Kick off the rebroadcaster.
	errChan := rebroadcaster.Rebroadcast(nil)
	select {
	case err = <-errChan:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// The rebroadcaster should halt indicating that it reached it's maximum
	// number of rebroadcast attempts allowed.
	if err != ErrRebroadcastMaxAttempts {
		t.Fatalf("expected ErrMaxRebroadcastAttempts, got %v", err)
	}

	// The number of attempts should match the max number allowed.
	numAttempts := rebroadcaster.NumAttempts()
	if numAttempts != cfg.MaxAttempts {
		t.Fatalf("expected %v attempts, got %v", cfg.MaxAttempts,
			numAttempts)
	}
}

// TestRebroadcasterDustLimit ensures that the rebroadcaster takes into account
// what we consider as "dust" on the chain. When bumping up the fee on a
// transaction, if the change outputs dips below this limit, then we should stop
// attempting to rebroadcast the transaction.
func TestRebroadcasterDustLimit(t *testing.T) {
	t.Parallel()

	// We'll start off by making a copy of the transaction to prevent
	// mutating the state of the global variable.
	tx := testTx.Copy()

	// We'll assume that the input amount is the output's + 1 BTC,
	// indicating there is a 1 BTC fee on the transaction.
	outputAmt := btcutil.Amount(tx.TxOut[0].Value)
	inputAmt := outputAmt + btcutil.SatoshiPerBitcoin

	// We'll now set our constraints on the rebroadcaster. Since we're
	// interested in detecting when a change output dips below the dust
	// limit after applying the new fee, we'll set the dust limit to be 1
	// satoshi below the current output's value, so that the check gets
	// triggered at the first attempt of rebroadcasting.
	const bumpPercentage = 100
	const maxAttempts = 10
	dustLimit := outputAmt - 1

	// Recreate the sign descriptor needed in order to re-sign this
	// transaction.
	testPrivKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)
	testPubKey := testPrivKey.PubKey()
	signDesc := &WitnessSignDescriptor{
		WitnessType: P2WPKH,
		SignDescriptor: &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: testPubKey,
			},
			SigHashes: txscript.NewTxSigHashes(tx),
		},
	}
	signDescs := []*WitnessSignDescriptor{signDesc}

	cfg := TxRebroadcasterCfg{
		Tx:                tx,
		ChangeOutputIndex: 0,
		Signer:            &emptyMockSigner{},
		WitnessSignDescs:  signDescs,
		BumpPercentage:    bumpPercentage,
		MaxAttempts:       maxAttempts,
		FetchInputAmount: func(*wire.OutPoint) (btcutil.Amount, error) {
			return inputAmt, nil
		},
		// Broadcast will always return ErrInsufficientFee in order to
		// keep bumping up the fee and rebroadcasting until the output
		// dips below the dust limit.
		Broadcast: func(*wire.MsgTx) error {
			return ErrInsufficientFee
		},
		// The output should not be consumed for this test.
		DustLimit:           dustLimit,
		ConsumeChangeOutput: false,
	}

	rebroadcaster, err := NewTxRebroadcaster(cfg)
	if err != nil {
		t.Fatalf("unable to create tx rebroadcaster: %v", err)
	}

	// Kick off the rebroadcaster.
	errChan := rebroadcaster.Rebroadcast(nil)
	select {
	case err = <-errChan:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// The rebroadcaster should halt indicating that the change output
	// dipped below the dust limit.
	if err != ErrRebroadcastDustLimit {
		t.Fatalf("expected ErrRebroadcastDustLimit, got %v", err)
	}
}

// TestRebroadcasterConsumeOutput ensures that the rebroadcaster takes into
// account what we consider as "dust" on the chain. When bumping up the fee on a
// transaction with multiple outputs, if the change outputs dips below this
// limit, then we should just consume the output as a whole.
func TestRebroadcasterConsumeOutput(t *testing.T) {
	t.Parallel()

	// We'll start off by making a copy of the transaction to prevent
	// mutating the state of the global variable.
	tx := testTx.Copy()

	// Since this test will be consuming an output, we'll add another one
	// such that the transaction is still valid and can be rebroadcast.
	// This new output will be seen as the change output.
	tx.AddTxOut(&wire.TxOut{Value: btcutil.SatoshiPerBitcoin})

	// We'll assume that the input amount is the output's + 1 BTC,
	// indicating there is a 1 BTC fee on the transaction.
	var outputAmt btcutil.Amount
	for _, output := range tx.TxOut {
		outputAmt += btcutil.Amount(output.Value)
	}
	inputAmt := outputAmt + btcutil.SatoshiPerBitcoin

	// We'll also declare a sufficent fee for this transaction as being at
	// least 2 BTC. By consuming the change output above, we should be able
	// successfully rebroadcast the transaction.
	sufficientFee := btcutil.Amount(2 * btcutil.SatoshiPerBitcoin)

	// Recreate the sign descriptor needed in order to re-sign this
	// transaction.
	testPrivKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)
	testPubKey := testPrivKey.PubKey()
	signDesc := &WitnessSignDescriptor{
		WitnessType: P2WPKH,
		SignDescriptor: &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: testPubKey,
			},
			SigHashes: txscript.NewTxSigHashes(tx),
		},
	}
	signDescs := []*WitnessSignDescriptor{signDesc}

	// We'll now set our constraints on the rebroadcaster. Since we're
	// interested in detecting when a change output dips below the dust
	// limit after applying the new fee, we'll set the dust limit to be 1
	// satoshi below the current output's value, so that the check gets
	// triggered at the first attempt of rebroadcasting. Since the output
	// should also be consumed, we'll set that as well.
	const bumpPercentage = 100
	const maxAttempts = 10
	dustLimit := outputAmt - 1

	cfg := TxRebroadcasterCfg{
		Tx:                  tx,
		ChangeOutputIndex:   1,
		Signer:              &emptyMockSigner{},
		WitnessSignDescs:    signDescs,
		BumpPercentage:      bumpPercentage,
		MaxAttempts:         maxAttempts,
		DustLimit:           dustLimit,
		ConsumeChangeOutput: true,
		FetchInputAmount: func(*wire.OutPoint) (btcutil.Amount, error) {
			return inputAmt, nil
		},
		// Broadcast will always return ErrInsufficientFee in order to
		// keep bumping up the fee and rebroadcasting until the output
		// dips below the dust limit.
		Broadcast: func(*wire.MsgTx) error {
			// Calculate the fee for the current version of the
			// transaction being broadcast and check if it's still
			// not enough.
			var outputAmt btcutil.Amount
			for _, output := range tx.TxOut {
				outputAmt += btcutil.Amount(output.Value)
			}
			fee := inputAmt - outputAmt
			if fee < sufficientFee {
				return ErrInsufficientFee
			}
			return nil
		},
	}

	rebroadcaster, err := NewTxRebroadcaster(cfg)
	if err != nil {
		t.Fatalf("unable to create tx rebroadcaster: %v", err)
	}

	// Kick off the rebroadcaster.
	errChan := rebroadcaster.Rebroadcast(nil)
	select {
	case err = <-errChan:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// The transaction should have been rebroadcast successfully.
	if err != nil {
		t.Fatalf("unable to rebroadcast tx: %v", err)
	}

	// Confirm that the transaction still has its other output left.
	lastTxBroadcast := rebroadcaster.LastTxBroadcast()
	if len(lastTxBroadcast.TxOut) != 1 {
		t.Fatalf("expected transaction to have one output left, got %v",
			len(lastTxBroadcast.TxOut))
	}

	// Since the change output was added without a PkScript, we'll check
	// that to determine whether the correct output was consumed.
	if lastTxBroadcast.TxOut[0].PkScript == nil {
		t.Fatal("wrong output consumed")
	}
}

// TestRebroadcasterConsumeOnlyOutput ensures that the rebroadcaster takes into
// account what we consider as "dust" on the chain. When bumping up the fee on a
// transaction with one output, if the change outputs dips below this limit,
// then we should just consume the output as a whole. This leads to the
// transaction being invalid as it no longer has any outputs.
func TestRebroadcasterConsumeOnlyOutput(t *testing.T) {
	t.Parallel()

	// We'll start off by making a copy of the transaction to prevent
	// mutating the state of the global variable.
	tx := testTx.Copy()

	// We'll assume that the input amount is the output's + 1 BTC,
	// indicating there is a 1 BTC fee on the transaction.
	outputAmt := btcutil.Amount(tx.TxOut[0].Value)
	inputAmt := outputAmt + btcutil.SatoshiPerBitcoin

	// Recreate the sign descriptor needed in order to re-sign this
	// transaction.
	testPrivKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)
	testPubKey := testPrivKey.PubKey()
	signDesc := &WitnessSignDescriptor{
		WitnessType: P2WPKH,
		SignDescriptor: &SignDescriptor{
			KeyDesc: keychain.KeyDescriptor{
				PubKey: testPubKey,
			},
			SigHashes: txscript.NewTxSigHashes(tx),
		},
	}
	signDescs := []*WitnessSignDescriptor{signDesc}

	// We'll now set our constraints on the rebroadcaster. Since we're
	// interested in detecting when a change output dips below the dust
	// limit after applying the new fee, we'll set the dust limit to be 1
	// satoshi below the current output's value, so that the check gets
	// triggered at the first attempt of rebroadcasting. Since the output
	// should also be consumed, we'll set that as well.
	const bumpPercentage = 100
	const maxAttempts = 10
	dustLimit := outputAmt - 1

	cfg := TxRebroadcasterCfg{
		Tx:                  tx,
		ChangeOutputIndex:   0,
		Signer:              &emptyMockSigner{},
		WitnessSignDescs:    signDescs,
		BumpPercentage:      bumpPercentage,
		MaxAttempts:         maxAttempts,
		DustLimit:           dustLimit,
		ConsumeChangeOutput: true,
		FetchInputAmount: func(*wire.OutPoint) (btcutil.Amount, error) {
			return inputAmt, nil
		},
		// Broadcast will always return ErrInsufficientFee in order to
		// keep bumping up the fee and rebroadcasting until the output
		// dips below the dust limit.
		Broadcast: func(*wire.MsgTx) error {
			return ErrInsufficientFee
		},
	}

	rebroadcaster, err := NewTxRebroadcaster(cfg)
	if err != nil {
		t.Fatalf("unable to create tx rebroadcaster: %v", err)
	}

	// Kick off the rebroadcaster.
	errChan := rebroadcaster.Rebroadcast(nil)
	select {
	case err = <-errChan:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// Since the transaction only includes one output which has been
	// consumed, the transaction is no longer valid, so the rebroadcaster
	// should halt indicating such an error.
	if !strings.Contains(err.Error(), "no outputs") {
		t.Fatalf("expected transaction to have no outputs, got: %v",
			err)
	}
}

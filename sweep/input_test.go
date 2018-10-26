package sweep_test

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
)

type setWitnessStackTestCase struct {
	name          string
	outpoint      wire.OutPoint
	witnessStack  [][]byte
	witnessScript []byte
}

var setWitnessStackTestCases = []setWitnessStackTestCase{
	{
		name:          "nil witness stack, nil witness script",
		outpoint:      wire.OutPoint{},
		witnessStack:  nil,
		witnessScript: nil,
	},
	{
		name:          "nil witness stack, non-nil witness script",
		outpoint:      wire.OutPoint{},
		witnessStack:  nil,
		witnessScript: bytes.Repeat([]byte("a"), 32),
	},
	{
		name:          "non-nil witness stack, nil witness script",
		outpoint:      wire.OutPoint{},
		witnessStack:  [][]byte{bytes.Repeat([]byte("b"), 64)},
		witnessScript: nil,
	},
	{
		name:          "non-nil witness stack, non-nil witness script",
		outpoint:      wire.OutPoint{},
		witnessStack:  [][]byte{bytes.Repeat([]byte("b"), 64)},
		witnessScript: bytes.Repeat([]byte("a"), 32),
	},
	{
		name:     "multi witness stack, non-nil witness script",
		outpoint: wire.OutPoint{},
		witnessStack: [][]byte{
			bytes.Repeat([]byte("b"), 64),
			bytes.Repeat([]byte("c"), 32),
			nil,
			bytes.Repeat([]byte("hello"), 2),
		},
		witnessScript: bytes.Repeat([]byte("a"), 32),
	},
}

// TestBaseInputSetWitnessStack verifies that setting an externally computed witness
// is properly returned from Witness(), and that calls fail if no witness has
// been set.
func TestBaseInputSetWitnessStack(t *testing.T) {
	for _, test := range setWitnessStackTestCases {
		t.Run(test.name, func(t *testing.T) {
			testBaseInputSetWitnessStack(t, test)
		})
	}
}

func testBaseInputSetWitnessStack(t *testing.T, test setWitnessStackTestCase) {
	// Populate the witness script in the input's sign descriptor.
	signDesc := &lnwallet.SignDescriptor{
		WitnessScript: test.witnessScript,
	}

	// Construct a fresh input to test setting the witness stack. The
	// witness type should be irrelevant in this test.
	input := sweep.MakeBaseInput(
		&test.outpoint, lnwallet.PreAuthorized, signDesc,
	)

	// Attempting to retrieve the witness without calling SetWitnessStack
	// should fail.
	_, err := input.Witness()
	if err != sweep.ErrNoWitnessStack {
		t.Fatalf("expected error %v, got %v",
			sweep.ErrNoWitnessStack, err)
	}

	// Now, set the witness stack provided by the test vector. Nil-valued
	// witness stacks should have no effect.
	input.SetWitnessStack(test.witnessStack)

	// Attempt to retrieve the final witness.
	witness, err := input.Witness()
	switch {

	// If the witness stack was set with a nil-value, there will be no final
	// witness produced. We simply check that the error returned signals
	// that no witness stack has been provided.
	case test.witnessStack == nil:
		if err != sweep.ErrNoWitnessStack {
			t.Fatalf("unexpected error after setting nil "+
				"witness stack: want %v, got %v",
				sweep.ErrNoWitnessStack, err)
		}
		return

	// Otherwise, a non-nil witness was provided and fetching the witness
	// should result in no errors.
	case test.witnessStack != nil:
		if err != nil {
			t.Fatalf("unexpected error after setting non-nil "+
				"witness stack: %v", err)
		}
	}

	// Verify that the length of the witness is one more than the witness
	// stack, indicating that the witness script was appended.
	if len(witness) != len(test.witnessStack)+1 {
		t.Fatalf("witness has invalid length, want %v, got %v",
			len(test.witnessStack)+1, len(witness))
	}

	// Verify that the witness stack elements were copied as the witness's
	// prefix.
	for i := 0; i < len(test.witnessStack); i++ {
		if !bytes.Equal(witness[0], test.witnessStack[0]) {
			t.Fatalf("%d-th element in witness stack is incorrect: "+
				"want %x, got %x", i,
				test.witnessStack[i], witness[i])
		}
	}

	// Verify the last element in the witness matches the intended witness
	// script.
	if !bytes.Equal(witness[len(witness)-1], test.witnessScript) {
		t.Fatalf("witness script is incorrect: "+
			"want %x, got %x", test.witnessScript, witness[len(witness)-1])
	}
}

// TestHtlcSucceedInputSetWitnessStack verifies that setting an externally
// computed witness is properly returned from Witness(), and that calls fail if
// no witness has been set.
func TestHtlcSucceedInputSetWitnessStack(t *testing.T) {
	for _, test := range setWitnessStackTestCases {
		t.Run(test.name, func(t *testing.T) {
			testHtlcSucceedInputSetWitnessStack(t, test)
		})
	}
}

func testHtlcSucceedInputSetWitnessStack(t *testing.T, test setWitnessStackTestCase) {
	// Populate the witness script in the input's sign descriptor.
	signDesc := &lnwallet.SignDescriptor{
		WitnessScript: test.witnessScript,
	}

	// Construct a fresh htlc succeed input to test setting the witness
	// stack.
	input := sweep.MakeHtlcSucceedInput(&test.outpoint, signDesc, nil)

	// Attempting to retrieve the witness without calling SetWitnessStack
	// should fail.
	_, err := input.Witness()
	if err != sweep.ErrNoWitnessStack {
		t.Fatalf("expected error %v, got %v",
			sweep.ErrNoWitnessStack, err)
	}

	// Now, set the witness stack provided by the test vector. All witness
	// stacks should have no effect.
	input.SetWitnessStack(test.witnessStack)

	// Attempting to retrieve the witness without calling SetWitnessStack
	// should fail.
	if err != sweep.ErrNoWitnessStack {
		t.Fatalf("unexpected error after setting nil "+
			"witness stack: want %v, got %v",
			sweep.ErrNoWitnessStack, err)
	}

	// Attempt to retrieve the final witness. This should always fail since
	// the htlc succeed input's SetWitnessStack is implemented as a NOP.
	_, err = input.Witness()
	if err != sweep.ErrNoWitnessStack {
		t.Fatalf("unexpected error after setting nil "+
			"witness stack: want %v, got %v",
			sweep.ErrNoWitnessStack, err)
	}
}

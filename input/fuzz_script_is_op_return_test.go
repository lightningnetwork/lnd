package input

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

// FuzzScriptIsOpReturn fuzzes the ScriptIsOpReturn function.
func FuzzScriptIsOpReturn(f *testing.F) {
	// Seed the corpus with some representative inputs.
	f.Add([]byte{})
	f.Add([]byte{txscript.OP_RETURN})

	// Use our canonical ScriptBuilder to produce a valid OP_RETURN script.
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_RETURN)
	builder.AddData([]byte("valid data"))
	script, err := builder.Script()
	require.NoError(f, err)
	f.Add(script)

	// An example of a script that does not start with OP_RETURN.
	f.Add([]byte{txscript.OP_DUP, txscript.OP_RETURN})

	f.Fuzz(func(t *testing.T, pkScript []byte) {
		result := ScriptIsOpReturn(pkScript)

		var expected bool
		if len(script) == 0 || script[0] != txscript.OP_RETURN {
			expected = false
		} else {
			scriptClass, _, _, err := txscript.ExtractPkScriptAddrs(
				pkScript, &chaincfg.MainNetParams,
			)
			if err != nil {
				t.Fatalf("unable to extract pk script "+
					"addresses: %v", err)
			}

			expected = scriptClass == txscript.NullDataTy
		}

		if result != expected {
			t.Fatalf("for script %x, expected ScriptIsOpReturn=%v;"+
				" got %v", script, expected, result)
		}
	})
}

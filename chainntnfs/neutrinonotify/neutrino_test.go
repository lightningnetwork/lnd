package neutrinonotify

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/require"
)

var testP2SHPkScript = []byte{
	txscript.OP_HASH160,
	txscript.OP_DATA_20,
	0x90, 0x1c, 0x86, 0x94, 0xc0, 0x3f, 0xaf, 0xd5,
	0x52, 0x28, 0x10, 0xe0, 0x33, 0x0f, 0x26, 0xe6,
	0x7a, 0x85, 0x33, 0xcd,
	txscript.OP_EQUAL,
}

// TestPkScriptsToAddrsRejectsUnsupportedScripts ensures scripts that
// cannot be converted into neutrino filter addresses are rejected.
func TestPkScriptsToAddrsRejectsUnsupportedScripts(t *testing.T) {
	t.Parallel()

	_, err := pkScriptsToAddrs(
		[][]byte{{txscript.OP_TRUE}}, chaincfg.MainNetParams,
	)
	require.ErrorIs(t, err, chainntnfs.ErrUnsupportedPkScript)
}

// TestPkScriptsToAddrsDeduplicatesScripts ensures duplicate scripts only
// produce one neutrino filter address.
func TestPkScriptsToAddrsDeduplicatesScripts(t *testing.T) {
	t.Parallel()

	addrs, err := pkScriptsToAddrs(
		[][]byte{testP2SHPkScript, testP2SHPkScript},
		chaincfg.MainNetParams,
	)
	require.NoError(t, err)
	require.Len(t, addrs, 1)
}

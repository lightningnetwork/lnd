//go:build integration

package itest

import (
	"io"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/neutrino/chainimport"
	"github.com/lightninglabs/neutrino/headerfs"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/require"
)

// testNeutrinoHeadersImport verifies that a neutrino node can import block
// and filter headers from pre-built files, allowing it to sync faster than
// downloading headers one-by-one via P2P.
func testNeutrinoHeadersImport(ht *lntest.HarnessTest) {
	// This test only applies to the neutrino backend.
	if !ht.IsNeutrinoBackend() {
		ht.Skipf("skipping neutrino headers import test " +
			"for non-neutrino backend")
	}

	// Mine blocks so there is a meaningful chain to sync.
	const numBlocks = 50
	ht.MineBlocks(numBlocks)

	// Start a reference node that syncs normally via P2P. NewNode waits
	// for blockchain sync automatically.
	refNode := ht.NewNode("Reference", nil)
	refInfo := refNode.RPC.GetInfo()
	require.True(ht, refInfo.SyncedToChain,
		"reference node should be synced")

	bestHeight := refInfo.BlockHeight

	// Locate the header files created by the reference node's neutrino
	// backend. These are stored as flat binary files in the chain data
	// directory.
	netName := chaincfg.RegressionNetParams.Name
	chainDir := filepath.Join(
		refNode.Cfg.DataDir, "chain", "bitcoin", netName,
	)
	blockHeadersPath := filepath.Join(chainDir, "block_headers.bin")
	filterHeadersPath := filepath.Join(
		chainDir, "reg_filter_headers.bin",
	)

	// Verify the header files exist.
	_, err := os.Stat(blockHeadersPath)
	require.NoError(ht, err, "block_headers.bin not found")
	_, err = os.Stat(filterHeadersPath)
	require.NoError(ht, err, "reg_filter_headers.bin not found")

	// Copy the header files to a temporary directory for import.
	importDir := ht.T.TempDir()
	blockImportPath := filepath.Join(importDir, "block_headers.bin")
	filterImportPath := filepath.Join(
		importDir, "reg_filter_headers.bin",
	)

	copyFile(ht, blockHeadersPath, blockImportPath)
	copyFile(ht, filterHeadersPath, filterImportPath)

	// Add import metadata to the copied files. The metadata prepend
	// includes network magic, version, header type, and start height.
	// This is required by neutrino's chainimport package.
	err = chainimport.AddHeadersImportMetadata(
		blockImportPath, chaincfg.RegressionNetParams.Net,
		0, headerfs.Block, 0,
	)
	require.NoError(ht, err, "failed to add block header metadata")

	err = chainimport.AddHeadersImportMetadata(
		filterImportPath, chaincfg.RegressionNetParams.Net,
		0, headerfs.RegularFilter, 0,
	)
	require.NoError(ht, err, "failed to add filter header metadata")

	// Shut down the reference node to free resources.
	ht.Shutdown(refNode)

	// Start a new node configured to import headers from the prepared
	// files. The node imports the headers from file before falling back
	// to P2P sync for any remaining blocks.
	importArgs := []string{
		"--neutrino.blockheaderssource=" + blockImportPath,
		"--neutrino.filterheaderssource=" + filterImportPath,
	}
	importNode := ht.NewNode("Import", importArgs)

	// Verify the import node synced to the chain.
	importInfo := importNode.RPC.GetInfo()
	require.True(ht, importInfo.SyncedToChain,
		"import node should be synced to chain")
	require.GreaterOrEqual(
		ht, importInfo.BlockHeight, bestHeight,
		"import node should have at least the same height as "+
			"the reference node",
	)

	// Mine additional blocks using the miner directly and verify the
	// import node picks them up via P2P sync (hybrid import + P2P
	// sync). We use the miner directly because MineBlocks asserts all
	// active nodes are synced after each block, which can race with
	// neutrino's P2P sync.
	const additionalBlocks = 10
	ht.Miner().MineBlocks(additionalBlocks)

	// Wait for the import node to see the new blocks.
	expectedHeight := int32(bestHeight) + additionalBlocks
	ht.WaitForNodeBlockHeight(importNode, expectedHeight)
}

// copyFile copies a file from src to dst.
func copyFile(ht *lntest.HarnessTest, src, dst string) {
	srcFile, err := os.Open(src)
	require.NoError(ht, err, "failed to open source file: %s", src)
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	require.NoError(ht, err, "failed to create dest file: %s", dst)
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	require.NoError(ht, err, "failed to copy file")

	err = dstFile.Sync()
	require.NoError(ht, err, "failed to sync dest file")
}


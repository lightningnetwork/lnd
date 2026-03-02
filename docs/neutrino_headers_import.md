# Neutrino Fast Sync via Headers Import

When LND is configured to use the neutrino (light client) backend, the initial
sync requires downloading all block headers and compact filter headers from the
network. On mainnet, this can take a significant amount of time as each header
must be fetched individually via P2P.

The headers import feature allows neutrino to bootstrap from a pre-built header
file or HTTP endpoint, dramatically reducing initial sync time. After importing
headers from the external source, neutrino falls back to normal P2P sync for any
remaining headers not covered by the import.

## How It Works

1. On startup, if header import sources are configured, neutrino downloads (or
   reads from disk) the block headers and filter headers from the specified
   sources.

2. Each import file begins with a 10-byte metadata header:
   - **Network magic** (4 bytes, little-endian): Identifies the target network
     (mainnet, testnet, etc.).
   - **Version** (1 byte): Format version (currently `0`).
   - **Header type** (1 byte): `0` for block headers, `1` for filter headers.
   - **Start height** (4 bytes, little-endian): The block height of the first
     header in the file.

3. Following the metadata, the file contains consecutive raw headers:
   - **Block headers**: 80 bytes each (standard Bitcoin block header).
   - **Filter headers**: 32 bytes each (BIP 158 compact filter header hash).

4. During import, neutrino validates all block headers by checking
   proof-of-work against the target network's consensus rules. Headers that
   fail validation are rejected.

5. After the import completes, neutrino resumes normal P2P sync to fetch any
   headers beyond what the import file contained, ensuring the node is fully
   caught up to the chain tip.

## Configuration

Both `neutrino.blockheaderssource` and `neutrino.filterheaderssource` must be
specified together. Setting only one will cause LND to fail at startup with a
configuration error.

Sources are auto-detected as either HTTP URLs or local file paths based on
whether the value starts with `http`.

### Using block-dn.org (Recommended for Production)

The [block-dn.org](https://github.com/guggero/block-dn) service provides
pre-built header files for multiple Bitcoin networks via HTTP. This is the
recommended approach for production deployments.

The `end_block` parameter in the URL must be divisible by 100,000 and should be
the highest such value below the current chain tip. For example, if the chain
tip is at block 878,432, use `800000` as the end block.

#### Mainnet

```ini
[neutrino]
neutrino.blockheaderssource=https://block-dn.org/headers/import/800000
neutrino.filterheaderssource=https://block-dn.org/filter-headers/import/800000
```

#### Testnet3

```ini
[neutrino]
neutrino.blockheaderssource=https://testnet3.block-dn.org/headers/import/2900000
neutrino.filterheaderssource=https://testnet3.block-dn.org/filter-headers/import/2900000
```

#### Testnet4

```ini
[neutrino]
neutrino.blockheaderssource=https://testnet4.block-dn.org/headers/import/200000
neutrino.filterheaderssource=https://testnet4.block-dn.org/filter-headers/import/200000
```

#### Signet

```ini
[neutrino]
neutrino.blockheaderssource=https://signet.block-dn.org/headers/import/200000
neutrino.filterheaderssource=https://signet.block-dn.org/filter-headers/import/200000
```

You can check the current status and available block heights for each network at
the corresponding status page:
- Mainnet: https://block-dn.org/status
- Testnet3: https://testnet3.block-dn.org/status
- Testnet4: https://testnet4.block-dn.org/status
- Signet: https://signet.block-dn.org/status

### Using Local Files

If you have pre-built header files on disk (for example, copied from an existing
neutrino data directory), you can point LND at them directly:

```ini
[neutrino]
neutrino.blockheaderssource=/path/to/block_headers.bin
neutrino.filterheaderssource=/path/to/filter_headers.bin
```

Local files must include the 10-byte import metadata prefix. Raw header files
from neutrino's data directory (`block_headers.bin` and
`reg_filter_headers.bin`) do not include this metadata by default. You can add
it programmatically using neutrino's `chainimport.AddHeadersImportMetadata()`
utility.

## Security Considerations

The headers import feature includes several safeguards:

- **Proof-of-work validation**: All imported block headers are validated against
  the target network's consensus rules. An attacker cannot provide invalid
  headers without solving the proof-of-work puzzle.

- **Network magic check**: The import file's network magic must match the
  configured Bitcoin network, preventing accidental cross-network imports.

- **Filter header consistency**: Filter headers are validated against the block
  headers to ensure consistency.

- **P2P fallback**: After import, neutrino continues syncing via P2P. The P2P
  network provides an independent check — if the imported headers diverge from
  the honest chain, the P2P sync will detect and correct this.

For additional assurance, you can combine header import with neutrino's existing
`assertfilterheader` option to checkpoint a known-good filter header hash at a
specific height:

```ini
[neutrino]
neutrino.assertfilterheader=800000:0123456789abcdef...
```

## Troubleshooting

### "both neutrino.blockheaderssource and neutrino.filterheaderssource must be specified together"

Both options must be set together. If you only need block headers, you still
must provide a filter headers source, and vice versa.

### HTTP download failures

If using HTTP sources, ensure the URL is reachable and the `end_block` value in
the URL is divisible by 100,000. Check the service status page to confirm
availability.

### "failed to deserialize import metadata"

The import file is missing or has a corrupt metadata prefix. Ensure the file
includes the 10-byte metadata header. Files copied directly from neutrino's data
directory need metadata added via `chainimport.AddHeadersImportMetadata()`.

### "network magic mismatch"

The header file was built for a different Bitcoin network than what LND is
configured to use. Ensure the header file matches your `bitcoin.network`
setting (mainnet, testnet, signet, etc.).

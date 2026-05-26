# Neutrino Fast Sync via Headers Import

When LND is configured to use the neutrino (light client) backend, the initial
sync requires downloading every block header and compact filter header from
the P2P network. On mainnet, that historical fetch dominates time-to-sync on a
fresh install and can take hours.

The headers import feature lets neutrino bootstrap from a pre-built header
file or HTTP endpoint, dramatically reducing initial sync time. After the
import completes, neutrino transitions to normal P2P sync to catch up from
the import target to the current chain tip.

## How It Works

1. On startup, if header import sources are configured, neutrino downloads
   (or reads from disk) the block headers and compact filter headers from
   the configured sources.

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

4. During import on public networks (mainnet/testnet), neutrino runs the
   full contextual header validation pipeline — proof-of-work, median-time-
   past, and relative-ancestor checks — so the imported chain is held to
   the same standard as headers fetched over P2P. Local networks
   (regtest/simnet) fall back to `BFFastAdd` so the harness can ingest
   rapidly-mined timestamps without churn.

5. After the import completes, neutrino resumes normal P2P sync to fetch
   any headers beyond the import target, ensuring the node catches up to
   the chain tip.

## Configuration

Both `neutrino.blockheaderssource` and `neutrino.filterheaderssource` must
be specified together. Setting only one will cause LND to fail at startup
with a configuration error.

Sources are auto-detected as either HTTP URLs or local file paths based on
whether the value starts with `http`.

### Using block-dn.org (Recommended for Production)

The [block-dn.org](https://github.com/guggero/block-dn) service publishes
pre-built header files for multiple Bitcoin networks. Each network exposes
four endpoints:

- `/headers/import/<end_block>` — block headers up to a specific block.
- `/headers/import/latest` — block headers up to the latest block.
- `/filter-headers/import/<end_block>` — compact filter headers up to a specific
  block.
- `/filter-headers/import/latest` — compact filter headers up to the latest
  block.

`<end_block>` is **non-inclusive** and must be divisible by the service's
`entries_per_header_file` (currently `100,000`). It identifies the highest
such boundary at or below the current chain tip.

Each service also publishes a `/status` JSON endpoint that reports
`best_block_height` and `entries_per_header_file`.

#### Mainnet

Mainnet imports also require `fee.url`, since the neutrino backend has no
mempool to derive fee estimates from. A full runnable CLI invocation looks
like:

```sh
lnd \
  --bitcoin.mainnet \
  --bitcoin.node=neutrino \
  --fee.url=https://nodes.lightning.computer/fees/v1/btc-fee-estimates.json \
  --neutrino.blockheaderssource=https://block-dn.org/headers/import/latest \
  --neutrino.filterheaderssource=https://block-dn.org/filter-headers/import/latest
```

The equivalent `lnd.conf` stanza:

```ini
[Application Options]
fee.url=https://nodes.lightning.computer/fees/v1/btc-fee-estimates.json

[Bitcoin]
bitcoin.mainnet=true
bitcoin.node=neutrino

[neutrino]
neutrino.blockheaderssource=https://block-dn.org/headers/import/latest
neutrino.filterheaderssource=https://block-dn.org/filter-headers/import/latest
```

Without `fee.url`, lnd will complete the header import and then exit with
`--fee.url parameter required when running neutrino on mainnet`.

#### Testnet3

```sh
lnd \
  --bitcoin.testnet \
  --bitcoin.node=neutrino \
  --neutrino.blockheaderssource=https://testnet3.block-dn.org/headers/import/latest \
  --neutrino.filterheaderssource=https://testnet3.block-dn.org/filter-headers/import/latest
```

#### Testnet4

```sh
lnd \
  --bitcoin.testnet4 \
  --bitcoin.node=neutrino \
  --neutrino.blockheaderssource=https://testnet4.block-dn.org/headers/import/latest \
  --neutrino.filterheaderssource=https://testnet4.block-dn.org/filter-headers/import/latest
```

#### Signet

```sh
lnd \
  --bitcoin.signet \
  --bitcoin.node=neutrino \
  --neutrino.blockheaderssource=https://signet.block-dn.org/headers/import/latest \
  --neutrino.filterheaderssource=https://signet.block-dn.org/filter-headers/import/latest
```

Current service status and available end-block targets per network:
- Mainnet:  https://block-dn.org/status
- Testnet3: https://testnet3.block-dn.org/status
- Testnet4: https://testnet4.block-dn.org/status
- Signet:   https://signet.block-dn.org/status

### Using Local Files

If you have pre-built header files on disk (for example, copied from an
existing neutrino data directory), you can point LND at them directly:

```ini
[neutrino]
neutrino.blockheaderssource=/path/to/block_headers.bin
neutrino.filterheaderssource=/path/to/filter_headers.bin
```

Local files must include the 10-byte import metadata prefix. Raw header
files from neutrino's data directory (`block_headers.bin` and
`reg_filter_headers.bin`) do not include this metadata by default. You can
add it programmatically using neutrino's `chainimport.AddHeadersImportMetadata()`
utility.

### Test-Only / Throwaway Validation Runs

When validating import end-to-end against a clean temp state — for example
to time a fresh sync, or to confirm the import path before committing to a
real `lnddir` — the following flags are useful:

```sh
lnd \
  --configfile=/dev/null \
  --lnddir=/tmp/lnd-mainnet-import \
  --no-macaroons \
  --noseedbackup \
  --bitcoin.mainnet \
  --bitcoin.node=neutrino \
  --fee.url=https://nodes.lightning.computer/fees/v1/btc-fee-estimates.json \
  --neutrino.blockheaderssource=https://block-dn.org/headers/import/latest \
  --neutrino.filterheaderssource=https://block-dn.org/filter-headers/import/latest
```

> ⚠️ `--configfile=/dev/null`, `--lnddir=/tmp/...`, `--no-macaroons`, and
> `--noseedbackup` are **test/dev shortcuts**. They bypass the wallet
> seed prompt, disable authentication on the RPC, and use throwaway state
> that will be discarded on the next boot. They are **not** production
> defaults — production deployments should retain macaroons, seed
> backup, and a persistent `lnddir`.

## Validation

Header import shares the same validation pipeline as P2P headers, scoped
per network:

| Network          | Validation flags | Notes                                        |
|------------------|------------------|----------------------------------------------|
| mainnet, testnet | `BFNone`         | Full contextual validation (PoW + MTP + ancestor checks). |
| simnet, regtest  | `BFFastAdd`     | Skip contextual checks so the harness can ingest rapidly-mined timestamps. |

Together with the protections below, this means importing from block-dn.org
on a public network does not require trusting the host: the chain is
fully validated locally, and the P2P catch-up step provides an independent
cross-check against the honest network.

- **Proof-of-work validation**: All imported block headers must satisfy the
  network's PoW target. An attacker cannot serve invalid headers without
  finding the cumulative work to back them.

- **Network magic check**: The import file's network magic must match the
  configured Bitcoin network, preventing accidental cross-network imports.

- **Filter header consistency**: Filter headers are validated against the
  block headers to ensure consistency.

- **P2P fallback**: After import, neutrino continues syncing via P2P. The
  P2P network provides an independent check — if the imported headers
  diverge from the honest chain, the P2P sync will detect and correct
  this.

For additional assurance, you can combine header import with neutrino's
existing `assertfilterheader` option to checkpoint a known-good filter
header hash at a specific height:

```ini
[neutrino]
neutrino.assertfilterheader=800000:0123456789abcdef...
```

## What Happens After Import

The import target is only the start of the chain neutrino has on disk —
it is not the chain tip. After the metadata + raw headers are ingested,
neutrino:

1. Connects to peers and announces the imported height.
2. Fetches the remaining block headers from the import target up to the
   current tip over P2P.
3. Fetches the remaining compact filter headers in the same range.
4. Marks the backend as synced.

For a 900,000-block mainnet import on a typical residential connection,
step 1 takes seconds, the import itself takes single-digit seconds, and
steps 2–4 add roughly the time required to fetch the remaining
~50,000-block tail.

## Troubleshooting

### `both neutrino.blockheaderssource and neutrino.filterheaderssource must be specified together`

Both options must be set together. If you only need block headers, you
still must provide a filter headers source, and vice versa.

### `--fee.url parameter required when running neutrino on mainnet`

Mainnet neutrino has no mempool to derive fee estimates from. Add
`--fee.url` (or `fee.url=...` in `lnd.conf`) pointing at a fee estimate
JSON endpoint such as `https://nodes.lightning.computer/fees/v1/btc-fee-estimates.json`.

### HTTP download failures

If using HTTP sources, ensure the URL is reachable and the `end_block`
value in the URL is divisible by `entries_per_header_file` (currently
`100,000`). Check the service's `/status` page to confirm the highest
valid target.

### `failed to deserialize import metadata`

The import file is missing or has a corrupt metadata prefix. Ensure the
file includes the 10-byte metadata header. Files copied directly from
neutrino's data directory need metadata added via
`chainimport.AddHeadersImportMetadata()`.

### `network magic mismatch`

The header file was built for a different Bitcoin network than what LND
is configured to use. Ensure the header file matches your
`bitcoin.network` setting (mainnet, testnet, signet, etc.).

### Import succeeds but P2P catch-up stalls or hangs

This is the symptom of a broken upstream import file: the import
chain-links cleanly within the file, but the file's terminal header is
inconsistent with what live peers serve. The current known case is
testnet3 `4900000` (see the warning above). Pin to a known-good
end-block target (`4700000` for testnet3 at present) until the source
is regenerated.

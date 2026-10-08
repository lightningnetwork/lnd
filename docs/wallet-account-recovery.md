# Durable named-account recovery

A wallet seed does not rediscover named accounts. Their names do not determine
keys. Recovery also needs each original scope and account index, public key,
address schema, and the number of receive and change addresses issued.

`--wallet-account-backup=/independent/wallet-accounts.json` records that public
metadata before LND successfully returns a named-account address or funded
partially signed Bitcoin transaction (PSBT). Failed persistence returns an
error; callers must discard failed results. Account creation and imports also
record metadata before returning success. A periodic refresh records other
wallet activity, but the synchronous writes provide the issuance guarantee.

## Safety rule and limits

Before a supported LND method successfully issues a named-account address or a
funded PSBT containing named-account change, independent durable storage holds
that account's immutable identity and both next-index bounds covering those
keys. Counts never decrease. Normal startup refuses missing recorded accounts,
changed identities, or incompletely reconstructed named-account branches.

The protected methods are `NewAddress`, `LastUnusedAddress`, `CreateAccount`,
`ImportAccount`, and named-account `FundPsbt` in the btcwallet backend.
`PsbtCoinSelect` derives change through `NewAddress`. `SendOutputs` and
`CreateSimpleTx` derive default-account change and do not create named-account
change. Ordinary default-account operations retain their existing behavior.

The guarantee assumes:

- The original seed and the synchronous recovery file survive wallet database
  loss. Put the file on a separate durable filesystem or volume. A different
  directory on the lost wallet volume is insufficient.
- The filesystem honors file and directory `fsync` and atomic rename. Only one
  LND process owns the stable `.lock` file. Keep that lock file in place for
  the lifetime of the backup; do not replace it while LND runs.
  Native Windows is not supported by this mode: syncing the directory through
  its read-only handle fails, so protected startup fails closed. The integration
  test skips Windows instead of weakening the durability requirement.
- Consumers fund addresses returned successfully by the protected methods.
  Unacknowledged keys observed through wallet inspection, and keys derived
  offline from an xpub, are outside this bound. No finite counter can cover
  arbitrary offline derivation.
- Imported account metadata is retained, but its signing seed or external key
  source must survive separately. This feature does not convert watch-only
  keys into locally signable keys. Entirely watch-only/remote-signing wallets
  are not supported by this initial protection mode.

This is not a replacement for static channel backups, live channel state,
imported scripts, private imported keys, or application databases. Their
existing recovery obligations still apply. If both the wallet and synchronous
backup are lost, a periodic copy is only a lower bound. An arbitrary lookahead
margin cannot turn it into proof of complete recovery.

## Enrollment

1. Stop wallet clients and all address issuance. Retain a current wallet backup
   and the seed through the normal secure procedure.
2. Select an independent backup volume. Start the matching LND binary once
   with both `--wallet-account-backup=PATH` and
   `--wallet-account-backup-create`. The latter refuses an existing file.
   Enrollment records the current wallet before normal wallet service starts.
3. Stop LND and remove `--wallet-account-backup-create`. Restart with only the
   path flag. Missing, corrupt, or mismatched evidence must now prevent startup.
4. Run an isolated recovery drill before admitting value to named accounts.
   Do not configure the one-time creation flag in a permanent deployment.

For Kubernetes, use a separately retained backup PVC and a distinct secondary
Secret. A periodic exporter must use atomic compare-and-swap and merge branch
maxima. It must not overwrite the RPC credential Secret. An unavailable
secondary store does not remove the synchronous file's protection.

## File format

Version 1 stores `network`, `updated_at`, and an `accounts` array. Each record
has name, purpose, coin type, hardened account index, xpub, master fingerprint,
watch-only status, external/internal address types, and both key counts. The
path is `m/purpose'/coin'/account_index'`; external children use branch 0 and
internal children branch 1. Both counts are exclusive bounds, not balances.
A zero fingerprint is valid when a local account does not report one.

The address-type numbers are btcwallet `waddrmgr.AddressType` values, not
WalletKit's protobuf enum values. Preserve the whole record. In particular,
BIP49 can use different external and internal address types.

This file is deliberately distinct from the `ListAccountsResponse` JSON used
by older watch-only bootstrap tools. Do not pass it to an account importer
expecting that different format. Extended public keys expose wallet activity;
restrict file and backup access even though the file contains no private key.

## Reconstruction and rescan

1. Stop every wallet client and preserve an immutable copy of surviving
   metadata. Fence the original wallet so only the recovered node can issue
   addresses. Preserve product databases and channel recovery material.
2. Restore the original seed into an isolated maintenance wallet. Bind RPCs to
   loopback, disconnect application clients, and disable normal spending.
   Temporarily omit the account-backup flag for reconstruction. Never delete
   or overwrite the surviving record to make normal startup succeed.
3. Recreate locally derived accounts in ascending index within each scope.
   Account zero already exists. Recreate earlier accounts, including unused
   placeholders, before the desired account. Creating the correct name at the
   wrong index produces different keys. For taproot, use:

   ```sh
   lncli wallet accounts create --address_type p2tr \
       --i_know_what_i_am_doing ACCOUNT_NAME
   ```

   Match the saved path, xpub, address schema, and fingerprint when present.
   Stop on a mismatch. Imported accounts require their original external key
   source and import metadata; do not recreate them as locally derived keys.
   For purpose 1017 records, the saved account index is an internal key family.
   LND initializes families 0 through 255 automatically. Recreate any recorded
   higher family through `WalletKit.DeriveKey` with `key_family` set to its
   saved account index and `key_index=0`, while the backup flag is omitted.
   This restores the deterministic raw account; normal startup does not
   require replaying the branch counts of these internal key families.
4. Re-derive both branches through `WalletKit.NextAddr`. Use `change=false`
   for branch 0 and `change=true` for branch 1. Reconstruct until the live
   counts reach the saved exclusive bounds. If a maintenance attempt already
   derived keys, derive only the remaining difference. `lncli newaddress`
   alone covers only the external branch.
5. Only after reconstruction, stop LND and restart with the surviving backup
   path plus `--reset-wallet-transactions`. The startup gate must accept the
   reconstructed identities and named-account counts. Wait for synchronization.
   Remove the rescan flag after this one successful rescan.
6. Reconcile expected unspent outputs and amounts against independent records.
   Verify signatures and a controlled spend of recovered internal change in
   the drill. A successful rescan or zero balance alone does not prove recovery.
   Resume applications only after identity, both counts, outputs and signing
   agree.

## Failure and rollback

If storage fails after allocation, the address or PSBT request fails. Repair
storage and retry; the unused allocation may remain in the wallet, and the next
successful write includes it. Retain the previous file on any failed attempt.
The stable writer lock prevents two processes from sharing one backup.

Account creation and import also commit before recording. If recording fails,
retrying creation can report that the account already exists. Repair storage
and verify its identity before retrying address issuance; successful issuance
records it synchronously, and the periodic refresh also retries the snapshot.

Rollback of the binary is format-safe for the wallet database, but an older
binary does not enforce this recovery rule. Stop named-account activity before
rollback, retain all metadata, and resume only after an equivalent protection
mechanism is active. Never reuse a partially restored wallet in parallel with
the original.

## Validation

The normal integration harness covers issuance failure, restart, wallet loss,
the omitted-internal-branch negative control, reconstruction and a confirmed
spend. Run it with either supported full-node backend:

```sh
make itest backend=bitcoind icase=wallet-account_backup_recovery
make itest backend=btcd icase=wallet-account_backup_recovery
```

`go test -race ./lnwallet/accountbackup ./lnwallet/btcwallet` covers concurrent
stale snapshots, identity changes, missing evidence, both branch counts, and
write failure after real wallet allocation/funding.

Build `lnd` with `walletrpc signrpc`, then run:

```sh
LND_BINARY=/path/to/lnd python3 scripts/test-account-backup.py
```

The script uses Bitcoin Core and temporary regtest wallets. It creates an
account at index 2, funds it, publishes a transaction with internal change,
destroys the wallet, restores the seed, proves that missing internal derivation
fails, reconstructs both branches, rescans, and spends recovered change. It
never accesses a cluster or prints the test seed.

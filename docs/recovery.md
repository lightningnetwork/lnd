# Table of Contents

* [Recovering Funds From `lnd` (funds are safu!)](#recovering-funds-from-lnd-funds-are-safu)
  * [On-Chain Recovery](#on-chain-recovery)
    * [24-word Cipher Seeds](#24-word-cipher-seeds)
    * [Wallet and Seed Passphrases](#wallet-and-seed-passphrases)
    * [Starting On-Chain Recovery](#starting-on-chain-recovery)
    * [Forced In-Place Rescan](#forced-in-place-rescan)
  * [Off-Chain Recovery](#off-chain-recovery)
    * [Obtaining SCBs](#obtaining-scbs)
      * [On-Disk `channel.backup`](#on-disk-channelbackup)
      * [Using the `ExportChanBackup` RPC](#using-the-exportchanbackup-rpc)
      * [Streaming Updates via `SubscribeChannelBackups`.](#streaming-updates-via-subscribechannelbackups)
    * [Recovering Using SCBs](#recovering-using-scbs)

# Recovering Funds From `lnd` (funds are safu!)

In this document, we'll go over the various built-in mechanisms for recovering
funds from `lnd` due to any sort of data loss, or malfunction. Coins in `lnd`
can exist in one of two pools: on-chain or off-chain. On-chain funds are
outputs under the control of `lnd` that can be spent immediately, and without
any auxiliary data. Off-chain funds on the other hand exist within a 2-of-2
multi-sig output typically referred to as a payment channel. Depending on the
exact nature of operation of a given `lnd` node, one of these pools of funds
may be empty.

Fund recovery for `lnd` will require two pieces of data: 
  1. Your 24-word cipher seed
  2. Your encrypted Static Channel Backup file (or the raw data)

If one is only attempting to recover _on chain_ funds, then only the first item
is required.

The SCB file is encrypted using a key _derived_ from the user's seed. As a
result, it cannot be used in isolation.

## On-Chain Recovery 

### 24-word Cipher Seeds

When a new `lnd` node is created, it is given a 24-word seed phrase, called an
[`aezeed cipher seed`](https://github.com/lightningnetwork/lnd/tree/master/aezeed).
The [BIP39](https://github.com/bitcoin/bips/blob/master/bip-0039.mediawiki) and
[`aezeed cipher seed`](https://github.com/lightningnetwork/lnd/tree/master/aezeed)
formats look similar, but the only commonality they share is that they use the
same default [English](https://raw.githubusercontent.com/bitcoin/bips/master/bip-0039/english.txt)
wordlist.
A valid seed phrase obtained over the CLI `lncli create` command looks something
like: 
```text
!!!YOU MUST WRITE DOWN THIS SEED TO BE ABLE TO RESTORE THE WALLET!!!

---------------BEGIN LND CIPHER SEED---------------
 1. ability   2. noise   3. lift     4. document
 5. certain   6. month   7. shoot    8. perfect
 9. matrix   10. mango  11. excess  12. turkey
13. river    14. pitch  15. fluid   16. rack
17. drill    18. text   19. buddy   20. pool
21. soul     22. fatal  23. ship    24. jelly
---------------END LND CIPHER SEED-----------------

!!!YOU MUST WRITE DOWN THIS SEED TO BE ABLE TO RESTORE THE WALLET!!!
```

### Wallet and Seed Passphrases

During the creation process, users are first prompted to enter a **wallet
password**:
```text
Input wallet password:
Confirm wallet password:
```

This password is used to _encrypt_ the wallet on disk, which includes any
derived master private keys or public key data.

Users can also _optionally_ enter a second passphrase which we call the _cipher
seed passphrase_:
```text
Your cipher seed can optionally be encrypted.
Input your passphrase if you wish to encrypt it (or press enter to proceed without a cipher seed passphrase):
```

If specified, then this will be used to encrypt the cipher seed itself. The
cipher seed format is unique in that the 24-word phrase is actually a
_ciphertext_. As a result, there's no standard word list as any arbitrary
encoding can be used. If a passphrase is specified, then the cipher seed you
write down is actually an _encryption_ of the entropy used to generate the BIP
32 root key for the wallet. Unlike a BIP 39 24-word phrase, the cipher seed is
able to _detect_ incorrect passphrase. BIP 39 on the other hand, will instead
silently decrypt to a new (likely empty) wallet.

### Starting On-Chain Recovery

The initial entry point to trigger recovery of on-chain funds in the command
line is the `lncli create` command.
```shell
$   lncli create
```

Next, one can enter a _new_ wallet password to encrypt any newly derived keys
as a result of the recovery process.
```text
Input wallet password:
Confirm wallet password:
```

Once a new wallet password has been obtained, the user will be prompted for
their _existing_ cipher seed:
```text
Input your 24-word mnemonic separated by spaces: ability noise lift document certain month shoot perfect matrix mango excess turkey river pitch fluid rack drill text buddy pool soul fatal ship jelly
```

If a _cipher seed passphrase_ was used when the seed was created, it MUST be entered now:
```text
Input your cipher seed passphrase (press enter if your seed doesn't have a passphrase):
```

Finally, the user has an option to choose a _recovery window_:
```text
Input an optional address look-ahead used to scan for used keys (default 2500):
```

The recovery window is a metric that the on-chain rescanner will use to
determine when all the "used" addresses have been found. If the recovery window
is two, lnd will fail to find funds in any addresses generated after the point
in which two consecutive addresses were generated but never used. If an `lnd`
on-chain wallet was extensively used, then users may want to _increase_ the
default value.  

If all the information provided was valid, then you'll be presented with the
seed again: 
```text

!!!YOU MUST WRITE DOWN THIS SEED TO BE ABLE TO RESTORE THE WALLET!!!

---------------BEGIN LND CIPHER SEED---------------
 1. ability   2. noise   3. lift     4. document
 5. certain   6. month   7. shoot    8. perfect
 9. matrix   10. mango  11. excess  12. turkey
13. river    14. pitch  15. fluid   16. rack
17. drill    18. text   19. buddy   20. pool
21. soul     22. fatal  23. ship    24. jelly
---------------END LND CIPHER SEED-----------------

!!!YOU MUST WRITE DOWN THIS SEED TO BE ABLE TO RESTORE THE WALLET!!!

lnd successfully initialized!
```

In `lnd`'s logs, you should see something along the lines of (irrelevant lines skipped):
```text
[INF] LNWL: Opened wallet
[INF] LTND: Wallet recovery mode enabled with address lookahead of 2500 addresses
[INF] LNWL: RECOVERY MODE ENABLED -- rescanning for used addresses with recovery_window=2500
[INF] CHBU: Updating backup file at test_lnd3/data/chain/bitcoin/simnet/channel.backup
[INF] CHBU: Swapping old multi backup file from test_lnd3/data/chain/bitcoin/simnet/temp-dont-use.backup to test_lnd3/data/chain/bitcoin/simnet/channel.backup
[INF] LNWL: Seed birthday surpassed, starting recovery of wallet from height=748 hash=3032830c812a4a6ea305d8ead13b52e9e69d6400ff3c997970b6f76fbc770920 with recovery-window=2500
[INF] LNWL: Scanning 1 blocks for recoverable addresses
[INF] LNWL: Recovered addresses from blocks 748-748
[INF] LNWL: Started rescan from block 3032830c812a4a6ea305d8ead13b52e9e69d6400ff3c997970b6f76fbc770920 (height 748) for 800 addresses
[INF] LNWL: Catching up block hashes to height 748, this might take a while
[INF] LNWL: Done catching up block hashes
[INF] LNWL: Finished rescan for 800 addresses (synced to block 3032830c812a4a6ea305d8ead13b52e9e69d6400ff3c997970b6f76fbc770920, height 748)
```

That final line indicates the rescan is complete! If not all funds have
appeared, then the user may need to _repeat_ the process with a higher recovery
window. Depending on how old the wallet is (the cipher seed stores the wallet's
birthday!) and how many addresses were used, the rescan may take anywhere from
a few minutes to a few hours. To track the recovery progress, one can use the
command `lncli getrecoveryinfo`. When finished, the following is returned,
```shell
$  lncli getrecoveryinfo
{
    "recovery_mode": true,
    "recovery_finished": true,
    "progress": 1
}
```

If the rescan wasn't able to complete fully (`lnd` was shutdown for example),
then from `lncli unlock`, it's possible to _restart_ the rescan from where it
left off with the `--recovery-window` argument:
```shell
$  lncli unlock --recovery_window=2500
```

Note that if this argument is not specified, then the wallet will not
_re-enter_ the recovery mode and may miss funds during the portion of the
rescan.

### Forced In-Place Rescan

The recovery methods described above assume a clean slate for a node, so
there's no existing UTXO or key data in the node's database. However, there are
times when an _existing_ node may want to _manually_ rescan the chain. We have
a command line flag for that! Just start `lnd` and add the following flag:
```shell
$  lnd --reset-wallet-transactions
```

The `--reset-wallet-transactions` flag will _reset_ the best synced height of
the wallet back to its birthday, or genesis if the birthday isn't known (for
some older wallets).

Just run `lnd` with the flag, unlock it, then the wallet should begin
rescanning. An entry resembling the following will show up in the logs once it's
complete:
```text
[INF] LNWL: Finished rescan for 800 addresses (synced to block 3032830c812a4a6ea305d8ead13b52e9e69d6400ff3c997970b6f76fbc770920, height 748)
```

**Remember to remove the flag once the rescan was completed successfully to
avoid rescanning again for every restart of lnd**.

## Off-Chain Recovery

After version `v0.6-beta` of `lnd`, the daemon now ships with a new feature
called Static Channel Backups (SCBs). We call these _static_ as they only need
to be obtained _once_: when the channel is created. From there on, a backup is
good until the channel is closed. The backup contains all the information we
need to initiate the Data Loss Protection (DLP) feature in the protocol, which
ultimately leads to us recovering the funds from the channel _on-chain_. This
is a foolproof _safe_ backup mechanism.

We say _safe_, as care has been taken to ensure that there are no foot guns in
this method of backing up channels, vs doing things like `rsync`ing or copying
the `channel.db` file periodically. Those methods can be dangerous as one never
knows if they have the latest state of a channel or not. Instead, we aim to
provide a simple, safe method to allow users to recover the settled funds in
their channels in the case of partial or complete data loss. The backups
themselves are encrypted using a key derived from the user's seed, this way we
protect privacy of the users channels in the backup state, and ensure that a
random node can't attempt to import another user's channels.

Given a valid SCB, the user will be able to recover funds that are fully
settled within their channels. By "fully settled" we mean funds that are in the
base commitment outputs, and not HTLCs.  We can only restore these funds as
right after the channel is created, as we have all the data required to make a
backup, but lack information about the future HTLCs that the channel will
process.

### Obtaining SCBs

#### On-Disk `channel.backup`

There are multiple ways of obtaining SCBs from `lnd`. The most commonly used
method will likely be via the `channel.backup` file that's stored on-disk
alongside the rest of the chain data. This is a special file that contains SCB
entries for _all_ currently open channels. Each time a channel is opened or
closed, this file is updated on disk safely (atomic file rename). As
a result, unlike the `channel.db` file, it's _always_ safe to copy this file
for backup at ones desired location. The default location on Linux is: 
`~/.lnd/data/chain/bitcoin/mainnet/channel.backup`

An example of using file system level notification to [copy the backup to a
distinct volume/partition/drive can be found
here](https://gist.github.com/alexbosworth/2c5e185aedbdac45a03655b709e255a3).

##### Last resort manual force close

Reserve this option as a last resort when the peer is offline and all other
avenues to retrieve funds from the channel have been exhausted. The primary
motivation for introducing this option is to provide a means of recovery,
albeit with some risk, rather than losing the funds indefinitely. This is a very
dangerous option, so it should only be used after consulting with a recovery
specialist or after opening an issue to make sure!!!

Starting with release 0.19.0 LND includes unsigned force close transaction
for a channel into channel.backup file and RPCs returning channel backups.
To generate a force close transaction from the backup file, utilize the
`chantools scbforceclose` command. However, exercise caution as this action is
perilous. If the channel has been updated since the backup creation, another
node or a watchtower may issue a penalty transaction, seizing all funds!

#### Using the `ExportChanBackup` RPC

Another way to obtain SCBS for all or a target channel is via the new
`exportchanbackup` `lncli` command:
```shell
$  lncli --network=simnet exportchanbackup --chan_point=29be6d259dc71ebdf0a3a0e83b240eda78f9023d8aeaae13c89250c7e59467d5:0
{
    "chan_point": "29be6d259dc71ebdf0a3a0e83b240eda78f9023d8aeaae13c89250c7e59467d5:0",
    "chan_backup": "02e7b423c8cf11038354732e9696caff9d5ac9720440f70a50ca2b9fcef5d873c8e64d53bdadfe208a86c96c7f31dc4eb370a02631bb02dce6611c435753a0c1f86c9f5b99006457f0dc7ee4a1c19e0d31a1036941d65717a50136c877d66ec80bb8f3e67cee8d9a5cb3f4081c3817cd830a8d0cf851c1f1e03fee35d790e42d98df5b24e07e6d9d9a46a16352e9b44ad412571c903a532017a5bc1ffe1369c123e1e17e1e4d52cc32329aa205d73d57f846389a6e446f612eeb2dcc346e4590f59a4c533f216ee44f09c1d2298b7d6c"
}

$  lncli --network=simnet exportchanbackup --all
{
    "chan_points": [
        "29be6d259dc71ebdf0a3a0e83b240eda78f9023d8aeaae13c89250c7e59467d5:0"
    ],
    "multi_chan_backup": "fd73e992e5133aa085c8e45548e0189c411c8cfe42e902b0ee2dec528a18fb472c3375447868ffced0d4812125e4361d667b7e6a18b2357643e09bbe7e9110c6b28d74f4f55e7c29e92419b52509e5c367cf2d977b670a2ff7560f5fe24021d246abe30542e6c6e3aa52f903453c3a2389af918249dbdb5f1199aaecf4931c0366592165b10bdd58eaf706d6df02a39d9323a0c65260ffcc84776f2705e4942d89e4dbefa11c693027002c35582d56e295dcf74d27e90873699657337696b32c05c8014911a7ec8eb03bdbe526fe658be8abdf50ab12c4fec9ddeefc489cf817721c8e541d28fbe71e32137b5ea066a9f4e19814deedeb360def90eff2965570aab5fedd0ebfcd783ce3289360953680ac084b2e988c9cbd0912da400861467d7bb5ad4b42a95c2d541653e805cbfc84da401baf096fba43300358421ae1b43fd25f3289c8c73489977592f75bc9f73781f41718a752ab325b70c8eb2011c5d979f6efc7a76e16492566e43d94dbd42698eb06ff8ad4fd3f2baabafded"
}

$  lncli --network=simnet exportchanbackup --all --output_file=channel.backup
```

As shown above, a user can either: specify a specific channel to back up, backup
all existing channels, or backup directly to an on-disk file. All backups use
the same format.

#### Streaming Updates via `SubscribeChannelBackups`

Using the gRPC interface directly, [a new call:
`SubscribeChannelBackups`](https://api.lightning.community/#subscribechannelbackups).
This call allows users to receive a new notification each time the underlying
SCB state changes. This can be used to implement more complex backup
schemes, compared to the file system notification based approach.

### Recovering Using SCBs

If a node is being created from scratch, then it's possible to pass in an
existing SCB using the `lncli create` or `lncli unlock` commands:
```shell
$  lncli create --multi_file=channel.backup
```

Alternatively, the `restorechanbackup` command can be used if `lnd` has already
been created at the time of SCB restoration:
```shell
$  lncli restorechanbackup -h
NAME:
   lncli restorechanbackup - Restore an existing single or multi-channel static channel backup

USAGE:
   lncli restorechanbackup [command options] [--single_backup] [--multi_backup] [--multi_file=]

CATEGORY:
   Channels

DESCRIPTION:

  Allows a user to restore a Static Channel Backup (SCB) that was
  obtained either via the exportchanbackup command, or from lnd's
  automatically managed channel.backup file. This command should be used
  if a user is attempting to restore a channel due to data loss on a
  running node restored with the same seed as the node that created the
  channel. If successful, this command will allows the user to recover
  the settled funds stored in the recovered channels.

  The command will accept backups in one of three forms:

     * A single channel packed SCB, which can be obtained from
       exportchanbackup. This should be passed in hex encoded format.

     * A packed multi-channel SCB, which couples several individual
       static channel backups in single blob.

     * A file path which points to a packed multi-channel backup within a
       file, using the same format that lnd does in its channel.backup
       file.


OPTIONS:
   --single_backup value  a hex encoded single channel backup obtained from exportchanbackup
   --multi_backup value   a hex encoded multi-channel backup obtained from exportchanbackup
   --multi_file value     the path to a multi-channel back up file
```

Once the process has been initiated, `lnd` will proceed to:

  1. Given the set of channels to recover, the server will then will insert a
     series of "channel shells" into the database. These contain only the
     information required to initiate the DLP (data loss protection) protocol
     and nothing more. As a result, they're marked as "recovered" channels in
     the database, and we'll disallow trying to use them for any other process.
  2. Once the channel shell is recovered, the
     [chanbackup](https://github.com/lightningnetwork/lnd/tree/master/chanbackup)
     package will attempt to insert a LinkNode that contains all prior
     addresses that we were able to reach the peer at. During the process,
     we'll also insert the edge for that channel (only in the outgoing
     direction) into the database as well.
  3. lnd will then start up, and as usual attempt to establish connections to
     all peers that we have channels open with. If `lnd` is already running,
     then a new persistent connection attempt will be initiated.
  4. Once we connect with a peer, we'll then initiate the DLP protocol. The
     remote peer will discover that we've lost data, and then immediately force
     close their channel. Before they do though, they'll send over the channel
     reestablishment handshake message which contains the unrevoked commitment
     point which we need to derive keys (will be fixed in
     BOLT 1.1 by making the key static) to sweep our funds.
  5. Once the commitment transaction confirms, given information within the SCB
     we'll re-derive all keys we need, and then sweep the funds.

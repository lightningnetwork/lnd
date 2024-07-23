# Wallet management

The wallet in the context of `lnd` is a database file (located in the data
directory, for example `~/.lnd/data/chain/bitcoin/mainnet/wallet.db` on Linux)
that contains all addresses and private keys for the on-chain **and** off-chain
(LN) funds.

The wallet is independent of the chain backend that is used (`bitcoind`, `btcd`
or `neutrino`) and must therefore be created as the first step after starting
up a fresh `lnd` node.

To protect the sensitive content of the wallet, the database is encrypted with
a password chosen by the user when creating the wallet (simply called "wallet
password"). `lnd` will not store that password anywhere by itself (as that would
defeat the purpose of the password) so every time `lnd` is restarted, its wallet
needs to be unlocked with that password. This can either be done [manually
through the command line](#unlocking-a-wallet) or (starting with `lnd` version
`v0.13.0-beta`) [automatically from a file](#auto-unlocking-a-wallet).

## Creating a wallet

If `lnd` is being run for the first time, create a new wallet with:
```shell
$   lncli create
```
This will prompt for a wallet password, and optionally a cipher seed
passphrase.

`lnd` will then print a 24 word cipher seed mnemonic, which can be used to
recover the wallet in case of data loss. The user should write this down and
keep in a safe place.

In case a node needs to be recovered from an existing seed, this can also be
done through the `create` command. Please refer to the
[recovery guide](recovery.md) for more information about recovering a node.

## Unlocking a wallet

Every time `lnd` starts up fresh (e.g. after a system restart or a version
upgrade) the user-chosen wallet password needs to be entered to unlock (decrypt)
the wallet database.

This will be indicated in `lnd`'s log with a message like this:

```text
2021-05-06 11:36:11.445 [INF] LTND: Waiting for wallet encryption password. Use `lncli create` to create a wallet, `lncli unlock` to unlock an existing wallet, or `lncli changepassword` to change the password of an existing wallet and unlock it.
```

Unlocking the password manually is as simple as running the command
```shell
$   lncli unlock
```
and then typing the wallet password.

## Auto-unlocking a wallet

In some situations (for example automated, cluster based setups) it can be
impractical to manually unlock the wallet every time `lnd` is restarted.

In `lnd` version `v0.13.0-beta` and later there is a configuration option to
tell the wallet to auto-unlock itself by reading the password from a file. This
can only be activated _after_ the wallet was created manually.

### Very basic example (not very secure)

This example only tries to give a basic, minimal example on how to use the
auto-unlock feature. Storing a password in a file on the same disk as the wallet
database is not in itself more secure than leaving the database unencrypted in
the first place. This example might be useful in a containerized environment
though where the secrets are mounted to a file anyway.

- Start `lnd` without the flag:
  ```shell
  $   lnd --bitcoin.active --bitcoin.xxxx .....
  ```
- Create the wallet and write down the seed in a safe place:
  ```shell
  $   lncli create
  ```
- Stop `lnd` again:
  ```shell
  $   lncli stop
  ```
- Write the password to a file:
  ```shell
  $   echo 'my-$up3r-Secret-Passw0rd' > /some/safe/location/password.txt
  ```
- Make sure the password file can only be read by our user:
  ```shell
  $   chmod 0400 /some/safe/location/password.txt
  ```
- Start `lnd` with the auto-unlock flag:
  ```shell
  $   lnd --bitcoin.active --bitcoin.xxxx ..... \
         --wallet-unlock-password-file=/some/safe/location/password.txt
  ```

As with every command line flag, the `wallet-unlock-password-file` option can
also be added to `lnd`'s configuration file, for example:

```text
[Application Options]
debuglevel=debug
wallet-unlock-password-file=/some/safe/location/password.txt

[Bitcoin]
bitcoin.active=1
...
```

### More secure example with password manager and using a named pipe

This example is a bit more involved and requires the use of a password manager
of some sort. It will also only work on Unix like file systems that support
named pipes.

We will use the password manager [`pass`](https://www.passwordstore.org/) as an
example here, but it should work similarly with other password managers.

- Start `lnd` without the flag:
  ```shell
  $   lnd --bitcoin.active --bitcoin.xxxx .....
  ```
- Create the wallet and write down the seed in a safe place:
  ```shell
  $   lncli create
  ```
- Stop `lnd` again:
  ```shell
  $   lncli stop
  ```
- Store the password in `pass`:
  ```shell
  $   pass insert lnd/my-wallet-password
  ```
- Create a startup script for starting `lnd`, for example `run-lnd.sh`:
  ```shell
  #!/bin/bash

  # Create a named pipe. As the name suggests, this is a FIFO (first in first
  # out) pipe. Everything sent in can be read out again without the content
  # actually being written to a disk.
  mkfifo /tmp/wallet-password-pipe
  
  # Read the password from the manager and attempt to write it to the pipe. Any
  # write to a pipe will only be accepted once there is a process that reads
  # from the pipe at the same time. That's why we need to run this process in
  # the background (the ampersand & at the end) because it would block our
  # script from continuing otherwise.
  pass lnd/my-wallet-password > /tmp/wallet-password-pipe &
  
  # Now we can start lnd.
  lnd --bitcoin.active --bitcoin.xxxx ..... \
    --wallet-unlock-password-file=/tmp/wallet-password-pipe
  ```
- Run the startup script instead of running `lnd` directly.
  ```shell
  $   ./run-lnd.sh
  ```

## Changing the password

Changing the wallet password is possible but only while the wallet is locked.
So after restarting `lnd`, instead of using the `unlock` command, the
`changepassword` command can be used:

```shell
$   lncli changepassword
```

This will ask for the old/existing password and a new one. If successful, the
database is re-encrypted with the new password and then the wallet is also
unlocked in the process.

## DO NOT USE --noseedbackup on mainnet

There is a way to get rid of the need to unlock the wallet password: The
`--noseedbackup` flag.

Using that flag with **real funds (mainnet) is extremely risky for two reasons**:
1. On first startup a wallet is created automatically. The seed phrase (the 24
   words needed to restore a wallet) is never shown to the user. Therefore, if
   the worst thing happens and the hard disk crashes or the wallet file is
   deleted by accident, **THERE IS NO WAY OF GETTING THE FUNDS BACK**.
2. In addition to the seed not being known to the user, the wallet database is
   also not protected. A well-known default password is chosen for the
   encryption. Any user (or malware) with access to the wallet database can
   steal the funds if they copy the file.

The `--noseedbackup` flag should only ever be used in a test setup, for example
on Bitcoin testnet, regtest or simnet.

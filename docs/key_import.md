# Overview

This document serves as an introductory point for users interested in reducing
their hot-wallet risks, allowing them to maintain on-chain funds outside of
`lnd` but still be able to manage them within `lnd`. As of `v0.13.0-beta`, `lnd`
is able to import BIP-0049 and BIP-0084 extended public keys either at the
account path (`m/purpose'/coin_type'/account'`) or at the address index path
(`m/purpose'/coin_type'/account'/change/address_index`) as watch-only through
the `WalletKit` APIs.

Note that in order to follow the rest of this document and/or use the
`WalletKit` APIs, users will need to obtain an `lnd` build compiled with the
`walletrpc` tag. Our release builds already include this tag by default, so this
would only be necessary when compiling from source.

# `lnd`'s Default Wallet Accounts

Upon initializing `lnd`, a wallet is created with four default accounts:

* A custom BIP-0049 account (more on this later) to generate NP2WKH external
  addresses.
* A BIP-0084 account to generate P2WKH external and change addresses.
* A catch-all BIP-0049 account where all imported BIP-0049 address keys (NP2WKH
  addresses) exist within.
* A catch-all BIP-0084 account where all imported BIP-0049 address keys (P2WKH
  addresses) exist within.

Prior to `v0.13.0-beta`, these accounts were abstracted away from users. As part
of the key import feature, they are now exposed through the new `WalletKit` RPCs
(`ListAccounts`, `ImportAccount`, `ImportPublicKey`) and the `lncli wallet
accounts` command.

```shell
$ lncli wallet accounts
NAME:
   lncli wallet accounts - Interact with wallet accounts.

USAGE:
   lncli wallet accounts command [command options] [arguments...]

COMMANDS:
     list           Retrieve information of existing on-chain wallet accounts.
     import         Import an on-chain account into the wallet through its extended public key.
     import-pubkey  Import a public key as watch-only into the wallet.

OPTIONS:
   --help, -h  show help
```

## Account Details

Before interacting with the new set of APIs, users will want to become familiar
with how wallet accounts are represented within `lnd`. The
`WalletKit.ListAccounts` RPC or `lncli wallet accounts list` command can be used
to retrieve the details of accounts.

```shell
$ lncli wallet accounts list
{
    "accounts": [
        {
            "name": "default",
            "address_type": "HYBRID_NESTED_WITNESS_PUBKEY_HASH",
            "extended_public_key": "upub5EbJZz2tYCpPFgDAMDnXpTeLs5EMNJAfyzRKQuUiTugSaJDjnDdk9vNcENzpw1FnxkerNW7jLuBeoxmcGMtopGExmaWqrMB7wRgU8tExTMz",
            "master_key_fingerprint": null,
            "derivation_path": "m/49'/0'/0'",
            "external_key_count": 0,
            "internal_key_count": 0,
            "watch_only": false
        },
        {
            "name": "default",
            "address_type": "WITNESS_PUBKEY_HASH",
            "extended_public_key": "vpub5Z9beF6NYCrHeDmKC38tM3xXMDFFSARa9sdHRPChEMGqtxiELfZB8hm6FwBpBvfPpX2HGG8edYVV9Wupe43PEJJhhfnz1egtQNNaDXyYExn",
            "master_key_fingerprint": null,
            "derivation_path": "m/84'/0'/0'",
            "external_key_count": 0,
            "internal_key_count": 0,
            "watch_only": false
        }
    ]
}
```

There's a lot to unpack in the response above, so let's cover each account field
in detail. As mentioned above, four default accounts should exist, though only
two are shown in the output. The catch-all imported accounts are hidden by
default until a key has been imported into them.

* `name`: Each account has a name it can be identified by. `lnd`'s default
  spendable accounts have the name "default". The default catch-all imported
  accounts have the name "imported".
* `extended_public_key`: The BIP-0044 extended public key for the account. Any
  addresses generated for the account are derived from this key. Each key has a
  version prefix that identifies the chain and derivation scheme being used. At
  the time of writing, `lnd` supports the following versions:
  * `xpub/tpub`: The commonly used version prefix originally intended for
    BIP-0032 mainnet/testnet extended keys. Since `lnd` does not support
    BIP-0032 extended keys, this version serves as a catch-all for the other
    versions.
  * `ypub/upub`: The version prefix for BIP-0049 mainnet/testnet extended keys.
  * `zpub/vpub`: The version prefix for BIP-0084 mainnet/testnet extended keys.
* `address_type`: The type of addresses the account can derive. There are three
  supported address types:
  * `WITNESS_PUBKEY_HASH`: The standard derivation scheme for BIP-0084 with
    P2WKH for external and change addresses.
  * `NESTED_WITNESS_PUBKEY_HASH`: The standard derivation scheme for BIP-0049
    with P2WKH for external and change addresses.
  * `HYBRID_NESTED_WITNESS_PUBKEY_HASH` A custom derivation scheme for BIP-0049
    used by `lnd` where NP2WKH is used for external addresses and P2WKH for
    change addresses.
* `master_key_fingerprint`: The 4 byte fingerprint of the master key
  corresponding to the account. This is usually required by hardware
  wallet/external signers to identify the proper signing key.
* `derivation_path`: The BIP-0044 derivation path used on the master key to
  obtain the account key.
* `external_key_count`: The number of external addresses generated.
* `internal_key_count`: The number of change addresses generated.
* `watch_only`: Whether the wallet has private key information for the account.
  `lnd`'s default wallet accounts always have private key information, so this
  value is `false`.

# Key Import

An existing limitation to the key import APIs is that events (deposits/spends)
for imported keys, including those derived from an imported account, will only
be detected by lnd if they happen after the import. Rescans to detect past
events are currently not supported, but will come at a later time.

## Account Key Import

The `WalletKit.ImportAccount` RPC and `lncli wallet accounts import` command can
be used to import an account. At the time of writing, importing an account has
the following request parameters:

* `name` (required): A name to identify the imported account with.
* `extended_public_key` (required): A public key that corresponds to a wallet account
  represented as an extended key. It must conform to a derivation path of the
  form `m/purpose'/coin_type'/account'`.
* `master_key_fingerprint` (optional): The fingerprint of the root key (also
  known as the key with derivation path m/) from which the account public key
  was derived from. This may be required by some hardware wallets for proper
  identification and signing.
* `address_type` (optional): An address type is only required when the extended
  account public key has a legacy version (xpub, tpub, etc.), such that the
  wallet cannot detect what address scheme it belongs to.
* `dry_run` (optional): Whether a dry run should be attempted when importing the
  account. This serves as a way to confirm whether the account is being imported
  correctly by returning the first N addresses for the external and internal
  branches of the account. If these addresses match as expected, then it should
  be safe to import the account as is.

For the sake of simplicity, we'll present an example with two `lnd` nodes Alice
and Bob, where Alice acts as a signer _only_, and Bob manages Alice's on-chain
BIP-0084 account by crafting transactions and watching/spending addresses. Since
Alice will only act as a signer, we'll want to import her BIP-0084 account into
Bob's node, which will require knowledge of Alice's extended public key.

Alice's BIP-0084 extended public key can be obtained as follows.

```shell
$ lncli-alice wallet accounts list --name=default --address_type=p2wkh
{
    "accounts": [
        {
            "name": "default",
            "address_type": "WITNESS_PUBKEY_HASH",
            "extended_public_key": "vpub5Z9beF6NYCrHeDmKC38tM3xXMDFFSARa9sdHRPChEMGqtxiELfZB8hm6FwBpBvfPpX2HGG8edYVV9Wupe43PEJJhhfnz1egtQNNaDXyYExn",
            "master_key_fingerprint": null,
            "derivation_path": "m/84'/0'/0'",
            "external_key_count": 0,
            "internal_key_count": 0,
            "watch_only": false
        }
    ]
}
```

Bob can then import the account with the following command:

```shell
$ lncli-bob wallet accounts import vpub5Z9beF6NYCrHeDmKC38tM3xXMDFFSARa9sdHRPChEMGqtxiELfZB8hm6FwBpBvfPpX2HGG8edYVV9Wupe43PEJJhhfnz1egtQNNaDXyYExn alice
```

Before Bob imports the account, they may want to confirm the account is being
imported using the correct derivation scheme. This can be done with the dry run
request parameter. When a dry run is done, the response will include the usual
account details, as well as the first 5 external and change addresses, which can
be used to confirm they match with what the account owner expects.

```shell
$ lncli-bob wallet accounts import vpub5Z9beF6NYCrHeDmKC38tM3xXMDFFSARa9sdHRPChEMGqtxiELfZB8hm6FwBpBvfPpX2HGG8edYVV9Wupe43PEJJhhfnz1egtQNNaDXyYExn alice --dry_run
{
    "account": {
        "name": "alice",
        "address_type": "WITNESS_PUBKEY_HASH",
        "extended_public_key": "vpub5Z9beF6NYCrHeDmKC38tM3xXMDFFSARa9sdHRPChEMGqtxiELfZB8hm6FwBpBvfPpX2HGG8edYVV9Wupe43PEJJhhfnz1egtQNNaDXyYExn",
        "master_key_fingerprint": null,
        "derivation_path": "m/84'/0'/0'",
        "external_key_count": 0,
        "internal_key_count": 0,
        "watch_only": true
    },
    "dry_run_external_addrs": [
        "bcrt1q8zdjz2q92eh7jw9ah3upf2u9553226gq79el5l",
        "bcrt1qmx2m4ngd2el0rmmcu0mz453yzzl3aq9mag0l79",
        "bcrt1q904yve7yvt2t3v0s5r7rueweh4jjr3enfgam8w",
        "bcrt1qa7k20jwfvsep8x0dx4jfu9xm0tlwaa8wrrgl77",
        "bcrt1qzypxx35cfsl24mslqextetuc5m8vvadlqp20d8"
    ],
    "dry_run_internal_addrs": [
        "bcrt1qlstwh8ecy7szfw7k6rllc4ajkg6922xjwj6a23",
        "bcrt1qdrz9glz4ld7uyxwv3jz2anx4k9pe3zm86hpy9g",
        "bcrt1qfdu6tfhs85q20tf48nhtx0kjgr0t2j25apm90t",
        "bcrt1qkmysm9wlnhyyc4uhfaxyafj6q3e3ujcnh97cqc",
        "bcrt1qw8hhmdg3atfp7dcwjtysq4kcmnh07kjy2rd2ay"
    ]
}
```

Once Bob has confirmed the correct account derivation scheme is being used, the
account can be imported without the dry run parameter.

```shell
$ lncli-bob wallet accounts import vpub5Z9beF6NYCrHeDmKC38tM3xXMDFFSARa9sdHRPChEMGqtxiELfZB8hm6FwBpBvfPpX2HGG8edYVV9Wupe43PEJJhhfnz1egtQNNaDXyYExn alice
{
    "account": {
        "name": "alice",
        "address_type": "WITNESS_PUBKEY_HASH",
        "extended_public_key": "vpub5Z9beF6NYCrHeDmKC38tM3xXMDFFSARa9sdHRPChEMGqtxiELfZB8hm6FwBpBvfPpX2HGG8edYVV9Wupe43PEJJhhfnz1egtQNNaDXyYExn",
        "master_key_fingerprint": null,
        "derivation_path": "m/84'/0'/0'",
        "external_key_count": 0,
        "internal_key_count": 0,
        "watch_only": true
    }
}
```

### Generating Addresses from an Imported Account

External addresses from an imported account can be generated through the
existing `Lightning.NewAddress` RPC and `lncli newaddress` command, as they now
take an additional optional parameter to specify which account the address
should be derived from.

Following the example above, Bob is able to generate an external address for an
incoming deposit as follows:

```shell
$ lncli-bob newaddress p2wkh --account=alice
{
    "address": "bcrt1q8zdjz2q92eh7jw9ah3upf2u9553226gq79el5l"
}
```

Change addresses cannot be generated on demand, they are generated automatically
when a transaction is crafted that requires a change output.

### Crafting Transactions through PSBTs from an Imported Account

Assuming a deposit of 1 tBTC was made to the address above
(`bcrt1q8zdjz2q92eh7jw9ah3upf2u9553226gq79el5l`), Bob should be able to craft a
transaction spending their new UTXO. Since Bob is unable to sign the transaction
themselves, they'll use PSBTs to craft the transaction, and provide it to Alice
to sign.

```shell
$ lncli-bob wallet psbt fund --account=alice --outputs="{\"bcrt1qpjqr663tylcksysa4u76xvremee9k8af3pqd5h\": 500000}" --sat_per_vbyte=1
{
        "psbt": "cHNidP8BAHECAAAAAYDHzEGcDW4Qf+gVbIgWpG2PVSUY6aZ3xUGk/3Ia/XnJAAAAAAD/////AiChBwAAAAAAFgAUDIA9aisn8WgSHa89ozB53nJbH6lWNf4pAQAAABYAFPwW6584J6Aku9bQ//xXsrI0VSjSAAAAAAABAKgCAAAAAAEBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/////A1oBAf////8CAPIFKgEAAAAWABQ4myEoBVZv6Ti9vHgUq4WlIqVpAAAAAAAAAAAAJmokqiGp7eL2HD9x0d79P6mZ36NpU3VcaQaJeZlitIvr2DaXToz5ASAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAR8A8gUqAQAAABYAFDibISgFVm/pOL28eBSrhaUipWkAAQMEAQAAACIGArbCQ3C0eTrSeuEokWjN7ty25lSzNxiClZL3tnbmlDG6GAAAAABUAACAAAAAgAAAAIAAAAAAAAAAAAAAAA==",
        "change_output_index": 1,
        "locks": [
                {
                        "id": "ede19a92ed321a4705f8a1cccc1d4f6182545d4bb4fae08bd5937831b7e38f98",
                        "outpoint": "c979fd1a72ffa441c577a6e91825558f6da416886c15e87f106e0d9c41ccc780:0",
                        "expiration": 1621632493
                }
        ]
}
```

The PSBT can then be provided to Alice to sign:

```shell
$ lncli-alice wallet psbt finalize --funded_psbt="cHNidP8BAHECAAAAAYDHzEGcDW4Qf+gVbIgWpG2PVSUY6aZ3xUGk/3Ia/XnJAAAAAAD/////AiChBwAAAAAAFgAUDIA9aisn8WgSHa89ozB53nJbH6lWNf4pAQAAABYAFPwW6584J6Aku9bQ//xXsrI0VSjSAAAAAAABAKgCAAAAAAEBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/////A1oBAf////8CAPIFKgEAAAAWABQ4myEoBVZv6Ti9vHgUq4WlIqVpAAAAAAAAAAAAJmokqiGp7eL2HD9x0d79P6mZ36NpU3VcaQaJeZlitIvr2DaXToz5ASAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAR8A8gUqAQAAABYAFDibISgFVm/pOL28eBSrhaUipWkAAQMEAQAAACIGArbCQ3C0eTrSeuEokWjN7ty25lSzNxiClZL3tnbmlDG6GAAAAABUAACAAAAAgAAAAIAAAAAAAAAAAAAAAA=="
{
        "psbt": "cHNidP8BAHECAAAAAYDHzEGcDW4Qf+gVbIgWpG2PVSUY6aZ3xUGk/3Ia/XnJAAAAAAD/////AiChBwAAAAAAFgAUDIA9aisn8WgSHa89ozB53nJbH6lWNf4pAQAAABYAFPwW6584J6Aku9bQ//xXsrI0VSjSAAAAAAABAKgCAAAAAAEBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD/////A1oBAf////8CAPIFKgEAAAAWABQ4myEoBVZv6Ti9vHgUq4WlIqVpAAAAAAAAAAAAJmokqiGp7eL2HD9x0d79P6mZ36NpU3VcaQaJeZlitIvr2DaXToz5ASAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABAR8A8gUqAQAAABYAFDibISgFVm/pOL28eBSrhaUipWkAAQhsAkgwRQIhALZOShGB8ATptNZFQ/R2h+2haZVoyBF7cW+GFp07ZbUNAiBzXYNYd5qS8BLQJDhEzW3VgxFhg9uRYedyhHEK1BVstwEhArbCQ3C0eTrSeuEokWjN7ty25lSzNxiClZL3tnbmlDG6AAAA",
        "final_tx": "0200000000010180c7cc419c0d6e107fe8156c8816a46d8f552518e9a677c541a4ff721afd79c90000000000ffffffff0220a10700000000001600140c803d6a2b27f168121daf3da33079de725b1fa95635fe2901000000160014fc16eb9f3827a024bbd6d0fffc57b2b2345528d202483045022100b64e4a1181f004e9b4d64543f47687eda1699568c8117b716f86169d3b65b50d0220735d8358779a92f012d0243844cd6dd583116183db9161e77284710ad4156cb7012102b6c24370b4793ad27ae1289168cdeedcb6e654b33718829592f7b676e69431ba00000000"
}
```

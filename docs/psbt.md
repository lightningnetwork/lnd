# PSBT

This document describes various use cases around the topic of Partially Signed
Bitcoin Transactions (PSBTs). Currently only channel funding is possible with
PSBTs but more features are planned.

See [BIP174](https://github.com/bitcoin/bips/blob/master/bip-0174.mediawiki) for
a full description of the PSBT format and the different _roles_ that a
participant in a PSBT can have.

## Opening a channel by using a PSBT

This is a step-by-step guide on how to open a channel with `lnd` by using a PSBT
as the funding transaction.  
We will use `bitcoind` to create and sign the transaction just to keep the
example simple. Of course any other PSBT compatible wallet could be used and the
process would likely be spread out over multiple signing steps. The goal of this
example is not to cover each and every possible edge case but to help users of
`lnd` understand what inputs the `lncli` utility expects.

The goal is to open a channel of 1'234'567 satoshis to the node
`03db1e56e5f76bc4018cf6f03d1bb98a7ae96e3f18535e929034f85e7f1ca2b8ac` by using
a PSBT. That means, `lnd` can have a wallet balance of `0` and is still able to
open a channel. We'll jump into an example right away.

The new funding flow has a small caveat: _Time matters_.
  
When opening a channel using the PSBT flow, we start the negotiation
with the remote peer immediately so we can obtain their multisig key they are
going to use for the channel. Then we pause the whole process until we get a
fully signed transaction back from the user. Unfortunately there is no reliable
way to know after how much time the remote node starts to clean up and "forgets"
about the pending channel. If the remote node is an `lnd` node, we know it's
after 10 minutes. **So as long as the whole process takes less than 10 minutes,
everything should work fine.**

### Safety warning

**DO NOT PUBLISH** the finished transaction by yourself or with another tool.
lnd MUST publish it in the proper funding flow order **OR THE FUNDS CAN BE
LOST**!

This is very important to remember when using wallets like `Wasabi` for
instance, where the "publish" button is very easy to hit by accident.

### 1. Use the new `--psbt` flag in `lncli openchannel`

The new `--psbt` flag in the `openchannel` command starts an interactive dialog
between `lncli` and the user. Below the command you see an example output from
a regtest setup. Of course all values will be different.

```bash
$ lncli openchannel --node_key 03db1e56e5f76bc4018cf6f03d1bb98a7ae96e3f18535e929034f85e7f1ca2b8ac --local_amt 1234567 --psbt

Starting PSBT funding flow with pending channel ID fc7853889a04d33b8115bd79ebc99c5eea80d894a0bead40fae5a06bcbdccd3d.
PSBT funding initiated with peer 03db1e56e5f76bc4018cf6f03d1bb98a7ae96e3f18535e929034f85e7f1ca2b8ac.
Please create a PSBT that sends 0.01234567 BTC (1234567 satoshi) to the funding address bcrt1qh33ghvgjj3ef625nl9jxz6nnrz2z9e65vsdey7w5msrklgr6rc0sv0s08q.

Example with bitcoind:
        bitcoin-cli walletcreatefundedpsbt [] '[{"bcrt1qh33ghvgjj3ef625nl9jxz6nnrz2z9e65vsdey7w5msrklgr6rc0sv0s08q":0.01234567}]'

Or if you are using a wallet that can fund a PSBT directly (currently not
possible with bitcoind), you can use this PSBT that contains the same address
and amount: cHNidP8BADUCAAAAAAGH1hIAAAAAACIAILxii7ESlHKdKpP5ZGFqcxiUIudUZBuSedTcB2+geh4fAAAAAAAA

Paste the funded PSBT here to continue the funding flow.
Base64 encoded PSBT:
```

The command line now waits until a PSBT is entered. We'll create one in the next
step. Make sure to use a new shell window/tab for the next commands and leave
the prompt from the `openchannel` running as is.

### 2. Use `bitcoind` to create a funding transaction

The output of the last command already gave us an example command to use with
`bitcoind`. We'll go ahead and execute it now. The meaning of this command is
something like "bitcoind, give me a PSBT that sends the given amount to the
given address, choose any input you see fit":

```bash
$ bitcoin-cli walletcreatefundedpsbt [] '[{"bcrt1qh33ghvgjj3ef625nl9jxz6nnrz2z9e65vsdey7w5msrklgr6rc0sv0s08q":0.01234567}]'

{
  "psbt": "cHNidP8BAH0CAAAAAbxLLf9+AYfqfF69QAQuETnL6cas7GDiWBZF+3xxc/Y/AAAAAAD+////AofWEgAAAAAAIgAgvGKLsRKUcp0qk/lkYWpzGJQi51RkG5J51NwHb6B6Hh+1If0jAQAAABYAFL+6THEGhybJnOkFGSRFbtCcPOG8AAAAAAABAR8wBBAkAQAAABYAFHemJ11XF7CU7WXBIJLD/qZF+6jrAAAA",
  "fee": 0.00003060,
  "changepos": 1
}
```

We see that `bitcoind` has given us a transaction that would pay `3060` satoshi
in fees. Fee estimation/calculation can be changed with parameters of the 
`walletcreatefundedpsbt` command. To see all options, use
`bitcoin-cli help walletcreatefundedpsbt`.

If we want to know what exactly is in this PSBT, we can look at it with the
`decodepsbt` command:

```bash
$ bitcoin-cli decodepsbt cHNidP8BAH0CAAAAAbxLLf9+AYfqfF69QAQuETnL6cas7GDiWBZF+3xxc/Y/AAAAAAD+////AofWEgAAAAAAIgAgvGKLsRKUcp0qk/lkYWpzGJQi51RkG5J51NwHb6B6Hh+1If0jAQAAABYAFL+6THEGhybJnOkFGSRFbtCcPOG8AAAAAAABAR8wBBAkAQAAABYAFHemJ11XF7CU7WXBIJLD/qZF+6jrAAAA

{
  "tx": {
    "txid": "374504e4246a93a45b4a2c2bc31d8adc8525aa101c7b9065db6dc01c4bdfce0a",
    "hash": "374504e4246a93a45b4a2c2bc31d8adc8525aa101c7b9065db6dc01c4bdfce0a",
    "version": 2,
    "size": 125,
    "vsize": 125,
    "weight": 500,
    "locktime": 0,
    "vin": [
      {
        "txid": "3ff673717cfb451658e260ecacc6e9cb39112e0440bd5e7cea87017eff2d4bbc",
        "vout": 0,
        "scriptSig": {
          "asm": "",
          "hex": ""
        },
        "sequence": 4294967294
      }
    ],
    "vout": [
      {
        "value": 0.01234567,
        "n": 0,
        "scriptPubKey": {
          "asm": "0 bc628bb11294729d2a93f964616a73189422e754641b9279d4dc076fa07a1e1f",
          "hex": "0020bc628bb11294729d2a93f964616a73189422e754641b9279d4dc076fa07a1e1f",
          "reqSigs": 1,
          "type": "witness_v0_scripthash",
          "addresses": [
            "bcrt1qh33ghvgjj3ef625nl9jxz6nnrz2z9e65vsdey7w5msrklgr6rc0sv0s08q"
          ]
        }
      },
      {
        "value": 48.98759093,
        "n": 1,
        "scriptPubKey": {
          "asm": "0 bfba4c71068726c99ce9051924456ed09c3ce1bc",
          "hex": "0014bfba4c71068726c99ce9051924456ed09c3ce1bc",
          "reqSigs": 1,
          "type": "witness_v0_keyhash",
          "addresses": [
            "bcrt1qh7aycugxsunvn88fq5vjg3tw6zwrecduvvgre5"
          ]
        }
      }
    ]
  },
  "unknown": {
  },
  "inputs": [
    {
      "witness_utxo": {
        "amount": 48.99996720,
        "scriptPubKey": {
          "asm": "0 77a6275d5717b094ed65c12092c3fea645fba8eb",
          "hex": "001477a6275d5717b094ed65c12092c3fea645fba8eb",
          "type": "witness_v0_keyhash",
          "address": "bcrt1qw7nzwh2hz7cffmt9cysf9sl75ezlh28tzl4n4e"
        }
      }
    }
  ],
  "outputs": [
    {
    },
    {
    }
  ],
  "fee": 0.00003060
}
```

This tells us that we got a PSBT with a big input, the channel output and a
change output for the rest. Everything is there but the signatures/witness data,
which is exactly what we need.

### 3. Verify and sign the PSBT

Now that we have a valid PSBT that has everything but the final
signatures/witness data, we can paste it into the prompt in `lncli` that is
still waiting for our input.

```bash
...
Base64 encoded PSBT: cHNidP8BAH0CAAAAAbxLLf9+AYfqfF69QAQuETnL6cas7GDiWBZF+3xxc/Y/AAAAAAD+////AofWEgAAAAAAIgAgvGKLsRKUcp0qk/lkYWpzGJQi51RkG5J51NwHb6B6Hh+1If0jAQAAABYAFL+6THEGhybJnOkFGSRFbtCcPOG8AAAAAAABAR8wBBAkAQAAABYAFHemJ11XF7CU7WXBIJLD/qZF+6jrAAAA

PSBT verified by lnd, please continue the funding flow by signing the PSBT by
all required parties/devices. Once the transaction is fully signed, paste it
again here.

Base64 encoded PSBT:
```

We can now go ahead and sign the transaction. We are going to use `bitcoind` for
this again, but in practice this would now happen on a hardware wallet and
perhaps `bitcoind` would only know the public keys and couldn't sign for the
transaction itself. Again, this is only an example and can't reflect all
real-world use cases.

```bash
$ bitcoin-cli walletprocesspsbt cHNidP8BAH0CAAAAAbxLLf9+AYfqfF69QAQuETnL6cas7GDiWBZF+3xxc/Y/AAAAAAD+////AofWEgAAAAAAIgAgvGKLsRKUcp0qk/lkYWpzGJQi51RkG5J51NwHb6B6Hh+1If0jAQAAABYAFL+6THEGhybJnOkFGSRFbtCcPOG8AAAAAAABAR8wBBAkAQAAABYAFHemJ11XF7CU7WXBIJLD/qZF+6jrAAAA

{
"psbt": "cHNidP8BAH0CAAAAAbxLLf9+AYfqfF69QAQuETnL6cas7GDiWBZF+3xxc/Y/AAAAAAD+////AofWEgAAAAAAIgAgvGKLsRKUcp0qk/lkYWpzGJQi51RkG5J51NwHb6B6Hh+1If0jAQAAABYAFL+6THEGhybJnOkFGSRFbtCcPOG8AAAAAAABAR8wBBAkAQAAABYAFHemJ11XF7CU7WXBIJLD/qZF+6jrAQhrAkcwRAIgHKQbenZYvgADRd9TKGVO36NnaIgW3S12OUg8XGtSrE8CICmeaYoJ/U7Ecm+/GneY8i2hu2QCaQnuomJgzn+JAnrDASEDUBmCLcsybA5qXSRBBdZ0Uk/FQiay9NgOpv4D26yeJpAAAAA=",
"complete": true
}
```

Interpreting the output, we now have a complete, final, and signed transaction
inside the PSBT.

**!!! WARNING !!!**

**DO NOT PUBLISH** the finished transaction by yourself or with another tool.
lnd MUST publish it in the proper funding flow order **OR THE FUNDS CAN BE
LOST**!

Let's give it to `lncli` to continue:

```bash
...
Base64 encoded PSBT: cHNidP8BAH0CAAAAAbxLLf9+AYfqfF69QAQuETnL6cas7GDiWBZF+3xxc/Y/AAAAAAD+////AofWEgAAAAAAIgAgvGKLsRKUcp0qk/lkYWpzGJQi51RkG5J51NwHb6B6Hh+1If0jAQAAABYAFL+6THEGhybJnOkFGSRFbtCcPOG8AAAAAAABAR8wBBAkAQAAABYAFHemJ11XF7CU7WXBIJLD/qZF+6jrAQhrAkcwRAIgHKQbenZYvgADRd9TKGVO36NnaIgW3S12OUg8XGtSrE8CICmeaYoJ/U7Ecm+/GneY8i2hu2QCaQnuomJgzn+JAnrDASEDUBmCLcsybA5qXSRBBdZ0Uk/FQiay9NgOpv4D26yeJpAAAAA=
{
        "funding_txid": "374504e4246a93a45b4a2c2bc31d8adc8525aa101c7b9065db6dc01c4bdfce0a"
}
```

Success! We now have the final transaction ID of the published funding
transaction. Now we only have to wait for some confirmations, then we can start
using the freshly created channel.

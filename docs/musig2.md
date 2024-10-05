# MuSig2

With `lnd v0.15.0-beta` a new, experimental MuSig2 API was added to the
`signrpc` subserver RPC. With MuSig2 it is possible to combine the public keys
of multiple signing participants into a single, combined key. The signers can
then later come together and cooperatively produce a single signature that is
valid for that combined public key. MuSig2 therefore is an interactive `n-of-n`
signature scheme that produces a final/complete Schnorr signature out of `n`
partial signatures.

**NOTE**: At the time the MuSig2 code in `btcd`/`lnd` was written, there was no
"official" MuSig2 support merged to either `bitcoind` or `secp256k1`. Therefore,
some smaller details in the signing protocol could change in the future that
might not be backward compatible. So this API must be seen as highly
experimental and backward compatibility can't be guaranteed at this point.
See the [versions and compatibility matrix](#versions-and-compatibility-matrix)
below.

## References
 * [MuSig2 paper](https://eprint.iacr.org/2020/1261.pdf)
 * [BIP 327 MuSig](https://github.com/bitcoin/bips/blob/master/bip-0327.mediawiki)
 * [MuSig2 implementation discussion in `bitcoind`](https://github.com/bitcoin/bitcoin/issues/23326)

## A note on security

The MuSig2 signing protocol is secure from leaking private key information of
the signers **as long as the same secret nonce is never used multiple times**.
If the same nonce is used for signing multiple times then the private key can
leak. To avoid this security risk, the `signrpc.MuSig2Sign` RPC can only be
called a single time for the same session ID. This has the implication that if a
signing session fails or is aborted (for example because a participant isn't
responsive or the message changes after some participants have already started
signing), a completely new signing session needs to be initialized, which
internally creates fresh nonces.

## Examples

An API is sometimes easiest explained by showing concrete usage examples. Here
we take a look at the MuSig2 integration tests in `lnd`, since they both serve
to test the RPCs and to showcase the different use cases.

### 3-of-3 Taproot key spend path (BIP-0086)

See `testTaprootMuSig2KeySpendBip86` in
[`itest/lnd_taproot_test.go`](../itest/lnd_taproot_test.go) to see
the full code.

This example uses combines the public keys of 3 participants into a shared
MuSig2 public key that is then tweaked with the
[BIP-0086](https://github.com/bitcoin/bips/blob/master/bip-0086.mediawiki#address-derivation)
TapTweak to be turned into a Taproot public key that can be used directly as the
`pkScript` of a p2tr output on chain.

The most notable parameter for this to work is the `TaprootTweak` parameter in
the `MuSig2CreateSession` RPC call:

```go
	taprootTweak := &signrpc.TaprootTweakDesc{
        KeySpendOnly: true,
	}
	
	sessResp1, err := node.SignerClient.MuSig2CreateSession(
		ctx, &signrpc.MuSig2SessionRequest{
			KeyLoc:           keyDesc1.KeyLoc,
			AllSignerPubkeys: allPubKeys,
			TaprootTweak:     taprootTweak,
		},
	)
```

### 3-of-3 Taproot key spend path with root hash commitment

See `testTaprootMuSig2KeySpendRootHash` in
[`itest/lnd_taproot_test.go`](../itest/lnd_taproot_test.go) to see
the full code.

This is very similar to the above example but with the main difference that the
p2tr output on chain not only commits to a key spend path but also a script
path. The MuSig2 combined key becomes the Taproot internal key and the TapTweak
commits to both the internal key and the Taproot script merkle root hash.

The most notable parameter for this to work is the `TaprootTweak` parameter in
the `MuSig2CreateSession` RPC call:

```go
	taprootTweak := &signrpc.TaprootTweakDesc{
	    ScriptRoot: rootHash[:],
	}
	
	sessResp1, err := node.SignerClient.MuSig2CreateSession(
		ctx, &signrpc.MuSig2SessionRequest{
			KeyLoc:           keyDesc1.KeyLoc,
			AllSignerPubkeys: allPubKeys,
			TaprootTweak:     taprootTweak,
		},
	)
```

### 3-of-3 `OP_CHECKSIG` in Taproot script spend path

See `testTaprootMuSig2CombinedLeafKeySpend` in
[`itest/lnd_taproot_test.go`](../itest/lnd_taproot_test.go) to see
the full code.

This example is definitely the most involved one. To be able to use a MuSig2
combined key and then spend it through a Taproot script spend with an
`OP_CHECKSIG` script, the following steps need to be performed:

1. Derive signing keys on signers, combine them through `MuSig2CombineKeys`.
2. Create a Taproot script tree with a script leaf `<combinedKey> OP_CHECKSIG`.
3. Create the Taproot key by committing to the script tree root hash.
4. When spending the output, the message being signed needs to be the sighash of
   a Taproot script spend that also commits to the leaf hash.
5. The final witness stack needs to contain the combined signature, the leaf
   script and the control block (which contains the internal public key and the
   inclusion proof if there were any other script leaves).


# Versions and compatibility matrix

The [MuSig2 BIP
draft](https://github.com/jonasnick/bips/blob/musig2/bip-musig2.mediawiki)
underwent (and is likely still undergoing) multiple changes and is being
versioned for that reason. Starting with `lnd v0.16.0-beta` the MuSig2 RPCs will
offer backward compatibility in order to support applications that might already
have created MuSig2 based outputs on chain with an earlier version of the
protocol.

## MuSig2 versions in lnd

* `lnd v0.15.x-beta`: Uses MuSig2 `v0.4.0` exclusively.
* `lnd v0.16.x-beta`: Supports MuSig2 `v0.4.0` and `v1.0.0rc2` by introducing a
  new **mandatory version** enum `MuSig2Version` that must be specified for the
  `MuSig2CombineKeys` and `MuSig2CreateSession` RPCs. A session created with a
  specific version will use that version during its lifetime (e.g. calls to
  RPCs that specify the `session_id` field automatically know what version of
  the MuSig2 API to use under the hood).

## Upgrading client applications

Client software using the MuSig2 API (such as Loop or Pool) will stop working
with `lnd v0.16.0-beta` if they aren't also updated because the added version
enum mentioned above is mandatory. In order to prepare for the `lnd 0.16.0-beta`
release such client software should use one of the following two strategies to
make sure forward compatibility is ensured:
 - Compile against `lnd` on `master` branch to pull in updated RPC definitions.
   Use the `GetVersion` RPC in the `verrpc` package to determine what `lnd`
   version the application is connected to. Expect MuSig2 `v0.4.0` to be used
   for `lnd v0.15.x-beta` and expect to explicitly set the `version` field on
   the `MuSig2CombineKeys` and `MuSig2CreateSession` RPCs for
   `lnd-v0.16.x-beta`. This is the recommended approach.
 - Compile against `lnd` on `master` branch to pull in updated RPC definitions.
   Always set the `version` field on the `MuSig2CombineKeys` and
   `MuSig2CreateSession` RPCs and check the `version` field in their respective
   response messages. If the `version` in the response reflects the version
   sent in the request, you're using `lnd v0.16.x-beta` or later. If the
   `version` is returned as `MUSIG2_VERSION_UNDEFINED` you're using
   `lnd v0.15.x-beta` and only `v0.4.0` is supported.

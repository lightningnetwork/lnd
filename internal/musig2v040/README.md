# MuSig2 v0.4.0

This package contains an exact copy of the MuSig2 code as found in
`github.com/btcsuite/btcec/v2/schnorr/musig2` at the tag `btcec/v2.2.2`.

This corresponds to the [MuSig2 BIP specification version of
`v0.4.0`](https://github.com/jonasnick/bips/blob/musig2/bip-musig2.mediawiki).

We only keep this code here to allow implementing a backward compatible,
versioned MuSig2 RPC.

## Unsupported Methods

The following methods from the newer MuSig2 specifications are not supported in
this legacy v0.4.0 implementation and will return `ErrUnsupportedMethod` if
called:

- `CombinedNonce()`: Returns error instead of the combined nonce.
- `RegisterCombinedNonce()`: Returns error instead of registering a
  pre-aggregated combined nonce.

These methods are only available when using MuSig2 v1.0.0rc2 or later. To use
these features, create sessions with `MuSig2Version100RC2` instead of
`MuSig2Version040`.

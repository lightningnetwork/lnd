// Package bolt12 implements encoding, decoding, and validation for BOLT 12
// Offers, Invoice Requests, and Invoices. It provides a pure codec library
// with no LND daemon dependencies.
//
// BOLT 12 messages use TLV streams encoded with a checksumless bech32 variant
// and signed with BIP-340 Schnorr signatures over a Merkle tree of TLV fields.
//
// Human-readable prefixes:
//   - lno: Offer
//   - lnr: Invoice Request
//   - lni: Invoice
//
// # Codec Contract
//
// Encode validates before serialising and refuses to emit bytes that would fail
// the writer requirements, invalid bytes are unrepresentable on the wire.
// Low-level decoders stay permissive so diagnostic and fuzz harnesses can
// inspect malformed input.
package bolt12

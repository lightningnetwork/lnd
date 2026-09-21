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
//
// DecodeOfferString, DecodeInvoiceRequestString, and DecodeInvoiceString
// (with their Encode counterparts) are the consumer entry point. Each folds
// bech32, the per-message TLV codec, and the spec reader gates into one
// validated call.
//
// An invoice that arrives as the response to an invoice request needs two
// further bindings that the message alone cannot supply, the mirror match
// against that request and the blinded-path node binding. A payer holding
// that request gates the decoded invoice with ValidateInvoiceForPayment,
// which the string wrappers cannot do for it. Such an invoice arrives as raw
// TLV over an onion message rather than as a string, so the payer path
// decodes with DecodeInvoice and validates separately.
package bolt12

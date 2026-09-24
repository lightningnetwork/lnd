// Package bolt12 implements encoding, decoding, and validation for BOLT 12
// Offers, Invoice Requests, and Invoices. It is a codec library: it does not
// reach into the daemon, and it takes the chain, the clock, the known feature
// bits and the node it expected to answer from its caller. Anything a message
// cannot prove about itself is the caller's to supply.
//
// BOLT 12 messages are TLV streams signed with BIP-340 Schnorr signatures over
// a Merkle tree of their own fields.
//
// # The flow
//
// A merchant publishes an offer out of band. A payer reads it with
// DecodeOfferString, mirrors its fields into a request with
// NewInvoiceRequestFromOffer, signs that with SignInvoiceRequest, and sends the
// EncodeSigned bytes inside an onion message. The receiver decodes them with
// DecodeInvoiceRequest, gates the result with ValidateInvoiceRequestRead, and
// answers with an invoice built by NewInvoiceFromRequest and signed with
// SignInvoice. The payer decodes that reply with DecodeInvoice, gates it with
// ValidateInvoiceForPayment, and pays the paths UsablePaths returns.
// TestOfferPaymentFlow in offer_test.go runs these steps in order, one per
// party.
//
// A payment can also start without an offer. The payer builds a request with no
// offer_issuer_id or offer_paths, signs it with SignInvoiceRequest, and
// publishes it with EncodeInvoiceRequestString as an lnr1 string, such as a QR
// code. The payee reads it with DecodeInvoiceRequestString and answers with an
// invoice built and signed as above, sent to invreq_paths or invreq_payer_id.
// The payer gates that invoice with ValidateInvoiceForPayment, passing a nil
// node id unless it confirmed the payee's key out of band.
// TestOfferlessPaymentFlow in offer_test.go runs these steps.
//
// # Wire form or string form
//
// An offer only ever travels out of band, as an lno1 string. An invoice_request
// and an invoice reach a peer as raw TLV inside an onion message, which is what
// EncodeSigned emits. An invoice also has an lni1 string, for display and for
// out-of-band delivery. An invoice_request that answers no offer is published
// as an lnr1 string instead, which EncodeInvoiceRequestString emits signed.
//
// # Pitfalls
//
// Gating a reply invoice against itself is not enough. The read gates check an
// invoice in isolation, but the mirror match against the request, the node
// binding and the expiry need state only the payer holds, and only
// ValidateInvoiceForPayment applies them. A payer that merely decodes and reads
// will accept a correctly signed invoice from the wrong node.
//
// Signing and encoding are independent. Signing reads the struct, so encoding
// never has to run first and does not require a signature. EncodeSigned and the
// string encoders are where a signature becomes mandatory.
//
// Unknown fields must survive a round trip. A signature covers the Merkle root
// over the message's own TLVs, so re-encoding has to reproduce the wire bytes.
// That is why decoding rejects a non-minimal encoding rather than normalising
// it, and why unknown TLVs are preserved verbatim.
//
// DecodeInvoiceStringUnvalidated skips every gate. Use it only to display an
// invoice that was validated when it was stored.
package bolt12

package zpay32

import (
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
)

const (
	// mSatPerBtc is the number of millisatoshis in 1 BTC.
	mSatPerBtc = 100000000000

	// signatureBase32Len is the number of 5-bit groups needed to encode
	// the 512 bit signature + 8 bit recovery ID.
	signatureBase32Len = 104

	// timestampBase32Len is the number of 5-bit groups needed to encode
	// the 35-bit timestamp.
	timestampBase32Len = 7

	// hashBase32Len is the number of 5-bit groups needed to encode a
	// 256-bit hash. Note that the last group will be padded with zeroes.
	hashBase32Len = 52

	// pubKeyBase32Len is the number of 5-bit groups needed to encode a
	// 33-byte compressed pubkey. Note that the last group will be padded
	// with zeroes.
	pubKeyBase32Len = 53

	// hopHintLen is the number of bytes needed to encode the hop hint of a
	// single private route.
	hopHintLen = 51

	// The following byte values correspond to the supported field types.
	// The field name is the character representing that 5-bit value in the
	// bech32 string.

	// fieldTypeP is the field containing the payment hash.
	fieldTypeP = 1

	// fieldTypeD contains a short description of the payment.
	fieldTypeD = 13

	// fieldTypeN contains the pubkey of the target node.
	fieldTypeN = 19

	// fieldTypeH contains the hash of a description of the payment.
	fieldTypeH = 23

	// fieldTypeX contains the expiry in seconds of the invoice.
	fieldTypeX = 6

	// fieldTypeF contains a fallback on-chain address.
	fieldTypeF = 9

	// fieldTypeR contains extra routing information.
	fieldTypeR = 3

	// fieldTypeC contains an optional requested final CLTV delta.
	fieldTypeC = 24

	// fieldType9 contains one or more bytes for signaling features
	// supported or required by the receiver.
	fieldType9 = 5

	// fieldTypeS contains a 32-byte payment address, which is a nonce
	// included in the final hop's payload to prevent intermediaries from
	// probing the recipient.
	fieldTypeS = 16

	// maxInvoiceLength is the maximum total length an invoice can have.
	// This is chosen to be the maximum number of bytes that can fit into a
	// single QR code: https://en.wikipedia.org/wiki/QR_code#Storage
	maxInvoiceLength = 7089

	// DefaultInvoiceExpiry is the default expiry duration from the creation
	// timestamp if expiry is set to zero.
	DefaultInvoiceExpiry = time.Hour
)

var (
	// ErrInvoiceTooLarge is returned when an invoice exceeds
	// maxInvoiceLength.
	ErrInvoiceTooLarge = errors.New("invoice is too large")

	// ErrInvalidFieldLength is returned when a tagged field was specified
	// with a length larger than the left over bytes of the data field.
	ErrInvalidFieldLength = errors.New("invalid field length")

	// ErrBrokenTaggedField is returned when the last tagged field is
	// incorrectly formatted and doesn't have enough bytes to be read.
	ErrBrokenTaggedField = errors.New("last tagged field is broken")
)

// MessageSigner is passed to the Encode method to provide a signature
// corresponding to the node's pubkey.
type MessageSigner struct {
	// SignCompact signs the passed hash with the node's privkey. The
	// returned signature should be 65 bytes, where the last 64 are the
	// compact signature, and the first one is a header byte. This is the
	// format returned by ecdsa.SignCompact.
	SignCompact func(hash []byte) ([]byte, error)
}

// Invoice represents a decoded invoice, or to-be-encoded invoice. Some of the
// fields are optional, and will only be non-nil if the invoice this was parsed
// from contains that field. When encoding, only the non-nil fields will be
// added to the encoded invoice.
type Invoice struct {
	// Net specifies what network this Lightning invoice is meant for.
	Net *chaincfg.Params

	// MilliSat specifies the amount of this invoice in millisatoshi.
	// Optional.
	MilliSat *lnwire.MilliSatoshi

	// Timestamp specifies the time this invoice was created.
	// Mandatory
	Timestamp time.Time

	// PaymentHash is the payment hash to be used for a payment to this
	// invoice.
	PaymentHash *[32]byte

	// PaymentAddr is the payment address to be used by payments to prevent
	// probing of the destination.
	PaymentAddr *[32]byte

	// Destination is the public key of the target node. This will always
	// be set after decoding, and can optionally be set before encoding to
	// include the pubkey as an 'n' field. If this is not set before
	// encoding then the destination pubkey won't be added as an 'n' field,
	// and the pubkey will be extracted from the signature during decoding.
	Destination *btcec.PublicKey

	// minFinalCLTVExpiry is the value that the creator of the invoice
	// expects to be used for the CLTV expiry of the HTLC extended to it in
	// the last hop.
	//
	// NOTE: This value is optional, and should be set to nil if the
	// invoice creator doesn't have a strong requirement on the CLTV expiry
	// of the final HTLC extended to it.
	//
	// This field is un-exported and can only be read by the
	// MinFinalCLTVExpiry() method. By forcing callers to read via this
	// method, we can easily enforce the default if not specified.
	minFinalCLTVExpiry *uint64

	// Description is a short description of the purpose of this invoice.
	// Optional. Non-nil iff DescriptionHash is nil.
	Description *string

	// DescriptionHash is the SHA256 hash of a description of the purpose of
	// this invoice.
	// Optional. Non-nil iff Description is nil.
	DescriptionHash *[32]byte

	// expiry specifies the timespan this invoice will be valid.
	// Optional. If not set, a default expiry of 60 min will be implied.
	//
	// This field is unexported and can be read by the Expiry() method. This
	// method makes sure the default expiry time is returned in case the
	// field is not set.
	expiry *time.Duration

	// FallbackAddr is an on-chain address that can be used for payment in
	// case the Lightning payment fails.
	// Optional.
	FallbackAddr btcutil.Address

	// RouteHints represents one or more different route hints. Each route
	// hint can be individually used to reach the destination. These usually
	// represent private routes.
	//
	// NOTE: This is optional.
	RouteHints [][]HopHint

	// Features represents an optional field used to signal optional or
	// required support for features by the receiver.
	Features *lnwire.FeatureVector
}

// Amount is a functional option that allows callers of NewInvoice to set the
// amount in millisatoshis that the Invoice should encode.
func Amount(milliSat lnwire.MilliSatoshi) func(*Invoice) {
	return func(i *Invoice) {
		i.MilliSat = &milliSat
	}
}

// Destination is a functional option that allows callers of NewInvoice to
// explicitly set the pubkey of the Invoice's destination node.
func Destination(destination *btcec.PublicKey) func(*Invoice) {
	return func(i *Invoice) {
		i.Destination = destination
	}
}

// Description is a functional option that allows callers of NewInvoice to set
// the payment description of the created Invoice.
//
// NOTE: Must be used if and only if DescriptionHash is not used.
func Description(description string) func(*Invoice) {
	return func(i *Invoice) {
		i.Description = &description
	}
}

// CLTVExpiry is an optional value which allows the receiver of the payment to
// specify the delta between the current height and the HTLC extended to the
// receiver.
func CLTVExpiry(delta uint64) func(*Invoice) {
	return func(i *Invoice) {
		i.minFinalCLTVExpiry = &delta
	}
}

// DescriptionHash is a functional option that allows callers of NewInvoice to
// set the payment description hash of the created Invoice.
//
// NOTE: Must be used if and only if Description is not used.
func DescriptionHash(descriptionHash [32]byte) func(*Invoice) {
	return func(i *Invoice) {
		i.DescriptionHash = &descriptionHash
	}
}

// Expiry is a functional option that allows callers of NewInvoice to set the
// expiry of the created Invoice. If not set, a default expiry of 60 min will
// be implied.
func Expiry(expiry time.Duration) func(*Invoice) {
	return func(i *Invoice) {
		i.expiry = &expiry
	}
}

// FallbackAddr is a functional option that allows callers of NewInvoice to set
// the Invoice's fallback on-chain address that can be used for payment in case
// the Lightning payment fails
func FallbackAddr(fallbackAddr btcutil.Address) func(*Invoice) {
	return func(i *Invoice) {
		i.FallbackAddr = fallbackAddr
	}
}

// RouteHint is a functional option that allows callers of NewInvoice to add
// one or more hop hints that represent a private route to the destination.
func RouteHint(routeHint []HopHint) func(*Invoice) {
	return func(i *Invoice) {
		i.RouteHints = append(i.RouteHints, routeHint)
	}
}

// Features is a functional option that allows callers of NewInvoice to set the
// desired feature bits that are advertised on the invoice. If this option is
// not used, an empty feature vector will automatically be populated.
func Features(features *lnwire.FeatureVector) func(*Invoice) {
	return func(i *Invoice) {
		i.Features = features
	}
}

// PaymentAddr is a functional option that allows callers of NewInvoice to set
// the desired payment address that is advertised on the invoice.
func PaymentAddr(addr [32]byte) func(*Invoice) {
	return func(i *Invoice) {
		i.PaymentAddr = &addr
	}
}

// NewInvoice creates a new Invoice object. The last parameter is a set of
// variadic arguments for setting optional fields of the invoice.
//
// NOTE: Either Description  or DescriptionHash must be provided for the Invoice
// to be considered valid.
func NewInvoice(net *chaincfg.Params, paymentHash [32]byte,
	timestamp time.Time, options ...func(*Invoice)) (*Invoice, error) {

	invoice := &Invoice{
		Net:         net,
		PaymentHash: &paymentHash,
		Timestamp:   timestamp,
	}

	for _, option := range options {
		option(invoice)
	}

	// If no features were set, we'll populate an empty feature vector.
	if invoice.Features == nil {
		invoice.Features = lnwire.NewFeatureVector(
			nil, lnwire.Features,
		)
	}

	if err := validateInvoice(invoice); err != nil {
		return nil, err
	}

	return invoice, nil
}

// Expiry returns the expiry time for this invoice. If expiry time is not set
// explicitly, the default 3600 second expiry will be returned.
func (invoice *Invoice) Expiry() time.Duration {
	if invoice.expiry != nil {
		return *invoice.expiry
	}

	// If no expiry is set for this invoice, default is 3600 seconds.
	return DefaultInvoiceExpiry
}

// MinFinalCLTVExpiry returns the minimum final CLTV expiry delta as specified
// by the creator of the invoice. This value specifies the delta between the
// current height and the expiry height of the HTLC extended in the last hop.
func (invoice *Invoice) MinFinalCLTVExpiry() uint64 {
	if invoice.minFinalCLTVExpiry != nil {
		return *invoice.minFinalCLTVExpiry
	}

	return DefaultFinalCLTVDelta
}

// validateInvoice does a sanity check of the provided Invoice, making sure it
// has all the necessary fields set for it to be considered valid by BOLT-0011.
func validateInvoice(invoice *Invoice) error {
	// The net must be set.
	if invoice.Net == nil {
		return fmt.Errorf("net params not set")
	}

	// The invoice must contain a payment hash.
	if invoice.PaymentHash == nil {
		return fmt.Errorf("no payment hash found")
	}

	// Either Description or DescriptionHash must be set, not both.
	if invoice.Description != nil && invoice.DescriptionHash != nil {
		return fmt.Errorf("both description and description hash set")
	}
	if invoice.Description == nil && invoice.DescriptionHash == nil {
		return fmt.Errorf("neither description nor description hash set")
	}

	// Check that we support the field lengths.
	if len(invoice.PaymentHash) != 32 {
		return fmt.Errorf("unsupported payment hash length: %d",
			len(invoice.PaymentHash))
	}

	if invoice.DescriptionHash != nil && len(invoice.DescriptionHash) != 32 {
		return fmt.Errorf("unsupported description hash length: %d",
			len(invoice.DescriptionHash))
	}

	if invoice.Destination != nil &&
		len(invoice.Destination.SerializeCompressed()) != 33 {
		return fmt.Errorf("unsupported pubkey length: %d",
			len(invoice.Destination.SerializeCompressed()))
	}

	// Ensure that all invoices have feature vectors.
	if invoice.Features == nil {
		return fmt.Errorf("missing feature vector")
	}

	return nil
}

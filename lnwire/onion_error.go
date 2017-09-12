package lnwire

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"bytes"

	"github.com/go-errors/errors"
)

// FailureMessage represents the onion failure object identified by its unique
// failure code.
type FailureMessage interface {
	Code() FailCode
}

// failureMessageLength is the size of the failure message plus the size of
// padding. The FailureMessage message should always be EXACLTY this size.
const failureMessageLength = 256

const (
	// FlagBadOnion error flag describes an unparseable, encrypted by
	// previous node.
	FlagBadOnion FailCode = 0x8000

	// FlagPerm error flag indicates a permanent failure.
	FlagPerm FailCode = 0x4000

	// FlagNode error flag indicates anode failure.
	FlagNode FailCode = 0x2000

	// FlagUpdate error flag indicates a new channel update is enclosed
	// within the error.
	FlagUpdate FailCode = 0x1000
)

// FailCode specifies the precise reason that an upstream HTLC was cancelled.
// Each UpdateFailHTLC message carries a FailCode which is to be passed
// backwards, encrypted at each step back to the source of the HTLC within the
// route.
type FailCode uint16

// The currently defined onion failure types within this current version of the
// Lightning protocol.
const (
	CodeNone                          FailCode = 0
	CodeInvalidRealm                           = FlagBadOnion | 1
	CodeTemporaryNodeFailure                   = FlagNode | 2
	CodePermanentNodeFailure                   = FlagPerm | FlagNode | 2
	CodeRequiredNodeFeatureMissing             = FlagPerm | FlagNode | 3
	CodeInvalidOnionVersion                    = FlagBadOnion | FlagPerm | 4
	CodeInvalidOnionHmac                       = FlagBadOnion | FlagPerm | 5
	CodeInvalidOnionKey                        = FlagBadOnion | FlagPerm | 6
	CodeTemporaryChannelFailure                = FlagUpdate | 7
	CodePermanentChannelFailure                = FlagPerm | 8
	CodeRequiredChannelFeatureMissing          = FlagPerm | 9
	CodeUnknownNextPeer                        = FlagPerm | 10
	CodeAmountBelowMinimum                     = FlagUpdate | 11
	CodeFeeInsufficient                        = FlagUpdate | 12
	CodeIncorrectCltvExpiry                    = FlagUpdate | 13
	CodeExpiryTooSoon                          = FlagUpdate | 14
	CodeChannelDisabled                        = FlagUpdate | 20
	CodeUnknownPaymentHash                     = FlagPerm | 15
	CodeIncorrectPaymentAmount                 = FlagPerm | 16
	CodeFinalExpiryTooSoon            FailCode = 17
	CodeFinalIncorrectCltvExpiry      FailCode = 18
	CodeFinalIncorrectHtlcAmount      FailCode = 19
)

// String returns the string representation of the failure code.
func (c FailCode) String() string {
	switch c {
	case CodeInvalidRealm:
		return "InvalidRealm"

	case CodeTemporaryNodeFailure:
		return "TemporaryNodeFailure"

	case CodePermanentNodeFailure:
		return "PermanentNodeFailure"

	case CodeRequiredNodeFeatureMissing:
		return "RequiredNodeFeatureMissing"

	case CodeInvalidOnionVersion:
		return "InvalidOnionVersion"

	case CodeInvalidOnionHmac:
		return "InvalidOnionHmac"

	case CodeInvalidOnionKey:
		return "InvalidOnionKey"

	case CodeTemporaryChannelFailure:
		return "TemporaryChannelFailure"

	case CodePermanentChannelFailure:
		return "PermanentChannelFailure"

	case CodeRequiredChannelFeatureMissing:
		return "RequiredChannelFeatureMissing"

	case CodeUnknownNextPeer:
		return "UnknownNextPeer"

	case CodeAmountBelowMinimum:
		return "AmountBelowMinimum"

	case CodeFeeInsufficient:
		return "FeeInsufficient"

	case CodeIncorrectCltvExpiry:
		return "IncorrectCltvExpiry"

	case CodeExpiryTooSoon:
		return "ExpiryTooSoon"

	case CodeChannelDisabled:
		return "ChannelDisabled"

	case CodeUnknownPaymentHash:
		return "UnknownPaymentHash"

	case CodeIncorrectPaymentAmount:
		return "IncorrectPaymentAmount"

	case CodeFinalExpiryTooSoon:
		return "FinalExpiryTooSoon"

	case CodeFinalIncorrectCltvExpiry:
		return "FinalIncorrectCltvExpiry"

	case CodeFinalIncorrectHtlcAmount:
		return "FinalIncorrectHtlcAmount"

	default:
		return "<unknown>"
	}
}

// FailInvalidRealm is returned if the realm byte is unknown.
//
// NOTE: May be returned by any node in the payment route.
type FailInvalidRealm struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailInvalidRealm) Code() FailCode {
	return CodeInvalidRealm
}

// FailTemporaryNodeFailure is returned if an otherwise unspecified transient
// error occurs for the entire node.
//
// NOTE: May be returned by any node in the payment route.
type FailTemporaryNodeFailure struct{}

// Code returns the failure unique code.
// NOTE: Part of the FailureMessage interface.
func (f FailTemporaryNodeFailure) Code() FailCode {
	return CodeTemporaryNodeFailure
}

// FailPermanentNodeFailure is returned if an otherwise unspecified permanent
// error occurs for the entire node.
//
// NOTE: May be returned by any node in the payment route.
type FailPermanentNodeFailure struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailPermanentNodeFailure) Code() FailCode {
	return CodePermanentNodeFailure
}

// FailRequiredNodeFeatureMissing is returned if a node has requirement
// advertised in its node_announcement features which were not present in the
// onion.
//
// NOTE: May be returned by any node in the payment route.
type FailRequiredNodeFeatureMissing struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailRequiredNodeFeatureMissing) Code() FailCode {
	return CodeRequiredNodeFeatureMissing
}

// FailPermanentChannelFailure is return if an otherwise unspecified permanent
// error occurs for the outgoing channel (eg. channel (recently).
//
// NOTE: May be returned by any node in the payment route.
type FailPermanentChannelFailure struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailPermanentChannelFailure) Code() FailCode {
	return CodePermanentChannelFailure
}

// FailRequiredChannelFeatureMissing is returned if the outgoing channel has a
// requirement advertised in its channel announcement features which were not
// present in the onion.
//
// NOTE: May only be returned by intermediate nodes.
type FailRequiredChannelFeatureMissing struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailRequiredChannelFeatureMissing) Code() FailCode {
	return CodeRequiredChannelFeatureMissing
}

// FailUnknownNextPeer is returned if the next peer specified by the onion is
// not known.
//
// NOTE: May only be returned by intermediate nodes.
type FailUnknownNextPeer struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailUnknownNextPeer) Code() FailCode {
	return CodeUnknownNextPeer
}

// FailUnknownPaymentHash is returned If the payment hash has already been
// paid, the final node MAY treat the payment hash as unknown, or may succeed
// in accepting the HTLC. If the payment hash is unknown, the final node MUST
// fail the HTLC.
//
// NOTE: May only be returned by the final node in the path.
type FailUnknownPaymentHash struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailUnknownPaymentHash) Code() FailCode {
	return CodeUnknownPaymentHash
}

// FailIncorrectPaymentAmount is returned if the amount paid is less than the
// amount expected, the final node MUST fail the HTLC. If the amount paid is
// more than twice the amount expected, the final node SHOULD fail the HTLC.
// This allows the sender to reduce information leakage by altering the amount,
// without allowing accidental gross overpayment.
//
// NOTE: May only be returned by the final node in the path.
type FailIncorrectPaymentAmount struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailIncorrectPaymentAmount) Code() FailCode {
	return CodeIncorrectPaymentAmount
}

// FailFinalExpiryTooSoon is returned if the cltv_expiry is too low, the final
// node MUST fail the HTLC.
//
// NOTE: May only be returned by the final node in the path.
type FailFinalExpiryTooSoon struct{}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f FailFinalExpiryTooSoon) Code() FailCode {
	return CodeFinalExpiryTooSoon
}

// FailInvalidOnionVersion is returned if the onion version byte is unknown.
//
// NOTE: May be returned only by intermediate nodes.
type FailInvalidOnionVersion struct {
	// OnionSHA256 hash of the onion blob which haven't been proceeded.
	OnionSHA256 [sha256.Size]byte
}

// NewInvalidOnionVersion creates new instance of the FailInvalidOnionVersion.
func NewInvalidOnionVersion(onion []byte) *FailInvalidOnionVersion {
	return &FailInvalidOnionVersion{OnionSHA256: sha256.Sum256(onion)}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailInvalidOnionVersion) Code() FailCode {
	return CodeInvalidOnionVersion
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailInvalidOnionVersion) Decode(r io.Reader, pver uint32) error {
	return readElement(r, f.OnionSHA256[:])
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailInvalidOnionVersion) Encode(w io.Writer, pver uint32) error {
	return writeElement(w, f.OnionSHA256[:])
}

// FailInvalidOnionHmac is return if the onion HMAC is incorrect.
//
// NOTE: May only be returned by intermediate nodes.
type FailInvalidOnionHmac struct {
	// OnionSHA256 hash of the onion blob which haven't been proceeded.
	OnionSHA256 [sha256.Size]byte
}

// NewInvalidOnionHmac creates new instance of the FailInvalidOnionHmac.
func NewInvalidOnionHmac(onion []byte) *FailInvalidOnionHmac {
	return &FailInvalidOnionHmac{OnionSHA256: sha256.Sum256(onion)}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailInvalidOnionHmac) Code() FailCode {
	return CodeInvalidOnionHmac
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailInvalidOnionHmac) Decode(r io.Reader, pver uint32) error {
	return readElement(r, f.OnionSHA256[:])
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailInvalidOnionHmac) Encode(w io.Writer, pver uint32) error {
	return writeElement(w, f.OnionSHA256[:])
}

// FailInvalidOnionKey is return if the ephemeral key in the onion is
// unparsable.
//
// NOTE: May only be returned by intermediate nodes.
type FailInvalidOnionKey struct {
	// OnionSHA256 hash of the onion blob which haven't been proceeded.
	OnionSHA256 [sha256.Size]byte
}

// NewInvalidOnionKey creates new instance of the FailInvalidOnionKey.
func NewInvalidOnionKey(onion []byte) *FailInvalidOnionKey {
	return &FailInvalidOnionKey{OnionSHA256: sha256.Sum256(onion)}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailInvalidOnionKey) Code() FailCode {
	return CodeInvalidOnionKey
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailInvalidOnionKey) Decode(r io.Reader, pver uint32) error {
	return readElement(r, f.OnionSHA256[:])
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailInvalidOnionKey) Encode(w io.Writer, pver uint32) error {
	return writeElement(w, f.OnionSHA256[:])
}

// FailTemporaryChannelFailure is if an otherwise unspecified transient error
// occurs for the outgoing channel (eg. channel capacity reached, too many
// in-flight htlcs)
//
// NOTE: May only be returned by intermediate nodes.
type FailTemporaryChannelFailure struct {
	// Update is used to update information about state of the channel
	// which caused the failure.
	//
	// NOTE: This field is optional.
	Update *ChannelUpdate
}

// NewTemporaryChannelFailure creates new instance of the FailTemporaryChannelFailure.
func NewTemporaryChannelFailure(update *ChannelUpdate) *FailTemporaryChannelFailure {
	return &FailTemporaryChannelFailure{Update: update}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailTemporaryChannelFailure) Code() FailCode {
	return CodeTemporaryChannelFailure
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailTemporaryChannelFailure) Decode(r io.Reader, pver uint32) error {
	var length uint16
	err := readElement(r, &length)
	if err != nil {
		return err
	}

	if length != 0 {
		f.Update = &ChannelUpdate{}
		return f.Update.Decode(r, pver)
	}
	return nil
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailTemporaryChannelFailure) Encode(w io.Writer, pver uint32) error {
	var payload []byte
	if f.Update != nil {
		var bw bytes.Buffer
		if err := f.Update.Encode(&bw, pver); err != nil {
			return err
		}
		payload = bw.Bytes()
	}

	if err := writeElement(w, uint16(len(payload))); err != nil {
		return err
	}

	_, err := w.Write(payload)
	return err
}

// FailAmountBelowMinimum is returned if the HTLC does not reach the current
// minimum amount, we tell them the amount of the incoming HTLC and the current
// channel setting for the outgoing channel.
//
// NOTE: May only be returned by the intermediate nodes in the path.
type FailAmountBelowMinimum struct {
	// HtlcMsat is the wrong amount of the incoming HTLC.
	HtlcMsat MilliSatoshi

	// Update is used to update information about state of the channel
	// which caused the failure.
	Update ChannelUpdate
}

// NewAmountBelowMinimum creates new instance of the FailAmountBelowMinimum.
func NewAmountBelowMinimum(htlcMsat MilliSatoshi,
	update ChannelUpdate) *FailAmountBelowMinimum {

	return &FailAmountBelowMinimum{
		HtlcMsat: htlcMsat,
		Update:   update,
	}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailAmountBelowMinimum) Code() FailCode {
	return CodeAmountBelowMinimum
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailAmountBelowMinimum) Decode(r io.Reader, pver uint32) error {
	if err := readElement(r, &f.HtlcMsat); err != nil {
		return err
	}

	var length uint16
	if err := readElement(r, &length); err != nil {
		return err
	}

	f.Update = ChannelUpdate{}
	return f.Update.Decode(r, pver)
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailAmountBelowMinimum) Encode(w io.Writer, pver uint32) error {
	if err := writeElement(w, f.HtlcMsat); err != nil {
		return err
	}

	err := writeElement(w, uint16(f.Update.MaxPayloadLength(pver)))
	if err != nil {
		return err
	}

	return f.Update.Encode(w, pver)
}

// FailFeeInsufficient is returned if the HTLC does not pay sufficient fee, we
// tell them the amount of the incoming HTLC and the current channel setting
// for the outgoing channel.
//
// NOTE: May only be returned by intermediate nodes.
type FailFeeInsufficient struct {
	// HtlcMsat is the wrong amount of the incoming HTLC.
	HtlcMsat MilliSatoshi

	// Update is used to update information about state of the channel
	// which caused the failure.
	Update ChannelUpdate
}

// NewFeeInsufficient creates new instance of the FailFeeInsufficient.
func NewFeeInsufficient(htlcMsat MilliSatoshi,
	update ChannelUpdate) *FailFeeInsufficient {
	return &FailFeeInsufficient{
		HtlcMsat: htlcMsat,
		Update:   update,
	}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailFeeInsufficient) Code() FailCode {
	return CodeFeeInsufficient
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailFeeInsufficient) Decode(r io.Reader, pver uint32) error {
	if err := readElement(r, &f.HtlcMsat); err != nil {
		return err
	}

	var length uint16
	if err := readElement(r, &length); err != nil {
		return err
	}

	f.Update = ChannelUpdate{}
	return f.Update.Decode(r, pver)
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailFeeInsufficient) Encode(w io.Writer, pver uint32) error {
	if err := writeElement(w, f.HtlcMsat); err != nil {
		return err
	}

	err := writeElement(w, uint16(f.Update.MaxPayloadLength(pver)))
	if err != nil {
		return err
	}

	return f.Update.Encode(w, pver)
}

// FailIncorrectCltvExpiry is returned if outgoing cltv value does not match
// the update add htlc's cltv expiry minus cltv expiry delta for the outgoing
// channel, we tell them the cltv expiry and the current channel setting for
// the outgoing channel.
//
// NOTE: May only be returned by intermediate nodes.
type FailIncorrectCltvExpiry struct {
	// CltvExpiry is the wrong absolute timeout in blocks, after which
	// outgoing HTLC expires.
	CltvExpiry uint32

	// Update is used to update information about state of the channel
	// which caused the failure.
	Update ChannelUpdate
}

// NewIncorrectCltvExpiry creates new instance of the FailIncorrectCltvExpiry.
func NewIncorrectCltvExpiry(cltvExpiry uint32,
	update ChannelUpdate) *FailIncorrectCltvExpiry {

	return &FailIncorrectCltvExpiry{
		CltvExpiry: cltvExpiry,
		Update:     update,
	}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailIncorrectCltvExpiry) Code() FailCode {
	return CodeIncorrectCltvExpiry
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailIncorrectCltvExpiry) Decode(r io.Reader, pver uint32) error {
	if err := readElement(r, &f.CltvExpiry); err != nil {
		return err
	}

	var length uint16
	if err := readElement(r, &length); err != nil {
		return err
	}

	f.Update = ChannelUpdate{}
	return f.Update.Decode(r, pver)
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailIncorrectCltvExpiry) Encode(w io.Writer, pver uint32) error {
	if err := writeElement(w, f.CltvExpiry); err != nil {
		return err
	}

	err := writeElement(w, uint16(f.Update.MaxPayloadLength(pver)))
	if err != nil {
		return err
	}

	return f.Update.Encode(w, pver)
}

// FailExpiryTooSoon is returned if the ctlv-expiry is too near, we tell them
// the current channel setting for the outgoing channel.
//
// NOTE: May only be returned by intermediate nodes.
type FailExpiryTooSoon struct {
	// Update is used to update information about state of the channel
	// which caused the failure.
	Update ChannelUpdate
}

// NewExpiryTooSoon creates new instance of the FailExpiryTooSoon.
func NewExpiryTooSoon(update ChannelUpdate) *FailExpiryTooSoon {
	return &FailExpiryTooSoon{
		Update: update,
	}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailExpiryTooSoon) Code() FailCode {
	return CodeExpiryTooSoon
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailExpiryTooSoon) Decode(r io.Reader, pver uint32) error {
	var length uint16
	if err := readElement(r, &length); err != nil {
		return err
	}

	f.Update = ChannelUpdate{}
	return f.Update.Decode(r, pver)
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailExpiryTooSoon) Encode(w io.Writer, pver uint32) error {
	err := writeElement(w, uint16(f.Update.MaxPayloadLength(pver)))
	if err != nil {
		return err
	}

	return f.Update.Encode(w, pver)
}

// FailChannelDisabled is returned if the channel is disabled, we tell them the
// current channel setting for the outgoing channel.
//
// NOTE: May only be returned by intermediate nodes.
type FailChannelDisabled struct {
	// Flags least-significant bit must be set to 0 if the creating node
	// corresponds to the first node in the previously sent channel
	// announcement and 1 otherwise.
	Flags uint16

	// Update is used to update information about state of the channel
	// which caused the failure.
	Update ChannelUpdate
}

// NewChannelDisabled creates new instance of the FailChannelDisabled.
func NewChannelDisabled(flags uint16, update ChannelUpdate) *FailChannelDisabled {
	return &FailChannelDisabled{
		Flags:  flags,
		Update: update,
	}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailChannelDisabled) Code() FailCode {
	return CodeChannelDisabled
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailChannelDisabled) Decode(r io.Reader, pver uint32) error {
	if err := readElement(r, &f.Flags); err != nil {
		return err
	}

	var length uint16
	if err := readElement(r, &length); err != nil {
		return err
	}

	f.Update = ChannelUpdate{}
	return f.Update.Decode(r, pver)
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailChannelDisabled) Encode(w io.Writer, pver uint32) error {
	if err := writeElement(w, f.Flags); err != nil {
		return err
	}

	err := writeElement(w, uint16(f.Update.MaxPayloadLength(pver)))
	if err != nil {
		return err
	}

	return f.Update.Encode(w, pver)
}

// FailFinalIncorrectCltvExpiry is returned if the outgoing_cltv_value does not
// match the ctlv_expiry of the HTLC at the final hop.
//
// NOTE: might be returned by final node only.
type FailFinalIncorrectCltvExpiry struct {
	// CltvExpiry is the wrong absolute timeout in blocks, after which
	// outgoing HTLC expires.
	CltvExpiry uint32
}

// NewFinalIncorrectCltvExpiry creates new instance of the
// FailFinalIncorrectCltvExpiry.
func NewFinalIncorrectCltvExpiry(cltvExpiry uint32) *FailFinalIncorrectCltvExpiry {
	return &FailFinalIncorrectCltvExpiry{
		CltvExpiry: cltvExpiry,
	}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailFinalIncorrectCltvExpiry) Code() FailCode {
	return CodeFinalIncorrectCltvExpiry
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailFinalIncorrectCltvExpiry) Decode(r io.Reader, pver uint32) error {
	return readElement(r, &f.CltvExpiry)
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailFinalIncorrectCltvExpiry) Encode(w io.Writer, pver uint32) error {
	return writeElement(w, f.CltvExpiry)
}

// FailFinalIncorrectHtlcAmount is returned if the amt_to_forward is higher
// than incoming_htlc_amt of the HTLC at the final hop.
//
// NOTE: May only be returned by the final node.
type FailFinalIncorrectHtlcAmount struct {
	// IncomingHTLCAmount is the wrong forwarded htlc amount.
	IncomingHTLCAmount MilliSatoshi
}

// NewFinalIncorrectHtlcAmount creates new instance of the
// FailFinalIncorrectHtlcAmount.
func NewFinalIncorrectHtlcAmount(amount MilliSatoshi) *FailFinalIncorrectHtlcAmount {
	return &FailFinalIncorrectHtlcAmount{
		IncomingHTLCAmount: amount,
	}
}

// Code returns the failure unique code.
//
// NOTE: Part of the FailureMessage interface.
func (f *FailFinalIncorrectHtlcAmount) Code() FailCode {
	return CodeFinalIncorrectHtlcAmount
}

// Decode decodes the failure from bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailFinalIncorrectHtlcAmount) Decode(r io.Reader, pver uint32) error {
	return readElement(r, &f.IncomingHTLCAmount)
}

// Encode writes the failure in bytes stream.
//
// NOTE: Part of the Serializable interface.
func (f *FailFinalIncorrectHtlcAmount) Encode(w io.Writer, pver uint32) error {
	return writeElement(w, f.IncomingHTLCAmount)
}

// DecodeFailure decodes, validates, and parses the lnwire onion failure, for
// the provided protocol version.
func DecodeFailure(r io.Reader, pver uint32) (FailureMessage, error) {
	// First, we'll parse out the encapsulated failure message itself. This
	// is a 2 byte length followed by the payload itself.
	var failureLength uint16
	if err := readElement(r, &failureLength); err != nil {
		return nil, fmt.Errorf("unable to read error len: %v", err)
	}
	if failureLength > failureMessageLength {
		return nil, fmt.Errorf("failure message is too "+
			"long: %v", failureLength)
	}
	failureData := make([]byte, failureLength)
	if _, err := io.ReadFull(r, failureData); err != nil {
		return nil, fmt.Errorf("unable to full read payload of "+
			"%v: %v", failureLength, err)
	}

	dataReader := bytes.NewReader(failureData)

	// Once we have the failure data, we can obtain the failure code from
	// the first two bytes of the buffer.
	var codeBytes [2]byte
	if _, err := io.ReadFull(dataReader, codeBytes[:]); err != nil {
		return nil, fmt.Errorf("unable to read failure code: %v", err)
	}
	failCode := FailCode(binary.BigEndian.Uint16(codeBytes[:]))

	// Create the empty failure by given code and populate the failure with
	// additional data if needed.
	failure, err := makeEmptyOnionError(failCode)
	if err != nil {
		return nil, fmt.Errorf("unable to make empty error: %v", err)
	}

	// Finally, if this failure has a payload, then we'll read that now as
	// well.
	switch f := failure.(type) {
	case Serializable:
		if err := f.Decode(dataReader, pver); err != nil {
			return nil, fmt.Errorf("unable to decode error "+
				"update: %v", err)
		}
	}

	return failure, nil
}

// EncodeFailure encodes, including the necessary onion failure header
// information.
func EncodeFailure(w io.Writer, failure FailureMessage, pver uint32) error {
	var failureMessageBuffer bytes.Buffer

	// First, we'll write out the error code itself into the failure
	// buffer.
	var codeBytes [2]byte
	code := uint16(failure.Code())
	binary.BigEndian.PutUint16(codeBytes[:], code)
	_, err := failureMessageBuffer.Write(codeBytes[:])
	if err != nil {
		return err
	}

	// Next, some message have an additional message payload, if this is
	// one of those types, then we'll also encode the error payload as
	// well.
	switch failure := failure.(type) {
	case Serializable:
		if err := failure.Encode(&failureMessageBuffer, pver); err != nil {
			return err
		}
	}

	// The combined size of this message must be below the max allowed
	// failure message length.
	failureMessage := failureMessageBuffer.Bytes()
	if len(failureMessage) > failureMessageLength {
		return fmt.Errorf("failure message exceed max "+
			"available size: %v", len(failureMessage))
	}

	// Finally, we'll add some padding in order to ensure that all failure
	// messages are fixed size.
	pad := make([]byte, failureMessageLength-len(failureMessage))

	return writeElements(w,
		uint16(len(failureMessage)),
		failureMessage,
		uint16(len(pad)),
		pad,
	)
}

// makeEmptyOnionError creates a new empty onion error  of the proper concrete
// type based on the passed failure code.
func makeEmptyOnionError(code FailCode) (FailureMessage, error) {
	switch code {
	case CodeInvalidRealm:
		return &FailInvalidRealm{}, nil

	case CodeTemporaryNodeFailure:
		return &FailTemporaryNodeFailure{}, nil

	case CodePermanentNodeFailure:
		return &FailPermanentNodeFailure{}, nil

	case CodeRequiredNodeFeatureMissing:
		return &FailRequiredNodeFeatureMissing{}, nil

	case CodePermanentChannelFailure:
		return &FailPermanentChannelFailure{}, nil

	case CodeRequiredChannelFeatureMissing:
		return &FailRequiredChannelFeatureMissing{}, nil

	case CodeUnknownNextPeer:
		return &FailUnknownNextPeer{}, nil

	case CodeUnknownPaymentHash:
		return &FailUnknownPaymentHash{}, nil

	case CodeIncorrectPaymentAmount:
		return &FailIncorrectPaymentAmount{}, nil

	case CodeFinalExpiryTooSoon:
		return &FailFinalExpiryTooSoon{}, nil

	case CodeInvalidOnionVersion:
		return &FailInvalidOnionVersion{}, nil

	case CodeInvalidOnionHmac:
		return &FailInvalidOnionHmac{}, nil

	case CodeInvalidOnionKey:
		return &FailInvalidOnionKey{}, nil

	case CodeTemporaryChannelFailure:
		return &FailTemporaryChannelFailure{}, nil

	case CodeAmountBelowMinimum:
		return &FailAmountBelowMinimum{}, nil

	case CodeFeeInsufficient:
		return &FailFeeInsufficient{}, nil

	case CodeIncorrectCltvExpiry:
		return &FailIncorrectCltvExpiry{}, nil

	case CodeExpiryTooSoon:
		return &FailExpiryTooSoon{}, nil

	case CodeChannelDisabled:
		return &FailChannelDisabled{}, nil

	case CodeFinalIncorrectCltvExpiry:
		return &FailFinalIncorrectCltvExpiry{}, nil

	case CodeFinalIncorrectHtlcAmount:
		return &FailFinalIncorrectHtlcAmount{}, nil
	default:
		return nil, errors.Errorf("unknown error code: %v", code)
	}
}

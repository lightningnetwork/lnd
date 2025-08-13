package lnwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrFeaturePairExists signals an error in feature vector construction
	// where the opposing bit in a feature pair has already been set.
	ErrFeaturePairExists = errors.New("feature pair exists")

	// ErrFeatureStandard is returned when attempts to modify LND's known
	// set of features are made.
	ErrFeatureStandard = errors.New("feature is used in standard " +
		"protocol set")

	// ErrFeatureBitMaximum is returned when a feature bit exceeds the
	// maximum allowable value.
	ErrFeatureBitMaximum = errors.New("feature bit exceeds allowed maximum")
)

// FeatureBit represents a feature that can be enabled in either a local or
// global feature vector at a specific bit position. Feature bits follow the
// "it's OK to be odd" rule, where features at even bit positions must be known
// to a node receiving them from a peer while odd bits do not. In accordance,
// feature bits are usually assigned in pairs, first being assigned an odd bit
// position which may later be changed to the preceding even position once
// knowledge of the feature becomes required on the network.
type FeatureBit uint16

const (
	// DataLossProtectRequired is a feature bit that indicates that a peer
	// *requires* the other party know about the data-loss-protect optional
	// feature. If the remote peer does not know of such a feature, then
	// the sending peer SHOULD disconnect them. The data-loss-protect
	// feature allows a peer that's lost partial data to recover their
	// settled funds of the latest commitment state.
	DataLossProtectRequired FeatureBit = 0

	// DataLossProtectOptional is an optional feature bit that indicates
	// that the sending peer knows of this new feature and can activate it
	// it. The data-loss-protect feature allows a peer that's lost partial
	// data to recover their settled funds of the latest commitment state.
	DataLossProtectOptional FeatureBit = 1

	// InitialRoutingSync is a local feature bit meaning that the receiving
	// node should send a complete dump of routing information when a new
	// connection is established.
	InitialRoutingSync FeatureBit = 3

	// UpfrontShutdownScriptRequired is a feature bit which indicates that a
	// peer *requires* that the remote peer accept an upfront shutdown script to
	// which payout is enforced on cooperative closes.
	UpfrontShutdownScriptRequired FeatureBit = 4

	// UpfrontShutdownScriptOptional is an optional feature bit which indicates
	// that the peer will accept an upfront shutdown script to which payout is
	// enforced on cooperative closes.
	UpfrontShutdownScriptOptional FeatureBit = 5

	// GossipQueriesRequired is a feature bit that indicates that the
	// receiving peer MUST know of the set of features that allows nodes to
	// more efficiently query the network view of peers on the network for
	// reconciliation purposes.
	GossipQueriesRequired FeatureBit = 6

	// GossipQueriesOptional is an optional feature bit that signals that
	// the setting peer knows of the set of features that allows more
	// efficient network view reconciliation.
	GossipQueriesOptional FeatureBit = 7

	// TLVOnionPayloadRequired is a feature bit that indicates a node is
	// able to decode the new TLV information included in the onion packet.
	TLVOnionPayloadRequired FeatureBit = 8

	// TLVOnionPayloadOptional is an optional feature bit that indicates a
	// node is able to decode the new TLV information included in the onion
	// packet.
	TLVOnionPayloadOptional FeatureBit = 9

	// StaticRemoteKeyRequired is a required feature bit that signals that
	// within one's commitment transaction, the key used for the remote
	// party's non-delay output should not be tweaked.
	StaticRemoteKeyRequired FeatureBit = 12

	// StaticRemoteKeyOptional is an optional feature bit that signals that
	// within one's commitment transaction, the key used for the remote
	// party's non-delay output should not be tweaked.
	StaticRemoteKeyOptional FeatureBit = 13

	// PaymentAddrRequired is a required feature bit that signals that a
	// node requires payment addresses, which are used to mitigate probing
	// attacks on the receiver of a payment.
	PaymentAddrRequired FeatureBit = 14

	// PaymentAddrOptional is an optional feature bit that signals that a
	// node supports payment addresses, which are used to mitigate probing
	// attacks on the receiver of a payment.
	PaymentAddrOptional FeatureBit = 15

	// MPPRequired is a required feature bit that signals that the receiver
	// of a payment requires settlement of an invoice with more than one
	// HTLC.
	MPPRequired FeatureBit = 16

	// MPPOptional is an optional feature bit that signals that the receiver
	// of a payment supports settlement of an invoice with more than one
	// HTLC.
	MPPOptional FeatureBit = 17

	// WumboChannelsRequired is a required feature bit that signals that a
	// node is willing to accept channels larger than 2^24 satoshis.
	WumboChannelsRequired FeatureBit = 18

	// WumboChannelsOptional is an optional feature bit that signals that a
	// node is willing to accept channels larger than 2^24 satoshis.
	WumboChannelsOptional FeatureBit = 19

	// AnchorsRequired is a required feature bit that signals that the node
	// requires channels to be made using commitments having anchor
	// outputs.
	AnchorsRequired FeatureBit = 20

	// AnchorsOptional is an optional feature bit that signals that the
	// node supports channels to be made using commitments having anchor
	// outputs.
	AnchorsOptional FeatureBit = 21

	// AnchorsZeroFeeHtlcTxRequired is a required feature bit that signals
	// that the node requires channels having zero-fee second-level HTLC
	// transactions, which also imply anchor commitments.
	AnchorsZeroFeeHtlcTxRequired FeatureBit = 22

	// AnchorsZeroFeeHtlcTxOptional is an optional feature bit that signals
	// that the node supports channels having zero-fee second-level HTLC
	// transactions, which also imply anchor commitments.
	AnchorsZeroFeeHtlcTxOptional FeatureBit = 23

	// RouteBlindingRequired is a required feature bit that signals that
	// the node supports blinded payments.
	RouteBlindingRequired FeatureBit = 24

	// RouteBlindingOptional is an optional feature bit that signals that
	// the node supports blinded payments.
	RouteBlindingOptional FeatureBit = 25

	// ShutdownAnySegwitRequired is an required feature bit that signals
	// that the sender is able to properly handle/parse segwit witness
	// programs up to version 16. This enables utilization of Taproot
	// addresses for cooperative closure addresses.
	ShutdownAnySegwitRequired FeatureBit = 26

	// ShutdownAnySegwitOptional is an optional feature bit that signals
	// that the sender is able to properly handle/parse segwit witness
	// programs up to version 16. This enables utilization of Taproot
	// addresses for cooperative closure addresses.
	ShutdownAnySegwitOptional FeatureBit = 27

	// AMPRequired is a required feature bit that signals that the receiver
	// of a payment supports accepts spontaneous payments, i.e.
	// sender-generated preimages according to BOLT XX.
	AMPRequired FeatureBit = 30

	// AMPOptional is an optional feature bit that signals that the receiver
	// of a payment supports accepts spontaneous payments, i.e.
	// sender-generated preimages according to BOLT XX.
	AMPOptional FeatureBit = 31

	// QuiescenceRequired is a required feature bit that denotes that a
	// connection established with this node must support the quiescence
	// protocol if it wants to have a channel relationship.
	QuiescenceRequired FeatureBit = 34

	// QuiescenceOptional is an optional feature bit that denotes that a
	// connection established with this node is permitted to use the
	// quiescence protocol.
	QuiescenceOptional FeatureBit = 35

	// ExplicitChannelTypeRequired is a required bit that denotes that a
	// connection established with this node is to use explicit channel
	// commitment types for negotiation instead of the existing implicit
	// negotiation methods. With this bit, there is no longer a "default"
	// implicit channel commitment type, allowing a connection to
	// open/maintain types of several channels over its lifetime.
	ExplicitChannelTypeRequired = 44

	// ExplicitChannelTypeOptional is an optional bit that denotes that a
	// connection established with this node is to use explicit channel
	// commitment types for negotiation instead of the existing implicit
	// negotiation methods. With this bit, there is no longer a "default"
	// implicit channel commitment type, allowing a connection to
	// TODO: Decide on actual feature bit value.
	ExplicitChannelTypeOptional = 45

	// ScidAliasRequired is a required feature bit that signals that the
	// node requires understanding of ShortChannelID aliases in the TLV
	// segment of the channel_ready message.
	ScidAliasRequired FeatureBit = 46

	// ScidAliasOptional is an optional feature bit that signals that the
	// node understands ShortChannelID aliases in the TLV segment of the
	// channel_ready message.
	ScidAliasOptional FeatureBit = 47

	// PaymentMetadataRequired is a required bit that denotes that if an
	// invoice contains metadata, it must be passed along with the payment
	// htlc(s).
	PaymentMetadataRequired = 48

	// PaymentMetadataOptional is an optional bit that denotes that if an
	// invoice contains metadata, it may be passed along with the payment
	// htlc(s).
	PaymentMetadataOptional = 49

	// ZeroConfRequired is a required feature bit that signals that the
	// node requires understanding of the zero-conf channel_type.
	ZeroConfRequired FeatureBit = 50

	// ZeroConfOptional is an optional feature bit that signals that the
	// node understands the zero-conf channel type.
	ZeroConfOptional FeatureBit = 51

	// KeysendRequired is a required bit that indicates that the node is
	// able and willing to accept keysend payments.
	KeysendRequired = 54

	// KeysendOptional is an optional bit that indicates that the node is
	// able and willing to accept keysend payments.
	KeysendOptional = 55

	// RbfCoopCloseRequired is a required feature bit that signals that
	// the new RBF-based co-op close protocol is supported.
	RbfCoopCloseRequired = 60

	// RbfCoopCloseOptional is an optional feature bit that signals that the
	// new RBF-based co-op close protocol is supported.
	RbfCoopCloseOptional = 61

	// RbfCoopCloseRequiredStaging is a required feature bit that signals
	// that the new RBF-based co-op close protocol is supported.
	RbfCoopCloseRequiredStaging = 160

	// RbfCoopCloseOptionalStaging is an optional feature bit that signals
	// that the new RBF-based co-op close protocol is supported.
	RbfCoopCloseOptionalStaging = 161

	// ScriptEnforcedLeaseRequired is a required feature bit that signals
	// that the node requires channels having zero-fee second-level HTLC
	// transactions, which also imply anchor commitments, along with an
	// additional CLTV constraint of a channel lease's expiration height
	// applied to all outputs that pay directly to the channel initiator.
	//
	// TODO: Decide on actual feature bit value.
	ScriptEnforcedLeaseRequired FeatureBit = 2022

	// ScriptEnforcedLeaseOptional is an optional feature bit that signals
	// that the node requires channels having zero-fee second-level HTLC
	// transactions, which also imply anchor commitments, along with an
	// additional CLTV constraint of a channel lease's expiration height
	// applied to all outputs that pay directly to the channel initiator.
	//
	// TODO: Decide on actual feature bit value.
	ScriptEnforcedLeaseOptional FeatureBit = 2023

	// SimpleTaprootChannelsRequiredFinal is a required bit that indicates
	// the node is able to create taproot-native channels. This is the
	// final feature bit to be used once the channel type is finalized.
	SimpleTaprootChannelsRequiredFinal = 80

	// SimpleTaprootChannelsOptionalFinal is an optional bit that indicates
	// the node is able to create taproot-native channels. This is the
	// final feature bit to be used once the channel type is finalized.
	SimpleTaprootChannelsOptionalFinal = 81

	// SimpleTaprootChannelsRequiredStaging is a required bit that indicates
	// the node is able to create taproot-native channels. This is a
	// feature bit used in the wild while the channel type is still being
	// finalized.
	SimpleTaprootChannelsRequiredStaging = 180

	// SimpleTaprootChannelsOptionalStaging is an optional bit that
	// indicates the node is able to create taproot-native channels. This
	// is a feature bit used in the wild while the channel type is still
	// being finalized.
	SimpleTaprootChannelsOptionalStaging = 181

	// ExperimentalEndorsementRequired is a required feature bit that
	// indicates that the node will relay experimental endorsement signals.
	ExperimentalEndorsementRequired FeatureBit = 260

	// ExperimentalEndorsementOptional is an optional feature bit that
	// indicates that the node will relay experimental endorsement signals.
	ExperimentalEndorsementOptional FeatureBit = 261

	// Bolt11BlindedPathsRequired is a required feature bit that indicates
	// that the node is able to understand the blinded path tagged field in
	// a BOLT 11 invoice.
	Bolt11BlindedPathsRequired = 262

	// Bolt11BlindedPathsOptional is an optional feature bit that indicates
	// that the node is able to understand the blinded path tagged field in
	// a BOLT 11 invoice.
	Bolt11BlindedPathsOptional = 263

	// SimpleTaprootOverlayChansRequired is a required bit that indicates
	// support for the special custom taproot overlay channel.
	SimpleTaprootOverlayChansOptional = 2025

	// SimpleTaprootOverlayChansRequired is a required bit that indicates
	// support for the special custom taproot overlay channel.
	SimpleTaprootOverlayChansRequired = 2026

	// MaxBolt11Feature is the maximum feature bit value allowed in bolt 11
	// invoices.
	//
	// The base 32 encoded tagged fields in invoices are limited to 10 bits
	// to express the length of the field's data.
	//nolint:ll
	// See: https://github.com/lightning/bolts/blob/master/11-payment-encoding.md#tagged-fields
	//
	// With a maximum length field of 1023 (2^10 -1) and 5 bit encoding,
	// the highest feature bit that can be expressed is:
	// 1023 * 5 - 1 = 5114.
	MaxBolt11Feature = 5114
)

// IsRequired returns true if the feature bit is even, and false otherwise.
func (b FeatureBit) IsRequired() bool {
	return b&0x01 == 0x00
}

// Features is a mapping of known feature bits to a descriptive name. All known
// feature bits must be assigned a name in this mapping, and feature bit pairs
// must be assigned together for correct behavior.
var Features = map[FeatureBit]string{
	DataLossProtectRequired:              "data-loss-protect",
	DataLossProtectOptional:              "data-loss-protect",
	InitialRoutingSync:                   "initial-routing-sync",
	UpfrontShutdownScriptRequired:        "upfront-shutdown-script",
	UpfrontShutdownScriptOptional:        "upfront-shutdown-script",
	GossipQueriesRequired:                "gossip-queries",
	GossipQueriesOptional:                "gossip-queries",
	TLVOnionPayloadRequired:              "tlv-onion",
	TLVOnionPayloadOptional:              "tlv-onion",
	StaticRemoteKeyOptional:              "static-remote-key",
	StaticRemoteKeyRequired:              "static-remote-key",
	PaymentAddrOptional:                  "payment-addr",
	PaymentAddrRequired:                  "payment-addr",
	MPPOptional:                          "multi-path-payments",
	MPPRequired:                          "multi-path-payments",
	AnchorsRequired:                      "anchor-commitments",
	AnchorsOptional:                      "anchor-commitments",
	AnchorsZeroFeeHtlcTxRequired:         "anchors-zero-fee-htlc-tx",
	AnchorsZeroFeeHtlcTxOptional:         "anchors-zero-fee-htlc-tx",
	WumboChannelsRequired:                "wumbo-channels",
	WumboChannelsOptional:                "wumbo-channels",
	AMPRequired:                          "amp",
	AMPOptional:                          "amp",
	QuiescenceRequired:                   "quiescence",
	QuiescenceOptional:                   "quiescence",
	PaymentMetadataOptional:              "payment-metadata",
	PaymentMetadataRequired:              "payment-metadata",
	ExplicitChannelTypeOptional:          "explicit-commitment-type",
	ExplicitChannelTypeRequired:          "explicit-commitment-type",
	KeysendOptional:                      "keysend",
	KeysendRequired:                      "keysend",
	ScriptEnforcedLeaseRequired:          "script-enforced-lease",
	ScriptEnforcedLeaseOptional:          "script-enforced-lease",
	ScidAliasRequired:                    "scid-alias",
	ScidAliasOptional:                    "scid-alias",
	ZeroConfRequired:                     "zero-conf",
	ZeroConfOptional:                     "zero-conf",
	RouteBlindingRequired:                "route-blinding",
	RouteBlindingOptional:                "route-blinding",
	ShutdownAnySegwitRequired:            "shutdown-any-segwit",
	ShutdownAnySegwitOptional:            "shutdown-any-segwit",
	SimpleTaprootChannelsRequiredFinal:   "simple-taproot-chans",
	SimpleTaprootChannelsOptionalFinal:   "simple-taproot-chans",
	SimpleTaprootChannelsRequiredStaging: "simple-taproot-chans-x",
	SimpleTaprootChannelsOptionalStaging: "simple-taproot-chans-x",
	SimpleTaprootOverlayChansOptional:    "taproot-overlay-chans",
	SimpleTaprootOverlayChansRequired:    "taproot-overlay-chans",
	ExperimentalEndorsementRequired:      "endorsement-x",
	ExperimentalEndorsementOptional:      "endorsement-x",
	Bolt11BlindedPathsOptional:           "bolt-11-blinded-paths",
	Bolt11BlindedPathsRequired:           "bolt-11-blinded-paths",
	RbfCoopCloseOptional:                 "rbf-coop-close",
	RbfCoopCloseRequired:                 "rbf-coop-close",
	RbfCoopCloseOptionalStaging:          "rbf-coop-close-x",
	RbfCoopCloseRequiredStaging:          "rbf-coop-close-x",
}

// RawFeatureVector represents a set of feature bits as defined in BOLT-09.  A
// RawFeatureVector itself just stores a set of bit flags but can be used to
// construct a FeatureVector which binds meaning to each bit. Feature vectors
// can be serialized and deserialized to/from a byte representation that is
// transmitted in Lightning network messages.
type RawFeatureVector struct {
	features map[FeatureBit]struct{}
}

// NewRawFeatureVector creates a feature vector with all of the feature bits
// given as arguments enabled.
func NewRawFeatureVector(bits ...FeatureBit) *RawFeatureVector {
	fv := &RawFeatureVector{features: make(map[FeatureBit]struct{})}
	for _, bit := range bits {
		fv.Set(bit)
	}
	return fv
}

// IsEmpty returns whether the feature vector contains any feature bits.
func (fv RawFeatureVector) IsEmpty() bool {
	return len(fv.features) == 0
}

// OnlyContains determines whether only the specified feature bits are found.
func (fv RawFeatureVector) OnlyContains(bits ...FeatureBit) bool {
	if len(bits) != len(fv.features) {
		return false
	}
	for _, bit := range bits {
		if !fv.IsSet(bit) {
			return false
		}
	}
	return true
}

// Equals determines whether two features vectors contain exactly the same
// features.
func (fv RawFeatureVector) Equals(other *RawFeatureVector) bool {
	if len(fv.features) != len(other.features) {
		return false
	}
	for bit := range fv.features {
		if _, ok := other.features[bit]; !ok {
			return false
		}
	}
	return true
}

// Merge sets all feature bits in other on the receiver's feature vector.
func (fv *RawFeatureVector) Merge(other *RawFeatureVector) error {
	for bit := range other.features {
		err := fv.SafeSet(bit)
		if err != nil {
			return err
		}
	}
	return nil
}

// ValidateUpdate checks whether a feature vector can safely be updated to the
// new feature vector provided, checking that it does not alter any of the
// "standard" features that are defined by LND. The new feature vector should
// be inclusive of all features in the original vector that it still wants to
// advertise, setting and unsetting updates as desired. Features in the vector
// are also checked against a maximum inclusive value, as feature vectors in
// different contexts have different maximum values.
func (fv *RawFeatureVector) ValidateUpdate(other *RawFeatureVector,
	maximumValue FeatureBit) error {

	// Run through the new set of features and check that we're not adding
	// any feature bits that are defined but not set in LND.
	for feature := range other.features {
		if fv.IsSet(feature) {
			continue
		}

		if feature > maximumValue {
			return fmt.Errorf("can't set feature bit %d: %w %v",
				feature, ErrFeatureBitMaximum,
				maximumValue)
		}

		if name, known := Features[feature]; known {
			return fmt.Errorf("can't set feature "+
				"bit %d (%v): %w", feature, name,
				ErrFeatureStandard)
		}
	}

	// Check that the new feature vector for this set does not unset any
	// features that are standard in LND by comparing the features in our
	// current set to the omitted values in the new set.
	for feature := range fv.features {
		if other.IsSet(feature) {
			continue
		}

		if name, known := Features[feature]; known {
			return fmt.Errorf("can't unset feature "+
				"bit %d (%v): %w", feature, name,
				ErrFeatureStandard)
		}
	}

	return nil
}

// ValidatePairs checks each feature bit in a raw vector to ensure that the
// opposing bit is not set, validating that the vector has either the optional
// or required bit set, not both.
func (fv *RawFeatureVector) ValidatePairs() error {
	for feature := range fv.features {
		if _, ok := fv.features[feature^1]; ok {
			return ErrFeaturePairExists
		}
	}

	return nil
}

// Clone makes a copy of a feature vector.
func (fv *RawFeatureVector) Clone() *RawFeatureVector {
	newFeatures := NewRawFeatureVector()
	for bit := range fv.features {
		newFeatures.Set(bit)
	}
	return newFeatures
}

// IsSet returns whether a particular feature bit is enabled in the vector.
func (fv *RawFeatureVector) IsSet(feature FeatureBit) bool {
	_, ok := fv.features[feature]
	return ok
}

// Set marks a feature as enabled in the vector.
func (fv *RawFeatureVector) Set(feature FeatureBit) {
	fv.features[feature] = struct{}{}
}

// SafeSet sets the chosen feature bit in the feature vector, but returns an
// error if the opposing feature bit is already set. This ensures both that we
// are creating properly structured feature vectors, and in some cases, that
// peers are sending properly encoded ones, i.e. it can't be both optional and
// required.
func (fv *RawFeatureVector) SafeSet(feature FeatureBit) error {
	if _, ok := fv.features[feature^1]; ok {
		return ErrFeaturePairExists
	}

	fv.Set(feature)
	return nil
}

// Unset marks a feature as disabled in the vector.
func (fv *RawFeatureVector) Unset(feature FeatureBit) {
	delete(fv.features, feature)
}

// SerializeSize returns the number of bytes needed to represent feature vector
// in byte format.
func (fv *RawFeatureVector) SerializeSize() int {
	// We calculate byte-length via the largest bit index.
	return fv.serializeSize(8)
}

// SerializeSize32 returns the number of bytes needed to represent feature
// vector in base32 format.
func (fv *RawFeatureVector) SerializeSize32() int {
	// We calculate base32-length via the largest bit index.
	return fv.serializeSize(5)
}

// serializeSize returns the number of bytes required to encode the feature
// vector using at most width bits per encoded byte.
func (fv *RawFeatureVector) serializeSize(width int) int {
	// Find the largest feature bit index
	max := -1
	for feature := range fv.features {
		index := int(feature)
		if index > max {
			max = index
		}
	}
	if max == -1 {
		return 0
	}

	return max/width + 1
}

// Encode writes the feature vector in byte representation. Every feature
// encoded as a bit, and the bit vector is serialized using the least number of
// bytes. Since the bit vector length is variable, the first two bytes of the
// serialization represent the length.
func (fv *RawFeatureVector) Encode(w io.Writer) error {
	// Write length of feature vector.
	var l [2]byte
	length := fv.SerializeSize()
	binary.BigEndian.PutUint16(l[:], uint16(length))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}

	return fv.encode(w, length, 8)
}

// EncodeBase256 writes the feature vector in base256 representation. Every
// feature is encoded as a bit, and the bit vector is serialized using the least
// number of bytes.
func (fv *RawFeatureVector) EncodeBase256(w io.Writer) error {
	length := fv.SerializeSize()
	return fv.encode(w, length, 8)
}

// EncodeBase32 writes the feature vector in base32 representation. Every feature
// is encoded as a bit, and the bit vector is serialized using the least number of
// bytes.
func (fv *RawFeatureVector) EncodeBase32(w io.Writer) error {
	length := fv.SerializeSize32()
	return fv.encode(w, length, 5)
}

// encode writes the feature vector
func (fv *RawFeatureVector) encode(w io.Writer, length, width int) error {
	// Generate the data and write it.
	data := make([]byte, length)
	for feature := range fv.features {
		byteIndex := int(feature) / width
		bitIndex := int(feature) % width
		data[length-byteIndex-1] |= 1 << uint(bitIndex)
	}

	_, err := w.Write(data)
	return err
}

// Decode reads the feature vector from its byte representation. Every feature
// is encoded as a bit, and the bit vector is serialized using the least number
// of bytes. Since the bit vector length is variable, the first two bytes of the
// serialization represent the length.
func (fv *RawFeatureVector) Decode(r io.Reader) error {
	// Read the length of the feature vector.
	var l [2]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint16(l[:])

	return fv.decode(r, int(length), 8)
}

// DecodeBase256 reads the feature vector from its base256 representation. Every
// feature encoded as a bit, and the bit vector is serialized using the least
// number of bytes.
func (fv *RawFeatureVector) DecodeBase256(r io.Reader, length int) error {
	return fv.decode(r, length, 8)
}

// DecodeBase32 reads the feature vector from its base32 representation. Every
// feature encoded as a bit, and the bit vector is serialized using the least
// number of bytes.
func (fv *RawFeatureVector) DecodeBase32(r io.Reader, length int) error {
	return fv.decode(r, length, 5)
}

// decode reads a feature vector from the next length bytes of the io.Reader,
// assuming each byte has width feature bits encoded per byte.
func (fv *RawFeatureVector) decode(r io.Reader, length, width int) error {
	// Read the feature vector data.
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}

	// Set feature bits from parsed data.
	bitsNumber := len(data) * width
	for i := 0; i < bitsNumber; i++ {
		byteIndex := int(i / width)
		bitIndex := uint(i % width)
		if (data[length-byteIndex-1]>>bitIndex)&1 == 1 {
			fv.Set(FeatureBit(i))
		}
	}

	return nil
}

// sizeFunc returns the length required to encode the feature vector.
func (fv *RawFeatureVector) sizeFunc() uint64 {
	return uint64(fv.SerializeSize())
}

// Record returns a TLV record that can be used to encode/decode raw feature
// vectors. Note that the length of the feature vector is not included, because
// it is covered by the TLV record's length field.
func (fv *RawFeatureVector) Record() tlv.Record {
	return tlv.MakeDynamicRecord(
		0, fv, fv.sizeFunc, rawFeatureEncoder, rawFeatureDecoder,
	)
}

// rawFeatureEncoder is a custom TLV encoder for raw feature vectors.
func rawFeatureEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	if v, ok := val.(*RawFeatureVector); ok {
		// Encode the feature bits as a byte slice without its length
		// prepended, as that's already taken care of by the TLV record.
		fv := *v
		return fv.encode(w, fv.SerializeSize(), 8)
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.RawFeatureVector")
}

// rawFeatureDecoder is a custom TLV decoder for raw feature vectors.
func rawFeatureDecoder(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	if v, ok := val.(*RawFeatureVector); ok {
		fv := NewRawFeatureVector()
		if err := fv.decode(r, int(l), 8); err != nil {
			return err
		}
		*v = *fv

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "lnwire.RawFeatureVector")
}

// FeatureVector represents a set of enabled features. The set stores
// information on enabled flags and metadata about the feature names. A feature
// vector is serializable to a compact byte representation that is included in
// Lightning network messages.
type FeatureVector struct {
	*RawFeatureVector
	featureNames map[FeatureBit]string
}

// NewFeatureVector constructs a new FeatureVector from a raw feature vector
// and mapping of feature definitions. If the feature vector argument is nil, a
// new one will be constructed with no enabled features.
func NewFeatureVector(featureVector *RawFeatureVector,
	featureNames map[FeatureBit]string) *FeatureVector {

	if featureVector == nil {
		featureVector = NewRawFeatureVector()
	}
	return &FeatureVector{
		RawFeatureVector: featureVector,
		featureNames:     featureNames,
	}
}

// EmptyFeatureVector returns a feature vector with no bits set.
func EmptyFeatureVector() *FeatureVector {
	return NewFeatureVector(nil, Features)
}

// Record implements the RecordProducer interface for FeatureVector. Note that
// it uses a zero-value type is used to produce the record, as we expect this
// type value to be overwritten when used in generic TLV record production.
// This allows a single Record function to serve in the many different contexts
// in which feature vectors are encoded. This record wraps the encoding/
// decoding for our raw feature vectors so that we can directly parse fully
// formed feature vector types.
func (fv *FeatureVector) Record() tlv.Record {
	return tlv.MakeDynamicRecord(0, fv, fv.sizeFunc,
		func(w io.Writer, val interface{}, buf *[8]byte) error {
			if f, ok := val.(*FeatureVector); ok {
				return rawFeatureEncoder(
					w, f.RawFeatureVector, buf,
				)
			}

			return tlv.NewTypeForEncodingErr(
				val, "*lnwire.FeatureVector",
			)
		},
		func(r io.Reader, val interface{}, buf *[8]byte,
			l uint64) error {

			if f, ok := val.(*FeatureVector); ok {
				features := NewFeatureVector(nil, Features)
				err := rawFeatureDecoder(
					r, features.RawFeatureVector, buf, l,
				)
				if err != nil {
					return err
				}

				*f = *features

				return nil
			}

			return tlv.NewTypeForDecodingErr(
				val, "*lnwire.FeatureVector", l, l,
			)
		},
	)
}

// HasFeature returns whether a particular feature is included in the set. The
// feature can be seen as set either if the bit is set directly OR the queried
// bit has the same meaning as its corresponding even/odd bit, which is set
// instead. The second case is because feature bits are generally assigned in
// pairs where both the even and odd position represent the same feature.
func (fv *FeatureVector) HasFeature(feature FeatureBit) bool {
	return fv.IsSet(feature) ||
		(fv.isFeatureBitPair(feature) && fv.IsSet(feature^1))
}

// RequiresFeature returns true if the referenced feature vector *requires*
// that the given required bit be set. This method can be used with both
// optional and required feature bits as a parameter.
func (fv *FeatureVector) RequiresFeature(feature FeatureBit) bool {
	// If we weren't passed a required feature bit, then we'll flip the
	// lowest bit to query for the required version of the feature. This
	// lets callers pass in both the optional and required bits.
	if !feature.IsRequired() {
		feature ^= 1
	}

	return fv.IsSet(feature)
}

// UnknownRequiredFeatures returns a list of feature bits set in the vector
// that are unknown and in an even bit position. Feature bits with an even
// index must be known to a node receiving the feature vector in a message.
func (fv *FeatureVector) UnknownRequiredFeatures() []FeatureBit {
	var unknown []FeatureBit
	for feature := range fv.features {
		if feature%2 == 0 && !fv.IsKnown(feature) {
			unknown = append(unknown, feature)
		}
	}
	return unknown
}

// UnknownFeatures returns a boolean if a feature vector contains *any*
// unknown features (even if they are odd).
func (fv *FeatureVector) UnknownFeatures() bool {
	for feature := range fv.features {
		if !fv.IsKnown(feature) {
			return true
		}
	}

	return false
}

// Name returns a string identifier for the feature represented by this bit. If
// the bit does not represent a known feature, this returns a string indicating
// as such.
func (fv *FeatureVector) Name(bit FeatureBit) string {
	name, known := fv.featureNames[bit]
	if !known {
		return "unknown"
	}
	return name
}

// IsKnown returns whether this feature bit represents a known feature.
func (fv *FeatureVector) IsKnown(bit FeatureBit) bool {
	_, known := fv.featureNames[bit]
	return known
}

// isFeatureBitPair returns whether this feature bit and its corresponding
// even/odd bit both represent the same feature. This may often be the case as
// bits are generally assigned in pairs, first being assigned an odd bit
// position then being promoted to an even bit position once the network is
// ready.
func (fv *FeatureVector) isFeatureBitPair(bit FeatureBit) bool {
	name1, known1 := fv.featureNames[bit]
	name2, known2 := fv.featureNames[bit^1]
	return known1 && known2 && name1 == name2
}

// Features returns the set of raw features contained in the feature vector.
func (fv *FeatureVector) Features() map[FeatureBit]struct{} {
	fs := make(map[FeatureBit]struct{}, len(fv.RawFeatureVector.features))
	for b := range fv.RawFeatureVector.features {
		fs[b] = struct{}{}
	}
	return fs
}

// Clone copies a feature vector, carrying over its feature bits. The feature
// names are not copied.
func (fv *FeatureVector) Clone() *FeatureVector {
	features := fv.RawFeatureVector.Clone()
	return NewFeatureVector(features, fv.featureNames)
}

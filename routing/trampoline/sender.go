package trampoline

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
)

var trampolineSessionDomain = []byte("lnd/trampoline/session/v1")

var (
	// ErrNoTrampolineHops is returned when an onion has no destination.
	ErrNoTrampolineHops = errors.New("trampoline path has no hops")

	// ErrTrampolineFeatureMissing is returned when a selected hop did not
	// advertise trampoline routing.
	ErrTrampolineFeatureMissing = errors.New(
		"trampoline routing feature missing",
	)

	// ErrTrampolinePaddingTooLarge is returned when requested padding
	// exceeds the normal payment-onion payload ceiling.
	ErrTrampolinePaddingTooLarge = errors.New(
		"trampoline onion padding too large",
	)
)

// Bolt11Hop contains the authenticated forwarding values for one trampoline
// onion hop.
type Bolt11Hop struct {
	NodeID        *btcec.PublicKey
	Features      *lnwire.FeatureVector
	AmountMsat    uint64
	OutgoingCLTV  uint32
	PaymentSecret *[32]byte
	Metadata      []byte
}

// Bolt11Request contains all inputs needed to construct a BOLT 11 trampoline
// onion independently of the ordinary outer onion.
type Bolt11Request struct {
	PaymentHash []byte
	Hops        []Bolt11Hop
	PaddingSize uint16
	SessionKey  *btcec.PrivateKey
}

// Bolt12Request contains the clear trampoline prefix and blinded path needed
// to construct a trampoline-capable BOLT 12 payment onion.
type Bolt12Request struct {
	PaymentHash []byte

	// ClearHops are optional trampoline nodes prepended before the blinded
	// path. PaymentSecret and Metadata are ignored.
	ClearHops []Bolt11Hop

	// IntroductionNode is the resolved public key for the blinded path's
	// introduction node. It is required for sciddir introduction nodes.
	IntroductionNode *btcec.PublicKey

	// BlindedPath contains the encrypted recipient data and blinded node
	// IDs supplied by the invoice.
	BlindedPath lnwire.BlindedPath

	// BlindedFeatures is the invoice feature vector that commits to
	// trampoline support for the blinded recipient path.
	BlindedFeatures *lnwire.FeatureVector

	AmountMsat   uint64
	OutgoingCLTV uint32
	TotalMsat    uint64
	PaddingSize  uint16
	SessionKey   *btcec.PrivateKey
}

// BuildBolt11Onion constructs and serializes a variable-sized trampoline
// onion. Every selected hop must advertise trampoline routing, and the final
// hop must carry the invoice payment secret.
func BuildBolt11Onion(request Bolt11Request) ([]byte, error) {
	if len(request.Hops) == 0 {
		return nil, ErrNoTrampolineHops
	}
	if len(request.Hops) > sphinx.NumMaxHops {
		return nil, fmt.Errorf("trampoline hop count %d exceeds %d",
			len(request.Hops), sphinx.NumMaxHops)
	}
	if len(request.PaymentHash) != 32 {
		return nil, fmt.Errorf("payment hash must be 32 bytes")
	}

	var path sphinx.PaymentPath
	for i := range request.Hops {
		hop := &request.Hops[i]
		if hop.NodeID == nil {
			return nil, fmt.Errorf(
				"trampoline hop %d has no node id", i,
			)
		}
		hasFeature := hop.Features != nil && hop.Features.HasFeature(
			lnwire.TrampolineRoutingOptional,
		)
		if !hasFeature {
			return nil, fmt.Errorf("%w at hop %d",
				ErrTrampolineFeatureMissing, i)
		}

		payload := &Payload{
			AmountToForward: &hop.AmountMsat,
			OutgoingCLTV:    &hop.OutgoingCLTV,
		}
		final := i == len(request.Hops)-1
		if final {
			if hop.PaymentSecret == nil {
				return nil, fmt.Errorf(
					"final trampoline hop has no " +
						"payment secret",
				)
			}
			payload.PaymentData = record.NewMPP(
				lnwire.MilliSatoshi(hop.AmountMsat),
				*hop.PaymentSecret,
			)
			payload.PaymentMetadata = append(
				[]byte(nil), hop.Metadata...,
			)
		} else {
			payload.OutgoingNodeID = request.Hops[i+1].NodeID
		}

		encoded, err := encodePayload(payload)
		if err != nil {
			return nil, fmt.Errorf(
				"encode trampoline hop %d: %w", i, err,
			)
		}
		hopPayload, err := sphinx.NewTLVHopPayload(encoded)
		if err != nil {
			return nil, fmt.Errorf(
				"create trampoline hop %d: %w", i, err,
			)
		}
		path[i] = sphinx.OnionHop{
			NodePub:    *hop.NodeID,
			HopPayload: hopPayload,
		}
	}

	return buildOnion(
		path, request.PaymentHash, request.PaddingSize,
		request.SessionKey,
	)
}

// BuildBolt12Onion constructs a trampoline onion that may contain clear
// trampoline nodes followed by every node in a BOLT 12 blinded path.
func BuildBolt12Onion(request Bolt12Request) ([]byte, error) {
	if len(request.PaymentHash) != 32 {
		return nil, fmt.Errorf("payment hash must be 32 bytes")
	}
	if request.IntroductionNode == nil {
		return nil, errors.New("blinded introduction node missing")
	}
	if request.BlindedPath.BlindingPoint == nil ||
		len(request.BlindedPath.Hops) == 0 {

		return nil, errors.New("blinded trampoline path is empty")
	}
	if request.BlindedFeatures == nil ||
		!request.BlindedFeatures.HasFeature(
			lnwire.TrampolineRoutingOptional,
		) {

		return nil, ErrTrampolineFeatureMissing
	}
	hopCount := len(request.ClearHops) + len(request.BlindedPath.Hops)
	if hopCount > sphinx.NumMaxHops {
		return nil, fmt.Errorf("trampoline hop count %d exceeds %d",
			hopCount, sphinx.NumMaxHops)
	}
	if request.TotalMsat < request.AmountMsat {
		return nil, errors.New(
			"blinded trampoline total is below final amount",
		)
	}

	var path sphinx.PaymentPath
	pathIndex := 0
	for i := range request.ClearHops {
		hop := &request.ClearHops[i]
		if hop.NodeID == nil {
			return nil, fmt.Errorf(
				"clear trampoline hop %d has no node id", i,
			)
		}
		if hop.Features == nil ||
			!hop.Features.HasFeature(
				lnwire.TrampolineRoutingOptional,
			) {

			return nil, fmt.Errorf(
				"%w at clear hop %d",
				ErrTrampolineFeatureMissing, i,
			)
		}

		nextNode := request.IntroductionNode
		if i+1 < len(request.ClearHops) {
			nextNode = request.ClearHops[i+1].NodeID
		}
		payload := &Payload{
			AmountToForward: &hop.AmountMsat,
			OutgoingCLTV:    &hop.OutgoingCLTV,
			OutgoingNodeID:  nextNode,
		}
		encoded, err := encodePayload(payload)
		if err != nil {
			return nil, fmt.Errorf(
				"encode clear trampoline hop %d: %w", i, err,
			)
		}
		hopPayload, err := sphinx.NewTLVHopPayload(encoded)
		if err != nil {
			return nil, fmt.Errorf(
				"create clear trampoline hop %d: %w", i, err,
			)
		}
		path[pathIndex] = sphinx.OnionHop{
			NodePub:    *hop.NodeID,
			HopPayload: hopPayload,
		}
		pathIndex++
	}

	lastBlinded := len(request.BlindedPath.Hops) - 1
	for i := range request.BlindedPath.Hops {
		hop := &request.BlindedPath.Hops[i]
		if hop.BlindedNodeID == nil ||
			len(hop.EncryptedData) == 0 {

			return nil, fmt.Errorf(
				"blinded trampoline hop %d is incomplete", i,
			)
		}
		payload := &Payload{
			EncryptedRecipientData: append(
				[]byte(nil), hop.EncryptedData...,
			),
		}
		if i == 0 {
			payload.CurrentPathKey =
				request.BlindedPath.BlindingPoint
		}
		if i == lastBlinded {
			payload.AmountToForward = &request.AmountMsat
			payload.OutgoingCLTV = &request.OutgoingCLTV
			payload.TotalAmountMsat = &request.TotalMsat
		}

		encoded, err := encodePayload(payload)
		if err != nil {
			return nil, fmt.Errorf(
				"encode blinded trampoline hop %d: %w", i,
				err,
			)
		}
		hopPayload, err := sphinx.NewTLVHopPayload(encoded)
		if err != nil {
			return nil, fmt.Errorf(
				"create blinded trampoline hop %d: %w", i,
				err,
			)
		}

		nodeID := hop.BlindedNodeID
		if i == 0 {
			nodeID = request.IntroductionNode
		}
		path[pathIndex] = sphinx.OnionHop{
			NodePub:    *nodeID,
			HopPayload: hopPayload,
		}
		pathIndex++
	}

	return buildOnion(
		path, request.PaymentHash, request.PaddingSize,
		request.SessionKey,
	)
}

func buildOnion(path sphinx.PaymentPath, paymentHash []byte,
	paddingSize uint16, sessionKey *btcec.PrivateKey) ([]byte, error) {

	payloadSize := path.TotalPayloadSize() + int(paddingSize)
	if payloadSize > sphinx.MaxRoutingPayloadSize {
		return nil, fmt.Errorf("%w: size=%d max=%d",
			ErrTrampolinePaddingTooLarge, payloadSize,
			sphinx.MaxRoutingPayloadSize)
	}

	if sessionKey == nil {
		var err error
		sessionKey, err = btcec.NewPrivateKey()
		if err != nil {
			return nil, fmt.Errorf(
				"create trampoline session key: %w", err,
			)
		}
	}

	packet, err := sphinx.NewOnionPacket(
		&path, sessionKey, paymentHash, sphinx.RandPacketFiller,
		sphinx.WithMaxPayloadSize(payloadSize),
	)
	if err != nil {
		return nil, fmt.Errorf("create trampoline onion: %w", err)
	}

	return EncodeOnionPacket(packet)
}

// DeriveSessionKey derives an independent trampoline onion session key from
// the persisted ordinary payment attempt key. Domain separation avoids Sphinx
// stream reuse while making error decryption recoverable after restart.
func DeriveSessionKey(
	outerSessionKey *btcec.PrivateKey) (*btcec.PrivateKey, error) {

	if outerSessionKey == nil {
		return nil, errors.New("outer payment session key is nil")
	}

	key := outerSessionKey.Serialize()
	for counter := byte(0); ; counter++ {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write(trampolineSessionDomain)
		_, _ = mac.Write([]byte{counter})

		var scalar btcec.ModNScalar
		overflow := scalar.SetByteSlice(mac.Sum(nil))
		if !overflow && !scalar.IsZero() {
			return btcec.PrivKeyFromScalar(&scalar), nil
		}
		if counter == ^byte(0) {
			return nil, errors.New(
				"unable to derive trampoline session key",
			)
		}
	}
}

func encodePayload(payload *Payload) ([]byte, error) {
	var buffer bytes.Buffer
	if err := payload.Encode(&buffer); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

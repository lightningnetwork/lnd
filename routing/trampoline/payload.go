package trampoline

import (
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// OutgoingNodeIDType is the BOLT 4 TLV type that identifies the next
	// trampoline node.
	OutgoingNodeIDType tlv.Type = 14

	// RecipientFeaturesType is the BOLT 4 TLV type for recipient features
	// supplied to the final trampoline node.
	RecipientFeaturesType tlv.Type = 21

	// RecipientBlindedPathsType is the BOLT 4 TLV type for recipient
	// blinded paths supplied to the final trampoline node.
	RecipientBlindedPathsType tlv.Type = 22
)

// Payload is a trampoline-context BOLT 4 hop payload. Type 14 is deliberately
// decoded as outgoing_node_id here; the ordinary final-hop parser continues
// to decode lnd's pre-existing type 14 AMP record.
type Payload struct {
	// AmountToForward is the amount the trampoline hop must forward.
	AmountToForward *uint64

	// OutgoingCLTV is the absolute CLTV value the trampoline hop must use.
	OutgoingCLTV *uint32

	// PaymentData carries the payment secret and total amount.
	PaymentData *record.MPP

	// EncryptedRecipientData carries route-blinding instructions.
	EncryptedRecipientData []byte

	// CurrentPathKey carries the route-blinding path key.
	CurrentPathKey *btcec.PublicKey

	// OutgoingNodeID identifies the next trampoline node.
	OutgoingNodeID *btcec.PublicKey

	// PaymentMetadata carries invoice-provided payment metadata.
	PaymentMetadata []byte

	// TotalAmountMsat carries the blinded final recipient's total amount.
	TotalAmountMsat *uint64

	// TrampolineOnion is the embedded, variable-sized trampoline onion.
	TrampolineOnion *sphinx.OnionPacket

	// RecipientFeatures carries the recipient feature vector.
	RecipientFeatures *lnwire.RawFeatureVector

	// RecipientBlindedPaths carries structured blinded paths and their
	// aggregate payment constraints.
	RecipientBlindedPaths *RecipientBlindedPaths
}

// Encode writes the payload as a canonical TLV stream.
func (p *Payload) Encode(w io.Writer) error {
	records := make([]tlv.Record, 0, 10)

	if p.AmountToForward != nil {
		records = append(records, record.NewAmtToFwdRecord(
			p.AmountToForward,
		))
	}
	if p.OutgoingCLTV != nil {
		records = append(records, record.NewLockTimeRecord(
			p.OutgoingCLTV,
		))
	}
	if p.PaymentData != nil {
		records = append(records, p.PaymentData.Record())
	}
	if p.EncryptedRecipientData != nil {
		records = append(records, record.NewEncryptedDataRecord(
			&p.EncryptedRecipientData,
		))
	}
	if p.CurrentPathKey != nil {
		records = append(records, record.NewBlindingPointRecord(
			&p.CurrentPathKey,
		))
	}
	if p.OutgoingNodeID != nil {
		records = append(records, tlv.MakePrimitiveRecord(
			OutgoingNodeIDType, &p.OutgoingNodeID,
		))
	}
	if p.PaymentMetadata != nil {
		records = append(records, record.NewMetadataRecord(
			&p.PaymentMetadata,
		))
	}
	if p.TotalAmountMsat != nil {
		records = append(records, record.NewTotalAmtMsatBlinded(
			p.TotalAmountMsat,
		))
	}
	if p.TrampolineOnion != nil {
		records = append(records, newOnionPacketRecord(
			&p.TrampolineOnion,
		))
	}
	if p.RecipientFeatures != nil {
		records = append(records, newFeatureRecord(
			&p.RecipientFeatures,
		))
	}
	if p.RecipientBlindedPaths != nil {
		records = append(records, newRecipientBlindedPathsRecord(
			&p.RecipientBlindedPaths,
		))
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// DecodePayload parses an untrusted trampoline-context hop payload.
func DecodePayload(r io.Reader) (*Payload, tlv.TypeMap, error) {
	var (
		amount         uint64
		cltv           uint32
		paymentData    record.MPP
		encryptedData  []byte
		currentPathKey *btcec.PublicKey
		outgoingNodeID *btcec.PublicKey
		metadata       []byte
		totalAmount    uint64
		onion          *sphinx.OnionPacket
		features       *lnwire.RawFeatureVector
		blindedPaths   *RecipientBlindedPaths
	)

	stream, err := tlv.NewStream(
		record.NewAmtToFwdRecord(&amount),
		record.NewLockTimeRecord(&cltv),
		paymentData.Record(),
		record.NewEncryptedDataRecord(&encryptedData),
		record.NewBlindingPointRecord(&currentPathKey),
		tlv.MakePrimitiveRecord(
			OutgoingNodeIDType, &outgoingNodeID,
		),
		record.NewMetadataRecord(&metadata),
		record.NewTotalAmtMsatBlinded(&totalAmount),
		newOnionPacketRecord(&onion),
		newFeatureRecord(&features),
		newRecipientBlindedPathsRecord(&blindedPaths),
	)
	if err != nil {
		return nil, nil, err
	}

	parsed, err := stream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return nil, nil, err
	}

	for typ, value := range parsed {
		if value != nil && typ%2 == 0 {
			return nil, nil, fmt.Errorf(
				"unknown required trampoline payload type %d",
				typ,
			)
		}
	}

	payload := &Payload{}
	if _, ok := parsed[record.AmtOnionType]; ok {
		payload.AmountToForward = &amount
	}
	if _, ok := parsed[record.LockTimeOnionType]; ok {
		payload.OutgoingCLTV = &cltv
	}
	if _, ok := parsed[record.MPPOnionType]; ok {
		payload.PaymentData = &paymentData
	}
	if _, ok := parsed[record.EncryptedDataOnionType]; ok {
		payload.EncryptedRecipientData = encryptedData
	}
	if _, ok := parsed[record.BlindingPointOnionType]; ok {
		payload.CurrentPathKey = currentPathKey
	}
	if _, ok := parsed[OutgoingNodeIDType]; ok {
		payload.OutgoingNodeID = outgoingNodeID
	}
	if _, ok := parsed[record.MetadataOnionType]; ok {
		payload.PaymentMetadata = metadata
	}
	if _, ok := parsed[record.TotalAmtMsatBlindedType]; ok {
		payload.TotalAmountMsat = &totalAmount
	}
	if _, ok := parsed[OnionPacketType]; ok {
		payload.TrampolineOnion = onion
	}
	if _, ok := parsed[RecipientFeaturesType]; ok {
		payload.RecipientFeatures = features
	}
	if _, ok := parsed[RecipientBlindedPathsType]; ok {
		payload.RecipientBlindedPaths = blindedPaths
	}

	return payload, parsed, nil
}

// newFeatureRecord creates the raw, length-from-TLV feature-vector codec used
// by recipient_features.
func newFeatureRecord(features **lnwire.RawFeatureVector) tlv.Record {
	size := func() uint64 {
		if features == nil || *features == nil {
			return 0
		}

		return uint64((*features).SerializeSize())
	}

	encoder := func(w io.Writer, val interface{}, _ *[8]byte) error {
		target, ok := val.(**lnwire.RawFeatureVector)
		if !ok || target == nil || *target == nil {
			return tlv.NewTypeForEncodingErr(
				val, "**lnwire.RawFeatureVector",
			)
		}

		return (*target).EncodeBase256(w)
	}

	decoder := func(r io.Reader, val interface{}, _ *[8]byte,
		l uint64) error {

		target, ok := val.(**lnwire.RawFeatureVector)
		if !ok || target == nil {
			return tlv.NewTypeForDecodingErr(
				val, "**lnwire.RawFeatureVector", l,
				tlv.MaxRecordSize,
			)
		}

		vector := lnwire.NewRawFeatureVector()
		if err := vector.DecodeBase256(r, int(l)); err != nil {
			return err
		}
		if vector.SerializeSize() != int(l) {
			return fmt.Errorf("non-minimal recipient features")
		}

		*target = vector

		return nil
	}

	return tlv.MakeDynamicRecord(
		RecipientFeaturesType, features, size, encoder, decoder,
	)
}

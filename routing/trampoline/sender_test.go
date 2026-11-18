package trampoline

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/stretchr/testify/require"
)

// TestDeriveSessionKey verifies deterministic domain separation from the
// ordinary payment attempt key.
func TestDeriveSessionKey(t *testing.T) {
	t.Parallel()

	outer, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x19}, 32))
	derived1, err := DeriveSessionKey(outer)
	require.NoError(t, err)
	derived2, err := DeriveSessionKey(outer)
	require.NoError(t, err)

	require.Equal(t, derived1.Serialize(), derived2.Serialize())
	require.NotEqual(t, outer.Serialize(), derived1.Serialize())

	_, err = DeriveSessionKey(nil)
	require.Error(t, err)
}

// TestBuildBolt11Onion verifies the complete sender-to-relay-to-recipient
// construction, including the final invoice secret and random padding.
func TestBuildBolt11Onion(t *testing.T) {
	t.Parallel()

	relayKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x31}, 32))
	finalKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x32}, 32))
	sessionKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x33}, 32))
	hash := bytes.Repeat([]byte{0x42}, 32)
	secret := [32]byte{1, 2, 3}
	features := trampolineFeatures()

	encoded, err := BuildBolt11Onion(Bolt11Request{
		PaymentHash: hash,
		PaddingSize: 47,
		SessionKey:  sessionKey,
		Hops: []Bolt11Hop{
			{
				NodeID:       relayKey.PubKey(),
				Features:     features,
				AmountMsat:   100_500,
				OutgoingCLTV: 700,
			},
			{
				NodeID:        finalKey.PubKey(),
				Features:      features,
				AmountMsat:    100_000,
				OutgoingCLTV:  660,
				PaymentSecret: &secret,
				Metadata:      []byte{4, 5, 6},
			},
		},
	})
	require.NoError(t, err)

	packet, err := DecodeOnionPacket(encoded)
	require.NoError(t, err)

	relayRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: relayKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err := relayRouter.ProcessOnionPacket(packet, hash, 0)
	require.NoError(t, err)
	relayPayload, _, err := DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(t, uint64(100_500), *relayPayload.AmountToForward)
	require.Equal(t, finalKey.PubKey(), relayPayload.OutgoingNodeID)
	require.Nil(t, relayPayload.PaymentData)

	finalRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: finalKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err = finalRouter.ProcessOnionPacket(
		processed.NextPacket, hash, 0,
	)
	require.NoError(t, err)
	finalPayload, _, err := DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Nil(t, finalPayload.OutgoingNodeID)
	require.Equal(t, secret, finalPayload.PaymentData.PaymentAddr())
	require.Equal(t, []byte{4, 5, 6}, finalPayload.PaymentMetadata)
}

// TestBuildOfficialBolt11Onion verifies byte-for-byte construction of the
// trampoline onion in lightning/bolts#836 at proposal commit 644b82f1.
func TestBuildOfficialBolt11Onion(t *testing.T) {
	t.Parallel()

	session, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x03}, 32))
	secret := [32]byte(bytes.Repeat([]byte{0x2a}, 32))
	features := trampolineFeatures()

	encoded, err := BuildBolt11Onion(Bolt11Request{
		PaymentHash: bytes.Repeat([]byte{0x42}, 32),
		SessionKey:  session,
		Hops: []Bolt11Hop{
			{
				NodeID: mustPubKey(
					t, "027f31ebc5462c1fdce1b737ecff52d37"+
						"d75dea43ce11c74d25aa297165"+
						"faa2007",
				),
				Features:     features,
				AmountMsat:   100_000_000,
				OutgoingCLTV: 800_000,
			},
			{
				NodeID: mustPubKey(
					t,
					"02edabbd16b41c8371b92ef2"+
						"f04c1185b4f03b6dcd52ba9"+
						"b78d9d7c89c8f221145",
				),
				Features:      features,
				AmountMsat:    100_000_000,
				OutgoingCLTV:  800_000,
				PaymentSecret: &secret,
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, mustHex(t, officialInnerOnion), encoded)
}

// TestBuildBolt11OnionValidation verifies deterministic rejection of missing
// feature support, missing secrets, invalid hashes, and excessive padding.
func TestBuildBolt11OnionValidation(t *testing.T) {
	t.Parallel()

	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x41}, 32))
	secret := [32]byte{}
	valid := Bolt11Request{
		PaymentHash: make([]byte, 32),
		Hops: []Bolt11Hop{{
			NodeID:        key.PubKey(),
			Features:      trampolineFeatures(),
			AmountMsat:    1,
			OutgoingCLTV:  1,
			PaymentSecret: &secret,
		}},
	}

	_, err := BuildBolt11Onion(Bolt11Request{})
	require.ErrorIs(t, err, ErrNoTrampolineHops)

	missingFeature := valid
	missingFeature.Hops = append([]Bolt11Hop(nil), valid.Hops...)
	missingFeature.Hops[0].Features = lnwire.EmptyFeatureVector()
	_, err = BuildBolt11Onion(missingFeature)
	require.ErrorIs(t, err, ErrTrampolineFeatureMissing)

	missingSecret := valid
	missingSecret.Hops = append([]Bolt11Hop(nil), valid.Hops...)
	missingSecret.Hops[0].PaymentSecret = nil
	_, err = BuildBolt11Onion(missingSecret)
	require.ErrorContains(t, err, "payment secret")

	invalidHash := valid
	invalidHash.PaymentHash = []byte{1}
	_, err = BuildBolt11Onion(invalidHash)
	require.ErrorContains(t, err, "payment hash")

	excessivePadding := valid
	excessivePadding.PaddingSize = sphinx.MaxRoutingPayloadSize
	_, err = BuildBolt11Onion(excessivePadding)
	require.ErrorIs(t, err, ErrTrampolinePaddingTooLarge)
}

// TestBuildBolt12Onion verifies a clear trampoline prefix followed by a real
// two-node blinded path, including blinded Sphinx peeling at the final node.
func TestBuildBolt12Onion(t *testing.T) {
	t.Parallel()

	relayKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x51}, 32))
	introKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x52}, 32))
	finalKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x53}, 32))
	blindingSession, _ := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{0x54}, 32),
	)
	onionSession, _ := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{0x55}, 32),
	)
	hash := bytes.Repeat([]byte{0x56}, 32)
	pathID := bytes.Repeat([]byte{0x57}, 32)

	introData, err := record.EncodeBlindedRouteData(
		record.NewDummyHopRouteData(
			finalKey.PubKey(),
			record.PaymentRelayInfo{
				CltvExpiryDelta: 10,
				BaseFee:         100,
			},
			record.PaymentConstraints{
				MaxCltvExpiry:   200,
				HtlcMinimumMsat: 1,
			},
		),
	)
	require.NoError(t, err)
	finalData, err := record.EncodeBlindedRouteData(
		record.NewFinalHopBlindedRouteData(nil, pathID),
	)
	require.NoError(t, err)

	blinded, err := sphinx.BuildBlindedPath(
		blindingSession, []*sphinx.HopInfo{
			{
				NodePub:   introKey.PubKey(),
				PlainText: introData,
			},
			{
				NodePub:   finalKey.PubKey(),
				PlainText: finalData,
			},
		},
	)
	require.NoError(t, err)
	introduction, err := lnwire.NewPubkeyIntro(
		blinded.Path.IntroductionPoint,
	)
	require.NoError(t, err)
	wirePath := lnwire.BlindedPath{
		IntroductionNode: introduction,
		BlindingPoint:    blinded.Path.BlindingPoint,
		Hops: make(
			[]lnwire.BlindedHop, len(blinded.Path.BlindedHops),
		),
	}
	for i := range blinded.Path.BlindedHops {
		hop := blinded.Path.BlindedHops[i]
		wirePath.Hops[i] = lnwire.BlindedHop{
			BlindedNodeID: hop.BlindedNodePub,
			EncryptedData: append([]byte(nil), hop.CipherText...),
		}
	}

	encoded, err := BuildBolt12Onion(Bolt12Request{
		PaymentHash:      hash,
		IntroductionNode: introKey.PubKey(),
		BlindedPath:      wirePath,
		BlindedFeatures:  trampolineFeatures(),
		AmountMsat:       1_000,
		OutgoingCLTV:     110,
		TotalMsat:        1_000,
		PaddingSize:      31,
		SessionKey:       onionSession,
		ClearHops: []Bolt11Hop{{
			NodeID:       relayKey.PubKey(),
			Features:     trampolineFeatures(),
			AmountMsat:   1_100,
			OutgoingCLTV: 130,
		}},
	})
	require.NoError(t, err)

	packet, err := DecodeOnionPacket(encoded)
	require.NoError(t, err)
	relayRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: relayKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err := relayRouter.ProcessOnionPacket(packet, hash, 0)
	require.NoError(t, err)
	relayPayload, _, err := DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(t, introKey.PubKey(), relayPayload.OutgoingNodeID)

	introRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: introKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err = introRouter.ProcessOnionPacket(
		processed.NextPacket, hash, 0,
	)
	require.NoError(t, err)
	introPayload, _, err := DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(
		t, blinded.Path.BlindingPoint, introPayload.CurrentPathKey,
	)
	require.Nil(t, introPayload.AmountToForward)
	decrypted, err := introRouter.DecryptBlindedHopData(
		introPayload.CurrentPathKey,
		introPayload.EncryptedRecipientData,
	)
	require.NoError(t, err)
	require.Equal(t, introData, decrypted)

	nextPathKey, err := introRouter.NextEphemeral(
		introPayload.CurrentPathKey,
	)
	require.NoError(t, err)
	finalRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: finalKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err = finalRouter.ProcessOnionPacket(
		processed.NextPacket, hash, 0,
		sphinx.WithBlindingPoint(nextPathKey),
	)
	require.NoError(t, err)
	require.EqualValues(t, sphinx.ExitNode, processed.Action)
	finalPayload, _, err := DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(t, uint64(1_000), *finalPayload.AmountToForward)
	require.Equal(t, uint32(110), *finalPayload.OutgoingCLTV)
	require.Equal(t, uint64(1_000), *finalPayload.TotalAmountMsat)
	require.Nil(t, finalPayload.CurrentPathKey)
}

// TestBuildBolt12OnionValidation verifies bounded path and feature checks.
func TestBuildBolt12OnionValidation(t *testing.T) {
	t.Parallel()

	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x61}, 32))
	request := Bolt12Request{
		PaymentHash:      make([]byte, 32),
		IntroductionNode: key.PubKey(),
		BlindedFeatures:  trampolineFeatures(),
		AmountMsat:       1,
		TotalMsat:        1,
		BlindedPath: lnwire.BlindedPath{
			BlindingPoint: key.PubKey(),
			Hops: []lnwire.BlindedHop{{
				BlindedNodeID: key.PubKey(),
				EncryptedData: []byte{1},
			}},
		},
	}

	_, err := BuildBolt12Onion(Bolt12Request{})
	require.ErrorContains(t, err, "payment hash")

	missingFeature := request
	missingFeature.BlindedFeatures = lnwire.EmptyFeatureVector()
	_, err = BuildBolt12Onion(missingFeature)
	require.ErrorIs(t, err, ErrTrampolineFeatureMissing)

	totalBelowAmount := request
	totalBelowAmount.TotalMsat = 0
	_, err = BuildBolt12Onion(totalBelowAmount)
	require.ErrorContains(t, err, "total")
}

func trampolineFeatures() *lnwire.FeatureVector {
	raw := lnwire.NewRawFeatureVector(
		lnwire.TLVOnionPayloadOptional,
		lnwire.TrampolineRoutingOptional,
	)

	return lnwire.NewFeatureVector(raw, lnwire.Features)
}

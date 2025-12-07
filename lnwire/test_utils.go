package lnwire

import (
	"crypto/sha256"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// charset contains valid UTF-8 characters that can be used to generate random
// strings for testing purposes.
const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// RandPartialSig generates a random ParialSig message using rapid's
// generators.
func RandPartialSig(t *rapid.T) *PartialSig {
	// Generate random private key bytes
	sigBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "privKeyBytes")

	var s btcec.ModNScalar
	s.SetByteSlice(sigBytes)

	return &PartialSig{
		Sig: s,
	}
}

// RandPartialSigWithNonce generates a random PartialSigWithNonce using rapid
// generators.
func RandPartialSigWithNonce(t *rapid.T) *PartialSigWithNonce {
	sigLen := rapid.IntRange(1, 65).Draw(t, "partialSigLen")
	sigBytes := rapid.SliceOfN(
		rapid.Byte(), sigLen, sigLen,
	).Draw(t, "partialSig")

	sigScalar := new(btcec.ModNScalar)
	sigScalar.SetByteSlice(sigBytes)

	return NewPartialSigWithNonce(
		RandMusig2Nonce(t), *sigScalar,
	)
}

// RandPubKey generates a random public key using rapid's generators.
func RandPubKey(t *rapid.T) *btcec.PublicKey {
	privKeyBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
		t, "privKeyBytes",
	)
	_, pub := btcec.PrivKeyFromBytes(privKeyBytes)

	return pub
}

// RandChannelID generates a random channel ID.
func RandChannelID(t *rapid.T) ChannelID {
	var c ChannelID
	bytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "channelID")
	copy(c[:], bytes)

	return c
}

// RandShortChannelID generates a random short channel ID.
func RandShortChannelID(t *rapid.T) ShortChannelID {
	return NewShortChanIDFromInt(
		uint64(rapid.IntRange(1, 100000).Draw(t, "shortChanID")),
	)
}

// RandFeatureVector generates a random feature vector.
func RandFeatureVector(t *rapid.T) *RawFeatureVector {
	featureVec := NewRawFeatureVector()

	// Add a random number of random feature bits
	numFeatures := rapid.IntRange(0, 20).Draw(t, "numFeatures")
	for i := 0; i < numFeatures; i++ {
		bit := FeatureBit(rapid.IntRange(0, 100).Draw(
			t, fmt.Sprintf("featureBit-%d", i)),
		)
		featureVec.Set(bit)
	}

	return featureVec
}

// RandSignature generates a signature for testing.
func RandSignature(t *rapid.T) Sig {
	testRScalar := new(btcec.ModNScalar)
	testSScalar := new(btcec.ModNScalar)

	// Generate random bytes for R and S
	rBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "rBytes")
	sBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "sBytes")
	_ = testRScalar.SetByteSlice(rBytes)
	_ = testSScalar.SetByteSlice(sBytes)

	testSig := ecdsa.NewSignature(testRScalar, testSScalar)

	sig, err := NewSigFromSignature(testSig)
	if err != nil {
		panic(fmt.Sprintf("unable to create signature: %v", err))
	}

	return sig
}

// RandPaymentHash generates a random payment hash.
func RandPaymentHash(t *rapid.T) [32]byte {
	var hash [32]byte
	bytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "paymentHash")
	copy(hash[:], bytes)

	return hash
}

// RandPaymentPreimage generates a random payment preimage.
func RandPaymentPreimage(t *rapid.T) [32]byte {
	var preimage [32]byte
	bytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "preimage")
	copy(preimage[:], bytes)

	return preimage
}

// RandChainHash generates a random chain hash.
func RandChainHash(t *rapid.T) chainhash.Hash {
	var hash [32]byte
	bytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "chainHash")
	copy(hash[:], bytes)

	return hash
}

// RandNodeAlias generates a random node alias.
func RandNodeAlias(t *rapid.T) NodeAlias {
	var alias NodeAlias
	aliasLength := rapid.IntRange(0, 32).Draw(t, "aliasLength")

	aliasBytes := rapid.SliceOfN(
		rapid.SampledFrom([]rune(charset)), aliasLength, aliasLength,
	).Draw(t, "alias")

	copy(alias[:], string(aliasBytes))

	return alias
}

// RandNodeAlias2 generates a random node alias that is compatible with
// NodeAlias2 validation (non-empty, max 32 bytes, valid UTF-8).
func RandNodeAlias2(t *rapid.T) NodeAlias2 {
	// Ensure length is at least 1 to satisfy NodeAlias2 validation
	aliasLength := rapid.IntRange(1, 32).Draw(t, "aliasLength")

	aliasBytes := rapid.SliceOfN(
		rapid.SampledFrom([]rune(charset)), aliasLength, aliasLength,
	).Draw(t, "alias")

	return []byte(string(aliasBytes))
}

// RandNetAddrs generates random network addresses.
func RandNetAddrs(t *rapid.T) []net.Addr {
	numAddresses := rapid.IntRange(0, 5).Draw(t, "numAddresses")
	if numAddresses == 0 {
		return nil
	}

	addresses := make([]net.Addr, numAddresses)
	for i := 0; i < numAddresses; i++ {
		addressType := rapid.IntRange(0, 1).Draw(
			t, fmt.Sprintf("addressType-%d", i),
		)

		switch addressType {
		// IPv4.
		case 0:
			ipBytes := rapid.SliceOfN(rapid.Byte(), 4, 4).Draw(
				t, fmt.Sprintf("ipv4-%d", i),
			)
			port := rapid.IntRange(1, 65535).Draw(
				t, fmt.Sprintf("port-%d", i),
			)
			addresses[i] = &net.TCPAddr{
				IP:   ipBytes,
				Port: port,
			}

		// IPv6.
		case 1:
			ipBytes := rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(
				t, fmt.Sprintf("ipv6-%d", i),
			)
			port := rapid.IntRange(1, 65535).Draw(
				t, fmt.Sprintf("port-%d", i),
			)
			addresses[i] = &net.TCPAddr{
				IP:   ipBytes,
				Port: port,
			}
		}
	}

	return addresses
}

// RandCustomRecords generates random custom TLV records.
func RandCustomRecords(t *rapid.T, ignoreRecords fn.Set[uint64]) (CustomRecords,
	fn.Set[uint64]) {

	customRecords, set := RandTLVRecords(
		t, ignoreRecords, MinCustomRecordsTlvType,
	)

	// Validate the custom records as a sanity check.
	require.NoError(t, customRecords.Validate())

	return customRecords, set
}

// RandSignedRangeRecords generates a random set of signed records in the
// second "signed" tlv range for pure TLV messages.
func RandSignedRangeRecords(t *rapid.T) (CustomRecords, fn.Set[uint64]) {
	return RandTLVRecords(t, nil, pureTLVSignedSecondRangeStart)
}

// RandTLVRecords generates custom TLV records.
func RandTLVRecords(t *rapid.T, ignoreRecords fn.Set[uint64],
	rangeStart int) (CustomRecords, fn.Set[uint64]) {

	numRecords := rapid.IntRange(0, 5).Draw(t, "numRecords")
	customRecords := make(CustomRecords)

	if numRecords == 0 {
		return nil, nil
	}

	rangeStop := rangeStart + 30_000

	ignoreSet := fn.NewSet[uint64]()
	for i := 0; i < numRecords; i++ {
		recordType := uint64(
			rapid.IntRange(rangeStart, rangeStop).
				Filter(func(i int) bool {
					return !ignoreRecords.Contains(
						uint64(i),
					)
				}).
				Draw(
					t, fmt.Sprintf("recordType-%d", i),
				),
		)
		recordLen := rapid.IntRange(4, 64).Draw(
			t, fmt.Sprintf("recordLen-%d", i),
		)
		record := rapid.SliceOfN(
			rapid.Byte(), recordLen, recordLen,
		).Draw(t, fmt.Sprintf("record-%d", i))

		customRecords[recordType] = record

		ignoreSet.Add(recordType)
	}

	return customRecords, ignoreSet
}

// RandMusig2Nonce generates a random musig2 nonce.
func RandMusig2Nonce(t *rapid.T) Musig2Nonce {
	var nonce Musig2Nonce
	bytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "nonce")
	copy(nonce[:], bytes)

	return nonce
}

// RandExtraOpaqueData generates random extra opaque data.
func RandExtraOpaqueData(t *rapid.T,
	ignoreRecords fn.Set[uint64]) ExtraOpaqueData {

	// Make some random records.
	cRecords, _ := RandTLVRecords(t, ignoreRecords, 0)
	if cRecords == nil {
		return ExtraOpaqueData{}
	}

	// Encode those records as opaque data.
	recordBytes, err := cRecords.Serialize()
	require.NoError(t, err)

	return ExtraOpaqueData(recordBytes)
}

// RandOpaqueReason generates a random opaque reason for HTLC failures.
func RandOpaqueReason(t *rapid.T) OpaqueReason {
	reasonLen := rapid.IntRange(32, 300).Draw(t, "reasonLen")
	return rapid.SliceOfN(rapid.Byte(), reasonLen, reasonLen).Draw(
		t, "opaqueReason",
	)
}

// RandFailCode generates a random HTLC failure code.
func RandFailCode(t *rapid.T) FailCode {
	// List of known failure codes to choose from Using only the documented
	// codes.
	validCodes := []FailCode{
		CodeInvalidRealm,
		CodeTemporaryNodeFailure,
		CodePermanentNodeFailure,
		CodeRequiredNodeFeatureMissing,
		CodePermanentChannelFailure,
		CodeRequiredChannelFeatureMissing,
		CodeUnknownNextPeer,
		CodeIncorrectOrUnknownPaymentDetails,
		CodeIncorrectPaymentAmount,
		CodeFinalExpiryTooSoon,
		CodeInvalidOnionVersion,
		CodeInvalidOnionHmac,
		CodeInvalidOnionKey,
		CodeTemporaryChannelFailure,
		CodeChannelDisabled,
		CodeExpiryTooSoon,
		CodeMPPTimeout,
		CodeInvalidOnionPayload,
		CodeFeeInsufficient,
	}

	// Choose a random code from the list.
	idx := rapid.IntRange(0, len(validCodes)-1).Draw(t, "failCodeIndex")

	return validCodes[idx]
}

// RandSHA256Hash generates a random SHA256 hash.
func RandSHA256Hash(t *rapid.T) [sha256.Size]byte {
	var hash [sha256.Size]byte
	bytes := rapid.SliceOfN(rapid.Byte(), sha256.Size, sha256.Size).Draw(
		t, "sha256Hash",
	)
	copy(hash[:], bytes)

	return hash
}

// RandDeliveryAddress generates a random delivery address (script).
func RandDeliveryAddress(t *rapid.T) DeliveryAddress {
	addrLen := rapid.IntRange(1, 34).Draw(t, "addrLen")

	return rapid.SliceOfN(rapid.Byte(), addrLen, addrLen).Draw(
		t, "deliveryAddress",
	)
}

// RandChannelType generates a random channel type.
func RandChannelType(t *rapid.T) *ChannelType {
	vec := RandFeatureVector(t)
	chanType := ChannelType(*vec)

	return &chanType
}

// RandLeaseExpiry generates a random lease expiry.
func RandLeaseExpiry(t *rapid.T) *LeaseExpiry {
	exp := LeaseExpiry(
		uint32(rapid.IntRange(1000, 1000000).Draw(t, "leaseExpiry")),
	)

	return &exp
}

// RandOutPoint generates a random transaction outpoint.
func RandOutPoint(t *rapid.T) wire.OutPoint {
	// Generate a random transaction ID
	var txid chainhash.Hash
	txidBytes := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, "txid")
	copy(txid[:], txidBytes)

	// Generate a random output index
	vout := uint32(rapid.IntRange(0, 10).Draw(t, "vout"))

	return wire.OutPoint{
		Hash:  txid,
		Index: vout,
	}
}

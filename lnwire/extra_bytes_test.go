package lnwire

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/lightningnetwork/lnd/tlv"
)

// TestExtraOpaqueDataEncodeDecode tests that we're able to encode/decode
// arbitrary payloads.
func TestExtraOpaqueDataEncodeDecode(t *testing.T) {
	t.Parallel()

	type testCase struct {
		// emptyBytes indicates if we should try to encode empty bytes
		// or not.
		emptyBytes bool

		// inputBytes if emptyBytes is false, then we'll read in this
		// set of bytes instead.
		inputBytes []byte
	}

	// We should be able to read in an arbitrary set of bytes as an
	// ExtraOpaqueData, then encode those new bytes into a new instance.
	// The final two instances should be identical.
	scenario := func(test testCase) bool {
		var (
			extraData ExtraOpaqueData
			b         bytes.Buffer
		)

		copy(extraData[:], test.inputBytes)

		if err := extraData.Encode(&b, ProtocolVersionTLV); err != nil {
			t.Fatalf("unable to encode extra data: %v", err)
			return false
		}

		var newBytes ExtraOpaqueData
		if err := newBytes.Decode(&b, ProtocolVersionTLV); err != nil {
			t.Fatalf("unable to decode extra bytes: %v", err)
			return false
		}

		if !bytes.Equal(extraData[:], newBytes[:]) {
			t.Fatalf("expected %x, got %x", extraData,
				newBytes)
			return false
		}

		return true
	}

	// We'll make a function to generate random test data. Half of the
	// time, we'll actually feed in blank bytes.
	quickCfg := &quick.Config{
		Values: func(v []reflect.Value, r *rand.Rand) {

			var newTestCase testCase
			if r.Int31()%2 == 0 {
				newTestCase.emptyBytes = true
			}

			if !newTestCase.emptyBytes {
				numBytes := r.Int31n(1000)
				newTestCase.inputBytes = make([]byte, numBytes)

				_, err := r.Read(newTestCase.inputBytes)
				if err != nil {
					t.Fatalf("unable to gen random bytes: %v", err)
					return
				}
			}

			v[0] = reflect.ValueOf(newTestCase)
		},
	}

	if err := quick.Check(scenario, quickCfg); err != nil {
		t.Fatalf("encode+decode test failed: %v", err)
	}
}

// TestExtraOpaqueDataPackUnpackRecords tests that we're able to pack a set of
// tlv.Records into a stream, and unpack them on the other side to obtain the
// same set of records.
func TestExtraOpaqueDataPackUnpackRecords(t *testing.T) {
	t.Parallel()

	var (
		type1 tlv.Type = 1
		type2 tlv.Type = 2

		channelType1 uint8 = 2
		channelType2 uint8

		hop1 uint32 = 99
		hop2 uint32
	)
	testRecords := []tlv.Record{
		tlv.MakePrimitiveRecord(type1, &channelType1),
		tlv.MakePrimitiveRecord(type2, &hop1),
	}

	// Now that we have our set of sample records and types, we'll encode
	// them into the passed ExtraOpaqueData instance.
	var extraBytes ExtraOpaqueData
	if err := extraBytes.PackRecords(testRecords); err != nil {
		t.Fatalf("unable to pack records: %v", err)
	}

	// We'll now simulate decoding these types _back_ into records on the
	// other side.
	newRecords := []tlv.Record{
		tlv.MakePrimitiveRecord(type1, &channelType2),
		tlv.MakePrimitiveRecord(type2, &hop2),
	}
	typeMap, err := extraBytes.ExtractRecords(newRecords...)
	if err != nil {
		t.Fatalf("unable to extract record: %v", err)
	}

	// We should find that the new backing values have been populated with
	// the proper value.
	switch {
	case channelType1 != channelType2:
		t.Fatalf("wrong record for channel type: expected %v, got %v",
			channelType1, channelType2)

	case hop1 != hop2:
		t.Fatalf("wrong record for hop: expected %v, got %v", hop1,
			hop2)
	}

	// Both types we created above should be found in the type map.
	if _, ok := typeMap[type1]; !ok {
		t.Fatalf("type1 not found in typeMap")
	}
	if _, ok := typeMap[type2]; !ok {
		t.Fatalf("type2 not found in typeMap")
	}
}

// TestExtraOpaqueDataProtocolVersion tests that the encode/decode methods will
// observe the passed protocol version.
func TestExtraOpaqueDataProtocolVersion(t *testing.T) {
	t.Parallel()

	extraData := ExtraOpaqueData([]byte("kek"))

	var b bytes.Buffer
	if err := extraData.Encode(&b, ProtocolVersionLegacy); err != nil {
		t.Fatalf("unable to encode: %v", err)
	}

	// The statement above shouldn't have included the extra data since
	// we're using the legacy protocol version.
	if len(b.Bytes()) != 0 {
		t.Fatalf("bytes were encoded using legacy "+
			"protocol version: %x", b.Bytes())
	}

	// If we encode using the proper version, then we should find the same
	// data encoded on the other side.
	if err := extraData.Encode(&b, ProtocolVersionTLV); err != nil {
		t.Fatalf("unable to encode: %v", err)
	}
	if !bytes.Equal(b.Bytes(), extraData[:]) {
		t.Fatalf("encoding mismatch: expected %x, got %x",
			b.Bytes(), extraData[:])
	}

	// Now for the other direction, we'll attempt to decode into a fresh
	// buffer, but using the legacy version. In the end, no bytes should be
	// decoded.
	var newExtraData ExtraOpaqueData
	if err := newExtraData.Decode(&b, ProtocolVersionLegacy); err != nil {
		t.Fatalf("unable to decode data: %v", err)
	}

	if len(newExtraData[:]) != 0 {
		t.Fatalf("expected not data to be decoded!")
	}

	// Finally, if we decode using the proper protocol version, we should get
	// the same bytes out that we put in.
	if err := newExtraData.Decode(&b, ProtocolVersionTLV); err != nil {
		t.Fatalf("unable to decode data: %v", err)
	}
	if !bytes.Equal(extraData[:], newExtraData[:]) {
		t.Fatalf("encoding mismatch: expected %x, got %x",
			extraData, newExtraData)
	}

}

package channeldb

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	priv, _ = btcec.NewPrivateKey(btcec.S256())
	pub     = priv.PubKey()

	tlvBytes   = []byte{1, 2, 3}
	tlvEncoder = tlv.StubEncoder(tlvBytes)
	testHop1   = &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		TLVRecords: []tlv.Record{
			tlv.MakeStaticRecord(1, nil, 3, tlvEncoder, nil),
			tlv.MakeStaticRecord(2, nil, 3, tlvEncoder, nil),
		},
	}

	testHop2 = &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
		LegacyPayload:    true,
	}

	testRoute = route.Route{
		TotalTimeLock: 123,
		TotalAmount:   1234567,
		SourcePubKey:  route.NewVertex(pub),
		Hops: []*route.Hop{
			testHop1,
			testHop2,
		},
	}
)

func makeFakePayment() *outgoingPayment {
	fakeInvoice := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate:   time.Unix(time.Now().Unix(), 0),
		Memo:           []byte("fake memo"),
		Receipt:        []byte("fake receipt"),
		PaymentRequest: []byte(""),
	}

	copy(fakeInvoice.Terms.PaymentPreimage[:], rev[:])
	fakeInvoice.Terms.Value = lnwire.NewMSatFromSatoshis(10000)

	fakePath := make([][33]byte, 3)
	for i := 0; i < 3; i++ {
		copy(fakePath[i][:], bytes.Repeat([]byte{byte(i)}, 33))
	}

	fakePayment := &outgoingPayment{
		Invoice:        *fakeInvoice,
		Fee:            101,
		Path:           fakePath,
		TimeLockLength: 1000,
	}
	copy(fakePayment.PaymentPreimage[:], rev[:])
	return fakePayment
}

func makeFakeInfo() (*PaymentCreationInfo, *PaymentAttemptInfo) {
	var preimg lntypes.Preimage
	copy(preimg[:], rev[:])

	c := &PaymentCreationInfo{
		PaymentHash: preimg.Hash(),
		Value:       1000,
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate:   time.Unix(time.Now().Unix(), 0),
		PaymentRequest: []byte(""),
	}

	a := &PaymentAttemptInfo{
		PaymentID:  44,
		SessionKey: priv,
		Route:      testRoute,
	}
	return c, a
}

func makeFakePaymentHash() [32]byte {
	var paymentHash [32]byte
	rBytes, _ := randomBytes(0, 32)
	copy(paymentHash[:], rBytes)

	return paymentHash
}

// randomBytes creates random []byte with length in range [minLen, maxLen)
func randomBytes(minLen, maxLen int) ([]byte, error) {
	randBuf := make([]byte, minLen+rand.Intn(maxLen-minLen))

	if _, err := rand.Read(randBuf); err != nil {
		return nil, fmt.Errorf("Internal error. "+
			"Cannot generate random string: %v", err)
	}

	return randBuf, nil
}

func makeRandomFakePayment() (*outgoingPayment, error) {
	var err error
	fakeInvoice := &Invoice{
		// Use single second precision to avoid false positive test
		// failures due to the monotonic time component.
		CreationDate: time.Unix(time.Now().Unix(), 0),
	}

	fakeInvoice.Memo, err = randomBytes(1, 50)
	if err != nil {
		return nil, err
	}

	fakeInvoice.Receipt, err = randomBytes(1, 50)
	if err != nil {
		return nil, err
	}

	fakeInvoice.PaymentRequest, err = randomBytes(1, 50)
	if err != nil {
		return nil, err
	}

	preImg, err := randomBytes(32, 33)
	if err != nil {
		return nil, err
	}
	copy(fakeInvoice.Terms.PaymentPreimage[:], preImg)

	fakeInvoice.Terms.Value = lnwire.MilliSatoshi(rand.Intn(10000))

	fakePathLen := 1 + rand.Intn(5)
	fakePath := make([][33]byte, fakePathLen)
	for i := 0; i < fakePathLen; i++ {
		b, err := randomBytes(33, 34)
		if err != nil {
			return nil, err
		}
		copy(fakePath[i][:], b)
	}

	fakePayment := &outgoingPayment{
		Invoice:        *fakeInvoice,
		Fee:            lnwire.MilliSatoshi(rand.Intn(1001)),
		Path:           fakePath,
		TimeLockLength: uint32(rand.Intn(10000)),
	}
	copy(fakePayment.PaymentPreimage[:], fakeInvoice.Terms.PaymentPreimage[:])

	return fakePayment, nil
}

func TestSentPaymentSerialization(t *testing.T) {
	t.Parallel()

	c, s := makeFakeInfo()

	var b bytes.Buffer
	if err := serializePaymentCreationInfo(&b, c); err != nil {
		t.Fatalf("unable to serialize creation info: %v", err)
	}

	newCreationInfo, err := deserializePaymentCreationInfo(&b)
	if err != nil {
		t.Fatalf("unable to deserialize creation info: %v", err)
	}

	if !reflect.DeepEqual(c, newCreationInfo) {
		t.Fatalf("Payments do not match after "+
			"serialization/deserialization %v vs %v",
			spew.Sdump(c), spew.Sdump(newCreationInfo),
		)
	}

	b.Reset()
	if err := serializePaymentAttemptInfo(&b, s); err != nil {
		t.Fatalf("unable to serialize info: %v", err)
	}

	newAttemptInfo, err := deserializePaymentAttemptInfo(&b)
	if err != nil {
		t.Fatalf("unable to deserialize info: %v", err)
	}

	// First we verify all the records match up porperly, as they aren't
	// able to be properly compared using reflect.DeepEqual.
	assertRouteHopRecordsEqual(&s.Route, &newAttemptInfo.Route)

	// With the hop recrods, equal, we'll now blank them out as
	// reflect.DeepEqual can't properly compare tlv.Record instances.
	newAttemptInfo.Route.Hops[0].TLVRecords = nil
	newAttemptInfo.Route.Hops[1].TLVRecords = nil
	s.Route.Hops[0].TLVRecords = nil
	s.Route.Hops[1].TLVRecords = nil

	if !reflect.DeepEqual(s, newAttemptInfo) {
		s.SessionKey.Curve = nil
		newAttemptInfo.SessionKey.Curve = nil
		t.Fatalf("Payments do not match after "+
			"serialization/deserialization %v vs %v",
			spew.Sdump(s), spew.Sdump(newAttemptInfo),
		)
	}

}

func assertRouteHopRecordsEqual(r1, r2 *route.Route) error {
	for i := 0; i < len(r1.Hops); i++ {
		for j := 0; j < len(r1.Hops[i].TLVRecords); j++ {
			expectedRecord := r1.Hops[i].TLVRecords[j]
			newRecord := r2.Hops[i].TLVRecords[j]

			err := assertHopRecordsEqual(expectedRecord, newRecord)
			if err != nil {
				return fmt.Errorf("route record mismatch: %v", err)
			}
		}
	}

	return nil
}

func assertHopRecordsEqual(h1, h2 tlv.Record) error {
	if h1.Type() != h2.Type() {
		return fmt.Errorf("wrong type: expected %v, got %v", h1.Type(),
			h2.Type())
	}

	var b bytes.Buffer
	if err := h2.Encode(&b); err != nil {
		return fmt.Errorf("unable to encode record: %v", err)
	}

	if !bytes.Equal(b.Bytes(), tlvBytes) {
		return fmt.Errorf("wrong raw record: expected %x, got %x",
			tlvBytes, b.Bytes())
	}

	if h1.Size() != h2.Size() {
		return fmt.Errorf("wrong size: expected %v, "+
			"got %v", h1.Size(), h2.Size())
	}

	return nil
}

func TestRouteSerialization(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	if err := SerializeRoute(&b, testRoute); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(b.Bytes())
	route2, err := DeserializeRoute(r)
	if err != nil {
		t.Fatal(err)
	}

	// First we verify all the records match up porperly, as they aren't
	// able to be properly compared using reflect.DeepEqual.
	err = assertRouteHopRecordsEqual(&testRoute, &route2)
	if err != nil {
		t.Fatalf("route tlv records don't match: %v", err)
	}

	// Now that we know the records match up, we'll examine the remainder
	// of the route without the TLV records attached as reflect.DeepEqual
	// can't properly assert their equality.
	testRoute.Hops[0].TLVRecords = nil
	testRoute.Hops[1].TLVRecords = nil
	route2.Hops[0].TLVRecords = nil
	route2.Hops[1].TLVRecords = nil

	if !reflect.DeepEqual(testRoute, route2) {
		t.Fatalf("routes not equal: \n%v vs \n%v",
			spew.Sdump(testRoute), spew.Sdump(route2))
	}
}

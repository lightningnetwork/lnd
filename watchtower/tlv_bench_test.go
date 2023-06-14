package watchtower_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
	"github.com/stretchr/testify/require"
)

// CreateSessionTLV mirrors the wtwire.CreateSession message, but uses TLV for
// encoding/decoding.
type CreateSessionTLV struct {
	BlobType     blob.Type
	MaxUpdates   uint16
	RewardBase   uint32
	RewardRate   uint32
	SweepFeeRate chainfee.SatPerKWeight

	tlvStream *tlv.Stream
}

// EBlobType is an encoder for blob.Type.
func EBlobType(w io.Writer, val interface{}, buf *[8]byte) error {
	if t, ok := val.(*blob.Type); ok {
		return tlv.EUint16T(w, uint16(*t), buf)
	}
	return tlv.NewTypeForEncodingErr(val, "blob.Type")
}

// EBlobType is an decoder for blob.Type.
func DBlobType(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if typ, ok := val.(*blob.Type); ok {
		var t uint16
		err := tlv.DUint16(r, &t, buf, l)
		if err != nil {
			return err
		}
		*typ = blob.Type(t)
		return nil
	}
	return tlv.NewTypeForDecodingErr(val, "blob.Type", l, 2)
}

// ESatPerKW is an encoder for lnwallet.SatPerKWeight.
func ESatPerKW(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*chainfee.SatPerKWeight); ok {
		v64 := uint64(*v)
		return tlv.EUint64(w, &v64, buf)
	}
	return tlv.NewTypeForEncodingErr(val, "chainfee.SatPerKWeight")
}

// DSatPerKW is an decoder for lnwallet.SatPerKWeight.
func DSatPerKW(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*chainfee.SatPerKWeight); ok {
		var sat uint64
		err := tlv.DUint64(r, &sat, buf, l)
		if err != nil {
			return err
		}
		*v = chainfee.SatPerKWeight(sat)
		return nil
	}
	return tlv.NewTypeForDecodingErr(val, "chainfee.SatPerKWeight", l, 8)
}

// NewCreateSessionTLV initializes a new CreateSessionTLV message.
func NewCreateSessionTLV() *CreateSessionTLV {
	m := &CreateSessionTLV{}
	m.tlvStream = tlv.MustNewStream(
		tlv.MakeStaticRecord(0, &m.BlobType, 2, EBlobType, DBlobType),
		tlv.MakePrimitiveRecord(1, &m.MaxUpdates),
		tlv.MakePrimitiveRecord(2, &m.RewardBase),
		tlv.MakePrimitiveRecord(3, &m.RewardRate),
		tlv.MakeStaticRecord(4, &m.SweepFeeRate, 8, ESatPerKW, DSatPerKW),
	)

	return m
}

// Encode writes the CreateSessionTLV to the passed io.Writer.
func (c *CreateSessionTLV) Encode(w io.Writer) error {
	return c.tlvStream.Encode(w)
}

// Decode reads the CreateSessionTLV from the passed io.Reader.
func (c *CreateSessionTLV) Decode(r io.Reader) error {
	return c.tlvStream.Decode(r)
}

// BenchmarkEncodeCreateSession benchmarks encoding of the non-TLV
// CreateSession.
func BenchmarkEncodeCreateSession(t *testing.B) {
	m := &wtwire.CreateSession{}

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		err = m.Encode(io.Discard, 0)
	}
	require.NoError(t, err)
}

// BenchmarkEncodeCreateSessionTLV benchmarks encoding of the TLV CreateSession.
func BenchmarkEncodeCreateSessionTLV(t *testing.B) {
	m := NewCreateSessionTLV()

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		err = m.Encode(io.Discard)
	}
	require.NoError(t, err)
}

// BenchmarkDecodeCreateSession benchmarks encoding of the non-TLV
// CreateSession.
func BenchmarkDecodeCreateSession(t *testing.B) {
	m := &wtwire.CreateSession{}

	var b bytes.Buffer
	err := m.Encode(&b, 0)
	require.NoError(t, err)

	r := bytes.NewReader(b.Bytes())

	t.ReportAllocs()
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		r.Seek(0, 0)
		err = m.Decode(r, 0)
	}
	require.NoError(t, err)
}

// BenchmarkDecodeCreateSessionTLV benchmarks decoding of the TLV CreateSession.
func BenchmarkDecodeCreateSessionTLV(t *testing.B) {
	m := NewCreateSessionTLV()

	var b bytes.Buffer
	err := m.Encode(&b)
	require.NoError(t, err)

	r := bytes.NewReader(b.Bytes())

	t.ReportAllocs()
	t.ResetTimer()

	for i := 0; i < t.N; i++ {
		r.Seek(0, 0)
		err = m.Decode(r)
	}
	require.NoError(t, err)
}

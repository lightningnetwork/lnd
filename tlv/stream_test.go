package tlv_test

import (
	"bytes"
	"io"
	"io/ioutil"
	"reflect"
	"testing"

	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

type testObj struct {
	u16 uint16
	u32 uint32
	u64 uint64
	buf []byte
}

type tlvTestCase struct {
	name   string
	encGen func() (*testObj, *tlv.Stream)
	decGen func() (*testObj, *tlv.Stream)
	expObj *testObj
	mode   tlv.ParseMode
}

var tlvTests = []tlvTestCase{
	{
		name: "empty",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		expObj: &testObj{},
	},
	{
		name: "empty sentinel",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakeSentinelRecord(),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		expObj: &testObj{},
	},
	{
		name: "partial encode",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{
				u32: 3,
				buf: bytes.Repeat([]byte{0x05}, 3),
			}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		expObj: &testObj{
			u32: 3,
			buf: bytes.Repeat([]byte{0x05}, 3),
		},
	},
	{
		name: "partial encode sentinel",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{
				u32: 3,
				buf: bytes.Repeat([]byte{0x05}, 3),
			}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(3, &o.buf),
				tlv.MakeSentinelRecord(),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		expObj: &testObj{
			u32: 3,
			buf: bytes.Repeat([]byte{0x05}, 3),
		},
	},
	{
		name: "partial decode",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{
				u16: 2,
				u32: 5,
				u64: 7,
				buf: bytes.Repeat([]byte{0x05}, 3),
			}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(2, &o.u64),
			)
			return o, s
		},
		expObj: &testObj{
			u16: 2,
			u64: 7,
		},
	},
	{
		name: "partial decode sentinel",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{
				u16: 2,
				u32: 5,
				u64: 7,
				buf: bytes.Repeat([]byte{0x05}, 3),
			}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
				tlv.MakeSentinelRecord(),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(2, &o.u64),
			)
			return o, s
		},
		expObj: &testObj{
			u16: 2,
			u64: 7,
		},
	},
	{
		name: "partial decode retain",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{
				u16: 2,
				u32: 5,
				u64: 7,
				buf: bytes.Repeat([]byte{0x05}, 3),
			}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
				tlv.MakeSentinelRecord(),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(2, &o.u64),
			)
			return o, s
		},
		expObj: &testObj{
			u16: 2,
			u64: 7,
		},
		mode: tlv.ParseModeRetain,
	},
	{
		name: "full decode encode",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{
				u16: 2,
				u32: 3,
				u64: 7,
				buf: bytes.Repeat([]byte{0x05}, 3),
			}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		expObj: &testObj{
			u16: 2,
			u32: 3,
			u64: 7,
			buf: bytes.Repeat([]byte{0x05}, 3),
		},
	},
	{
		name: "full encode decode sentinel",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{
				u16: 2,
				u32: 3,
				u64: 7,
				buf: bytes.Repeat([]byte{0x05}, 3),
			}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
				tlv.MakeSentinelRecord(),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		expObj: &testObj{
			u16: 2,
			u32: 3,
			u64: 7,
			buf: bytes.Repeat([]byte{0x05}, 3),
		},
	},
	{
		name: "full encode decode sentinel retain",
		encGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{
				u16: 2,
				u32: 3,
				u64: 7,
				buf: bytes.Repeat([]byte{0x05}, 3),
			}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
				tlv.MakeSentinelRecord(),
			)
			return o, s
		},
		decGen: func() (*testObj, *tlv.Stream) {
			o := &testObj{}
			s := tlv.NewStream(
				wtwire.WriteElement, wtwire.ReadElement,
				tlv.MakePrimitiveRecord(0, &o.u16),
				tlv.MakePrimitiveRecord(1, &o.u32),
				tlv.MakePrimitiveRecord(2, &o.u64),
				tlv.MakePrimitiveRecord(3, &o.buf),
			)
			return o, s
		},
		expObj: &testObj{
			u16: 2,
			u32: 3,
			u64: 7,
			buf: bytes.Repeat([]byte{0x05}, 3),
		},
		mode: tlv.ParseModeRetain,
	},
}

func TestTLV(t *testing.T) {
	for _, test := range tlvTests {
		t.Run(test.name, func(t *testing.T) {
			testTLV(t, test)
		})
	}
}

func testTLV(t *testing.T, test tlvTestCase) {
	_, s1 := test.encGen()

	var b bytes.Buffer
	err := s1.Encode(&b)
	if err != nil {
		t.Fatalf("unable to encode tlv stream: %v", err)
	}

	obj2, s2 := test.decGen()
	err = s2.Decode(bytes.NewReader(b.Bytes()), test.mode)
	if err != nil {
		t.Fatalf("unable to decode tlv stream: %v", err)
	}

	if !reflect.DeepEqual(test.expObj, obj2) {
		t.Fatalf("mismatch object, want: %v, got: %v",
			test.expObj, obj2)
	}

	if test.mode == tlv.ParseModeDiscard {
		return
	}

	var b2 bytes.Buffer
	err = s2.Encode(&b2)
	if err != nil {
		t.Fatalf("unable to encode retained tlv stream: %v", err)
	}

	if bytes.Compare(b2.Bytes(), b.Bytes()) != 0 {
		t.Fatalf("mismatch retained bytes, want: %x, got: %x",
			b.Bytes(), b2.Bytes())
	}
}

type thing struct {
	known uint32

	field1   uint32
	field9   uint32
	field10  uint32
	field100 uint32
	field101 []byte

	tlvStream *tlv.Stream
}

type CreateSessionTLV struct {
	BlobType     blob.Type
	MaxUpdates   uint16
	RewardBase   uint32
	RewardRate   uint32
	SweepFeeRate lnwallet.SatPerKWeight

	tlvStream *tlv.Stream
}

func NewCreateSessionTLV() *CreateSessionTLV {
	m := &CreateSessionTLV{}
	m.tlvStream = tlv.NewStream(
		wtwire.WriteElement, wtwire.ReadElement,
		tlv.MakeStaticRecord(0, &m.BlobType, 2),
		tlv.MakePrimitiveRecord(1, &m.MaxUpdates),
		tlv.MakePrimitiveRecord(2, &m.RewardBase),
		tlv.MakePrimitiveRecord(3, &m.RewardRate),
		tlv.MakeStaticRecord(4, &m.SweepFeeRate, 8),
		tlv.MakeSentinelRecord(),
	)

	return m
}

func (c *CreateSessionTLV) Encode(w io.Writer) error {
	return c.tlvStream.Encode(w)
}

func (c *CreateSessionTLV) Decode(r io.Reader) error {
	return c.tlvStream.Decode(r, tlv.ParseModeDiscard)
}

func newThing() *thing {
	t := new(thing)

	t.tlvStream = tlv.NewStream(
		wtwire.WriteElement, wtwire.ReadElement,
		[]tlv.Record{
			tlv.MakePrimitiveRecord(1, &t.field1),
			tlv.MakePrimitiveRecord(9, &t.field9),
			tlv.MakePrimitiveRecord(10, &t.field10),
			tlv.MakePrimitiveRecord(100, &t.field100),
			tlv.MakePrimitiveRecord(101, &t.field101),
			tlv.MakeSentinelRecord(),
		}...,
	)

	return t
}

func (t *thing) Encode(w io.Writer) error {
	err := wtwire.WriteElement(w, t.known)
	if err != nil {
		return err
	}

	return t.tlvStream.Encode(w)
}

func (t *thing) EncodeNorm(w io.Writer) error {
	return wtwire.WriteElements(w,
		t.known,
		t.field1,
		t.field9,
		t.field10,
		t.field100,
		t.field101,
	)
}

func (t *thing) DecodeNorm(r io.Reader) error {
	return wtwire.ReadElements(r,
		&t.known,
		&t.field1,
		&t.field9,
		&t.field10,
		&t.field100,
		&t.field101,
	)
}
func (t *thing) Decode(r io.Reader) error {
	err := wtwire.ReadElement(r, &t.known, 0)
	if err != nil {
		return err
	}

	return t.tlvStream.Decode(r, tlv.ParseModeDiscard)
}

func BenchmarkEncodeTLV(t *testing.B) {
	ting := newThing()
	ting.field101 = bytes.Repeat([]byte{0xaa}, 32)

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		err = ting.Encode(ioutil.Discard)
	}
	_ = err
}

func BenchmarkEncode(t *testing.B) {
	ting := newThing()
	ting.field101 = bytes.Repeat([]byte{0xaa}, 32)

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		err = ting.EncodeNorm(ioutil.Discard)
	}
	_ = err
}

func BenchmarkDecodeTLV(t *testing.B) {
	ting := newThing()
	ting.field101 = bytes.Repeat([]byte{0xaa}, 32)

	var b bytes.Buffer
	ting.Encode(&b)
	r := bytes.NewReader(b.Bytes())

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		r.Seek(0, 0)
		err = ting.Decode(r)
	}
	_ = err
}

func BenchmarkDecode(t *testing.B) {
	ting := newThing()
	ting.field101 = bytes.Repeat([]byte{0xaa}, 32)

	var b bytes.Buffer
	ting.Encode(&b)
	r := bytes.NewReader(b.Bytes())

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		r.Seek(0, 0)
		err = ting.DecodeNorm(r)
	}
	_ = err
}

func BenchmarkEncodeCreateSession(t *testing.B) {
	m := &wtwire.CreateSession{}

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		err = m.Encode(ioutil.Discard, 0)
	}
	_ = err
}

func BenchmarkEncodeCreateSessionTLV(t *testing.B) {
	m := NewCreateSessionTLV()

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		err = m.Encode(ioutil.Discard)
	}
	_ = err
}

func BenchmarkDecodeCreateSession(t *testing.B) {
	m := &wtwire.CreateSession{}

	var b bytes.Buffer
	m.Encode(&b, 0)
	r := bytes.NewReader(b.Bytes())

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		r.Seek(0, 0)
		err = m.Decode(r, 0)
	}
	_ = err
}

func BenchmarkDecodeCreateSessionTLV(t *testing.B) {
	m := NewCreateSessionTLV()

	var b bytes.Buffer
	m.Encode(&b)
	r := bytes.NewReader(b.Bytes())

	t.ReportAllocs()
	t.ResetTimer()

	var err error
	for i := 0; i < t.N; i++ {
		r.Seek(0, 0)
		err = m.Decode(r)
	}
	_ = err
}

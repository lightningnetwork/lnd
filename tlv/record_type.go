package tlv

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"golang.org/x/exp/constraints"
)

// RecordT is a high-order type makes it easy to encode known "primitive" types
// as TLV records.
type RecordT[T TlvType, V any] struct {
	// recordType is the type of the TLV record.
	recordType T

	// val is the value of the underlying record. Go doesn't let us just
	// embed the type param as a struct field, so we need an intermediate
	// variable.
	Val V
}

// RecordProducerT is a type-aware wrapper around the normal RecordProducer
// interface.
type RecordProducerT[T any] interface {
	RecordProducer

	// This is a non-interface type constraint that allows us to pass a
	// concrete type as a type parameter rather than a pointer to the type
	// that satisfies the Record interface.
	*T
}

// NewRecordT creates a new RecordT type from a given RecordProducer type. This
// is useful to wrap a given record in this utility type, which also serves as
// an extra type annotation. The underlying type of the record is retained.
func NewRecordT[T TlvType, K any, V RecordProducerT[K]](
	record K,
) RecordT[T, K] {

	return RecordT[T, K]{
		Val: record,
	}
}

// Primitive is a type constraint that capture the set of "primitive" types,
// which are the built in stdlib types, and type defs of those types
type Primitive interface {
	constraints.Unsigned | ~[]byte | ~[32]byte | ~[33]byte | ~bool |
		~*btcec.PublicKey | ~[64]byte
}

// NewPrimitiveRecord creates a new RecordT type from a given primitive type.
func NewPrimitiveRecord[T TlvType, V Primitive](val V) RecordT[T, V] {
	return RecordT[T, V]{
		Val: val,
	}
}

// Record returns the underlying record interface for the record type.
func (t *RecordT[T, V]) Record() Record {
	// Go doesn't allow type assertions on a type param, so to work around
	// this, we'll convert to any, then do our type assertion.
	tlvRecord, ok := any(&t.Val).(RecordProducer)
	if !ok {
		return MakePrimitiveRecord(
			t.recordType.TypeVal(), &t.Val,
		)
	}

	// To enforce proper usage of the RecordT type, we'll make a wrapper
	// record that uses the proper internal type value.
	ogRecord := tlvRecord.Record()

	return Record{
		value:      ogRecord.value,
		typ:        t.recordType.TypeVal(),
		staticSize: ogRecord.staticSize,
		sizeFunc:   ogRecord.sizeFunc,
		encoder:    ogRecord.encoder,
		decoder:    ogRecord.decoder,
	}
}

// TlvType returns the type of the record. This is the value used to identify
// this type on the wire. This value is bound to the specified TlvType type
// param.
func (t *RecordT[T, V]) TlvType() Type {
	return t.recordType.TypeVal()
}

// Zero returns a zero value of the record type.
func (t *RecordT[T, V]) Zero() RecordT[T, V] {
	return ZeroRecordT[T, V]()
}

// OptionalRecordT is a high-order type that represents an optional TLV record.
// This can be used when a TLV record doesn't always need to be present (ok to
// be odd).
type OptionalRecordT[T TlvType, V any] struct {
	fn.Option[RecordT[T, V]]
}

// TlvType returns the type of the record. This is the value used to identify
// this type on the wire. This value is bound to the specified TlvType type
// param.
func (o *OptionalRecordT[T, V]) TlvType() Type {
	zeroRecord := ZeroRecordT[T, V]()
	return zeroRecord.TlvType()
}

// WhenSomeV executes the given function if the optional record is present.
// This operates on the inner most type, V, which is the value of the record.
func (o *OptionalRecordT[T, V]) WhenSomeV(f func(V)) {
	o.Option.WhenSome(func(r RecordT[T, V]) {
		f(r.Val)
	})
}

// UnwrapOrFailV is used to extract a value from an option within a test
// context. If the option is None, then the test fails. This gives the
// underlying value of the record, rather then the record itself.
func (o *OptionalRecordT[T, V]) UnwrapOrFailV(t *testing.T) V {
	inner := o.Option.UnwrapOrFail(t)

	return inner.Val
}

// UnwrapOrErrV is used to extract a value from an option, if the option is
// empty, then the specified error is returned directly. This gives the
// underlying value of the record, instead of the record itself.
func (o *OptionalRecordT[T, V]) UnwrapOrErrV(err error) (V, error) {
	var zero V

	inner, err := o.Option.UnwrapOrErr(err)
	if err != nil {
		return zero, err
	}

	return inner.Val, nil
}

// Zero returns a zero value of the record type.
func (o *OptionalRecordT[T, V]) Zero() RecordT[T, V] {
	return ZeroRecordT[T, V]()
}

// ValOpt returns an Option of the underlying value. This can be used to chain
// other option related methods to avoid needing to first go through the outer
// record.
func (o *OptionalRecordT[T, V]) ValOpt() fn.Option[V] {
	return fn.MapOption(func(record RecordT[T, V]) V {
		return record.Val
	})(o.Option)
}

// SomeRecordT creates a new OptionalRecordT type from a given RecordT type.
func SomeRecordT[T TlvType, V any](record RecordT[T, V]) OptionalRecordT[T, V] {
	return OptionalRecordT[T, V]{
		Option: fn.Some(record),
	}
}

// ZeroRecordT returns a zero value of the RecordT type.
func ZeroRecordT[T TlvType, V any]() RecordT[T, V] {
	var v V
	return RecordT[T, V]{
		Val: v,
	}
}

// BigSizeT is a high-order type that represents a TLV record that encodes an
// integer as a BigSize value in the stream.
type BigSizeT[T constraints.Integer] struct {
	// We'll store the base value in the struct as a uin64, but then expose
	// a public method to cast to the specified type.
	v uint64
}

// NewBigSizeT creates a new BigSizeT type from a given integer type.
func NewBigSizeT[T constraints.Integer](val T) BigSizeT[T] {
	return BigSizeT[T]{
		v: uint64(val),
	}
}

// Int returns the underlying integer value of the BigSize record.
func (b BigSizeT[T]) Int() T {
	return T(b.v)
}

// Record returns the underlying record interface for the record type.
func (b *BigSizeT[T]) Record() Record {
	// We use a zero value for the type here as this should be used with
	// the higher order RecordT type.
	return MakeBigSizeRecord(0, &b.v)
}

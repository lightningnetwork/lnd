package trampoline

import (
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// ErrorPathDBType is a database-only TLV record used to persist the
	// trampoline error path with a payment attempt. Types below the custom
	// range cannot collide with user-supplied custom records.
	ErrorPathDBType tlv.Type = 1002

	// ErrorSessionKeyDBType is a database-only TLV record used to persist
	// an explicitly supplied trampoline error session key. Automatically
	// constructed onions derive their error key from the outer attempt key
	// and therefore do not need this record.
	ErrorSessionKeyDBType tlv.Type = 1003
)

var (
	// ErrInvalidErrorPathEncoding is returned for malformed persisted error
	// path records.
	ErrInvalidErrorPathEncoding = errors.New(
		"invalid trampoline error path encoding",
	)

	// ErrInvalidErrorSessionKey is returned for malformed explicit error
	// session keys received over RPC or loaded from disk.
	ErrInvalidErrorSessionKey = errors.New(
		"invalid trampoline error session key",
	)
)

// ParseErrorSessionKey validates and parses an explicitly supplied trampoline
// error session key without reducing invalid scalars modulo the curve order.
func ParseErrorSessionKey(key []byte) (*btcec.PrivateKey, error) {
	if len(key) != btcec.PrivKeyBytesLen {
		return nil, fmt.Errorf(
			"%w: length %d, want %d", ErrInvalidErrorSessionKey,
			len(key), btcec.PrivKeyBytesLen,
		)
	}

	var scalar btcec.ModNScalar
	if overflow := scalar.SetByteSlice(key); overflow || scalar.IsZero() {
		return nil, ErrInvalidErrorSessionKey
	}

	return btcec.PrivKeyFromScalar(&scalar), nil
}

// NewErrorSessionKeyRecord creates the database-only explicit trampoline
// error session key record. The record is removed during deserialization and
// is never sent on the wire.
func NewErrorSessionKeyRecord(key *[btcec.PrivKeyBytesLen]byte) tlv.Record {
	encoder := func(w io.Writer, val interface{}, buf *[8]byte) error {
		target, ok := val.(*[btcec.PrivKeyBytesLen]byte)
		if !ok || target == nil {
			return tlv.NewTypeForEncodingErr(
				val, "*[32]byte",
			)
		}
		if _, err := ParseErrorSessionKey(target[:]); err != nil {
			return err
		}

		return tlv.EBytes32(w, target, buf)
	}

	decoder := func(r io.Reader, val interface{}, buf *[8]byte,
		l uint64) error {

		target, ok := val.(*[btcec.PrivKeyBytesLen]byte)
		if !ok || target == nil {
			return tlv.NewTypeForDecodingErr(
				val, "*[32]byte", l, btcec.PrivKeyBytesLen,
			)
		}
		if err := tlv.DBytes32(r, target, buf, l); err != nil {
			return err
		}
		_, err := ParseErrorSessionKey(target[:])

		return err
	}

	return tlv.MakeStaticRecord(
		ErrorSessionKeyDBType, key, btcec.PrivKeyBytesLen,
		encoder, decoder,
	)
}

// NewErrorPathRecord creates the database-only trampoline error path record.
// The record is removed during deserialization and is never sent on the wire.
func NewErrorPathRecord(path *[]*btcec.PublicKey) tlv.Record {
	size := func() uint64 {
		if path == nil {
			return 0
		}

		return uint64(len(*path) * btcec.PubKeyBytesLenCompressed)
	}

	encoder := func(w io.Writer, val interface{}, _ *[8]byte) error {
		target, ok := val.(*[]*btcec.PublicKey)
		if !ok || target == nil {
			return tlv.NewTypeForEncodingErr(
				val, "*[]*btcec.PublicKey",
			)
		}
		if len(*target) == 0 || len(*target) > sphinx.NumMaxHops {
			return fmt.Errorf("%w: hop count %d",
				ErrInvalidErrorPathEncoding, len(*target))
		}

		for i, node := range *target {
			if node == nil || !node.IsOnCurve() {
				return fmt.Errorf("%w: node %d",
					ErrInvalidErrorPathEncoding, i)
			}
			serialized := node.SerializeCompressed()
			if _, err := w.Write(serialized); err != nil {
				return err
			}
		}

		return nil
	}

	decoder := func(r io.Reader, val interface{}, _ *[8]byte,
		l uint64) error {

		target, ok := val.(*[]*btcec.PublicKey)
		if !ok || target == nil {
			maxLength := uint64(
				sphinx.NumMaxHops *
					btcec.PubKeyBytesLenCompressed,
			)

			return tlv.NewTypeForDecodingErr(
				val, "*[]*btcec.PublicKey", l,
				maxLength,
			)
		}
		if l == 0 || l%btcec.PubKeyBytesLenCompressed != 0 {
			return fmt.Errorf("%w: length %d",
				ErrInvalidErrorPathEncoding, l)
		}

		count := int(l / btcec.PubKeyBytesLenCompressed)
		if count > sphinx.NumMaxHops {
			return fmt.Errorf("%w: hop count %d",
				ErrInvalidErrorPathEncoding, count)
		}

		result := make([]*btcec.PublicKey, count)
		var encoded [btcec.PubKeyBytesLenCompressed]byte
		for i := range result {
			if _, err := io.ReadFull(r, encoded[:]); err != nil {
				return err
			}

			node, err := btcec.ParsePubKey(encoded[:])
			if err != nil {
				return fmt.Errorf("%w: node %d: %w",
					ErrInvalidErrorPathEncoding, i, err)
			}
			result[i] = node
		}
		*target = result

		return nil
	}

	return tlv.MakeDynamicRecord(
		ErrorPathDBType, path, size, encoder, decoder,
	)
}

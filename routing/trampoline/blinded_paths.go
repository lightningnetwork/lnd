package trampoline

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const maxRecipientBlindedPaths = 32

var (
	// ErrNoRecipientBlindedPaths is returned when an encoded type 22 record
	// contains no path.
	ErrNoRecipientBlindedPaths = errors.New("no recipient blinded paths")

	// ErrTooManyRecipientBlindedPaths is returned when a type 22 record
	// exceeds the local path-count resource bound.
	ErrTooManyRecipientBlindedPaths = errors.New(
		"too many recipient blinded paths",
	)
)

// RecipientBlindedPath pairs one BOLT 12 blinded path with the payment
// parameters needed to route through it.
type RecipientBlindedPath struct {
	// Path contains the blinded introduction point and encrypted hops.
	Path lnwire.BlindedPath

	// PaymentInfo contains the aggregate fee, CLTV, HTLC, and feature
	// constraints for Path.
	PaymentInfo bolt12.BlindedPayInfo
}

// RecipientBlindedPaths is the structured value of hop TLV type 22.
type RecipientBlindedPaths struct {
	// Paths contains each path immediately followed by its payment info.
	Paths []RecipientBlindedPath
}

// Encode serializes the concatenated payment_blinded_path subtypes.
func (r *RecipientBlindedPaths) Encode(w io.Writer) error {
	if r == nil || len(r.Paths) == 0 {
		return ErrNoRecipientBlindedPaths
	}
	if len(r.Paths) > maxRecipientBlindedPaths {
		return fmt.Errorf("%w: count=%d max=%d",
			ErrTooManyRecipientBlindedPaths, len(r.Paths),
			maxRecipientBlindedPaths)
	}

	for i := range r.Paths {
		path := &r.Paths[i]
		if err := path.Path.Encode(w); err != nil {
			return fmt.Errorf(
				"encode recipient path %d: %w", i, err,
			)
		}
		if err := path.PaymentInfo.Encode(w); err != nil {
			return fmt.Errorf("encode recipient payinfo %d: %w", i,
				err)
		}
	}

	return nil
}

// DecodeRecipientBlindedPaths parses a complete, bounded type 22 value.
func DecodeRecipientBlindedPaths(data []byte) (*RecipientBlindedPaths, error) {
	if len(data) == 0 {
		return nil, ErrNoRecipientBlindedPaths
	}
	if len(data) > tlv.MaxRecordSize {
		return nil, tlv.ErrRecordTooLarge
	}

	reader := bytes.NewReader(data)
	result := &RecipientBlindedPaths{
		Paths: make([]RecipientBlindedPath, 0, 1),
	}

	for reader.Len() > 0 {
		if len(result.Paths) >= maxRecipientBlindedPaths {
			return nil, ErrTooManyRecipientBlindedPaths
		}

		path, pathBytes, err := lnwire.DecodeBlindedPath(
			reader, uint64(reader.Len()),
		)
		if err != nil {
			return nil, fmt.Errorf("decode recipient path %d: %w",
				len(result.Paths), err)
		}
		if pathBytes == 0 {
			return nil, fmt.Errorf(
				"recipient path consumed no bytes",
			)
		}

		info, infoBytes, err := bolt12.DecodeBlindedPayInfo(
			reader, uint64(reader.Len()),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"decode recipient payinfo %d: %w",
				len(result.Paths), err,
			)
		}
		if infoBytes == 0 {
			return nil, errors.New(
				"recipient payinfo consumed no bytes",
			)
		}

		result.Paths = append(result.Paths, RecipientBlindedPath{
			Path:        *path,
			PaymentInfo: *info,
		})
	}

	return result, nil
}

// newRecipientBlindedPathsRecord creates the type 22 hop-payload codec.
func newRecipientBlindedPathsRecord(
	paths **RecipientBlindedPaths) tlv.Record {

	size := func() uint64 {
		if paths == nil || *paths == nil {
			return 0
		}

		var b bytes.Buffer
		if err := (*paths).Encode(&b); err != nil {
			return 0
		}

		return uint64(b.Len())
	}

	encoder := func(w io.Writer, val interface{}, _ *[8]byte) error {
		target, ok := val.(**RecipientBlindedPaths)
		if !ok || target == nil || *target == nil {
			return tlv.NewTypeForEncodingErr(
				val, "**trampoline.RecipientBlindedPaths",
			)
		}

		return (*target).Encode(w)
	}

	decoder := func(r io.Reader, val interface{}, _ *[8]byte,
		l uint64) error {

		target, ok := val.(**RecipientBlindedPaths)
		if !ok || target == nil {
			return tlv.NewTypeForDecodingErr(
				val, "**trampoline.RecipientBlindedPaths", l,
				tlv.MaxRecordSize,
			)
		}
		if l > tlv.MaxRecordSize {
			return tlv.ErrRecordTooLarge
		}

		data := make([]byte, l)
		if _, err := io.ReadFull(r, data); err != nil {
			return err
		}

		decoded, err := DecodeRecipientBlindedPaths(data)
		if err != nil {
			return err
		}

		*target = decoded

		return nil
	}

	return tlv.MakeDynamicRecord(
		RecipientBlindedPathsType, paths, size, encoder, decoder,
	)
}

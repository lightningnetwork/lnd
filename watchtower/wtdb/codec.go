package wtdb

import (
	"io"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
)

// UnknownElementType is an alias for channeldb.UnknownElementType.
type UnknownElementType = channeldb.UnknownElementType

// ReadElement deserializes a single element from the provided io.Reader.
func ReadElement(r io.Reader, element interface{}) error {
	err := channeldb.ReadElement(r, element)
	switch {

	// Known to channeldb codec.
	case err == nil:
		return nil

	// Fail if error is not UnknownElementType.
	default:
		if _, ok := err.(UnknownElementType); !ok {
			return err
		}
	}

	// Process any wtdb-specific extensions to the codec.
	switch e := element.(type) {

	case *SessionID:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case *blob.BreachHint:
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return err
		}

	case *wtpolicy.Policy:
		var (
			blobType     uint16
			sweepFeeRate uint64
		)
		err := channeldb.ReadElements(r,
			&blobType,
			&e.MaxUpdates,
			&e.RewardBase,
			&e.RewardRate,
			&sweepFeeRate,
		)
		if err != nil {
			return err
		}

		e.BlobType = blob.Type(blobType)
		e.SweepFeeRate = chainfee.SatPerKWeight(sweepFeeRate)

	// Type is still unknown to wtdb extensions, fail.
	default:
		return channeldb.NewUnknownElementType(
			"ReadElement", element,
		)
	}

	return nil
}

// WriteElement serializes a single element into the provided io.Writer.
func WriteElement(w io.Writer, element interface{}) error {
	err := channeldb.WriteElement(w, element)
	switch {

	// Known to channeldb codec.
	case err == nil:
		return nil

	// Fail if error is not UnknownElementType.
	default:
		if _, ok := err.(UnknownElementType); !ok {
			return err
		}
	}

	// Process any wtdb-specific extensions to the codec.
	switch e := element.(type) {

	case SessionID:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case blob.BreachHint:
		if _, err := w.Write(e[:]); err != nil {
			return err
		}

	case wtpolicy.Policy:
		return channeldb.WriteElements(w,
			uint16(e.BlobType),
			e.MaxUpdates,
			e.RewardBase,
			e.RewardRate,
			uint64(e.SweepFeeRate),
		)

	// Type is still unknown to wtdb extensions, fail.
	default:
		return channeldb.NewUnknownElementType(
			"WriteElement", element,
		)
	}

	return nil
}

// WriteElements serializes a variadic list of elements into the given
// io.Writer.
func WriteElements(w io.Writer, elements ...interface{}) error {
	for _, element := range elements {
		if err := WriteElement(w, element); err != nil {
			return err
		}
	}

	return nil
}

// ReadElements deserializes the provided io.Reader into a variadic list of
// target elements.
func ReadElements(r io.Reader, elements ...interface{}) error {
	for _, element := range elements {
		if err := ReadElement(r, element); err != nil {
			return err
		}
	}

	return nil
}

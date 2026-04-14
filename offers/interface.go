package offers

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrOfferNotFound is returned when no offer has the requested offer
	// hash.
	ErrOfferNotFound = errors.New("offer not found")

	// ErrOfferExists is returned when an offer with the same offer hash is
	// already stored.
	ErrOfferExists = errors.New("offer already exists")
)

// Offer represents a persisted BOLT 12 offer. It keeps the encoded offer and
// the local state that the offer string does not carry. Code that needs an
// offer field decodes Encoded.
type Offer struct {
	// ID is the database primary key.
	ID int64

	// Hash is the SHA256 hash of the TLV-encoded offer, used as a unique
	// external identifier.
	Hash [32]byte

	// Encoded is the full bech32-encoded offer string (lno1...).
	Encoded string

	// IsDisabled indicates the offer has been administratively disabled.
	IsDisabled bool

	// CreatedAt is the time the store persisted the offer.
	CreatedAt time.Time
}

// Store defines the interface for persisting and querying BOLT 12 offers.
type Store interface {
	// InsertOffer persists a new offer and returns its database ID. The
	// store sets the creation time, and a new offer is always enabled. On
	// success it also sets ID and CreatedAt on offer to the stored values.
	// It returns ErrOfferExists if an offer with the same offer hash is
	// already stored.
	//
	// The store does not decode or validate the offer. The caller passes
	// an offer that the bolt12 reader accepts, and Hash must be the offer
	// hash of the TLV records in Encoded. CreateOffer meets both
	// conditions.
	InsertOffer(ctx context.Context, offer *Offer) (int64, error)

	// GetOfferByHash retrieves an offer by its 32-byte offer hash. It
	// returns ErrOfferNotFound if no offer has that hash.
	GetOfferByHash(ctx context.Context, offerHash [32]byte) (*Offer, error)
}

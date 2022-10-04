package migration1

import (
	"encoding/binary"
	"io"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtpolicy"
)

// SessionIDSize is 33-bytes; it is a serialized, compressed public key.
const SessionIDSize = 33

// UnknownElementType is an alias for channeldb.UnknownElementType.
type UnknownElementType = channeldb.UnknownElementType

// SessionID is created from the remote public key of a client, and serves as a
// unique identifier and authentication for sending state updates.
type SessionID [SessionIDSize]byte

// TowerID is a unique 64-bit identifier allocated to each unique watchtower.
// This allows the client to conserve on-disk space by not needing to always
// reference towers by their pubkey.
type TowerID uint64

// Bytes encodes a TowerID into an 8-byte slice in big-endian byte order.
func (id TowerID) Bytes() []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(id))
	return buf[:]
}

// ClientSession encapsulates a SessionInfo returned from a successful
// session negotiation, and also records the tower and ephemeral secret used for
// communicating with the tower.
type ClientSession struct {
	// ID is the client's public key used when authenticating with the
	// tower.
	ID SessionID
	ClientSessionBody
}

// CSessionStatus is a bit-field representing the possible statuses of
// ClientSessions.
type CSessionStatus uint8

type ClientSessionBody struct {
	// SeqNum is the next unallocated sequence number that can be sent to
	// the tower.
	SeqNum uint16

	// TowerLastApplied the last last-applied the tower has echoed back.
	TowerLastApplied uint16

	// TowerID is the unique, db-assigned identifier that references the
	// Tower with which the session is negotiated.
	TowerID TowerID

	// KeyIndex is the index of key locator used to derive the client's
	// session key so that it can authenticate with the tower to update its
	// session. In order to rederive the private key, the key locator should
	// use the keychain.KeyFamilyTowerSession key family.
	KeyIndex uint32

	// Policy holds the negotiated session parameters.
	Policy wtpolicy.Policy

	// Status indicates the current state of the ClientSession.
	Status CSessionStatus

	// RewardPkScript is the pkscript that the tower's reward will be
	// deposited to if a sweep transaction confirms and the sessions
	// specifies a reward output.
	RewardPkScript []byte
}

// Encode writes a ClientSessionBody to the passed io.Writer.
func (s *ClientSessionBody) Encode(w io.Writer) error {
	return WriteElements(w,
		s.SeqNum,
		s.TowerLastApplied,
		uint64(s.TowerID),
		s.KeyIndex,
		uint8(s.Status),
		s.Policy,
		s.RewardPkScript,
	)
}

// Decode reads a ClientSessionBody from the passed io.Reader.
func (s *ClientSessionBody) Decode(r io.Reader) error {
	var (
		towerID uint64
		status  uint8
	)
	err := ReadElements(r,
		&s.SeqNum,
		&s.TowerLastApplied,
		&towerID,
		&s.KeyIndex,
		&status,
		&s.Policy,
		&s.RewardPkScript,
	)
	if err != nil {
		return err
	}

	s.TowerID = TowerID(towerID)
	s.Status = CSessionStatus(status)

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

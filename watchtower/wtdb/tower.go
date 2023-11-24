package wtdb

import (
	"encoding/hex"
	"fmt"
	"io"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// TowerStatus represents the state of the tower as set by the tower client.
type TowerStatus uint8

const (
	// TowerStatusActive is the default state of the tower, and it indicates
	// that this tower should be used to attempt session creation.
	TowerStatusActive TowerStatus = 0

	// TowerStatusInactive indicates that the tower should not be used to
	// attempt session creation.
	TowerStatusInactive TowerStatus = 1
)

const (
	// TowerStatusTLVType is the TLV type number that will be used to store
	// the tower's status.
	TowerStatusTLVType = tlv.Type(0)
)

// TowerID is a unique 64-bit identifier allocated to each unique watchtower.
// This allows the client to conserve on-disk space by not needing to always
// reference towers by their pubkey.
type TowerID uint64

// TowerIDFromBytes constructs a TowerID from the provided byte slice. The
// argument must have at least 8 bytes, and should contain the TowerID in
// big-endian byte order.
func TowerIDFromBytes(towerIDBytes []byte) TowerID {
	return TowerID(byteOrder.Uint64(towerIDBytes))
}

// Bytes encodes a TowerID into an 8-byte slice in big-endian byte order.
func (id TowerID) Bytes() []byte {
	var buf [8]byte
	byteOrder.PutUint64(buf[:], uint64(id))
	return buf[:]
}

// Tower holds the necessary components required to connect to a remote tower.
// Communication is handled by brontide, and requires both a public key and an
// address.
type Tower struct {
	// ID is a unique ID for this record assigned by the database.
	ID TowerID

	// IdentityKey is the public key of the remote node, used to
	// authenticate the brontide transport.
	IdentityKey *btcec.PublicKey

	// Addresses is a list of possible addresses to reach the tower.
	Addresses []net.Addr

	// Status is the status of this tower as set by the client.
	Status TowerStatus
}

// AddAddress adds the given address to the tower's in-memory list of addresses.
// If the address's string is already present, the Tower will be left
// unmodified. Otherwise, the address is prepended to the beginning of the
// Tower's addresses, on the assumption that it is fresher than the others.
//
// NOTE: This method is NOT safe for concurrent use.
func (t *Tower) AddAddress(addr net.Addr) {
	// Ensure we don't add a duplicate address.
	addrStr := addr.String()
	for _, existingAddr := range t.Addresses {
		if existingAddr.String() == addrStr {
			return
		}
	}

	// Add this address to the front of the list, on the assumption that it
	// is a fresher address and will be tried first.
	t.Addresses = append([]net.Addr{addr}, t.Addresses...)
}

// RemoveAddress removes the given address from the tower's in-memory list of
// addresses. If the address doesn't exist, then this will act as a NOP.
func (t *Tower) RemoveAddress(addr net.Addr) {
	addrStr := addr.String()
	for i, address := range t.Addresses {
		if address.String() != addrStr {
			continue
		}
		t.Addresses = append(t.Addresses[:i], t.Addresses[i+1:]...)
		return
	}
}

// String returns a user-friendly identifier of the tower.
func (t *Tower) String() string {
	pubKey := hex.EncodeToString(t.IdentityKey.SerializeCompressed())
	if len(t.Addresses) == 0 {
		return pubKey
	}
	return fmt.Sprintf("%v@%v", pubKey, t.Addresses[0])
}

// Encode writes the Tower to the passed io.Writer. The TowerID is not
// serialized, since it acts as the key.
func (t *Tower) Encode(w io.Writer) error {
	err := WriteElements(w,
		t.IdentityKey,
		t.Addresses,
	)
	if err != nil {
		return err
	}

	status := uint8(t.Status)
	tlvRecords := []tlv.Record{
		tlv.MakePrimitiveRecord(TowerStatusTLVType, &status),
	}

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// Decode reads a Tower from the passed io.Reader. The TowerID is meant to be
// decoded from the key.
func (t *Tower) Decode(r io.Reader) error {
	err := ReadElements(r,
		&t.IdentityKey,
		&t.Addresses,
	)
	if err != nil {
		return err
	}

	var status uint8
	tlvRecords := []tlv.Record{
		tlv.MakePrimitiveRecord(TowerStatusTLVType, &status),
	}

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	typeMap, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := typeMap[TowerStatusTLVType]; ok {
		t.Status = TowerStatus(status)
	}

	return nil
}

package wtdb

import (
	"encoding/hex"
	"fmt"
	"io"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnwire"
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

// LNAddrs generates a list of lnwire.NetAddress from a Tower instance's
// addresses. This can be used to have a client try multiple addresses for the
// same Tower.
//
// NOTE: This method is NOT safe for concurrent use.
func (t *Tower) LNAddrs() []*lnwire.NetAddress {
	addrs := make([]*lnwire.NetAddress, 0, len(t.Addresses))
	for _, addr := range t.Addresses {
		addrs = append(addrs, &lnwire.NetAddress{
			IdentityKey: t.IdentityKey,
			Address:     addr,
		})
	}

	return addrs
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
	return WriteElements(w,
		t.IdentityKey,
		t.Addresses,
	)
}

// Decode reads a Tower from the passed io.Reader. The TowerID is meant to be
// decoded from the key.
func (t *Tower) Decode(r io.Reader) error {
	return ReadElements(r,
		&t.IdentityKey,
		&t.Addresses,
	)
}

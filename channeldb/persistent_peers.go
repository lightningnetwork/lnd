package channeldb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// persistentPeersBucket is the name of a top level bucket in which we
	// store the set of peers that the user has explicitly requested to
	// remain connected to across LND restarts (e.g. via
	// `lncli connect --perm`). Entries are keyed by the peer's compressed
	// pubkey and the value is the serialized list of addresses we should
	// try when (re)connecting.
	persistentPeersBucket = []byte("persistent-peers-bucket")

	// ErrPersistentPeerNotFound is returned when no entry exists in the
	// persistent peers bucket for the requested pubkey.
	ErrPersistentPeerNotFound = errors.New("persistent peer not found")
)

// PersistentPeer represents a peer that the user has explicitly marked as
// persistent. Its addresses are stored so that the connection can be
// re-established when LND restarts.
type PersistentPeer struct {
	// PubKey is the identity public key of the peer.
	PubKey *btcec.PublicKey

	// Addresses is the set of addresses that we will attempt to dial when
	// (re)connecting to the peer.
	Addresses []net.Addr
}

// AddPersistentPeer unions the given addresses with whatever is already
// stored for the peer, deduping by string. Calling this multiple times for
// the same peer is additive: each call only adds addresses, never removes
// them. Use DeletePersistentPeer to fully forget a peer. The boolean return
// is true if at least one of the given addresses was newly added (i.e. the
// stored set grew); false if everything in addrs was already present.
func (d *DB) AddPersistentPeer(pubKey *btcec.PublicKey,
	addrs []net.Addr) (bool, error) {

	var addedNew bool
	err := kvdb.Update(d, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(persistentPeersBucket)
		if err != nil {
			return err
		}

		key := pubKey.SerializeCompressed()

		// Read any existing entry so we can union with it.
		existing := make([]net.Addr, 0)
		if v := bucket.Get(key); v != nil {
			existing, err = deserializeAddresses(
				bytes.NewReader(v),
			)
			if err != nil {
				return fmt.Errorf("unable to read existing "+
					"persistent peer entry: %w", err)
			}
		}

		seen := make(map[string]struct{}, len(existing)+len(addrs))
		merged := make([]net.Addr, 0, len(existing)+len(addrs))
		for _, a := range existing {
			if _, ok := seen[a.String()]; ok {
				continue
			}
			seen[a.String()] = struct{}{}
			merged = append(merged, a)
		}
		addedNew = false
		for _, a := range addrs {
			if _, ok := seen[a.String()]; ok {
				continue
			}
			seen[a.String()] = struct{}{}
			merged = append(merged, a)
			addedNew = true
		}

		var b bytes.Buffer
		if err := serializeAddresses(&b, merged); err != nil {
			return err
		}

		return bucket.Put(key, b.Bytes())
	}, func() {
		addedNew = false
	})
	return addedNew, err
}

// DeletePersistentPeer removes the persistent peer entry for the given
// pubkey. If no entry exists for the pubkey, this is a no-op.
func (d *DB) DeletePersistentPeer(pubKey *btcec.PublicKey) error {
	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(persistentPeersBucket)
		if bucket == nil {
			return nil
		}

		return bucket.Delete(pubKey.SerializeCompressed())
	}, func() {})
}

// FetchAllPersistentPeers returns all persistent peers that the user has
// previously asked LND to keep connected to.
func (d *DB) FetchAllPersistentPeers() ([]*PersistentPeer, error) {
	var peers []*PersistentPeer
	err := kvdb.View(d, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(persistentPeersBucket)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {
			if v == nil {
				return nil
			}

			pubKey, err := btcec.ParsePubKey(k)
			if err != nil {
				return fmt.Errorf("unable to parse "+
					"persistent peer pubkey: %w", err)
			}

			addrs, err := deserializeAddresses(bytes.NewReader(v))
			if err != nil {
				return fmt.Errorf("unable to deserialize "+
					"persistent peer addresses: %w", err)
			}

			peers = append(peers, &PersistentPeer{
				PubKey:    pubKey,
				Addresses: addrs,
			})
			return nil
		})
	}, func() {
		peers = nil
	})
	if err != nil {
		return nil, err
	}

	return peers, nil
}

// serializeAddresses writes the given list of addresses to w using the
// graphdb address serialization helpers.
func serializeAddresses(w io.Writer, addrs []net.Addr) error {
	var buf [4]byte
	byteOrder.PutUint32(buf[:], uint32(len(addrs)))
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}

	for _, addr := range addrs {
		if err := graphdb.SerializeAddr(w, addr); err != nil {
			return err
		}
	}

	return nil
}

// deserializeAddresses reads a list of addresses from r in the format
// written by serializeAddresses.
func deserializeAddresses(r io.Reader) ([]net.Addr, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}
	numAddrs := byteOrder.Uint32(buf[:])

	addrs := make([]net.Addr, 0, numAddrs)
	for i := uint32(0); i < numAddrs; i++ {
		addr, err := graphdb.DeserializeAddr(r)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, addr)
	}

	return addrs, nil
}

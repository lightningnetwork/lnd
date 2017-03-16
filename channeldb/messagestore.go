package channeldb

import (
	"bytes"
	"encoding/binary"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/wire"
)

var (
	// basePeerBucketKey is the base key for generating peer bucket keys.
	// Peer bucket stores top-level bucket that maps: index -> code || msg
	// concatenating the message code to the stored data allows the db logic
	// to properly parse the wire message without trial and error or needing
	// an additional index.
	basePeerBucketKey = []byte("peermessagesstore")
)

// MessageStore represents the storage for lnwire messages, it might be boltd storage,
// local storage, test storage or hybrid one.
//
// NOTE: The original purpose of creating this interface was in separation
// between the retransmission logic and specifics of storage itself. In case of
// such interface we may create the test storage and register the add,
// remove, get actions without the need of population of lnwire messages with
// data.
type MessageStore interface {
	// Get returns the sorted set of messages in the order they have been
	// added originally and also the array of associated index to this
	// messages within the message store.
	Get() ([]uint64, []lnwire.Message, error)

	// Add adds new message in the storage with preserving the order and
	// returns the index of message within the message store.
	Add(msg lnwire.Message) (uint64, error)

	// Remove deletes message with this indexes.
	Remove(indexes []uint64) error
}

// messagesStore represents the boltdb storage for messages inside
// retransmission sub-system.
type messagesStore struct {
	// id is a unique slice of bytes identifying a peer. This value is
	// typically a peer's identity public key serialized in compressed
	// format.
	id []byte
	db *DB
}

// NewMessageStore creates new instance of message storage.
func NewMessageStore(id []byte, db *DB) MessageStore {
	return &messagesStore{
		id: id,
		db: db,
	}
}

// Add adds message to the storage and returns the index which
// corresponds the the message by which it might be removed later.
func (s *messagesStore) Add(msg lnwire.Message) (uint64, error) {
	var index uint64

	err := s.db.Batch(func(tx *bolt.Tx) error {
		var err error

		// Get or create the top peer bucket.
		peerBucketKey := s.getPeerBucketKey()
		peerBucket, err := tx.CreateBucketIfNotExists(peerBucketKey)
		if err != nil {
			return err
		}

		// Generate next index number to add it to the message code
		// bucket.
		index, err = peerBucket.NextSequence()
		if err != nil {
			return err
		}
		indexBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(indexBytes, index)

		// Encode the message and place it in the top bucket.
		var b bytes.Buffer
		_, err = lnwire.WriteMessage(&b, msg, 0, wire.MainNet)
		if err != nil {
			return err
		}

		return peerBucket.Put(indexBytes, b.Bytes())
	})

	return index, err
}

// Remove removes the message from storage by index that were assigned to
// message during its addition to the storage.
func (s *messagesStore) Remove(indexes []uint64) error {
	return s.db.Batch(func(tx *bolt.Tx) error {
		// Get or create the top peer bucket.
		peerBucketKey := s.getPeerBucketKey()
		peerBucket := tx.Bucket(peerBucketKey)
		if peerBucket == nil {
			return ErrPeerMessagesNotFound
		}

		// Retrieve the messages indexes with this type/code and
		// remove them from top peer bucket.
		for _, index := range indexes {
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], index)

			if err := peerBucket.Delete(key[:]); err != nil {
				return err
			}
		}

		return nil
	})
}

// Get retrieves messages from storage in the order in which they were
// originally added, proper order is needed for retransmission subsystem, as
// far this messages should be resent to remote peer in the same order as they
// were sent originally.
func (s *messagesStore) Get() ([]uint64, []lnwire.Message, error) {
	var messages []lnwire.Message
	var indexes []uint64

	if err := s.db.View(func(tx *bolt.Tx) error {
		peerBucketKey := s.getPeerBucketKey()
		peerBucket := tx.Bucket(peerBucketKey)
		if peerBucket == nil {
			return nil
		}

		// Iterate over messages buckets.
		return peerBucket.ForEach(func(k, v []byte) error {
			// Skip buckets fields.
			if v == nil {
				return nil
			}

			// Decode the message from and add it to the array.
			r := bytes.NewReader(v)
			_, msg, _, err := lnwire.ReadMessage(r, 0, wire.MainNet)
			if err != nil {
				return err
			}

			messages = append(messages, msg)
			indexes = append(indexes, binary.BigEndian.Uint64(k))
			return nil
		})
	}); err != nil {
		return nil, nil, err
	}

	// If bucket was haven't been created yet or just not contains any
	// messages.
	if len(messages) == 0 {
		return nil, nil, ErrPeerMessagesNotFound
	}

	return indexes, messages, nil
}

// getPeerBucketKey generates the peer bucket boltd key by peer id and base
// peer bucket key.
func (s *messagesStore) getPeerBucketKey() []byte {
	return append(basePeerBucketKey[:], s.id[:]...)
}

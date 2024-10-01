package discovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// messageStoreBucket is a key used to create a top level bucket in the
	// gossiper database, used for storing messages that are to be sent to
	// peers. Upon restarts, these messages will be read and resent to their
	// respective peers.
	//
	// maps:
	//   pubKey (33 bytes) + msgShortChanID (8 bytes) + msgType (2 bytes) -> msg
	messageStoreBucket = []byte("message-store")

	// ErrUnsupportedMessage is an error returned when we attempt to add a
	// message to the store that is not supported.
	ErrUnsupportedMessage = errors.New("unsupported message type")

	// ErrCorruptedMessageStore indicates that the on-disk bucketing
	// structure has altered since the gossip message store instance was
	// initialized.
	ErrCorruptedMessageStore = errors.New("gossip message store has been " +
		"corrupted")
)

// GossipMessageStore is a store responsible for storing gossip messages which
// we should reliably send to our peers.
type GossipMessageStore interface {
	// AddMessage adds a message to the store for this peer.
	AddMessage(lnwire.Message, [33]byte) error

	// DeleteMessage deletes a message from the store for this peer.
	DeleteMessage(lnwire.Message, [33]byte) error

	// Messages returns the total set of messages that exist within the
	// store for all peers.
	Messages() (map[[33]byte][]lnwire.Message, error)

	// Peers returns the public key of all peers with messages within the
	// store.
	Peers() (map[[33]byte]struct{}, error)

	// MessagesForPeer returns the set of messages that exists within the
	// store for the given peer.
	MessagesForPeer([33]byte) ([]lnwire.Message, error)
}

// MessageStore is an implementation of the GossipMessageStore interface backed
// by a channeldb instance. By design, this store will only keep the latest
// version of a message (like in the case of multiple ChannelUpdate's) for a
// channel with a peer.
type MessageStore struct {
	db kvdb.Backend
}

// A compile-time assertion to ensure messageStore implements the
// GossipMessageStore interface.
var _ GossipMessageStore = (*MessageStore)(nil)

// NewMessageStore creates a new message store backed by a channeldb instance.
func NewMessageStore(db kvdb.Backend) (*MessageStore, error) {
	err := kvdb.Batch(db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(messageStoreBucket)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("unable to create required buckets: %w",
			err)
	}

	return &MessageStore{db}, nil
}

// msgShortChanID retrieves the short channel ID of the message.
func msgShortChanID(msg lnwire.Message) (lnwire.ShortChannelID, error) {
	var shortChanID lnwire.ShortChannelID
	switch msg := msg.(type) {
	case *lnwire.AnnounceSignatures1:
		shortChanID = msg.ShortChannelID
	case *lnwire.ChannelUpdate1:
		shortChanID = msg.ShortChannelID
	default:
		return shortChanID, ErrUnsupportedMessage
	}

	return shortChanID, nil
}

// messageStoreKey constructs the database key for the message to be stored.
func messageStoreKey(msg lnwire.Message, peerPubKey [33]byte) ([]byte, error) {
	shortChanID, err := msgShortChanID(msg)
	if err != nil {
		return nil, err
	}

	var k [33 + 8 + 2]byte
	copy(k[:33], peerPubKey[:])
	binary.BigEndian.PutUint64(k[33:41], shortChanID.ToUint64())
	binary.BigEndian.PutUint16(k[41:43], uint16(msg.MsgType()))

	return k[:], nil
}

// AddMessage adds a message to the store for this peer.
func (s *MessageStore) AddMessage(msg lnwire.Message, peerPubKey [33]byte) error {
	log.Tracef("Adding message of type %v to store for peer %x",
		msg.MsgType(), peerPubKey)

	// Construct the key for which we'll find this message with in the
	// store.
	msgKey, err := messageStoreKey(msg, peerPubKey)
	if err != nil {
		return err
	}

	// Serialize the message with its wire encoding.
	var b bytes.Buffer
	if _, err := lnwire.WriteMessage(&b, msg, 0); err != nil {
		return err
	}

	return kvdb.Batch(s.db, func(tx kvdb.RwTx) error {
		messageStore := tx.ReadWriteBucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		return messageStore.Put(msgKey, b.Bytes())
	})
}

// DeleteMessage deletes a message from the store for this peer.
func (s *MessageStore) DeleteMessage(msg lnwire.Message,
	peerPubKey [33]byte) error {

	log.Tracef("Deleting message of type %v from store for peer %x",
		msg.MsgType(), peerPubKey)

	// Construct the key for which we'll find this message with in the
	// store.
	msgKey, err := messageStoreKey(msg, peerPubKey)
	if err != nil {
		return err
	}

	return kvdb.Batch(s.db, func(tx kvdb.RwTx) error {
		messageStore := tx.ReadWriteBucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		// In the event that we're attempting to delete a ChannelUpdate
		// from the store, we'll make sure that we're actually deleting
		// the correct one as it can be overwritten.
		if msg, ok := msg.(*lnwire.ChannelUpdate1); ok {
			// Deleting a value from a bucket that doesn't exist
			// acts as a NOP, so we'll return if a message doesn't
			// exist under this key.
			v := messageStore.Get(msgKey)
			if v == nil {
				return nil
			}

			dbMsg, err := lnwire.ReadMessage(bytes.NewReader(v), 0)
			if err != nil {
				return err
			}

			// If the timestamps don't match, then the update stored
			// should be the latest one, so we'll avoid deleting it.
			m, ok := dbMsg.(*lnwire.ChannelUpdate1)
			if !ok {
				return fmt.Errorf("expected "+
					"*lnwire.ChannelUpdate1, got: %T",
					dbMsg)
			}
			if msg.Timestamp != m.Timestamp {
				return nil
			}
		}

		return messageStore.Delete(msgKey)
	})
}

// readMessage reads a message from its serialized form and ensures its
// supported by the current version of the message store.
func readMessage(msgBytes []byte) (lnwire.Message, error) {
	msg, err := lnwire.ReadMessage(bytes.NewReader(msgBytes), 0)
	if err != nil {
		return nil, err
	}

	// Check if the message is supported by the store. We can reuse the
	// check for ShortChannelID as its a dependency on messages stored.
	if _, err := msgShortChanID(msg); err != nil {
		return nil, err
	}

	return msg, nil
}

// Messages returns the total set of messages that exist within the store for
// all peers.
func (s *MessageStore) Messages() (map[[33]byte][]lnwire.Message, error) {
	var msgs map[[33]byte][]lnwire.Message
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		messageStore := tx.ReadBucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		return messageStore.ForEach(func(k, v []byte) error {
			var pubKey [33]byte
			copy(pubKey[:], k[:33])

			// Deserialize the message from its raw bytes and filter
			// out any which are not currently supported by the
			// store.
			msg, err := readMessage(v)
			if err == ErrUnsupportedMessage {
				return nil
			}
			if err != nil {
				return err
			}

			msgs[pubKey] = append(msgs[pubKey], msg)
			return nil
		})
	}, func() {
		msgs = make(map[[33]byte][]lnwire.Message)
	})
	if err != nil {
		return nil, err
	}

	return msgs, nil
}

// MessagesForPeer returns the set of messages that exists within the store for
// the given peer.
func (s *MessageStore) MessagesForPeer(
	peerPubKey [33]byte) ([]lnwire.Message, error) {

	var msgs []lnwire.Message
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		messageStore := tx.ReadBucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		c := messageStore.ReadCursor()
		k, v := c.Seek(peerPubKey[:])
		for ; bytes.HasPrefix(k, peerPubKey[:]); k, v = c.Next() {
			// Deserialize the message from its raw bytes and filter
			// out any which are not currently supported by the
			// store.
			msg, err := readMessage(v)
			if err == ErrUnsupportedMessage {
				continue
			}
			if err != nil {
				return err
			}

			msgs = append(msgs, msg)
		}

		return nil
	}, func() {
		msgs = nil
	})
	if err != nil {
		return nil, err
	}

	return msgs, nil
}

// Peers returns the public key of all peers with messages within the store.
func (s *MessageStore) Peers() (map[[33]byte]struct{}, error) {
	var peers map[[33]byte]struct{}
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		messageStore := tx.ReadBucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		return messageStore.ForEach(func(k, _ []byte) error {
			var pubKey [33]byte
			copy(pubKey[:], k[:33])
			peers[pubKey] = struct{}{}
			return nil
		})
	}, func() {
		peers = make(map[[33]byte]struct{})
	})
	if err != nil {
		return nil, err
	}

	return peers, nil
}

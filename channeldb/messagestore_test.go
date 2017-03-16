package channeldb

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"reflect"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
)

// TestRetransmitterMessage tests the ability of message storage to
// add/remove/get messages and also checks that the order in which the
// messages had been added corresponds to the order of messages from Get
// function.
func TestRetransmitterMessageOrder(t *testing.T) {
	db, clean, err := makeTestDB()
	if err != nil {
		t.Fatal(err)
	}
	defer clean()
	s := NewMessageStore([]byte("id"), db)

	var (
		hash1, _ = chainhash.NewHash(bytes.Repeat([]byte("a"), 32))
		hash2, _ = chainhash.NewHash(bytes.Repeat([]byte("b"), 32))

		chanPoint1 = wire.NewOutPoint(hash1, 0)
		chanPoint2 = wire.NewOutPoint(hash2, 1)

		preimage1 = [sha256.Size]byte{0}
		preimage2 = [sha256.Size]byte{1}
	)

	// Check that we are receiving the error without messages inside
	// the storage.
	_, messages, err := s.Get()
	if err != nil && err != ErrPeerMessagesNotFound {
		t.Fatalf("can't get the message: %v", err)
	} else if len(messages) != 0 {
		t.Fatal("wrong length of messages")
	}

	msg1 := &lnwire.UpdateFufillHTLC{
		ChannelPoint:    *chanPoint1,
		ID:              0,
		PaymentPreimage: preimage1,
	}

	index1, err := s.Add(msg1)
	if err != nil {
		t.Fatalf("can't add the message to the message store: %v", err)
	}

	msg2 := &lnwire.UpdateFufillHTLC{
		ChannelPoint:    *chanPoint2,
		ID:              1,
		PaymentPreimage: preimage2,
	}

	index2, err := s.Add(msg2)
	if err != nil {
		t.Fatalf("can't add the message to the message store: %v", err)
	}

	_, messages, err = s.Get()
	if err != nil {
		t.Fatalf("can't get the message: %v", err)
	} else if len(messages) != 2 {
		t.Fatal("wrong length of messages")
	}

	m, ok := messages[0].(*lnwire.UpdateFufillHTLC)
	if !ok {
		t.Fatal("wrong type of message")
	}

	// Check the order
	if !reflect.DeepEqual(m, msg1) {
		t.Fatal("wrong order of message")
	}

	m, ok = messages[1].(*lnwire.UpdateFufillHTLC)
	if !ok {
		t.Fatal("wrong type of message")
	}

	// Check the order
	if !reflect.DeepEqual(m, msg2) {
		t.Fatal("wrong order of message")
	}

	// Remove the messages by index and check that get function return
	// non of the messages.
	if err := s.Remove([]uint64{index1, index2}); err != nil {
		t.Fatalf("can't remove the message: %v", err)
	}

	_, messages, err = s.Get()
	if err != nil && err != ErrPeerMessagesNotFound {
		t.Fatalf("can't get the message: %v", err)
	} else if len(messages) != 0 {
		t.Fatal("wrong length of messages")
	}
}

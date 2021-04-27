package discovery

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"os"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

func createTestMessageStore(t *testing.T) (*MessageStore, func()) {
	t.Helper()

	tempDir, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}
	db, err := channeldb.Open(tempDir)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("unable to open db: %v", err)
	}

	cleanUp := func() {
		db.Close()
		os.RemoveAll(tempDir)
	}

	store, err := NewMessageStore(db)
	if err != nil {
		cleanUp()
		t.Fatalf("unable to initialize message store: %v", err)
	}

	return store, cleanUp
}

func randPubKey(t *testing.T) *btcec.PublicKey {
	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to create private key: %v", err)
	}

	return priv.PubKey()
}

func randCompressedPubKey(t *testing.T) [33]byte {
	t.Helper()

	pubKey := randPubKey(t)

	var compressedPubKey [33]byte
	copy(compressedPubKey[:], pubKey.SerializeCompressed())

	return compressedPubKey
}

func randAnnounceSignatures() *lnwire.AnnounceSignatures {
	return &lnwire.AnnounceSignatures{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(rand.Uint64()),
		ExtraOpaqueData: make([]byte, 0),
	}
}

func randChannelUpdate() *lnwire.ChannelUpdate {
	return &lnwire.ChannelUpdate{
		ShortChannelID:  lnwire.NewShortChanIDFromInt(rand.Uint64()),
		ExtraOpaqueData: make([]byte, 0),
	}
}

// TestMessageStoreMessages ensures that messages can be properly queried from
// the store.
func TestMessageStoreMessages(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test message store.
	msgStore, cleanUp := createTestMessageStore(t)
	defer cleanUp()

	// We'll then create some test messages for two test peers, and none for
	// an additional test peer.
	channelUpdate1 := randChannelUpdate()
	announceSignatures1 := randAnnounceSignatures()
	peer1 := randCompressedPubKey(t)
	if err := msgStore.AddMessage(channelUpdate1, peer1); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	if err := msgStore.AddMessage(announceSignatures1, peer1); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	expectedPeerMsgs1 := map[uint64]lnwire.MessageType{
		channelUpdate1.ShortChannelID.ToUint64():      channelUpdate1.MsgType(),
		announceSignatures1.ShortChannelID.ToUint64(): announceSignatures1.MsgType(),
	}

	channelUpdate2 := randChannelUpdate()
	peer2 := randCompressedPubKey(t)
	if err := msgStore.AddMessage(channelUpdate2, peer2); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	expectedPeerMsgs2 := map[uint64]lnwire.MessageType{
		channelUpdate2.ShortChannelID.ToUint64(): channelUpdate2.MsgType(),
	}

	peer3 := randCompressedPubKey(t)
	expectedPeerMsgs3 := map[uint64]lnwire.MessageType{}

	// assertPeerMsgs is a helper closure that we'll use to ensure we
	// retrieve the correct set of messages for a given peer.
	assertPeerMsgs := func(peerMsgs []lnwire.Message,
		expected map[uint64]lnwire.MessageType) {

		t.Helper()

		if len(peerMsgs) != len(expected) {
			t.Fatalf("expected %d pending messages, got %d",
				len(expected), len(peerMsgs))
		}
		for _, msg := range peerMsgs {
			var shortChanID uint64
			switch msg := msg.(type) {
			case *lnwire.AnnounceSignatures:
				shortChanID = msg.ShortChannelID.ToUint64()
			case *lnwire.ChannelUpdate:
				shortChanID = msg.ShortChannelID.ToUint64()
			default:
				t.Fatalf("found unexpected message type %T", msg)
			}

			msgType, ok := expected[shortChanID]
			if !ok {
				t.Fatalf("retrieved message with unexpected ID "+
					"%d from store", shortChanID)
			}
			if msgType != msg.MsgType() {
				t.Fatalf("expected message of type %v, got %v",
					msg.MsgType(), msgType)
			}
		}
	}

	// Then, we'll query the store for the set of messages for each peer and
	// ensure it matches what we expect.
	peers := [][33]byte{peer1, peer2, peer3}
	expectedPeerMsgs := []map[uint64]lnwire.MessageType{
		expectedPeerMsgs1, expectedPeerMsgs2, expectedPeerMsgs3,
	}
	for i, peer := range peers {
		peerMsgs, err := msgStore.MessagesForPeer(peer)
		if err != nil {
			t.Fatalf("unable to retrieve messages: %v", err)
		}
		assertPeerMsgs(peerMsgs, expectedPeerMsgs[i])
	}

	// Finally, we'll query the store for all of its messages of every peer.
	// Again, each peer should have a set of messages that match what we
	// expect.
	//
	// We'll construct the expected response. Only the first two peers will
	// have messages.
	totalPeerMsgs := make(map[[33]byte]map[uint64]lnwire.MessageType, 2)
	for i := 0; i < 2; i++ {
		totalPeerMsgs[peers[i]] = expectedPeerMsgs[i]
	}

	msgs, err := msgStore.Messages()
	if err != nil {
		t.Fatalf("unable to retrieve all peers with pending messages: "+
			"%v", err)
	}
	if len(msgs) != len(totalPeerMsgs) {
		t.Fatalf("expected %d peers with messages, got %d",
			len(totalPeerMsgs), len(msgs))
	}
	for peer, peerMsgs := range msgs {
		expected, ok := totalPeerMsgs[peer]
		if !ok {
			t.Fatalf("expected to find pending messages for peer %x",
				peer)
		}

		assertPeerMsgs(peerMsgs, expected)
	}

	peerPubKeys, err := msgStore.Peers()
	if err != nil {
		t.Fatalf("unable to retrieve all peers with pending messages: "+
			"%v", err)
	}
	if len(peerPubKeys) != len(totalPeerMsgs) {
		t.Fatalf("expected %d peers with messages, got %d",
			len(totalPeerMsgs), len(peerPubKeys))
	}
	for peerPubKey := range peerPubKeys {
		if _, ok := totalPeerMsgs[peerPubKey]; !ok {
			t.Fatalf("expected to find peer %x", peerPubKey)
		}
	}
}

// TestMessageStoreUnsupportedMessage ensures that we are not able to add a
// message which is unsupported, and if a message is found to be unsupported by
// the current version of the store, that it is properly filtered out from the
// response.
func TestMessageStoreUnsupportedMessage(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test message store.
	msgStore, cleanUp := createTestMessageStore(t)
	defer cleanUp()

	// Create a message that is known to not be supported by the store.
	peer := randCompressedPubKey(t)
	unsupportedMsg := &lnwire.Error{}

	// Attempting to add it to the store should result in
	// ErrUnsupportedMessage.
	err := msgStore.AddMessage(unsupportedMsg, peer)
	if err != ErrUnsupportedMessage {
		t.Fatalf("expected ErrUnsupportedMessage, got %v", err)
	}

	// We'll now pretend that the message is actually supported in a future
	// version of the store, so it's able to be added successfully. To
	// replicate this, we'll add the message manually rather than through
	// the existing AddMessage method.
	msgKey := peer[:]
	var rawMsg bytes.Buffer
	if _, err := lnwire.WriteMessage(&rawMsg, unsupportedMsg, 0); err != nil {
		t.Fatalf("unable to serialize message: %v", err)
	}
	err = kvdb.Update(msgStore.db, func(tx kvdb.RwTx) error {
		messageStore := tx.ReadWriteBucket(messageStoreBucket)
		return messageStore.Put(msgKey, rawMsg.Bytes())
	}, func() {})
	if err != nil {
		t.Fatalf("unable to add unsupported message to store: %v", err)
	}

	// Finally, we'll check that the store can properly filter out messages
	// that are currently unknown to it. We'll make sure this is done for
	// both Messages and MessagesForPeer.
	totalMsgs, err := msgStore.Messages()
	if err != nil {
		t.Fatalf("unable to retrieve messages: %v", err)
	}
	if len(totalMsgs) != 0 {
		t.Fatalf("expected to filter out unsupported message")
	}
	peerMsgs, err := msgStore.MessagesForPeer(peer)
	if err != nil {
		t.Fatalf("unable to retrieve peer messages: %v", err)
	}
	if len(peerMsgs) != 0 {
		t.Fatalf("expected to filter out unsupported message")
	}
}

// TestMessageStoreDeleteMessage ensures that we can properly delete messages
// from the store.
func TestMessageStoreDeleteMessage(t *testing.T) {
	t.Parallel()

	msgStore, cleanUp := createTestMessageStore(t)
	defer cleanUp()

	// assertMsg is a helper closure we'll use to ensure a message
	// does/doesn't exist within the store.
	assertMsg := func(msg lnwire.Message, peer [33]byte, exists bool) {
		t.Helper()

		storeMsgs, err := msgStore.MessagesForPeer(peer)
		if err != nil {
			t.Fatalf("unable to retrieve messages: %v", err)
		}

		found := false
		for _, storeMsg := range storeMsgs {
			if reflect.DeepEqual(msg, storeMsg) {
				found = true
			}
		}

		if found != exists {
			str := "find"
			if !exists {
				str = "not find"
			}
			t.Fatalf("expected to %v message %v", str,
				spew.Sdump(msg))
		}
	}

	// An AnnounceSignatures message should exist within the store after
	// adding it, and should no longer exists after deleting it.
	peer := randCompressedPubKey(t)
	annSig := randAnnounceSignatures()
	if err := msgStore.AddMessage(annSig, peer); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	assertMsg(annSig, peer, true)
	if err := msgStore.DeleteMessage(annSig, peer); err != nil {
		t.Fatalf("unable to delete message: %v", err)
	}
	assertMsg(annSig, peer, false)

	// The store allows overwriting ChannelUpdates, since there can be
	// multiple versions, so we'll test things slightly different.
	//
	// The ChannelUpdate message should exist within the store after adding
	// it.
	chanUpdate := randChannelUpdate()
	if err := msgStore.AddMessage(chanUpdate, peer); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	assertMsg(chanUpdate, peer, true)

	// Now, we'll create a new version for the same ChannelUpdate message.
	// Adding this one to the store will overwrite the previous one, so only
	// the new one should exist.
	newChanUpdate := randChannelUpdate()
	newChanUpdate.ShortChannelID = chanUpdate.ShortChannelID
	newChanUpdate.Timestamp = chanUpdate.Timestamp + 1
	if err := msgStore.AddMessage(newChanUpdate, peer); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	assertMsg(chanUpdate, peer, false)
	assertMsg(newChanUpdate, peer, true)

	// Deleting the older message should act as a NOP and should NOT delete
	// the newer version as the older no longer exists.
	if err := msgStore.DeleteMessage(chanUpdate, peer); err != nil {
		t.Fatalf("unable to delete message: %v", err)
	}
	assertMsg(chanUpdate, peer, false)
	assertMsg(newChanUpdate, peer, true)

	// The newer version should no longer exist within the store after
	// deleting it.
	if err := msgStore.DeleteMessage(newChanUpdate, peer); err != nil {
		t.Fatalf("unable to delete message: %v", err)
	}
	assertMsg(newChanUpdate, peer, false)
}

package main

import (
	"sync"

	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// retransmitter represents the retransmission subsystem which is described
// in details in BOLT #2 (Message Retransmission). This subsystem keeps
// records of all messages that were sent to other peer and waits the ACK
// message to be received from other side. The ACK message denotes that the
// previous messages was read. Because communication transports are unreliable
// and may need to be re-established from time to time and reconnection
// introduces doubt as to what has been received such logic is needed to be sure
// that peers are in consistent state in terms of message communication.
type retransmitter struct {
	// storage is a message storage which is needed to store the messages
	// which weren't acked and restore them after node restarts, in order to
	// send them to other side again.
	storage channeldb.MessageStore

	// codeToIndex map is used to locate which messages can be deleted from
	// the message storage in response to a retrieved ACK message. The
	// mapping for items is code -> {index #1 , ... , index #n}. So when
	// receiving a new message, we check for the existence of the message
	// code in this index bucket, then delete all the messages from the
	// top-level bucket that are returned.
	codeToIndex map[lnwire.MessageCode][]uint64

	// messagesToRetransmit list of messages that should be retransmitted
	// to other side.
	messagesToRetransmit []lnwire.Message

	mutex sync.RWMutex
}

// newRetransmitter creates new instance of retransmitter.
func newRetransmitter(storage channeldb.MessageStore) (*retransmitter, error) {

	// Retrieve the messages from the message storage with their
	// associated indexes.
	indexes, messages, err := storage.Get()
	if err != channeldb.ErrPeerMessagesNotFound && err != nil {
		return nil, err
	}

	// Initialize map of code to index map, so than later we can retrieve
	// indexes of messages that should be removed.
	codeToIndex := make(map[lnwire.MessageCode][]uint64)
	for i, message := range messages {
		codeToIndex[message.Command()] = append(
			codeToIndex[message.Command()],
			indexes[i],
		)
	}

	return &retransmitter{
		storage:              storage,
		codeToIndex:          codeToIndex,
		messagesToRetransmit: messages,
	}, nil
}

// Register adds message that should be acknowledged in the message storage.
func (rt *retransmitter) Register(msg lnwire.Message) error {
	switch msg.Command() {
	// messages without acknowledgment
	case lnwire.CmdCloseComplete,
		lnwire.CmdChannelUpdateAnnouncement,
		lnwire.CmdChannelAnnouncement,
		lnwire.CmdNodeAnnouncement,
		lnwire.CmdPing,
		lnwire.CmdPong,
		lnwire.CmdErrorGeneric,
		lnwire.CmdInit:
		return nil
	default:
		// Adds message to storage and returns the message index which
		// have been associated with this message within the storage.
		index, err := rt.storage.Add(msg)
		if err != nil {
			return err
		}

		// Associate the message index within the message storage
		// with message code in order to remove messages by index later.
		rt.mutex.Lock()
		rt.codeToIndex[msg.Command()] = append(
			rt.codeToIndex[msg.Command()], index,
		)
		rt.mutex.Unlock()

		return nil
	}
}

// Ack encapsulates the specification logic about which messages should be
// acknowledged by receiving this one.
func (rt *retransmitter) Ack(msg lnwire.Message) error {
	switch msg.Command() {

	case lnwire.CmdSingleFundingResponse:
		return rt.remove(
			lnwire.CmdSingleFundingRequest,
		)
	case lnwire.CmdSingleFundingComplete:
		return rt.remove(
			lnwire.CmdSingleFundingResponse,
		)
	case lnwire.CmdSingleFundingSignComplete:
		return rt.remove(
			lnwire.CmdSingleFundingComplete,
		)
	case lnwire.CmdFundingLocked:
		return rt.remove(
			lnwire.CmdSingleFundingSignComplete,
		)
	case lnwire.CmdUpdateAddHTLC,
		lnwire.CmdUpdateFailHTLC,
		lnwire.CmdUpdateFufillHTLC:
		return rt.remove(
			lnwire.CmdFundingLocked,
		)
	case lnwire.CmdRevokeAndAck:
		return rt.remove(
			lnwire.CmdUpdateAddHTLC,
			lnwire.CmdUpdateFailHTLC,
			lnwire.CmdUpdateFufillHTLC,
			lnwire.CmdCommitSig,
			lnwire.CmdCloseRequest,
			lnwire.CmdFundingLocked,
		)

	case lnwire.CmdCommitSig,
		lnwire.CmdCloseRequest:
		return rt.remove(
			lnwire.CmdFundingLocked,
			lnwire.CmdRevokeAndAck,
		)
	case lnwire.CmdCloseComplete:
		return rt.remove(
			lnwire.CmdCloseRequest,
		)
	case lnwire.CmdPing,
		lnwire.CmdPong,
		lnwire.CmdChannelUpdateAnnouncement,
		lnwire.CmdChannelAnnouncement,
		lnwire.CmdNodeAnnouncement,
		lnwire.CmdErrorGeneric,
		lnwire.CmdInit,
		lnwire.CmdSingleFundingRequest:
		return nil

	default:
		return errors.Errorf("wrong message type: %v", msg.Command())
	}
}

// remove retrieves the messages storage indexes of the messages that
// corresponds to given types/codes and remove them from the message storage
// thereby acknowledge them.
func (rt *retransmitter) remove(codes ...lnwire.MessageCode) error {
	rt.mutex.RLock()
	var messagesToRemove []uint64
	for _, code := range codes {
		indexes, ok := rt.codeToIndex[code]
		if !ok {
			continue
		}
		messagesToRemove = append(messagesToRemove, indexes...)
	}
	rt.mutex.RUnlock()

	if err := rt.storage.Remove(messagesToRemove); err != nil {
		return err
	}

	// After successful deletion the messages by index, clean up the code
	// to index map.
	rt.mutex.Lock()
	for _, code := range codes {
		delete(rt.codeToIndex, code)
	}
	rt.mutex.Unlock()

	return nil
}

// MessagesToRetransmit returns the array of messages, that were not
// acknowledged in previous session with this peer, in the order they have been
// originally added in storage.
func (rt *retransmitter) MessagesToRetransmit() []lnwire.Message {
	return rt.messagesToRetransmit
}

// Flush removes the initialized messages after the have been successfully
// retransmitted.
func (rt *retransmitter) Flush() {
	rt.messagesToRetransmit = nil
}

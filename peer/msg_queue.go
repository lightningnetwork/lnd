package peer

import "container/list"

// msgQueue owns the two priority lists and their combined resource accounting.
// Message-specific retained-memory estimates are supplied in outgoingMsg, so
// this type remains independent of Ping, Pong, and other wire semantics.
type msgQueue struct {
	// priorityMsgs stores messages that must be selected before any lazy
	// message while preserving insertion order within the priority class.
	priorityMsgs list.List

	// lazyMsgs stores deferrable messages in insertion order so front can
	// service them only when the strict-priority list is empty.
	lazyMsgs list.List

	// limits is the immutable per-peer policy used by push to decide when
	// retaining another message requires disconnecting the connection.
	limits queueLimits

	// numMsgs mirrors the combined list length, avoiding a traversal on
	// every insertion while enforcing the total message-count bound.
	numMsgs int

	// numBytes mirrors the combined queueCost values, letting push and pop
	// enforce retained-memory bounds without knowing wire message shapes.
	numBytes int
}

// newMsgQueue constructs an empty queue with per-peer resource bounds. The
// list zero values are ready for use, so only the immutable limits are stored.
func newMsgQueue(limits queueLimits) *msgQueue {
	return &msgQueue{limits: limits}
}

// front returns the next message using strict priority ordering. Returning a
// nil element lets queueHandler disable its send case without a second select.
func (q *msgQueue) front() (*list.Element, outgoingMsg) {
	elem := q.priorityMsgs.Front()
	if elem == nil {
		elem = q.lazyMsgs.Front()
	}
	if elem == nil {
		return nil, outgoingMsg{}
	}

	return elem, msgFromElement(elem)
}

// msgFromElement enforces msgQueue's internal list invariant. A panic denotes
// a programming error because push is the only method that inserts elements.
func msgFromElement(elem *list.Element) outgoingMsg {
	msg, ok := elem.Value.(outgoingMsg)
	if !ok {
		panic("msgQueue element is not an outgoingMsg")
	}

	return msg
}

// push appends a message to its priority list and charges its immutable cost
// to the combined backlog. A false result tells the owner to disconnect rather
// than dropping an ordered protocol message or blocking an arbitrary producer.
func (q *msgQueue) push(msg outgoingMsg) bool {
	if msg.priority {
		q.priorityMsgs.PushBack(msg)
	} else {
		q.lazyMsgs.PushBack(msg)
	}

	q.numMsgs++
	q.numBytes += msg.queueCost

	return q.numMsgs <= q.limits.maxMsgs &&
		q.numBytes <= q.limits.maxBytes
}

// pop removes the selected front element and releases the exact cost charged
// at insertion, avoiding both message-type knowledge and cost recomputation.
func (q *msgQueue) pop(elem *list.Element) {
	msg := msgFromElement(elem)
	if msg.priority {
		q.priorityMsgs.Remove(elem)
	} else {
		q.lazyMsgs.Remove(elem)
	}

	q.numMsgs--
	q.numBytes -= msg.queueCost
}

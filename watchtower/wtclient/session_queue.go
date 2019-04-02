package wtclient

import (
	"container/list"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// retryInterval is the default duration we will wait between attempting to
// connect back out to a tower if the prior state update failed.
const retryInterval = 2 * time.Second

// reserveStatus is an enum that signals how full a particular session is.
type reserveStatus uint8

const (
	// reserveAvailable indicates that the session has space for at least
	// one more backup.
	reserveAvailable reserveStatus = iota

	// reserveExhausted indicates that all slots in the session have been
	// allocated.
	reserveExhausted
)

// sessionQueueConfig bundles the resources required by the sessionQueue to
// perform its duties. All entries MUST be non-nil.
type sessionQueueConfig struct {
	// ClientSession provides access to the negotiated session parameters
	// and updating its persistent storage.
	ClientSession *wtdb.ClientSession

	// ChainHash identifies the chain for which the session's justice
	// transactions are targeted.
	ChainHash chainhash.Hash

	// Dial allows the client to dial the tower using it's public key and
	// net address.
	Dial func(*btcec.PrivateKey,
		*lnwire.NetAddress) (wtserver.Peer, error)

	// SendMessage encodes, encrypts, and writes a message to the given peer.
	SendMessage func(wtserver.Peer, wtwire.Message) error

	// ReadMessage receives, decypts, and decodes a message from the given
	// peer.
	ReadMessage func(wtserver.Peer) (wtwire.Message, error)

	// Signer facilitates signing of inputs, used to construct the witnesses
	// for justice transaction inputs.
	Signer input.Signer

	// DB provides access to the client's stable storage.
	DB DB

	// MinBackoff defines the initial backoff applied by the session
	// queue before reconnecting to the tower after a failed or partially
	// successful batch is sent. Subsequent backoff durations will grow
	// exponentially up until MaxBackoff.
	MinBackoff time.Duration

	// MaxBackoff defines the maximum backoff applied by the session
	// queue before reconnecting to the tower after a failed or partially
	// successful batch is sent. If the exponential backoff produces a
	// timeout greater than this value, the backoff duration will be clamped
	// to MaxBackoff.
	MaxBackoff time.Duration
}

// sessionQueue implements a reliable queue that will encrypt and send accepted
// backups to the watchtower specified in the config's ClientSession. Calling
// Quit will attempt to perform a clean shutdown by receiving an ACK from the
// tower for all pending backups before exiting. The clean shutdown can be
// aborted by using ForceQuit, which will attempt to shutdown the queue
// immediately.
type sessionQueue struct {
	started sync.Once
	stopped sync.Once
	forced  sync.Once

	cfg *sessionQueueConfig

	commitQueue  *list.List
	pendingQueue *list.List
	queueMtx     sync.Mutex
	queueCond    *sync.Cond

	localInit *wtwire.Init
	towerAddr *lnwire.NetAddress

	seqNum uint16

	retryBackoff time.Duration

	quit      chan struct{}
	forceQuit chan struct{}
	shutdown  chan struct{}
}

// newSessionQueue intiializes a fresh sessionQueue.
func newSessionQueue(cfg *sessionQueueConfig) *sessionQueue {
	localInit := wtwire.NewInitMessage(
		lnwire.NewRawFeatureVector(wtwire.WtSessionsRequired),
		cfg.ChainHash,
	)

	towerAddr := &lnwire.NetAddress{
		IdentityKey: cfg.ClientSession.Tower.IdentityKey,
		Address:     cfg.ClientSession.Tower.Addresses[0],
	}

	sq := &sessionQueue{
		cfg:          cfg,
		commitQueue:  list.New(),
		pendingQueue: list.New(),
		localInit:    localInit,
		towerAddr:    towerAddr,
		seqNum:       cfg.ClientSession.SeqNum,
		retryBackoff: cfg.MinBackoff,
		quit:         make(chan struct{}),
		forceQuit:    make(chan struct{}),
		shutdown:     make(chan struct{}),
	}
	sq.queueCond = sync.NewCond(&sq.queueMtx)

	sq.restoreCommittedUpdates()

	return sq
}

// Start idempotently starts the sessionQueue so that it can begin accepting
// backups.
func (q *sessionQueue) Start() {
	q.started.Do(func() {
		// TODO(conner): load prior committed state updates from disk an
		// populate in queue.

		go q.sessionManager()
	})
}

// Stop idempotently stops the sessionQueue by initiating a clean shutdown that
// will clear all pending tasks in the queue before returning to the caller.
func (q *sessionQueue) Stop() {
	q.stopped.Do(func() {
		log.Debugf("Stopping session queue %s", q.ID())

		close(q.quit)
		q.signalUntilShutdown()

		// Skip log if we also force quit.
		select {
		case <-q.forceQuit:
			return
		default:
		}

		log.Debugf("Session queue %s successfully stopped", q.ID())
	})
}

// ForceQuit idempotently aborts any clean shutdown in progress and returns to
// he caller after all lingering goroutines have spun down.
func (q *sessionQueue) ForceQuit() {
	q.forced.Do(func() {
		log.Infof("Force quitting session queue %s", q.ID())

		close(q.forceQuit)
		q.signalUntilShutdown()

		log.Infof("Session queue %s unclean shutdown complete", q.ID())
	})
}

// ID returns the wtdb.SessionID for the queue, which can be used to uniquely
// identify this a particular queue.
func (q *sessionQueue) ID() *wtdb.SessionID {
	return &q.cfg.ClientSession.ID
}

// AcceptTask attempts to queue a backupTask for delivery to the sessionQueue's
// tower. The session will only be accepted if the queue is not already
// exhausted and the task is successfully bound to the ClientSession.
func (q *sessionQueue) AcceptTask(task *backupTask) (reserveStatus, bool) {
	q.queueCond.L.Lock()

	// Examine the current reserve status of the session queue.
	curStatus := q.reserveStatus()
	switch curStatus {

	// The session queue is exhausted, and cannot accept the task because it
	// is full. Reject the task such that it can be tried against a
	// different session.
	case reserveExhausted:
		q.queueCond.L.Unlock()
		return curStatus, false

	// The session queue is not exhausted. Compute the sweep and reward
	// outputs as a function of the session parameters. If the outputs are
	// dusty or uneconomical to backup, the task is rejected and will not be
	// tried again.
	//
	// TODO(conner): queue backups and retry with different session params.
	case reserveAvailable:
		err := task.bindSession(q.cfg.ClientSession)
		if err != nil {
			q.queueCond.L.Unlock()
			log.Debugf("SessionQueue %s rejected backup chanid=%s "+
				"commit-height=%d: %v", q.ID(), task.id.ChanID,
				task.id.CommitHeight, err)
			return curStatus, false
		}
	}

	// The sweep and reward outputs satisfy the session's policy, queue the
	// task for final signing and delivery.
	q.pendingQueue.PushBack(task)

	// Finally, compute the session's *new* reserve status. This will be
	// used by the client to determine if it can continue using this session
	// queue, or if it should negotiate a new one.
	newStatus := q.reserveStatus()
	q.queueCond.L.Unlock()

	q.queueCond.Signal()

	return newStatus, true
}

// updateWithSeqNum stores a CommittedUpdate with its assigned sequence number.
// This allows committed updates to be sorted after a restart, and added to the
// commitQueue in the proper order for delivery.
type updateWithSeqNum struct {
	seqNum uint16
	update *wtdb.CommittedUpdate
}

// restoreCommittedUpdates processes any CommittedUpdates loaded on startup by
// sorting them in ascending order of sequence numbers and adding them to the
// commitQueue. These will be sent before any pending updates are processed.
func (q *sessionQueue) restoreCommittedUpdates() {
	committedUpdates := q.cfg.ClientSession.CommittedUpdates

	// Construct and unordered slice of all committed updates with their
	// assigned sequence numbers.
	sortedUpdates := make([]updateWithSeqNum, 0, len(committedUpdates))
	for seqNum, update := range committedUpdates {
		sortedUpdates = append(sortedUpdates, updateWithSeqNum{
			seqNum: seqNum,
			update: update,
		})
	}

	// Sort the resulting slice by increasing sequence number.
	sort.Slice(sortedUpdates, func(i, j int) bool {
		return sortedUpdates[i].seqNum < sortedUpdates[j].seqNum
	})

	// Finally, add the sorted, committed updates to he commitQueue. These
	// updates will be prioritized before any new tasks are assigned to the
	// sessionQueue. The queue will begin uploading any tasks in the
	// commitQueue as soon as it is started, e.g. during client
	// initialization when detecting that this session has unacked updates.
	for _, update := range sortedUpdates {
		q.commitQueue.PushBack(update)
	}
}

// sessionManager is the primary event loop for the sessionQueue, and is
// responsible for encrypting and sending accepted tasks to the tower.
func (q *sessionQueue) sessionManager() {
	defer close(q.shutdown)

	for {
		q.queueCond.L.Lock()
		for q.commitQueue.Len() == 0 &&
			q.pendingQueue.Len() == 0 {

			q.queueCond.Wait()

			select {
			case <-q.quit:
				if q.commitQueue.Len() == 0 &&
					q.pendingQueue.Len() == 0 {
					q.queueCond.L.Unlock()
					return
				}
			case <-q.forceQuit:
				q.queueCond.L.Unlock()
				return
			default:
			}
		}
		q.queueCond.L.Unlock()

		// Exit immediately if a force quit has been requested.  If the
		// either of the queues still has state updates to send to the
		// tower, we may never exit in the above case if we are unable
		// to reach the tower for some reason.
		select {
		case <-q.forceQuit:
			return
		default:
		}

		// Initiate a new connection to the watchtower and attempt to
		// drain all pending tasks.
		q.drainBackups()
	}
}

// drainBackups attempts to send all pending updates in the queue to the tower.
func (q *sessionQueue) drainBackups() {
	// First, check that we are able to dial this session's tower.
	conn, err := q.cfg.Dial(q.cfg.ClientSession.SessionPrivKey, q.towerAddr)
	if err != nil {
		log.Errorf("Unable to dial watchtower at %v: %v",
			q.towerAddr, err)

		q.increaseBackoff()
		select {
		case <-time.After(q.retryBackoff):
		case <-q.forceQuit:
		}
		return
	}
	defer conn.Close()

	// Begin draining the queue of pending state updates. Before the first
	// update is sent, we will precede it with an Init message. If the first
	// is successful, subsequent updates can be streamed without sending an
	// Init.
	for sendInit := true; ; sendInit = false {
		// Generate the next state update to upload to the tower. This
		// method will first proceed in dequeueing committed updates
		// before attempting to dequeue any pending updates.
		stateUpdate, isPending, err := q.nextStateUpdate()
		if err != nil {
			log.Errorf("Unable to get next state update: %v", err)
			return
		}

		// Now, send the state update to the tower and wait for a reply.
		err = q.sendStateUpdate(
			conn, stateUpdate, q.localInit, sendInit, isPending,
		)
		if err != nil {
			log.Errorf("Unable to send state update: %v", err)

			q.increaseBackoff()
			select {
			case <-time.After(q.retryBackoff):
			case <-q.forceQuit:
			}
			return
		}

		// If the last task was backed up successfully, we'll exit and
		// continue once more tasks are added to the queue. We'll also
		// clear any accumulated backoff as this batch was able to be
		// sent reliably.
		if stateUpdate.IsComplete == 1 {
			q.resetBackoff()
			return
		}

		// Always apply a small delay between sends, which makes the
		// unit tests more reliable. If we were requested to back off,
		// when we will do so.
		select {
		case <-time.After(time.Millisecond):
		case <-q.forceQuit:
			return
		}
	}
}

// nextStateUpdate returns the next wtwire.StateUpdate to upload to the tower.
// If any committed updates are present, this method will reconstruct the state
// update from the committed update using the current last applied value found
// in the database. Otherwise, it will select the next pending update, craft the
// payload, and commit an update before returning the state update to send. The
// boolean value in the response is true if the state update is taken from the
// pending queue, allowing the caller to remove the update from either the
// commit or pending queue if the update is successfully acked.
func (q *sessionQueue) nextStateUpdate() (*wtwire.StateUpdate, bool, error) {
	var (
		seqNum    uint16
		update    *wtdb.CommittedUpdate
		isLast    bool
		isPending bool
	)

	q.queueCond.L.Lock()
	switch {

	// If the commit queue is non-empty, parse the next committed update.
	case q.commitQueue.Len() > 0:
		next := q.commitQueue.Front()
		updateWithSeq := next.Value.(updateWithSeqNum)

		seqNum = updateWithSeq.seqNum
		update = updateWithSeq.update

		// If this is the last item in the commit queue and no items
		// exist in the pending queue, we will use the IsComplete flag
		// in the StateUpdate to signal that the tower can release the
		// connection after replying to free up resources.
		isLast = q.commitQueue.Len() == 1 && q.pendingQueue.Len() == 0
		q.queueCond.L.Unlock()

		log.Debugf("Reprocessing committed state update for "+
			"session=%s seqnum=%d", q.ID(), seqNum)

	// Otherwise, craft and commit the next update from the pending queue.
	default:
		isPending = true

		// Determine the current sequence number to apply for this
		// pending update.
		seqNum = q.seqNum + 1

		// Obtain the next task from the queue.
		next := q.pendingQueue.Front()
		task := next.Value.(*backupTask)

		// If this is the last item in the pending queue, we will use
		// the IsComplete flag in the StateUpdate to signal that the
		// tower can release the connection after replying to free up
		// resources.
		isLast = q.pendingQueue.Len() == 1
		q.queueCond.L.Unlock()

		hint, encBlob, err := task.craftSessionPayload(q.cfg.Signer)
		if err != nil {
			// TODO(conner): mark will not send
			return nil, false, fmt.Errorf("unable to craft "+
				"session payload: %v", err)
		}
		// TODO(conner): special case other obscure errors

		update = &wtdb.CommittedUpdate{
			BackupID:      task.id,
			Hint:          hint,
			EncryptedBlob: encBlob,
		}

		log.Debugf("Committing state update for session=%s seqnum=%d",
			q.ID(), seqNum)
	}

	// Before sending the task to the tower, commit the state update
	// to disk using the assigned sequence number. If this task has already
	// been committed, the call will succeed and only be used for the
	// purpose of obtaining the last applied value to send to the tower.
	//
	// This step ensures that if we crash before receiving an ack that we
	// will retransmit the same update. If the tower successfully received
	// the update from before, it will reply with an ACK regardless of what
	// we send the next time. This step ensures that if we reliably send the
	// same update for a given sequence number, to prevent us from thinking
	// we backed up a state when we instead backed up another.
	lastApplied, err := q.cfg.DB.CommitUpdate(q.ID(), seqNum, update)
	if err != nil {
		// TODO(conner): mark failed/reschedule
		return nil, false, fmt.Errorf("unable to commit state update "+
			"for session=%s seqnum=%d: %v", q.ID(), seqNum, err)
	}

	stateUpdate := &wtwire.StateUpdate{
		SeqNum:        seqNum,
		LastApplied:   lastApplied,
		Hint:          update.Hint,
		EncryptedBlob: update.EncryptedBlob,
	}

	// Set the IsComplete flag if this is the last queued item.
	if isLast {
		stateUpdate.IsComplete = 1
	}

	return stateUpdate, isPending, nil
}

// sendStateUpdate sends a wtwire.StateUpdate to the watchtower and processes
// the ACK before returning. If sendInit is true, this method will first send
// the localInit message and verify that the tower supports our required feature
// bits. And error is returned if any part of the send fails. The boolean return
// variable indicates whether or not we should back off before attempting to
// send the next state update.
func (q *sessionQueue) sendStateUpdate(conn wtserver.Peer,
	stateUpdate *wtwire.StateUpdate, localInit *wtwire.Init,
	sendInit, isPending bool) error {

	// If this is the first message being sent to the tower, we must send an
	// Init message to establish that server supports the features we
	// require.
	if sendInit {
		// Send Init to tower.
		err := q.cfg.SendMessage(conn, q.localInit)
		if err != nil {
			return err
		}

		// Receive Init from tower.
		remoteMsg, err := q.cfg.ReadMessage(conn)
		if err != nil {
			return err
		}

		remoteInit, ok := remoteMsg.(*wtwire.Init)
		if !ok {
			return fmt.Errorf("watchtower responded with %T to "+
				"Init", remoteMsg)
		}

		// Validate Init.
		err = q.localInit.CheckRemoteInit(
			remoteInit, wtwire.FeatureNames,
		)
		if err != nil {
			return err
		}
	}

	// Send StateUpdate to tower.
	err := q.cfg.SendMessage(conn, stateUpdate)
	if err != nil {
		return err
	}

	// Receive StateUpdate from tower.
	remoteMsg, err := q.cfg.ReadMessage(conn)
	if err != nil {
		return err
	}

	stateUpdateReply, ok := remoteMsg.(*wtwire.StateUpdateReply)
	if !ok {
		return fmt.Errorf("watchtower responded with %T to StateUpdate",
			remoteMsg)
	}

	// Process the reply from the tower.
	switch stateUpdateReply.Code {

	// The tower reported a successful update, validate the response and
	// record the last applied returned.
	case wtwire.CodeOK:

	// TODO(conner): handle other error cases properly, ban towers, etc.
	default:
		err := fmt.Errorf("received error code %v in "+
			"StateUpdateReply from tower=%x session=%v",
			stateUpdateReply.Code,
			conn.RemotePub().SerializeCompressed(), q.ID())
		log.Warnf("Unable to upload state update: %v", err)
		return err
	}

	lastApplied := stateUpdateReply.LastApplied
	err = q.cfg.DB.AckUpdate(q.ID(), stateUpdate.SeqNum, lastApplied)
	switch {
	case err == wtdb.ErrUnallocatedLastApplied:
		// TODO(conner): borked watchtower
		err = fmt.Errorf("unable to ack update=%d session=%s: %v",
			stateUpdate.SeqNum, q.ID(), err)
		log.Errorf("Failed to ack update: %v", err)
		return err

	case err == wtdb.ErrLastAppliedReversion:
		// TODO(conner): borked watchtower
		err = fmt.Errorf("unable to ack update=%d session=%s: %v",
			stateUpdate.SeqNum, q.ID(), err)
		log.Errorf("Failed to ack update: %v", err)
		return err

	case err != nil:
		err = fmt.Errorf("unable to ack update=%d session=%s: %v",
			stateUpdate.SeqNum, q.ID(), err)
		log.Errorf("Failed to ack update: %v", err)
		return err
	}

	log.Infof("Removing update session=%s seqnum=%d is_pending=%v "+
		"from memory", q.ID(), stateUpdate.SeqNum, isPending)

	q.queueCond.L.Lock()
	if isPending {
		// If a pending update was successfully sent, increment the
		// sequence number and remove the item from the queue. This
		// ensures the total number of backups in the session remains
		// unchanged, which maintains the external view of the session's
		// reserve status.
		q.seqNum++
		q.pendingQueue.Remove(q.pendingQueue.Front())
	} else {
		// Otherwise, simply remove the update from the committed queue.
		// This has no effect on the queues reserve status since the
		// update had already been committed.
		q.commitQueue.Remove(q.commitQueue.Front())
	}
	q.queueCond.L.Unlock()

	return nil
}

// reserveStatus returns a reserveStatus indicating whether or not the
// sessionQueue can accept another task. reserveAvailable is returned when a
// task can be accepted, and reserveExhausted is returned if the all slots in
// the session have been allocated.
//
// NOTE: This method MUST be called with queueCond's exclusive lock held.
func (q *sessionQueue) reserveStatus() reserveStatus {
	numPending := uint32(q.pendingQueue.Len())
	maxUpdates := uint32(q.cfg.ClientSession.Policy.MaxUpdates)

	log.Debugf("SessionQueue %s reserveStatus seqnum=%d pending=%d "+
		"max-updates=%d", q.ID(), q.seqNum, numPending, maxUpdates)

	if uint32(q.seqNum)+numPending < maxUpdates {
		return reserveAvailable
	}

	return reserveExhausted

}

// resetBackoff returns the connection backoff the minimum configured backoff.
func (q *sessionQueue) resetBackoff() {
	q.retryBackoff = q.cfg.MinBackoff
}

// increaseBackoff doubles the current connection backoff, clamping to the
// configured maximum backoff if it would exceed the limit.
func (q *sessionQueue) increaseBackoff() {
	q.retryBackoff *= 2
	if q.retryBackoff > q.cfg.MaxBackoff {
		q.retryBackoff = q.cfg.MaxBackoff
	}
}

// signalUntilShutdown strobes the sessionQueue's condition variable until the
// main event loop exits.
func (q *sessionQueue) signalUntilShutdown() {
	for {
		select {
		case <-time.After(time.Millisecond):
			q.queueCond.Signal()
		case <-q.shutdown:
			return
		}
	}
}

// sessionQueueSet maintains a mapping of SessionIDs to their corresponding
// sessionQueue.
type sessionQueueSet map[wtdb.SessionID]*sessionQueue

// Add inserts a sessionQueue into the sessionQueueSet.
func (s *sessionQueueSet) Add(sessionQueue *sessionQueue) {
	(*s)[*sessionQueue.ID()] = sessionQueue
}

// ApplyAndWait executes the nil-adic function returned from getApply for each
// sessionQueue in the set in parallel, then waits for all of them to finish
// before returning to the caller.
func (s *sessionQueueSet) ApplyAndWait(getApply func(*sessionQueue) func()) {
	var wg sync.WaitGroup
	for _, sessionq := range *s {
		wg.Add(1)
		go func(sq *sessionQueue) {
			defer wg.Done()
			getApply(sq)()
		}(sessionq)
	}
	wg.Wait()
}

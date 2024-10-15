package wtclient

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/lightningnetwork/lnd/watchtower/wtserver"
	"github.com/lightningnetwork/lnd/watchtower/wtwire"
)

// sessionQueueStatus is an enum that signals how full a particular session is.
type sessionQueueStatus uint8

const (
	// sessionQueueAvailable indicates that the session has space for at
	// least one more backup.
	sessionQueueAvailable sessionQueueStatus = iota

	// sessionQueueExhausted indicates that all slots in the session have
	// been allocated.
	sessionQueueExhausted

	// sessionQueueShuttingDown indicates that the session queue is
	// shutting down and so is no longer accepting any more backups.
	sessionQueueShuttingDown
)

// sessionQueueConfig bundles the resources required by the sessionQueue to
// perform its duties. All entries MUST be non-nil.
type sessionQueueConfig struct {
	// ClientSession provides access to the negotiated session parameters
	// and updating its persistent storage.
	ClientSession *ClientSession

	// ChainHash identifies the chain for which the session's justice
	// transactions are targeted.
	ChainHash chainhash.Hash

	// Dial allows the client to dial the tower using it's public key and
	// net address.
	Dial func(keychain.SingleKeyECDH, *lnwire.NetAddress) (wtserver.Peer,
		error)

	// SendMessage encodes, encrypts, and writes a message to the given
	// peer.
	SendMessage func(wtserver.Peer, wtwire.Message) error

	// ReadMessage receives, decypts, and decodes a message from the given
	// peer.
	ReadMessage func(wtserver.Peer) (wtwire.Message, error)

	// Signer facilitates signing of inputs, used to construct the witnesses
	// for justice transaction inputs.
	Signer input.Signer

	// BuildBreachRetribution is a function closure that allows the client
	// to fetch the breach retribution info for a certain channel at a
	// certain revoked commitment height.
	BuildBreachRetribution BreachRetributionBuilder

	// TaskPipeline is a pipeline which the sessionQueue should use to send
	// any unhandled tasks on shutdown of the queue.
	TaskPipeline *DiskOverflowQueue[*wtdb.BackupID]

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

	// Log specifies the desired log output, which should be prefixed by the
	// client type, e.g. anchor or legacy.
	Log btclog.Logger
}

// sessionQueue implements a reliable queue that will encrypt and send accepted
// backups to the watchtower specified in the config's ClientSession. Calling
// Stop will attempt to perform a clean shutdown replaying any un-committed
// pending updates to the client's main task pipeline.
type sessionQueue struct {
	started sync.Once
	stopped sync.Once
	forced  sync.Once

	cfg *sessionQueueConfig
	log btclog.Logger

	commitQueue  *list.List
	pendingQueue *list.List
	queueMtx     sync.Mutex
	queueCond    *sync.Cond

	localInit *wtwire.Init
	tower     *Tower

	seqNum uint16

	retryBackoff time.Duration

	quit chan struct{}
	wg   sync.WaitGroup
}

// newSessionQueue initializes a fresh sessionQueue.
func newSessionQueue(cfg *sessionQueueConfig,
	updates []wtdb.CommittedUpdate) *sessionQueue {

	localInit := wtwire.NewInitMessage(
		lnwire.NewRawFeatureVector(wtwire.AltruistSessionsRequired),
		cfg.ChainHash,
	)

	sq := &sessionQueue{
		cfg:          cfg,
		log:          cfg.Log,
		commitQueue:  list.New(),
		pendingQueue: list.New(),
		localInit:    localInit,
		tower:        cfg.ClientSession.Tower,
		seqNum:       cfg.ClientSession.SeqNum,
		retryBackoff: cfg.MinBackoff,
		quit:         make(chan struct{}),
	}
	sq.queueCond = sync.NewCond(&sq.queueMtx)

	// The database should return them in sorted order, and session queue's
	// sequence number will be equal to that of the last committed update.
	for _, update := range updates {
		sq.commitQueue.PushBack(update)
	}

	return sq
}

// Start idempotently starts the sessionQueue so that it can begin accepting
// backups.
func (q *sessionQueue) Start() {
	q.started.Do(func() {
		q.wg.Add(1)
		go q.sessionManager()
	})
}

// Stop idempotently stops the sessionQueue by initiating a clean shutdown that
// will clear all pending tasks in the queue before returning to the caller.
// The final param should only be set to true if this is the last time that
// this session will be used. Otherwise, during normal shutdown, the final param
// should be false.
func (q *sessionQueue) Stop(final bool) error {
	var returnErr error
	q.stopped.Do(func() {
		q.log.Debugf("SessionQueue(%s) stopping ...", q.ID())

		close(q.quit)

		shutdown := make(chan struct{})
		go func() {
			for {
				select {
				case <-time.After(time.Millisecond):
					q.queueCond.Signal()
				case <-shutdown:
					return
				}
			}
		}()

		q.wg.Wait()
		close(shutdown)

		// Now, for any task in the pending queue that we have not yet
		// created a CommittedUpdate for, re-add the task to the main
		// task pipeline.
		updates, err := q.cfg.DB.FetchSessionCommittedUpdates(q.ID())
		if err != nil {
			returnErr = err
			return
		}

		unAckedUpdates := make(map[wtdb.BackupID]bool)
		for _, update := range updates {
			unAckedUpdates[update.BackupID] = true

			if !final {
				continue
			}

			err := q.cfg.TaskPipeline.QueueBackupID(
				&update.BackupID,
			)
			if err != nil {
				log.Errorf("could not re-queue %s: %v",
					update.BackupID, err)
				continue
			}
		}

		if final {
			err = q.cfg.DB.DeleteCommittedUpdates(q.ID())
			if err != nil {
				log.Errorf("could not delete committed "+
					"updates for session %s", q.ID())
			}
		}

		// Push any task that was on the pending queue that there is
		// not yet a committed update for back to the main task
		// pipeline.
		q.queueCond.L.Lock()
		for q.pendingQueue.Len() > 0 {
			next := q.pendingQueue.Front()
			q.pendingQueue.Remove(next)

			//nolint:forcetypeassert
			task := next.Value.(*backupTask)

			if unAckedUpdates[task.id] {
				continue
			}

			err := q.cfg.TaskPipeline.QueueBackupID(&task.id)
			if err != nil {
				log.Errorf("could not re-queue backup task: "+
					"%v", err)
				continue
			}
		}
		q.queueCond.L.Unlock()

		q.log.Debugf("SessionQueue(%s) stopped", q.ID())
	})

	return returnErr
}

// ID returns the wtdb.SessionID for the queue, which can be used to uniquely
// identify this a particular queue.
func (q *sessionQueue) ID() *wtdb.SessionID {
	return &q.cfg.ClientSession.ID
}

// AcceptTask attempts to queue a backupTask for delivery to the sessionQueue's
// tower. The session will only be accepted if the queue is not already
// exhausted or shutting down and the task is successfully bound to the
// ClientSession.
func (q *sessionQueue) AcceptTask(task *backupTask) (sessionQueueStatus, bool) {
	// Exit early if the queue has started shutting down.
	select {
	case <-q.quit:
		return sessionQueueShuttingDown, false
	default:
	}

	q.queueCond.L.Lock()

	// There is a chance that sessionQueue started shutting down between
	// the last quit channel check and waiting for the lock. So check one
	// more time here.
	select {
	case <-q.quit:
		q.queueCond.L.Unlock()
		return sessionQueueShuttingDown, false
	default:
	}

	numPending := uint32(q.pendingQueue.Len())
	maxUpdates := q.cfg.ClientSession.Policy.MaxUpdates
	q.log.Debugf("SessionQueue(%s) deciding to accept %v seqnum=%d "+
		"pending=%d max-updates=%d",
		q.ID(), task.id, q.seqNum, numPending, maxUpdates)

	// Examine the current reserve status of the session queue.
	curStatus := q.status()

	switch curStatus {

	// The session queue is exhausted, and cannot accept the task because it
	// is full. Reject the task such that it can be tried against a
	// different session.
	case sessionQueueExhausted:
		q.queueCond.L.Unlock()
		return curStatus, false

	// The session queue is not exhausted. Compute the sweep and reward
	// outputs as a function of the session parameters. If the outputs are
	// dusty or uneconomical to backup, the task is rejected and will not be
	// tried again.
	//
	// TODO(conner): queue backups and retry with different session params.
	case sessionQueueAvailable:
		err := task.bindSession(
			&q.cfg.ClientSession.ClientSessionBody,
			q.cfg.BuildBreachRetribution,
		)
		if err != nil {
			q.queueCond.L.Unlock()
			q.log.Debugf("SessionQueue(%s) rejected %v: %v ",
				q.ID(), task.id, err)
			return curStatus, false
		}
	}

	// The sweep and reward outputs satisfy the session's policy, queue the
	// task for final signing and delivery.
	q.pendingQueue.PushBack(task)

	// Finally, compute the session's *new* reserve status. This will be
	// used by the client to determine if it can continue using this session
	// queue, or if it should negotiate a new one.
	newStatus := q.status()
	q.queueCond.L.Unlock()

	q.queueCond.Signal()

	return newStatus, true
}

// sessionManager is the primary event loop for the sessionQueue, and is
// responsible for encrypting and sending accepted tasks to the tower.
func (q *sessionQueue) sessionManager() {
	defer q.wg.Done()

	for {
		q.queueCond.L.Lock()
		for q.commitQueue.Len() == 0 &&
			q.pendingQueue.Len() == 0 {

			q.queueCond.Wait()

			select {
			case <-q.quit:
				q.queueCond.L.Unlock()
				return
			default:
			}
		}
		q.queueCond.L.Unlock()

		// Exit immediately if the sessionQueue has been stopped.
		select {
		case <-q.quit:
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
	var (
		conn      wtserver.Peer
		err       error
		towerAddr = q.tower.Addresses.Peek()
	)

	for {
		q.log.Infof("SessionQueue(%s) attempting to dial tower at %v",
			q.ID(), towerAddr)

		// First, check that we are able to dial this session's tower.
		conn, err = q.cfg.Dial(
			q.cfg.ClientSession.SessionKeyECDH, &lnwire.NetAddress{
				IdentityKey: q.tower.IdentityKey,
				Address:     towerAddr,
			},
		)
		if err != nil {
			// If there are more addrs available, immediately try
			// those.
			nextAddr, iteratorErr := q.tower.Addresses.Next()
			if iteratorErr == nil {
				towerAddr = nextAddr
				continue
			}

			// Otherwise, if we have exhausted the address list,
			// back off and try again later.
			q.tower.Addresses.Reset()

			q.log.Errorf("SessionQueue(%s) unable to dial tower "+
				"at any available Addresses: %v", q.ID(), err)

			q.increaseBackoff()
			select {
			case <-time.After(q.retryBackoff):
			case <-q.quit:
			}
			return
		}

		break
	}
	defer conn.Close()

	// Begin draining the queue of pending state updates. Before the first
	// update is sent, we will precede it with an Init message. If the first
	// is successful, subsequent updates can be streamed without sending an
	// Init.
	for sendInit := true; ; sendInit = false {
		// Generate the next state update to upload to the tower. This
		// method will first proceed in dequeuing committed updates
		// before attempting to dequeue any pending updates.
		stateUpdate, isPending, backupID, err := q.nextStateUpdate()
		if err != nil {
			q.log.Errorf("SessionQueue(%v) unable to get next "+
				"state update: %v", q.ID(), err)
			return
		}

		// Now, send the state update to the tower and wait for a reply.
		err = q.sendStateUpdate(conn, stateUpdate, sendInit, isPending)
		if err != nil {
			q.log.Errorf("SessionQueue(%s) unable to send state "+
				"update: %v", q.ID(), err)

			q.increaseBackoff()
			select {
			case <-time.After(q.retryBackoff):
			case <-q.quit:
			}
			return
		}

		q.log.Infof("SessionQueue(%s) uploaded %v seqnum=%d",
			q.ID(), backupID, stateUpdate.SeqNum)

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
		case <-q.quit:
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
func (q *sessionQueue) nextStateUpdate() (*wtwire.StateUpdate, bool,
	wtdb.BackupID, error) {

	var (
		seqNum    uint16
		update    wtdb.CommittedUpdate
		isLast    bool
		isPending bool
	)

	q.queueCond.L.Lock()
	switch {

	// If the commit queue is non-empty, parse the next committed update.
	case q.commitQueue.Len() > 0:
		next := q.commitQueue.Front()

		update = next.Value.(wtdb.CommittedUpdate)
		seqNum = update.SeqNum

		// If this is the last item in the commit queue and no items
		// exist in the pending queue, we will use the IsComplete flag
		// in the StateUpdate to signal that the tower can release the
		// connection after replying to free up resources.
		isLast = q.commitQueue.Len() == 1 && q.pendingQueue.Len() == 0
		q.queueCond.L.Unlock()

		q.log.Debugf("SessionQueue(%s) reprocessing committed state "+
			"update for %v seqnum=%d",
			q.ID(), update.BackupID, seqNum)

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
			err := fmt.Errorf("unable to craft session payload: %w",
				err)
			return nil, false, wtdb.BackupID{}, err
		}
		// TODO(conner): special case other obscure errors

		update = wtdb.CommittedUpdate{
			SeqNum: seqNum,
			CommittedUpdateBody: wtdb.CommittedUpdateBody{
				BackupID:      task.id,
				Hint:          hint,
				EncryptedBlob: encBlob,
			},
		}

		q.log.Debugf("SessionQueue(%s) committing state update "+
			"%v seqnum=%d", q.ID(), update.BackupID, seqNum)
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
	lastApplied, err := q.cfg.DB.CommitUpdate(q.ID(), &update)
	if err != nil {
		// TODO(conner): mark failed/reschedule
		err := fmt.Errorf("unable to commit state update for "+
			"%v seqnum=%d: %v", update.BackupID, seqNum, err)
		return nil, false, wtdb.BackupID{}, err
	}

	stateUpdate := &wtwire.StateUpdate{
		SeqNum:        update.SeqNum,
		LastApplied:   lastApplied,
		Hint:          update.Hint,
		EncryptedBlob: update.EncryptedBlob,
	}

	// Set the IsComplete flag if this is the last queued item.
	if isLast {
		stateUpdate.IsComplete = 1
	}

	return stateUpdate, isPending, update.BackupID, nil
}

// sendStateUpdate sends a wtwire.StateUpdate to the watchtower and processes
// the ACK before returning. If sendInit is true, this method will first send
// the localInit message and verify that the tower supports our required feature
// bits. And error is returned if any part of the send fails. The boolean return
// variable indicates whether we should back off before attempting to send the
// next state update.
func (q *sessionQueue) sendStateUpdate(conn wtserver.Peer,
	stateUpdate *wtwire.StateUpdate, sendInit, isPending bool) error {

	towerAddr := &lnwire.NetAddress{
		IdentityKey: conn.RemotePub(),
		Address:     conn.RemoteAddr(),
	}

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
			return fmt.Errorf("watchtower %s responded with %T "+
				"to Init", towerAddr, remoteMsg)
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
		return fmt.Errorf("watchtower %s responded with %T to "+
			"StateUpdate", towerAddr, remoteMsg)
	}

	// Process the reply from the tower.
	switch stateUpdateReply.Code {

	// The tower reported a successful update, validate the response and
	// record the last applied returned.
	case wtwire.CodeOK:

	// TODO(conner): handle other error cases properly, ban towers, etc.
	default:
		err := fmt.Errorf("received error code %v in "+
			"StateUpdateReply for seqnum=%d",
			stateUpdateReply.Code, stateUpdate.SeqNum)
		q.log.Warnf("SessionQueue(%s) unable to upload state update "+
			"to tower=%s: %v", q.ID(), towerAddr, err)
		return err
	}

	lastApplied := stateUpdateReply.LastApplied
	err = q.cfg.DB.AckUpdate(q.ID(), stateUpdate.SeqNum, lastApplied)
	switch {
	case err == wtdb.ErrUnallocatedLastApplied:
		// TODO(conner): borked watchtower
		err = fmt.Errorf("unable to ack seqnum=%d: %w",
			stateUpdate.SeqNum, err)
		q.log.Errorf("SessionQueue(%v) failed to ack update: %v",
			q.ID(), err)
		return err

	case err == wtdb.ErrLastAppliedReversion:
		// TODO(conner): borked watchtower
		err = fmt.Errorf("unable to ack seqnum=%d: %w",
			stateUpdate.SeqNum, err)
		q.log.Errorf("SessionQueue(%s) failed to ack update: %v",
			q.ID(), err)
		return err

	case err != nil:
		err = fmt.Errorf("unable to ack seqnum=%d: %w",
			stateUpdate.SeqNum, err)
		q.log.Errorf("SessionQueue(%s) failed to ack update: %v",
			q.ID(), err)
		return err
	}

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

// status returns a sessionQueueStatus indicating whether the sessionQueue can
// accept another task. sessionQueueAvailable is returned when a task can be
// accepted, and sessionQueueExhausted is returned if the all slots in the
// session have been allocated.
//
// NOTE: This method MUST be called with queueCond's exclusive lock held.
func (q *sessionQueue) status() sessionQueueStatus {
	numPending := uint32(q.pendingQueue.Len())
	maxUpdates := uint32(q.cfg.ClientSession.Policy.MaxUpdates)

	if uint32(q.seqNum)+numPending < maxUpdates {
		return sessionQueueAvailable
	}

	return sessionQueueExhausted

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

// sessionQueueSet maintains a mapping of SessionIDs to their corresponding
// sessionQueue.
type sessionQueueSet struct {
	queues map[wtdb.SessionID]*sessionQueue
	mu     sync.Mutex
}

// newSessionQueueSet constructs a new sessionQueueSet.
func newSessionQueueSet() *sessionQueueSet {
	return &sessionQueueSet{
		queues: make(map[wtdb.SessionID]*sessionQueue),
	}
}

// AddAndStart inserts a sessionQueue into the sessionQueueSet and starts it.
func (s *sessionQueueSet) AddAndStart(sessionQueue *sessionQueue) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.queues[*sessionQueue.ID()] = sessionQueue

	sessionQueue.Start()
}

// StopAndRemove stops the given session queue and removes it from the
// sessionQueueSet.
func (s *sessionQueueSet) StopAndRemove(id wtdb.SessionID, final bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	queue, ok := s.queues[id]
	if !ok {
		return nil
	}

	delete(s.queues, id)

	return queue.Stop(final)
}

// Get fetches and returns the sessionQueue with the given ID.
func (s *sessionQueueSet) Get(id wtdb.SessionID) (*sessionQueue, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	q, ok := s.queues[id]

	return q, ok
}

// ApplyAndWait executes the nil-adic function returned from getApply for each
// sessionQueue in the set in parallel, then waits for all of them to finish
// before returning to the caller.
func (s *sessionQueueSet) ApplyAndWait(getApply func(*sessionQueue) func()) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var wg sync.WaitGroup
	for _, sessionq := range s.queues {
		wg.Add(1)
		go func(sq *sessionQueue) {
			defer wg.Done()

			getApply(sq)()
		}(sessionq)
	}
	wg.Wait()
}

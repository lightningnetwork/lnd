package wtclient

import (
	"container/list"
	"sync"
	"time"
)

// taskPipeline implements a reliable, in-order queue that ensures its queue
// fully drained before exiting. Stopping the taskPipeline prevents the pipeline
// from accepting any further tasks, and will cause the pipeline to exit after
// all updates have been delivered to the downstream receiver. If this process
// hangs and is unable to make progress, users can optionally call ForceQuit to
// abandon the reliable draining of the queue in order to permit shutdown.
type taskPipeline struct {
	started sync.Once
	stopped sync.Once
	forced  sync.Once

	queueMtx  sync.Mutex
	queueCond *sync.Cond
	queue     *list.List

	newBackupTasks chan *backupTask

	quit      chan struct{}
	forceQuit chan struct{}
	shutdown  chan struct{}
}

// newTaskPipeline initializes a new taskPipeline.
func newTaskPipeline() *taskPipeline {
	rq := &taskPipeline{
		queue:          list.New(),
		newBackupTasks: make(chan *backupTask),
		quit:           make(chan struct{}),
		forceQuit:      make(chan struct{}),
		shutdown:       make(chan struct{}),
	}
	rq.queueCond = sync.NewCond(&rq.queueMtx)

	return rq
}

// Start spins up the taskPipeline, making it eligible to begin receiving backup
// tasks and deliver them to the receiver of NewBackupTasks.
func (q *taskPipeline) Start() {
	q.started.Do(func() {
		go q.queueManager()
	})
}

// Stop begins a graceful shutdown of the taskPipeline. This method returns once
// all backupTasks have been delivered via NewBackupTasks, or a ForceQuit causes
// the delivery of pending tasks to be interrupted.
func (q *taskPipeline) Stop() {
	q.stopped.Do(func() {
		log.Debugf("Stopping task pipeline")

		close(q.quit)
		q.signalUntilShutdown()

		// Skip log if we also force quit.
		select {
		case <-q.forceQuit:
		default:
			log.Debugf("Task pipeline stopped successfully")
		}
	})
}

// ForceQuit signals the taskPipeline to immediately exit, dropping any
// backupTasks that have not been delivered via NewBackupTasks.
func (q *taskPipeline) ForceQuit() {
	q.forced.Do(func() {
		log.Infof("Force quitting task pipeline")

		close(q.forceQuit)
		q.signalUntilShutdown()

		log.Infof("Task pipeline unclean shutdown complete")
	})
}

// NewBackupTasks returns a read-only channel for enqueue backupTasks. The
// channel will be closed after a call to Stop and all pending tasks have been
// delivered, or if a call to ForceQuit is called before the pending entries
// have been drained.
func (q *taskPipeline) NewBackupTasks() <-chan *backupTask {
	return q.newBackupTasks
}

// QueueBackupTask enqueues a backupTask for reliable delivery to the consumer
// of NewBackupTasks. If the taskPipeline is shutting down, ErrClientExiting is
// returned. Otherwise, if QueueBackupTask returns nil it is guaranteed to be
// delivered via NewBackupTasks unless ForceQuit is called before completion.
func (q *taskPipeline) QueueBackupTask(task *backupTask) error {
	q.queueCond.L.Lock()
	select {

	// Reject new tasks after quit has been signaled.
	case <-q.quit:
		q.queueCond.L.Unlock()
		return ErrClientExiting

	// Reject new tasks after force quit has been signaled.
	case <-q.forceQuit:
		q.queueCond.L.Unlock()
		return ErrClientExiting

	default:
	}

	// Queue the new task and signal the queue's condition variable to wake up
	// the queueManager for processing.
	q.queue.PushBack(task)
	q.queueCond.L.Unlock()

	q.queueCond.Signal()

	return nil
}

// queueManager processes all incoming backup requests that get added via
// QueueBackupTask. The manager will exit
//
// NOTE: This method MUST be run as a goroutine.
func (q *taskPipeline) queueManager() {
	defer close(q.shutdown)
	defer close(q.newBackupTasks)

	for {
		q.queueCond.L.Lock()
		for q.queue.Front() == nil {
			q.queueCond.Wait()

			select {
			case <-q.quit:
				// Exit only after the queue has been fully drained.
				if q.queue.Len() == 0 {
					q.queueCond.L.Unlock()
					log.Debugf("Revoked state pipeline flushed.")
					return
				}

			case <-q.forceQuit:
				q.queueCond.L.Unlock()
				log.Debugf("Revoked state pipeline force quit.")
				return

			default:
			}
		}

		// Pop the first element from the queue.
		e := q.queue.Front()
		task := q.queue.Remove(e).(*backupTask)
		q.queueCond.L.Unlock()

		select {

		// Backup task submitted to dispatcher. We don't select on quit to
		// ensure that we still drain tasks while shutting down.
		case q.newBackupTasks <- task:

		// Force quit, return immediately to allow the client to exit.
		case <-q.forceQuit:
			log.Debugf("Revoked state pipeline force quit.")
			return
		}
	}
}

// signalUntilShutdown strobes the queue's condition variable to ensure the
// queueManager reliably unblocks to check for the exit condition.
func (q *taskPipeline) signalUntilShutdown() {
	for {
		select {
		case <-time.After(time.Millisecond):
			q.queueCond.Signal()
		case <-q.shutdown:
			return
		}
	}
}

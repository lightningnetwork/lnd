package discovery

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrVBarrierShuttingDown signals that the barrier has been requested
	// to shutdown, and that the caller should not treat the wait condition
	// as fulfilled.
	ErrVBarrierShuttingDown = errors.New("ValidationBarrier shutting down")
)

// JobID identifies an active job in the validation barrier. It is large so
// that we don't need to worry about overflows.
type JobID uint64

// jobInfo stores job dependency info for a set of dependent gossip messages.
type jobInfo struct {
	// activeParentJobIDs is the set of active parent job ids.
	activeParentJobIDs fn.Set[JobID]

	// activeDependentJobs is the set of active dependent job ids.
	activeDependentJobs fn.Set[JobID]
}

// ValidationBarrier is a barrier used to enforce a strict validation order
// while concurrently validating other updates for channel edges. It uses a set
// of maps to track validation dependencies. This is needed in practice because
// gossip messages for a given channel may arive in order, but then due to
// scheduling in different goroutines, may be validated in the wrong order.
// With the ValidationBarrier, the dependent update will wait until the parent
// update completes.
type ValidationBarrier struct {
	// validationSemaphore is a channel of structs which is used as a
	// semaphore. Initially we'll fill this with a buffered channel of the
	// size of the number of active requests. Each new job will consume
	// from this channel, then restore the value upon completion.
	validationSemaphore chan struct{}

	// jobInfoMap stores the set of job ids for each channel.
	// NOTE: This MUST be used with the mutex.
	// NOTE: This currently stores string representations of
	// lnwire.ShortChannelID and route.Vertex. Since these are of different
	// lengths, collision cannot occur in their string representations.
	// N.B.: Check that any new string-converted types don't collide with
	// existing string-converted types.
	jobInfoMap map[string]*jobInfo

	// jobDependencies is a mapping from a child's JobID to the set of
	// parent JobID that it depends on.
	// NOTE: This MUST be used with the mutex.
	jobDependencies map[JobID]fn.Set[JobID]

	// childJobChans stores the notification channel that each child job
	// listens on for parent job completions.
	// NOTE: This MUST be used with the mutex.
	childJobChans map[JobID]chan struct{}

	// idCtr is an atomic integer that is used to assign JobIDs.
	idCtr atomic.Uint64

	quit chan struct{}
	sync.Mutex
}

// NewValidationBarrier creates a new instance of a validation barrier given
// the total number of active requests, and a quit channel which will be used
// to know when to kill pending, but unfilled jobs.
func NewValidationBarrier(numActiveReqs int,
	quitChan chan struct{}) *ValidationBarrier {

	v := &ValidationBarrier{
		jobInfoMap:      make(map[string]*jobInfo),
		jobDependencies: make(map[JobID]fn.Set[JobID]),
		childJobChans:   make(map[JobID]chan struct{}),
		quit:            quitChan,
	}

	// We'll first initialize a set of semaphores to limit our concurrency
	// when validating incoming requests in parallel.
	v.validationSemaphore = make(chan struct{}, numActiveReqs)
	for i := 0; i < numActiveReqs; i++ {
		v.validationSemaphore <- struct{}{}
	}

	return v
}

// InitJobDependencies will wait for a new job slot to become open, and then
// sets up any dependent signals/trigger for the new job.
func (v *ValidationBarrier) InitJobDependencies(job interface{}) (JobID,
	error) {

	// We'll wait for either a new slot to become open, or for the quit
	// channel to be closed.
	select {
	case <-v.validationSemaphore:
	case <-v.quit:
	}

	v.Lock()
	defer v.Unlock()

	// updateOrCreateJobInfo modifies the set of activeParentJobs for this
	// annID and updates jobInfoMap.
	updateOrCreateJobInfo := func(annID string, annJobID JobID) {
		info, ok := v.jobInfoMap[annID]
		if ok {
			// If an entry already exists for annID, then a job
			// related to it is being validated. Add to the set of
			// parent job ids. This addition will only affect
			// _later_, _child_ jobs for the annID.
			info.activeParentJobIDs.Add(annJobID)
			return
		}

		// No entry exists for annID, meaning that we should create
		// one.
		parentJobSet := fn.NewSet(annJobID)

		info = &jobInfo{
			activeParentJobIDs:  parentJobSet,
			activeDependentJobs: fn.NewSet[JobID](),
		}
		v.jobInfoMap[annID] = info
	}

	// populateDependencies populates the job dependency mappings (i.e.
	// which should complete after another) for the (annID, childJobID)
	// tuple.
	populateDependencies := func(annID string, childJobID JobID) {
		// If there is no entry in the jobInfoMap, we don't have to
		// wait on any parent jobs to finish.
		info, ok := v.jobInfoMap[annID]
		if !ok {
			return
		}

		// We want to see a snapshot of active parent jobs for this
		// annID that are already registered in activeParentJobIDs. The
		// child job identified by childJobID can only run after these
		// parent jobs have run. After grabbing the snapshot, we then
		// want to persist a slice of these jobs.

		// Create the notification chan that parent jobs will send (or
		// close) on when they complete.
		jobChan := make(chan struct{})

		// Add to set of activeDependentJobs for this annID.
		info.activeDependentJobs.Add(childJobID)

		// Store in childJobChans. The parent jobs will fetch this chan
		// to notify on. The child job will later fetch this chan to
		// listen on when WaitForParents is called.
		v.childJobChans[childJobID] = jobChan

		// Copy over the parent job IDs at this moment for this annID.
		// This job must be processed AFTER those parent IDs.
		parentJobs := info.activeParentJobIDs.Copy()

		// Populate the jobDependencies mapping.
		v.jobDependencies[childJobID] = parentJobs
	}

	// Once a slot is open, we'll examine the message of the job, to see if
	// there need to be any dependent barriers set up.
	switch msg := job.(type) {
	case *lnwire.ChannelAnnouncement1:
		id := JobID(v.idCtr.Add(1))

		updateOrCreateJobInfo(msg.ShortChannelID.String(), id)
		updateOrCreateJobInfo(route.Vertex(msg.NodeID1).String(), id)
		updateOrCreateJobInfo(route.Vertex(msg.NodeID2).String(), id)

		return id, nil

	// Populate the dependency mappings for the below child jobs.
	case *lnwire.ChannelUpdate1:
		childJobID := JobID(v.idCtr.Add(1))
		populateDependencies(msg.ShortChannelID.String(), childJobID)

		return childJobID, nil
	case *lnwire.NodeAnnouncement1:
		childJobID := JobID(v.idCtr.Add(1))
		populateDependencies(
			route.Vertex(msg.NodeID).String(), childJobID,
		)

		return childJobID, nil
	case *lnwire.AnnounceSignatures1:
		// TODO(roasbeef): need to wait on chan ann?
		// - We can do the above by calling populateDependencies. For
		//   now, while we evaluate potential side effects, don't do
		//   anything with childJobID and just return it.
		childJobID := JobID(v.idCtr.Add(1))
		return childJobID, nil

	default:
		// An invalid message was passed into InitJobDependencies.
		// Return an error.
		return JobID(0), errors.New("invalid message")
	}
}

// CompleteJob returns a free slot to the set of available job slots. This
// should be called once a job has been fully completed. Otherwise, slots may
// not be returned to the internal scheduling, causing a deadlock when a new
// overflow job is attempted.
func (v *ValidationBarrier) CompleteJob() {
	select {
	case v.validationSemaphore <- struct{}{}:
	case <-v.quit:
	}
}

// WaitForParents will block until all parent job dependencies have went
// through the validation pipeline. This allows us a graceful way to run jobs
// in goroutines and still have strict ordering guarantees. If this job doesn't
// have any parent job dependencies, then this function will return
// immediately.
func (v *ValidationBarrier) WaitForParents(childJobID JobID,
	job interface{}) error {

	var (
		ok      bool
		jobDesc string

		parentJobIDs fn.Set[JobID]
		annID        string
		jobChan      chan struct{}
	)

	// Acquire a lock to read ValidationBarrier.
	v.Lock()

	switch msg := job.(type) {
	// Any ChannelUpdate or NodeAnnouncement1 jobs will need to wait on the
	// completion of any active ChannelAnnouncement jobs related to them.
	case *lnwire.ChannelUpdate1:
		annID = msg.ShortChannelID.String()

		parentJobIDs, ok = v.jobDependencies[childJobID]
		if !ok {
			// If ok is false, it means that this child job never
			// had any parent jobs to wait on.
			v.Unlock()
			return nil
		}

		jobDesc = fmt.Sprintf("job=lnwire.ChannelUpdate, scid=%v",
			msg.ShortChannelID.ToUint64())

	case *lnwire.NodeAnnouncement1:
		annID = route.Vertex(msg.NodeID).String()

		parentJobIDs, ok = v.jobDependencies[childJobID]
		if !ok {
			// If ok is false, it means that this child job never
			// had any parent jobs to wait on.
			v.Unlock()
			return nil
		}

		jobDesc = fmt.Sprintf("job=lnwire.NodeAnnouncement1, pub=%s",
			route.Vertex(msg.NodeID))

	// Other types of jobs can be executed immediately, so we'll just
	// return directly.
	case *lnwire.AnnounceSignatures1:
		// TODO(roasbeef): need to wait on chan ann?
		v.Unlock()
		return nil

	case *lnwire.ChannelAnnouncement1:
		v.Unlock()
		return nil
	}

	// Release the lock once the above read is finished.
	v.Unlock()

	log.Debugf("Waiting for dependent on %s", jobDesc)

	v.Lock()
	jobChan, ok = v.childJobChans[childJobID]
	if !ok {
		v.Unlock()

		// The entry may not exist because this job does not depend on
		// any parent jobs.
		return nil
	}
	v.Unlock()

	for {
		select {
		case <-v.quit:
			return ErrVBarrierShuttingDown

		case <-jobChan:
			// Every time this is sent on or if it's closed, a
			// parent job has finished. The parent jobs have to
			// also potentially close the channel because if all
			// the parent jobs finish and call SignalDependents
			// before the goroutine running WaitForParents has a
			// chance to grab the notification chan from
			// childJobChans, then the running goroutine will wait
			// here for a notification forever. By having the last
			// parent job close the notificiation chan, we avoid
			// this issue.

			// Check and see if we have any parent jobs left. If we
			// don't, we can finish up.
			v.Lock()
			info, found := v.jobInfoMap[annID]
			if !found {
				v.Unlock()

				// No parent job info found, proceed with
				// validation.
				return nil
			}

			x := parentJobIDs.Intersect(info.activeParentJobIDs)
			v.Unlock()
			if x.IsEmpty() {
				// The parent jobs have all completed. We can
				// proceed with validation.
				return nil
			}

			// If we've reached this point, we are still waiting on
			// a parent job to complete.
		}
	}
}

// SignalDependents signals to any child jobs that this parent job has
// finished.
func (v *ValidationBarrier) SignalDependents(job interface{}, id JobID) error {
	v.Lock()
	defer v.Unlock()

	// removeJob either removes a child job or a parent job. If it is
	// removing a child job, then it removes the child's JobID from the set
	// of dependent jobs for the announcement ID. If this is removing a
	// parent job, then it removes the parentJobID from the set of active
	// parent jobs and notifies the child jobs that it has finished
	// validating.
	removeJob := func(annID string, id JobID, child bool) error {
		if child {
			// If we're removing a child job, check jobInfoMap and
			// remove this job from activeDependentJobs.
			info, ok := v.jobInfoMap[annID]
			if ok {
				info.activeDependentJobs.Remove(id)
			}

			// Remove the notification chan from childJobChans.
			delete(v.childJobChans, id)

			// Remove this job's dependency mapping.
			delete(v.jobDependencies, id)

			return nil
		}

		// Otherwise, we are removing a parent job.
		jobInfo, found := v.jobInfoMap[annID]
		if !found {
			// NOTE: Some sort of consistency guarantee has been
			// broken.
			return fmt.Errorf("no job info found for "+
				"identifier(%v)", id)
		}

		jobInfo.activeParentJobIDs.Remove(id)

		lastJob := jobInfo.activeParentJobIDs.IsEmpty()

		// Notify all dependent jobs that a parent job has completed.
		for child := range jobInfo.activeDependentJobs {
			notifyChan, ok := v.childJobChans[child]
			if !ok {
				// NOTE: Some sort of consistency guarantee has
				// been broken.
				return fmt.Errorf("no job info found for "+
					"identifier(%v)", id)
			}

			// We don't want to block when sending out the signal.
			select {
			case notifyChan <- struct{}{}:
			default:
			}

			// If this is the last parent job for this annID, also
			// close the channel. This is needed because it's
			// possible that the parent job cleans up the job
			// mappings before the goroutine handling the child job
			// has a chance to call WaitForParents and catch the
			// signal sent above. We are allowed to close because
			// no other parent job will be able to send along the
			// channel (or close) as we're removing the entry from
			// the jobInfoMap below.
			if lastJob {
				close(notifyChan)
			}
		}

		// Remove from jobInfoMap if last job.
		if lastJob {
			delete(v.jobInfoMap, annID)
		}

		return nil
	}

	switch msg := job.(type) {
	case *lnwire.ChannelAnnouncement1:
		// Signal to the child jobs that parent validation has
		// finished. We have to call removeJob for each annID
		// that this ChannelAnnouncement can be associated with.
		err := removeJob(msg.ShortChannelID.String(), id, false)
		if err != nil {
			return err
		}

		err = removeJob(route.Vertex(msg.NodeID1).String(), id, false)
		if err != nil {
			return err
		}

		err = removeJob(route.Vertex(msg.NodeID2).String(), id, false)
		if err != nil {
			return err
		}

		return nil

	case *lnwire.NodeAnnouncement1:
		// Remove child job info.
		return removeJob(route.Vertex(msg.NodeID).String(), id, true)

	case *lnwire.ChannelUpdate1:
		// Remove child job info.
		return removeJob(msg.ShortChannelID.String(), id, true)

	case *lnwire.AnnounceSignatures1:
		// No dependency mappings are stored for AnnounceSignatures1,
		// so do nothing.
		return nil
	}

	return errors.New("invalid message - no job dependencies")
}

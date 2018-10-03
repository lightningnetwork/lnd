package lnwallet

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// jobBuffer is a constant the represents the buffer of jobs in the two
	// main queues. This allows clients avoid necessarily blocking when
	// submitting jobs into the queue.
	jobBuffer = 100

	// TODO(roasbeef): job buffer pool?
)

// verifyJob is a job sent to the sigPool to verify a signature on a
// transaction. The items contained in the struct are necessary and sufficient
// to verify the full signature. The passed sigHash closure function should be
// set to a function that generates the relevant sighash.
//
// TODO(roasbeef): when we move to ecschnorr, make into batch signature
// verification using bos-coster (or pip?).
type verifyJob struct {
	// pubKey is the public key that was used to generate the purported
	// valid signature. Note that with the current channel construction,
	// this public key will likely have been tweaked using the current per
	// commitment point for a particular commitment transactions.
	pubKey *btcec.PublicKey

	// sig is the raw signature generated using the above public key.  This
	// is the signature to be verified.
	sig *btcec.Signature

	// sigHash is a function closure generates the sighashes that the
	// passed signature is known to have signed.
	sigHash func() ([]byte, error)

	// htlcIndex is the index of the HTLC from the PoV of the remote
	// party's update log.
	htlcIndex uint64

	// cancel is a channel that should be closed if the caller wishes to
	// cancel all pending verification jobs part of a single batch. This
	// channel is to be closed in the case that a single signature in a
	// batch has been returned as invalid, as there is no need to verify
	// the remainder of the signatures.
	cancel chan struct{}

	// errResp is the channel that the result of the signature verification
	// is to be sent over. In the see that the signature is valid, a nil
	// error will be passed. Otherwise, a concrete error detailing the
	// issue will be passed.
	errResp chan *htlcIndexErr
}

// verifyJobErr is a special type of error that also includes a pointer to the
// original validation job. Ths error message allows us to craft more detailed
// errors at upper layers.
type htlcIndexErr struct {
	error

	*verifyJob
}

// signJob is a job sent to the sigPool to generate a valid signature according
// to the passed SignDescriptor for the passed transaction. Jobs are intended
// to be sent in batches in order to parallelize the job of generating
// signatures for a new commitment transaction.
type signJob struct {
	// signDesc is intended to be a full populated SignDescriptor which
	// encodes the necessary material (keys, witness script, etc) required
	// to generate a valid signature for the specified input.
	signDesc SignDescriptor

	// tx is the transaction to be signed. This is required to generate the
	// proper sighash for the input to be signed.
	tx *wire.MsgTx

	// outputIndex is the output index of the HTLC on the commitment
	// transaction being signed.
	outputIndex int32

	// cancel is a channel that should be closed if the caller wishes to
	// abandon all pending sign jobs part of a single batch.
	cancel chan struct{}

	// resp is the channel that the response to this particular signJob
	// will be sent over.
	//
	// TODO(roasbeef): actually need to allow caller to set, need to retain
	// order mark commit sig as special
	resp chan signJobResp
}

// signJobResp is the response to a sign job. Both channels are to be read in
// order to ensure no unnecessary goroutine blocking occurs. Additionally, both
// channels should be buffered.
type signJobResp struct {
	// sig is the generated signature for a particular signJob In the case
	// of an error during signature generation, then this value sent will
	// be nil.
	sig lnwire.Sig

	// err is the error that occurred when executing the specified
	// signature job. In the case that no error occurred, this value will
	// be nil.
	err error
}

// sigPool is a struct that is meant to allow the current channel state machine
// to parallelize all signature generation and verification. This struct is
// needed as _each_ HTLC when creating a commitment transaction requires a
// signature, and similarly a receiver of a new commitment must verify all the
// HTLC signatures included within the CommitSig message. A pool of workers
// will be maintained by the sigPool. Batches of jobs (either to sign or
// verify) can be sent to the pool of workers which will asynchronously perform
// the specified job.
//
// TODO(roasbeef): rename?
//  * ecdsaPool?
type sigPool struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	signer Signer

	verifyJobs chan verifyJob
	signJobs   chan signJob

	wg   sync.WaitGroup
	quit chan struct{}

	numWorkers int
}

// newSigPool creates a new signature pool with the specified number of
// workers. The recommended parameter for the number of works is the number of
// physical CPU cores available on the target machine.
func newSigPool(numWorkers int, signer Signer) *sigPool {
	return &sigPool{
		signer:     signer,
		numWorkers: numWorkers,
		verifyJobs: make(chan verifyJob, jobBuffer),
		signJobs:   make(chan signJob, jobBuffer),
		quit:       make(chan struct{}),
	}
}

// Start starts of all goroutines that the sigPool needs to carry out its
// duties.
func (s *sigPool) Start() error {
	if !atomic.CompareAndSwapUint32(&s.started, 0, 1) {
		return nil
	}

	for i := 0; i < s.numWorkers; i++ {
		s.wg.Add(1)
		go s.poolWorker()
	}

	return nil
}

// Stop signals any active workers carrying out jobs to exit so the sigPool can
// gracefully shutdown.
func (s *sigPool) Stop() error {
	if !atomic.CompareAndSwapUint32(&s.stopped, 0, 1) {
		return nil
	}

	close(s.quit)
	s.wg.Wait()

	return nil
}

// poolWorker is the main worker goroutine within the sigPool. Individual
// batches are distributed amongst each of the active workers. The workers then
// execute the task based on the type of job, and return the result back to
// caller.
func (s *sigPool) poolWorker() {
	defer s.wg.Done()

	for {
		select {

		// We've just received a new signature job. Given the items
		// contained within the message, we'll craft a signature and
		// send the result along with a possible error back to the
		// caller.
		case sigMsg := <-s.signJobs:
			rawSig, err := s.signer.SignOutputRaw(sigMsg.tx,
				&sigMsg.signDesc)
			if err != nil {
				select {
				case sigMsg.resp <- signJobResp{
					sig: lnwire.Sig{},
					err: err,
				}:
					continue
				case <-sigMsg.cancel:
					continue
				case <-s.quit:
					return
				}
			}

			sig, err := lnwire.NewSigFromRawSignature(rawSig)
			select {
			case sigMsg.resp <- signJobResp{
				sig: sig,
				err: err,
			}:
			case <-sigMsg.cancel:
				continue
			case <-s.quit:
				return
			}

		// We've just received a new verification job from the outside
		// world. We'll attempt to construct the sighash, parse the
		// signature, and finally verify the signature.
		case verifyMsg := <-s.verifyJobs:
			sigHash, err := verifyMsg.sigHash()
			if err != nil {
				select {
				case verifyMsg.errResp <- &htlcIndexErr{
					error:     err,
					verifyJob: &verifyMsg,
				}:
					continue
				case <-verifyMsg.cancel:
					continue
				}
			}

			rawSig := verifyMsg.sig

			if !rawSig.Verify(sigHash, verifyMsg.pubKey) {
				err := fmt.Errorf("invalid signature "+
					"sighash: %x, sig: %x", sigHash, rawSig.Serialize())
				select {
				case verifyMsg.errResp <- &htlcIndexErr{
					error:     err,
					verifyJob: &verifyMsg,
				}:
				case <-verifyMsg.cancel:
				case <-s.quit:
					return
				}
			} else {
				select {
				case verifyMsg.errResp <- nil:
				case <-verifyMsg.cancel:
				case <-s.quit:
					return
				}
			}

		// The sigPool is exiting, so we will as well.
		case <-s.quit:
			return
		}
	}
}

// SubmitSignBatch submits a batch of signature jobs to the sigPool. The
// response and cancel channels for each of the signJob's are expected to be
// fully populated, as the response for each job will be sent over the response
// channel within the job itself.
func (s *sigPool) SubmitSignBatch(signJobs []signJob) {
	for _, job := range signJobs {
		select {
		case s.signJobs <- job:
		case <-job.cancel:
			// TODO(roasbeef): return error?
		case <-s.quit:
			return
		}
	}
}

// SubmitVerifyBatch submits a batch of verification jobs to the sigPool. For
// each job submitted, an error will be passed into the returned channel
// denoting if signature verification was valid or not. The passed cancelChan
// allows the caller to cancel all pending jobs in the case that they wish to
// bail early.
func (s *sigPool) SubmitVerifyBatch(verifyJobs []verifyJob,
	cancelChan chan struct{}) <-chan *htlcIndexErr {

	errChan := make(chan *htlcIndexErr, len(verifyJobs))

	for _, job := range verifyJobs {
		job.cancel = cancelChan
		job.errResp = errChan

		select {
		case s.verifyJobs <- job:
		case <-job.cancel:
			return errChan
		}
	}

	return errChan
}

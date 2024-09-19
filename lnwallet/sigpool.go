package lnwallet

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// jobBuffer is a constant the represents the buffer of jobs in the two
	// main queues. This allows clients avoid necessarily blocking when
	// submitting jobs into the queue.
	jobBuffer = 100

	// TODO(roasbeef): job buffer pool?
)

// VerifyJob is a job sent to the sigPool sig pool to verify a signature
// on a transaction. The items contained in the struct are necessary and
// sufficient to verify the full signature. The passed sigHash closure function
// should be set to a function that generates the relevant sighash.
//
// TODO(roasbeef): when we move to ecschnorr, make into batch signature
// verification using bos-coster (or pip?).
type VerifyJob struct {
	// PubKey is the public key that was used to generate the purported
	// valid signature. Note that with the current channel construction,
	// this public key will likely have been tweaked using the current per
	// commitment point for a particular commitment transactions.
	PubKey *btcec.PublicKey

	// Sig is the raw signature generated using the above public key.  This
	// is the signature to be verified.
	Sig input.Signature

	// SigHash is a function closure generates the sighashes that the
	// passed signature is known to have signed.
	SigHash func() ([]byte, error)

	// HtlcIndex is the index of the HTLC from the PoV of the remote
	// party's update log.
	HtlcIndex uint64

	// Cancel is a channel that is closed by the caller if they wish to
	// cancel all pending verification jobs part of a single batch. This
	// channel is closed in the case that a single signature in a batch has
	// been returned as invalid, as there is no need to verify the remainder
	// of the signatures.
	Cancel <-chan struct{}

	// ErrResp is the channel that the result of the signature verification
	// is to be sent over. In the see that the signature is valid, a nil
	// error will be passed. Otherwise, a concrete error detailing the
	// issue will be passed. This channel MUST be buffered.
	ErrResp chan *HtlcIndexErr
}

// HtlcIndexErr is a special type of error that also includes a pointer to the
// original validation job. This error message allows us to craft more detailed
// errors at upper layers.
type HtlcIndexErr struct {
	error

	*VerifyJob
}

// SignJob is a job sent to the sigPool sig pool to generate a valid
// signature according to the passed SignDescriptor for the passed transaction.
// Jobs are intended to be sent in batches in order to parallelize the job of
// generating signatures for a new commitment transaction.
type SignJob struct {
	// SignDesc is intended to be a full populated SignDescriptor which
	// encodes the necessary material (keys, witness script, etc) required
	// to generate a valid signature for the specified input.
	SignDesc input.SignDescriptor

	// Tx is the transaction to be signed. This is required to generate the
	// proper sighash for the input to be signed.
	Tx *wire.MsgTx

	// OutputIndex is the output index of the HTLC on the commitment
	// transaction being signed.
	OutputIndex int32

	// Cancel is a channel that is closed by the caller if they wish to
	// abandon all pending sign jobs part of a single batch. This should
	// never be closed by the validator.
	Cancel <-chan struct{}

	// Resp is the channel that the response to this particular SignJob
	// will be sent over. This channel MUST be buffered.
	//
	// TODO(roasbeef): actually need to allow caller to set, need to retain
	// order mark commit sig as special
	Resp chan SignJobResp
}

// SignJobResp is the response to a sign job. Both channels are to be read in
// order to ensure no unnecessary goroutine blocking occurs. Additionally, both
// channels should be buffered.
type SignJobResp struct {
	// Sig is the generated signature for a particular SignJob In the case
	// of an error during signature generation, then this value sent will
	// be nil.
	Sig lnwire.Sig

	// Err is the error that occurred when executing the specified
	// signature job. In the case that no error occurred, this value will
	// be nil.
	Err error
}

// SigPool is a struct that is meant to allow the current channel state
// machine to parallelize all signature generation and verification. This
// struct is needed as _each_ HTLC when creating a commitment transaction
// requires a signature, and similarly a receiver of a new commitment must
// verify all the HTLC signatures included within the CommitSig message. A pool
// of workers will be maintained by the sigPool. Batches of jobs (either
// to sign or verify) can be sent to the pool of workers which will
// asynchronously perform the specified job.
type SigPool struct {
	started sync.Once
	stopped sync.Once

	signer input.Signer

	verifyJobs chan VerifyJob
	signJobs   chan SignJob

	wg   sync.WaitGroup
	quit chan struct{}

	numWorkers int
}

// NewSigPool creates a new signature pool with the specified number of
// workers. The recommended parameter for the number of works is the number of
// physical CPU cores available on the target machine.
func NewSigPool(numWorkers int, signer input.Signer) *SigPool {
	return &SigPool{
		signer:     signer,
		numWorkers: numWorkers,
		verifyJobs: make(chan VerifyJob, jobBuffer),
		signJobs:   make(chan SignJob, jobBuffer),
		quit:       make(chan struct{}),
	}
}

// Start starts of all goroutines that the sigPool sig pool needs to
// carry out its duties.
func (s *SigPool) Start() error {
	s.started.Do(func() {
		walletLog.Info("SigPool starting")
		for i := 0; i < s.numWorkers; i++ {
			s.wg.Add(1)
			go s.poolWorker()
		}
	})
	return nil
}

// Stop signals any active workers carrying out jobs to exit so the sigPool can
// gracefully shutdown.
func (s *SigPool) Stop() error {
	s.stopped.Do(func() {
		close(s.quit)
		s.wg.Wait()
	})
	return nil
}

// poolWorker is the main worker goroutine within the sigPool sig pool.
// Individual batches are distributed amongst each of the active workers. The
// workers then execute the task based on the type of job, and return the
// result back to caller.
func (s *SigPool) poolWorker() {
	defer s.wg.Done()

	for {
		select {

		// We've just received a new signature job. Given the items
		// contained within the message, we'll craft a signature and
		// send the result along with a possible error back to the
		// caller.
		case sigMsg := <-s.signJobs:
			rawSig, err := s.signer.SignOutputRaw(
				sigMsg.Tx, &sigMsg.SignDesc,
			)
			if err != nil {
				select {
				case sigMsg.Resp <- SignJobResp{
					Sig: lnwire.Sig{},
					Err: err,
				}:
					continue
				case <-sigMsg.Cancel:
					continue
				case <-s.quit:
					return
				}
			}

			// Use the sig mapper to go from the input.Signature
			// into the serialized lnwire.Sig that we'll send
			// across the wire.
			sig, err := lnwire.NewSigFromSignature(rawSig)

			select {
			case sigMsg.Resp <- SignJobResp{
				Sig: sig,
				Err: err,
			}:
			case <-sigMsg.Cancel:
				continue
			case <-s.quit:
				return
			}

		// We've just received a new verification job from the outside
		// world. We'll attempt to construct the sighash, parse the
		// signature, and finally verify the signature.
		case verifyMsg := <-s.verifyJobs:
			sigHash, err := verifyMsg.SigHash()
			if err != nil {
				select {
				case verifyMsg.ErrResp <- &HtlcIndexErr{
					error:     err,
					VerifyJob: &verifyMsg,
				}:
					continue
				case <-verifyMsg.Cancel:
					continue
				}
			}

			rawSig := verifyMsg.Sig

			if !rawSig.Verify(sigHash, verifyMsg.PubKey) {
				err := fmt.Errorf("invalid signature "+
					"sighash: %x, sig: %x", sigHash,
					rawSig.Serialize())

				select {
				case verifyMsg.ErrResp <- &HtlcIndexErr{
					error:     err,
					VerifyJob: &verifyMsg,
				}:
				case <-verifyMsg.Cancel:
				case <-s.quit:
					return
				}
			} else {
				select {
				case verifyMsg.ErrResp <- nil:
				case <-verifyMsg.Cancel:
				case <-s.quit:
					return
				}
			}

		// The sigPool sig pool is exiting, so we will as well.
		case <-s.quit:
			return
		}
	}
}

// SubmitSignBatch submits a batch of signature jobs to the sigPool.  The
// response and cancel channels for each of the SignJob's are expected to be
// fully populated, as the response for each job will be sent over the
// response channel within the job itself.
func (s *SigPool) SubmitSignBatch(signJobs []SignJob) {
	for _, job := range signJobs {
		select {
		case s.signJobs <- job:
		case <-job.Cancel:
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
func (s *SigPool) SubmitVerifyBatch(verifyJobs []VerifyJob,
	cancelChan chan struct{}) <-chan *HtlcIndexErr {

	errChan := make(chan *HtlcIndexErr, len(verifyJobs))

	for _, job := range verifyJobs {
		job.Cancel = cancelChan
		job.ErrResp = errChan

		select {
		case s.verifyJobs <- job:
		case <-job.Cancel:
			return errChan
		}
	}

	return errChan
}

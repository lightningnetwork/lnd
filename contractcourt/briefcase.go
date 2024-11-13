package contractcourt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/tlv"
)

// ContractResolutions is a wrapper struct around the two forms of resolutions
// we may need to carry out once a contract is closing: resolving the
// commitment output, and resolving any incoming+outgoing HTLC's still present
// in the commitment.
type ContractResolutions struct {
	// CommitHash is the txid of the commitment transaction.
	CommitHash chainhash.Hash

	// CommitResolution contains all data required to fully resolve a
	// commitment output.
	CommitResolution *lnwallet.CommitOutputResolution

	// HtlcResolutions contains all data required to fully resolve any
	// incoming+outgoing HTLC's present within the commitment transaction.
	HtlcResolutions lnwallet.HtlcResolutions

	// AnchorResolution contains the data required to sweep the anchor
	// output. If the channel type doesn't include anchors, the value of
	// this field will be nil.
	AnchorResolution *lnwallet.AnchorResolution

	// BreachResolution contains the data required to manage the lifecycle
	// of a breach in the ChannelArbitrator.
	BreachResolution *BreachResolution
}

// IsEmpty returns true if the set of resolutions is "empty". A resolution is
// empty if: our commitment output has been trimmed, we don't have any
// incoming or outgoing HTLC's active, there is no anchor output to sweep, or
// there are no breached outputs to resolve.
func (c *ContractResolutions) IsEmpty() bool {
	return c.CommitResolution == nil &&
		len(c.HtlcResolutions.IncomingHTLCs) == 0 &&
		len(c.HtlcResolutions.OutgoingHTLCs) == 0 &&
		c.AnchorResolution == nil && c.BreachResolution == nil
}

// ArbitratorLog is the primary source of persistent storage for the
// ChannelArbitrator. The log stores the current state of the
// ChannelArbitrator's internal state machine, any items that are required to
// properly make a state transition, and any unresolved contracts.
type ArbitratorLog interface {
	// TODO(roasbeef): document on interface the errors expected to be
	// returned

	// CurrentState returns the current state of the ChannelArbitrator. It
	// takes an optional database transaction, which will be used if it is
	// non-nil, otherwise the lookup will be done in its own transaction.
	CurrentState(tx kvdb.RTx) (ArbitratorState, error)

	// CommitState persists, the current state of the chain attendant.
	CommitState(ArbitratorState) error

	// InsertUnresolvedContracts inserts a set of unresolved contracts into
	// the log. The log will then persistently store each contract until
	// they've been swapped out, or resolved. It takes a set of report which
	// should be written to disk if as well if it is non-nil.
	InsertUnresolvedContracts(reports []*channeldb.ResolverReport,
		resolvers ...ContractResolver) error

	// FetchUnresolvedContracts returns all unresolved contracts that have
	// been previously written to the log.
	FetchUnresolvedContracts() ([]ContractResolver, error)

	// SwapContract performs an atomic swap of the old contract for the new
	// contract. This method is used when after a contract has been fully
	// resolved, it produces another contract that needs to be resolved.
	SwapContract(old ContractResolver, new ContractResolver) error

	// ResolveContract marks a contract as fully resolved. Once a contract
	// has been fully resolved, it is deleted from persistent storage.
	ResolveContract(ContractResolver) error

	// LogContractResolutions stores a complete contract resolution for the
	// contract under watch. This method will be called once the
	// ChannelArbitrator either force closes a channel, or detects that the
	// remote party has broadcast their commitment on chain.
	LogContractResolutions(*ContractResolutions) error

	// FetchContractResolutions fetches the set of previously stored
	// contract resolutions from persistent storage.
	FetchContractResolutions() (*ContractResolutions, error)

	// InsertConfirmedCommitSet stores the known set of active HTLCs at the
	// time channel closure. We'll use this to reconstruct our set of chain
	// actions anew based on the confirmed and pending commitment state.
	InsertConfirmedCommitSet(c *CommitSet) error

	// FetchConfirmedCommitSet fetches the known confirmed active HTLC set
	// from the database. It takes an optional database transaction, which
	// will be used if it is non-nil, otherwise the lookup will be done in
	// its own transaction.
	FetchConfirmedCommitSet(tx kvdb.RTx) (*CommitSet, error)

	// FetchChainActions attempts to fetch the set of previously stored
	// chain actions. We'll use this upon restart to properly advance our
	// state machine forward.
	//
	// NOTE: This method only exists in order to be able to serve nodes had
	// channels in the process of closing before the CommitSet struct was
	// introduced.
	FetchChainActions() (ChainActionMap, error)

	// WipeHistory is to be called ONLY once *all* contracts have been
	// fully resolved, and the channel closure if finalized. This method
	// will delete all on-disk state within the persistent log.
	WipeHistory() error
}

// ArbitratorState is an enum that details the current state of the
// ChannelArbitrator's state machine.
type ArbitratorState uint8

const (
	// While some state transition is allowed, certain transitions are not
	// possible. Listed below is the full state transition map which
	// contains all possible paths. We start at StateDefault and end at
	// StateFullyResolved, or StateError(not listed as its a possible state
	// in every path). The format is,
	// 	-> state: conditions we switch to this state.
	//
	// StateDefault
	// |
	// |-> StateDefault: no actions and chain trigger
	// |
	// |-> StateBroadcastCommit: chain/user trigger
	// |   |
	// |   |-> StateCommitmentBroadcasted: chain/user trigger
	// |   |   |
	// |   |   |-> StateCommitmentBroadcasted: chain/user trigger
	// |   |   |
	// |   |   |-> StateContractClosed: local/remote/breach close trigger
	// |   |   |   |
	// |   |   |   |-> StateWaitingFullResolution: contract resolutions not empty
	// |   |   |   |   |
	// |   |   |   |   |-> StateWaitingFullResolution: contract resolutions not empty
	// |   |   |   |   |
	// |   |   |   |   |-> StateFullyResolved: contract resolutions empty
	// |   |   |   |
	// |   |   |   |-> StateFullyResolved: contract resolutions empty
	// |   |   |
	// |   |   |-> StateFullyResolved: coop/breach(legacy) close trigger
	// |   |
	// |   |-> StateContractClosed: local/remote/breach close trigger
	// |   |   |
	// |   |   |-> StateWaitingFullResolution: contract resolutions not empty
	// |   |   |   |
	// |   |   |   |-> StateWaitingFullResolution: contract resolutions not empty
	// |   |   |   |
	// |   |   |   |-> StateFullyResolved: contract resolutions empty
	// |   |   |
	// |   |   |-> StateFullyResolved: contract resolutions empty
	// |   |
	// |   |-> StateFullyResolved: coop/breach(legacy) close trigger
	// |
	// |-> StateContractClosed: local/remote/breach close trigger
	// |   |
	// |   |-> StateWaitingFullResolution: contract resolutions not empty
	// |   |   |
	// |   |   |-> StateWaitingFullResolution: contract resolutions not empty
	// |   |   |
	// |   |   |-> StateFullyResolved: contract resolutions empty
	// |   |
	// |   |-> StateFullyResolved: contract resolutions empty
	// |
	// |-> StateFullyResolved: coop/breach(legacy) close trigger.

	// StateDefault is the default state. In this state, no major actions
	// need to be executed.
	StateDefault ArbitratorState = 0

	// StateBroadcastCommit is a state that indicates that the attendant
	// has decided to broadcast the commitment transaction, but hasn't done
	// so yet.
	StateBroadcastCommit ArbitratorState = 1

	// StateCommitmentBroadcasted is a state that indicates that the
	// attendant has broadcasted the commitment transaction, and is now
	// waiting for it to confirm.
	StateCommitmentBroadcasted ArbitratorState = 6

	// StateContractClosed is a state that indicates the contract has
	// already been "closed", meaning the commitment is confirmed on chain.
	// At this point, we can now examine our active contracts, in order to
	// create the proper resolver for each one.
	StateContractClosed ArbitratorState = 2

	// StateWaitingFullResolution is a state that indicates that the
	// commitment transaction has been confirmed, and the attendant is now
	// waiting for all unresolved contracts to be fully resolved.
	StateWaitingFullResolution ArbitratorState = 3

	// StateFullyResolved is the final state of the attendant. In this
	// state, all related contracts have been resolved, and the attendant
	// can now be garbage collected.
	StateFullyResolved ArbitratorState = 4

	// StateError is the only error state of the resolver. If we enter this
	// state, then we cannot proceed with manual intervention as a state
	// transition failed.
	StateError ArbitratorState = 5
)

// String returns a human readable string describing the ArbitratorState.
func (a ArbitratorState) String() string {
	switch a {
	case StateDefault:
		return "StateDefault"

	case StateBroadcastCommit:
		return "StateBroadcastCommit"

	case StateCommitmentBroadcasted:
		return "StateCommitmentBroadcasted"

	case StateContractClosed:
		return "StateContractClosed"

	case StateWaitingFullResolution:
		return "StateWaitingFullResolution"

	case StateFullyResolved:
		return "StateFullyResolved"

	case StateError:
		return "StateError"

	default:
		return "unknown state"
	}
}

// IsContractClosed returns a bool to indicate whether the closing/breaching tx
// has been confirmed onchain. If the state is StateContractClosed,
// StateWaitingFullResolution, or StateFullyResolved, it means the contract has
// been closed and all related contracts have been launched.
func (a ArbitratorState) IsContractClosed() bool {
	return a == StateContractClosed || a == StateWaitingFullResolution ||
		a == StateFullyResolved
}

// resolverType is an enum that enumerates the various types of resolvers. When
// writing resolvers to disk, we prepend this to the raw bytes stored. This
// allows us to properly decode the resolver into the proper type.
type resolverType uint8

const (
	// resolverTimeout is the type of a resolver that's tasked with
	// resolving an outgoing HTLC that is very close to timing out.
	resolverTimeout resolverType = 0

	// resolverSuccess is the type of a resolver that's tasked with
	// resolving an incoming HTLC that we already know the preimage of.
	resolverSuccess resolverType = 1

	// resolverOutgoingContest is the type of a resolver that's tasked with
	// resolving an outgoing HTLC that hasn't yet timed out.
	resolverOutgoingContest resolverType = 2

	// resolverIncomingContest is the type of a resolver that's tasked with
	// resolving an incoming HTLC that we don't yet know the preimage to.
	resolverIncomingContest resolverType = 3

	// resolverUnilateralSweep is the type of resolver that's tasked with
	// sweeping out direct commitment output form the remote party's
	// commitment transaction.
	resolverUnilateralSweep resolverType = 4

	// resolverBreach is the type of resolver that manages a contract
	// breach on-chain.
	resolverBreach resolverType = 5
)

// resolverIDLen is the size of the resolver ID key. This is 36 bytes as we get
// 32 bytes from the hash of the prev tx, and 4 bytes for the output index.
const resolverIDLen = 36

// resolverID is a key that uniquely identifies a resolver within a particular
// chain. For this value we use the full outpoint of the resolver.
type resolverID [resolverIDLen]byte

// newResolverID returns a resolverID given the outpoint of a contract.
func newResolverID(op wire.OutPoint) resolverID {
	var r resolverID

	copy(r[:], op.Hash[:])

	endian.PutUint32(r[32:], op.Index)

	return r
}

// logScope is a key that we use to scope the storage of a ChannelArbitrator
// within the global log. We use this key to create a unique bucket within the
// database and ensure that we don't have any key collisions. The log's scope
// is define as: chainHash || chanPoint, where chanPoint is the chan point of
// the original channel.
type logScope [32 + 36]byte

// newLogScope creates a new logScope key from the passed chainhash and
// chanPoint.
func newLogScope(chain chainhash.Hash, op wire.OutPoint) (*logScope, error) {
	var l logScope
	b := bytes.NewBuffer(l[0:0])

	if _, err := b.Write(chain[:]); err != nil {
		return nil, err
	}
	if _, err := b.Write(op.Hash[:]); err != nil {
		return nil, err
	}

	if err := binary.Write(b, endian, op.Index); err != nil {
		return nil, err
	}

	return &l, nil
}

var (
	// stateKey is the key that we use to store the current state of the
	// arbitrator.
	stateKey = []byte("state")

	// contractsBucketKey is the bucket within the logScope that will store
	// all the active unresolved contracts.
	contractsBucketKey = []byte("contractkey")

	// resolutionsKey is the key under the logScope that we'll use to store
	// the full set of resolutions for a channel.
	resolutionsKey = []byte("resolutions")

	// resolutionsSignDetailsKey is the key under the logScope where we
	// will store input.SignDetails for each HTLC resolution. If this is
	// not found under the logScope, it means it was written before
	// SignDetails was introduced, and should be set nil for each HTLC
	// resolution.
	resolutionsSignDetailsKey = []byte("resolutions-sign-details")

	// anchorResolutionKey is the key under the logScope that we'll use to
	// store the anchor resolution, if any.
	anchorResolutionKey = []byte("anchor-resolution")

	// breachResolutionKey is the key under the logScope that we'll use to
	// store the breach resolution, if any. This is used rather than the
	// resolutionsKey.
	breachResolutionKey = []byte("breach-resolution")

	// actionsBucketKey is the key under the logScope that we'll use to
	// store all chain actions once they're determined.
	actionsBucketKey = []byte("chain-actions")

	// commitSetKey is the primary key under the logScope that we'll use to
	// store the confirmed active HTLC sets once we learn that a channel
	// has closed out on chain.
	commitSetKey = []byte("commit-set")

	// taprootDataKey is the key we'll use to store taproot specific data
	// for the set of channels we'll need to sweep/claim.
	taprootDataKey = []byte("taproot-data")
)

var (
	// errScopeBucketNoExist is returned when we can't find the proper
	// bucket for an arbitrator's scope.
	errScopeBucketNoExist = fmt.Errorf("scope bucket not found")

	// errNoContracts is returned when no contracts are found within the
	// log.
	errNoContracts = fmt.Errorf("no stored contracts")

	// errNoResolutions is returned when the log doesn't contain any active
	// chain resolutions.
	errNoResolutions = fmt.Errorf("no contract resolutions exist")

	// errNoActions is returned when the log doesn't contain any stored
	// chain actions.
	errNoActions = fmt.Errorf("no chain actions exist")

	// errNoCommitSet is return when the log doesn't contained a CommitSet.
	// This can happen if the channel hasn't closed yet, or a client is
	// running an older version that didn't yet write this state.
	errNoCommitSet = fmt.Errorf("no commit set exists")
)

// boltArbitratorLog is an implementation of the ArbitratorLog interface backed
// by a bolt DB instance.
type boltArbitratorLog struct {
	db kvdb.Backend

	cfg ChannelArbitratorConfig

	scopeKey logScope
}

// newBoltArbitratorLog returns a new instance of the boltArbitratorLog given
// an arbitrator config, and the items needed to create its log scope.
func newBoltArbitratorLog(db kvdb.Backend, cfg ChannelArbitratorConfig,
	chainHash chainhash.Hash, chanPoint wire.OutPoint) (*boltArbitratorLog, error) {

	scope, err := newLogScope(chainHash, chanPoint)
	if err != nil {
		return nil, err
	}

	return &boltArbitratorLog{
		db:       db,
		cfg:      cfg,
		scopeKey: *scope,
	}, nil
}

// A compile time check to ensure boltArbitratorLog meets the ArbitratorLog
// interface.
var _ ArbitratorLog = (*boltArbitratorLog)(nil)

func fetchContractReadBucket(tx kvdb.RTx, scopeKey []byte) (kvdb.RBucket, error) {
	scopeBucket := tx.ReadBucket(scopeKey)
	if scopeBucket == nil {
		return nil, errScopeBucketNoExist
	}

	contractBucket := scopeBucket.NestedReadBucket(contractsBucketKey)
	if contractBucket == nil {
		return nil, errNoContracts
	}

	return contractBucket, nil
}

func fetchContractWriteBucket(tx kvdb.RwTx, scopeKey []byte) (kvdb.RwBucket, error) {
	scopeBucket, err := tx.CreateTopLevelBucket(scopeKey)
	if err != nil {
		return nil, err
	}

	contractBucket, err := scopeBucket.CreateBucketIfNotExists(
		contractsBucketKey,
	)
	if err != nil {
		return nil, err
	}

	return contractBucket, nil
}

// writeResolver is a helper method that writes a contract resolver and stores
// it it within the passed contractBucket using its unique resolutionsKey key.
func (b *boltArbitratorLog) writeResolver(contractBucket kvdb.RwBucket,
	res ContractResolver) error {

	// Only persist resolvers that are stateful. Stateless resolvers don't
	// expose a resolver key.
	resKey := res.ResolverKey()
	if resKey == nil {
		return nil
	}

	// First, we'll write to the buffer the type of this resolver. Using
	// this byte, we can later properly deserialize the resolver properly.
	var (
		buf   bytes.Buffer
		rType resolverType
	)
	switch res.(type) {
	case *htlcTimeoutResolver:
		rType = resolverTimeout
	case *htlcSuccessResolver:
		rType = resolverSuccess
	case *htlcOutgoingContestResolver:
		rType = resolverOutgoingContest
	case *htlcIncomingContestResolver:
		rType = resolverIncomingContest
	case *commitSweepResolver:
		rType = resolverUnilateralSweep
	case *breachResolver:
		rType = resolverBreach
	}
	if _, err := buf.Write([]byte{byte(rType)}); err != nil {
		return err
	}

	// With the type of the resolver written, we can then write out the raw
	// bytes of the resolver itself.
	if err := res.Encode(&buf); err != nil {
		return err
	}

	return contractBucket.Put(resKey, buf.Bytes())
}

// CurrentState returns the current state of the ChannelArbitrator. It takes an
// optional database transaction, which will be used if it is non-nil, otherwise
// the lookup will be done in its own transaction.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) CurrentState(tx kvdb.RTx) (ArbitratorState, error) {
	var (
		s   ArbitratorState
		err error
	)

	if tx != nil {
		s, err = b.currentState(tx)
	} else {
		err = kvdb.View(b.db, func(tx kvdb.RTx) error {
			s, err = b.currentState(tx)
			return err
		}, func() {
			s = 0
		})
	}

	if err != nil && err != errScopeBucketNoExist {
		return s, err
	}

	return s, nil
}

func (b *boltArbitratorLog) currentState(tx kvdb.RTx) (ArbitratorState, error) {
	scopeBucket := tx.ReadBucket(b.scopeKey[:])
	if scopeBucket == nil {
		return 0, errScopeBucketNoExist
	}

	stateBytes := scopeBucket.Get(stateKey)
	if stateBytes == nil {
		return 0, nil
	}

	return ArbitratorState(stateBytes[0]), nil
}

// CommitState persists, the current state of the chain attendant.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) CommitState(s ArbitratorState) error {
	return kvdb.Batch(b.db, func(tx kvdb.RwTx) error {
		scopeBucket, err := tx.CreateTopLevelBucket(b.scopeKey[:])
		if err != nil {
			return err
		}

		return scopeBucket.Put(stateKey[:], []byte{uint8(s)})
	})
}

// FetchUnresolvedContracts returns all unresolved contracts that have been
// previously written to the log.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) FetchUnresolvedContracts() ([]ContractResolver, error) {
	resolverCfg := ResolverConfig{
		ChannelArbitratorConfig: b.cfg,
		Checkpoint:              b.checkpointContract,
	}
	var contracts []ContractResolver
	err := kvdb.View(b.db, func(tx kvdb.RTx) error {
		contractBucket, err := fetchContractReadBucket(tx, b.scopeKey[:])
		if err != nil {
			return err
		}

		return contractBucket.ForEach(func(resKey, resBytes []byte) error {
			if len(resKey) != resolverIDLen {
				return nil
			}

			var res ContractResolver

			// We'll snip off the first byte of the raw resolver
			// bytes in order to extract what type of resolver
			// we're about to encode.
			resType := resolverType(resBytes[0])

			// Then we'll create a reader using the remaining
			// bytes.
			resReader := bytes.NewReader(resBytes[1:])

			switch resType {
			case resolverTimeout:
				res, err = newTimeoutResolverFromReader(
					resReader, resolverCfg,
				)

			case resolverSuccess:
				res, err = newSuccessResolverFromReader(
					resReader, resolverCfg,
				)

			case resolverOutgoingContest:
				res, err = newOutgoingContestResolverFromReader(
					resReader, resolverCfg,
				)

			case resolverIncomingContest:
				res, err = newIncomingContestResolverFromReader(
					resReader, resolverCfg,
				)

			case resolverUnilateralSweep:
				res, err = newCommitSweepResolverFromReader(
					resReader, resolverCfg,
				)

			case resolverBreach:
				res, err = newBreachResolverFromReader(
					resReader, resolverCfg,
				)

			default:
				return fmt.Errorf("unknown resolver type: %v", resType)
			}

			if err != nil {
				return err
			}

			contracts = append(contracts, res)
			return nil
		})
	}, func() {
		contracts = nil
	})
	if err != nil && err != errScopeBucketNoExist && err != errNoContracts {
		return nil, err
	}

	return contracts, nil
}

// InsertUnresolvedContracts inserts a set of unresolved contracts into the
// log. The log will then persistently store each contract until they've been
// swapped out, or resolved.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) InsertUnresolvedContracts(reports []*channeldb.ResolverReport,
	resolvers ...ContractResolver) error {

	return kvdb.Batch(b.db, func(tx kvdb.RwTx) error {
		contractBucket, err := fetchContractWriteBucket(tx, b.scopeKey[:])
		if err != nil {
			return err
		}

		for _, resolver := range resolvers {
			err = b.writeResolver(contractBucket, resolver)
			if err != nil {
				return err
			}
		}

		// Persist any reports that are present.
		for _, report := range reports {
			err := b.cfg.PutResolverReport(tx, report)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// SwapContract performs an atomic swap of the old contract for the new
// contract. This method is used when after a contract has been fully resolved,
// it produces another contract that needs to be resolved.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) SwapContract(oldContract, newContract ContractResolver) error {
	return kvdb.Batch(b.db, func(tx kvdb.RwTx) error {
		contractBucket, err := fetchContractWriteBucket(tx, b.scopeKey[:])
		if err != nil {
			return err
		}

		oldContractkey := oldContract.ResolverKey()
		if err := contractBucket.Delete(oldContractkey); err != nil {
			return err
		}

		return b.writeResolver(contractBucket, newContract)
	})
}

// ResolveContract marks a contract as fully resolved. Once a contract has been
// fully resolved, it is deleted from persistent storage.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) ResolveContract(res ContractResolver) error {
	return kvdb.Batch(b.db, func(tx kvdb.RwTx) error {
		contractBucket, err := fetchContractWriteBucket(tx, b.scopeKey[:])
		if err != nil {
			return err
		}

		resKey := res.ResolverKey()
		return contractBucket.Delete(resKey)
	})
}

// LogContractResolutions stores a set of chain actions which are derived from
// our set of active contracts, and the on-chain state. We'll write this et of
// cations when: we decide to go on-chain to resolve a contract, or we detect
// that the remote party has gone on-chain.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) LogContractResolutions(c *ContractResolutions) error {
	return kvdb.Batch(b.db, func(tx kvdb.RwTx) error {
		scopeBucket, err := tx.CreateTopLevelBucket(b.scopeKey[:])
		if err != nil {
			return err
		}

		var b bytes.Buffer

		if _, err := b.Write(c.CommitHash[:]); err != nil {
			return err
		}

		// First, we'll write out the commit output's resolution.
		if c.CommitResolution == nil {
			if err := binary.Write(&b, endian, false); err != nil {
				return err
			}
		} else {
			if err := binary.Write(&b, endian, true); err != nil {
				return err
			}
			err = encodeCommitResolution(&b, c.CommitResolution)
			if err != nil {
				return err
			}
		}

		// As we write the HTLC resolutions, we'll serialize the sign
		// details for each, to store under a new key.
		var signDetailsBuf bytes.Buffer

		// With the output for the commitment transaction written, we
		// can now write out the resolutions for the incoming and
		// outgoing HTLC's.
		numIncoming := uint32(len(c.HtlcResolutions.IncomingHTLCs))
		if err := binary.Write(&b, endian, numIncoming); err != nil {
			return err
		}
		for _, htlc := range c.HtlcResolutions.IncomingHTLCs {
			err := encodeIncomingResolution(&b, &htlc)
			if err != nil {
				return err
			}

			err = encodeSignDetails(&signDetailsBuf, htlc.SignDetails)
			if err != nil {
				return err
			}
		}
		numOutgoing := uint32(len(c.HtlcResolutions.OutgoingHTLCs))
		if err := binary.Write(&b, endian, numOutgoing); err != nil {
			return err
		}
		for _, htlc := range c.HtlcResolutions.OutgoingHTLCs {
			err := encodeOutgoingResolution(&b, &htlc)
			if err != nil {
				return err
			}

			err = encodeSignDetails(&signDetailsBuf, htlc.SignDetails)
			if err != nil {
				return err
			}
		}

		// Put the resolutions under the resolutionsKey.
		err = scopeBucket.Put(resolutionsKey, b.Bytes())
		if err != nil {
			return err
		}

		// We'll put the serialized sign details under its own key to
		// stay backwards compatible.
		err = scopeBucket.Put(
			resolutionsSignDetailsKey, signDetailsBuf.Bytes(),
		)
		if err != nil {
			return err
		}

		// Write out the anchor resolution if present.
		if c.AnchorResolution != nil {
			var b bytes.Buffer
			err := encodeAnchorResolution(&b, c.AnchorResolution)
			if err != nil {
				return err
			}

			err = scopeBucket.Put(anchorResolutionKey, b.Bytes())
			if err != nil {
				return err
			}
		}

		// Write out the breach resolution if present.
		if c.BreachResolution != nil {
			var b bytes.Buffer
			err := encodeBreachResolution(&b, c.BreachResolution)
			if err != nil {
				return err
			}

			err = scopeBucket.Put(breachResolutionKey, b.Bytes())
			if err != nil {
				return err
			}
		}

		// If this isn't a taproot channel, then we can exit early here
		// as there's no extra data to write.
		switch {
		case c.AnchorResolution == nil:
			return nil
		case !txscript.IsPayToTaproot(
			c.AnchorResolution.AnchorSignDescriptor.Output.PkScript,
		):
			return nil
		}

		// With everything else encoded, we'll now populate the taproot
		// specific items we need to store for the musig2 channels.
		var tb bytes.Buffer
		err = encodeTaprootAuxData(&tb, c)
		if err != nil {
			return err
		}

		return scopeBucket.Put(taprootDataKey, tb.Bytes())
	})
}

// FetchContractResolutions fetches the set of previously stored contract
// resolutions from persistent storage.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) FetchContractResolutions() (*ContractResolutions, error) {
	var c *ContractResolutions
	err := kvdb.View(b.db, func(tx kvdb.RTx) error {
		scopeBucket := tx.ReadBucket(b.scopeKey[:])
		if scopeBucket == nil {
			return errScopeBucketNoExist
		}

		resolutionBytes := scopeBucket.Get(resolutionsKey)
		if resolutionBytes == nil {
			return errNoResolutions
		}

		resReader := bytes.NewReader(resolutionBytes)

		_, err := io.ReadFull(resReader, c.CommitHash[:])
		if err != nil {
			return err
		}

		// First, we'll attempt to read out the commit resolution (if
		// it exists).
		var haveCommitRes bool
		err = binary.Read(resReader, endian, &haveCommitRes)
		if err != nil {
			return err
		}
		if haveCommitRes {
			c.CommitResolution = &lnwallet.CommitOutputResolution{}
			err = decodeCommitResolution(
				resReader, c.CommitResolution,
			)
			if err != nil {
				return fmt.Errorf("unable to decode "+
					"commit res: %w", err)
			}
		}

		var (
			numIncoming uint32
			numOutgoing uint32
		)

		// Next, we'll read out the incoming and outgoing HTLC
		// resolutions.
		err = binary.Read(resReader, endian, &numIncoming)
		if err != nil {
			return err
		}
		c.HtlcResolutions.IncomingHTLCs = make([]lnwallet.IncomingHtlcResolution, numIncoming)
		for i := uint32(0); i < numIncoming; i++ {
			err := decodeIncomingResolution(
				resReader, &c.HtlcResolutions.IncomingHTLCs[i],
			)
			if err != nil {
				return fmt.Errorf("unable to decode "+
					"incoming res: %w", err)
			}
		}

		err = binary.Read(resReader, endian, &numOutgoing)
		if err != nil {
			return err
		}
		c.HtlcResolutions.OutgoingHTLCs = make([]lnwallet.OutgoingHtlcResolution, numOutgoing)
		for i := uint32(0); i < numOutgoing; i++ {
			err := decodeOutgoingResolution(
				resReader, &c.HtlcResolutions.OutgoingHTLCs[i],
			)
			if err != nil {
				return fmt.Errorf("unable to decode "+
					"outgoing res: %w", err)
			}
		}

		// Now we attempt to get the sign details for our HTLC
		// resolutions. If not present the channel is of a type that
		// doesn't need them. If present there will be SignDetails
		// encoded for each HTLC resolution.
		signDetailsBytes := scopeBucket.Get(resolutionsSignDetailsKey)
		if signDetailsBytes != nil {
			r := bytes.NewReader(signDetailsBytes)

			// They will be encoded in the same order as the
			// resolutions: firs incoming HTLCs, then outgoing.
			for i := uint32(0); i < numIncoming; i++ {
				htlc := &c.HtlcResolutions.IncomingHTLCs[i]
				htlc.SignDetails, err = decodeSignDetails(r)
				if err != nil {
					return fmt.Errorf("unable to decode "+
						"incoming sign desc: %w", err)
				}
			}

			for i := uint32(0); i < numOutgoing; i++ {
				htlc := &c.HtlcResolutions.OutgoingHTLCs[i]
				htlc.SignDetails, err = decodeSignDetails(r)
				if err != nil {
					return fmt.Errorf("unable to decode "+
						"outgoing sign desc: %w", err)
				}
			}
		}

		anchorResBytes := scopeBucket.Get(anchorResolutionKey)
		if anchorResBytes != nil {
			c.AnchorResolution = &lnwallet.AnchorResolution{}
			resReader := bytes.NewReader(anchorResBytes)
			err := decodeAnchorResolution(
				resReader, c.AnchorResolution,
			)
			if err != nil {
				return fmt.Errorf("unable to read anchor "+
					"data: %w", err)
			}
		}

		breachResBytes := scopeBucket.Get(breachResolutionKey)
		if breachResBytes != nil {
			c.BreachResolution = &BreachResolution{}
			resReader := bytes.NewReader(breachResBytes)
			err := decodeBreachResolution(
				resReader, c.BreachResolution,
			)
			if err != nil {
				return fmt.Errorf("unable to read breach "+
					"data: %w", err)
			}
		}

		tapCaseBytes := scopeBucket.Get(taprootDataKey)
		if tapCaseBytes != nil {
			err = decodeTapRootAuxData(
				bytes.NewReader(tapCaseBytes), c,
			)
			if err != nil {
				return fmt.Errorf("unable to read taproot "+
					"data: %w", err)
			}
		}
		return nil
	}, func() {
		c = &ContractResolutions{}
	})
	if err != nil {
		return nil, err
	}

	return c, err
}

// FetchChainActions attempts to fetch the set of previously stored chain
// actions. We'll use this upon restart to properly advance our state machine
// forward.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) FetchChainActions() (ChainActionMap, error) {
	var actionsMap ChainActionMap

	err := kvdb.View(b.db, func(tx kvdb.RTx) error {
		scopeBucket := tx.ReadBucket(b.scopeKey[:])
		if scopeBucket == nil {
			return errScopeBucketNoExist
		}

		actionsBucket := scopeBucket.NestedReadBucket(actionsBucketKey)
		if actionsBucket == nil {
			return errNoActions
		}

		return actionsBucket.ForEach(func(action, htlcBytes []byte) error {
			if htlcBytes == nil {
				return nil
			}

			chainAction := ChainAction(action[0])

			htlcReader := bytes.NewReader(htlcBytes)
			htlcs, err := channeldb.DeserializeHtlcs(htlcReader)
			if err != nil {
				return err
			}

			actionsMap[chainAction] = htlcs

			return nil
		})
	}, func() {
		actionsMap = make(ChainActionMap)
	})
	if err != nil {
		return nil, err
	}

	return actionsMap, nil
}

// InsertConfirmedCommitSet stores the known set of active HTLCs at the time
// channel closure. We'll use this to reconstruct our set of chain actions anew
// based on the confirmed and pending commitment state.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) InsertConfirmedCommitSet(c *CommitSet) error {
	return kvdb.Batch(b.db, func(tx kvdb.RwTx) error {
		scopeBucket, err := tx.CreateTopLevelBucket(b.scopeKey[:])
		if err != nil {
			return err
		}

		var b bytes.Buffer
		if err := encodeCommitSet(&b, c); err != nil {
			return err
		}

		return scopeBucket.Put(commitSetKey, b.Bytes())
	})
}

// FetchConfirmedCommitSet fetches the known confirmed active HTLC set from the
// database. It takes an optional database transaction, which will be used if it
// is non-nil, otherwise the lookup will be done in its own transaction.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) FetchConfirmedCommitSet(tx kvdb.RTx) (*CommitSet, error) {
	if tx != nil {
		return b.fetchConfirmedCommitSet(tx)
	}

	var c *CommitSet
	err := kvdb.View(b.db, func(tx kvdb.RTx) error {
		var err error
		c, err = b.fetchConfirmedCommitSet(tx)
		return err
	}, func() {
		c = nil
	})
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (b *boltArbitratorLog) fetchConfirmedCommitSet(tx kvdb.RTx) (*CommitSet,
	error) {

	scopeBucket := tx.ReadBucket(b.scopeKey[:])
	if scopeBucket == nil {
		return nil, errScopeBucketNoExist
	}

	commitSetBytes := scopeBucket.Get(commitSetKey)
	if commitSetBytes == nil {
		return nil, errNoCommitSet
	}

	return decodeCommitSet(bytes.NewReader(commitSetBytes))
}

// WipeHistory is to be called ONLY once *all* contracts have been fully
// resolved, and the channel closure if finalized. This method will delete all
// on-disk state within the persistent log.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) WipeHistory() error {
	return kvdb.Update(b.db, func(tx kvdb.RwTx) error {
		scopeBucket, err := tx.CreateTopLevelBucket(b.scopeKey[:])
		if err != nil {
			return err
		}

		// Once we have the main top-level bucket, we'll delete the key
		// that stores the state of the arbitrator.
		if err := scopeBucket.Delete(stateKey[:]); err != nil {
			return err
		}

		// Next, we'll delete any lingering contract state within the
		// contracts bucket by removing the bucket itself.
		err = scopeBucket.DeleteNestedBucket(contractsBucketKey)
		if err != nil && err != kvdb.ErrBucketNotFound {
			return err
		}

		// Next, we'll delete storage of any lingering contract
		// resolutions.
		if err := scopeBucket.Delete(resolutionsKey); err != nil {
			return err
		}

		err = scopeBucket.Delete(resolutionsSignDetailsKey)
		if err != nil {
			return err
		}

		// We'll delete any chain actions that are still stored by
		// removing the enclosing bucket.
		err = scopeBucket.DeleteNestedBucket(actionsBucketKey)
		if err != nil && err != kvdb.ErrBucketNotFound {
			return err
		}

		// Finally, we'll delete the enclosing bucket itself.
		return tx.DeleteTopLevelBucket(b.scopeKey[:])
	}, func() {})
}

// checkpointContract is a private method that will be fed into
// ContractResolver instances to checkpoint their state once they reach
// milestones during contract resolution. If the report provided is non-nil,
// it should also be recorded.
func (b *boltArbitratorLog) checkpointContract(c ContractResolver,
	reports ...*channeldb.ResolverReport) error {

	return kvdb.Update(b.db, func(tx kvdb.RwTx) error {
		contractBucket, err := fetchContractWriteBucket(tx, b.scopeKey[:])
		if err != nil {
			return err
		}

		if err := b.writeResolver(contractBucket, c); err != nil {
			return err
		}

		for _, report := range reports {
			if err := b.cfg.PutResolverReport(tx, report); err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// encodeSignDetails encodes the given SignDetails struct to the writer.
// SignDetails is allowed to be nil, in which we will encode that it is not
// present.
func encodeSignDetails(w io.Writer, s *input.SignDetails) error {
	// If we don't have sign details, write false and return.
	if s == nil {
		return binary.Write(w, endian, false)
	}

	// Otherwise write true, and the contents of the SignDetails.
	if err := binary.Write(w, endian, true); err != nil {
		return err
	}

	err := input.WriteSignDescriptor(w, &s.SignDesc)
	if err != nil {
		return err
	}
	err = binary.Write(w, endian, uint32(s.SigHashType))
	if err != nil {
		return err
	}

	// Write the DER-encoded signature.
	b := s.PeerSig.Serialize()
	if err := wire.WriteVarBytes(w, 0, b); err != nil {
		return err
	}

	return nil
}

// decodeSignDetails extracts a single SignDetails from the reader. It is
// allowed to return nil in case the SignDetails were empty.
func decodeSignDetails(r io.Reader) (*input.SignDetails, error) {
	var present bool
	if err := binary.Read(r, endian, &present); err != nil {
		return nil, err
	}

	// Simply return nil if the next SignDetails was not present.
	if !present {
		return nil, nil
	}

	// Otherwise decode the elements of the SignDetails.
	s := input.SignDetails{}
	err := input.ReadSignDescriptor(r, &s.SignDesc)
	if err != nil {
		return nil, err
	}

	var sigHash uint32
	err = binary.Read(r, endian, &sigHash)
	if err != nil {
		return nil, err
	}
	s.SigHashType = txscript.SigHashType(sigHash)

	// Read DER-encoded signature.
	rawSig, err := wire.ReadVarBytes(r, 0, 200, "signature")
	if err != nil {
		return nil, err
	}

	s.PeerSig, err = input.ParseSignature(rawSig)
	if err != nil {
		return nil, err
	}

	return &s, nil
}

func encodeIncomingResolution(w io.Writer, i *lnwallet.IncomingHtlcResolution) error {
	if _, err := w.Write(i.Preimage[:]); err != nil {
		return err
	}

	if i.SignedSuccessTx == nil {
		if err := binary.Write(w, endian, false); err != nil {
			return err
		}
	} else {
		if err := binary.Write(w, endian, true); err != nil {
			return err
		}

		if err := i.SignedSuccessTx.Serialize(w); err != nil {
			return err
		}
	}

	if err := binary.Write(w, endian, i.CsvDelay); err != nil {
		return err
	}
	if _, err := w.Write(i.ClaimOutpoint.Hash[:]); err != nil {
		return err
	}
	if err := binary.Write(w, endian, i.ClaimOutpoint.Index); err != nil {
		return err
	}
	err := input.WriteSignDescriptor(w, &i.SweepSignDesc)
	if err != nil {
		return err
	}

	return nil
}

func decodeIncomingResolution(r io.Reader, h *lnwallet.IncomingHtlcResolution) error {
	if _, err := io.ReadFull(r, h.Preimage[:]); err != nil {
		return err
	}

	var txPresent bool
	if err := binary.Read(r, endian, &txPresent); err != nil {
		return err
	}
	if txPresent {
		h.SignedSuccessTx = &wire.MsgTx{}
		if err := h.SignedSuccessTx.Deserialize(r); err != nil {
			return err
		}
	}

	err := binary.Read(r, endian, &h.CsvDelay)
	if err != nil {
		return err
	}
	_, err = io.ReadFull(r, h.ClaimOutpoint.Hash[:])
	if err != nil {
		return err
	}
	err = binary.Read(r, endian, &h.ClaimOutpoint.Index)
	if err != nil {
		return err
	}

	return input.ReadSignDescriptor(r, &h.SweepSignDesc)
}

func encodeOutgoingResolution(w io.Writer, o *lnwallet.OutgoingHtlcResolution) error {
	if err := binary.Write(w, endian, o.Expiry); err != nil {
		return err
	}

	if o.SignedTimeoutTx == nil {
		if err := binary.Write(w, endian, false); err != nil {
			return err
		}
	} else {
		if err := binary.Write(w, endian, true); err != nil {
			return err
		}

		if err := o.SignedTimeoutTx.Serialize(w); err != nil {
			return err
		}
	}

	if err := binary.Write(w, endian, o.CsvDelay); err != nil {
		return err
	}
	if _, err := w.Write(o.ClaimOutpoint.Hash[:]); err != nil {
		return err
	}
	if err := binary.Write(w, endian, o.ClaimOutpoint.Index); err != nil {
		return err
	}

	return input.WriteSignDescriptor(w, &o.SweepSignDesc)
}

func decodeOutgoingResolution(r io.Reader, o *lnwallet.OutgoingHtlcResolution) error {
	err := binary.Read(r, endian, &o.Expiry)
	if err != nil {
		return err
	}

	var txPresent bool
	if err := binary.Read(r, endian, &txPresent); err != nil {
		return err
	}
	if txPresent {
		o.SignedTimeoutTx = &wire.MsgTx{}
		if err := o.SignedTimeoutTx.Deserialize(r); err != nil {
			return err
		}
	}

	err = binary.Read(r, endian, &o.CsvDelay)
	if err != nil {
		return err
	}
	_, err = io.ReadFull(r, o.ClaimOutpoint.Hash[:])
	if err != nil {
		return err
	}
	err = binary.Read(r, endian, &o.ClaimOutpoint.Index)
	if err != nil {
		return err
	}

	return input.ReadSignDescriptor(r, &o.SweepSignDesc)
}

func encodeCommitResolution(w io.Writer,
	c *lnwallet.CommitOutputResolution) error {

	if _, err := w.Write(c.SelfOutPoint.Hash[:]); err != nil {
		return err
	}
	err := binary.Write(w, endian, c.SelfOutPoint.Index)
	if err != nil {
		return err
	}

	err = input.WriteSignDescriptor(w, &c.SelfOutputSignDesc)
	if err != nil {
		return err
	}

	return binary.Write(w, endian, c.MaturityDelay)
}

func decodeCommitResolution(r io.Reader,
	c *lnwallet.CommitOutputResolution) error {

	_, err := io.ReadFull(r, c.SelfOutPoint.Hash[:])
	if err != nil {
		return err
	}
	err = binary.Read(r, endian, &c.SelfOutPoint.Index)
	if err != nil {
		return err
	}

	err = input.ReadSignDescriptor(r, &c.SelfOutputSignDesc)
	if err != nil {
		return err
	}

	return binary.Read(r, endian, &c.MaturityDelay)
}

func encodeAnchorResolution(w io.Writer,
	a *lnwallet.AnchorResolution) error {

	if _, err := w.Write(a.CommitAnchor.Hash[:]); err != nil {
		return err
	}
	err := binary.Write(w, endian, a.CommitAnchor.Index)
	if err != nil {
		return err
	}

	return input.WriteSignDescriptor(w, &a.AnchorSignDescriptor)
}

func decodeAnchorResolution(r io.Reader,
	a *lnwallet.AnchorResolution) error {

	_, err := io.ReadFull(r, a.CommitAnchor.Hash[:])
	if err != nil {
		return err
	}
	err = binary.Read(r, endian, &a.CommitAnchor.Index)
	if err != nil {
		return err
	}

	return input.ReadSignDescriptor(r, &a.AnchorSignDescriptor)
}

func encodeBreachResolution(w io.Writer, b *BreachResolution) error {
	if _, err := w.Write(b.FundingOutPoint.Hash[:]); err != nil {
		return err
	}
	return binary.Write(w, endian, b.FundingOutPoint.Index)
}

func decodeBreachResolution(r io.Reader, b *BreachResolution) error {
	_, err := io.ReadFull(r, b.FundingOutPoint.Hash[:])
	if err != nil {
		return err
	}
	return binary.Read(r, endian, &b.FundingOutPoint.Index)
}

func encodeHtlcSetKey(w io.Writer, htlcSetKey HtlcSetKey) error {
	err := binary.Write(w, endian, htlcSetKey.IsRemote)
	if err != nil {
		return err
	}

	return binary.Write(w, endian, htlcSetKey.IsPending)
}

func encodeCommitSet(w io.Writer, c *CommitSet) error {
	confCommitKey, err := c.ConfCommitKey.UnwrapOrErr(
		fmt.Errorf("HtlcSetKey is not set"),
	)
	if err != nil {
		return err
	}
	if err := encodeHtlcSetKey(w, confCommitKey); err != nil {
		return err
	}

	numSets := uint8(len(c.HtlcSets))
	if err := binary.Write(w, endian, numSets); err != nil {
		return err
	}

	for htlcSetKey, htlcs := range c.HtlcSets {
		if err := encodeHtlcSetKey(w, htlcSetKey); err != nil {
			return err
		}

		if err := channeldb.SerializeHtlcs(w, htlcs...); err != nil {
			return err
		}
	}

	return nil
}

func decodeHtlcSetKey(r io.Reader, h *HtlcSetKey) error {
	err := binary.Read(r, endian, &h.IsRemote)
	if err != nil {
		return err
	}

	return binary.Read(r, endian, &h.IsPending)
}

func decodeCommitSet(r io.Reader) (*CommitSet, error) {
	confCommitKey := HtlcSetKey{}
	if err := decodeHtlcSetKey(r, &confCommitKey); err != nil {
		return nil, err
	}

	c := &CommitSet{
		ConfCommitKey: fn.Some(confCommitKey),
		HtlcSets:      make(map[HtlcSetKey][]channeldb.HTLC),
	}

	var numSets uint8
	if err := binary.Read(r, endian, &numSets); err != nil {
		return nil, err
	}

	for i := uint8(0); i < numSets; i++ {
		var htlcSetKey HtlcSetKey
		if err := decodeHtlcSetKey(r, &htlcSetKey); err != nil {
			return nil, err
		}

		htlcs, err := channeldb.DeserializeHtlcs(r)
		if err != nil {
			return nil, err
		}

		c.HtlcSets[htlcSetKey] = htlcs
	}

	return c, nil
}

func encodeTaprootAuxData(w io.Writer, c *ContractResolutions) error {
	tapCase := newTaprootBriefcase()

	if c.CommitResolution != nil {
		commitResolution := c.CommitResolution
		commitSignDesc := commitResolution.SelfOutputSignDesc
		//nolint:ll
		tapCase.CtrlBlocks.Val.CommitSweepCtrlBlock = commitSignDesc.ControlBlock

		c.CommitResolution.ResolutionBlob.WhenSome(func(b []byte) {
			tapCase.SettledCommitBlob = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType2](b),
			)
		})
	}

	htlcBlobs := newAuxHtlcBlobs()
	for _, htlc := range c.HtlcResolutions.IncomingHTLCs {
		htlc := htlc

		htlcSignDesc := htlc.SweepSignDesc
		ctrlBlock := htlcSignDesc.ControlBlock

		if ctrlBlock == nil {
			continue
		}

		var resID resolverID
		if htlc.SignedSuccessTx != nil {
			resID = newResolverID(
				htlc.SignedSuccessTx.TxIn[0].PreviousOutPoint,
			)
			//nolint:ll
			tapCase.CtrlBlocks.Val.SecondLevelCtrlBlocks[resID] = ctrlBlock

			// For HTLCs we need to go to the second level for, we
			// also need to store the control block needed to
			// publish the second level transaction.
			if htlc.SignDetails != nil {
				//nolint:ll
				bridgeCtrlBlock := htlc.SignDetails.SignDesc.ControlBlock
				//nolint:ll
				tapCase.CtrlBlocks.Val.IncomingHtlcCtrlBlocks[resID] = bridgeCtrlBlock
			}
		} else {
			resID = newResolverID(htlc.ClaimOutpoint)
			//nolint:ll
			tapCase.CtrlBlocks.Val.IncomingHtlcCtrlBlocks[resID] = ctrlBlock
		}

		htlc.ResolutionBlob.WhenSome(func(b []byte) {
			htlcBlobs[resID] = b
		})
	}
	for _, htlc := range c.HtlcResolutions.OutgoingHTLCs {
		htlc := htlc

		htlcSignDesc := htlc.SweepSignDesc
		ctrlBlock := htlcSignDesc.ControlBlock

		if ctrlBlock == nil {
			continue
		}

		var resID resolverID
		if htlc.SignedTimeoutTx != nil {
			resID = newResolverID(
				htlc.SignedTimeoutTx.TxIn[0].PreviousOutPoint,
			)
			//nolint:ll
			tapCase.CtrlBlocks.Val.SecondLevelCtrlBlocks[resID] = ctrlBlock

			// For HTLCs we need to go to the second level for, we
			// also need to store the control block needed to
			// publish the second level transaction.
			//
			//nolint:ll
			if htlc.SignDetails != nil {
				//nolint:ll
				bridgeCtrlBlock := htlc.SignDetails.SignDesc.ControlBlock
				//nolint:ll
				tapCase.CtrlBlocks.Val.OutgoingHtlcCtrlBlocks[resID] = bridgeCtrlBlock
			}
		} else {
			resID = newResolverID(htlc.ClaimOutpoint)
			//nolint:ll
			tapCase.CtrlBlocks.Val.OutgoingHtlcCtrlBlocks[resID] = ctrlBlock
		}

		htlc.ResolutionBlob.WhenSome(func(b []byte) {
			htlcBlobs[resID] = b
		})
	}

	if c.AnchorResolution != nil {
		anchorSignDesc := c.AnchorResolution.AnchorSignDescriptor
		tapCase.TapTweaks.Val.AnchorTweak = anchorSignDesc.TapTweak
	}

	if len(htlcBlobs) != 0 {
		tapCase.HtlcBlobs = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType4](htlcBlobs),
		)
	}

	return tapCase.Encode(w)
}

func decodeTapRootAuxData(r io.Reader, c *ContractResolutions) error {
	tapCase := newTaprootBriefcase()
	if err := tapCase.Decode(r); err != nil {
		return err
	}

	if c.CommitResolution != nil {
		c.CommitResolution.SelfOutputSignDesc.ControlBlock =
			tapCase.CtrlBlocks.Val.CommitSweepCtrlBlock

		tapCase.SettledCommitBlob.WhenSomeV(func(b []byte) {
			c.CommitResolution.ResolutionBlob = fn.Some(b)
		})
	}

	htlcBlobs := tapCase.HtlcBlobs.ValOpt().UnwrapOr(newAuxHtlcBlobs())

	for i := range c.HtlcResolutions.IncomingHTLCs {
		htlc := c.HtlcResolutions.IncomingHTLCs[i]

		var resID resolverID
		if htlc.SignedSuccessTx != nil {
			resID = newResolverID(
				htlc.SignedSuccessTx.TxIn[0].PreviousOutPoint,
			)

			//nolint:ll
			ctrlBlock := tapCase.CtrlBlocks.Val.SecondLevelCtrlBlocks[resID]
			htlc.SweepSignDesc.ControlBlock = ctrlBlock

			//nolint:ll
			if htlc.SignDetails != nil {
				bridgeCtrlBlock := tapCase.CtrlBlocks.Val.IncomingHtlcCtrlBlocks[resID]
				htlc.SignDetails.SignDesc.ControlBlock = bridgeCtrlBlock
			}
		} else {
			resID = newResolverID(htlc.ClaimOutpoint)

			//nolint:ll
			ctrlBlock := tapCase.CtrlBlocks.Val.IncomingHtlcCtrlBlocks[resID]
			htlc.SweepSignDesc.ControlBlock = ctrlBlock
		}

		if htlcBlob, ok := htlcBlobs[resID]; ok {
			htlc.ResolutionBlob = fn.Some(htlcBlob)
		}

		c.HtlcResolutions.IncomingHTLCs[i] = htlc

	}
	for i := range c.HtlcResolutions.OutgoingHTLCs {
		htlc := c.HtlcResolutions.OutgoingHTLCs[i]

		var resID resolverID
		if htlc.SignedTimeoutTx != nil {
			resID = newResolverID(
				htlc.SignedTimeoutTx.TxIn[0].PreviousOutPoint,
			)

			//nolint:ll
			ctrlBlock := tapCase.CtrlBlocks.Val.SecondLevelCtrlBlocks[resID]
			htlc.SweepSignDesc.ControlBlock = ctrlBlock

			//nolint:ll
			if htlc.SignDetails != nil {
				bridgeCtrlBlock := tapCase.CtrlBlocks.Val.OutgoingHtlcCtrlBlocks[resID]
				htlc.SignDetails.SignDesc.ControlBlock = bridgeCtrlBlock
			}
		} else {
			resID = newResolverID(htlc.ClaimOutpoint)

			//nolint:ll
			ctrlBlock := tapCase.CtrlBlocks.Val.OutgoingHtlcCtrlBlocks[resID]
			htlc.SweepSignDesc.ControlBlock = ctrlBlock
		}

		if htlcBlob, ok := htlcBlobs[resID]; ok {
			htlc.ResolutionBlob = fn.Some(htlcBlob)
		}

		c.HtlcResolutions.OutgoingHTLCs[i] = htlc
	}

	if c.AnchorResolution != nil {
		c.AnchorResolution.AnchorSignDescriptor.TapTweak =
			tapCase.TapTweaks.Val.AnchorTweak
	}

	return nil
}

package contractcourt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
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
}

// IsEmpty returns true if the set of resolutions is "empty". A resolution is
// empty if: our commitment output has been trimmed, and we don't have any
// incoming or outgoing HTLC's active.
func (c *ContractResolutions) IsEmpty() bool {
	return c.CommitResolution == nil &&
		len(c.HtlcResolutions.IncomingHTLCs) == 0 &&
		len(c.HtlcResolutions.OutgoingHTLCs) == 0
}

// ArbitratorLog is the primary source of persistent storage for the
// ChannelArbitrator. The log stores the current state of the
// ChannelArbitrator's internal state machine, any items that are required to
// properly make a state transition, and any unresolved contracts.
type ArbitratorLog interface {
	// TODO(roasbeef): document on interface the errors expected to be
	// returned

	// CurrentState returns the current state of the ChannelArbitrator.
	CurrentState() (ArbitratorState, error)

	// CommitState persists, the current state of the chain attendant.
	CommitState(ArbitratorState) error

	// InsertUnresolvedContracts inserts a set of unresolved contracts into
	// the log. The log will then persistently store each contract until
	// they've been swapped out, or resolved.
	InsertUnresolvedContracts(...ContractResolver) error

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

	// LogChainActions stores a set of chain actions which are derived from
	// our set of active contracts, and the on-chain state. We'll write
	// this et of cations when: we decide to go on-chain to resolve a
	// contract, or we detect that the remote party has gone on-chain.
	LogChainActions(ChainActionMap) error

	// FetchChainActions attempts to fetch the set of previously stored
	// chain actions. We'll use this upon restart to properly advance our
	// state machine forward.
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

// resolverType is an enum that enumerates the various types of resolvers. When
// writing resolvers to disk, we prepend this to the raw bytes stored. This
// allows us to properly decode the resolver into the proper type.
type resolverType uint8

const (
	// resolverTimeout is the type of a resolver that's tasked with
	// resolving an outgoing HTLC that is very close to timing out.
	resolverTimeout = 0

	// resolverSuccess is the type of a resolver that's tasked with
	// resolving an incoming HTLC that we already know the preimage of.
	resolverSuccess = 1

	// resolverOutgoingContest is the type of a resolver that's tasked with
	// resolving an outgoing HTLC that hasn't yet timed out.
	resolverOutgoingContest = 2

	// resolverIncomingContest is the type of a resolver that's tasked with
	// resolving an incoming HTLC that we don't yet know the preimage to.
	resolverIncomingContest = 3

	// resolverUnilateralSweep is the type of resolver that's tasked with
	// sweeping out direct commitment output form the remote party's
	// commitment transaction.
	resolverUnilateralSweep = 4
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

	// actionsBucketKey is the key under the logScope that we'll use to
	// store all chain actions once they're determined.
	actionsBucketKey = []byte("chain-actions")
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

	// errNoActions is retuned when the log doesn't contain any stored
	// chain actions.
	errNoActions = fmt.Errorf("no chain actions exist")
)

// boltArbitratorLog is an implementation of the ArbitratorLog interface backed
// by a bolt DB instance.
type boltArbitratorLog struct {
	db *bolt.DB

	cfg ChannelArbitratorConfig

	scopeKey logScope
}

// newBoltArbitratorLog returns a new instance of the boltArbitratorLog given
// an arbitrator config, and the items needed to create its log scope.
func newBoltArbitratorLog(db *bolt.DB, cfg ChannelArbitratorConfig,
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

func fetchContractReadBucket(tx *bolt.Tx, scopeKey []byte) (*bolt.Bucket, error) {
	scopeBucket := tx.Bucket(scopeKey)
	if scopeBucket == nil {
		return nil, errScopeBucketNoExist
	}

	contractBucket := scopeBucket.Bucket(contractsBucketKey)
	if contractBucket == nil {
		return nil, errNoContracts
	}

	return contractBucket, nil
}

func fetchContractWriteBucket(tx *bolt.Tx, scopeKey []byte) (*bolt.Bucket, error) {
	scopeBucket, err := tx.CreateBucketIfNotExists(scopeKey)
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
func (b *boltArbitratorLog) writeResolver(contractBucket *bolt.Bucket,
	res ContractResolver) error {

	// First, we'll write to the buffer the type of this resolver. Using
	// this byte, we can later properly deserialize the resolver properly.
	var (
		buf   bytes.Buffer
		rType uint8
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
	}
	if _, err := buf.Write([]byte{byte(rType)}); err != nil {
		return err
	}

	// With the type of the resolver written, we can then write out the raw
	// bytes of the resolver itself.
	if err := res.Encode(&buf); err != nil {
		return err
	}

	resKey := res.ResolverKey()

	return contractBucket.Put(resKey, buf.Bytes())
}

// CurrentState returns the current state of the ChannelArbitrator.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) CurrentState() (ArbitratorState, error) {
	var s ArbitratorState
	err := b.db.View(func(tx *bolt.Tx) error {
		scopeBucket := tx.Bucket(b.scopeKey[:])
		if scopeBucket == nil {
			return errScopeBucketNoExist
		}

		stateBytes := scopeBucket.Get(stateKey)
		if stateBytes == nil {
			return nil
		}

		s = ArbitratorState(stateBytes[0])
		return nil
	})
	if err != nil && err != errScopeBucketNoExist {
		return s, err
	}

	return s, nil
}

// CommitState persists, the current state of the chain attendant.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) CommitState(s ArbitratorState) error {
	return b.db.Batch(func(tx *bolt.Tx) error {
		scopeBucket, err := tx.CreateBucketIfNotExists(b.scopeKey[:])
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
	resKit := ResolverKit{
		ChannelArbitratorConfig: b.cfg,
		Checkpoint:              b.checkpointContract,
	}
	var contracts []ContractResolver
	err := b.db.View(func(tx *bolt.Tx) error {
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
			resType := resBytes[0]

			// Then we'll create a reader using the remaining
			// bytes.
			resReader := bytes.NewReader(resBytes[1:])

			switch resType {
			case resolverTimeout:
				timeoutRes := &htlcTimeoutResolver{}
				if err := timeoutRes.Decode(resReader); err != nil {
					return err
				}
				timeoutRes.AttachResolverKit(resKit)

				res = timeoutRes

			case resolverSuccess:
				successRes := &htlcSuccessResolver{}
				if err := successRes.Decode(resReader); err != nil {
					return err
				}

				res = successRes

			case resolverOutgoingContest:
				outContestRes := &htlcOutgoingContestResolver{
					htlcTimeoutResolver: htlcTimeoutResolver{},
				}
				if err := outContestRes.Decode(resReader); err != nil {
					return err
				}

				res = outContestRes

			case resolverIncomingContest:
				inContestRes := &htlcIncomingContestResolver{
					htlcSuccessResolver: htlcSuccessResolver{},
				}
				if err := inContestRes.Decode(resReader); err != nil {
					return err
				}

				res = inContestRes

			case resolverUnilateralSweep:
				sweepRes := &commitSweepResolver{}
				if err := sweepRes.Decode(resReader); err != nil {
					return err
				}

				res = sweepRes

			default:
				return fmt.Errorf("unknown resolver type: %v", resType)
			}

			resKit.Quit = make(chan struct{})
			res.AttachResolverKit(resKit)
			contracts = append(contracts, res)
			return nil
		})
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
func (b *boltArbitratorLog) InsertUnresolvedContracts(resolvers ...ContractResolver) error {
	return b.db.Batch(func(tx *bolt.Tx) error {
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

		return nil
	})
}

// SwapContract performs an atomic swap of the old contract for the new
// contract. This method is used when after a contract has been fully resolved,
// it produces another contract that needs to be resolved.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) SwapContract(oldContract, newContract ContractResolver) error {
	return b.db.Batch(func(tx *bolt.Tx) error {
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
	return b.db.Batch(func(tx *bolt.Tx) error {
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
	return b.db.Batch(func(tx *bolt.Tx) error {
		scopeBucket, err := tx.CreateBucketIfNotExists(b.scopeKey[:])
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
		}

		return scopeBucket.Put(resolutionsKey, b.Bytes())
	})
}

// FetchContractResolutions fetches the set of previously stored contract
// resolutions from persistent storage.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) FetchContractResolutions() (*ContractResolutions, error) {
	c := &ContractResolutions{}
	err := b.db.View(func(tx *bolt.Tx) error {
		scopeBucket := tx.Bucket(b.scopeKey[:])
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
				return err
			}
		}

		var (
			numIncoming uint32
			numOutgoing uint32
		)

		// Next, we'll read out he incoming and outgoing HTLC
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
				return err
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
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return c, err
}

// LogChainActions stores a set of chain actions which are derived from our set
// of active contracts, and the on-chain state. We'll write this et of cations
// when: we decide to go on-chain to resolve a contract, or we detect that the
// remote party has gone on-chain.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) LogChainActions(actions ChainActionMap) error {
	return b.db.Batch(func(tx *bolt.Tx) error {
		scopeBucket, err := tx.CreateBucketIfNotExists(b.scopeKey[:])
		if err != nil {
			return err
		}

		actionsBucket, err := scopeBucket.CreateBucketIfNotExists(
			actionsBucketKey,
		)
		if err != nil {
			return err
		}

		for chainAction, htlcs := range actions {
			var htlcBuf bytes.Buffer
			err := channeldb.SerializeHtlcs(&htlcBuf, htlcs...)
			if err != nil {
				return err
			}

			actionKey := []byte{byte(chainAction)}
			err = actionsBucket.Put(actionKey, htlcBuf.Bytes())
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// FetchChainActions attempts to fetch the set of previously stored chain
// actions. We'll use this upon restart to properly advance our state machine
// forward.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) FetchChainActions() (ChainActionMap, error) {
	actionsMap := make(ChainActionMap)

	err := b.db.View(func(tx *bolt.Tx) error {
		scopeBucket := tx.Bucket(b.scopeKey[:])
		if scopeBucket == nil {
			return errScopeBucketNoExist
		}

		actionsBucket := scopeBucket.Bucket(actionsBucketKey)
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
	})
	if err != nil {
		return nil, err
	}

	return actionsMap, nil
}

// WipeHistory is to be called ONLY once *all* contracts have been fully
// resolved, and the channel closure if finalized. This method will delete all
// on-disk state within the persistent log.
//
// NOTE: Part of the ContractResolver interface.
func (b *boltArbitratorLog) WipeHistory() error {
	return b.db.Update(func(tx *bolt.Tx) error {
		scopeBucket, err := tx.CreateBucketIfNotExists(b.scopeKey[:])
		if err != nil {
			return err
		}

		// Once we have the main top-level bucket, we'll delete the key
		// that stores the state of the arbitrator.
		if err := scopeBucket.Delete(stateKey[:]); err != nil {
			return err
		}

		// Next, we'll delete any lingering contract state within the
		// contracts bucket, and the bucket itself once we're done
		// clearing it out.
		contractBucket, err := scopeBucket.CreateBucketIfNotExists(
			contractsBucketKey,
		)
		if err != nil {
			return err
		}
		if err := contractBucket.ForEach(func(resKey, _ []byte) error {
			return contractBucket.Delete(resKey)
		}); err != nil {
			return err
		}
		if err := scopeBucket.DeleteBucket(contractsBucketKey); err != nil {
			fmt.Println("nah")
			return err
		}

		// Next, we'll delete storage of any lingering contract
		// resolutions.
		if err := scopeBucket.Delete(resolutionsKey); err != nil {
			return err
		}

		// Before we delta the enclosing bucket itself, we'll delta any
		// chain actions that are still stored.
		actionsBucket, err := scopeBucket.CreateBucketIfNotExists(
			actionsBucketKey,
		)
		if err != nil {
			return err
		}
		if err := actionsBucket.ForEach(func(resKey, _ []byte) error {
			return actionsBucket.Delete(resKey)
		}); err != nil {
			return err
		}
		if err := scopeBucket.DeleteBucket(actionsBucketKey); err != nil {
			return err
		}

		// Finally, we'll delete the enclosing bucket itself.
		return tx.DeleteBucket(b.scopeKey[:])
	})
}

// checkpointContract is a private method that will be fed into
// ContractResolver instances to checkpoint their state once they reach
// milestones during contract resolution.
func (b *boltArbitratorLog) checkpointContract(c ContractResolver) error {
	return b.db.Batch(func(tx *bolt.Tx) error {
		contractBucket, err := fetchContractWriteBucket(tx, b.scopeKey[:])
		if err != nil {
			return err
		}

		return b.writeResolver(contractBucket, c)
	})
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
	err := lnwallet.WriteSignDescriptor(w, &i.SweepSignDesc)
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

	return lnwallet.ReadSignDescriptor(r, &h.SweepSignDesc)
}

func encodeOutgoingResolution(w io.Writer, o *lnwallet.OutgoingHtlcResolution) error {
	if err := binary.Write(w, endian, o.Expiry); err != nil {
		return nil
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
		return nil
	}
	if _, err := w.Write(o.ClaimOutpoint.Hash[:]); err != nil {
		return err
	}
	if err := binary.Write(w, endian, o.ClaimOutpoint.Index); err != nil {
		return err
	}

	return lnwallet.WriteSignDescriptor(w, &o.SweepSignDesc)
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

	return lnwallet.ReadSignDescriptor(r, &o.SweepSignDesc)
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

	err = lnwallet.WriteSignDescriptor(w, &c.SelfOutputSignDesc)
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

	err = lnwallet.ReadSignDescriptor(r, &c.SelfOutputSignDesc)
	if err != nil {
		return err
	}

	return binary.Read(r, endian, &c.MaturityDelay)
}

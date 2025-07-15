package channeldb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/walletdb"
	mig "github.com/lightningnetwork/lnd/channeldb/migration"
	"github.com/lightningnetwork/lnd/channeldb/migration12"
	"github.com/lightningnetwork/lnd/channeldb/migration13"
	"github.com/lightningnetwork/lnd/channeldb/migration16"
	"github.com/lightningnetwork/lnd/channeldb/migration20"
	"github.com/lightningnetwork/lnd/channeldb/migration21"
	"github.com/lightningnetwork/lnd/channeldb/migration23"
	"github.com/lightningnetwork/lnd/channeldb/migration24"
	"github.com/lightningnetwork/lnd/channeldb/migration25"
	"github.com/lightningnetwork/lnd/channeldb/migration26"
	"github.com/lightningnetwork/lnd/channeldb/migration27"
	"github.com/lightningnetwork/lnd/channeldb/migration29"
	"github.com/lightningnetwork/lnd/channeldb/migration30"
	"github.com/lightningnetwork/lnd/channeldb/migration31"
	"github.com/lightningnetwork/lnd/channeldb/migration32"
	"github.com/lightningnetwork/lnd/channeldb/migration33"
	"github.com/lightningnetwork/lnd/channeldb/migration34"
	"github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/clock"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

const (
	dbName = "channel.db"
)

var (
	// ErrDryRunMigrationOK signals that a migration executed successful,
	// but we intentionally did not commit the result.
	ErrDryRunMigrationOK = errors.New("dry run migration successful")

	// ErrFinalHtlcsBucketNotFound signals that the top-level final htlcs
	// bucket does not exist.
	ErrFinalHtlcsBucketNotFound = errors.New("final htlcs bucket not " +
		"found")

	// ErrFinalChannelBucketNotFound signals that the channel bucket for
	// final htlc outcomes does not exist.
	ErrFinalChannelBucketNotFound = errors.New("final htlcs channel " +
		"bucket not found")
)

// migration is a function which takes a prior outdated version of the database
// instances and mutates the key/bucket structure to arrive at a more
// up-to-date version of the database.
type migration func(tx kvdb.RwTx) error

// mandatoryVersion defines a db version that must be applied before the lnd
// starts.
type mandatoryVersion struct {
	number    uint32
	migration migration
}

// MigrationConfig is an interface combines the config interfaces of all
// optional migrations.
type MigrationConfig interface {
	migration30.MigrateRevLogConfig
	migration34.MigrationConfig
}

// MigrationConfigImpl is a super set of all the various migration configs and
// an implementation of MigrationConfig.
type MigrationConfigImpl struct {
	migration30.MigrateRevLogConfigImpl
	migration34.MigrationConfigImpl
}

// optionalMigration defines an optional migration function. When a migration
// is optional, it usually involves a large scale of changes that might touch
// millions of keys. Due to OOM concern, the update cannot be safely done
// within one db transaction. Thus, for optional migrations, they must take the
// db backend and construct transactions as needed.
type optionalMigration func(db kvdb.Backend, cfg MigrationConfig) error

// optionalVersion defines a db version that can be optionally applied. When
// applying migrations, we must apply all the mandatory migrations first before
// attempting optional ones.
type optionalVersion struct {
	name      string
	migration optionalMigration
}

var (
	// dbVersions is storing all mandatory versions of database. If current
	// version of database don't match with latest version this list will
	// be used for retrieving all migration function that are need to apply
	// to the current db.
	dbVersions = []mandatoryVersion{
		{
			// The base DB version requires no migration.
			number:    0,
			migration: nil,
		},
		{
			// The version of the database where two new indexes
			// for the update time of node and channel updates were
			// added.
			number:    1,
			migration: migration_01_to_11.MigrateNodeAndEdgeUpdateIndex,
		},
		{
			// The DB version that added the invoice event time
			// series.
			number:    2,
			migration: migration_01_to_11.MigrateInvoiceTimeSeries,
		},
		{
			// The DB version that updated the embedded invoice in
			// outgoing payments to match the new format.
			number:    3,
			migration: migration_01_to_11.MigrateInvoiceTimeSeriesOutgoingPayments,
		},
		{
			// The version of the database where every channel
			// always has two entries in the edges bucket. If
			// a policy is unknown, this will be represented
			// by a special byte sequence.
			number:    4,
			migration: migration_01_to_11.MigrateEdgePolicies,
		},
		{
			// The DB version where we persist each attempt to send
			// an HTLC to a payment hash, and track whether the
			// payment is in-flight, succeeded, or failed.
			number:    5,
			migration: migration_01_to_11.PaymentStatusesMigration,
		},
		{
			// The DB version that properly prunes stale entries
			// from the edge update index.
			number:    6,
			migration: migration_01_to_11.MigratePruneEdgeUpdateIndex,
		},
		{
			// The DB version that migrates the ChannelCloseSummary
			// to a format where optional fields are indicated with
			// boolean flags.
			number:    7,
			migration: migration_01_to_11.MigrateOptionalChannelCloseSummaryFields,
		},
		{
			// The DB version that changes the gossiper's message
			// store keys to account for the message's type and
			// ShortChannelID.
			number:    8,
			migration: migration_01_to_11.MigrateGossipMessageStoreKeys,
		},
		{
			// The DB version where the payments and payment
			// statuses are moved to being stored in a combined
			// bucket.
			number:    9,
			migration: migration_01_to_11.MigrateOutgoingPayments,
		},
		{
			// The DB version where we started to store legacy
			// payload information for all routes, as well as the
			// optional TLV records.
			number:    10,
			migration: migration_01_to_11.MigrateRouteSerialization,
		},
		{
			// Add invoice htlc and cltv delta fields.
			number:    11,
			migration: migration_01_to_11.MigrateInvoices,
		},
		{
			// Migrate to TLV invoice bodies, add payment address
			// and features, remove receipt.
			number:    12,
			migration: migration12.MigrateInvoiceTLV,
		},
		{
			// Migrate to multi-path payments.
			number:    13,
			migration: migration13.MigrateMPP,
		},
		{
			// Initialize payment address index and begin using it
			// as the default index, falling back to payment hash
			// index.
			number:    14,
			migration: mig.CreateTLB(payAddrIndexBucket),
		},
		{
			// This used to be create payment related top-level
			// buckets, however this is now done by the payment
			// package.
			number: 15,
			migration: func(tx kvdb.RwTx) error {
				return nil
			},
		},
		{
			// Add our existing payments to the index bucket created
			// in migration 15.
			number:    16,
			migration: migration16.MigrateSequenceIndex,
		},
		{
			// Create a top level bucket which will store extra
			// information about channel closes.
			number:    17,
			migration: mig.CreateTLB(closeSummaryBucket),
		},
		{
			// Create a top level bucket which holds information
			// about our peers.
			number:    18,
			migration: mig.CreateTLB(peersBucket),
		},
		{
			// Create a top level bucket which holds outpoint
			// information.
			number:    19,
			migration: mig.CreateTLB(outpointBucket),
		},
		{
			// Migrate some data to the outpoint index.
			number:    20,
			migration: migration20.MigrateOutpointIndex,
		},
		{
			// Migrate to length prefixed wire messages everywhere
			// in the database.
			number:    21,
			migration: migration21.MigrateDatabaseWireMessages,
		},
		{
			// Initialize set id index so that invoices can be
			// queried by individual htlc sets.
			number:    22,
			migration: mig.CreateTLB(setIDIndexBucket),
		},
		{
			number:    23,
			migration: migration23.MigrateHtlcAttempts,
		},
		{
			// Remove old forwarding packages of closed channels.
			number:    24,
			migration: migration24.MigrateFwdPkgCleanup,
		},
		{
			// Save the initial local/remote balances in channel
			// info.
			number:    25,
			migration: migration25.MigrateInitialBalances,
		},
		{
			// Migrate the initial local/remote balance fields into
			// tlv records.
			number:    26,
			migration: migration26.MigrateBalancesToTlvRecords,
		},
		{
			// Patch the initial local/remote balance fields with
			// empty values for historical channels.
			number:    27,
			migration: migration27.MigrateHistoricalBalances,
		},
		{
			number:    28,
			migration: mig.CreateTLB(chanIDBucket),
		},
		{
			number:    29,
			migration: migration29.MigrateChanID,
		},
		{
			// Removes the "sweeper-last-tx" bucket. Although we
			// do not have a mandatory version 30 we skip this
			// version because its naming is already used for the
			// first optional migration.
			number:    31,
			migration: migration31.DeleteLastPublishedTxTLB,
		},
		{
			number:    32,
			migration: migration32.MigrateMCRouteSerialisation,
		},
		{
			number:    33,
			migration: migration33.MigrateMCStoreNameSpacedResults,
		},
	}

	// optionalVersions stores all optional migrations that are applied
	// after dbVersions.
	//
	// NOTE: optional migrations must be fault-tolerant and re-run already
	// migrated data must be noop, which means the migration must be able
	// to determine its state.
	optionalVersions = []optionalVersion{
		{
			name: "prune_revocation_log",
			migration: func(db kvdb.Backend,
				cfg MigrationConfig) error {

				return migration30.MigrateRevocationLog(db, cfg)
			},
		},
		{
			name: "gc_decayed_log",
			migration: func(db kvdb.Backend,
				cfg MigrationConfig) error {

				return migration34.MigrateDecayedLog(
					db, cfg,
				)
			},
		},
	}

	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian

	// channelOpeningStateBucket is the database bucket used to store the
	// channelOpeningState for each channel that is currently in the process
	// of being opened.
	channelOpeningStateBucket = []byte("channelOpeningState")
)

// DB is the primary datastore for the lnd daemon. The database stores
// information related to nodes, routing data, open/closed channels, fee
// schedules, and reputation data.
type DB struct {
	kvdb.Backend

	// channelStateDB separates all DB operations on channel state.
	channelStateDB *ChannelStateDB

	dbPath                    string
	clock                     clock.Clock
	dryRun                    bool
	storeFinalHtlcResolutions bool

	// noRevLogAmtData if true, means that commitment transaction amount
	// data should not be stored in the revocation log.
	noRevLogAmtData bool
}

// OpenForTesting opens or creates a channeldb to be used for tests. Any
// necessary schemas migrations due to updates will take place as necessary.
func OpenForTesting(t testing.TB, dbPath string,
	modifiers ...OptionModifier) *DB {

	backend, err := kvdb.GetBoltBackend(&kvdb.BoltBackendConfig{
		DBPath:            dbPath,
		DBFileName:        dbName,
		NoFreelistSync:    true,
		AutoCompact:       false,
		AutoCompactMinAge: kvdb.DefaultBoltAutoCompactMinAge,
		DBTimeout:         kvdb.DefaultDBTimeout,
	})
	require.NoError(t, err)

	db, err := CreateWithBackend(backend, modifiers...)
	require.NoError(t, err)

	db.dbPath = dbPath

	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return db
}

// CreateWithBackend creates channeldb instance using the passed kvdb.Backend.
// Any necessary schemas migrations due to updates will take place as necessary.
func CreateWithBackend(backend kvdb.Backend, modifiers ...OptionModifier) (*DB,
	error) {

	opts := DefaultOptions()
	for _, modifier := range modifiers {
		modifier(&opts)
	}

	if !opts.NoMigration {
		if err := initChannelDB(backend); err != nil {
			return nil, err
		}
	}

	chanDB := &DB{
		Backend: backend,
		channelStateDB: &ChannelStateDB{
			linkNodeDB: &LinkNodeDB{
				backend: backend,
			},
			backend: backend,
		},
		clock:                     opts.clock,
		dryRun:                    opts.dryRun,
		storeFinalHtlcResolutions: opts.storeFinalHtlcResolutions,
		noRevLogAmtData:           opts.NoRevLogAmtData,
	}

	// Set the parent pointer (only used in tests).
	chanDB.channelStateDB.parent = chanDB

	// Synchronize the version of database and apply migrations if needed.
	if !opts.NoMigration {
		if err := chanDB.syncVersions(dbVersions); err != nil {
			backend.Close()
			return nil, err
		}

		// Grab the optional migration config.
		omc := opts.OptionalMiragtionConfig
		if err := chanDB.applyOptionalVersions(omc); err != nil {
			backend.Close()
			return nil, err
		}
	}

	return chanDB, nil
}

// Path returns the file path to the channel database.
func (d *DB) Path() string {
	return d.dbPath
}

var dbTopLevelBuckets = [][]byte{
	openChannelBucket,
	closedChannelBucket,
	forwardingLogBucket,
	fwdPackagesKey,
	invoiceBucket,
	payAddrIndexBucket,
	setIDIndexBucket,
	peersBucket,
	nodeInfoBucket,
	metaBucket,
	closeSummaryBucket,
	outpointBucket,
	chanIDBucket,
	historicalChannelBucket,
}

// Wipe completely deletes all saved state within all used buckets within the
// database. The deletion is done in a single transaction, therefore this
// operation is fully atomic.
func (d *DB) Wipe() error {
	err := kvdb.Update(d, func(tx kvdb.RwTx) error {
		for _, tlb := range dbTopLevelBuckets {
			err := tx.DeleteTopLevelBucket(tlb)
			if err != nil && err != kvdb.ErrBucketNotFound {
				return err
			}
		}
		return nil
	}, func() {})
	if err != nil {
		return err
	}

	return initChannelDB(d.Backend)
}

// initChannelDB creates and initializes a fresh version of channeldb. In the
// case that the target path has not yet been created or doesn't yet exist, then
// the path is created. Additionally, all required top-level buckets used within
// the database are created.
func initChannelDB(db kvdb.Backend) error {
	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		// Check if DB was marked as inactive with a tomb stone.
		if err := EnsureNoTombstone(tx); err != nil {
			return err
		}

		for _, tlb := range dbTopLevelBuckets {
			if _, err := tx.CreateTopLevelBucket(tlb); err != nil {
				return err
			}
		}

		meta := &Meta{}
		// Check if DB is already initialized.
		err := FetchMeta(meta, tx)
		if err == nil {
			return nil
		}

		meta.DbVersionNumber = getLatestDBVersion(dbVersions)
		return putMeta(meta, tx)
	}, func() {})
	if err != nil {
		return fmt.Errorf("unable to create new channeldb: %w", err)
	}

	return nil
}

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// ChannelStateDB is a database that keeps track of all channel state.
type ChannelStateDB struct {
	// linkNodeDB separates all DB operations on LinkNodes.
	linkNodeDB *LinkNodeDB

	// parent holds a pointer to the "main" channeldb.DB object. This is
	// only used for testing and should never be used in production code.
	// For testing use the ChannelStateDB.GetParentDB() function to retrieve
	// this pointer.
	parent *DB

	// backend points to the actual backend holding the channel state
	// database. This may be a real backend or a cache middleware.
	backend kvdb.Backend
}

// GetParentDB returns the "main" channeldb.DB object that is the owner of this
// ChannelStateDB instance. Use this function only in tests where passing around
// pointers makes testing less readable. Never to be used in production code!
func (c *ChannelStateDB) GetParentDB() *DB {
	return c.parent
}

// LinkNodeDB returns the current instance of the link node database.
func (c *ChannelStateDB) LinkNodeDB() *LinkNodeDB {
	return c.linkNodeDB
}

// FetchOpenChannels starts a new database transaction and returns all stored
// currently active/open channels associated with the target nodeID. In the case
// that no active channels are known to have been created with this node, then a
// zero-length slice is returned.
func (c *ChannelStateDB) FetchOpenChannels(nodeID *btcec.PublicKey) (
	[]*OpenChannel, error) {

	var channels []*OpenChannel
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		var err error
		channels, err = c.fetchOpenChannels(tx, nodeID)
		return err
	}, func() {
		channels = nil
	})

	return channels, err
}

// fetchOpenChannels uses and existing database transaction and returns all
// stored currently active/open channels associated with the target nodeID. In
// the case that no active channels are known to have been created with this
// node, then a zero-length slice is returned.
func (c *ChannelStateDB) fetchOpenChannels(tx kvdb.RTx,
	nodeID *btcec.PublicKey) ([]*OpenChannel, error) {

	// Get the bucket dedicated to storing the metadata for open channels.
	openChanBucket := tx.ReadBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, nil
	}

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	pub := nodeID.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadBucket(pub)
	if nodeChanBucket == nil {
		return nil, nil
	}

	// Next, we'll need to go down an additional layer in order to retrieve
	// the channels for each chain the node knows of.
	var channels []*OpenChannel
	err := nodeChanBucket.ForEach(func(chainHash, v []byte) error {
		// If there's a value, it's not a bucket so ignore it.
		if v != nil {
			return nil
		}

		// If we've found a valid chainhash bucket, then we'll retrieve
		// that so we can extract all the channels.
		chainBucket := nodeChanBucket.NestedReadBucket(chainHash)
		if chainBucket == nil {
			return fmt.Errorf("unable to read bucket for chain=%x",
				chainHash[:])
		}

		// Finally, we both of the necessary buckets retrieved, fetch
		// all the active channels related to this node.
		nodeChannels, err := c.fetchNodeChannels(chainBucket)
		if err != nil {
			return fmt.Errorf("unable to read channel for "+
				"chain_hash=%x, node_key=%x: %v",
				chainHash[:], pub, err)
		}

		channels = append(channels, nodeChannels...)
		return nil
	})

	return channels, err
}

// fetchNodeChannels retrieves all active channels from the target chainBucket
// which is under a node's dedicated channel bucket. This function is typically
// used to fetch all the active channels related to a particular node.
func (c *ChannelStateDB) fetchNodeChannels(chainBucket kvdb.RBucket) (
	[]*OpenChannel, error) {

	var channels []*OpenChannel

	// A node may have channels on several chains, so for each known chain,
	// we'll extract all the channels.
	err := chainBucket.ForEach(func(chanPoint, v []byte) error {
		// If there's a value, it's not a bucket so ignore it.
		if v != nil {
			return nil
		}

		// Once we've found a valid channel bucket, we'll extract it
		// from the node's chain bucket.
		chanBucket := chainBucket.NestedReadBucket(chanPoint)

		var outPoint wire.OutPoint
		err := graphdb.ReadOutpoint(
			bytes.NewReader(chanPoint), &outPoint,
		)
		if err != nil {
			return err
		}
		oChannel, err := fetchOpenChannel(chanBucket, &outPoint)
		if err != nil {
			return fmt.Errorf("unable to read channel data for "+
				"chan_point=%v: %w", outPoint, err)
		}
		oChannel.Db = c

		channels = append(channels, oChannel)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return channels, nil
}

// FetchChannel attempts to locate a channel specified by the passed channel
// point. If the channel cannot be found, then an error will be returned.
func (c *ChannelStateDB) FetchChannel(chanPoint wire.OutPoint) (*OpenChannel,
	error) {

	var targetChanPoint bytes.Buffer
	err := graphdb.WriteOutpoint(&targetChanPoint, &chanPoint)
	if err != nil {
		return nil, err
	}

	targetChanPointBytes := targetChanPoint.Bytes()
	selector := func(chainBkt walletdb.ReadBucket) ([]byte, *wire.OutPoint,
		error) {

		return targetChanPointBytes, &chanPoint, nil
	}

	return c.channelScanner(nil, selector)
}

// FetchChannelByID attempts to locate a channel specified by the passed channel
// ID. If the channel cannot be found, then an error will be returned.
// Optionally an existing db tx can be supplied.
func (c *ChannelStateDB) FetchChannelByID(tx kvdb.RTx, id lnwire.ChannelID) (
	*OpenChannel, error) {

	selector := func(chainBkt walletdb.ReadBucket) ([]byte, *wire.OutPoint,
		error) {

		var (
			targetChanPointBytes []byte
			targetChanPoint      *wire.OutPoint

			// errChanFound is used to signal that the channel has
			// been found so that iteration through the DB buckets
			// can stop.
			errChanFound = errors.New("channel found")
		)
		err := chainBkt.ForEach(func(k, _ []byte) error {
			var outPoint wire.OutPoint
			err := graphdb.ReadOutpoint(
				bytes.NewReader(k), &outPoint,
			)
			if err != nil {
				return err
			}

			chanID := lnwire.NewChanIDFromOutPoint(outPoint)
			if chanID != id {
				return nil
			}

			targetChanPoint = &outPoint
			targetChanPointBytes = k

			return errChanFound
		})
		if err != nil && !errors.Is(err, errChanFound) {
			return nil, nil, err
		}
		if targetChanPoint == nil {
			return nil, nil, ErrChannelNotFound
		}

		return targetChanPointBytes, targetChanPoint, nil
	}

	return c.channelScanner(tx, selector)
}

// ChanCount is used by the server in determining access control.
type ChanCount struct {
	HasOpenOrClosedChan bool
	PendingOpenCount    uint64
}

// FetchPermAndTempPeers returns a map where the key is the remote node's
// public key and the value is a struct that has a tally of the pending-open
// channels and whether the peer has an open or closed channel with us.
func (c *ChannelStateDB) FetchPermAndTempPeers(
	chainHash []byte) (map[string]ChanCount, error) {

	peerChanInfo := make(map[string]ChanCount)

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		openChanBucket := tx.ReadBucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoChanDBExists
		}

		openChanErr := openChanBucket.ForEach(func(nodePub,
			v []byte) error {

			// If there is a value, this is not a bucket.
			if v != nil {
				return nil
			}

			nodeChanBucket := openChanBucket.NestedReadBucket(
				nodePub,
			)
			if nodeChanBucket == nil {
				return nil
			}

			chainBucket := nodeChanBucket.NestedReadBucket(
				chainHash,
			)
			if chainBucket == nil {
				return fmt.Errorf("no chain bucket exists")
			}

			var isPermPeer bool
			var pendingOpenCount uint64

			internalErr := chainBucket.ForEach(func(chanPoint,
				val []byte) error {

				// If there is a value, this is not a bucket.
				if val != nil {
					return nil
				}

				chanBucket := chainBucket.NestedReadBucket(
					chanPoint,
				)
				if chanBucket == nil {
					return nil
				}

				var op wire.OutPoint
				readErr := graphdb.ReadOutpoint(
					bytes.NewReader(chanPoint), &op,
				)
				if readErr != nil {
					return readErr
				}

				// We need to go through each channel and look
				// at the IsPending status.
				openChan, err := fetchOpenChannel(
					chanBucket, &op,
				)
				if err != nil {
					return err
				}

				if openChan.IsPending {
					// Add to the pending-open count since
					// this is a temp peer.
					pendingOpenCount++
					return nil
				}

				// Since IsPending is false, this is a perm
				// peer.
				isPermPeer = true

				return nil
			})
			if internalErr != nil {
				return internalErr
			}

			peerCount := ChanCount{
				HasOpenOrClosedChan: isPermPeer,
				PendingOpenCount:    pendingOpenCount,
			}
			peerChanInfo[string(nodePub)] = peerCount

			return nil
		})
		if openChanErr != nil {
			return openChanErr
		}

		// Now check the closed channel bucket.
		historicalChanBucket := tx.ReadBucket(historicalChannelBucket)
		if historicalChanBucket == nil {
			return ErrNoHistoricalBucket
		}

		historicalErr := historicalChanBucket.ForEach(func(chanPoint,
			v []byte) error {
			// Parse each nested bucket and the chanInfoKey to get
			// the IsPending bool. This determines whether the
			// peer is protected or not.
			if v != nil {
				// This is not a bucket. This is currently not
				// possible.
				return nil
			}

			chanBucket := historicalChanBucket.NestedReadBucket(
				chanPoint,
			)
			if chanBucket == nil {
				// This is not possible.
				return fmt.Errorf("no historical channel " +
					"bucket exists")
			}

			var op wire.OutPoint
			readErr := graphdb.ReadOutpoint(
				bytes.NewReader(chanPoint), &op,
			)
			if readErr != nil {
				return readErr
			}

			// This channel is closed, but the structure of the
			// historical bucket is the same. This is by design,
			// which means we can call fetchOpenChannel.
			channel, fetchErr := fetchOpenChannel(chanBucket, &op)
			if fetchErr != nil {
				return fetchErr
			}

			// Only include this peer in the protected class if
			// the closing transaction confirmed. Note that
			// CloseChannel can be called in the funding manager
			// while IsPending is true which is why we need this
			// special-casing to not count premature funding
			// manager calls to CloseChannel.
			if !channel.IsPending {
				// Fetch the public key of the remote node. We
				// need to use the string-ified serialized,
				// compressed bytes as the key.
				remotePub := channel.IdentityPub
				remoteSer := remotePub.SerializeCompressed()
				remoteKey := string(remoteSer)

				count, exists := peerChanInfo[remoteKey]
				if exists {
					count.HasOpenOrClosedChan = true
					peerChanInfo[remoteKey] = count
				} else {
					peerCount := ChanCount{
						HasOpenOrClosedChan: true,
					}
					peerChanInfo[remoteKey] = peerCount
				}
			}

			return nil
		})
		if historicalErr != nil {
			return historicalErr
		}

		return nil
	}, func() {
		clear(peerChanInfo)
	})

	return peerChanInfo, err
}

// channelSelector describes a function that takes a chain-hash bucket from
// within the open-channel DB and returns the wanted channel point bytes, and
// channel point. It must return the ErrChannelNotFound error if the wanted
// channel is not in the given bucket.
type channelSelector func(chainBkt walletdb.ReadBucket) ([]byte, *wire.OutPoint,
	error)

// channelScanner will traverse the DB to each chain-hash bucket of each node
// pub-key bucket in the open-channel-bucket. The chanSelector will then be used
// to fetch the wanted channel outpoint from the chain bucket.
func (c *ChannelStateDB) channelScanner(tx kvdb.RTx,
	chanSelect channelSelector) (*OpenChannel, error) {

	var (
		targetChan *OpenChannel

		// errChanFound is used to signal that the channel has been
		// found so that iteration through the DB buckets can stop.
		errChanFound = errors.New("channel found")
	)

	// chanScan will traverse the following bucket structure:
	//  * nodePub => chainHash => chanPoint
	//
	// At each level we go one further, ensuring that we're traversing the
	// proper key (that's actually a bucket). By only reading the bucket
	// structure and skipping fully decoding each channel, we save a good
	// bit of CPU as we don't need to do things like decompress public
	// keys.
	chanScan := func(tx kvdb.RTx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		openChanBucket := tx.ReadBucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoActiveChannels
		}

		// Within the node channel bucket, are the set of node pubkeys
		// we have channels with, we don't know the entire set, so we'll
		// check them all.
		return openChanBucket.ForEach(func(nodePub, v []byte) error {
			// Ensure that this is a key the same size as a pubkey,
			// and also that it leads directly to a bucket.
			if len(nodePub) != 33 || v != nil {
				return nil
			}

			nodeChanBucket := openChanBucket.NestedReadBucket(
				nodePub,
			)
			if nodeChanBucket == nil {
				return nil
			}

			// The next layer down is all the chains that this node
			// has channels on with us.
			return nodeChanBucket.ForEach(func(chainHash,
				v []byte) error {

				// If there's a value, it's not a bucket so
				// ignore it.
				if v != nil {
					return nil
				}

				chainBucket := nodeChanBucket.NestedReadBucket(
					chainHash,
				)
				if chainBucket == nil {
					return fmt.Errorf("unable to read "+
						"bucket for chain=%x",
						chainHash)
				}

				// Finally, we reach the leaf bucket that stores
				// all the chanPoints for this node.
				targetChanBytes, chanPoint, err := chanSelect(
					chainBucket,
				)
				if errors.Is(err, ErrChannelNotFound) {
					return nil
				} else if err != nil {
					return err
				}

				chanBucket := chainBucket.NestedReadBucket(
					targetChanBytes,
				)
				if chanBucket == nil {
					return nil
				}

				channel, err := fetchOpenChannel(
					chanBucket, chanPoint,
				)
				if err != nil {
					return err
				}

				targetChan = channel
				targetChan.Db = c

				return errChanFound
			})
		})
	}

	var err error
	if tx == nil {
		err = kvdb.View(c.backend, chanScan, func() {})
	} else {
		err = chanScan(tx)
	}
	if err != nil && !errors.Is(err, errChanFound) {
		return nil, err
	}

	if targetChan != nil {
		return targetChan, nil
	}

	// If we can't find the channel, then we return with an error, as we
	// have nothing to back up.
	return nil, ErrChannelNotFound
}

// FetchAllChannels attempts to retrieve all open channels currently stored
// within the database, including pending open, fully open and channels waiting
// for a closing transaction to confirm.
func (c *ChannelStateDB) FetchAllChannels() ([]*OpenChannel, error) {
	return fetchChannels(c)
}

// FetchAllOpenChannels will return all channels that have the funding
// transaction confirmed, and is not waiting for a closing transaction to be
// confirmed.
func (c *ChannelStateDB) FetchAllOpenChannels() ([]*OpenChannel, error) {
	return fetchChannels(
		c,
		pendingChannelFilter(false),
		waitingCloseFilter(false),
	)
}

// FetchPendingChannels will return channels that have completed the process of
// generating and broadcasting funding transactions, but whose funding
// transactions have yet to be confirmed on the blockchain.
func (c *ChannelStateDB) FetchPendingChannels() ([]*OpenChannel, error) {
	return fetchChannels(c,
		pendingChannelFilter(true),
		waitingCloseFilter(false),
	)
}

// FetchWaitingCloseChannels will return all channels that have been opened,
// but are now waiting for a closing transaction to be confirmed.
//
// NOTE: This includes channels that are also pending to be opened.
func (c *ChannelStateDB) FetchWaitingCloseChannels() ([]*OpenChannel, error) {
	return fetchChannels(
		c, waitingCloseFilter(true),
	)
}

// fetchChannelsFilter applies a filter to channels retrieved in fetchchannels.
// A set of filters can be combined to filter across multiple dimensions.
type fetchChannelsFilter func(channel *OpenChannel) bool

// pendingChannelFilter returns a filter based on whether channels are pending
// (ie, their funding transaction still needs to confirm). If pending is false,
// channels with confirmed funding transactions are returned.
func pendingChannelFilter(pending bool) fetchChannelsFilter {
	return func(channel *OpenChannel) bool {
		return channel.IsPending == pending
	}
}

// waitingCloseFilter returns a filter which filters channels based on whether
// they are awaiting the confirmation of their closing transaction. If waiting
// close is true, channels that have had their closing tx broadcast are
// included. If it is false, channels that are not awaiting confirmation of
// their close transaction are returned.
func waitingCloseFilter(waitingClose bool) fetchChannelsFilter {
	return func(channel *OpenChannel) bool {
		// If the channel is in any other state than Default,
		// then it means it is waiting to be closed.
		channelWaitingClose :=
			channel.ChanStatus() != ChanStatusDefault

		// Include the channel if it matches the value for
		// waiting close that we are filtering on.
		return channelWaitingClose == waitingClose
	}
}

// fetchChannels attempts to retrieve channels currently stored in the
// database. It takes a set of filters which are applied to each channel to
// obtain a set of channels with the desired set of properties. Only channels
// which have a true value returned for *all* of the filters will be returned.
// If no filters are provided, every channel in the open channels bucket will
// be returned.
func fetchChannels(c *ChannelStateDB, filters ...fetchChannelsFilter) (
	[]*OpenChannel, error) {

	var channels []*OpenChannel

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		openChanBucket := tx.ReadBucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoActiveChannels
		}

		// Next, fetch the bucket dedicated to storing metadata related
		// to all nodes. All keys within this bucket are the serialized
		// public keys of all our direct counterparties.
		nodeMetaBucket := tx.ReadBucket(nodeInfoBucket)
		if nodeMetaBucket == nil {
			return fmt.Errorf("node bucket not created")
		}

		// Finally for each node public key in the bucket, fetch all
		// the channels related to this particular node.
		return nodeMetaBucket.ForEach(func(k, v []byte) error {
			nodeChanBucket := openChanBucket.NestedReadBucket(k)
			if nodeChanBucket == nil {
				return nil
			}

			return nodeChanBucket.ForEach(func(chainHash, v []byte) error {
				// If there's a value, it's not a bucket so
				// ignore it.
				if v != nil {
					return nil
				}

				// If we've found a valid chainhash bucket,
				// then we'll retrieve that so we can extract
				// all the channels.
				chainBucket := nodeChanBucket.NestedReadBucket(
					chainHash,
				)
				if chainBucket == nil {
					return fmt.Errorf("unable to read "+
						"bucket for chain=%x", chainHash[:])
				}

				nodeChans, err := c.fetchNodeChannels(chainBucket)
				if err != nil {
					return fmt.Errorf("unable to read "+
						"channel for chain_hash=%x, "+
						"node_key=%x: %v", chainHash[:], k, err)
				}
				for _, channel := range nodeChans {
					// includeChannel indicates whether the channel
					// meets the criteria specified by our filters.
					includeChannel := true

					// Run through each filter and check whether the
					// channel should be included.
					for _, f := range filters {
						// If the channel fails the filter, set
						// includeChannel to false and don't bother
						// checking the remaining filters.
						if !f(channel) {
							includeChannel = false
							break
						}
					}

					// If the channel passed every filter, include it in
					// our set of channels.
					if includeChannel {
						channels = append(channels, channel)
					}
				}
				return nil
			})

		})
	}, func() {
		channels = nil
	})
	if err != nil {
		return nil, err
	}

	return channels, nil
}

// FetchClosedChannels attempts to fetch all closed channels from the database.
// The pendingOnly bool toggles if channels that aren't yet fully closed should
// be returned in the response or not. When a channel was cooperatively closed,
// it becomes fully closed after a single confirmation.  When a channel was
// forcibly closed, it will become fully closed after _all_ the pending funds
// (if any) have been swept.
func (c *ChannelStateDB) FetchClosedChannels(pendingOnly bool) (
	[]*ChannelCloseSummary, error) {

	var chanSummaries []*ChannelCloseSummary

	if err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		closeBucket := tx.ReadBucket(closedChannelBucket)
		if closeBucket == nil {
			return ErrNoClosedChannels
		}

		return closeBucket.ForEach(func(chanID []byte, summaryBytes []byte) error {
			summaryReader := bytes.NewReader(summaryBytes)
			chanSummary, err := deserializeCloseChannelSummary(summaryReader)
			if err != nil {
				return err
			}

			// If the query specified to only include pending
			// channels, then we'll skip any channels which aren't
			// currently pending.
			if !chanSummary.IsPending && pendingOnly {
				return nil
			}

			chanSummaries = append(chanSummaries, chanSummary)
			return nil
		})
	}, func() {
		chanSummaries = nil
	}); err != nil {
		return nil, err
	}

	return chanSummaries, nil
}

// ErrClosedChannelNotFound signals that a closed channel could not be found in
// the channeldb.
var ErrClosedChannelNotFound = errors.New("unable to find closed channel summary")

// FetchClosedChannel queries for a channel close summary using the channel
// point of the channel in question.
func (c *ChannelStateDB) FetchClosedChannel(chanID *wire.OutPoint) (
	*ChannelCloseSummary, error) {

	var chanSummary *ChannelCloseSummary
	if err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		closeBucket := tx.ReadBucket(closedChannelBucket)
		if closeBucket == nil {
			return ErrClosedChannelNotFound
		}

		var b bytes.Buffer
		var err error
		if err = graphdb.WriteOutpoint(&b, chanID); err != nil {
			return err
		}

		summaryBytes := closeBucket.Get(b.Bytes())
		if summaryBytes == nil {
			return ErrClosedChannelNotFound
		}

		summaryReader := bytes.NewReader(summaryBytes)
		chanSummary, err = deserializeCloseChannelSummary(summaryReader)

		return err
	}, func() {
		chanSummary = nil
	}); err != nil {
		return nil, err
	}

	return chanSummary, nil
}

// FetchClosedChannelForID queries for a channel close summary using the
// channel ID of the channel in question.
func (c *ChannelStateDB) FetchClosedChannelForID(cid lnwire.ChannelID) (
	*ChannelCloseSummary, error) {

	var chanSummary *ChannelCloseSummary
	if err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		closeBucket := tx.ReadBucket(closedChannelBucket)
		if closeBucket == nil {
			return ErrClosedChannelNotFound
		}

		// The first 30 bytes of the channel ID and outpoint will be
		// equal.
		cursor := closeBucket.ReadCursor()
		op, c := cursor.Seek(cid[:30])

		// We scan over all possible candidates for this channel ID.
		for ; op != nil && bytes.Compare(cid[:30], op[:30]) <= 0; op, c = cursor.Next() {
			var outPoint wire.OutPoint
			err := graphdb.ReadOutpoint(
				bytes.NewReader(op), &outPoint,
			)
			if err != nil {
				return err
			}

			// If the found outpoint does not correspond to this
			// channel ID, we continue.
			if !cid.IsChanPoint(&outPoint) {
				continue
			}

			// Deserialize the close summary and return.
			r := bytes.NewReader(c)
			chanSummary, err = deserializeCloseChannelSummary(r)
			if err != nil {
				return err
			}

			return nil
		}
		return ErrClosedChannelNotFound
	}, func() {
		chanSummary = nil
	}); err != nil {
		return nil, err
	}

	return chanSummary, nil
}

// MarkChanFullyClosed marks a channel as fully closed within the database. A
// channel should be marked as fully closed if the channel was initially
// cooperatively closed and it's reached a single confirmation, or after all
// the pending funds in a channel that has been forcibly closed have been
// swept.
func (c *ChannelStateDB) MarkChanFullyClosed(chanPoint *wire.OutPoint) error {
	var (
		openChannels  []*OpenChannel
		pruneLinkNode *btcec.PublicKey
	)
	err := kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		var b bytes.Buffer
		if err := graphdb.WriteOutpoint(&b, chanPoint); err != nil {
			return err
		}

		chanID := b.Bytes()

		closedChanBucket, err := tx.CreateTopLevelBucket(
			closedChannelBucket,
		)
		if err != nil {
			return err
		}

		chanSummaryBytes := closedChanBucket.Get(chanID)
		if chanSummaryBytes == nil {
			return fmt.Errorf("no closed channel for "+
				"chan_point=%v found", chanPoint)
		}

		chanSummaryReader := bytes.NewReader(chanSummaryBytes)
		chanSummary, err := deserializeCloseChannelSummary(
			chanSummaryReader,
		)
		if err != nil {
			return err
		}

		chanSummary.IsPending = false

		var newSummary bytes.Buffer
		err = serializeChannelCloseSummary(&newSummary, chanSummary)
		if err != nil {
			return err
		}

		err = closedChanBucket.Put(chanID, newSummary.Bytes())
		if err != nil {
			return err
		}

		// Now that the channel is closed, we'll check if we have any
		// other open channels with this peer. If we don't we'll
		// garbage collect it to ensure we don't establish persistent
		// connections to peers without open channels.
		pruneLinkNode = chanSummary.RemotePub
		openChannels, err = c.fetchOpenChannels(
			tx, pruneLinkNode,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch open channels for "+
				"peer %x: %v",
				pruneLinkNode.SerializeCompressed(), err)
		}

		return nil
	}, func() {
		openChannels = nil
		pruneLinkNode = nil
	})
	if err != nil {
		return err
	}

	// Decide whether we want to remove the link node, based upon the number
	// of still open channels.
	return c.pruneLinkNode(openChannels, pruneLinkNode)
}

// pruneLinkNode determines whether we should garbage collect a link node from
// the database due to no longer having any open channels with it. If there are
// any left, then this acts as a no-op.
func (c *ChannelStateDB) pruneLinkNode(openChannels []*OpenChannel,
	remotePub *btcec.PublicKey) error {

	if len(openChannels) > 0 {
		return nil
	}

	log.Infof("Pruning link node %x with zero open channels from database",
		remotePub.SerializeCompressed())

	return c.linkNodeDB.DeleteLinkNode(remotePub)
}

// PruneLinkNodes attempts to prune all link nodes found within the database
// with whom we no longer have any open channels with.
func (c *ChannelStateDB) PruneLinkNodes() error {
	allLinkNodes, err := c.linkNodeDB.FetchAllLinkNodes()
	if err != nil {
		return err
	}

	for _, linkNode := range allLinkNodes {
		var (
			openChannels []*OpenChannel
			linkNode     = linkNode
		)
		err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
			var err error
			openChannels, err = c.fetchOpenChannels(
				tx, linkNode.IdentityPub,
			)
			return err
		}, func() {
			openChannels = nil
		})
		if err != nil {
			return err
		}

		err = c.pruneLinkNode(openChannels, linkNode.IdentityPub)
		if err != nil {
			return err
		}
	}

	return nil
}

// ChannelShell is a shell of a channel that is meant to be used for channel
// recovery purposes. It contains a minimal OpenChannel instance along with
// addresses for that target node.
type ChannelShell struct {
	// NodeAddrs the set of addresses that this node has known to be
	// reachable at in the past.
	NodeAddrs []net.Addr

	// Chan is a shell of an OpenChannel, it contains only the items
	// required to restore the channel on disk.
	Chan *OpenChannel
}

// RestoreChannelShells is a method that allows the caller to reconstruct the
// state of an OpenChannel from the ChannelShell. We'll attempt to write the
// new channel to disk, create a LinkNode instance with the passed node
// addresses, and finally create an edge within the graph for the channel as
// well. This method is idempotent, so repeated calls with the same set of
// channel shells won't modify the database after the initial call.
func (c *ChannelStateDB) RestoreChannelShells(channelShells ...*ChannelShell) error {
	err := kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		for _, channelShell := range channelShells {
			channel := channelShell.Chan

			// When we make a channel, we mark that the channel has
			// been restored, this will signal to other sub-systems
			// to not attempt to use the channel as if it was a
			// regular one.
			channel.chanStatus |= ChanStatusRestored

			// First, we'll attempt to create a new open channel
			// and link node for this channel. If the channel
			// already exists, then in order to ensure this method
			// is idempotent, we'll continue to the next step.
			channel.Db = c
			err := syncNewChannel(
				tx, channel, channelShell.NodeAddrs,
			)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
	if err != nil {
		return err
	}

	return nil
}

// AddrsForNode consults the channel database for all addresses known to the
// passed node public key. The returned boolean indicates if the given node is
// unknown to the channel DB or not.
//
// NOTE: this is part of the AddrSource interface.
func (d *DB) AddrsForNode(_ context.Context, nodePub *btcec.PublicKey) (bool,
	[]net.Addr, error) {

	linkNode, err := d.channelStateDB.linkNodeDB.FetchLinkNode(nodePub)
	// Only if the error is something other than ErrNodeNotFound do we
	// return it.
	switch {
	case err != nil && !errors.Is(err, ErrNodeNotFound):
		return false, nil, err

	case errors.Is(err, ErrNodeNotFound):
		return false, nil, nil
	}

	return true, linkNode.Addresses, nil
}

// AbandonChannel attempts to remove the target channel from the open channel
// database. If the channel was already removed (has a closed channel entry),
// then we'll return a nil error. Otherwise, we'll insert a new close summary
// into the database.
func (c *ChannelStateDB) AbandonChannel(chanPoint *wire.OutPoint,
	bestHeight uint32) error {

	// With the chanPoint constructed, we'll attempt to find the target
	// channel in the database. If we can't find the channel, then we'll
	// return the error back to the caller.
	dbChan, err := c.FetchChannel(*chanPoint)
	switch {
	// If the channel wasn't found, then it's possible that it was already
	// abandoned from the database.
	case err == ErrChannelNotFound:
		_, closedErr := c.FetchClosedChannel(chanPoint)
		if closedErr != nil {
			return closedErr
		}

		// If the channel was already closed, then we don't return an
		// error as we'd like this step to be repeatable.
		return nil
	case err != nil:
		return err
	}

	// Now that we've found the channel, we'll populate a close summary for
	// the channel, so we can store as much information for this abounded
	// channel as possible. We also ensure that we set Pending to false, to
	// indicate that this channel has been "fully" closed.
	summary := &ChannelCloseSummary{
		CloseType:               Abandoned,
		ChanPoint:               *chanPoint,
		ChainHash:               dbChan.ChainHash,
		CloseHeight:             bestHeight,
		RemotePub:               dbChan.IdentityPub,
		Capacity:                dbChan.Capacity,
		SettledBalance:          dbChan.LocalCommitment.LocalBalance.ToSatoshis(),
		ShortChanID:             dbChan.ShortChanID(),
		RemoteCurrentRevocation: dbChan.RemoteCurrentRevocation,
		RemoteNextRevocation:    dbChan.RemoteNextRevocation,
		LocalChanConfig:         dbChan.LocalChanCfg,
	}

	// Finally, we'll close the channel in the DB, and return back to the
	// caller. We set ourselves as the close initiator because we abandoned
	// the channel.
	return dbChan.CloseChannel(summary, ChanStatusLocalCloseInitiator)
}

// SaveChannelOpeningState saves the serialized channel state for the provided
// chanPoint to the channelOpeningStateBucket.
func (c *ChannelStateDB) SaveChannelOpeningState(outPoint,
	serializedState []byte) error {

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket, err := tx.CreateTopLevelBucket(channelOpeningStateBucket)
		if err != nil {
			return err
		}

		return bucket.Put(outPoint, serializedState)
	}, func() {})
}

// GetChannelOpeningState fetches the serialized channel state for the provided
// outPoint from the database, or returns ErrChannelNotFound if the channel
// is not found.
func (c *ChannelStateDB) GetChannelOpeningState(outPoint []byte) ([]byte,
	error) {

	var serializedState []byte
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(channelOpeningStateBucket)
		if bucket == nil {
			// If the bucket does not exist, it means we never added
			//  a channel to the db, so return ErrChannelNotFound.
			return ErrChannelNotFound
		}

		stateBytes := bucket.Get(outPoint)
		if stateBytes == nil {
			return ErrChannelNotFound
		}

		serializedState = append(serializedState, stateBytes...)

		return nil
	}, func() {
		serializedState = nil
	})
	return serializedState, err
}

// DeleteChannelOpeningState removes any state for outPoint from the database.
func (c *ChannelStateDB) DeleteChannelOpeningState(outPoint []byte) error {
	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(channelOpeningStateBucket)
		if bucket == nil {
			return ErrChannelNotFound
		}

		return bucket.Delete(outPoint)
	}, func() {})
}

// syncVersions function is used for safe db version synchronization. It
// applies migration functions to the current database and recovers the
// previous state of db if at least one error/panic appeared during migration.
func (d *DB) syncVersions(versions []mandatoryVersion) error {
	meta, err := d.FetchMeta()
	if err != nil {
		if err == ErrMetaNotFound {
			meta = &Meta{}
		} else {
			return err
		}
	}

	latestVersion := getLatestDBVersion(versions)
	log.Infof("Checking for schema update: latest_version=%v, "+
		"db_version=%v", latestVersion, meta.DbVersionNumber)

	switch {

	// If the database reports a higher version that we are aware of, the
	// user is probably trying to revert to a prior version of lnd. We fail
	// here to prevent reversions and unintended corruption.
	case meta.DbVersionNumber > latestVersion:
		log.Errorf("Refusing to revert from db_version=%d to "+
			"lower version=%d", meta.DbVersionNumber,
			latestVersion)
		return ErrDBReversion

	// If the current database version matches the latest version number,
	// then we don't need to perform any migrations.
	case meta.DbVersionNumber == latestVersion:
		return nil
	}

	log.Infof("Performing database schema migration")

	// Otherwise, we fetch the migrations which need to applied, and
	// execute them serially within a single database transaction to ensure
	// the migration is atomic.
	migrations, migrationVersions := getMigrationsToApply(
		versions, meta.DbVersionNumber,
	)
	return kvdb.Update(d, func(tx kvdb.RwTx) error {
		for i, migration := range migrations {
			if migration == nil {
				continue
			}

			log.Infof("Applying migration #%v",
				migrationVersions[i])

			if err := migration(tx); err != nil {
				log.Infof("Unable to apply migration #%v",
					migrationVersions[i])
				return err
			}
		}

		meta.DbVersionNumber = latestVersion
		err := putMeta(meta, tx)
		if err != nil {
			return err
		}

		// In dry-run mode, return an error to prevent the transaction
		// from committing.
		if d.dryRun {
			return ErrDryRunMigrationOK
		}

		return nil
	}, func() {})
}

// applyOptionalVersions applies the optional migrations to the database if
// specified in the config.
func (d *DB) applyOptionalVersions(cfg OptionalMiragtionConfig) error {
	// TODO(yy): need to design the db to support dry run for optional
	// migrations.
	if d.dryRun {
		log.Info("Skipped optional migrations as dry run mode is not " +
			"supported yet")
		return nil
	}

	om, err := d.fetchOptionalMeta()
	if err != nil {
		if err == ErrMetaNotFound {
			om = &OptionalMeta{
				Versions: make(map[uint64]string),
			}
		} else {
			return fmt.Errorf("unable to fetch optional "+
				"meta: %w", err)
		}
	}

	// migrationCfg is the parent configuration which implements the config
	// interfaces of all the single optional migrations.
	migrationCfg := &MigrationConfigImpl{
		migration30.MigrateRevLogConfigImpl{
			NoAmountData: d.noRevLogAmtData,
		},
		migration34.MigrationConfigImpl{
			DecayedLog: cfg.DecayedLog,
		},
	}

	log.Infof("Applying %d optional migrations", len(optionalVersions))

	// Apply the optional migrations if requested.
	for number, version := range optionalVersions {
		log.Infof("Checking for optional update: name=%v", version.name)

		// Exit early if the optional migration is not specified.
		if !cfg.MigrationFlags[number] {
			log.Debugf("Skipping optional migration: name=%s as "+
				"it is not specified in the config",
				version.name)

			continue
		}

		// Exit early if the optional migration has already been
		// applied.
		if _, ok := om.Versions[uint64(number)]; ok {
			log.Debugf("Skipping optional migration: name=%s as "+
				"it has already been applied", version.name)

			continue
		}

		log.Infof("Performing database optional migration: %s",
			version.name)

		// Call the migration function for the specific optional
		// migration.
		if err := version.migration(d, migrationCfg); err != nil {
			log.Errorf("Unable to apply optional migration: %s, "+
				"error: %v", version.name, err)
			return err
		}

		// Update the optional meta. Notice that unlike the mandatory db
		// migrations where we perform the migration and updating meta
		// in a single db transaction, we use different transactions
		// here. Even when the following update is failed, we should be
		// fine here as we would re-run the optional migration again,
		// which is a noop, during next startup.
		om.Versions[uint64(number)] = version.name
		if err := d.putOptionalMeta(om); err != nil {
			log.Errorf("Unable to update optional meta: %v", err)
			return err
		}

		log.Infof("Successfully applied optional migration: %s",
			version.name)
	}

	return nil
}

// ChannelStateDB returns the sub database that is concerned with the channel
// state.
func (d *DB) ChannelStateDB() *ChannelStateDB {
	return d.channelStateDB
}

// LatestDBVersion returns the number of the latest database version currently
// known to the channel DB.
func LatestDBVersion() uint32 {
	return getLatestDBVersion(dbVersions)
}

func getLatestDBVersion(versions []mandatoryVersion) uint32 {
	return versions[len(versions)-1].number
}

// getMigrationsToApply retrieves the migration function that should be
// applied to the database.
func getMigrationsToApply(versions []mandatoryVersion,
	version uint32) ([]migration, []uint32) {

	migrations := make([]migration, 0, len(versions))
	migrationVersions := make([]uint32, 0, len(versions))

	for _, v := range versions {
		if v.number > version {
			migrations = append(migrations, v.migration)
			migrationVersions = append(migrationVersions, v.number)
		}
	}

	return migrations, migrationVersions
}

// fetchHistoricalChanBucket returns a the channel bucket for a given outpoint
// from the historical channel bucket. If the bucket does not exist,
// ErrNoHistoricalBucket is returned.
func fetchHistoricalChanBucket(tx kvdb.RTx,
	outPoint *wire.OutPoint) (kvdb.RBucket, error) {

	// First fetch the top level bucket which stores all data related to
	// historically stored channels.
	historicalChanBucket := tx.ReadBucket(historicalChannelBucket)
	if historicalChanBucket == nil {
		return nil, ErrNoHistoricalBucket
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for the channel itself.
	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}
	chanBucket := historicalChanBucket.NestedReadBucket(
		chanPointBuf.Bytes(),
	)
	if chanBucket == nil {
		return nil, ErrChannelNotFound
	}

	return chanBucket, nil
}

// FetchHistoricalChannel fetches open channel data from the historical channel
// bucket.
func (c *ChannelStateDB) FetchHistoricalChannel(outPoint *wire.OutPoint) (
	*OpenChannel, error) {

	var channel *OpenChannel
	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		chanBucket, err := fetchHistoricalChanBucket(tx, outPoint)
		if err != nil {
			return err
		}

		channel, err = fetchOpenChannel(chanBucket, outPoint)
		if err != nil {
			return err
		}

		channel.Db = c
		return nil
	}, func() {
		channel = nil
	})
	if err != nil {
		return nil, err
	}

	return channel, nil
}

func fetchFinalHtlcsBucket(tx kvdb.RTx,
	chanID lnwire.ShortChannelID) (kvdb.RBucket, error) {

	finalHtlcsBucket := tx.ReadBucket(finalHtlcsBucket)
	if finalHtlcsBucket == nil {
		return nil, ErrFinalHtlcsBucketNotFound
	}

	var chanIDBytes [8]byte
	byteOrder.PutUint64(chanIDBytes[:], chanID.ToUint64())

	chanBucket := finalHtlcsBucket.NestedReadBucket(chanIDBytes[:])
	if chanBucket == nil {
		return nil, ErrFinalChannelBucketNotFound
	}

	return chanBucket, nil
}

var ErrHtlcUnknown = errors.New("htlc unknown")

// LookupFinalHtlc retrieves a final htlc resolution from the database. If the
// htlc has no final resolution yet, ErrHtlcUnknown is returned.
func (c *ChannelStateDB) LookupFinalHtlc(chanID lnwire.ShortChannelID,
	htlcIndex uint64) (*FinalHtlcInfo, error) {

	var idBytes [8]byte
	byteOrder.PutUint64(idBytes[:], htlcIndex)

	var settledByte byte

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		finalHtlcsBucket, err := fetchFinalHtlcsBucket(
			tx, chanID,
		)
		switch {
		case errors.Is(err, ErrFinalHtlcsBucketNotFound):
			fallthrough

		case errors.Is(err, ErrFinalChannelBucketNotFound):
			return ErrHtlcUnknown

		case err != nil:
			return fmt.Errorf("cannot fetch final htlcs bucket: %w",
				err)
		}

		value := finalHtlcsBucket.Get(idBytes[:])
		if value == nil {
			return ErrHtlcUnknown
		}

		if len(value) != 1 {
			return errors.New("unexpected final htlc value length")
		}

		settledByte = value[0]

		return nil
	}, func() {
		settledByte = 0
	})
	if err != nil {
		return nil, err
	}

	info := FinalHtlcInfo{
		Settled:  settledByte&byte(FinalHtlcSettledBit) != 0,
		Offchain: settledByte&byte(FinalHtlcOffchainBit) != 0,
	}

	return &info, nil
}

// PutOnchainFinalHtlcOutcome stores the final on-chain outcome of an htlc in
// the database.
func (c *ChannelStateDB) PutOnchainFinalHtlcOutcome(
	chanID lnwire.ShortChannelID, htlcID uint64, settled bool) error {

	// Skip if the user did not opt in to storing final resolutions.
	if !c.parent.storeFinalHtlcResolutions {
		return nil
	}

	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		finalHtlcsBucket, err := fetchFinalHtlcsBucketRw(tx, chanID)
		if err != nil {
			return err
		}

		return putFinalHtlc(
			finalHtlcsBucket, htlcID,
			FinalHtlcInfo{
				Settled:  settled,
				Offchain: false,
			},
		)
	}, func() {})
}

// MakeTestInvoiceDB is used to create a test invoice database for testing
// purposes. It simply calls into MakeTestDB so the same modifiers can be used.
func MakeTestInvoiceDB(t *testing.T, modifiers ...OptionModifier) (
	invoices.InvoiceDB, error) {

	return MakeTestDB(t, modifiers...)
}

// MakeTestDB creates a new instance of the ChannelDB for testing purposes.
// A callback which cleans up the created temporary directories is also
// returned and intended to be executed after the test completes.
func MakeTestDB(t *testing.T, modifiers ...OptionModifier) (*DB, error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName := t.TempDir()

	// Next, create channeldb for the first time.
	backend, backendCleanup, err := kvdb.GetTestBackend(tempDirName, "cdb")
	if err != nil {
		backendCleanup()
		return nil, err
	}

	cdb, err := CreateWithBackend(backend, modifiers...)
	if err != nil {
		backendCleanup()
		return nil, err
	}

	t.Cleanup(func() {
		cdb.Close()
		backendCleanup()
	})

	return cdb, nil
}

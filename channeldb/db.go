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
	"github.com/lightningnetwork/lnd/channeldb/migration35"
	"github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/chanstate"
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
	ErrFinalHtlcsBucketNotFound = chanstate.ErrFinalHtlcsBucketNotFound

	// ErrFinalChannelBucketNotFound signals that the channel bucket for
	// final htlc outcomes does not exist.
	ErrFinalChannelBucketNotFound = chanstate.ErrFinalChannelBucketNotFound
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
		{
			// We skip mandatory version 34 because that
			// number is already associated with an
			// optional migration package.
			number:    35,
			migration: migration35.MigrateWaitingProofStore,
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
			kvStore: chanstate.NewKVStore(
				backend,
				opts.storeFinalHtlcResolutions,
				opts.NoRevLogAmtData,
				opts.tombstoneClosedChannels,
			),
			backend:                 backend,
			tombstoneClosedChannels: opts.tombstoneClosedChannels,
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

	// kvStore is the chanstate-owned KV implementation. ChannelStateDB
	// keeps compatibility wrappers while callers still import channeldb.
	kvStore *chanstate.KVStore

	// tombstoneClosedChannels is set by OptionTombstoneClosedChannels.
	// When true, CloseChannel skips deleting nested per-channel state and
	// relies on the outpointBucket flip to outpointClosed as the
	// authoritative closed-channel signal.
	tombstoneClosedChannels bool
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

	channels, err := c.kvStore.FetchOpenChannels(nodeID)
	if err != nil {
		return nil, err
	}

	return c.attachOpenChannelStores(channels), nil
}

// fetchOpenChannels uses and existing database transaction and returns all
// stored currently active/open channels associated with the target nodeID. In
// the case that no active channels are known to have been created with this
// node, then a zero-length slice is returned.
func (c *ChannelStateDB) fetchOpenChannels(tx kvdb.RTx,
	nodeID *btcec.PublicKey) ([]*OpenChannel, error) {

	channels, err := chanstate.FetchOpenChannelsTx(tx, nodeID)
	if err != nil {
		return nil, err
	}

	return c.attachOpenChannelStores(channels), nil
}

func (c *ChannelStateDB) attachOpenChannelStore(
	channel *OpenChannel) *OpenChannel {

	if channel != nil {
		channel.Db = c
	}

	return channel
}

func (c *ChannelStateDB) attachOpenChannelStores(
	channels []*OpenChannel) []*OpenChannel {

	for _, channel := range channels {
		c.attachOpenChannelStore(channel)
	}

	return channels
}

// FetchChannel attempts to locate a channel specified by the passed channel
// point. If the channel cannot be found, then an error will be returned.
func (c *ChannelStateDB) FetchChannel(chanPoint wire.OutPoint) (*OpenChannel,
	error) {

	channel, err := c.kvStore.FetchChannel(chanPoint)
	if err != nil {
		return nil, err
	}

	return c.attachOpenChannelStore(channel), nil
}

// FetchChannelByID attempts to locate a channel specified by the passed channel
// ID. If the channel cannot be found, then an error will be returned.
func (c *ChannelStateDB) FetchChannelByID(id lnwire.ChannelID) (*OpenChannel,
	error) {

	channel, err := c.kvStore.FetchChannelByID(id)
	if err != nil {
		return nil, err
	}

	return c.attachOpenChannelStore(channel), nil
}

// ChanCount is used by the server in determining access control.
type ChanCount = chanstate.ChanCount

// FetchPermAndTempPeers returns a map where the key is the remote node's
// public key and the value is a struct that has a tally of the pending-open
// channels and whether the peer has an open or closed channel with us.
func (c *ChannelStateDB) FetchPermAndTempPeers(
	chainHash []byte) (map[string]ChanCount, error) {

	return c.kvStore.FetchPermAndTempPeers(chainHash)
}

// FetchAllChannels attempts to retrieve all open channels currently stored
// within the database, including pending open, fully open and channels waiting
// for a closing transaction to confirm.
func (c *ChannelStateDB) FetchAllChannels() ([]*OpenChannel, error) {
	channels, err := c.kvStore.FetchAllChannels()
	if err != nil {
		return nil, err
	}

	return c.attachOpenChannelStores(channels), nil
}

// FetchAllOpenChannels will return all channels that have the funding
// transaction confirmed, and is not waiting for a closing transaction to be
// confirmed.
func (c *ChannelStateDB) FetchAllOpenChannels() ([]*OpenChannel, error) {
	channels, err := c.kvStore.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	return c.attachOpenChannelStores(channels), nil
}

// FetchPendingChannels will return channels that have completed the process of
// generating and broadcasting funding transactions, but whose funding
// transactions have yet to be confirmed on the blockchain.
func (c *ChannelStateDB) FetchPendingChannels() ([]*OpenChannel, error) {
	channels, err := c.kvStore.FetchPendingChannels()
	if err != nil {
		return nil, err
	}

	return c.attachOpenChannelStores(channels), nil
}

// FetchWaitingCloseChannels will return all channels that have been opened,
// but are now waiting for a closing transaction to be confirmed.
//
// NOTE: This includes channels that are also pending to be opened.
func (c *ChannelStateDB) FetchWaitingCloseChannels() ([]*OpenChannel, error) {
	channels, err := c.kvStore.FetchWaitingCloseChannels()
	if err != nil {
		return nil, err
	}

	return c.attachOpenChannelStores(channels), nil
}

// FetchClosedChannels attempts to fetch all closed channels from the database.
// The pendingOnly bool toggles if channels that aren't yet fully closed should
// be returned in the response or not. When a channel was cooperatively closed,
// it becomes fully closed after a single confirmation.  When a channel was
// forcibly closed, it will become fully closed after _all_ the pending funds
// (if any) have been swept.
func (c *ChannelStateDB) FetchClosedChannels(pendingOnly bool) (
	[]*ChannelCloseSummary, error) {

	return c.kvStore.FetchClosedChannels(pendingOnly)
}

// FetchClosedChannel queries for a channel close summary using the channel
// point of the channel in question.
func (c *ChannelStateDB) FetchClosedChannel(chanID *wire.OutPoint) (
	*ChannelCloseSummary, error) {

	return c.kvStore.FetchClosedChannel(chanID)
}

// FetchClosedChannelForID queries for a channel close summary using the
// channel ID of the channel in question.
func (c *ChannelStateDB) FetchClosedChannelForID(cid lnwire.ChannelID) (
	*ChannelCloseSummary, error) {

	return c.kvStore.FetchClosedChannelForID(cid)
}

// MarkChanFullyClosed marks a channel as fully closed within the database. A
// channel should be marked as fully closed if the channel was initially
// cooperatively closed and it's reached a single confirmation, or after all
// the pending funds in a channel that has been forcibly closed have been
// swept.
func (c *ChannelStateDB) MarkChanFullyClosed(chanPoint *wire.OutPoint) error {
	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
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
		remotePub := chanSummary.RemotePub
		openChannels, err := c.fetchOpenChannels(tx, remotePub)
		if err != nil {
			return fmt.Errorf("unable to fetch open channels for "+
				"peer %x: %v",
				remotePub.SerializeCompressed(), err)
		}

		if len(openChannels) > 0 {
			return nil
		}

		// If there are no open channels with this peer, prune the
		// link node. We do this within the same transaction to avoid
		// a race condition where a new channel could be opened
		// between this check and the deletion.
		log.Infof("Pruning link node %x with zero open "+
			"channels from database",
			remotePub.SerializeCompressed())

		err = deleteLinkNode(tx, remotePub)
		if err != nil {
			return fmt.Errorf("unable to delete link "+
				"node: %w", err)
		}

		return nil
	}, func() {})
}

// pruneLinkNode determines whether we should garbage collect a link node from
// the database due to no longer having any open channels with it.
//
// NOTE: This function should be called after an initial check shows no open
// channels exist. It will double-check within a write transaction to avoid a
// race condition where a channel could be opened between the initial check
// and the deletion.
func (c *ChannelStateDB) pruneLinkNode(remotePub *btcec.PublicKey) error {
	return kvdb.Update(c.backend, func(tx kvdb.RwTx) error {
		// Double-check for open channels to avoid deleting a link node
		// if a channel was opened since the caller's initial check.
		//
		// NOTE: This avoids a race condition where a channel could be
		// opened between the initial check and the deletion.
		openChannels, err := c.fetchOpenChannels(tx, remotePub)
		if err != nil {
			return err
		}

		// If channels exist now, don't prune.
		if len(openChannels) > 0 {
			return nil
		}

		// No open channels, safe to prune the link node.
		log.Infof("Pruning link node %x with zero open channels "+
			"from database",
			remotePub.SerializeCompressed())

		err = deleteLinkNode(tx, remotePub)
		if err != nil {
			return fmt.Errorf("unable to prune link node: %w", err)
		}

		return nil
	}, func() {})
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

		if len(openChannels) > 0 {
			continue
		}

		err = c.pruneLinkNode(linkNode.IdentityPub)
		if err != nil {
			return err
		}
	}

	return nil
}

// RepairLinkNodes scans all channels in the database and ensures that a
// link node exists for each remote peer. This should be called on startup to
// ensure that our database is consistent.
//
// NOTE: This function is designed to repair database inconsistencies that may
// have occurred due to the race condition in link node pruning (where link
// nodes could be incorrectly deleted while channels still existed). This can
// be removed once we move to native sql.
func (c *ChannelStateDB) RepairLinkNodes(network wire.BitcoinNet) error {
	// In a single read transaction, build a list of all peers with open
	// channels and check which ones are missing link nodes.
	var missingPeers []*btcec.PublicKey

	err := kvdb.View(c.backend, func(tx kvdb.RTx) error {
		openChanBucket := tx.ReadBucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoActiveChannels
		}

		var peersWithChannels []*btcec.PublicKey

		err := openChanBucket.ForEach(func(nodePubBytes,
			_ []byte) error {

			nodePub, err := btcec.ParsePubKey(nodePubBytes)
			if err != nil {
				return err
			}

			channels, err := c.fetchOpenChannels(tx, nodePub)
			if err != nil {
				return err
			}

			if len(channels) > 0 {
				peersWithChannels = append(
					peersWithChannels, nodePub,
				)
			}

			return nil
		})
		if err != nil {
			return err
		}

		// Now check which peers are missing link nodes within the
		// same transaction.
		missingPeers, err = c.linkNodeDB.FindMissingLinkNodes(
			tx, peersWithChannels,
		)

		return err
	}, func() {
		missingPeers = nil
	})
	if err != nil && !errors.Is(err, ErrNoActiveChannels) {
		return fmt.Errorf("unable to fetch channels: %w", err)
	}

	// Early exit if no repairs needed.
	if len(missingPeers) == 0 {
		return nil
	}

	// Create all missing link nodes in a single write transaction
	// using the LinkNodeDB abstraction.
	linkNodesToCreate := make([]*LinkNode, 0, len(missingPeers))
	for _, remotePub := range missingPeers {
		linkNode := NewLinkNode(c.linkNodeDB, network, remotePub)
		linkNodesToCreate = append(linkNodesToCreate, linkNode)

		log.Infof("Repairing missing link node for peer %x",
			remotePub.SerializeCompressed())
	}

	err = c.linkNodeDB.CreateLinkNodes(nil, linkNodesToCreate)
	if err != nil {
		return err
	}

	log.Infof("Repaired %d missing link nodes on startup",
		len(missingPeers))

	return nil
}

// ChannelShell is a shell of a channel that is meant to be used for channel
// recovery purposes.
type ChannelShell = chanstate.ChannelShell

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
			channel.SetChannelStatusForStore(
				channel.ChannelStatusForStore() |
					ChanStatusRestored,
			)

			// First, we'll attempt to create a new open channel
			// and link node for this channel. If the channel
			// already exists, then in order to ensure this method
			// is idempotent, we'll continue to the next step.
			channel.Db = c
			err := syncNewChannel(
				tx, channel, channelShell.NodeAddrs, c.backend,
				channel.FundingBroadcastHeight,
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

	return c.kvStore.SaveChannelOpeningState(outPoint, serializedState)
}

// GetChannelOpeningState fetches the serialized channel state for the provided
// outPoint from the database, or returns ErrChannelNotFound if the channel
// is not found.
func (c *ChannelStateDB) GetChannelOpeningState(outPoint []byte) ([]byte,
	error) {

	return c.kvStore.GetChannelOpeningState(outPoint)
}

// DeleteChannelOpeningState removes any state for outPoint from the database.
func (c *ChannelStateDB) DeleteChannelOpeningState(outPoint []byte) error {
	return c.kvStore.DeleteChannelOpeningState(outPoint)
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

// FetchHistoricalChannel fetches open channel data from the historical channel
// bucket.
func (c *ChannelStateDB) FetchHistoricalChannel(outPoint *wire.OutPoint) (
	*OpenChannel, error) {

	channel, err := c.kvStore.FetchHistoricalChannel(outPoint)
	if err != nil {
		return nil, err
	}

	channel.Db = c

	return channel, nil
}

var ErrHtlcUnknown = chanstate.ErrHtlcUnknown

// LookupFinalHtlc retrieves a final htlc resolution from the database. If the
// htlc has no final resolution yet, ErrHtlcUnknown is returned.
func (c *ChannelStateDB) LookupFinalHtlc(chanID lnwire.ShortChannelID,
	htlcIndex uint64) (*FinalHtlcInfo, error) {

	return c.kvStore.LookupFinalHtlc(chanID, htlcIndex)
}

// PutOnchainFinalHtlcOutcome stores the final on-chain outcome of an htlc in
// the database.
func (c *ChannelStateDB) PutOnchainFinalHtlcOutcome(
	chanID lnwire.ShortChannelID, htlcID uint64, settled bool) error {

	return c.kvStore.PutOnchainFinalHtlcOutcome(chanID, htlcID, settled)
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

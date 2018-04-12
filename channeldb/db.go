package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/coreos/bbolt"
	"github.com/go-errors/errors"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
)

const (
	dbName           = "channel.db"
	dbFilePermission = 0600
)

// migration is a function which takes a prior outdated version of the database
// instances and mutates the key/bucket structure to arrive at a more
// up-to-date version of the database.
type migration func(tx *bolt.Tx) error

type version struct {
	number    uint32
	migration migration
}

var (
	// dbVersions is storing all versions of database. If current version
	// of database don't match with latest version this list will be used
	// for retrieving all migration function that are need to apply to the
	// current db.
	dbVersions = []version{
		{
			// The base DB version requires no migration.
			number:    0,
			migration: nil,
		},
	}

	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian
)

var bufPool = &sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// DB is the primary datastore for the lnd daemon. The database stores
// information related to nodes, routing data, open/closed channels, fee
// schedules, and reputation data.
type DB struct {
	*bolt.DB
	dbPath string
}

// Open opens an existing channeldb. Any necessary schemas migrations due to
// updates will take place as necessary.
func Open(dbPath string) (*DB, error) {
	path := filepath.Join(dbPath, dbName)

	if !fileExists(path) {
		if err := createChannelDB(dbPath); err != nil {
			return nil, err
		}
	}

	bdb, err := bolt.Open(path, dbFilePermission, nil)
	if err != nil {
		return nil, err
	}

	chanDB := &DB{
		DB:     bdb,
		dbPath: dbPath,
	}

	// Synchronize the version of database and apply migrations if needed.
	if err := chanDB.syncVersions(dbVersions); err != nil {
		bdb.Close()
		return nil, err
	}

	return chanDB, nil
}

// Path returns the file path to the channel database.
func (d *DB) Path() string {
	return d.dbPath
}

// Wipe completely deletes all saved state within all used buckets within the
// database. The deletion is done in a single transaction, therefore this
// operation is fully atomic.
func (d *DB) Wipe() error {
	return d.Update(func(tx *bolt.Tx) error {
		err := tx.DeleteBucket(openChannelBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(closedChannelBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(invoiceBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(nodeInfoBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(nodeBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}
		err = tx.DeleteBucket(edgeBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}
		err = tx.DeleteBucket(edgeIndexBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}
		err = tx.DeleteBucket(graphMetaBucket)
		if err != nil && err != bolt.ErrBucketNotFound {
			return err
		}

		return nil
	})
}

// createChannelDB creates and initializes a fresh version of channeldb. In
// the case that the target path has not yet been created or doesn't yet exist,
// then the path is created. Additionally, all required top-level buckets used
// within the database are created.
func createChannelDB(dbPath string) error {
	if !fileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return err
		}
	}

	path := filepath.Join(dbPath, dbName)
	bdb, err := bolt.Open(path, dbFilePermission, nil)
	if err != nil {
		return err
	}

	err = bdb.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucket(openChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(closedChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(invoiceBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(nodeInfoBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(nodeBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(edgeBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(edgeIndexBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(graphMetaBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(metaBucket); err != nil {
			return err
		}

		meta := &Meta{
			DbVersionNumber: getLatestDBVersion(dbVersions),
		}
		return putMeta(meta, tx)
	})
	if err != nil {
		return fmt.Errorf("unable to create new channeldb")
	}

	return bdb.Close()
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

// FetchOpenChannels returns all stored currently active/open channels
// associated with the target nodeID. In the case that no active channels are
// known to have been created with this node, then a zero-length slice is
// returned.
func (d *DB) FetchOpenChannels(nodeID *btcec.PublicKey) ([]*OpenChannel, error) {
	var channels []*OpenChannel
	err := d.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChanBucket == nil {
			return nil
		}

		// Within this top level bucket, fetch the bucket dedicated to
		// storing open channel data specific to the remote node.
		pub := nodeID.SerializeCompressed()
		nodeChanBucket := openChanBucket.Bucket(pub)
		if nodeChanBucket == nil {
			return nil
		}

		// Next, we'll need to go down an additional layer in order to
		// retrieve the channels for each chain the node knows of.
		return nodeChanBucket.ForEach(func(chainHash, v []byte) error {
			// If there's a value, it's not a bucket so ignore it.
			if v != nil {
				return nil
			}

			// If we've found a valid chainhash bucket, then we'll
			// retrieve that so we can extract all the channels.
			chainBucket := nodeChanBucket.Bucket(chainHash)
			if chainBucket == nil {
				return fmt.Errorf("unable to read bucket for "+
					"chain=%x", chainHash[:])
			}

			// Finally, we both of the necessary buckets retrieved,
			// fetch all the active channels related to this node.
			nodeChannels, err := d.fetchNodeChannels(chainBucket)
			if err != nil {
				return fmt.Errorf("unable to read channel for "+
					"chain_hash=%x, node_key=%x: %v",
					chainHash[:], pub, err)
			}

			channels = nodeChannels
			return nil
		})
	})

	return channels, err
}

// fetchNodeChannels retrieves all active channels from the target chainBucket
// which is under a node's dedicated channel bucket. This function is typically
// used to fetch all the active channels related to a particular node.
func (d *DB) fetchNodeChannels(chainBucket *bolt.Bucket) ([]*OpenChannel, error) {

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
		chanBucket := chainBucket.Bucket(chanPoint)

		var outPoint wire.OutPoint
		err := readOutpoint(bytes.NewReader(chanPoint), &outPoint)
		if err != nil {
			return err
		}
		oChannel, err := fetchOpenChannel(chanBucket, &outPoint)
		if err != nil {
			return fmt.Errorf("unable to read channel data for "+
				"chan_point=%v: %v", outPoint, err)
		}
		oChannel.Db = d

		channels = append(channels, oChannel)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return channels, nil
}

// FetchAllChannels attempts to retrieve all open channels currently stored
// within the database, including pending open, fully open and channels waiting
// for a closing transaction to confirm.
func (d *DB) FetchAllChannels() ([]*OpenChannel, error) {
	var channels []*OpenChannel

	// TODO(halseth): fetch all in one db tx.
	openChannels, err := d.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}
	channels = append(channels, openChannels...)

	pendingChannels, err := d.FetchPendingChannels()
	if err != nil {
		return nil, err
	}
	channels = append(channels, pendingChannels...)

	waitingClose, err := d.FetchWaitingCloseChannels()
	if err != nil {
		return nil, err
	}
	channels = append(channels, waitingClose...)

	return channels, nil
}

// FetchAllOpenChannels will return all channels that have the funding
// transaction confirmed, and is not waiting for a closing transaction to be
// confirmed.
func (d *DB) FetchAllOpenChannels() ([]*OpenChannel, error) {
	return fetchChannels(d, false, false)
}

// FetchPendingChannels will return channels that have completed the process of
// generating and broadcasting funding transactions, but whose funding
// transactions have yet to be confirmed on the blockchain.
func (d *DB) FetchPendingChannels() ([]*OpenChannel, error) {
	return fetchChannels(d, true, false)
}

// FetchWaitingCloseChannels will return all channels that have been opened,
// but now is waiting for a closing transaction to be confirmed.
func (d *DB) FetchWaitingCloseChannels() ([]*OpenChannel, error) {
	return fetchChannels(d, false, true)
}

// fetchChannels attempts to retrieve channels currently stored in the
// database. The pending parameter determines whether only pending channels
// will be returned, or only open channels will be returned. The waitingClose
// parameter determines wheter only channels waiting for a closing transaction
// to be confirmed should be returned. If no active channels exist within the
// network, then ErrNoActiveChannels is returned.
func fetchChannels(d *DB, pending, waitingClose bool) ([]*OpenChannel, error) {
	var channels []*OpenChannel

	err := d.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoActiveChannels
		}

		// Next, fetch the bucket dedicated to storing metadata related
		// to all nodes. All keys within this bucket are the serialized
		// public keys of all our direct counterparties.
		nodeMetaBucket := tx.Bucket(nodeInfoBucket)
		if nodeMetaBucket == nil {
			return fmt.Errorf("node bucket not created")
		}

		// Finally for each node public key in the bucket, fetch all
		// the channels related to this particular node.
		return nodeMetaBucket.ForEach(func(k, v []byte) error {
			nodeChanBucket := openChanBucket.Bucket(k)
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
				chainBucket := nodeChanBucket.Bucket(chainHash)
				if chainBucket == nil {
					return fmt.Errorf("unable to read "+
						"bucket for chain=%x", chainHash[:])
				}

				nodeChans, err := d.fetchNodeChannels(chainBucket)
				if err != nil {
					return fmt.Errorf("unable to read "+
						"channel for chain_hash=%x, "+
						"node_key=%x: %v", chainHash[:], k, err)
				}
				for _, channel := range nodeChans {
					if channel.IsPending != pending {
						continue
					}

					// If the channel is in any other state
					// than Default, then it means it is
					// waiting to be closed.
					channelWaitingClose :=
						channel.ChanStatus != Default

					// Only include it if we requested
					// channels with the same waitingClose
					// status.
					if channelWaitingClose != waitingClose {
						continue
					}

					channels = append(channels, channel)
				}
				return nil
			})

		})
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
func (d *DB) FetchClosedChannels(pendingOnly bool) ([]*ChannelCloseSummary, error) {
	var chanSummaries []*ChannelCloseSummary

	if err := d.View(func(tx *bolt.Tx) error {
		closeBucket := tx.Bucket(closedChannelBucket)
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
func (d *DB) FetchClosedChannel(chanID *wire.OutPoint) (*ChannelCloseSummary, error) {
	var chanSummary *ChannelCloseSummary
	if err := d.View(func(tx *bolt.Tx) error {
		closeBucket := tx.Bucket(closedChannelBucket)
		if closeBucket == nil {
			return ErrClosedChannelNotFound
		}

		var b bytes.Buffer
		var err error
		if err = writeOutpoint(&b, chanID); err != nil {
			return err
		}

		summaryBytes := closeBucket.Get(b.Bytes())
		if summaryBytes == nil {
			return ErrClosedChannelNotFound
		}

		summaryReader := bytes.NewReader(summaryBytes)
		chanSummary, err = deserializeCloseChannelSummary(summaryReader)

		return err
	}); err != nil {
		return nil, err
	}

	return chanSummary, nil
}

// MarkChanFullyClosed marks a channel as fully closed within the database. A
// channel should be marked as fully closed if the channel was initially
// cooperatively closed and it's reached a single confirmation, or after all the
// pending funds in a channel that has been forcibly closed have been swept.
func (d *DB) MarkChanFullyClosed(chanPoint *wire.OutPoint) error {
	return d.Update(func(tx *bolt.Tx) error {
		var b bytes.Buffer
		if err := writeOutpoint(&b, chanPoint); err != nil {
			return err
		}

		chanID := b.Bytes()

		closedChanBucket, err := tx.CreateBucketIfNotExists(
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

		return closedChanBucket.Put(chanID, newSummary.Bytes())
	})
}

// syncVersions function is used for safe db version synchronization. It
// applies migration functions to the current database and recovers the
// previous state of db if at least one error/panic appeared during migration.
func (d *DB) syncVersions(versions []version) error {
	meta, err := d.FetchMeta(nil)
	if err != nil {
		if err == ErrMetaNotFound {
			meta = &Meta{}
		} else {
			return err
		}
	}

	// If the current database version matches the latest version number,
	// then we don't need to perform any migrations.
	latestVersion := getLatestDBVersion(versions)
	log.Infof("Checking for schema update: latest_version=%v, "+
		"db_version=%v", latestVersion, meta.DbVersionNumber)
	if meta.DbVersionNumber == latestVersion {
		return nil
	}

	log.Infof("Performing database schema migration")

	// Otherwise, we fetch the migrations which need to applied, and
	// execute them serially within a single database transaction to ensure
	// the migration is atomic.
	migrations, migrationVersions := getMigrationsToApply(versions,
		meta.DbVersionNumber)
	return d.Update(func(tx *bolt.Tx) error {
		for i, migration := range migrations {
			if migration == nil {
				continue
			}

			log.Infof("Applying migration #%v", migrationVersions[i])

			if err := migration(tx); err != nil {
				log.Infof("Unable to apply migration #%v",
					migrationVersions[i])
				return err
			}
		}

		meta.DbVersionNumber = latestVersion
		return putMeta(meta, tx)
	})
}

// ChannelGraph returns a new instance of the directed channel graph.
func (d *DB) ChannelGraph() *ChannelGraph {
	return &ChannelGraph{d}
}

func getLatestDBVersion(versions []version) uint32 {
	return versions[len(versions)-1].number
}

// getMigrationsToApply retrieves the migration function that should be
// applied to the database.
func getMigrationsToApply(versions []version, version uint32) ([]migration, []uint32) {
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

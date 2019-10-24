package migration_01_to_11

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/coreos/bbolt"
)

const (
	dbName           = "channel.db"
	dbFilePermission = 0600
)

// migration is a function which takes a prior outdated version of the database
// instances and mutates the key/bucket structure to arrive at a more
// up-to-date version of the database.
type migration func(tx *bbolt.Tx) error

type version struct {
	number    uint32
	migration migration
}

var (
	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian
)

// DB is the primary datastore for the lnd daemon. The database stores
// information related to nodes, routing data, open/closed channels, fee
// schedules, and reputation data.
type DB struct {
	*bbolt.DB
	dbPath string
	graph  *ChannelGraph
	now    func() time.Time
}

// Open opens an existing channeldb. Any necessary schemas migrations due to
// updates will take place as necessary.
func Open(dbPath string, modifiers ...OptionModifier) (*DB, error) {
	path := filepath.Join(dbPath, dbName)

	if !fileExists(path) {
		if err := createChannelDB(dbPath); err != nil {
			return nil, err
		}
	}

	opts := DefaultOptions()
	for _, modifier := range modifiers {
		modifier(&opts)
	}

	// Specify bbolt freelist options to reduce heap pressure in case the
	// freelist grows to be very large.
	options := &bbolt.Options{
		NoFreelistSync: opts.NoFreelistSync,
		FreelistType:   bbolt.FreelistMapType,
	}

	bdb, err := bbolt.Open(path, dbFilePermission, options)
	if err != nil {
		return nil, err
	}

	chanDB := &DB{
		DB:     bdb,
		dbPath: dbPath,
		now:    time.Now,
	}
	chanDB.graph = newChannelGraph(
		chanDB, opts.RejectCacheSize, opts.ChannelCacheSize,
	)

	return chanDB, nil
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
	bdb, err := bbolt.Open(path, dbFilePermission, nil)
	if err != nil {
		return err
	}

	err = bdb.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucket(openChannelBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(closedChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(invoiceBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(paymentBucket); err != nil {
			return err
		}

		nodes, err := tx.CreateBucket(nodeBucket)
		if err != nil {
			return err
		}
		_, err = nodes.CreateBucket(aliasIndexBucket)
		if err != nil {
			return err
		}
		_, err = nodes.CreateBucket(nodeUpdateIndexBucket)
		if err != nil {
			return err
		}

		edges, err := tx.CreateBucket(edgeBucket)
		if err != nil {
			return err
		}
		if _, err := edges.CreateBucket(edgeIndexBucket); err != nil {
			return err
		}
		if _, err := edges.CreateBucket(edgeUpdateIndexBucket); err != nil {
			return err
		}
		if _, err := edges.CreateBucket(channelPointBucket); err != nil {
			return err
		}
		if _, err := edges.CreateBucket(zombieBucket); err != nil {
			return err
		}

		graphMeta, err := tx.CreateBucket(graphMetaBucket)
		if err != nil {
			return err
		}
		_, err = graphMeta.CreateBucket(pruneLogBucket)
		if err != nil {
			return err
		}

		if _, err := tx.CreateBucket(metaBucket); err != nil {
			return err
		}

		meta := &Meta{
			DbVersionNumber: 0,
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

// FetchClosedChannels attempts to fetch all closed channels from the database.
// The pendingOnly bool toggles if channels that aren't yet fully closed should
// be returned in the response or not. When a channel was cooperatively closed,
// it becomes fully closed after a single confirmation.  When a channel was
// forcibly closed, it will become fully closed after _all_ the pending funds
// (if any) have been swept.
func (d *DB) FetchClosedChannels(pendingOnly bool) ([]*ChannelCloseSummary, error) {
	var chanSummaries []*ChannelCloseSummary

	if err := d.View(func(tx *bbolt.Tx) error {
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
	return d.Update(func(tx *bbolt.Tx) error {
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
	return d.graph
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

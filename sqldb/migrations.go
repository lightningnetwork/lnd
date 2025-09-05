package sqldb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/httpfs"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/pmezard/go-difflib/difflib"
)

var (
	// migrationConfig defines a list of migrations to be applied to the
	// database. Each migration is assigned a version number, determining
	// its execution order.
	// The schema version, tracked by golang-migrate, ensures migrations are
	// applied to the correct schema. For migrations involving only schema
	// changes, the migration function can be left nil. For custom
	// migrations an implemented migration function is required.
	//
	// NOTE: The migration function may have runtime dependencies, which
	// must be injected during runtime.
	migrationConfig = append([]MigrationConfig{
		{
			Name:          "000001_invoices",
			Version:       1,
			SchemaVersion: 1,
		},
		{
			Name:          "000002_amp_invoices",
			Version:       2,
			SchemaVersion: 2,
		},
		{
			Name:          "000003_invoice_events",
			Version:       3,
			SchemaVersion: 3,
		},
		{
			Name:          "000004_invoice_expiry_fix",
			Version:       4,
			SchemaVersion: 4,
		},
		{
			Name:          "000005_migration_tracker",
			Version:       5,
			SchemaVersion: 5,
		},
		{
			Name:          "000006_invoice_migration",
			Version:       6,
			SchemaVersion: 6,
		},
		{
			Name:          "kv_invoice_migration",
			Version:       7,
			SchemaVersion: 6,
			// A migration function is may be attached to this
			// migration to migrate KV invoices to the native SQL
			// schema. This is optional and can be disabled by the
			// user if necessary.
		},
		{
			Name:          "000007_invoice_add_settled_index",
			Version:       8,
			SchemaVersion: 7,
		},
		{
			Name:          "000008_graph",
			Version:       9,
			SchemaVersion: 8,
		},
		{
			Name:          "kv_graph_migration",
			Version:       10,
			SchemaVersion: 8,
			// A migration function may be attached to this
			// migration to migrate KV graph to the native SQL
			// schema. This is optional and can be disabled by the
			// user if necessary.
		},
	}, migrationAdditions...)

	// ErrMigrationMismatch is returned when a migrated record does not
	// match the original record.
	ErrMigrationMismatch = fmt.Errorf("migrated record does not match " +
		"original record")
)

// MigrationConfig is a configuration struct that describes SQL migrations. Each
// migration is associated with a specific schema version and a global database
// version. Migrations are applied in the order of their global database
// version. If a migration includes a non-nil MigrationFn, it is executed after
// the SQL schema has been migrated to the corresponding schema version.
type MigrationConfig struct {
	// Name is the name of the migration.
	Name string

	// Version represents the "global" database version for this migration.
	// Unlike the schema version tracked by golang-migrate, it encompasses
	// all migrations, including those managed by golang-migrate as well
	// as custom in-code migrations.
	Version int

	// SchemaVersion represents the schema version tracked by golang-migrate
	// at which the migration is applied.
	SchemaVersion int

	// MigrationFn is the function executed for custom migrations at the
	// specified version. It is used to handle migrations that cannot be
	// performed through SQL alone. If set to nil, no custom migration is
	// applied.
	MigrationFn func(tx *sqlc.Queries) error
}

// MigrationTarget is a functional option that can be passed to applyMigrations
// to specify a target version to migrate to.
type MigrationTarget func(mig *migrate.Migrate) error

// MigrationExecutor is an interface that abstracts the migration functionality.
type MigrationExecutor interface {
	// ExecuteMigrations runs database migrations up to the specified target
	// version or all migrations if no target is specified. A migration may
	// include a schema change, a custom migration function, or both.
	// Developers must ensure that migrations are defined in the correct
	// order. Migration details are stored in the global variable
	// migrationConfig.
	ExecuteMigrations(target MigrationTarget) error

	// GetSchemaVersion returns the current schema version of the database.
	GetSchemaVersion() (int, bool, error)

	// SetSchemaVersion sets the schema version of the database.
	//
	// NOTE: This alters the internal database schema tracker. USE WITH
	// CAUTION!!!
	SetSchemaVersion(version int, dirty bool) error
}

var (
	// TargetLatest is a MigrationTarget that migrates to the latest
	// version available.
	TargetLatest = func(mig *migrate.Migrate) error {
		return mig.Up()
	}

	// TargetVersion is a MigrationTarget that migrates to the given
	// version.
	TargetVersion = func(version uint) MigrationTarget {
		return func(mig *migrate.Migrate) error {
			return mig.Migrate(version)
		}
	}
)

// GetMigrations returns a copy of the migration configuration.
func GetMigrations() []MigrationConfig {
	migrations := make([]MigrationConfig, len(migrationConfig))
	copy(migrations, migrationConfig)

	return migrations
}

// migrationLogger is a logger that wraps the passed btclog.Logger so it can be
// used to log migrations.
type migrationLogger struct {
	log btclog.Logger
}

// Printf is like fmt.Printf. We map this to the target logger based on the
// current log level.
func (m *migrationLogger) Printf(format string, v ...interface{}) {
	// Trim trailing newlines from the format.
	format = strings.TrimRight(format, "\n")

	switch m.log.Level() {
	case btclog.LevelTrace:
		m.log.Tracef(format, v...)
	case btclog.LevelDebug:
		m.log.Debugf(format, v...)
	case btclog.LevelInfo:
		m.log.Infof(format, v...)
	case btclog.LevelWarn:
		m.log.Warnf(format, v...)
	case btclog.LevelError:
		m.log.Errorf(format, v...)
	case btclog.LevelCritical:
		m.log.Criticalf(format, v...)
	case btclog.LevelOff:
	}
}

// Verbose should return true when verbose logging output is wanted
func (m *migrationLogger) Verbose() bool {
	return m.log.Level() <= btclog.LevelDebug
}

// applyMigrations executes all database migration files found in the given file
// system under the given path, using the passed database driver and database
// name.
func applyMigrations(fs fs.FS, driver database.Driver, path,
	dbName string, targetVersion MigrationTarget) error {

	// With the migrate instance open, we'll create a new migration source
	// using the embedded file system stored in sqlSchemas. The library
	// we're using can't handle a raw file system interface, so we wrap it
	// in this intermediate layer.
	migrateFileServer, err := httpfs.New(http.FS(fs), path)
	if err != nil {
		return err
	}

	// Finally, we'll run the migration with our driver above based on the
	// open DB, and also the migration source stored in the file system
	// above.
	sqlMigrate, err := migrate.NewWithInstance(
		"migrations", migrateFileServer, dbName, driver,
	)
	if err != nil {
		return err
	}

	migrationVersion, _, err := sqlMigrate.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		log.Errorf("Unable to determine current migration version: %v",
			err)

		return err
	}

	log.Infof("Applying migrations from version=%v", migrationVersion)

	// Apply our local logger to the migration instance.
	sqlMigrate.Log = &migrationLogger{log}

	// Execute the migration based on the target given.
	err = targetVersion(sqlMigrate)
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}

	return nil
}

// replacerFS is an implementation of a fs.FS virtual file system that wraps an
// existing file system but does a search-and-replace operation on each file
// when it is opened.
type replacerFS struct {
	parentFS fs.FS
	replaces map[string]string
}

// A compile-time assertion to make sure replacerFS implements the fs.FS
// interface.
var _ fs.FS = (*replacerFS)(nil)

// newReplacerFS creates a new replacer file system, wrapping the given parent
// virtual file system. Each file within the file system is undergoing a
// search-and-replace operation when it is opened, using the given map where the
// key denotes the search term and the value the term to replace each occurrence
// with.
func newReplacerFS(parent fs.FS, replaces map[string]string) *replacerFS {
	return &replacerFS{
		parentFS: parent,
		replaces: replaces,
	}
}

// Open opens a file in the virtual file system.
//
// NOTE: This is part of the fs.FS interface.
func (t *replacerFS) Open(name string) (fs.File, error) {
	f, err := t.parentFS.Open(name)
	if err != nil {
		return nil, err
	}

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}

	if stat.IsDir() {
		return f, err
	}

	return newReplacerFile(f, t.replaces)
}

type replacerFile struct {
	parentFile fs.File
	buf        bytes.Buffer
}

// A compile-time assertion to make sure replacerFile implements the fs.File
// interface.
var _ fs.File = (*replacerFile)(nil)

func newReplacerFile(parent fs.File, replaces map[string]string) (*replacerFile,
	error) {

	content, err := io.ReadAll(parent)
	if err != nil {
		return nil, err
	}

	contentStr := string(content)
	for from, to := range replaces {
		contentStr = strings.ReplaceAll(contentStr, from, to)
	}

	var buf bytes.Buffer
	_, err = buf.WriteString(contentStr)
	if err != nil {
		return nil, err
	}

	return &replacerFile{
		parentFile: parent,
		buf:        buf,
	}, nil
}

// Stat returns statistics/info about the file.
//
// NOTE: This is part of the fs.File interface.
func (t *replacerFile) Stat() (fs.FileInfo, error) {
	return t.parentFile.Stat()
}

// Read reads as many bytes as possible from the file into the given slice.
//
// NOTE: This is part of the fs.File interface.
func (t *replacerFile) Read(bytes []byte) (int, error) {
	return t.buf.Read(bytes)
}

// Close closes the underlying file.
//
// NOTE: This is part of the fs.File interface.
func (t *replacerFile) Close() error {
	// We already fully read and then closed the file when creating this
	// instance, so there's nothing to do for us here.
	return nil
}

// ApplyMigrations applies the provided migrations to the database in sequence.
// It ensures migrations are executed in the correct order, applying both custom
// migration functions and SQL migrations as needed.
func ApplyMigrations(ctx context.Context, db *BaseDB,
	migrator MigrationExecutor, migrations []MigrationConfig) error {

	// Ensure that the migrations are sorted by version.
	for i := 0; i < len(migrations); i++ {
		if migrations[i].Version != i+1 {
			return fmt.Errorf("migration version %d is out of "+
				"order. Expected %d", migrations[i].Version,
				i+1)
		}
	}
	// Construct a transaction executor to apply custom migrations.
	executor := NewTransactionExecutor(db, func(tx *sql.Tx) *sqlc.Queries {
		return db.WithTx(tx)
	})

	currentVersion := 0
	version, err := db.GetDatabaseVersion(ctx)
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return fmt.Errorf("error getting current database "+
				"version: %w", err)
		}

		currentVersion = int(version)
	} else {
		// Since we don't have a version tracked by our own table yet,
		// we'll use the schema version reported by sqlc to determine
		// the current version.
		//
		// NOTE: This is safe because the first in-code migration was
		// introduced in version 7. This is only possible if the user
		// has a schema version <= 4.
		var dirty bool
		currentVersion, dirty, err = migrator.GetSchemaVersion()
		if err != nil {
			return err
		}

		log.Infof("No database version found, using schema version %d "+
			"(dirty=%v) as base version", currentVersion, dirty)
	}

	// Due to an a migration issue in v0.19.0-rc1 we may be at version 2 and
	// have a dirty schema due to failing migration 3. If this is indeed the
	// case, we need to reset the dirty flag to be able to apply the fixed
	// migration.
	// NOTE: this could be removed as soon as we drop v0.19.0-beta.
	if version == 2 {
		schemaVersion, dirty, err := migrator.GetSchemaVersion()
		if err != nil {
			return err
		}

		if schemaVersion == 3 && dirty {
			log.Warnf("Schema version %d is dirty. This is "+
				"likely a consequence of a failed migration "+
				"in v0.19.0-rc1. Attempting to recover by "+
				"resetting the dirty flag", schemaVersion)

			err = migrator.SetSchemaVersion(4, false)
			if err != nil {
				return err
			}
		}
	}

	for _, migration := range migrations {
		if migration.Version <= currentVersion {
			log.Infof("Skipping migration '%s' (version %d) as it "+
				"has already been applied", migration.Name,
				migration.Version)

			continue
		}

		log.Infof("Migrating SQL schema to version %d",
			migration.SchemaVersion)

		// Execute SQL schema migrations up to the target version.
		err = migrator.ExecuteMigrations(
			TargetVersion(uint(migration.SchemaVersion)),
		)
		if err != nil {
			return fmt.Errorf("error executing schema migrations "+
				"to target version %d: %w",
				migration.SchemaVersion, err)
		}

		opts := WriteTxOpt()

		// Run the custom migration as a transaction to ensure
		// atomicity. If successful, mark the migration as complete in
		// the migration tracker table.
		err = executor.ExecTx(ctx, opts, func(tx *sqlc.Queries) error {
			// Apply the migration function if one is provided.
			if migration.MigrationFn != nil {
				log.Infof("Applying custom migration '%v' "+
					"(version %d) to schema version %d",
					migration.Name, migration.Version,
					migration.SchemaVersion)

				err = migration.MigrationFn(tx)
				if err != nil {
					return fmt.Errorf("error applying "+
						"migration '%v' (version %d) "+
						"to schema version %d: %w",
						migration.Name,
						migration.Version,
						migration.SchemaVersion, err)
				}

				log.Infof("Migration '%v' (version %d) "+
					"applied ", migration.Name,
					migration.Version)
			}

			// Mark the migration as complete by adding the version
			// to the migration tracker table along with the current
			// timestamp.
			err = tx.SetMigration(ctx, sqlc.SetMigrationParams{
				Version:       int32(migration.Version),
				MigrationTime: time.Now(),
			})
			if err != nil {
				return fmt.Errorf("error setting migration "+
					"version %d: %w", migration.Version,
					err)
			}

			return nil
		}, func() {})
		if err != nil {
			return err
		}
	}

	return nil
}

// CompareRecords checks if the original and migrated objects are equal. If
// they are not, it returns an error with a unified diff of the two objects.
func CompareRecords(original, migrated any, identifier string) error {
	if reflect.DeepEqual(original, migrated) {
		return nil
	}

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(spew.Sdump(original)),
		B:        difflib.SplitLines(spew.Sdump(migrated)),
		FromFile: "Expected",
		FromDate: "",
		ToFile:   "Actual",
		ToDate:   "",
		Context:  3,
	}
	diffText, _ := difflib.GetUnifiedDiffString(diff)

	return fmt.Errorf("%w: %s.\n%v", ErrMigrationMismatch, identifier,
		diffText)
}

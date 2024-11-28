package sqldb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"github.com/btcsuite/btclog/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/httpfs"
)

// MigrationConfig is a configuration struct that is used to describe in-code
// migration targets. Each such migration is applied at a specific schema
// version.
type MigrationConfig struct {
	// Name is the name of the migration.
	Name string

	// Version is the schema version at which the migration is applied.
	Version int

	// MigrationFn is the migration function that is applied at the given
	// version.
	MigrationFn func(db *BaseDB) error
}

// MigrationTarget is a functional option that can be passed to applyMigrations
// to specify a target version to migrate to.
type MigrationTarget func(mig *migrate.Migrate) error

// MigrationExecutor is an interface that abstracts the migration functionality.
type MigrationExecutor interface {
	// CurrentSchemaVersion returns the current schema version of the
	// database.
	CurrentSchemaVersion() (int, error)

	// ExecuteMigrations runs migrations for the database, depending on the
	// target given, either all migrations or up to a given version.
	ExecuteMigrations(target MigrationTarget) error
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
// migration functions and SQL migrations as needed. If no custom migrations are
// specified, all SQL migrations are applied up to the latest version.
func ApplyMigrations(db *BaseDB, migrator MigrationExecutor,
	migrations []MigrationConfig) error {

	// Sort migrations by version to ensure they are applied in order.
	sort.SliceStable(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	currentVersion, err := migrator.CurrentSchemaVersion()
	if err != nil {
		return fmt.Errorf("error determining current schema "+
			"version: %w", err)
	}

	for _, migration := range migrations {
		if migration.Version < currentVersion {
			log.Infof("Skipping migration '%v' as current schema "+
				"version %v is higher than recommended"+
				"version %v", migration.Name, currentVersion,
				migration.Version)

			continue
		}

		log.Infof("Migrating SQL DB to version %v",
			migration.Version)

		// Execute SQL migrations up to the target version.
		err := migrator.ExecuteMigrations(
			TargetVersion(uint(migration.Version)),
		)
		if err != nil {
			return fmt.Errorf("error executing migrations to "+
				"target version %v: %w", migration.Version, err)
		}

		log.Infof("Applying migration %v to target version %v",
			migration.Name, migration.Version)

		// Apply the migration function.
		err = migration.MigrationFn(db)
		if err != nil {
			return fmt.Errorf("error applying migration %v to "+
				"target version %v: %w", migration.Name,
				migration.Version, err)
		}
	}

	// Apply any remaining SQL migrations up to the latest version.
	err = migrator.ExecuteMigrations(TargetLatest)
	if err != nil {
		return fmt.Errorf("error executing migrations to "+
			"latest version: %w", err)
	}

	return nil
}

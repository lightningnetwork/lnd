package sqldb

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/httpfs"
)

// applyMigrations executes all database migration files found in the given file
// system under the given path, using the passed database driver and database
// name.
func applyMigrations(fs fs.FS, driver database.Driver, path,
	dbName string) error {

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
	err = sqlMigrate.Up()
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

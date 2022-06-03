//go:build kvdb_sqlite
// +build kvdb_sqlite

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"path"

	"github.com/btcsuite/btcwallet/walletdb"
)

func NewSqliteTestFixture(prefix string) (*fixture, error) {
	tmpDir, err := ioutil.TempDir("", "sqlite")
	if err != nil {
		return nil, fmt.Errorf("unable to create temp dir for sqlite db: %w", err)
	}

	cfg := &Config{
		TablePrefix: prefix,
		Filename:    path.Join(tmpDir, "sqlite_test.db"),
	}

	db, err := newSqliteBackend(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to create temp dir for sqlite db: %w", err)
	}

	return &fixture{
		cleanup: func() {
			_ = db.Close()
			os.RemoveAll(tmpDir)
		},
		Config: cfg,
		Db:     db,
		dbConn: db.db,
	}, nil
}

type fixture struct {
	cleanup func()
	Config  *Config
	Db      walletdb.DB
	dbConn  *sql.DB
}

func (b *fixture) DB() walletdb.DB {
	return b.Db
}

// Dump returns the raw contents of the database.
func (b *fixture) Dump() (map[string]interface{}, error) {
	rows, err := b.dbConn.Query(`
		SELECT name FROM sqlite_schema
		WHERE type='table'
		AND name like '` + b.Config.TablePrefix + `%'
`)
	if err != nil {
		return nil, err
	}

	var tables []string
	for rows.Next() {
		var table string
		err := rows.Scan(&table)
		if err != nil {
			return nil, err
		}

		tables = append(tables, table)
	}

	result := make(map[string]interface{})

	for _, table := range tables {
		rows, err := b.dbConn.Query("SELECT * FROM " + table + " ORDER BY id ASC")
		if err != nil {
			return nil, err
		}

		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		colCount := len(cols)

		var tableRows []map[string]interface{}
		for rows.Next() {
			values := make([]interface{}, colCount)
			valuePtrs := make([]interface{}, colCount)
			for i := range values {
				valuePtrs[i] = &values[i]
			}

			err := rows.Scan(valuePtrs...)
			if err != nil {
				return nil, err
			}

			tableData := make(map[string]interface{})
			for i, v := range values {
				// Cast byte slices to string to keep the
				// expected database contents in test code more
				// readable.
				if ar, ok := v.([]uint8); ok {
					v = string(ar)
				}
				tableData[cols[i]] = v
			}

			tableRows = append(tableRows, tableData)
		}

		result[table] = tableRows
	}

	return result, nil
}

func (b *fixture) Cleanup() {
	b.cleanup()
}

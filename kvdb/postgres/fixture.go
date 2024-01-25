//go:build kvdb_postgres

package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
)

const (
	testDsnTemplate = "postgres://postgres:postgres@localhost:9876/%v?sslmode=disable"
	prefix          = "test"
)

func getTestDsn(dbName string) string {
	return fmt.Sprintf(testDsnTemplate, dbName)
}

var testPostgres *embeddedpostgres.EmbeddedPostgres

const testMaxConnections = 200

// StartEmbeddedPostgres starts an embedded postgres instance. This only needs
// to be done once, because NewFixture will create random new databases on every
// call. It returns a stop closure that stops the database if called.
func StartEmbeddedPostgres() (func() error, error) {
	sqlbase.Init(testMaxConnections)

	postgres := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(9876).
			StartParameters(
				map[string]string{
					"max_connections": fmt.Sprintf(
						"%d", testMaxConnections,
					),
				},
			),
	)

	err := postgres.Start()
	if err != nil {
		return nil, err
	}

	testPostgres = postgres

	return testPostgres.Stop, nil
}

// NewFixture returns a new postgres test database. The database name is
// randomly generated.
func NewFixture(dbName string) (*fixture, error) {
	if dbName == "" {
		// Create random database name.
		randBytes := make([]byte, 8)
		_, err := rand.Read(randBytes)
		if err != nil {
			return nil, err
		}

		dbName = "test_" + hex.EncodeToString(randBytes)
	}

	// Create database if it doesn't exist yet.
	dbConn, err := sql.Open("pgx", getTestDsn("postgres"))
	if err != nil {
		return nil, err
	}
	defer dbConn.Close()

	_, err = dbConn.ExecContext(
		context.Background(), "CREATE DATABASE "+dbName,
	)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return nil, err
	}

	// Open database
	dsn := getTestDsn(dbName)
	db, err := newPostgresBackend(
		context.Background(),
		&Config{
			Dsn:     dsn,
			Timeout: time.Minute,
		},
		prefix,
	)
	if err != nil {
		return nil, err
	}

	return &fixture{
		Dsn: dsn,
		Db:  db,
	}, nil
}

type fixture struct {
	Dsn string
	Db  walletdb.DB
}

func (b *fixture) DB() walletdb.DB {
	return b.Db
}

// Dump returns the raw contents of the database.
func (b *fixture) Dump() (map[string]interface{}, error) {
	dbConn, err := sql.Open("pgx", b.Dsn)
	if err != nil {
		return nil, err
	}

	rows, err := dbConn.Query(
		"SELECT tablename FROM pg_catalog.pg_tables WHERE schemaname='public'",
	)
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
		rows, err := dbConn.Query("SELECT * FROM " + table)
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

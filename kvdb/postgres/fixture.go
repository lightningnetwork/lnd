//go:build kvdb_postgres

package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	_ "github.com/lib/pq"
	"github.com/lightningnetwork/lnd/kvdb/sqlbase"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
)

const (
	testPgUser  = "test"
	testPgPass  = "test"
	testPgDb    = "test"
	PostgresTag = "11"

	testDsnTemplate = "postgres://test:test@%s:%d/%s?sslmode=disable"
	prefix          = "test"

	testMaxConnections = 200
)

var (
	// DefaultExpiry is the default expiry time for the test fixture. This
	// means a single unit test is not allowed to run more than 3 hours
	// (which is the same duration as the default test timeout).
	DefaultExpiry = 180 * time.Minute
)

func getTestDsn(host string, port int, dbName string) string {
	return fmt.Sprintf(testDsnTemplate, host, port, dbName)
}

var testPostgres *testPgFixture

// testPgFixture is a test fixture that starts a Postgres 11 instance in a
// docker container.
type testPgFixture struct {
	db       *sql.DB
	pool     *dockertest.Pool
	resource *dockertest.Resource
	host     string
	port     int
	Dsn      string
}

// StartEmbeddedPostgres starts an embedded postgres instance. This only needs
// to be done once, because NewFixture will create random new databases on every
// call. It returns a stop closure that stops the database if called.
func StartEmbeddedPostgres() (func() error, error) {
	sqlbase.Init(testMaxConnections)

	// uses a sensible default on windows (tcp/http) and linux/osx (socket)
	pool, err := dockertest.NewPool("")
	if err != nil {
		return nil, fmt.Errorf("error creating pool: %w", err)
	}

	// pulls an image, creates a container based on it and runs it
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        PostgresTag,
		Env: []string{
			fmt.Sprintf("POSTGRES_USER=%v", testPgUser),
			fmt.Sprintf("POSTGRES_PASSWORD=%v", testPgPass),
			fmt.Sprintf("POSTGRES_DB=%v", testPgDb),
			"listen_addresses='*'",
		},
	}, func(config *docker.HostConfig) {
		// Set AutoRemove to true so that stopped container goes away
		// by itself.
		config.AutoRemove = true
		config.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		return nil, fmt.Errorf("error creating resource: %w", err)
	}

	hostAndPort := resource.GetHostPort("5432/tcp")
	parts := strings.Split(hostAndPort, ":")
	host := parts[0]
	port, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("error parsing port: %w", err)
	}

	pgFixture := &testPgFixture{
		host: host,
		port: int(port),
	}
	databaseURL := getTestDsn(host, int(port), testPgDb)

	// Tell docker to hard kill the container in "expiry" seconds.
	err = resource.Expire(uint(DefaultExpiry.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("error setting expiry: %w", err)
	}

	// Exponential backoff-retry, because the application in the container
	// might not be ready to accept connections yet
	pool.MaxWait = 120 * time.Second

	var testDB *sql.DB
	err = pool.Retry(func() error {
		testDB, err = sql.Open("postgres", databaseURL)
		if err != nil {
			return err
		}
		return testDB.Ping()
	})
	if err != nil {
		return nil, fmt.Errorf("error connecting to docker: %w", err)
	}

	// Now fill remaining fields of the fixture.
	pgFixture.db = testDB
	pgFixture.pool = pool
	pgFixture.resource = resource
	pgFixture.Dsn = databaseURL

	testPostgres = pgFixture

	return pgFixture.tearDown, nil
}

// tearDown stops the underlying docker container.
func (f *testPgFixture) tearDown() error {
	return f.pool.Purge(f.resource)
}

// NewTestPgFixture constructs a new TestPgFixture starting up a docker
// container running Postgres 11. The started container will expire in after
// the passed duration.
func NewTestPgFixture(t testing.TB, dbName string) *fixture {
	if dbName == "" {
		// Create random database name.
		randBytes := make([]byte, 8)
		_, err := rand.Read(randBytes)
		require.NoError(t, err)

		dbName = "test_" + hex.EncodeToString(randBytes)
	}

	// Create database if it doesn't exist yet.
	dbConn, err := sql.Open("pgx", testPostgres.Dsn)
	require.NoError(t, err)
	defer dbConn.Close()

	_, err = dbConn.ExecContext(
		context.Background(), "CREATE DATABASE "+dbName,
	)
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unable to create database: %v", err)
	}

	// Open new database.
	dsn := getTestDsn(testPostgres.host, testPostgres.port, dbName)
	db, err := newPostgresBackend(
		context.Background(),
		&Config{
			Dsn:     dsn,
			Timeout: time.Minute,
		},
		prefix,
	)
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	return &fixture{
		Dsn: dsn,
		Db:  db,
	}
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
		"SELECT tablename FROM pg_catalog.pg_tables WHERE " +
			"schemaname='public'",
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

func init() {
	sqlbase.Init(testMaxConnections)
}

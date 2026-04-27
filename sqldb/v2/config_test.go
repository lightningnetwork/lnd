package sqldb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSqliteConfigMaxConns verifies that SQLite keeps the low default
// connection limit unless the caller overrides it explicitly.
func TestSqliteConfigMaxConns(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		maxConns     int
		expectedConn int
	}{
		{
			name:         "default limit",
			expectedConn: DefaultSqliteMaxConns,
		},
		{
			name:         "explicit limit",
			maxConns:     7,
			expectedConn: 7,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := &SqliteConfig{
				MaxConnections: testCase.maxConns,
			}

			require.Equal(t, testCase.expectedConn, cfg.MaxConns())
		})
	}
}

// TestSqliteConfigMaxIdleConns verifies that SQLite defaults its idle
// connections to the open connection limit unless the caller overrides it.
func TestSqliteConfigMaxIdleConns(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name             string
		maxConns         int
		maxIdleConns     int
		expectedIdleConn int
	}{
		{
			name:             "default idle limit",
			expectedIdleConn: DefaultSqliteMaxConns,
		},
		{
			name:             "inherits explicit open limit",
			maxConns:         4,
			expectedIdleConn: 4,
		},
		{
			name:             "explicit idle limit",
			maxConns:         4,
			maxIdleConns:     3,
			expectedIdleConn: 3,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := &SqliteConfig{
				MaxConnections:     testCase.maxConns,
				MaxIdleConnections: testCase.maxIdleConns,
			}

			require.Equal(t, testCase.expectedIdleConn,
				cfg.MaxIdleConns())
		})
	}
}

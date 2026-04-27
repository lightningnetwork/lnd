package sqldb

import (
	"io"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

// TestPostgresSchemaReplacements verifies that the Postgres schema
// replacements do not rewrite SQL keywords that only contain a replacement
// token as a substring.
func TestPostgresSchemaReplacements(t *testing.T) {
	t.Parallel()

	postgresFS := newReplacerFS(fstest.MapFS{
		"schema.sql": &fstest.MapFile{
			Data: []byte("created_at TIMESTAMP NOT NULL DEFAULT " +
				"CURRENT_TIMESTAMP"),
		},
	}, postgresSchemaReplacements)

	file, err := postgresFS.Open("schema.sql")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, file.Close())
	})

	content, err := io.ReadAll(file)
	require.NoError(t, err)

	require.Equal(t,
		"created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT "+
			"CURRENT_TIMESTAMP", string(content),
	)
}

// TestMigrationSetValidate verifies that migration descriptors remain aligned
// with the migration stream metadata.
func TestMigrationSetValidate(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		set    MigrationSet
		expect string
	}{
		{
			name: "valid descriptors",
			set: MigrationSet{
				LatestMigrationVersion: 2,
				Descriptors: []MigrationDescriptor{
					{Version: 1},
					{Version: 2},
				},
			},
		},
		{
			name: "descriptor gap",
			set: MigrationSet{
				LatestMigrationVersion: 2,
				Descriptors: []MigrationDescriptor{
					{Version: 1},
					{Version: 3},
				},
			},
			expect: "out of order",
		},
		{
			name: "missing descriptors for latest version",
			set: MigrationSet{
				LatestMigrationVersion: 1,
			},
			expect: "requires at least one descriptor",
		},
		{
			name: "latest version mismatch",
			set: MigrationSet{
				LatestMigrationVersion: 3,
				Descriptors: []MigrationDescriptor{
					{Version: 1},
					{Version: 2},
				},
			},
			expect: "does not match",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := testCase.set.validate()
			if testCase.expect == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, testCase.expect)
		})
	}
}

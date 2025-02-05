package lnutils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCreateDir verifies the behavior of CreateDir function in various
// scenarios:
// - Creating a new directory when it doesn't exist
// - Handling an already existing directory
// - Dealing with symlinks pointing to non-existent directories
// - Handling invalid paths
// The test uses a temporary directory and runs multiple test cases to ensure
// proper directory creation, permission settings, and error handling.
func TestCreateDir(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()

	tests := []struct {
		name      string
		setup     func() string
		wantError bool
	}{
		{
			name: "create directory",
			setup: func() string {
				return filepath.Join(tempDir, "testdir")
			},
			wantError: false,
		},
		{
			name: "existing directory",
			setup: func() string {
				dir := filepath.Join(tempDir, "testdir")
				err := os.Mkdir(dir, 0700)
				require.NoError(t, err)

				return dir
			},
			wantError: false,
		},
		{
			name: "symlink to non-existent directory",
			setup: func() string {
				dir := filepath.Join(tempDir, "testdir")
				symlink := filepath.Join(tempDir, "symlink")
				err := os.Symlink(dir, symlink)
				require.NoError(t, err)

				return symlink
			},
			wantError: true,
		},
		{
			name: "invalid path",
			setup: func() string {
				return string([]byte{0})
			},
			wantError: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup()
			defer os.RemoveAll(dir)

			err := CreateDir(dir, 0700)
			if tc.wantError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			info, err := os.Stat(dir)
			require.NoError(t, err)
			require.True(t, info.IsDir())
		})
	}
}

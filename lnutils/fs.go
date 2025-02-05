package lnutils

import (
	"errors"
	"fmt"
	"os"
)

// CreateDir creates a directory if it doesn't exist and also handles
// symlink-related errors with user-friendly messages. It creates all necessary
// parent directories with the specified permissions.
func CreateDir(dir string, perm os.FileMode) error {
	err := os.MkdirAll(dir, perm)
	if err == nil {
		return nil
	}

	// Show a nicer error message if it's because a symlink
	// is linked to a directory that does not exist
	// (probably because it's not mounted).
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && os.IsExist(err) {
		link, lerr := os.Readlink(pathErr.Path)
		if lerr == nil {
			return fmt.Errorf("is symlink %s -> %s "+
				"mounted?", pathErr.Path, link)
		}
	}

	return fmt.Errorf("failed to create directory '%s': %w", dir, err)
}

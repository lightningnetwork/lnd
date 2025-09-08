package healthcheck

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// CheckFileDescriptors checks if there are any free file descriptors available.
// It returns an error if no free file descriptors are available or if an unexpected error occurs.
func CheckFileDescriptors() error {
	// Attempt to open /dev/null to test for available file descriptors
	fd, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		// Check if the error is due to "too many open files"
		if errors.Is(err, syscall.EMFILE) {
			return fmt.Errorf("no free file descriptors available")
		}

		return fmt.Errorf("error checking file descriptors: %w", err)
	}

	fd.Close()
	return nil
}

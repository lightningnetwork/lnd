package fn

import (
	"os"
)

// WriteFile synchronously writes data to the named file.
// If the file does not exist, WriteFile creates it with permissions perm
// (before umask); otherwise WriteFile truncates it before writing, without
// changing permissions.
// By opening the file with O_SYNC, it ensures the data is written to disk.
// If an error occurs, it does not remove the file.
func WriteFile(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(
		name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_SYNC, perm,
	)
	if err != nil {
		return err
	}

	_, err = f.Write(data)

	// Prioritize the error on Write but make sure to call Close regardless
	// to avoid leaking a file handle.
	if err1 := f.Close(); err1 != nil && err == nil {
		err = err1
	}

	return err
}

// WriteFileRemove synchronously writes data to the named file.
// If the file does not exist, WriteFileRemove creates it with permissions perm
// (before umask); otherwise WriteFileRemove truncates it before writing,
// without changing permissions.
// By opening the file with O_SYNC, it ensures the data is written to disk.
// If an error occurs, it removes the file.
func WriteFileRemove(name string, data []byte, perm os.FileMode) error {
	err := WriteFile(name, data, perm)
	if err != nil {
		_ = os.Remove(name)
	}

	return err
}

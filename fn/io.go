package fn

import (
	"os"
)

// FileRemovalOnErr is a flag used to indicate the removal or not of a file on a
// failure.
type FileRemovalOnErr bool

const (
	// KeepFileOnErr indicates no deletion of the file on error.
	KeepFileOnErr FileRemovalOnErr = false

	// RemoveFileOnErr indicates deletion of the file on error.
	RemoveFileOnErr FileRemovalOnErr = true
)

// WriteFile synchronous writes data to the named file.
// If the file does not exist, WriteFile creates it with permissions perm
// (before umask); otherwise WriteFile truncates it before writing, without
// changing permissions.
// By opening the file with O_SYNC, it ensures the data is written to disk.
// The removeOnErr flag controls the removal or not of the file on error.
func WriteFile(name string, data []byte, perm os.FileMode,
	removeOnErr FileRemovalOnErr) (err error) {

	defer func() {
		if err != nil && removeOnErr == RemoveFileOnErr {
			_ = os.Remove(name)
		}
	}()

	var f *os.File
	f, err = os.OpenFile(
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

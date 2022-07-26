package io

import (
	"io/fs"
	"os"
)

// WriteFileToDisk writes data to specified file with perm file mode.
// If file exists trucates it and forces new permissions; otherwise creates it with specified permission
// This method uses same parameters as ioutil.WriteFile but persists the newly created file on storage
// using f.sync.
func WriteFileToDisk(file string, fileBytes []byte, perm fs.FileMode) error {
	f, err := createFileForWrite(file, perm)
	if err != nil {
		return err
	}
	return writeSyncClose(f, fileBytes)
}

// WriteFileTransactional does a write with sync like WriteFileToDisk
// Additionally, if there is an error after opening,
// it attempts to remove the file in question.
func WriteFileTransactional(file string, fileBytes []byte, perm fs.FileMode) error {
	f, err := createFileForWrite(file, perm)
	if err != nil {
		return err
	}
	err = writeSyncClose(f, fileBytes)
	if err != nil {
		_ = os.Remove(file)
	}
	return err
}

// WriteRemoveOnError is the same as WriteFileTransactional
// It will remove the file if there is an error on write
// However it takes an open handle as an argument instead of a file:
// this is at least useful for testing purposes.
func WriteRemoveOnError(f *os.File, fileBytes []byte) error {
	err := writeSyncClose(f, fileBytes)
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func writeSyncClose(f *os.File, fileBytes []byte) error {
	_, err := f.Write(fileBytes)
	if err2 := f.Sync(); err2 != nil && err == nil {
		err = err2
	}
	if err2 := f.Close(); err2 != nil && err == nil {
		err = err2
	}
	return err
}

func createFileForWrite(file string, perm fs.FileMode) (*os.File, error) {
	return os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
}

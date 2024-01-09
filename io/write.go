package io

import (
	"io/fs"
	"os"
)

type WriteRemovalBehaviour bool

const (
	Default         WriteRemovalBehaviour = false
	RemoveOnFailure WriteRemovalBehaviour = true
)

// WriteFileToDisk ensures the data is written to disk.
// This is done by opening the file with O_SYNC and closing it after write.
// It also opens the file with O_WRONLY, O_CREATE, O_TRUNC, and the given
// permissions this method uses same parameters as ioutil.WriteFile.
func WriteFileToDisk(
	file string, fileBytes []byte, perm fs.FileMode,
	removeOnFailure WriteRemovalBehaviour,
) error {

	f, err := os.OpenFile(
		file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_SYNC, perm,
	)
	if err != nil {
		return err
	}
	err = writeClose(f, fileBytes)
	if err != nil && removeOnFailure {
		_ = os.Remove(file)
	}

	return err
}

func writeClose(f *os.File, fileBytes []byte) error {
	_, err := f.Write(fileBytes)
	// prioritize the error on Write because it happens first
	// But make sure to call Close regardless to avoid leaking a file handle
	err2 := f.Close()
	if err != nil {
		return err
	}

	return err2
}

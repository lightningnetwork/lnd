package io

import (
	"io/fs"
	"os"
)

// WriteFileToDisk ensures the data is written to disk.
// This is done by opening the file with O_SYNC and closing it after write.
// It also opens the file with O_WRONLY, O_CREATE, O_TRUNC, and the given permissions
// This method uses same parameters as ioutil.WriteFile.
func WriteFileToDisk(file string, fileBytes []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_SYNC, perm)
	if err != nil {
		return err
	}
	return writeClose(f, fileBytes)
}

// WriteFileTransactional first writes to file.tmp
// Once the write is successful, it will rename it to file.
// This avoids the possiblity of ending up with an empty file.
// Additionally, if there is an error after opening,
// it attempts to remove the file in question.
func WriteFileTransactional(file string, fileBytes []byte, perm fs.FileMode) error {
	tmp_name := file + ".tmp"
	// Ignore any errors during file removal
	// since we don't really care about the .tmp file
	defer os.Remove(tmp_name)
	if err := WriteFileToDisk(tmp_name, fileBytes, perm); err != nil {
		return err
	}
	err := os.Rename(tmp_name, file)
	if err != nil {
		// Ignore this error and return the prior error
		// This may be unecessary but shouldn't cause a problem
		_ = os.Remove(file)
	}
	return err
}

func writeClose(f *os.File, fileBytes []byte) error {
	_, err := f.Write(fileBytes)
	if err2 := f.Close(); err2 != nil && err == nil {
		err = err2
	}
	return err
}

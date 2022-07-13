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
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(fileBytes)
	if err1 := f.Close(); err1 != nil && err == nil {
		err = err1
	}

	if err2 := f.Sync(); err2 != nil && err == nil {
		err = err2
	}

	return err
}

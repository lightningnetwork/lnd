package chanbackup

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// DefaultBackupFileName is the default name of the auto updated static
	// channel backup fie.
	DefaultBackupFileName = "channel.backup"

	// DefaultTempBackupFileName is the default name of the temporary SCB
	// file that we'll use to atomically update the primary back up file
	// when new channel are detected.
	DefaultTempBackupFileName = "temp-dont-use.backup"
)

var (
	// ErrNoBackupFileExists is returned if caller attempts to call
	// UpdateAndSwap with the file name not set.
	ErrNoBackupFileExists = fmt.Errorf("back up file name not set")

	// ErrNoTempBackupFile is returned if caller attempts to call
	// UpdateAndSwap with the temp back up file name not set.
	ErrNoTempBackupFile = fmt.Errorf("temp backup file not set")
)

// MultiFile represents a file on disk that a caller can use to read the packed
// multi backup into an unpacked one, and also atomically update the contents
// on disk once new channels have been opened, and old ones closed. This struct
// relies on an atomic file rename property which most widely use file systems
// have.
type MultiFile struct {
	// fileName is the file name of the main back up file.
	fileName string

	// mainFile is an open handle to the main back up file.
	mainFile *os.File

	// tempFileName is the name of the file that we'll use to stage a new
	// packed multi-chan backup, and the rename to the main back up file.
	tempFileName string

	// tempFile is an open handle to the temp back up file.
	tempFile *os.File
}

// NewMultiFile create a new multi-file instance at the target location on the
// file system.
func NewMultiFile(fileName string) *MultiFile {

	// We'll our temporary backup file in the very same directory as the
	// main backup file.
	backupFileDir := filepath.Dir(fileName)
	tempFileName := filepath.Join(
		backupFileDir, DefaultTempBackupFileName,
	)

	return &MultiFile{
		fileName:     fileName,
		tempFileName: tempFileName,
	}
}

// UpdateAndSwap will attempt write a new temporary backup file to disk with
// the newBackup encoded, then atomically swap (via rename) the old file for
// the new file by updating the name of the new file to the old.
func (b *MultiFile) UpdateAndSwap(newBackup PackedMulti) error {
	// If the main backup file isn't set, then we can't proceed.
	if b.fileName == "" {
		return ErrNoBackupFileExists
	}

	log.Infof("Updating backup file at %v", b.fileName)

	// If the old back up file still exists, then we'll delete it before
	// proceeding.
	if _, err := os.Stat(b.tempFileName); err == nil {
		log.Infof("Found old temp backup @ %v, removing before swap",
			b.tempFileName)

		err = os.Remove(b.tempFileName)
		if err != nil {
			return fmt.Errorf("unable to remove temp "+
				"backup file: %v", err)
		}
	}

	// Now that we know the staging area is clear, we'll create the new
	// temporary back up file.
	var err error
	b.tempFile, err = os.Create(b.tempFileName)
	if err != nil {
		return err
	}

	// With the file created, we'll write the new packed multi backup and
	// remove the temporary file all together once this method exits.
	_, err = b.tempFile.Write([]byte(newBackup))
	if err != nil {
		return err
	}
	if err := b.tempFile.Sync(); err != nil {
		return err
	}
	defer os.Remove(b.tempFileName)

	log.Infof("Swapping old multi backup file from %v to %v",
		b.tempFileName, b.fileName)

	// Finally, we'll attempt to atomically rename the temporary file to
	// the main back up file. If this succeeds, then we'll only have a
	// single file on disk once this method exits.
	return os.Rename(b.tempFileName, b.fileName)
}

// ExtractMulti attempts to extract the packed multi backup we currently point
// to into an unpacked version. This method will fail if no backup file
// currently exists as the specified location.
func (b *MultiFile) ExtractMulti(keyChain keychain.KeyRing) (*Multi, error) {
	var err error

	// If the backup file isn't already set, then we'll attempt to open it
	// anew.
	if b.mainFile == nil {
		// We'll return an error if the main file isn't currently set.
		if b.fileName == "" {
			return nil, ErrNoBackupFileExists
		}

		// Otherwise, we'll open the file to prep for reading the
		// contents.
		b.mainFile, err = os.Open(b.fileName)
		if err != nil {
			return nil, err
		}
	}

	// Before we start to read the file, we'll ensure that the next read
	// call will start from the front of the file.
	_, err = b.mainFile.Seek(0, 0)
	if err != nil {
		return nil, err
	}

	// With our seek successful, we'll now attempt to read the contents of
	// the entire file in one swoop.
	multiBytes, err := ioutil.ReadAll(b.mainFile)
	if err != nil {
		return nil, err
	}

	// Finally, we'll attempt to unpack the file and return the unpack
	// version to the caller.
	packedMulti := PackedMulti(multiBytes)
	return packedMulti.Unpack(keyChain)
}

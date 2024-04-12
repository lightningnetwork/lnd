package chanbackup

import (
	"fmt"
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
		return fmt.Errorf("unable to create temp file: %w", err)
	}

	// With the file created, we'll write the new packed multi backup and
	// remove the temporary file all together once this method exits.
	_, err = b.tempFile.Write([]byte(newBackup))
	if err != nil {
		return fmt.Errorf("unable to write backup to temp file: %w",
			err)
	}
	if err := b.tempFile.Sync(); err != nil {
		return fmt.Errorf("unable to sync temp file: %w", err)
	}
	defer os.Remove(b.tempFileName)

	log.Infof("Swapping old multi backup file from %v to %v",
		b.tempFileName, b.fileName)

	// Before we rename the swap (atomic name swap), we'll make
	// sure to close the current file as some OSes don't support
	// renaming a file that's already open (Windows).
	if err := b.tempFile.Close(); err != nil {
		return fmt.Errorf("unable to close file: %w", err)
	}

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

	// We'll return an error if the main file isn't currently set.
	if b.fileName == "" {
		return nil, ErrNoBackupFileExists
	}

	// Now that we've confirmed the target file is populated, we'll read
	// all the contents of the file. This function ensures that file is
	// always closed, even if we can't read the contents.
	multiBytes, err := os.ReadFile(b.fileName)
	if err != nil {
		return nil, err
	}

	// Finally, we'll attempt to unpack the file and return the unpack
	// version to the caller.
	packedMulti := PackedMulti(multiBytes)
	return packedMulti.Unpack(keyChain)
}

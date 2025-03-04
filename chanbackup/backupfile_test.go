package chanbackup

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/stretchr/testify/require"
)

func makeFakePackedMulti() (PackedMulti, error) {
	newPackedMulti := make([]byte, 50)
	if _, err := rand.Read(newPackedMulti[:]); err != nil {
		return nil, fmt.Errorf("unable to make test backup: %w", err)
	}

	return PackedMulti(newPackedMulti), nil
}

func assertBackupMatches(t *testing.T, filePath string,
	currentBackup PackedMulti) {

	t.Helper()

	packedBackup, err := os.ReadFile(filePath)
	require.NoError(t, err, "unable to test file")

	if !bytes.Equal(packedBackup, currentBackup) {
		t.Fatalf("backups don't match after first swap: "+
			"expected %x got %x", packedBackup[:],
			currentBackup)
	}
}

func assertFileDeleted(t *testing.T, filePath string) {
	t.Helper()

	_, err := os.Stat(filePath)
	if err == nil {
		t.Fatalf("file %v still exists: ", filePath)
	}
}

// TestUpdateAndSwapWithArchive test that we're able to properly swap out old
// backups on disk with new ones. In addition, we check for noBackupArchive to
// ensure that the archive file is created when it's set to false, and not
// created when it's set to true.
func TestUpdateAndSwapWithArchive(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		noBackupArchive bool
	}{
		// Test with noBackupArchive set to true - should not create
		// archive.
		{
			name:            "no archive file",
			noBackupArchive: true,
		},

		// Test with noBackupArchive set to false - should create
		// archive.
		{
			name:            "with archive file",
			noBackupArchive: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tempTestDir := t.TempDir()

			fileName := filepath.Join(
				tempTestDir, DefaultBackupFileName,
			)
			tempFileName := filepath.Join(
				tempTestDir, DefaultTempBackupFileName,
			)

			backupFile := NewMultiFile(fileName, tc.noBackupArchive)

			// To start with, we'll make a random byte slice that'll
			// pose as our packed multi backup.
			newPackedMulti, err := makeFakePackedMulti()
			require.NoError(t, err)

			// With our backup created, we'll now attempt to swap
			// out this backup, for the old one.
			err = backupFile.UpdateAndSwap(newPackedMulti)
			require.NoError(t, err)

			// If we read out the file on disk, then it should match
			// exactly what we wrote. The temp backup file should
			// also be gone.
			assertBackupMatches(t, fileName, newPackedMulti)
			assertFileDeleted(t, tempFileName)

			// Now that we know this is a valid test case, we'll
			// make a new packed multi to swap out this current one.
			newPackedMulti2, err := makeFakePackedMulti()
			require.NoError(t, err)

			// We'll then attempt to swap the old version for this
			// new one.
			err = backupFile.UpdateAndSwap(newPackedMulti2)
			require.NoError(t, err)

			// Once again, the file written on disk should have been
			// properly swapped out with the new instance.
			assertBackupMatches(t, fileName, newPackedMulti2)

			// Additionally, we shouldn't be able to find the temp
			// backup file on disk, as it should be deleted each
			// time.
			assertFileDeleted(t, tempFileName)

			// Now check if archive was created when noBackupArchive
			// is false.
			archiveDir := filepath.Join(
				filepath.Dir(fileName),
				DefaultChanBackupArchiveDirName,
			)

			// When noBackupArchive is true, no new archive file
			// should be created.
			//
			// NOTE: In a real environment, the archive directory
			// might exist with older backups before the feature is
			// disabled, but for test simplicity (since we clean up
			// the directory between test cases), we verify the
			// directory doesn't exist at all.
			if tc.noBackupArchive {
				require.NoDirExists(t, archiveDir)
				return
			}

			// Otherwise we expect an archive to be created.
			files, err := os.ReadDir(archiveDir)
			require.NoError(t, err)
			require.Len(t, files, 1)

			// Verify the archive contents match the previous
			// backup.
			archiveFile := filepath.Join(
				archiveDir, files[0].Name(),
			)

			// The archived content should match the previous backup
			// (newPackedMulti) that was just swapped out.
			assertBackupMatches(t, archiveFile, newPackedMulti)

			// Clean up the archive directory.
			os.RemoveAll(archiveDir)
		})
	}
}

// TestUpdateAndSwap test that we're able to properly swap out old backups on
// disk with new ones. Additionally, after a swap operation succeeds, then each
// time we should only have the main backup file on disk, as the temporary file
// has been removed.
func TestUpdateAndSwap(t *testing.T) {
	t.Parallel()

	// Check that when the main file name is blank, an error is returned.
	backupFile := NewMultiFile("", false)

	err := backupFile.UpdateAndSwap(PackedMulti(nil))
	require.ErrorIs(t, err, ErrNoBackupFileExists)

	testCases := []struct {
		name          string
		oldTempExists bool
	}{
		// Old temporary file still exists, should be removed. Only one
		// file should remain.
		{
			name:          "remove old temp file",
			oldTempExists: true,
		},

		// Old temp doesn't exist, should swap out file, only a single
		// file remains.
		{
			name:          "swap out file",
			oldTempExists: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tempTestDir := t.TempDir()

			fileName := filepath.Join(
				tempTestDir, DefaultBackupFileName,
			)
			tempFileName := filepath.Join(
				tempTestDir, DefaultTempBackupFileName,
			)

			backupFile := NewMultiFile(fileName, false)

			// To start with, we'll make a random byte slice that'll
			// pose as our packed multi backup.
			newPackedMulti, err := makeFakePackedMulti()
			require.NoError(t, err)

			// If the old temporary file is meant to exist, then
			// we'll create it now as an empty file.
			if tc.oldTempExists {
				f, err := os.Create(tempFileName)
				require.NoError(t, err)
				require.NoError(t, f.Close())

				// TODO(roasbeef): mock out fs calls?
			}

			// With our backup created, we'll now attempt to swap
			// out this backup, for the old one.
			err = backupFile.UpdateAndSwap(newPackedMulti)
			require.NoError(t, err)

			// If we read out the file on disk, then it should match
			// exactly what we wrote. The temp backup file should
			// also be gone.
			assertBackupMatches(t, fileName, newPackedMulti)
			assertFileDeleted(t, tempFileName)

			// Now that we know this is a valid test case, we'll
			// make a new packed multi to swap out this current one.
			newPackedMulti2, err := makeFakePackedMulti()
			require.NoError(t, err)

			// We'll then attempt to swap the old version for this
			// new one.
			err = backupFile.UpdateAndSwap(newPackedMulti2)
			require.NoError(t, err)

			// Once again, the file written on disk should have been
			// properly swapped out with the new instance.
			assertBackupMatches(t, fileName, newPackedMulti2)

			// Additionally, we shouldn't be able to find the temp
			// backup file on disk, as it should be deleted each
			// time.
			assertFileDeleted(t, tempFileName)

			// Now check if archive was created when noBackupArchive
			// is false.
			archiveDir := filepath.Join(
				filepath.Dir(fileName),
				DefaultChanBackupArchiveDirName,
			)
			files, err := os.ReadDir(archiveDir)
			require.NoError(t, err)
			require.Len(t, files, 1)

			// Verify the archive contents match the previous
			// backup.
			archiveFile := filepath.Join(
				archiveDir, files[0].Name(),
			)

			// The archived content should match the previous backup
			// (newPackedMulti) that was just swapped out.
			assertBackupMatches(t, archiveFile, newPackedMulti)

			// Clean up the archive directory.
			os.RemoveAll(archiveDir)
		})
	}
}

func assertMultiEqual(t *testing.T, a, b *Multi) {

	if len(a.StaticBackups) != len(b.StaticBackups) {
		t.Fatalf("expected %v backups, got %v", len(a.StaticBackups),
			len(b.StaticBackups))
	}

	for i := 0; i < len(a.StaticBackups); i++ {
		assertSingleEqual(t, a.StaticBackups[i], b.StaticBackups[i])
	}
}

// TestExtractMulti tests that given a valid packed multi file on disk, we're
// able to read it multiple times repeatedly.
func TestExtractMulti(t *testing.T) {
	t.Parallel()

	keyRing := &lnencrypt.MockKeyRing{}

	// First, as prep, we'll create a single chan backup, then pack that
	// fully into a multi backup.
	channel, err := genRandomOpenChannelShell()
	require.NoError(t, err, "unable to gen chan")

	singleBackup := NewSingle(channel, nil)

	var b bytes.Buffer
	unpackedMulti := Multi{
		StaticBackups: []Single{singleBackup},
	}
	err = unpackedMulti.PackToWriter(&b, keyRing)
	require.NoError(t, err, "unable to pack to writer")

	packedMulti := PackedMulti(b.Bytes())

	// Finally, we'll make a new temporary file, then write out the packed
	// multi directly to it.
	tempFile, err := os.CreateTemp("", "")
	require.NoError(t, err, "unable to create temp file")
	t.Cleanup(func() {
		require.NoError(t, tempFile.Close())
		require.NoError(t, os.Remove(tempFile.Name()))
	})

	_, err = tempFile.Write(packedMulti)
	require.NoError(t, err, "unable to write temp file")
	if err := tempFile.Sync(); err != nil {
		t.Fatalf("unable to sync temp file: %v", err)
	}

	testCases := []struct {
		fileName string
		pass     bool
	}{
		// Main file not read, file name not present.
		{
			fileName: "",
			pass:     false,
		},

		// Main file not read, file name is there, but file doesn't
		// exist.
		{
			fileName: "kek",
			pass:     false,
		},

		// Main file not read, should be able to read multiple times.
		{
			fileName: tempFile.Name(),
			pass:     true,
		},
	}
	for i, testCase := range testCases {
		// First, we'll make our backup file with the specified name.
		backupFile := NewMultiFile(testCase.fileName, false)

		// With our file made, we'll now attempt to read out the
		// multi-file.
		freshUnpackedMulti, err := backupFile.ExtractMulti(keyRing)
		switch {
		// If this is a valid test case, and we failed, then we'll
		// return an error.
		case err != nil && testCase.pass:
			t.Fatalf("#%v, unable to extract file: %v", i, err)

			// If this is an invalid test case, and we passed it, then
			// we'll return an error.
		case err == nil && !testCase.pass:
			t.Fatalf("#%v file extraction should have "+
				"failed: %v", i, err)
		}

		if !testCase.pass {
			continue
		}

		// We'll now ensure that the unpacked multi we read is
		// identical to the one we wrote out above.
		assertMultiEqual(t, &unpackedMulti, freshUnpackedMulti)

		// We should also be able to read the file again, as we have an
		// existing handle to it.
		freshUnpackedMulti, err = backupFile.ExtractMulti(keyRing)
		if err != nil {
			t.Fatalf("unable to unpack multi: %v", err)
		}

		assertMultiEqual(t, &unpackedMulti, freshUnpackedMulti)
	}
}

// TestCreateArchiveFile tests that we're able to create an archive file
// with a timestamped name in the specified archive directory, and copy the
// contents of the main backup file to the new archive file.
func TestCreateArchiveFile(t *testing.T) {
	t.Parallel()

	// First, we'll create a temporary directory for our test files.
	tempDir := t.TempDir()
	archiveDir := filepath.Join(tempDir, DefaultChanBackupArchiveDirName)

	// Next, we'll create a test backup file and write some content to it.
	backupFile := filepath.Join(tempDir, DefaultBackupFileName)
	testContent := []byte("test backup content")
	err := os.WriteFile(backupFile, testContent, 0644)
	require.NoError(t, err)

	tests := []struct {
		name            string
		setup           func()
		noBackupArchive bool
		wantError       bool
	}{
		{
			name:            "successful archive",
			noBackupArchive: false,
		},
		{
			name:            "skip archive when disabled",
			noBackupArchive: true,
		},
		{
			name: "invalid archive directory permissions",
			setup: func() {
				// Create dir with no write permissions.
				err := os.MkdirAll(archiveDir, 0500)
				require.NoError(t, err)
			},
			noBackupArchive: false,
			wantError:       true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer os.RemoveAll(archiveDir)
			if tc.setup != nil {
				tc.setup()
			}

			multiFile := NewMultiFile(
				backupFile, tc.noBackupArchive,
			)

			err := multiFile.createArchiveFile()
			if tc.wantError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			// If archiving is disabled, verify no archive was
			// created.
			if tc.noBackupArchive {
				require.NoDirExists(t, archiveDir)
				return
			}

			// Verify archive exists and content matches.
			files, err := os.ReadDir(archiveDir)
			require.NoError(t, err)
			require.Len(t, files, 1)

			archivedContent, err := os.ReadFile(
				filepath.Join(archiveDir, files[0].Name()),
			)
			require.NoError(t, err)
			assertBackupMatches(t, backupFile, archivedContent)
		})
	}
}

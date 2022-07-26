package io_test

import (
	"errors"
	"io/ioutil"
	"os"
	"testing"

	"github.com/lightningnetwork/lnd/io"
)

var (
	data1 = "The network is robust in its unstructured simplicity."
	data2 = "Programming today is a race between software engineers striving to " +
		"build bigger and better idiot-proof programs, and the Universe trying " +
		"to produce bigger and better idiots. So far, the Universe is winning."
)

// TestWriteFileToDisk uses same scenario of ioutil asserting the content created and stored on new
// file with the original one.
func TestWriteFileToDisk(t *testing.T) {
	f, deferred := ensureTempfile(t)
	filename := f.Name()
	defer deferred()

	if err := io.WriteFileToDisk(filename, []byte(data1), 0644); err != nil {
		t.Fatalf("WriteFile %s: %v", filename, err)
	}

	ensureFileContents(t, filename, data1)

	// Changing the permission to read-only will cause the write to fail
	if err := os.Chmod(filename, 0444); err != nil {
		t.Fatalf("Error changing to read-only %s: %v", filename, err)
	}
	if err := io.WriteFileToDisk(filename, []byte(data2), 0644); err == nil {
		t.Fatalf("WriteFileToDisk expected permission error %s", filename)
	}
	_, err := os.Stat(filename)
	if err != nil {
		t.Fatalf("WriteFileToDisk %s: %v", filename, err)
	}
	ensureFileContents(t, filename, data1)
}

func TestWriteFileTransactional(t *testing.T) {
	f, deferred := ensureTempfile(t)
	filename := f.Name()
	defer deferred()
	if err := io.WriteFileTransactional(filename, []byte(data1), 0644); err != nil {
		t.Fatalf("WriteFileTransactional %s: %v", filename, err)
	}

	ensureFileContents(t, filename, data1)

	// Changing the permission to read-only will cause the write to fail
	if err := os.Chmod(filename, 0444); err != nil {
		t.Fatalf("Error changing to read-only %s: %v", filename, err)
	}

	if err := io.WriteFileTransactional(filename, []byte(data2), 0644); err == nil {
		t.Fatalf("WriteFileTransactional expected permission error %s", filename)
	}
	// Because the permission error occurs on file open, the file is not removed
	_, err := os.Stat(filename)
	if err != nil {
		t.Fatalf("WriteFileTransactional %s: %v", filename, err)
	}
	ensureFileContents(t, filename, data1)
}

func TestWriteRemoveOnError(t *testing.T) {
	f, deferred := ensureTempfile(t)
	filename := f.Name()
	defer deferred()

	if err := io.WriteRemoveOnError(f, []byte(data1)); err != nil {
		t.Fatalf("WriteRemoveOnError %s: %v", filename, err)
	}

	ensureFileContents(t, filename, data1)

	// Changing the permission to read-only will cause the write to fail
	if err := os.Chmod(filename, 0444); err != nil {
		t.Fatalf("Error changing to read-only %s: %v", filename, err)
	}

	if err := io.WriteRemoveOnError(f, []byte(data2)); err == nil {
		t.Fatalf("WriteRemoveOnError expected permission error %s", filename)
	}

	// The file should be removed on failure
	_, err := os.Stat(filename)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("WriteRemoveOnError %s: %v", filename, err)
	}
}

func ensureTempfile(t *testing.T) (*os.File, func()) {
	t.Helper()
	f, err := ioutil.TempFile("", "io-test-TestWriteFileToDisk")
	if err != nil {
		t.Fatal(err)
	}
	return f, func() {
		f.Close()
		os.Remove(f.Name())
	}

}

func ensureFileContents(t *testing.T, filename string, data string) {
	t.Helper()
	contents, err := ioutil.ReadFile(filename)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", filename, err)
	}

	if string(contents) != data {
		t.Fatalf("contents = %q\nexpected = %q", string(contents), data)
	}
}

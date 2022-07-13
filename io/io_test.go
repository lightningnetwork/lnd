package io_test

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/lightningnetwork/lnd/io"
)

// TestWriteFileToDisk uses same scenario of ioutil asserting the content created and stored on new
// file with the original one.
func TestWriteFileToDisk(t *testing.T) {
	f, err := ioutil.TempFile("", "io-test")
	if err != nil {
		t.Fatal(err)
	}
	filename := f.Name()
	data := "Programming today is a race between software engineers striving to " +
		"build bigger and better idiot-proof programs, and the Universe trying " +
		"to produce bigger and better idiots. So far, the Universe is winning."

	if err := io.WriteFileToDisk(filename, []byte(data), 0644); err != nil {
		t.Fatalf("WriteFile %s: %v", filename, err)
	}

	contents, err := ioutil.ReadFile(filename)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", filename, err)
	}

	if string(contents) != data {
		t.Fatalf("contents = %q\nexpected = %q", string(contents), data)
	}

	f.Close()
	os.Remove(filename)
}

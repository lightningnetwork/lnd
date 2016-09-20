package sphinx

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestLionnessCorrectness(t *testing.T) {
	var m [messageSize]byte
	msg := []byte("hello")
	copy(m[:], msg)

	var key [securityParameter]byte
	rand.Read(key[:])

	cipherText := lionessEncode(key, m)
	plainText := lionessDecode(key, cipherText)

	if !bytes.Equal(m[:], plainText[:]) {
		t.Fatalf("texts not equal")
	}
}

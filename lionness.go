package sphinx

import (
	"crypto/hmac"
	"crypto/sha256"
)

// lionEncode encrypts a message of fixed size using the LIONESS block cipher.
// The LIONESS block cipher is unique due to its variable block size. Within our
// application we set the block size equivalent to our fixed message size.
// Details concerning the rationale, security model and internals of the
// LIONESS block cipher can be found here: http://www.cl.cam.ac.uk/~rja14/Papers/bear-lion.pdf (section 6)
func lionessEncode(key [securityParameter]byte, message [messageSize]byte) [messageSize]byte {
	var cipherText [messageSize]byte
	copy(cipherText[:], message[:])

	L := cipherText[:securityParameter]
	R := cipherText[securityParameter:]

	// Round 1.
	// L = L XOR H_k1(R)
	h := hmac.New(sha256.New, append(key[:], 0x01))
	h.Write(R)
	xor(L[:], h.Sum(nil)[:securityParameter], L[:])

	// Round 2.
	// R = R XOR S(L XOR K_2)
	var k2 [securityParameter]byte
	xor(k2[:], L[:], key[:])
	xor(R[:], R[:], generateCipherStream(k2, uint(len(R))))

	// Round 3.
	// L = L XOR H_k3(R)
	h = hmac.New(sha256.New, append(key[:], 0x03))
	h.Write(R)
	xor(L[:], h.Sum(nil)[:securityParameter], L[:])

	// Round 4.
	// R = R XOR S(L XOR K_4)
	var k4 [securityParameter]byte
	xor(k4[:], L[:], key[:])
	xor(R[:], R[:], generateCipherStream(k4, uint(len(R))))

	return cipherText
}

// lionDecode performs the inverse operation of lionessEncode. Namely,
// decrypting a previously generated cipher text, returning the original plaintext.
func lionessDecode(key [securityParameter]byte, cipherText [messageSize]byte) [messageSize]byte {
	var message [messageSize]byte
	copy(message[:], cipherText[:])

	L := message[:securityParameter]
	R := message[securityParameter:]

	// Round 4.
	// R = R XOR S(L XOR K_4)
	var k4 [securityParameter]byte
	xor(k4[:], L[:], key[:])
	xor(R[:], R[:], generateCipherStream(k4, uint(len(R))))

	// Round 3.
	// L = L XOR H_k3(R)
	h := hmac.New(sha256.New, append(key[:], 0x03))
	h.Write(R)
	xor(L[:], h.Sum(nil)[:securityParameter], L[:])

	// Round 2.
	// R = R XOR S(L XOR K_2)
	var k2 [securityParameter]byte
	xor(k2[:], L[:], key[:])
	xor(R[:], R[:], generateCipherStream(k2, uint(len(R))))

	// Round 1.
	// L = L XOR H_k1(R)
	h = hmac.New(sha256.New, append(key[:], 0x01))
	h.Write(R)
	xor(L[:], h.Sum(nil)[:securityParameter], L[:])

	return message
}

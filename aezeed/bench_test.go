package aezeed

import (
	"testing"
	"time"
)

var (
	mnemonic Mnemonic

	seed *CipherSeed
)

// BenchmarkFrommnemonic benchmarks the process of converting a cipher seed
// (given the salt), to an enciphered mnemonic.
func BenchmarkTomnemonic(b *testing.B) {
	scryptN = 32768
	scryptR = 8
	scryptP = 1

	pass := []byte("1234567890abcedfgh")
	cipherSeed, err := New(0, nil, time.Now())
	if err != nil {
		b.Fatalf("unable to create seed: %v", err)
	}

	var r Mnemonic
	for i := 0; i < b.N; i++ {
		r, err = cipherSeed.ToMnemonic(pass)
		if err != nil {
			b.Fatalf("unable to encipher: %v", err)
		}
	}

	b.ReportAllocs()

	mnemonic = r
}

// BenchmarkToCipherSeed benchmarks the process of deciphering an existing
// enciphered mnemonic.
func BenchmarkToCipherSeed(b *testing.B) {
	scryptN = 32768
	scryptR = 8
	scryptP = 1

	pass := []byte("1234567890abcedfgh")
	cipherSeed, err := New(0, nil, time.Now())
	if err != nil {
		b.Fatalf("unable to create seed: %v", err)
	}

	mnemonic, err := cipherSeed.ToMnemonic(pass)
	if err != nil {
		b.Fatalf("unable to create mnemonic: %v", err)
	}

	var s *CipherSeed
	for i := 0; i < b.N; i++ {
		s, err = mnemonic.ToCipherSeed(pass)
		if err != nil {
			b.Fatalf("unable to decipher: %v", err)
		}
	}

	b.ReportAllocs()

	seed = s
}

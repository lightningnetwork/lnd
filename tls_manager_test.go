package lnd

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/stretchr/testify/require"
)

const (
	testTLSCertDuration = 42 * time.Hour
)

var (
	privKeyBytes = channels.AlicesPrivKey

	privKey, _ = btcec.PrivKeyFromBytes(privKeyBytes)
)

// TestGenerateOrRenewCert creates an expired TLS certificate, to test that a
// new TLS certificate pair is regenerated when the old pair expires. This is
// necessary because the pair expires after a little over a year.
func TestGenerateOrRenewCert(t *testing.T) {
	t.Parallel()

	// Write an expired certificate to disk.
	certPath, keyPath, expiredCert := writeTestCertFiles(
		t, true, false, nil,
	)

	// Now let's run the TLSManager's getConfig. If it works properly, it
	// should delete the cert and create a new one.
	cfg := &TLSManagerCfg{
		TLSCertPath:     certPath,
		TLSKeyPath:      keyPath,
		TLSCertDuration: testTLSCertDuration,
	}
	tlsManager := NewTLSManager(cfg)
	_, err := tlsManager.generateOrRenewCert()
	require.NoError(t, err)
	_, _, _, cleanUp, err := tlsManager.getConfig()
	require.NoError(t, err, "couldn't retrieve TLS config")
	t.Cleanup(cleanUp)

	// Grab the certificate to test that getTLSConfig did its job correctly
	// and generated a new cert.
	newCertData, err := tls.LoadX509KeyPair(certPath, keyPath)
	require.NoError(t, err, "couldn't grab new certificate")

	newCert, err := x509.ParseCertificate(newCertData.Certificate[0])
	require.NoError(t, err, "couldn't parse new certificate")

	// Check that the expired certificate was successfully deleted and
	// replaced with a new one.
	require.True(t, newCert.NotAfter.After(expiredCert.NotAfter),
		"New certificate expiration is too old")
}

// TestTLSManagerGenCert tests that the new TLS Manager loads correctly,
// whether the encrypted TLS key flag is set or not.
func TestTLSManagerGenCert(t *testing.T) {
	t.Parallel()

	_, certPath, keyPath := newTestDirectory(t)

	cfg := &TLSManagerCfg{
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
	}
	tlsManager := NewTLSManager(cfg)

	_, err := tlsManager.generateOrRenewCert()
	require.NoError(t, err, "failed to generate new certificate")

	// After this is run, a new certificate should be created and written
	// to disk. Since the TLSEncryptKey flag isn't set, we should be able
	// to read it in plaintext from disk.
	_, keyBytes, err := cert.GetCertBytesFromPath(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)
	require.NoError(t, err, "unable to load certificate")
	require.True(t, bytes.HasPrefix(keyBytes, privateKeyPrefix),
		"key is encrypted, but shouldn't be")

	// Now test that if the TLSEncryptKey flag is set, an encrypted key is
	// created and written to disk.
	_, certPath, keyPath = newTestDirectory(t)

	cfg = &TLSManagerCfg{
		TLSEncryptKey:   true,
		TLSCertPath:     certPath,
		TLSKeyPath:      keyPath,
		TLSCertDuration: testTLSCertDuration,
	}
	tlsManager = NewTLSManager(cfg)
	keyRing := &mock.SecretKeyRing{
		RootKey: privKey,
	}

	err = tlsManager.generateCertPair(keyRing)
	require.NoError(t, err, "failed to generate new certificate")

	_, keyBytes, err = cert.GetCertBytesFromPath(
		certPath, keyPath,
	)
	require.NoError(t, err, "unable to load certificate")
	require.False(t, bytes.HasPrefix(keyBytes, privateKeyPrefix),
		"key isn't encrypted, but should be")
}

// TestEnsureEncryption tests that ensureEncryption does a couple of things:
// 1) If we have cfg.TLSEncryptKey set, but the tls file saved to disk is not
// encrypted, generateOrRenewCert encrypts the file and rewrites it to disk.
// 2) If cfg.TLSEncryptKey is not set, but the file *is* encrypted, then we
// need to return an error to the user.
func TestEnsureEncryption(t *testing.T) {
	t.Parallel()

	keyRing := &mock.SecretKeyRing{
		RootKey: privKey,
	}

	// Write an unencrypted cert file to disk.
	certPath, keyPath, _ := writeTestCertFiles(
		t, false, false, keyRing,
	)

	cfg := &TLSManagerCfg{
		TLSEncryptKey: true,
		TLSCertPath:   certPath,
		TLSKeyPath:    keyPath,
	}
	tlsManager := NewTLSManager(cfg)

	// Check that the keyBytes are initially plaintext.
	_, newKeyBytes, err := cert.GetCertBytesFromPath(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)

	require.NoError(t, err, "unable to load certificate files")
	require.True(t, bytes.HasPrefix(newKeyBytes, privateKeyPrefix),
		"key doesn't have correct plaintext prefix")

	// ensureEncryption should detect that the TLS key is in plaintext,
	// encrypt it, and rewrite the encrypted version to disk.
	err = tlsManager.ensureEncryption(keyRing)
	require.NoError(t, err, "failed to generate new certificate")

	// Grab the file from disk to check that the key is no longer
	// plaintext.
	_, newKeyBytes, err = cert.GetCertBytesFromPath(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)
	require.NoError(t, err, "unable to load certificate")
	require.False(t, bytes.HasPrefix(newKeyBytes, privateKeyPrefix),
		"key isn't encrypted, but should be")

	// Now let's flip the cfg.TLSEncryptKey to false. Since the key on file
	// is encrypted, ensureEncryption should error out.
	tlsManager.cfg.TLSEncryptKey = false
	err = tlsManager.ensureEncryption(keyRing)
	require.Error(t, err)
}

// TestGenerateEphemeralCert tests that an ephemeral certificate is created and
// stored to disk in a .tmp file and that LoadPermanentCertificate deletes
// file and replaces it with a fresh certificate pair.
func TestGenerateEphemeralCert(t *testing.T) {
	t.Parallel()

	_, certPath, keyPath := newTestDirectory(t)
	tmpCertPath := certPath + ".tmp"

	cfg := &TLSManagerCfg{
		TLSCertPath:     certPath,
		TLSKeyPath:      keyPath,
		TLSEncryptKey:   true,
		TLSCertDuration: testTLSCertDuration,
	}
	tlsManager := NewTLSManager(cfg)

	keyBytes, err := tlsManager.loadEphemeralCertificate()
	require.NoError(t, err, "failed to generate new certificate")

	certBytes, err := os.ReadFile(tmpCertPath)
	require.NoError(t, err)

	tlsr, err := cert.NewTLSReloader(certBytes, keyBytes)
	require.NoError(t, err)
	tlsManager.tlsReloader = tlsr

	// Make sure .tmp file is created at the tmp cert path.
	_, err = os.ReadFile(tmpCertPath)
	require.NoError(t, err, "couldn't find temp cert file")

	// But no key should be stored.
	_, err = os.ReadFile(cfg.TLSKeyPath)
	require.Error(t, err, "shouldn't have found file")

	// And no permanent cert file should be stored.
	_, err = os.ReadFile(cfg.TLSCertPath)
	require.Error(t, err, "shouldn't have found a permanent cert file")

	// Now test that when we reload the certificate it generates the new
	// certificate properly.
	keyRing := &mock.SecretKeyRing{
		RootKey: privKey,
	}
	err = tlsManager.LoadPermanentCertificate(keyRing)
	require.NoError(t, err, "unable to reload certificate")

	// Make sure .tmp file is deleted.
	_, _, err = cert.GetCertBytesFromPath(
		tmpCertPath, cfg.TLSKeyPath,
	)
	require.Error(t, err, ".tmp file should have been deleted")

	// Make sure a certificate now exists at the permanent cert path.
	_, _, err = cert.GetCertBytesFromPath(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)
	require.NoError(t, err, "error loading permanent certificate")
}

// genCertPair generates a key/cert pair, with the option of generating expired
// certificates to make sure they are being regenerated correctly.
func genCertPair(t *testing.T, expired bool) ([]byte, []byte) {
	t.Helper()

	// Max serial number.
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)

	// Generate a serial number that's below the serialNumberLimit.
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	require.NoError(t, err, "failed to generate serial number")

	host := "lightning"

	// Create a simple ip address for the fake certificate.
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	dnsNames := []string{host, "unix", "unixpacket"}

	var notBefore, notAfter time.Time
	if expired {
		notBefore = time.Now().Add(-time.Hour * 24)
		notAfter = time.Now()
	} else {
		notBefore = time.Now()
		notAfter = time.Now().Add(time.Hour * 24)
	}

	// Construct the certificate template.
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"lnd autogenerated cert"},
			CommonName:   host,
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,
		KeyUsage: x509.KeyUsageKeyEncipherment |
			x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true, // so can sign self.
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}

	// Generate a private key for the certificate.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate a private key")
	}

	certDerBytes, err := x509.CreateCertificate(
		rand.Reader, &template, &template, &priv.PublicKey, priv,
	)
	require.NoError(t, err, "failed to create certificate")

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	require.NoError(t, err, "unable to encode privkey")

	return certDerBytes, keyBytes
}

// writeTestCertFiles creates test files and writes them to a temporary testing
// directory.
func writeTestCertFiles(t *testing.T, expiredCert, encryptTLSKey bool,
	keyRing keychain.KeyRing) (string, string, *x509.Certificate) {

	t.Helper()

	tempDir, certPath, keyPath := newTestDirectory(t)

	var certDerBytes, keyBytes []byte
	// Either create a valid certificate or an expired certificate pair,
	// depending on the test.
	if expiredCert {
		certDerBytes, keyBytes = genCertPair(t, true)
	} else {
		certDerBytes, keyBytes = genCertPair(t, false)
	}

	parsedCert, err := x509.ParseCertificate(certDerBytes)
	require.NoError(t, err, "failed to parse certificate")

	certBuf := bytes.Buffer{}
	err = pem.Encode(
		&certBuf, &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: certDerBytes,
		},
	)
	require.NoError(t, err, "failed to encode certificate")

	var keyBuf *bytes.Buffer
	if !encryptTLSKey {
		keyBuf = &bytes.Buffer{}
		err = pem.Encode(
			keyBuf, &pem.Block{
				Type:  "EC PRIVATE KEY",
				Bytes: keyBytes,
			},
		)
		require.NoError(t, err, "failed to encode private key")
	} else {
		e, err := lnencrypt.KeyRingEncrypter(keyRing)
		require.NoError(t, err, "unable to generate key encrypter")
		err = e.EncryptPayloadToWriter(
			keyBytes, keyBuf,
		)
		require.NoError(t, err, "failed to encrypt private key")
	}

	err = os.WriteFile(tempDir+"/tls.cert", certBuf.Bytes(), 0644)
	require.NoError(t, err, "failed to write cert file")
	err = os.WriteFile(tempDir+"/tls.key", keyBuf.Bytes(), 0600)
	require.NoError(t, err, "failed to write key file")

	return certPath, keyPath, parsedCert
}

// newTestDirectory creates a new test directory and returns the location of
// the test tls.cert and tls.key files.
func newTestDirectory(t *testing.T) (string, string, string) {
	t.Helper()

	tempDir := t.TempDir()
	certPath := tempDir + "/tls.cert"
	keyPath := tempDir + "/tls.key"

	return tempDir, certPath, keyPath
}

// TestGenerateCertPairWithPartialFiles tests that generateCertPair regenerates
// a cert/key pair when only one file exists.
func TestGenerateCertPairWithPartialFiles(t *testing.T) {
	t.Parallel()

	keyRing := &mock.SecretKeyRing{
		RootKey: privKey,
	}

	testCases := []struct {
		name  string
		setup func(t *testing.T, certPath, keyPath string)
	}{
		{
			name: "only key exists",
			setup: func(t *testing.T, certPath, keyPath string) {
				// Create only a key file. It simulates leftover
				// from previous run.
				_, keyBytes := genCertPair(t, false)
				keyBuf := &bytes.Buffer{}
				err := pem.Encode(
					keyBuf, &pem.Block{
						Type:  "EC PRIVATE KEY",
						Bytes: keyBytes,
					},
				)
				require.NoError(t, err)

				err = os.WriteFile(
					keyPath, keyBuf.Bytes(), 0600,
				)
				require.NoError(t, err)
			},
		},
		{
			name: "only cert exists",
			setup: func(t *testing.T, certPath, keyPath string) {
				// Create only a cert file. It simulates
				// leftover from previous run.
				certBytes, _ := genCertPair(t, false)
				certBuf := &bytes.Buffer{}
				err := pem.Encode(
					certBuf, &pem.Block{
						Type:  "CERTIFICATE",
						Bytes: certBytes,
					},
				)
				require.NoError(t, err)

				err = os.WriteFile(
					certPath, certBuf.Bytes(), 0644,
				)
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tempDir := t.TempDir()
			certPath := tempDir + "/tls.cert"
			keyPath := tempDir + "/tls.key"

			tc.setup(t, certPath, keyPath)

			cfg := &TLSManagerCfg{
				TLSCertPath:     certPath,
				TLSKeyPath:      keyPath,
				TLSCertDuration: testTLSCertDuration,
			}
			tlsManager := NewTLSManager(cfg)

			err := tlsManager.generateCertPair(keyRing)
			require.NoError(
				t, err, "should generate new cert pair when %s",
				tc.name,
			)

			_, _, err = cert.GetCertBytesFromPath(certPath, keyPath)
			require.NoError(
				t, err, "should be able to load cert pair",
			)
		})
	}
}

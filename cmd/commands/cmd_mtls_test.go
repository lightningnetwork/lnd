package commands

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
)

func TestMTLSCommandRegistration(t *testing.T) {
	dir := t.TempDir()
	app := cli.NewApp()
	app.Commands = []cli.Command{mtlsCommand}
	err := app.Run([]string{"lncli", "mtls", "new-client", "--keyout", filepath.Join(dir, "key"), "--csrout", filepath.Join(dir, "csr")})
	require.NoError(t, err)
}

func TestMTLSOfflineEnrollment(t *testing.T) {
	dir := t.TempDir()
	keyPath, csrPath := filepath.Join(dir, "client.key"), filepath.Join(dir, "client.csr")
	require.NoError(t, newMTLSClient(keyPath, csrPath))
	keyPEM, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	block, _ := pem.Decode(keyPEM)
	require.NotNil(t, block)
	key, err := x509.ParseECPrivateKey(block.Bytes)
	require.NoError(t, err)
	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
	csrPEM, err := os.ReadFile(csrPath)
	require.NoError(t, err)
	block, _ = pem.Decode(csrPEM)
	require.NotNil(t, block)
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	require.NoError(t, err)
	require.NoError(t, csr.CheckSignature())
	require.True(t, key.PublicKey.Equal(csr.PublicKey.(*ecdsa.PublicKey)))
	require.Error(t, newMTLSClient(keyPath, csrPath))
	samePath := filepath.Join(dir, "same")
	require.Error(t, newMTLSClient(samePath, filepath.Join(dir, ".", "same")))
	alias := filepath.Join(dir, "alias")
	if os.Symlink(dir, alias) == nil {
		require.Error(t, newMTLSClient(samePath, filepath.Join(alias, "same")))
		_, err = os.Stat(samePath)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	freshKey := filepath.Join(dir, "fresh.key")
	require.Error(t, newMTLSClient(freshKey, csrPath))
	_, err = os.Stat(freshKey)
	require.ErrorIs(t, err, os.ErrNotExist)
	existing, err := os.ReadFile(csrPath)
	require.NoError(t, err)
	require.Equal(t, csrPEM, existing)

	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, csr.PublicKey, key)
	require.NoError(t, err)
	certPath := filepath.Join(dir, "client.crt")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	allowPath := filepath.Join(dir, "allowlist")
	require.NoError(t, os.WriteFile(allowPath, []byte("# retained"), 0640))
	allowLink := filepath.Join(dir, "allowlink")
	if os.Symlink(allowPath, allowLink) == nil {
		require.ErrorContains(t, allowMTLSClient(certPath, allowLink), "regular file")
	}
	require.NoError(t, allowMTLSClient(certPath, allowPath))
	fingerprint := sha256.Sum256(der)
	expected := fmt.Sprintf("# retained\n%x\n", fingerprint)
	actual, err := os.ReadFile(allowPath)
	require.NoError(t, err)
	require.Equal(t, expected, string(actual))
	info, err = os.Stat(allowPath)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0640), info.Mode().Perm())
	}
	require.NoError(t, allowMTLSClient(certPath, allowPath))
	actual, err = os.ReadFile(allowPath)
	require.NoError(t, err)
	require.Equal(t, expected, string(actual))
	require.NoError(t, os.WriteFile(allowPath, []byte("invalid\n"), 0600))
	require.Error(t, allowMTLSClient(certPath, allowPath))
	actual, err = os.ReadFile(allowPath)
	require.NoError(t, err)
	require.Equal(t, "invalid\n", string(actual))
	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	malformed := append([]byte("-----BEGIN CERTIFICATE-----\nbad prefix\n"), certPEM...)
	require.NoError(t, os.WriteFile(certPath, malformed, 0600))
	require.ErrorContains(t, allowMTLSClient(certPath, allowPath), "exactly one PEM certificate")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(allowPath+".lock", nil, 0600))
	require.ErrorContains(t, allowMTLSClient(certPath, allowPath), "lock")
	require.NoError(t, os.WriteFile(certPath, []byte("invalid"), 0600))
	require.Error(t, allowMTLSClient(certPath, allowPath))
	require.Error(t, newMTLSClient("", ""))
	require.Error(t, allowMTLSClient("", ""))
}

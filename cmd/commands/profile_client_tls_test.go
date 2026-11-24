package commands

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
)

func TestProfileClientTLS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.pem")
	keyPath := filepath.Join(dir, "client.key")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: der,
	}), 0600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "PRIVATE KEY", Bytes: keyDER,
	}), 0600))

	flags := func(args ...string) *cli.Context {
		t.Helper()
		set := flag.NewFlagSet("lncli", flag.ContinueOnError)
		set.String("tlsclientcertpath", "", "")
		set.String("tlsclientkeypath", "", "")
		require.NoError(t, set.Parse(args))
		app := cli.NewApp()
		global := cli.NewContext(app, set, nil)
		return cli.NewContext(app, flag.NewFlagSet("command", flag.ContinueOnError), global)
	}

	var original profileEntry
	cert, err := original.clientCertificate()
	require.NoError(t, err)
	require.Nil(t, cert)
	require.NoError(t, original.setClientCertificatePaths(flags(
		"--tlsclientcertpath="+certPath, "--tlsclientkeypath="+keyPath,
	), true))

	// A profile round-trip stores paths only and loads the private key from
	// disk when the next command connects.
	encoded, err := json.Marshal(original)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "BEGIN PRIVATE KEY")
	var loaded profileEntry
	require.NoError(t, json.Unmarshal(encoded, &loaded))
	require.NoError(t, loaded.setClientCertificatePaths(flags(), false))
	require.Equal(t, certPath, loaded.TLSClientCertPath)
	require.Equal(t, keyPath, loaded.TLSClientKeyPath)
	cert, err = loaded.clientCertificate()
	require.NoError(t, err)
	require.Equal(t, der, cert.Certificate[0])

	// Explicit flags can override the profile only as a pair.
	require.Error(t, loaded.setClientCertificatePaths(flags(
		"--tlsclientcertpath=another.pem",
	), false))
	require.Equal(t, certPath, loaded.TLSClientCertPath)
	require.NoError(t, loaded.setClientCertificatePaths(flags(
		"--tlsclientcertpath=another.pem", "--tlsclientkeypath=another.key",
	), false))
	require.Equal(t, "another.pem", loaded.TLSClientCertPath)
	require.Equal(t, "another.key", loaded.TLSClientKeyPath)

	original.Insecure = true
	_, err = original.clientCertificate()
	require.ErrorContains(t, err, "cannot use insecure mode")
}

package commands

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lightningnetwork/lnd/internal/rpcclientallowlist"
	"github.com/urfave/cli"
)

var mtlsCommand = cli.Command{
	Name:     "mtls",
	Category: "Security",
	Usage:    "Manage offline RPC client enrollment.",
	Subcommands: []cli.Command{
		{
			Name: "new-client",
			Usage: "Generate a client private key and CSR " +
				"for an external CA.",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:      "keyout",
					Usage:     "Private key output path.",
					TakesFile: true,
				},
				cli.StringFlag{
					Name: "csrout",
					Usage: "Certificate signing request " +
						"output path.",
					TakesFile: true,
				},
			},
			Action: newMTLSClientAction,
		},
		{
			Name: "allow-client",
			Usage: "Add a signed client certificate " +
				"to an allowlist.",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name: "cert",
					Usage: "Signed leaf certificate " +
						"path.",
					TakesFile: true,
				},
				cli.StringFlag{
					Name:      "allowlist",
					Usage:     "Allowlist path.",
					TakesFile: true,
				},
			},
			Action: allowMTLSClientAction,
		},
	},
}

func newMTLSClientAction(ctx *cli.Context) error {
	return newMTLSClient(ctx.String("keyout"), ctx.String("csrout"))
}

func allowMTLSClientAction(ctx *cli.Context) error {
	return allowMTLSClient(ctx.String("cert"), ctx.String("allowlist"))
}

func newMTLSClient(keyPath, csrPath string) (err error) {
	if keyPath == "" || csrPath == "" {
		return errors.New("keyout and csrout are required")
	}
	keyAbs, err := filepath.Abs(keyPath)
	if err != nil {
		return err
	}
	csrAbs, err := filepath.Abs(csrPath)
	if err != nil {
		return err
	}
	if keyAbs == csrAbs {
		return errors.New("keyout and csrout must be distinct")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(
		rand.Reader, &x509.CertificateRequest{}, key,
	)
	if err != nil {
		return err
	}
	keyFile, err := os.OpenFile(
		keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600,
	)
	if err != nil {
		return err
	}
	// Remove the output if a later step fails.
	defer func() {
		if err != nil {
			_ = os.Remove(keyPath)
		}
	}()
	_, err = keyFile.Write(pem.EncodeToMemory(&pem.Block{
		Type: "EC PRIVATE KEY", Bytes: keyDER,
	}))
	if closeErr := keyFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	csrFile, err := os.OpenFile(
		csrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644,
	)
	if err != nil {
		return err
	}
	// Remove the output if a later step fails.
	defer func() {
		if err != nil {
			_ = os.Remove(csrPath)
		}
	}()
	_, err = csrFile.Write(pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE REQUEST", Bytes: csrDER,
	}))
	if closeErr := csrFile.Close(); err == nil {
		err = closeErr
	}

	return err
}

func allowMTLSClient(certPath, allowPath string) error {
	if certPath == "" || allowPath == "" {
		return errors.New("cert and allowlist are required")
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return err
	}
	certPEM = bytes.TrimSpace(certPEM)
	if !bytes.HasPrefix(certPEM, []byte("-----BEGIN CERTIFICATE-----")) ||
		bytes.Count(certPEM, []byte("-----BEGIN ")) != 1 {

		return errors.New("cert must contain exactly one " +
			"PEM certificate")
	}
	block, rest := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" ||
		len(bytes.TrimSpace(rest)) != 0 {

		return errors.New("cert must contain exactly one " +
			"PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}
	if cert.IsCA {
		return errors.New("cert must be a leaf, not a CA")
	}
	fingerprint := sha256.Sum256(cert.Raw)

	// Cooperating writers fail rather than race. A crash can leave a stale
	// lock.
	lockPath := allowPath + ".lock"
	lock, err := os.OpenFile(
		lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600,
	)
	if err != nil {
		return fmt.Errorf(
			"acquire allowlist lock %s: %w", lockPath, err,
		)
	}
	defer os.Remove(lockPath)
	if err := lock.Close(); err != nil {
		return err
	}

	mode := os.FileMode(0600)
	info, err := os.Lstat(allowPath)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return errors.New("allowlist must be a regular file")
		}
		mode = info.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	contents, err := os.ReadFile(allowPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := rpcclientallowlist.Parse(bytes.NewReader(contents))
	if err != nil {
		return fmt.Errorf("invalid allowlist: %w", err)
	}
	if _, ok := entries[fingerprint]; ok {
		return nil
	}
	if len(contents) > 0 && !strings.HasSuffix(string(contents), "\n") {
		contents = append(contents, '\n')
	}
	contents = append(contents, []byte(fmt.Sprintf("%x\n", fingerprint))...)
	tmp, err := os.CreateTemp(filepath.Dir(allowPath), ".mtls-allowlist-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(contents); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), allowPath)
}

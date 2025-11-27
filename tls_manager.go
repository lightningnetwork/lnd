package lnd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/lightningnetwork/lnd/lnrpc"
	"golang.org/x/crypto/acme/autocert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	// modifyFilePermissons is the file permission used for writing
	// encrypted tls files.
	modifyFilePermissions = 0600

	// validityHours is the number of hours the ephemeral tls certificate
	// will be valid, if encrypting tls certificates is turned on.
	validityHours = 24
)

var (
	// privateKeyPrefix is the prefix to a plaintext TLS key.
	// It should match these two key formats:
	// - `-----BEGIN PRIVATE KEY-----`    (PKCS8).
	// - `-----BEGIN EC PRIVATE KEY-----` (SEC1/rfc5915, the legacy format).
	privateKeyPrefix = []byte("-----BEGIN ")
)

// TLSManagerCfg houses a set of values and methods that is passed to the
// TLSManager for it to properly manage LND's TLS options.
type TLSManagerCfg struct {
	TLSCertPath        string
	TLSKeyPath         string
	TLSEncryptKey      bool
	TLSExtraIPs        []string
	TLSExtraDomains    []string
	TLSAutoRefresh     bool
	TLSDisableAutofill bool
	TLSCertDuration    time.Duration

	LetsEncryptDir    string
	LetsEncryptDomain string
	LetsEncryptListen string

	DisableRestTLS bool

	HTTPHeaderTimeout time.Duration
}

// TLSManager generates/renews a TLS cert/key pair when needed. When required,
// it encrypts the TLS key. It also returns the certificate configuration
// options needed for gRPC and REST.
type TLSManager struct {
	cfg *TLSManagerCfg

	// tlsReloader is able to reload the certificate with the
	// GetCertificate function. In getConfig, tlsCfg.GetCertificate is
	// pointed towards t.tlsReloader.GetCertificateFunc(). When
	// TLSReloader's AttemptReload is called, the cert that tlsReloader
	// holds is changed, in turn changing the cert data
	// tlsCfg.GetCertificate will return.
	tlsReloader *cert.TLSReloader

	// These options are only used if we're currently using an ephemeral
	// TLS certificate, used when we're encrypting the TLS key.
	ephemeralKey      []byte
	ephemeralCert     []byte
	ephemeralCertPath string
}

// NewTLSManager returns a reference to a new TLSManager.
func NewTLSManager(cfg *TLSManagerCfg) *TLSManager {
	return &TLSManager{
		cfg: cfg,
	}
}

// getConfig returns a TLS configuration for the gRPC server and credentials
// and a proxy destination for the REST reverse proxy.
func (t *TLSManager) getConfig() ([]grpc.ServerOption, []grpc.DialOption,
	func(net.Addr) (net.Listener, error), func(), error) {

	var (
		keyBytes, certBytes []byte
		err                 error
	)
	if t.ephemeralKey != nil {
		keyBytes = t.ephemeralKey
		certBytes = t.ephemeralCert
	} else {
		certBytes, keyBytes, err = cert.GetCertBytesFromPath(
			t.cfg.TLSCertPath, t.cfg.TLSKeyPath,
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}

	certData, _, err := cert.LoadCertFromBytes(certBytes, keyBytes)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if t.tlsReloader == nil {
		tlsr, err := cert.NewTLSReloader(certBytes, keyBytes)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		t.tlsReloader = tlsr
	}

	tlsCfg := cert.TLSConfFromCert(certData)
	tlsCfg.GetCertificate = t.tlsReloader.GetCertificateFunc()

	// If Let's Encrypt is enabled, we need to set up the autocert manager
	// and override the TLS config's GetCertificate function.
	cleanUp := t.setUpLetsEncrypt(&certData, tlsCfg)

	// Now that we know that we have a certificate, let's generate the
	// required config options.
	serverCreds := credentials.NewTLS(tlsCfg)
	serverOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}

	// For our REST dial options, we skip TLS verification, and we also
	// increase the max message size that we'll decode to allow clients to
	// hit endpoints which return more data such as the DescribeGraph call.
	// We set this to 200MiB atm. Should be the same value as maxMsgRecvSize
	// in cmd/lncli/main.go.
	restDialOpts := []grpc.DialOption{
		// We are forwarding the requests directly to the address of our
		// own local listener. To not need to mess with the TLS
		// certificate (which might be tricky if we're using Let's
		// Encrypt or if the ephemeral tls cert is being used), we just
		// skip the certificate verification. Injecting a malicious
		// hostname into the listener address will result in an error
		// on startup so this should be quite safe.
		grpc.WithTransportCredentials(credentials.NewTLS(
			&tls.Config{InsecureSkipVerify: true},
		)),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(lnrpc.MaxGrpcMsgSize),
		),
	}

	// Return a function closure that can be used to listen on a given
	// address with the current TLS config.
	restListen := func(addr net.Addr) (net.Listener, error) {
		// For restListen we will call ListenOnAddress if TLS is
		// disabled.
		if t.cfg.DisableRestTLS {
			return lncfg.ListenOnAddress(addr)
		}

		return lncfg.TLSListenOnAddress(addr, tlsCfg)
	}

	return serverOpts, restDialOpts, restListen, cleanUp, nil
}

// generateOrRenewCert generates a new TLS certificate if we're not using one
// yet or renews it if it's outdated.
func (t *TLSManager) generateOrRenewCert() (*tls.Config, error) {
	// Generete a TLS pair if we don't have one yet.
	var emptyKeyRing keychain.SecretKeyRing
	err := t.generateCertPair(emptyKeyRing)
	if err != nil {
		return nil, err
	}

	certData, parsedCert, err := cert.LoadCert(
		t.cfg.TLSCertPath, t.cfg.TLSKeyPath,
	)
	if err != nil {
		return nil, err
	}

	// Check to see if the certificate needs to be renewed. If it does, we
	// return the newly generated certificate data instead.
	reloadedCertData, err := t.maintainCert(parsedCert)
	if err != nil {
		return nil, err
	}
	if reloadedCertData != nil {
		certData = *reloadedCertData
	}

	tlsCfg := cert.TLSConfFromCert(certData)

	return tlsCfg, nil
}

// generateCertPair creates and writes a TLS pair to disk if the pair
// doesn't exist yet. If the TLSEncryptKey setting is on, and a plaintext key
// is already written to disk, this function overwrites the plaintext key with
// the encrypted form.
func (t *TLSManager) generateCertPair(keyRing keychain.SecretKeyRing) error {
	// Ensure we create TLS key and certificate if they don't both exist.
	if lnrpc.FileExists(t.cfg.TLSCertPath) &&
		lnrpc.FileExists(t.cfg.TLSKeyPath) {

		// Handle discrepencies related to the TLSEncryptKey setting.
		return t.ensureEncryption(keyRing)
	}

	rpcsLog.Infof("Generating TLS certificates...")
	certBytes, keyBytes, err := cert.GenCertPair(
		"lnd autogenerated cert", t.cfg.TLSExtraIPs,
		t.cfg.TLSExtraDomains, t.cfg.TLSDisableAutofill,
		t.cfg.TLSCertDuration,
	)
	if err != nil {
		return err
	}

	if t.cfg.TLSEncryptKey {
		var b bytes.Buffer
		e, err := lnencrypt.KeyRingEncrypter(keyRing)
		if err != nil {
			return fmt.Errorf("unable to create "+
				"encrypt key %v", err)
		}

		err = e.EncryptPayloadToWriter(
			keyBytes, &b,
		)
		if err != nil {
			return err
		}

		keyBytes = b.Bytes()
	}

	err = cert.WriteCertPair(
		t.cfg.TLSCertPath, t.cfg.TLSKeyPath, certBytes, keyBytes,
	)

	rpcsLog.Infof("Done generating TLS certificates")

	return err
}

// ensureEncryption takes a look at a couple of things:
// 1) If the TLS key is in plaintext, but TLSEncryptKey is set, we need to
// encrypt the file and rewrite it to disk.
// 2) On the flip side, if TLSEncryptKey is not set, but the key on disk
// is encrypted, we need to error out and warn the user.
func (t *TLSManager) ensureEncryption(keyRing keychain.SecretKeyRing) error {
	_, keyBytes, err := cert.GetCertBytesFromPath(
		t.cfg.TLSCertPath, t.cfg.TLSKeyPath,
	)
	if err != nil {
		return err
	}

	if t.cfg.TLSEncryptKey && bytes.HasPrefix(keyBytes, privateKeyPrefix) {
		var b bytes.Buffer
		e, err := lnencrypt.KeyRingEncrypter(keyRing)
		if err != nil {
			return fmt.Errorf("unable to generate encrypt key %w",
				err)
		}

		err = e.EncryptPayloadToWriter(keyBytes, &b)
		if err != nil {
			return err
		}
		err = os.WriteFile(
			t.cfg.TLSKeyPath, b.Bytes(), modifyFilePermissions,
		)
		if err != nil {
			return err
		}
	}

	// If the private key is encrypted but the user didn't pass
	// --tlsencryptkey we error out. This is because the wallet is not
	// unlocked yet and we don't have access to the keys yet for decryption.
	if !t.cfg.TLSEncryptKey && !bytes.HasPrefix(keyBytes,
		privateKeyPrefix) {

		ltndLog.Errorf("The TLS private key is encrypted on disk.")

		return errors.New("the TLS key is encrypted but the " +
			"--tlsencryptkey flag is not passed. Please either " +
			"restart lnd with the --tlsencryptkey flag or delete " +
			"the TLS files for regeneration")
	}

	return nil
}

// decryptTLSKeyBytes decrypts the TLS key.
func decryptTLSKeyBytes(keyRing keychain.SecretKeyRing,
	encryptedData []byte) ([]byte, error) {

	reader := bytes.NewReader(encryptedData)
	encrypter, err := lnencrypt.KeyRingEncrypter(keyRing)
	if err != nil {
		return nil, err
	}

	plaintext, err := encrypter.DecryptPayloadFromReader(
		reader,
	)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}

// maintainCert checks if the certificate IP and domains matches the config,
// and renews the certificate if either this data is outdated or the
// certificate is expired.
func (t *TLSManager) maintainCert(
	parsedCert *x509.Certificate) (*tls.Certificate, error) {

	// We check whether the certificate we have on disk match the IPs and
	// domains specified by the config. If the extra IPs or domains have
	// changed from when the certificate was created, we will refresh the
	// certificate if auto refresh is active.
	refresh := false
	var err error
	if t.cfg.TLSAutoRefresh {
		refresh, err = cert.IsOutdated(
			parsedCert, t.cfg.TLSExtraIPs,
			t.cfg.TLSExtraDomains, t.cfg.TLSDisableAutofill,
		)
		if err != nil {
			return nil, err
		}
	}

	// If the certificate expired or it was outdated, delete it and the TLS
	// key and generate a new pair.
	if !time.Now().After(parsedCert.NotAfter) && !refresh {
		return nil, nil
	}

	ltndLog.Info("TLS certificate is expired or outdated, " +
		"generating a new one")

	err = os.Remove(t.cfg.TLSCertPath)
	if err != nil {
		return nil, err
	}

	err = os.Remove(t.cfg.TLSKeyPath)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("Renewing TLS certificates...")
	certBytes, keyBytes, err := cert.GenCertPair(
		"lnd autogenerated cert", t.cfg.TLSExtraIPs,
		t.cfg.TLSExtraDomains, t.cfg.TLSDisableAutofill,
		t.cfg.TLSCertDuration,
	)
	if err != nil {
		return nil, err
	}

	err = cert.WriteCertPair(
		t.cfg.TLSCertPath, t.cfg.TLSKeyPath, certBytes, keyBytes,
	)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("Done renewing TLS certificates")

	// Reload the certificate data.
	reloadedCertData, _, err := cert.LoadCert(
		t.cfg.TLSCertPath, t.cfg.TLSKeyPath,
	)

	return &reloadedCertData, err
}

// setUpLetsEncrypt automatically generates a Let's Encrypt certificate if the
// option is set.
func (t *TLSManager) setUpLetsEncrypt(certData *tls.Certificate,
	tlsCfg *tls.Config) func() {

	// If Let's Encrypt is enabled, instantiate autocert to request/renew
	// the certificates.
	cleanUp := func() {}
	if t.cfg.LetsEncryptDomain == "" {
		return cleanUp
	}

	ltndLog.Infof("Using Let's Encrypt certificate for domain %v",
		t.cfg.LetsEncryptDomain)

	manager := autocert.Manager{
		Cache:  autocert.DirCache(t.cfg.LetsEncryptDir),
		Prompt: autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(
			t.cfg.LetsEncryptDomain,
		),
	}

	srv := &http.Server{
		Addr:              t.cfg.LetsEncryptListen,
		Handler:           manager.HTTPHandler(nil),
		ReadHeaderTimeout: t.cfg.HTTPHeaderTimeout,
	}
	shutdownCompleted := make(chan struct{})
	cleanUp = func() {
		err := srv.Shutdown(context.Background())
		if err != nil {
			ltndLog.Errorf("Autocert listener shutdown "+
				" error: %v", err)

			return
		}
		<-shutdownCompleted
		ltndLog.Infof("Autocert challenge listener stopped")
	}

	go func() {
		ltndLog.Infof("Autocert challenge listener started "+
			"at %v", t.cfg.LetsEncryptListen)

		err := srv.ListenAndServe()
		if err != http.ErrServerClosed {
			ltndLog.Errorf("autocert http: %v", err)
		}
		close(shutdownCompleted)
	}()

	getCertificate := func(h *tls.ClientHelloInfo) (
		*tls.Certificate, error) {

		lecert, err := manager.GetCertificate(h)
		if err != nil {
			ltndLog.Errorf("GetCertificate: %v", err)
			return certData, nil
		}

		return lecert, err
	}

	// The self-signed tls.cert remains available as fallback.
	tlsCfg.GetCertificate = getCertificate

	return cleanUp
}

// SetCertificateBeforeUnlock takes care of loading the certificate before
// the wallet is unlocked. If the TLSEncryptKey setting is on, we need to
// generate an ephemeral certificate we're able to use until the wallet is
// unlocked and a new TLS pair can be encrypted to disk. Otherwise we can
// process the certificate normally.
func (t *TLSManager) SetCertificateBeforeUnlock() ([]grpc.ServerOption,
	[]grpc.DialOption, func(net.Addr) (net.Listener, error), func(),
	error) {

	if t.cfg.TLSEncryptKey {
		_, err := t.loadEphemeralCertificate()
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("unable to load "+
				"ephemeral certificate: %v", err)
		}
	} else {
		_, err := t.generateOrRenewCert()
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("unable to "+
				"generate or renew TLS certificate: %v", err)
		}
	}

	serverOpts, restDialOpts, restListen, cleanUp, err := t.getConfig()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("unable to load TLS "+
			"credentials: %v", err)
	}

	return serverOpts, restDialOpts, restListen, cleanUp, nil
}

// loadEphemeralCertificate creates and loads the ephemeral certificate which
// is used temporarily for secure communications before the wallet is unlocked.
func (t *TLSManager) loadEphemeralCertificate() ([]byte, error) {
	rpcsLog.Infof("Generating ephemeral TLS certificates...")

	tmpValidity := validityHours * time.Hour
	// Append .tmp to the end of the cert for differentiation.
	tmpCertPath := t.cfg.TLSCertPath + ".tmp"

	// Pass in a blank string for the key path so the
	// function doesn't write them to disk.
	certBytes, keyBytes, err := cert.GenCertPair(
		"lnd ephemeral autogenerated cert", t.cfg.TLSExtraIPs,
		t.cfg.TLSExtraDomains, t.cfg.TLSDisableAutofill, tmpValidity,
	)
	if err != nil {
		return nil, err
	}
	t.setEphemeralSettings(keyBytes, certBytes)

	err = cert.WriteCertPair(tmpCertPath, "", certBytes, keyBytes)
	if err != nil {
		return nil, err
	}

	rpcsLog.Infof("Done generating ephemeral TLS certificates")

	return keyBytes, nil
}

// LoadPermanentCertificate deletes the ephemeral certificate file and
// generates a new one with the real keyring.
func (t *TLSManager) LoadPermanentCertificate(
	keyRing keychain.SecretKeyRing) error {

	if !t.cfg.TLSEncryptKey {
		return nil
	}

	tmpCertPath := t.cfg.TLSCertPath + ".tmp"
	err := os.Remove(tmpCertPath)
	if err != nil {
		ltndLog.Warn("Unable to delete temp cert at %v",
			tmpCertPath)
	}

	err = t.generateCertPair(keyRing)
	if err != nil {
		return err
	}

	certBytes, encryptedKeyBytes, err := cert.GetCertBytesFromPath(
		t.cfg.TLSCertPath, t.cfg.TLSKeyPath,
	)
	if err != nil {
		return err
	}

	reader := bytes.NewReader(encryptedKeyBytes)
	e, err := lnencrypt.KeyRingEncrypter(keyRing)
	if err != nil {
		return fmt.Errorf("unable to generate encrypt key %w",
			err)
	}

	keyBytes, err := e.DecryptPayloadFromReader(reader)
	if err != nil {
		return err
	}

	// Switch the server's TLS certificate to the persistent one. By
	// changing the cert data the TLSReloader points to,
	err = t.tlsReloader.AttemptReload(certBytes, keyBytes)
	if err != nil {
		return err
	}

	t.deleteEphemeralSettings()

	return nil
}

// setEphemeralSettings sets the TLSManager settings needed when an ephemeral
// certificate is created.
func (t *TLSManager) setEphemeralSettings(keyBytes, certBytes []byte) {
	t.ephemeralKey = keyBytes
	t.ephemeralCert = certBytes
	t.ephemeralCertPath = t.cfg.TLSCertPath + ".tmp"
}

// deleteEphemeralSettings deletes the TLSManager ephemeral settings that are
// no longer needed when the ephemeral certificate is deleted so the Manager
// knows we're no longer using it.
func (t *TLSManager) deleteEphemeralSettings() {
	t.ephemeralKey = nil
	t.ephemeralCert = nil
	t.ephemeralCertPath = ""
}

// IsCertExpired checks if the current TLS certificate is expired.
func (t *TLSManager) IsCertExpired(keyRing keychain.SecretKeyRing) (bool,
	time.Time, error) {

	certBytes, keyBytes, err := cert.GetCertBytesFromPath(
		t.cfg.TLSCertPath, t.cfg.TLSKeyPath,
	)
	if err != nil {
		return false, time.Time{}, err
	}

	// If TLSEncryptKey is set, there are two states the
	// certificate can be in: ephemeral or permanent.
	// Retrieve the key depending on which state it is in.
	if t.ephemeralKey != nil {
		keyBytes = t.ephemeralKey
	} else if t.cfg.TLSEncryptKey {
		keyBytes, err = decryptTLSKeyBytes(keyRing, keyBytes)
		if err != nil {
			return false, time.Time{}, err
		}
	}

	_, parsedCert, err := cert.LoadCertFromBytes(
		certBytes, keyBytes,
	)
	if err != nil {
		return false, time.Time{}, err
	}

	// If the current time is passed the certificate's
	// expiry time, then it is considered expired
	if time.Now().After(parsedCert.NotAfter) {
		return true, parsedCert.NotAfter, nil
	}

	return false, parsedCert.NotAfter, nil
}

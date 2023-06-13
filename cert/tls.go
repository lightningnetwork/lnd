package cert

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"sync"
)

var (
	/*
	 * tlsCipherSuites is the list of cipher suites we accept for TLS
	 * connections. These cipher suites fit the following criteria:
	 * - Don't use outdated algorithms like SHA-1 and 3DES
	 * - Don't use ECB mode or other insecure symmetric methods
	 * - Included in the TLS v1.2 suite
	 * - Are available in the Go 1.7.6 standard library (more are
	 *   available in 1.8.3 and will be added after lnd no longer
	 *   supports 1.7, including suites that support CBC mode)
	**/
	tlsCipherSuites = []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
	}
)

// GetCertBytesFromPath reads the TLS certificate and key files at the given
// certPath and keyPath and returns the file bytes.
func GetCertBytesFromPath(certPath, keyPath string) (certBytes,
	keyBytes []byte, err error) {

	certBytes, err = os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyBytes, err = os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	return certBytes, keyBytes, nil
}

// LoadCert loads a certificate and its corresponding private key from the PEM
// files indicated and returns the certificate in the two formats it is most
// commonly used.
func LoadCert(certPath, keyPath string) (tls.Certificate, *x509.Certificate,
	error) {

	// The certData returned here is just a wrapper around the PEM blocks
	// loaded from the file. The PEM is not yet fully parsed but a basic
	// check is performed that the certificate and private key actually
	// belong together.
	certData, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	// Now parse the PEM block of the certificate into its x509 data
	// structure so it can be examined in more detail.
	x509Cert, err := x509.ParseCertificate(certData.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	return certData, x509Cert, nil
}

// LoadCertFromBytes loads a certificate and its corresponding private key from
// the PEM bytes indicated and returns the certificate in the two formats it is
// most commonly used.
func LoadCertFromBytes(certBytes, keyBytes []byte) (tls.Certificate,
	*x509.Certificate, error) {

	// The certData returned here is just a wrapper around the PEM blocks
	// loaded from the file. The PEM is not yet fully parsed but a basic
	// check is performed that the certificate and private key actually
	// belong together.
	certData, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	// Now parse the PEM block of the certificate into its x509 data
	// structure so it can be examined in more detail.
	x509Cert, err := x509.ParseCertificate(certData.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	return certData, x509Cert, nil
}

// TLSConfFromCert returns the default TLS configuration used for a server,
// using the given certificate as identity.
func TLSConfFromCert(certData tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{certData},
		CipherSuites: tlsCipherSuites,
		MinVersion:   tls.VersionTLS12,
	}
}

// TLSReloader updates the TLS certificate without restarting the server.
type TLSReloader struct {
	certMu sync.RWMutex
	cert   *tls.Certificate
}

// NewTLSReloader is used to create a new TLS Reloader that will be used
// to update the TLS certificate without restarting the server.
func NewTLSReloader(certBytes, keyBytes []byte) (*TLSReloader, error) {
	cert, _, err := LoadCertFromBytes(certBytes, keyBytes)
	if err != nil {
		return nil, err
	}
	return &TLSReloader{
		cert: &cert,
	}, nil
}

// AttemptReload will make an attempt to update the TLS certificate
// and key used by the server.
func (t *TLSReloader) AttemptReload(certBytes, keyBytes []byte) error {
	newCert, _, err := LoadCertFromBytes(certBytes, keyBytes)
	if err != nil {
		return err
	}

	t.certMu.Lock()
	t.cert = &newCert
	t.certMu.Unlock()

	return nil
}

// GetCertificateFunc is used in the server's TLS configuration to
// determine the correct TLS certificate to server on a request.
func (t *TLSReloader) GetCertificateFunc() func(*tls.ClientHelloInfo) (
	*tls.Certificate, error) {

	return func(clientHello *tls.ClientHelloInfo) (*tls.Certificate,
		error) {

		t.certMu.RLock()
		defer t.certMu.RUnlock()

		return t.cert, nil
	}
}

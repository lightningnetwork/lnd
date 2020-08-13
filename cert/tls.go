package cert

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
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
	tlsRSACipherSuites = []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	}
)

// LoadCert loads a certificate and its corresponding private key from the
// bytes indicated and returns the certificate in the two formats it is most
// commonly used.
func LoadCert(certBuf, keyBuf []byte) (tls.Certificate, *x509.Certificate,
	error) {

	// The certData returned here is just a wrapper around the PEM blocks
	// loaded from the file. The PEM is not yet fully parsed but a basic
	// check is performed that the certificate and private key actually
	// belong together.
	certData, err := tls.X509KeyPair(certBuf, keyBuf)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	// Now parse the the PEM block of the certificate into its x509 data
	// structure so it can be examined in more detail.
	x509Cert, err := x509.ParseCertificate(certData.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	return certData, x509Cert, nil
}

// TLSConfFromCert returns the default TLS configuration used for a server,
// using the given certificate as identity.
func TLSConfFromCert(certData []tls.Certificate) *tls.Config {
	var config *tls.Config

	getCertificate := func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		defaultCertList := []string{"localhost", "127.0.0.1"}
		for _, host := range defaultCertList {
			if strings.Contains(h.ServerName, host) {
				return &certData[0], nil
			}
		}
		return &certData[1], nil
	}

	if len(certData) > 1 {
		config = &tls.Config{
			Certificates:   []tls.Certificate{certData[0]},
			GetCertificate: getCertificate,
			CipherSuites:   tlsRSACipherSuites,
			MinVersion:     tls.VersionTLS12,
		}
	} else {
		config = &tls.Config{
			Certificates: []tls.Certificate{certData[0]},
			CipherSuites: tlsCipherSuites,
			MinVersion:   tls.VersionTLS12,
		}
	}
	return config
}

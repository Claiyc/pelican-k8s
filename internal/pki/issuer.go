package pki

import (
	"crypto/tls"
	"sync"
	"time"
)

// Issuer holds the CA in a process that signs certificates (the operator) and
// keeps that process's own client certificate fresh.
type Issuer struct {
	CA *CA
	// Now returns the current time; tests substitute a fixed clock.
	Now func() time.Time
	// ClientName is the common name of the process's own client certificate.
	ClientName string

	mu      sync.Mutex
	client  *tls.Certificate
	certPEM []byte
}

func (i *Issuer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// GetClientCertificate is tls.Config.GetClientCertificate: it issues the
// client certificate on first use and again when it enters its renewal window.
func (i *Issuer) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := i.now()
	if i.client != nil && !i.CA.NeedsRenewal(i.certPEM, nil, now) {
		return i.client, nil
	}
	certPEM, keyPEM, err := i.CA.Issue(i.ClientName, nil, Client, now)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	i.client, i.certPEM = &cert, certPEM
	return i.client, nil
}

// ClientConfig returns the TLS configuration of a client that trusts the CA
// and presents the issuer's own client certificate.
func (i *Issuer) ClientConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: i.CA.Pool(), GetClientCertificate: i.GetClientCertificate}
}

package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
	"time"
)

// Issuer holds the CA in a process that signs certificates (the operator) and
// keeps that process's own client certificate fresh. The CA and the trusted
// bundle can be replaced while it runs, when the CA rotates.
type Issuer struct {
	// Now returns the current time; tests substitute a fixed clock.
	Now func() time.Time
	// ClientName is the common name of the process's own client certificate.
	ClientName string

	mu       sync.Mutex
	ca       *CA
	trust    []byte
	trustSet *x509.CertPool
	client   *tls.Certificate
	certPEM  []byte
}

// NewIssuer returns an issuer signing with ca and trusting the PEM bundle
// trust; an empty bundle trusts ca alone.
func NewIssuer(ca *CA, trust []byte, clientName string) (*Issuer, error) {
	i := &Issuer{ClientName: clientName}
	if err := i.SetCA(ca, trust); err != nil {
		return nil, err
	}
	return i, nil
}

// SetCA replaces the signing CA and the trusted bundle. The client
// certificate is reissued on next use when the CA changed.
func (i *Issuer) SetCA(ca *CA, trust []byte) error {
	if len(trust) == 0 {
		trust = ca.CertPEM
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		return errors.New("trust bundle: no PEM certificate")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ca, i.trust, i.trustSet = ca, trust, pool
	return nil
}

// CA returns the signing CA.
func (i *Issuer) CA() *CA {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ca
}

// Trust returns the trusted bundle, the ca.crt of every issued certificate.
func (i *Issuer) Trust() []byte {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.trust
}

func (i *Issuer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

// GetClientCertificate is tls.Config.GetClientCertificate: it issues the
// client certificate on first use, again when it enters its renewal window
// and after the CA changed.
func (i *Issuer) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := i.now()
	if i.client != nil && !i.ca.NeedsRenewal(i.certPEM, nil, now) {
		return i.client, nil
	}
	certPEM, keyPEM, err := i.ca.Issue(i.ClientName, nil, Client, now)
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

// ClientConfig returns the TLS configuration of a client that trusts the
// current bundle and presents the issuer's own client certificate.
func (i *Issuer) ClientConfig() *tls.Config {
	i.mu.Lock()
	defer i.mu.Unlock()
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: i.trustSet, GetClientCertificate: i.GetClientCertificate}
}

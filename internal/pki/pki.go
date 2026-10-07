// Package pki is the internal certificate authority that secures traffic
// between the components (ARCHITECTURE.md 12.5): the operator keeps a CA in
// the system namespace and issues one certificate per agent and one for the
// gateway; the gateway and the operator present client certificates to agents.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"
)

// File names inside a certificate Secret and its mounted directory. They
// match the keys of a kubernetes.io/tls Secret plus the CA bundle.
const (
	CertFile = "tls.crt"
	KeyFile  = "tls.key"
	CAFile   = "ca.crt"
)

// Lifetimes. A leaf is renewed once less than RenewBefore of it remains, so a
// certificate is replaced well ahead of expiry and consumers that reload from
// disk pick it up without a restart.
const (
	CALifetime   = 10 * 365 * 24 * time.Hour
	LeafLifetime = 90 * 24 * time.Hour
	RenewBefore  = 30 * 24 * time.Hour
	// clockSkew backdates NotBefore so a peer whose clock is slightly behind
	// accepts a fresh certificate.
	clockSkew = 5 * time.Minute
)

// Common names of the client identities agents accept.
const (
	GatewayName  = "pelican-gateway"
	OperatorName = "pelican-operator"
)

// Usage selects the extended key usages of an issued certificate.
type Usage int

const (
	// Server certificates authenticate a listener (agent, gateway remote API).
	Server Usage = 1 << iota
	// Client certificates authenticate the gateway and the operator to agents.
	Client
)

// CA is the internal certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	// CertPEM is the CA certificate, the bundle every peer trusts.
	CertPEM []byte
}

// NewCA creates a self-signed CA and returns it with its PEM key.
func NewCA(now time.Time) (*CA, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pelican-k8s internal CA"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	ca, err := ParseCA(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM)
	return ca, keyPEM, err
}

// ParseCA loads a CA from its PEM certificate and key.
func ParseCA(certPEM, keyPEM []byte) (*CA, error) {
	cert, err := ParseCertificate(certPEM)
	if err != nil {
		return nil, fmt.Errorf("CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("CA certificate is not a CA")
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("CA key: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("CA key: %w", err)
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("CA key cannot sign")
	}
	return &CA{Cert: cert, Key: signer, CertPEM: certPEM}, nil
}

// Pool returns a pool holding the CA certificate.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// Issue signs a new leaf certificate for the common name and DNS names and
// returns the PEM certificate and key.
func (ca *CA) Issue(cn string, dnsNames []string, usage Usage, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}
	notAfter := now.Add(LeafLifetime)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if usage&Server != 0 {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if usage&Client != 0 {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

// NeedsRenewal reports whether a PEM leaf must be reissued: it does not parse,
// was not signed by this CA, enters its renewal window, or does not carry
// exactly the wanted DNS names.
func (ca *CA) NeedsRenewal(certPEM []byte, dnsNames []string, now time.Time) bool {
	cert, err := ParseCertificate(certPEM)
	if err != nil {
		return true
	}
	if cert.CheckSignatureFrom(ca.Cert) != nil {
		return true
	}
	if now.Add(renewBefore(cert)).After(cert.NotAfter) {
		return true
	}
	return !slices.Equal(cert.DNSNames, dnsNames)
}

// renewBefore is RenewBefore, or a third of the lifetime for a certificate
// shorter than the standard one (one cut short by the CA's own expiry).
func renewBefore(cert *x509.Certificate) time.Duration {
	life := cert.NotAfter.Sub(cert.NotBefore)
	if life < LeafLifetime {
		return life / 3
	}
	return RenewBefore
}

// ParseCertificate parses the first PEM certificate.
func ParseCertificate(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func encodeKey(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

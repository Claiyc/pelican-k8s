package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// reloadEvery bounds how often KeyPair re-reads its files. Kubernetes updates
// a mounted Secret about a minute after the Secret changes, and certificates
// are renewed weeks before they expire, so a short delay costs nothing.
const reloadEvery = 30 * time.Second

// KeyPair serves a certificate and key from files and picks up a renewed
// pair without a restart: the files are re-read at most every reloadEvery,
// and a pair that fails to load leaves the previous one in use.
type KeyPair struct {
	certPath, keyPath string
	now               func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	certPEM []byte
	keyPEM  []byte
	checked time.Time
}

// LoadKeyPair loads the certificate and key in dir (CertFile, KeyFile).
func LoadKeyPair(dir string) (*KeyPair, error) {
	kp := &KeyPair{certPath: filepath.Join(dir, CertFile), keyPath: filepath.Join(dir, KeyFile), now: time.Now}
	if _, err := kp.Certificate(); err != nil {
		return nil, err
	}
	return kp, nil
}

// Certificate returns the current certificate, reloading it when due.
func (kp *KeyPair) Certificate() (*tls.Certificate, error) {
	kp.mu.Lock()
	defer kp.mu.Unlock()
	now := kp.now()
	if kp.cert != nil && now.Sub(kp.checked) < reloadEvery {
		return kp.cert, nil
	}
	kp.checked = now
	certPEM, err := os.ReadFile(kp.certPath)
	if err != nil {
		return kp.keep(err)
	}
	keyPEM, err := os.ReadFile(kp.keyPath)
	if err != nil {
		return kp.keep(err)
	}
	if kp.cert != nil && bytes.Equal(certPEM, kp.certPEM) && bytes.Equal(keyPEM, kp.keyPEM) {
		return kp.cert, nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// The two files are swapped one after the other only with a subPath
		// mount; a whole-directory Secret mount swaps them together. Either
		// way the next check sees a matching pair.
		return kp.keep(fmt.Errorf("load %s: %w", kp.certPath, err))
	}
	kp.cert, kp.certPEM, kp.keyPEM = &cert, certPEM, keyPEM
	return kp.cert, nil
}

func (kp *KeyPair) keep(err error) (*tls.Certificate, error) {
	if kp.cert != nil {
		return kp.cert, nil
	}
	return nil, err
}

// GetCertificate is tls.Config.GetCertificate.
func (kp *KeyPair) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return kp.Certificate()
}

// GetClientCertificate is tls.Config.GetClientCertificate.
func (kp *KeyPair) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return kp.Certificate()
}

// LoadPool reads a PEM CA bundle.
func LoadPool(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, errors.New(path + ": no PEM certificate")
	}
	return p, nil
}

// Dir is a mounted certificate directory: a key pair and the CA bundle.
type Dir struct {
	KeyPair *KeyPair
	Roots   *x509.CertPool
}

// LoadDir loads a certificate directory (CertFile, KeyFile, CAFile).
func LoadDir(dir string) (*Dir, error) {
	kp, err := LoadKeyPair(dir)
	if err != nil {
		return nil, err
	}
	roots, err := LoadPool(filepath.Join(dir, CAFile))
	if err != nil {
		return nil, err
	}
	return &Dir{KeyPair: kp, Roots: roots}, nil
}

// ServerConfig returns the TLS configuration of a listener that presents
// the directory's certificate. With clientCAs set it asks for a client
// certificate and verifies one when given. The server enforces it per
// request, so that kubelet probes, which present none, still get through.
func (d *Dir) ServerConfig(verifyClients bool) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: d.KeyPair.GetCertificate}
	if verifyClients {
		c.ClientAuth = tls.VerifyClientCertIfGiven
		c.ClientCAs = d.Roots
	}
	return c
}

// ClientConfig returns the TLS configuration of a client that trusts the
// directory's CA and presents the directory's certificate.
func (d *Dir) ClientConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: d.Roots, GetClientCertificate: d.KeyPair.GetClientCertificate}
}

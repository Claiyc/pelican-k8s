package pki

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
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
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	return poolFromPEM(path, b)
}

func poolFromPEM(path string, b []byte) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, errors.New(path + ": no PEM certificate")
	}
	return p, nil
}

// Roots serves a CA bundle from a file and picks up a changed bundle the way
// KeyPair does, so a CA rotation (ARCHITECTURE.md 12.6) reaches a running
// process: the bundle holds the old and the new CA while certificates move
// from one to the other.
type Roots struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	pool    *x509.CertPool
	pem     []byte
	checked time.Time
}

// LoadRoots loads the CA bundle at path.
func LoadRoots(path string) (*Roots, error) {
	r := &Roots{path: filepath.Clean(path), now: time.Now}
	b, err := os.ReadFile(r.path)
	if err != nil {
		return nil, err
	}
	if r.pool, err = poolFromPEM(path, b); err != nil {
		return nil, err
	}
	r.pem, r.checked = b, r.now()
	return r, nil
}

// Pool returns the current bundle, reloading it when due. A bundle that
// fails to load leaves the previous one in use.
func (r *Roots) Pool() *x509.CertPool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.checked) < reloadEvery {
		return r.pool
	}
	r.checked = now
	b, err := os.ReadFile(r.path)
	if err != nil || bytes.Equal(b, r.pem) {
		return r.pool
	}
	if p, err := poolFromPEM(r.path, b); err == nil {
		r.pool, r.pem = p, b
	}
	return r.pool
}

// Dir is a mounted certificate directory: a key pair and the CA bundle.
type Dir struct {
	KeyPair *KeyPair
	Roots   *Roots
}

// LoadDir loads a certificate directory (CertFile, KeyFile, CAFile).
func LoadDir(dir string) (*Dir, error) {
	kp, err := LoadKeyPair(dir)
	if err != nil {
		return nil, err
	}
	roots, err := LoadRoots(filepath.Join(dir, CAFile))
	if err != nil {
		return nil, err
	}
	return &Dir{KeyPair: kp, Roots: roots}, nil
}

// ServerConfig returns the TLS configuration of a listener that presents
// the directory's certificate. With verifyClients it asks for a client
// certificate and verifies one when given against the current bundle. The
// server enforces it per request, so that kubelet probes, which present
// none, still get through.
func (d *Dir) ServerConfig(verifyClients bool) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: d.KeyPair.GetCertificate}
	if !verifyClients {
		return c
	}
	c.ClientAuth = tls.VerifyClientCertIfGiven
	base := c.Clone()
	c.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		cc := base.Clone()
		cc.ClientCAs = d.Roots.Pool()
		return cc, nil
	}
	return c
}

// ClientConfig returns the TLS configuration of a client that trusts the
// directory's current bundle and presents the directory's certificate. The
// bundle is the one of the moment: a long-lived client dials through
// TLSDialer(dial, d.ClientConfig) to follow it.
func (d *Dir) ClientConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: d.Roots.Pool(), GetClientCertificate: d.KeyPair.GetClientCertificate}
}

// DialFunc is the signature of net.Dialer.DialContext.
type DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

// TLSDialer returns a DialTLSContext for http.Transport (or a websocket
// dialer) that connects with dial and completes the handshake with a fresh
// config(), named after the address's host. Taking the configuration per
// connection is what lets a client follow a renewed CA bundle.
func TLSDialer(dial DialFunc, config func() *tls.Config) DialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		raw, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		c := config().Clone()
		if c.ServerName == "" {
			c.ServerName = host
		}
		conn := tls.Client(raw, c)
		if err := conn.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return conn, nil
	}
}

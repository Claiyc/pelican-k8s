package protocol

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/pki"
)

func TestListenTLS(t *testing.T) {
	ca, _, err := pki.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Issue("agent", []string{"agent.test"}, pki.Server, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen("127.0.0.1:0", []byte("secret"), &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// The token handshake runs inside TLS.
	conn, err := tls.Dial("tcp", ln.Addr(), &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: ca.Pool(), ServerName: "agent.test"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := Answer(conn, NewEncoder(conn), []byte("secret"), "pod-1", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	c, err := accept(t, ln, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if c.PodUID() != "pod-1" {
		t.Fatalf("pod UID %q", c.PodUID())
	}

	// A plain connection never completes the handshake.
	plain, err := net.Dial("tcp", ln.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := Answer(plain, NewEncoder(plain), []byte("secret"), "pod-2", time.Second); err == nil {
		t.Fatal("plain connection authenticated on a TLS listener")
	}
}

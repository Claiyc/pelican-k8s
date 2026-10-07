package supervisor

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Claiyc/pelican-k8s/internal/pki"
	"github.com/Claiyc/pelican-k8s/internal/shim/protocol"
)

// tlsAgent listens like an agent with a certificate for "localhost" and
// returns the listener, its address under that name and a file with the CA.
func tlsAgent(t *testing.T) (*protocol.Listener, string, string) {
	t.Helper()
	ca, _, err := pki.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Issue("agent", []string{"localhost"}, pki.Server, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := protocol.Listen("127.0.0.1:0", testToken, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, ca.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr())
	return ln, net.JoinHostPort("localhost", port), caFile
}

func runSupervisor(t *testing.T, o Options) {
	t.Helper()
	o.Token, o.Dir, o.RingSize, o.Stdout, o.Argv = testToken, t.TempDir(), 64, &safeBuf{}, []string{"/bin/sh", "-c", "sleep 30"}
	o.KillGrace, o.GracePeriod, o.TermLead = time.Second, 3*time.Second, 2*time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(o).Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not stop")
		}
	})
}

func TestConnectsOverTLS(t *testing.T) {
	ln, addr, caFile := tlsAgent(t)
	runSupervisor(t, Options{Agent: addr, AgentCA: caFile})
	c := acceptShim(t, ln)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != protocol.Version {
		t.Fatalf("status %+v", st)
	}
}

func TestRefusesAnUntrustedAgent(t *testing.T) {
	cases := map[string]func(t *testing.T, addr, caFile string) Options{
		// A CA bundle the agent's certificate does not chain to.
		"other CA": func(t *testing.T, addr, _ string) Options {
			other, _, err := pki.NewCA(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			f := filepath.Join(t.TempDir(), "other.crt")
			if err := os.WriteFile(f, other.CertPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			return Options{Agent: addr, AgentCA: f}
		},
		// A name the certificate does not carry.
		"other name": func(t *testing.T, addr, caFile string) Options {
			_, port, _ := net.SplitHostPort(addr)
			return Options{Agent: net.JoinHostPort("127.0.0.1", port), AgentCA: caFile}
		},
		// A bundle that cannot be read.
		"missing bundle": func(t *testing.T, addr, _ string) Options {
			return Options{Agent: addr, AgentCA: filepath.Join(t.TempDir(), "nope")}
		},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			ln, addr, caFile := tlsAgent(t)
			runSupervisor(t, opts(t, addr, caFile))
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			if c, err := ln.Accept(ctx); err == nil {
				c.Close()
				t.Fatal("the agent accepted a shim that should not trust it")
			}
		})
	}
}

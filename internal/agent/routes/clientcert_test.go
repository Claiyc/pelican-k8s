package routes

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Claiyc/pelican-k8s/internal/pki"
)

func TestRequireClientCert(t *testing.T) {
	h := RequireClientCert(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	chain := func(cn string) *tls.ConnectionState {
		return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{Subject: pkix.Name{CommonName: cn}}}}}
	}
	cases := []struct {
		name  string
		path  string
		state *tls.ConnectionState
		want  int
	}{
		{"healthz without a certificate", HealthzPath, &tls.ConnectionState{}, http.StatusTeapot},
		{"preStop without a certificate", PreStopPath, &tls.ConnectionState{}, http.StatusTeapot},
		{"api without a certificate", "/api/servers/x", &tls.ConnectionState{}, http.StatusForbidden},
		{"internal without a certificate", "/internal/v1/shim", &tls.ConnectionState{}, http.StatusForbidden},
		{"plain HTTP", "/api/servers/x", nil, http.StatusForbidden},
		{"api with the gateway's certificate", "/api/servers/x", chain(pki.GatewayName), http.StatusTeapot},
		{"api with the operator's certificate", "/api/servers/x", chain(pki.OperatorName), http.StatusTeapot},
		{"api with another certificate of the issuer", "/api/servers/x", chain("gs-other-agent"), http.StatusForbidden},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.TLS = tc.state
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

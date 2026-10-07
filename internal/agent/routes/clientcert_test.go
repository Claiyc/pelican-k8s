package routes

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireClientCert(t *testing.T) {
	h := RequireClientCert(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	verified := &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
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
		{"api with a verified certificate", "/api/servers/x", verified, http.StatusTeapot},
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

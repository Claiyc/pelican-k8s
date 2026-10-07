package pki

import (
	"context"
	"net"
	"net/netip"
	"strings"

	"github.com/Claiyc/pelican-k8s/internal/operator/names"
)

// AgentDNSNames returns the names an agent certificate carries: the agent
// Service in its short and namespaced forms (the shim dials the short one),
// and a wildcard under it for AgentHost.
func AgentDNSNames(uuid, namespace string) []string {
	svc := names.AgentService(uuid)
	return []string{
		svc,
		svc + "." + namespace,
		svc + "." + namespace + ".svc",
		"*." + svc + "." + namespace + ".svc",
	}
}

// AgentHost returns the host name the gateway and the operator use for the
// agent at pod address ip. Callers dial the pod address directly (the agent
// Service would hide which pod answers), but TLS needs a name: the pod
// address is encoded as the first label, under the agent Service name, so
// the certificate's wildcard covers it, a pod address alone verifies nothing
// about the server, and connection pools never share a connection between two
// pods or two servers. AgentDialer turns the name back into the address.
func AgentHost(ip, uuid, namespace string) string {
	return encodeIP(ip) + "." + names.AgentService(uuid) + "." + namespace + ".svc"
}

// AgentDialer wraps dial so that host names made by AgentHost connect to the
// pod address they carry. Any other address is passed through unchanged.
func AgentDialer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			if ip, ok := agentHostIP(host); ok {
				addr = net.JoinHostPort(ip, port)
			}
		}
		return dial(ctx, network, addr)
	}
}

func agentHostIP(host string) (string, bool) {
	label, rest, ok := strings.Cut(host, ".")
	if !ok || !strings.HasPrefix(rest, names.Prefix) || !strings.HasSuffix(rest, ".svc") {
		return "", false
	}
	return decodeIP(label)
}

// encodeIP turns a pod address into a DNS label: dots and colons become
// dashes ("10.0.0.5" is "10-0-0-5", "fd00::5" is "fd00--5").
func encodeIP(ip string) string {
	return strings.NewReplacer(".", "-", ":", "-").Replace(ip)
}

func decodeIP(label string) (string, bool) {
	if a, err := netip.ParseAddr(strings.ReplaceAll(label, "-", ".")); err == nil && a.Is4() {
		return a.String(), true
	}
	if a, err := netip.ParseAddr(strings.ReplaceAll(label, "-", ":")); err == nil && a.Is6() {
		return a.String(), true
	}
	return "", false
}

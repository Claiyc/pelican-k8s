// Package metallb discovers the addresses MetalLB is configured to hand out,
// so the Panel's allocation form offers IPs that MetalLB will actually
// announce. An allocation on any other address produces a Service that never
// gets an ingress IP, which is only visible as a pending LoadBalancer.
package metallb

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PoolGVK is the list kind of MetalLB's address pools.
var PoolGVK = schema.GroupVersionKind{Group: "metallb.io", Version: "v1beta1", Kind: "IPAddressPoolList"}

// DefaultMax caps how many addresses a pool contributes. The Panel renders
// them as a dropdown, so a large pool would be unusable, and MetalLB happily
// accepts a /16.
const DefaultMax = 256

// Pools lists the addresses of MetalLB's IPAddressPool objects.
type Pools struct {
	// Reader must be uncached: the CRD may not be installed, and a cache would
	// try to start an informer for a kind that does not exist.
	Reader client.Reader
	// Names filters by pool name. Empty means every pool.
	Names []string
	// Max caps the number of addresses returned. Zero means DefaultMax.
	Max int
	// TTL bounds how often the API server is asked. Zero means one minute.
	TTL time.Duration
	Log *slog.Logger

	mu     sync.Mutex
	cached []string
	at     time.Time
	warned bool
}

// Addresses returns the pool addresses, or nil when MetalLB is not installed,
// the gateway may not read the pools, or no pool matches. Discovery is a
// convenience, so every failure degrades to the caller's other sources.
func (p *Pools) Addresses(ctx context.Context) []string {
	ttl := p.TTL
	if ttl == 0 {
		ttl = time.Minute
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && time.Since(p.at) < ttl {
		return p.cached
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(PoolGVK)
	if err := p.Reader.List(ctx, list); err != nil {
		// No CRD or no RBAC is a deployment choice, not an incident: say so
		// once and keep serving the other sources.
		if !p.warned && p.Log != nil {
			p.Log.Info("metallb pool discovery unavailable", "err", err)
			p.warned = true
		}
		p.cached, p.at = nil, time.Now()
		return nil
	}
	p.warned = false

	max := p.Max
	if max <= 0 {
		max = DefaultMax
	}
	var out []string
	seen := map[string]bool{}
	for i := range list.Items {
		item := &list.Items[i]
		if !p.wanted(item.GetName()) {
			continue
		}
		entries, _, _ := unstructured.NestedStringSlice(item.Object, "spec", "addresses")
		avoidBuggy, _, _ := unstructured.NestedBool(item.Object, "spec", "avoidBuggyIPs")
		for _, e := range entries {
			for _, a := range expand(e, avoidBuggy, max-len(out)) {
				if !seen[a] {
					seen[a] = true
					out = append(out, a)
				}
			}
			if len(out) >= max {
				break
			}
		}
		if len(out) >= max {
			if p.Log != nil {
				p.Log.Warn("metallb pool discovery truncated", "max", max)
			}
			break
		}
	}
	p.cached, p.at = out, time.Now()
	return out
}

func (p *Pools) wanted(name string) bool {
	if len(p.Names) == 0 {
		return true
	}
	for _, n := range p.Names {
		if n == name {
			return true
		}
	}
	return false
}

// expand turns one MetalLB address entry into addresses. An entry is a CIDR
// ("192.0.2.0/28"), an inclusive range ("192.0.2.10-192.0.2.20") or a single
// address. At most limit addresses are returned.
func expand(entry string, avoidBuggy bool, limit int) []string {
	entry = strings.TrimSpace(entry)
	if limit <= 0 || entry == "" {
		return nil
	}
	switch {
	case strings.Contains(entry, "/"):
		pre, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil
		}
		first := pre.Masked().Addr()
		last := lastInPrefix(pre)
		// MetalLB skips the network and broadcast address of an IPv4 prefix
		// only when asked to; by default it assigns them.
		if avoidBuggy && pre.Addr().Is4() && pre.Bits() < 31 {
			first, last = first.Next(), prev(last)
		}
		return rangeOf(first, last, limit)
	case strings.Contains(entry, "-"):
		lo, hi, ok := strings.Cut(entry, "-")
		if !ok {
			return nil
		}
		first, err := netip.ParseAddr(strings.TrimSpace(lo))
		if err != nil {
			return nil
		}
		last, err := netip.ParseAddr(strings.TrimSpace(hi))
		if err != nil {
			return nil
		}
		return rangeOf(first, last, limit)
	default:
		a, err := netip.ParseAddr(entry)
		if err != nil {
			return nil
		}
		return []string{a.String()}
	}
}

func rangeOf(first, last netip.Addr, limit int) []string {
	if !first.IsValid() || !last.IsValid() || first.Compare(last) > 0 || first.Is4() != last.Is4() {
		return nil
	}
	var out []string
	for a := first; ; a = a.Next() {
		out = append(out, a.String())
		if a.Compare(last) == 0 || len(out) >= limit {
			break
		}
	}
	return out
}

func lastInPrefix(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	bits := p.Bits()
	for i := range b {
		host := (i+1)*8 - bits
		switch {
		case host >= 8:
			b[i] = 0xff
		case host > 0:
			b[i] |= byte(0xff >> (8 - host))
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

func prev(a netip.Addr) netip.Addr {
	b := a.AsSlice()
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] > 0 {
			b[i]--
			break
		}
		b[i] = 0xff
	}
	out, _ := netip.AddrFromSlice(b)
	return out
}

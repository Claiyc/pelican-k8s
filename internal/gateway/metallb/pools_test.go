package metallb

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExpand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entry      string
		avoidBuggy bool
		limit      int
		want       []string
	}{
		{name: "cidr", entry: "192.0.2.0/29", limit: 100,
			want: []string{"192.0.2.0", "192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4", "192.0.2.5", "192.0.2.6", "192.0.2.7"}},
		{name: "cidr avoids network and broadcast", entry: "192.0.2.0/29", avoidBuggy: true, limit: 100,
			want: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4", "192.0.2.5", "192.0.2.6"}},
		{name: "cidr not on a byte boundary", entry: "192.0.2.16/28", limit: 100,
			want: append([]string{}, seq("192.0.2.", 16, 31)...)},
		{name: "single host cidr", entry: "192.0.2.7/32", limit: 100, want: []string{"192.0.2.7"}},
		{name: "point to point keeps both", entry: "192.0.2.8/31", avoidBuggy: true, limit: 100,
			want: []string{"192.0.2.8", "192.0.2.9"}},
		{name: "range", entry: "192.0.2.10-192.0.2.13", limit: 100,
			want: []string{"192.0.2.10", "192.0.2.11", "192.0.2.12", "192.0.2.13"}},
		{name: "range with spaces", entry: " 192.0.2.10 - 192.0.2.11 ", limit: 100,
			want: []string{"192.0.2.10", "192.0.2.11"}},
		{name: "range crossing an octet", entry: "192.0.2.254-192.0.3.1", limit: 100,
			want: []string{"192.0.2.254", "192.0.2.255", "192.0.3.0", "192.0.3.1"}},
		{name: "single address", entry: "192.0.2.5", limit: 100, want: []string{"192.0.2.5"}},
		{name: "limit truncates", entry: "192.0.2.0/24", limit: 3,
			want: []string{"192.0.2.0", "192.0.2.1", "192.0.2.2"}},
		{name: "zero limit", entry: "192.0.2.0/24", limit: 0, want: nil},
		{name: "reversed range", entry: "192.0.2.20-192.0.2.10", limit: 100, want: nil},
		{name: "mixed family range", entry: "192.0.2.1-2001:db8::1", limit: 100, want: nil},
		{name: "garbage", entry: "not-an-address", limit: 100, want: nil},
		{name: "empty", entry: "", limit: 100, want: nil},
		{name: "ipv6 range", entry: "2001:db8::1-2001:db8::3", limit: 100,
			want: []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := expand(tc.entry, tc.avoidBuggy, tc.limit)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("expand(%q) = %v, want %v", tc.entry, got, tc.want)
			}
		})
	}
}

// A /16 must not turn the Panel's allocation dropdown into 65k entries.
func TestExpandCapsLargePrefix(t *testing.T) {
	if got := expand("10.0.0.0/16", false, DefaultMax); len(got) != DefaultMax {
		t.Fatalf("got %d addresses, want %d", len(got), DefaultMax)
	}
}

type listFunc func(list *unstructured.UnstructuredList) error

func (f listFunc) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	return f(list.(*unstructured.UnstructuredList))
}

func (f listFunc) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("not implemented")
}

func pool(name string, addresses ...string) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]any{}}
	u.SetName(name)
	addrs := make([]any, 0, len(addresses))
	for _, a := range addresses {
		addrs = append(addrs, a)
	}
	_ = unstructured.SetNestedSlice(u.Object, addrs, "spec", "addresses")
	return u
}

func reader(items ...unstructured.Unstructured) listFunc {
	return func(list *unstructured.UnstructuredList) error {
		list.Items = items
		return nil
	}
}

func TestAddressesAcrossPools(t *testing.T) {
	p := &Pools{Reader: reader(
		pool("games", "192.0.2.10-192.0.2.11"),
		pool("other", "192.0.2.20"),
	)}
	got := p.Addresses(context.Background())
	want := []string{"192.0.2.10", "192.0.2.11", "192.0.2.20"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAddressesFiltersByName(t *testing.T) {
	p := &Pools{Names: []string{"games"}, Reader: reader(
		pool("games", "192.0.2.10"),
		pool("other", "192.0.2.20"),
	)}
	if got := p.Addresses(context.Background()); strings.Join(got, ",") != "192.0.2.10" {
		t.Fatalf("got %v", got)
	}
}

func TestAddressesDeduplicates(t *testing.T) {
	p := &Pools{Reader: reader(
		pool("a", "192.0.2.10", "192.0.2.10-192.0.2.11"),
		pool("b", "192.0.2.11"),
	)}
	got := p.Addresses(context.Background())
	if strings.Join(got, ",") != "192.0.2.10,192.0.2.11" {
		t.Fatalf("got %v", got)
	}
}

// A missing CRD or a denied list is a deployment choice, not an error: the
// caller falls back to its other sources.
func TestAddressesTolerateNoMetalLB(t *testing.T) {
	p := &Pools{Reader: listFunc(func(*unstructured.UnstructuredList) error {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "metallb.io", Kind: "IPAddressPool"}}
	})}
	if got := p.Addresses(context.Background()); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestAddressesAreCached(t *testing.T) {
	calls := 0
	p := &Pools{Reader: listFunc(func(list *unstructured.UnstructuredList) error {
		calls++
		list.Items = []unstructured.Unstructured{pool("games", "192.0.2.10")}
		return nil
	})}
	for i := 0; i < 5; i++ {
		p.Addresses(context.Background())
	}
	if calls != 1 {
		t.Fatalf("listed %d times, want 1", calls)
	}
}

func TestAddressesRequestTheRightKind(t *testing.T) {
	var gvk string
	p := &Pools{Reader: listFunc(func(list *unstructured.UnstructuredList) error {
		gvk = list.GroupVersionKind().String()
		return nil
	})}
	p.Addresses(context.Background())
	if gvk != PoolGVK.String() {
		t.Fatalf("listed %s, want %s", gvk, PoolGVK)
	}
}

func seq(prefix string, lo, hi int) []string {
	var out []string
	for i := lo; i <= hi; i++ {
		out = append(out, prefix+strconv.Itoa(i))
	}
	return out
}

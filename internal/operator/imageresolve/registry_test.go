package imageresolve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// push stores a random image with the given entrypoint/cmd in an in-memory
// registry and returns the registry host and the image digest. The counter
// counts requests made to the registry.
func push(t *testing.T, entrypoint, cmd []string) (host, digest string, hits *atomic.Int32) {
	t.Helper()
	hits = new(atomic.Int32)
	reg := registry.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Config(img, v1.Config{Entrypoint: entrypoint, Cmd: cmd})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := name.ParseReference(u.Host+"/yolks:java", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return u.Host, d.String(), hits
}

func TestDigestAndEntrypoint(t *testing.T) {
	host, digest, hits := push(t, []string{"/tini", "--"}, []string{"/entry.sh"})
	r := NewResolver(authn.DefaultKeychain)
	ctx := context.Background()
	image := host + "/yolks:java"

	got, err := r.Digest(ctx, image)
	if err != nil || got != digest {
		t.Fatalf("Digest = %q, %v; want %q", got, err, digest)
	}
	n := hits.Load()
	if _, err := r.Digest(ctx, image); err != nil || hits.Load() != n {
		t.Fatalf("second Digest must be served from cache (hits %d -> %d, err %v)", n, hits.Load(), err)
	}

	argv, err := r.Entrypoint(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv, " ") != "/tini -- /entry.sh" {
		t.Fatalf("argv = %v", argv)
	}
	n = hits.Load()
	if _, err := r.Entrypoint(ctx, image); err != nil || hits.Load() != n {
		t.Fatalf("second Entrypoint must be cached (err %v)", err)
	}
	if c := r.cache[image]; c.digest != digest {
		t.Fatalf("cached digest %q", c.digest)
	}
}

func TestDigestPassthrough(t *testing.T) {
	r := NewResolver(nil)
	got, err := r.Digest(context.Background(), "example.invalid/x@sha256:abc")
	if err != nil || got != "sha256:abc" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestEntrypointEmpty(t *testing.T) {
	host, _, _ := push(t, nil, nil)
	r := NewResolver(authn.DefaultKeychain)
	if _, err := r.Entrypoint(context.Background(), host+"/yolks:java"); err == nil || !strings.Contains(err.Error(), "no entrypoint") {
		t.Fatalf("err = %v", err)
	}
}

func TestLookupErrors(t *testing.T) {
	r := NewResolver(authn.DefaultKeychain)
	ctx := context.Background()
	if _, err := r.Digest(ctx, "NOT A REF"); err == nil {
		t.Fatal("invalid reference must error")
	}

	host, _, hits := push(t, []string{"/x"}, nil)
	missing := host + "/yolks:missing"
	if _, err := r.Digest(ctx, missing); err == nil {
		t.Fatal("missing tag must error (HEAD)")
	}
	n := hits.Load()
	if _, err := r.Digest(ctx, missing); err == nil || hits.Load() != n {
		t.Fatal("errors are cached for the TTL")
	}
	if _, err := r.Entrypoint(ctx, host+"/yolks:missing2"); err == nil {
		t.Fatal("missing tag must error (GET)")
	}
}

func TestCacheExpiry(t *testing.T) {
	host, _, hits := push(t, []string{"/x"}, nil)
	r := NewResolver(authn.DefaultKeychain)
	r.TTL = time.Nanosecond
	ctx := context.Background()
	image := host + "/yolks:java"
	if _, err := r.Digest(ctx, image); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	n := hits.Load()
	if _, err := r.Digest(ctx, image); err != nil || hits.Load() == n {
		t.Fatalf("expired entry must be refetched (err %v)", err)
	}
}

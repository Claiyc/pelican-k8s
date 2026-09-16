// Package imageresolve resolves the argv and digest of an egg image
// (ARCHITECTURE.md 7.5, "Image entrypoint resolution").
package imageresolve

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// MatchOverride returns the argv of the most specific override matching the
// image reference: an exact match wins, then the longest matching pattern.
// Globs use path.Match semantics against the full reference; a trailing "*"
// matches any tag, and patterns without a tag part match every tag of the
// repository.
func MatchOverride(image string, overrides map[string][]string) ([]string, bool) {
	if argv, ok := overrides[image]; ok {
		return argv, true
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	for _, pattern := range keys {
		if globMatch(pattern, image) {
			return overrides[pattern], true
		}
	}
	return nil, false
}

func globMatch(pattern, image string) bool {
	if ok, _ := path.Match(pattern, image); ok {
		return true
	}
	// "*" in path.Match does not cross "/", so also try a segment-insensitive match.
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(image, strings.TrimSuffix(pattern, "*"))
	}
	if !strings.Contains(pattern, ":") && !strings.Contains(pattern, "@") {
		repo := image
		if i := strings.LastIndex(repo, "@"); i >= 0 {
			repo = repo[:i]
		}
		if i := strings.LastIndex(repo, ":"); i >= 0 && !strings.Contains(repo[i:], "/") {
			repo = repo[:i]
		}
		return repo == pattern
	}
	return false
}

// Resolver resolves image digests and, optionally, the image config from the
// registry. Results are cached briefly so reconcile loops do not hammer the
// registry.
type Resolver struct {
	Keychain authn.Keychain
	TTL      time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	digest string
	argv   []string
	at     time.Time
	err    error
}

// NewResolver returns a resolver using the default keychain (plus the given
// one, e.g. Kubernetes pull secrets).
func NewResolver(keychain authn.Keychain) *Resolver {
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	return &Resolver{Keychain: keychain, TTL: 5 * time.Minute, cache: map[string]cached{}}
}

// Digest returns the "sha256:..." digest of the image reference. A reference
// that already carries a digest is returned unchanged.
func (r *Resolver) Digest(ctx context.Context, image string) (string, error) {
	if i := strings.Index(image, "@sha256:"); i >= 0 {
		return image[i+1:], nil
	}
	c, err := r.lookup(ctx, image, false)
	if err != nil {
		return "", err
	}
	return c.digest, nil
}

// Entrypoint returns the image's ENTRYPOINT+CMD from its config.
func (r *Resolver) Entrypoint(ctx context.Context, image string) ([]string, error) {
	c, err := r.lookup(ctx, image, true)
	if err != nil {
		return nil, err
	}
	if len(c.argv) == 0 {
		return nil, fmt.Errorf("image %s has no entrypoint or cmd", image)
	}
	return c.argv, nil
}

func (r *Resolver) lookup(ctx context.Context, image string, needConfig bool) (cached, error) {
	r.mu.Lock()
	if c, ok := r.cache[image]; ok && time.Since(c.at) < r.TTL && (!needConfig || c.argv != nil || c.err != nil) {
		r.mu.Unlock()
		return c, c.err
	}
	r.mu.Unlock()

	ref, err := name.ParseReference(image)
	if err != nil {
		return cached{}, err
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(r.Keychain)}
	c := cached{at: time.Now()}
	if needConfig {
		img, err := remote.Image(ref, opts...)
		if err != nil {
			c.err = err
		} else {
			if d, err := img.Digest(); err == nil {
				c.digest = d.String()
			}
			cfg, err := img.ConfigFile()
			if err != nil {
				c.err = err
			} else {
				c.argv = append(append([]string(nil), cfg.Config.Entrypoint...), cfg.Config.Cmd...)
			}
		}
	} else {
		desc, err := remote.Head(ref, opts...)
		if err != nil {
			c.err = err
		} else {
			c.digest = desc.Digest.String()
		}
	}
	r.mu.Lock()
	r.cache[image] = c
	r.mu.Unlock()
	return c, c.err
}

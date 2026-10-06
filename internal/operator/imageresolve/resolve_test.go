package imageresolve

import "testing"

func TestMatchOverride(t *testing.T) {
	overrides := map[string][]string{
		"ghcr.io/pelican-eggs/yolks:*":      {"/usr/bin/tini", "-g", "--", "/entrypoint.sh"},
		"ghcr.io/pelican-eggs/yolks:java_8": {"/bin/bash", "/entrypoint.sh"},
		"docker.io/library/alpine":          {"/bin/sh"},
	}
	cases := map[string]string{
		"ghcr.io/pelican-eggs/yolks:java_21":  "/usr/bin/tini",
		"ghcr.io/pelican-eggs/yolks:java_8":   "/bin/bash", // exact match beats the glob
		"docker.io/library/alpine:3.20":       "/bin/sh",
		"docker.io/library/alpine@sha256:abc": "/bin/sh",
		"ghcr.io/pelican-eggs/games:source":   "",
		"quay.io/pelican-eggs/yolks:java_21":  "",
	}
	for image, want := range cases {
		argv, ok := MatchOverride(image, overrides)
		if want == "" {
			if ok {
				t.Errorf("%s matched %v", image, argv)
			}
			continue
		}
		if !ok || argv[0] != want {
			t.Errorf("%s: got %v ok=%v want %s", image, argv, ok, want)
		}
	}
	if _, ok := MatchOverride("x", nil); ok {
		t.Fatal("nil overrides")
	}
}

func TestMatchOverrideSpecificity(t *testing.T) {
	overrides := map[string][]string{
		"reg/a:*":     {"short"},
		"reg/a:v1*":   {"long"},
		"reg/b:tag":   {"exact-pattern"},
		"reg/c":       {"repo"},
		"reg/d@sha*":  {"digest"},
		"reg:5000/e":  {"port"},
		"reg/f/*:1.0": {"never"},
	}
	cases := map[string]string{
		"reg/a:v12":              "long", // longest pattern wins
		"reg/a:v2":               "short",
		"reg/b:tag":              "exact-pattern",
		"reg/c:latest":           "repo",
		"reg/c@sha256:abc":       "repo",
		"reg/d@sha256:abc":       "digest",
		"reg:5000/e":             "port",
		"reg:5000/e:1":           "port", // registry port is not a tag
		"reg/b:other":            "",
		"reg/cc:latest":          "",
		"reg/f/x:1.0":            "never", // path.Match: * matches one segment
		"reg:5000/other/e:1.0.1": "",
	}
	for image, want := range cases {
		argv, ok := MatchOverride(image, overrides)
		if want == "" {
			if ok {
				t.Errorf("%s matched %v", image, argv)
			}
			continue
		}
		if !ok || argv[0] != want {
			t.Errorf("%s: got %v ok=%v want %s", image, argv, ok, want)
		}
	}
}

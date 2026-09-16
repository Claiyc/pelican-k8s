package imageresolve

import "testing"

func TestMatchOverride(t *testing.T) {
	overrides := map[string][]string{
		"ghcr.io/pelican-eggs/yolks:*":       {"/usr/bin/tini", "-g", "--", "/entrypoint.sh"},
		"ghcr.io/pelican-eggs/yolks:java_8": {"/bin/bash", "/entrypoint.sh"},
		"docker.io/library/alpine":          {"/bin/sh"},
	}
	cases := map[string]string{
		"ghcr.io/pelican-eggs/yolks:java_21":              "/usr/bin/tini",
		"ghcr.io/pelican-eggs/yolks:java_8":               "/bin/bash", // exact match beats the glob
		"docker.io/library/alpine:3.20":                   "/bin/sh",
		"docker.io/library/alpine@sha256:abc":             "/bin/sh",
		"ghcr.io/pelican-eggs/games:source":               "",
		"quay.io/pelican-eggs/yolks:java_21":              "",
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

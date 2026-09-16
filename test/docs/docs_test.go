// Package docs checks that the documentation does not drift from the tree it
// documents. The install commands people copy out of the README are the most
// expensive kind of stale: a wrong --version fails at the first step of the
// quick start, before a reader has any context to debug it.
package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const repoRoot = "../.."

// chartRef matches an OCI chart reference and, when the same command line
// carries one, the --version pinned next to it.
var (
	chartRef   = regexp.MustCompile(`oci://ghcr\.io/[^/\s]+/pelican-k8s/charts/([A-Za-z0-9._-]+)`)
	versionArg = regexp.MustCompile(`--version\s+(\S+)`)
	// targetRevision in the Argo CD example pins the same release.
	targetRev = regexp.MustCompile(`targetRevision:\s*v?([0-9][^\s#]*)`)
)

// chartVersions reads the version each chart would be released at.
func chartVersions(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	dirs, err := filepath.Glob(filepath.Join(repoRoot, "charts", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "Chart.yaml"))
		if err != nil {
			continue
		}
		var c struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		}
		if err := yaml.Unmarshal(b, &c); err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if c.Name == "" || c.Version == "" {
			t.Fatalf("%s/Chart.yaml: name or version missing", d)
		}
		out[c.Name] = c.Version
	}
	if len(out) == 0 {
		t.Fatal("no charts found; the check is not meaningful")
	}
	return out
}

// markdownFiles is every document a reader might copy a command out of.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pat := range []string{"*.md", "docs/*.md"} {
		m, err := filepath.Glob(filepath.Join(repoRoot, pat))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no markdown found; the check is not meaningful")
	}
	return files
}

// TestInstallCommandsPinTheChartVersion fails when a documented `helm install`
// names a version the tree does not release. Bumping a chart therefore has to
// bump the docs in the same change.
func TestInstallCommandsPinTheChartVersion(t *testing.T) {
	versions := chartVersions(t)
	checked := 0
	for _, f := range markdownFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			ref := chartRef.FindStringSubmatch(line)
			if ref == nil {
				continue
			}
			// Prose mentions the charts without installing them.
			ver := versionArg.FindStringSubmatch(line)
			if ver == nil {
				continue
			}
			chart, documented := ref[1], ver[1]
			want, known := versions[chart]
			if !known {
				t.Errorf("%s:%d: references chart %q, which is not in charts/", f, i+1, chart)
				continue
			}
			checked++
			if documented != want {
				t.Errorf("%s:%d: installs %s --version %s, but charts/%s/Chart.yaml releases %s",
					f, i+1, chart, documented, chart, want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no versioned install commands found; the matcher is broken")
	}
	t.Logf("checked %d install commands against %d charts", checked, len(versions))
}

// TestExampleRevisionsMatchTheChart covers the Argo CD example, which pins the
// same release through targetRevision rather than --version.
func TestExampleRevisionsMatchTheChart(t *testing.T) {
	want, ok := chartVersions(t)["pelican-k8s"]
	if !ok {
		t.Fatal("charts/pelican-k8s not found")
	}
	checked := 0
	for _, f := range markdownFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			m := targetRev.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			checked++
			if m[1] != want {
				t.Errorf("%s:%d: targetRevision pins %s, but charts/pelican-k8s/Chart.yaml releases %s",
					f, i+1, m[1], want)
			}
		}
	}
	if checked == 0 {
		t.Skip("no targetRevision examples in the docs")
	}
}

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

// docsPins is the version the install docs should pin for each chart: the
// chart's version, except while charts/pelican-k8s is at a pre-release
// (X.Y.Z-beta1). Then they stay on the last release, the newest CHANGELOG.md
// section without a suffix: readers install releases, not betas.
func docsPins(t *testing.T) (pins, versions map[string]string) {
	t.Helper()
	versions = chartVersions(t)
	pins = map[string]string{}
	for chart, v := range versions {
		pins[chart] = v
	}
	if v := versions["pelican-k8s"]; strings.Contains(v, "-") {
		b, err := os.ReadFile(filepath.Join(repoRoot, "CHANGELOG.md"))
		if err != nil {
			t.Fatal(err)
		}
		last := lastRelease(string(b))
		if last == "" {
			t.Fatalf("charts/pelican-k8s/Chart.yaml is at the pre-release %s, but CHANGELOG.md has no release section to pin instead", v)
		}
		pins["pelican-k8s"] = last
	}
	return pins, versions
}

// releaseHeading matches a release's CHANGELOG.md section, not a pre-release's.
var releaseHeading = regexp.MustCompile(`(?m)^## \[([0-9]+\.[0-9]+\.[0-9]+)\]`)

// lastRelease is the newest release section in a CHANGELOG.md, or "".
func lastRelease(changelog string) string {
	if m := releaseHeading.FindStringSubmatch(changelog); m != nil {
		return m[1]
	}
	return ""
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
// bump the docs in the same change, unless it bumps to a pre-release.
func TestInstallCommandsPinTheChartVersion(t *testing.T) {
	pins, versions := docsPins(t)
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
			want, known := pins[chart]
			if !known {
				t.Errorf("%s:%d: references chart %q, which is not in charts/", f, i+1, chart)
				continue
			}
			checked++
			if documented != want {
				t.Errorf("%s:%d: installs %s --version %s, but should pin %s (charts/%s/Chart.yaml is at %s)",
					f, i+1, chart, documented, want, chart, versions[chart])
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
	pins, versions := docsPins(t)
	want, ok := pins["pelican-k8s"]
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
				t.Errorf("%s:%d: targetRevision pins %s, but should pin %s (charts/pelican-k8s/Chart.yaml is at %s)",
					f, i+1, m[1], want, versions["pelican-k8s"])
			}
		}
	}
	if checked == 0 {
		t.Skip("no targetRevision examples in the docs")
	}
}

func TestLastRelease(t *testing.T) {
	for _, c := range []struct{ changelog, want string }{
		{"## [Unreleased]\n\n## [2.0.0-beta2] - x\n\n## [2.0.0-beta1] - x\n\n## [1.1.0] - x\n\n## [1.0.2] - x\n", "1.1.0"},
		{"## [Unreleased]\n\n## [1.2.0] - x\n\n## [1.2.0-beta1] - x\n", "1.2.0"},
		{"## [Unreleased]\n\n## [1.0.0-beta1] - x\n", ""},
	} {
		if got := lastRelease(c.changelog); got != c.want {
			t.Errorf("lastRelease(%q) = %q, want %q", c.changelog, got, c.want)
		}
	}
}

// TestChartAppVersionIsTheRelease covers installs from a checkout or a git tag:
// those take image.tag from appVersion, so an appVersion left behind deploys
// the previous release's images under the new tag.
func TestChartAppVersionIsTheRelease(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "charts", "pelican-k8s", "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Version    string `yaml:"version"`
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.AppVersion != c.Version {
		t.Errorf("charts/pelican-k8s/Chart.yaml: appVersion %q differs from version %q; the images are released at the chart version",
			c.AppVersion, c.Version)
	}
}

// TestChangelogHasTheRelease fails when the chart version has no CHANGELOG
// section: the release workflow takes the release notes from it.
func TestChangelogHasTheRelease(t *testing.T) {
	want, ok := chartVersions(t)["pelican-k8s"]
	if !ok {
		t.Fatal("charts/pelican-k8s not found")
	}
	b, err := os.ReadFile(filepath.Join(repoRoot, "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\n## ["+want+"]") {
		t.Errorf("CHANGELOG.md has no \"## [%s]\" section for the version charts/pelican-k8s/Chart.yaml releases", want)
	}
	if !strings.Contains(string(b), "\n["+want+"]: ") {
		t.Errorf("CHANGELOG.md has no [%s] link reference", want)
	}
}

// TestChartReadmesStateTheChartVersion covers the version tables in the chart
// READMEs and the Panel guide, which are not install commands.
func TestChartReadmesStateTheChartVersion(t *testing.T) {
	versions := chartVersions(t)
	stated := regexp.MustCompile("Chart version \\| `([^`]+)`|name ([a-z0-9-]+), version (\\S+),")
	files := []string{"docs/panel.md"}
	m, err := filepath.Glob(filepath.Join(repoRoot, "charts", "*", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m {
		rel, _ := filepath.Rel(repoRoot, f)
		files = append(files, rel)
	}
	checked := 0
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(repoRoot, f))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			s := stated.FindStringSubmatch(line)
			if s == nil {
				continue
			}
			chart, documented := s[2], s[3]
			if s[1] != "" {
				// A chart README states its own version.
				chart, documented = filepath.Base(filepath.Dir(f)), s[1]
			}
			want, known := versions[chart]
			if !known {
				t.Errorf("%s:%d: states a version for chart %q, which is not in charts/", f, i+1, chart)
				continue
			}
			checked++
			if documented != want {
				t.Errorf("%s:%d: states %s version %s, but charts/%s/Chart.yaml releases %s",
					f, i+1, chart, documented, chart, want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no stated chart versions found; the matcher is broken")
	}
}

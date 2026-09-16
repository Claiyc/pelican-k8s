// Package supplychain guards the build inputs that OpenSSF Scorecard reports
// as code scanning alerts: every GitHub Action must be pinned to a commit SHA,
// every container base image to a digest, and no workflow may hand a write
// token to jobs that do not need one (docs/development.md, "Dependencies,
// security and quality").
package supplychain

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	workflowDir = "../../.github/workflows"
	buildDir    = "../../build"
)

// usesRef matches the action reference and the trailing version comment of a
// `uses:` line: "owner/repo/sub@ref  # v1.2.3".
var usesRef = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(\S+)\s*(?:#\s*(\S+))?\s*$`)

// commitSHA is a full 40-character git object name; Scorecard accepts nothing
// shorter, and neither does this test.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func workflowFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(workflowDir, "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no workflows under %s", workflowDir)
	}
	return files
}

// TestActionsArePinnedToACommitSHA keeps the Scorecard Pinned-Dependencies
// finding closed: a floating tag (@v4) can be repointed at any commit by
// whoever owns the action.
func TestActionsArePinnedToACommitSHA(t *testing.T) {
	seen := 0
	for _, f := range workflowFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			m := usesRef.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			ref, comment := m[1], m[2]
			// Local composite actions and reusable workflows in this repository
			// are already pinned by being in the tree.
			if strings.HasPrefix(ref, "./") {
				continue
			}
			seen++
			where := fmt.Sprintf("%s:%d", f, i+1)
			at := strings.LastIndex(ref, "@")
			if at < 0 {
				t.Errorf("%s: %s has no ref; pin it to a commit SHA", where, ref)
				continue
			}
			if !commitSHA.MatchString(ref[at+1:]) {
				t.Errorf("%s: %s is not pinned to a 40-character commit SHA", where, ref)
				continue
			}
			// The comment is what makes a SHA pin reviewable, and it is what
			// Dependabot rewrites when it bumps the action.
			if comment == "" {
				t.Errorf("%s: %s is pinned but carries no version comment (want `# v1.2.3`)", where, ref)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no external action references found; the matcher is broken")
	}
	t.Logf("checked %d action references", seen)
}

// fromRef extracts the image reference and the stage name of a Dockerfile FROM
// instruction, skipping flags such as --platform=$BUILDPLATFORM.
func fromRef(line string) (image, stage string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
		return "", "", false
	}
	rest := fields[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return "", "", false
	}
	image = rest[0]
	if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
		stage = rest[2]
	}
	return image, stage, true
}

// TestBaseImagesArePinnedToADigest keeps the other half of Pinned-Dependencies
// closed: a mutable tag means two builds of the same commit can differ.
func TestBaseImagesArePinnedToADigest(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(buildDir, "*.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no Dockerfiles under %s", buildDir)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		stages := map[string]bool{}
		for i, line := range strings.Split(string(b), "\n") {
			image, stage, ok := fromRef(line)
			if !ok {
				continue
			}
			if stage != "" {
				stages[stage] = true
			}
			switch {
			case image == "scratch", stages[image]:
				// The empty image and earlier stages of this same file carry
				// no registry content to pin.
			case !strings.Contains(image, "@sha256:"):
				t.Errorf("%s:%d: %s is not pinned to a digest", f, i+1, image)
			}
		}
	}
}

// workflow is the slice of the schema this test cares about. permissions is
// either a mapping of scope to level or one of the shorthand strings, so it is
// decoded loosely.
type workflow struct {
	Permissions yaml.Node `yaml:"permissions"`
	Jobs        map[string]struct {
		Permissions yaml.Node `yaml:"permissions"`
	} `yaml:"jobs"`
}

// writeScopes reports the scopes granted at a level above read, and whether a
// permissions block was present at all.
func writeScopes(n yaml.Node) (scopes []string, present bool) {
	switch n.Kind {
	case 0: // absent
		return nil, false
	case yaml.ScalarNode:
		// "read-all" and "none" are safe; "write-all" is not.
		if n.Value != "read-all" && n.Value != "none" {
			return []string{n.Value}, true
		}
		return nil, true
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			scope, level := n.Content[i].Value, n.Content[i+1].Value
			if level != "read" && level != "none" {
				scopes = append(scopes, scope+": "+level)
			}
		}
		return scopes, true
	}
	return nil, true
}

// TestWorkflowTokensAreLeastPrivilege keeps the Scorecard Token-Permissions
// finding closed. A write scope at the top level is handed to every job in the
// workflow, including ones that only check out and run tests.
func TestWorkflowTokensAreLeastPrivilege(t *testing.T) {
	for _, f := range workflowFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var w workflow
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		scopes, present := writeScopes(w.Permissions)
		if !present {
			t.Errorf("%s: no top-level permissions block; the workflow inherits the repository default", f)
			continue
		}
		if len(scopes) > 0 {
			t.Errorf("%s: top-level permissions grant write (%s); move them to the jobs that need them",
				f, strings.Join(scopes, ", "))
		}
		// A job that asks for write must say so itself, which is the readable
		// half of the same rule.
		for name, job := range w.Jobs {
			if _, jobPresent := writeScopes(job.Permissions); !jobPresent {
				continue
			}
			if job.Permissions.Kind == yaml.MappingNode && len(job.Permissions.Content) == 0 {
				t.Errorf("%s: job %q declares an empty permissions block", f, name)
			}
		}
	}
}

// unlinkedVulnerablePackages are packages that carry a known advisory with no
// fixed version, and that nothing in this repository links today. Scorecard
// reports the advisories against the whole module, so the only thing that can
// regress here is a new import pulling the vulnerable code into the build.
//
//	GO-2026-4883, GO-2026-4887, GO-2026-5617, GO-2026-5668, GO-2026-5746
//	   docker/docker, daemon-side; reachable only by something running the
//	   Docker daemon. The agent links api/types (through Wings) for struct
//	   definitions and never the daemon.
//	GO-2026-5932
//	   x/crypto/openpgp, unmaintained by upstream and unsafe by design.
var unlinkedVulnerablePackages = []string{
	"github.com/docker/docker/daemon",
	"golang.org/x/crypto/openpgp",
}

// TestVulnerablePackagesAreNotLinked pins the reason the outstanding advisories
// are not actionable. None of them has a fixed version, so they cannot be
// resolved by bumping a dependency; what keeps them harmless is that the
// vulnerable code is not compiled in.
func TestVulnerablePackagesAreNotLinked(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	// Anything past this point is a broken query, not a missing tool: failing
	// it loudly beats a green run that checked nothing.
	out, err := exec.Command("go", "list", "-deps", "../../...").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	linked := map[string]bool{}
	for _, p := range strings.Split(string(out), "\n") {
		linked[strings.TrimSpace(p)] = true
	}
	// Guard against the query silently returning nothing.
	if !linked["github.com/docker/docker/api/types"] {
		t.Fatal("go list -deps returned an unexpected graph; the check is not meaningful")
	}
	for _, pkg := range unlinkedVulnerablePackages {
		for p := range linked {
			if p == pkg || strings.HasPrefix(p, pkg+"/") {
				t.Errorf("%s is now in the build graph (via %s); it carries an advisory with no fix, so re-check whether the vulnerable code is reachable", pkg, p)
			}
		}
	}
}

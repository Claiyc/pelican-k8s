#!/usr/bin/env bash
# Prepares a release in the working tree: picks the next version from the
# [Unreleased] section of CHANGELOG.md (or takes it as $1), turns that section
# into the release's section and bumps everything that pins the release
# (charts/pelican-k8s/Chart.yaml and the install commands test/docs checks).
# The release PR workflow (.github/workflows/release-pr.yaml) runs it on every
# push to master; it works the same locally.
#
#   hack/release-prep.sh          # next version from the [Unreleased] headings
#   hack/release-prep.sh 1.2.0    # an explicit version
#
# The next version: a "### Removed" heading or the word BREAKING makes a major
# release, "### Added", "### Changed" or "### Deprecated" a minor one, anything
# else a patch. The pelican-panel chart is not touched: it is bumped by the PR
# that changes it and pushed at that version on every release.
#
# Prints the version. Exits 3 when [Unreleased] is empty: nothing to release.
set -euo pipefail
cd "$(dirname "$0")/.."

chart=charts/pelican-k8s/Chart.yaml
current=$(sed -n 's/^version:[[:space:]]*//p' "$chart" | tr -d '"')
unreleased=$(awk '/^## \[Unreleased\]/ {p=1; next} /^## \[/ {p=0} p' CHANGELOG.md)
if ! grep -q '[^[:space:]]' <<<"$unreleased"; then
  echo "CHANGELOG.md: [Unreleased] is empty, nothing to release" >&2
  exit 3
fi

next=${1:-}
if [ -z "$next" ]; then
  IFS=. read -r major minor patch <<<"$current"
  if grep -qE '^### Removed|BREAKING' <<<"$unreleased"; then
    next="$((major + 1)).0.0"
  elif grep -qE '^### (Added|Changed|Deprecated)' <<<"$unreleased"; then
    next="$major.$((minor + 1)).0"
  else
    next="$major.$minor.$((patch + 1))"
  fi
fi
[[ $next =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "not a release version: $next" >&2; exit 1; }
[ "$next" != "$current" ] || { echo "$next is already the chart version" >&2; exit 1; }

export CUR="$current" NEXT="$next" DATE
DATE=$(date -u +%F)
perl -pi -e 's/^version:.*/version: $ENV{NEXT}/; s/^appVersion:.*/appVersion: "$ENV{NEXT}"/' "$chart"
perl -pi -e 's{(charts/pelican-k8s --version )\Q$ENV{CUR}\E\b}{$1$ENV{NEXT}}g; s{(targetRevision: v)\Q$ENV{CUR}\E\b}{$1$ENV{NEXT}}g' \
  README.md docs/install.md
perl -0pi -e 's/^## \[Unreleased\]\n/## [Unreleased]\n\n## [$ENV{NEXT}] - $ENV{DATE}\n/m;
  s{^\[Unreleased\]: (\S*/compare/)v\Q$ENV{CUR}\E\.\.\.HEAD$}{[Unreleased]: $1v$ENV{NEXT}...HEAD\n[$ENV{NEXT}]: $1v$ENV{CUR}...v$ENV{NEXT}}m' \
  CHANGELOG.md

# Every edit above is a pattern; fail loudly if one stopped matching.
grep -qx "version: $next" "$chart" && grep -qx "appVersion: \"$next\"" "$chart" || { echo "$chart: version not bumped" >&2; exit 1; }
grep -qF "## [$next] - $DATE" CHANGELOG.md && grep -qF "[$next]: " CHANGELOG.md || { echo "CHANGELOG.md: section or link not added" >&2; exit 1; }
if grep -rnF -e "pelican-k8s --version $current" -e "targetRevision: v$current" README.md docs/install.md; then
  echo "install commands above still pin $current" >&2
  exit 1
fi
echo "$next"

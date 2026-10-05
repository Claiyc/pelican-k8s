#!/usr/bin/env bash
# Prepares a release in the working tree: writes the release's CHANGELOG.md
# section and bumps everything that pins the release
# (charts/pelican-k8s/Chart.yaml and the install commands test/docs checks).
# The release PR workflow (.github/workflows/release-pr.yaml) runs it on every
# push to master; it works the same locally.
#
#   hack/release-prep.sh          # the next patch version, or see below
#   hack/release-prep.sh 1.2.0    # an explicit version
#
# The section is GitHub's generated release notes for the PRs merged since the
# last release (the same list a GitHub release generates; .github/release.yml
# configures it), fetched with `gh api`, so gh must be logged in. Anything
# written by hand under [Unreleased] is kept above the list, and its headings
# pick the version: "### Removed" or the word BREAKING makes a major release,
# "### Added", "### Changed" or "### Deprecated" a minor one; otherwise it is
# a patch release. RELEASE_NOTES=<file> uses that file instead of the API.
#
# The pelican-panel chart is not touched: it is bumped by the PR that changes
# it and pushed at that version on every release.
#
# Prints the version. Exits 3 when there is nothing to prepare: nothing was
# merged since the last release, or the chart's version is not tagged yet.
set -euo pipefail
cd "$(dirname "$0")/.."

chart=charts/pelican-k8s/Chart.yaml
current=$(sed -n 's/^version:[[:space:]]*//p' "$chart" | tr -d '"')
# Right after a release PR is merged, the chart is at a version whose release
# (and tag) the Release workflow is still making: nothing to prepare yet.
if ! git rev-parse -q --verify "refs/tags/v$current" >/dev/null; then
  echo "v$current is not tagged (yet): its release is still running, or the tags were not fetched (actions/checkout: fetch-depth: 0)" >&2
  exit 3
fi
# Release PRs only carry this script's edits; they are not changes to release.
if [ -z "$(git log --format=%s "v$current..HEAD" | grep -v '^release: ' || true)" ]; then
  echo "nothing merged since v$current, nothing to release" >&2
  exit 3
fi

unreleased=$(awk '/^## \[Unreleased\]/ {p=1; next} /^## \[/ {p=0} p' CHANGELOG.md)
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

notes=$(mktemp)
trap 'rm -f "$notes" "$notes.section"' EXIT
if [ -n "${RELEASE_NOTES:-}" ]; then
  cat "$RELEASE_NOTES" >"$notes"
else
  repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
  gh api "repos/$repo/releases/generate-notes" -f tag_name="v$next" -f previous_tag_name="v$current" \
    -f target_commitish="$(git rev-parse HEAD)" --jq .body >"$notes"
fi
# One level down, to sit under "## [X.Y.Z]"; the compare link is the
# section's [X.Y.Z] reference already.
{
  if grep -q '[^[:space:]]' <<<"$unreleased"; then
    sed -e '/./,$!d' <<<"$unreleased" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
    echo
  fi
  sed -e 's/^## /### /' -e '/^\*\*Full Changelog\*\*/d' "$notes" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
} >"$notes.section"
grep -q '[^[:space:]]' "$notes.section" || { echo "no release notes generated" >&2; exit 1; }

export CUR="$current" NEXT="$next" DATE
DATE=$(date -u +%F)
perl -pi -e 's/^version:.*/version: $ENV{NEXT}/; s/^appVersion:.*/appVersion: "$ENV{NEXT}"/' "$chart"
perl -pi -e 's{(charts/pelican-k8s --version )\Q$ENV{CUR}\E\b}{$1$ENV{NEXT}}g; s{(targetRevision: v)\Q$ENV{CUR}\E\b}{$1$ENV{NEXT}}g' \
  README.md docs/install.md
# [Unreleased] is emptied into the new section.
awk -v head="## [$next] - $DATE" -v body="$notes.section" '
  /^## \[Unreleased\]/ { print; print ""; print head; print ""; while ((getline l < body) > 0) print l; print ""; skip=1; next }
  skip && /^## \[/ { skip=0 }
  !skip' CHANGELOG.md >CHANGELOG.md.new
mv CHANGELOG.md.new CHANGELOG.md
perl -pi -e 's{^\[Unreleased\]: (\S*/compare/)v\Q$ENV{CUR}\E\.\.\.HEAD$}{[Unreleased]: $1v$ENV{NEXT}...HEAD\n[$ENV{NEXT}]: $1v$ENV{CUR}...v$ENV{NEXT}}' \
  CHANGELOG.md

# Every edit above is a pattern; fail loudly if one stopped matching.
grep -qx "version: $next" "$chart" && grep -qx "appVersion: \"$next\"" "$chart" || { echo "$chart: version not bumped" >&2; exit 1; }
grep -qF "## [$next] - $DATE" CHANGELOG.md && grep -qF "[$next]: " CHANGELOG.md || { echo "CHANGELOG.md: section or link not added" >&2; exit 1; }
if grep -rnF -e "pelican-k8s --version $current" -e "targetRevision: v$current" README.md docs/install.md; then
  echo "install commands above still pin $current" >&2
  exit 1
fi
echo "$next"

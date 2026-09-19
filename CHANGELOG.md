# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and the project uses
[Semantic Versioning](https://semver.org/) once tagged.

## [Unreleased]

### Added
- Contract suite against a real Pelican Panel (`hack/e2e-kind.sh`, workflow `Contract`): a kind cluster runs the Panel from `charts/pelican-panel` and pelican-k8s built from the commit, then drives node registration, server creation and install, power, console websocket, files, SFTP, backup, suspension and deletion through the Panel's own services. It runs on every pull request against the pinned Panel version and nightly against `ghcr.io/pelican/panel:latest`; a nightly failure opens an `upstream` issue.
- Native Go fuzzing of the three parsers on a trust boundary: the shim's JSON-lines protocol, the output ring buffer and the gateway's JWT verification. The seed corpora run with the unit tests; a CI job fuzzes each target for 40s.
- README badges for CodeQL and the OpenSSF Scorecard. The Go Reference badge was dropped: every `pkg.go.dev` page for the module 404s while the badge image is served unconditionally, and 29 of the 37 packages are `internal/`, so there is no reference worth linking.
- `test/supplychain`: guards the pinning and least-privilege invariants above, so a future change cannot quietly reopen the Scorecard findings.
- `test/docs`: the versions in the documented `helm install` commands and the Argo CD example must match the chart versions the tree releases, so a chart bump cannot leave a quick start that fails on its first command.
- `test/supplychain` also asserts that `docker/docker/daemon` and `x/crypto/openpgp` stay out of the build graph; the six outstanding advisories have no fixed version, so not linking them is what keeps them harmless.

### Changed
- Pod readiness now carries the game state: the game container is ready only while Wings reports `running` (what the Panel shows as running), so a stopped or starting server shows `1/2` instead of `2/2`. Readiness restarts nothing and gates nothing (no liveness probe on the game container, Services publish not-ready addresses). The pod template changes, so existing pods are marked `RecreatePending` and pick the probe up at their next stop or start.
- Build with Go 1.27. `govulncheck` in CI fails only for reachable vulnerabilities that have a fix.
- Dependency updates come from Dependabot; the Renovate configuration was removed.

### Security
- Every GitHub Action is pinned to a commit SHA and every container base image to a digest, closing the Scorecard *Pinned-Dependencies* finding. Dependabot keeps both current.
- Workflow tokens are scoped to the jobs that need them: `security-events: write` (CodeQL), `contents: write` and `packages: write` (release) and `issues: write` (upstream drift) are no longer granted workflow-wide, closing the Scorecard *Token-Permissions* finding.

### Fixed
- A start or restart now syncs the server configuration from the Panel first. The Panel sends no sync for a startup variable change because Wings fetches the live configuration on every boot, but the agent's boot sync is answered from `spec.panel`, so such a change only applied after the next drift resync (up to 15 minutes).
- The image vulnerability scan could not resolve `aquasecurity/trivy-action@0.36.0` (the tags carry a `v` prefix from 0.29 on), so no Trivy results reached code scanning.
- The nightly *Upstream drift* workflow was failing on both jobs without any upstream having drifted. The Wings job pointed the `replace` at `github.com/pelican/wings@main`, which cannot resolve: package `boot` is one of the four hooks and exists only on the fork's `pelican-k8s-hooks` branch, so `go mod tidy` always failed, `|| true` hid it, and the drift tests then aborted on an inconsistent `go.mod` instead of running. The job now checks the tip of the hooks branch — the code the agent is actually built from — tidies without masking errors, warns when the pinned `replace` is behind that tip, and builds and tests against it instead of expecting the build to fail.
- The Panel job grepped the image for the literal string `servers/{uuid}/container/status`, which upstream no longer spells out anywhere since the daemon routes moved into `Route::prefix('/servers/{server:uuid}')->group(...)`; the endpoint itself is unchanged. The three greps are replaced by `hack/check-panel-contract.sh`, which resolves the prefix groups and diffs the whole remote route table, the `Http::daemon` bearer token and base URL, and the `Pelican Wings` response User-Agent check against docs/wings-panel-contract.md. It runs locally too (`PANEL_IMAGE=`, or `PANEL_SRC_DIR=` for a checkout without Docker).

## [0.1.0] - 2026-09-16

First release. Implements ARCHITECTURE.md end to end and was verified on an
OpenShift 4.22 / Kubernetes 1.35 cluster against Pelican Panel v1.0.0-beta38
with the Paper and Vanilla Minecraft eggs.

### Added
- Shim: PTY process supervisor as PID 1 of game containers with a JSON-lines socket protocol, output ring buffer, cgroup v2 stats, orphan reaping and OOM detection; `prepare`, `probe` and `install-run` helpers for init containers and install Jobs.
- Agent: Wings embedded as a library for one server (stock router, websocket, SFTP, filesystem, parser, crash detection, backups, activity) with a shim-backed process environment and a Job-backed installer.
- Operator: `GameServer` reconciler creating the PVC, Services, NetworkPolicy, StatefulSet and install Jobs, driving the agent (power, sync, install), in-place resize, deferred pod recreation, OOM relay, node-loss fencing, scheduled snapshots and Delete/Retain/SnapshotThenDelete finalizers.
- Gateway: Wings node API towards the Panel, agent-facing remote API, JWT re-signing for websockets and signed URLs, websocket proxy, SFTP relay, drift resync.
- Helm charts `pelican-k8s` and `pelican-panel`; CI (build, tests, lint, chart lint, multi-arch images), release and upstream-drift workflows; Renovate.
- Test suites: unit, spike (Paper egg through the agent and shim in Docker), e2e against a cluster, upstream compatibility diffs.

### Known limitations
- Server transfers, Panel mounts, `force_outgoing_ip`, swap, `io_weight`, CPU pinning and disabling the OOM killer are not supported (see docs/compatibility.md).
- Local (`wings` adapter) backups live on the pod's scratch volume and do not survive pod recreation; use the S3 adapter.
- Wings is pinned to a fork branch carrying four opt-in hooks until they are merged upstream.

[Unreleased]: https://github.com/Claiyc/pelican-k8s/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Claiyc/pelican-k8s/releases/tag/v0.1.0

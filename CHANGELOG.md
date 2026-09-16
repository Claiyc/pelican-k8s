# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and the project uses
[Semantic Versioning](https://semver.org/) once tagged.

## [Unreleased]

### Added
- Native Go fuzzing of the three parsers on a trust boundary: the shim's JSON-lines protocol, the output ring buffer and the gateway's JWT verification. The seed corpora run with the unit tests; a CI job fuzzes each target for 40s.
- README badges for CodeQL and the OpenSSF Scorecard.
- `test/supplychain`: guards the pinning and least-privilege invariants above, so a future change cannot quietly reopen the Scorecard findings.

### Changed
- Build with Go 1.27. `govulncheck` in CI fails only for reachable vulnerabilities that have a fix.
- Dependency updates come from Dependabot; the Renovate configuration was removed.

### Security
- Every GitHub Action is pinned to a commit SHA and every container base image to a digest, closing the Scorecard *Pinned-Dependencies* finding. Dependabot keeps both current.
- Workflow tokens are scoped to the jobs that need them: `security-events: write` (CodeQL), `contents: write` and `packages: write` (release) and `issues: write` (upstream drift) are no longer granted workflow-wide, closing the Scorecard *Token-Permissions* finding.

### Fixed
- The image vulnerability scan could not resolve `aquasecurity/trivy-action@0.36.0` (the tags carry a `v` prefix from 0.29 on), so no Trivy results reached code scanning.

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

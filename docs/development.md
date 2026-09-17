# Development

## Building

```bash
make build            # bin/{shim,agent,gateway,operator}
make test             # unit tests with -race
make generate         # deepcopy + CRDs after editing api/v1alpha1 (commit the output)
make images           # docker build of the four images (build/*.Dockerfile)
```

Go 1.26+ is required (`go.mod`). Images are multi-arch (`linux/amd64`, `linux/arm64`).

## Tests

| Suite | Command | Needs |
|---|---|---|
| Unit | `go test ./...` | nothing. Covers the shim (supervisor, protocol, cgroup parsing, prepare/probe/install-run), the shim environment against a live supervisor, the Job installer, object rendering and resource mapping, the reconciler (fake client, fake agent) and the gateway (fake Panel, fake agent, fake cluster) |
| Fuzz | `go test -run '^$' -fuzz FuzzDecode ./internal/shim/protocol` (likewise `FuzzWrite` in `internal/shim/ringbuf`, `FuzzVerify` in `internal/gateway/jwtx`) | nothing. The seed corpora also run as ordinary tests under `go test ./...`; CI fuzzes each target for 40s. A crasher lands in the package's `testdata/fuzz/` — commit it as a regression case with the fix |
| Docs | `go test ./test/docs/` | nothing. Fails when a documented `helm install --version` or an example's `targetRevision` names a version the tree does not release, so bumping a chart has to bump the docs in the same change |
| Supply chain | `go test ./test/supplychain/` | nothing. Asserts every action is SHA-pinned with a version comment, every base image digest-pinned, and that no workflow grants a write token above the job that needs it |
| Upstream | `go test ./test/upstream/` | the pinned Wings module. Diffs Wings' route table, remote client calls and `ProcessEnvironment` against what the gateway and agent handle |
| Spike (M0) | `go test -tags spike ./test/spike -v` | Docker and internet. Runs the Paper egg through the agent and the shim in a yolk container against the fake Panel: install, start, done detection, stats, websocket auth, commands, stop, crash restart |
| Contract (kind) | `hack/e2e-kind.sh` | Docker, kind, helm, kubectl (or `KUBECTL=oc`), internet. Throwaway kind cluster with the **real Pelican Panel** (our chart, SQLite) and pelican-k8s built from the working tree: node registration, egg import and server creation through the Panel's own services, install Job, auto-start, the e2e suite below, then Panel-side status, backup, suspension and deletion. CI runs it on every PR (`Contract` workflow) and nightly against `ghcr.io/pelican/panel:latest`, opening an `upstream` issue on failure. `PANEL_IMAGE=... hack/e2e-kind.sh` tests another Panel version, `KEEP=1` keeps the cluster |
| e2e | `go test -tags e2e ./test/e2e -v` | a deployed gateway and one server. Set `PELICAN_E2E_GATEWAY`, `PELICAN_E2E_TOKEN`, `PELICAN_E2E_PANEL_URL`, `PELICAN_E2E_SERVER` (and `PELICAN_E2E_SFTP`, `_SFTP_USER`, `_SFTP_PASSWORD`, `_INSECURE=true`) |

## Dev loop on a cluster

The canonical images are the ones CI publishes to
`ghcr.io/claiyc/pelican-k8s/*` for every commit (`sha-<commit>`, `master`)
and every release (`X.Y.Z`); clusters should run those. For iterating on
unpushed changes against a private registry, `hack/dev-push.sh` builds every image, pushes them with
[crane](https://github.com/google/go-containerregistry/tree/main/cmd/crane)
(works with registries whose TLS the Docker daemon does not trust) and rolls
the Helm release:

```bash
REGISTRY=harbor.example.com/pelican VALUES=my-values.yaml hack/dev-push.sh
```

The operator recreates game pods with the new shim/agent images at the next
safe point; an idle server is recreated immediately.

## Wings as a dependency

The agent imports `github.com/pelican/wings`. Four opt-in hooks are needed
(ARCHITECTURE.md 6.3) and live on the `pelican-k8s-hooks` branch of the
[Claiyc/wings](https://github.com/Claiyc/wings) fork, pinned in `go.mod`:

1. `server.WithEnvironmentFactory` / `server.WithInstaller` options on the server manager (Docker stays the default)
2. `server.ImageAndStopConfigurable` and `server.Attachable` interfaces instead of `*docker.Environment` type assertions
3. `server.Installer` interface with the install lock held by `Server.Install`
4. package `boot` exposing the activity database initialisation and the cron scheduler that live under `internal/`

Bumping Wings: rebase the branch on the new upstream tag, push, update the
`replace` line, run `go test ./test/upstream/ ./...` and the spike, then the
e2e suite. Each hook is meant to be submitted upstream as an independent PR;
nothing is pushed upstream from this repository automatically.

The fork is the dependency for as long as any hook is unmerged, and it is what
the nightly *Upstream drift* workflow checks. If all four land upstream, the
fork and the `replace` go away together: point `go.mod` and that workflow at
`github.com/pelican/wings` and delete the branch.

## Code map

| Package | Role |
|---|---|
| `internal/shim/supervisor` | PTY process supervisor and socket server (PID 1) |
| `internal/shim/protocol` | JSON-lines protocol and client |
| `internal/shim/cgroup` | cgroup v2 and `/proc/net/dev` sampling |
| `internal/shim/prepare` | PVC layout, entrypoint probe, install-run |
| `internal/agent/app` | Wings boot sequence without Docker |
| `internal/agent/shimenv` | `environment.ProcessEnvironment` over the shim |
| `internal/agent/installer` | Job-backed `server.Installer` |
| `internal/operator/render` | pure object builders and resource mapping |
| `internal/operator/controller` | reconciler, install state machine, finalizer, snapshots |
| `internal/gateway/panelapi` | Wings API towards the Panel and browsers |
| `internal/gateway/remoteapi` | Panel remote API towards agents |
| `internal/gateway/serversync` | Panel → spec, agent configuration assembly, resync |
| `internal/gateway/wsproxy`, `sftprelay`, `jwtx` | websocket relay, SFTP relay, JWT re-signing |

## Dependencies, security and quality

| What | How | Where to look |
|---|---|---|
| Go modules, GitHub Actions, base images | Dependabot weekly PRs (`.github/dependabot.yml`, Kubernetes libraries grouped) | Pull requests |
| Wings | Pinned by hand (fork branch + `replace`); the nightly *Upstream drift* workflow tests against the tip of `pelican-k8s-hooks`, warns when the pin is behind it and opens or updates an `upstream` issue when it breaks | Issues labelled `upstream` |
| Pelican Panel | The same workflow compares the chart `appVersion` with the latest Panel release and opens an issue | Issues labelled `upstream` |
| Known CVEs in Go dependencies | `govulncheck` in CI on every push and PR; fails when a reachable vulnerability has a fixed version, findings without a fix go to the step summary | CI job *Build and test* |
| CVEs in the container images | Trivy scans all four images on every push, HIGH/CRITICAL, fixed vulnerabilities only, uploaded as SARIF | Security → Code scanning |
| Static analysis | CodeQL (Go) on pushes, PRs and weekly; golangci-lint in CI | Security → Code scanning, CI job *golangci-lint* |
| Untrusted-input parsers | Native Go fuzzing of the shim protocol, the output ring buffer and the gateway's JWT verification | CI job *Fuzz* |
| Build inputs | Every GitHub Action is pinned to a commit SHA and every base image to a digest (the trailing comment carries the human-readable version); Dependabot bumps both. `test/supplychain` fails the build if a pin or a least-privilege token scope regresses | `.github/workflows/`, `build/*.Dockerfile` |
| Dependabot alerts and security updates | Enabled on the repository (GitHub advisory database) | Security → Dependabot |
| Supply chain posture | OpenSSF Scorecard weekly with published results | Security → Code scanning, scorecard badge |
| Vulnerability reports | Private vulnerability reporting is enabled (see SECURITY.md) | Security → Advisories |

### Standing Scorecard findings

Scorecard's score cannot reach 10 here, and two of the findings are worth
explaining rather than re-investigating.

**Vulnerabilities (4/10).** Six advisories are reported against modules in the
graph. **None of the six has a fixed version**, so no dependency bump can clear
them; what keeps them harmless is that the vulnerable code is never linked.
`test/supplychain` asserts exactly that, so a new import cannot change it
unnoticed.

| Advisory | Module | Where the vulnerable code lives |
|---|---|---|
| GO-2026-4883, GO-2026-4887 | `github.com/docker/docker` | plugin privilege validation and AuthZ plugin handling, daemon-side |
| GO-2026-5617, GO-2026-5668, GO-2026-5746 | `github.com/docker/docker` | `docker/docker/daemon` (`docker cp`, `PUT /containers/{id}/archive`) |
| GO-2026-5932 | `golang.org/x/crypto` | `x/crypto/openpgp`, unmaintained upstream and unsafe by design |

`github.com/docker/docker` enters through `internal/agent/installer` →
`wings/system` → `docker/docker/api/types`. 27 of its packages are linked —
`api/types/*`, `client`, `errdefs` and two `pkg/parsers` helpers — and
`docker/docker/daemon` is not among them. `x/crypto/openpgp` is not in the
build graph at all. This matches `govulncheck`, which finds no vulnerability
reachable from this code.

**Everything else** needs an action outside the tree: *Code-Review* wants
approvals on merged PRs, *CII-Best-Practices* wants the project registered at
bestpractices.coreinfrastructure.org, *Branch-Protection* errors out because
the default `GITHUB_TOKEN` cannot read classic branch protection rules (it
needs a fine-grained PAT in `scorecard.yaml`), *Signed-Releases* waits on a
first release, and *Maintained* clears once the repository is 90 days old.

## Releases

Tag `vX.Y.Z`. The release workflow builds and pushes the images with semver
tags and publishes both charts as OCI packages to
`oci://ghcr.io/claiyc/pelican-k8s/charts`, plus a GitHub release with the
chart tarballs.

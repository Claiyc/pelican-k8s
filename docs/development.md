# Development

## Building

```bash
make build            # bin/{shim,agent,gateway,operator}
make test             # unit tests with -race
make generate         # deepcopy + CRDs after editing api/v1alpha1 (commit the output)
make images           # docker build of the four images (build/*.Dockerfile)
```

Go 1.27 (`go.mod`). Images are multi-arch (`linux/amd64`, `linux/arm64`).

## Tests

| Suite | Command | Needs |
|---|---|---|
| Unit | `go test ./...` | nothing. Covers the shim (supervisor, protocol, cgroup parsing, prepare/probe/install-run), the shim environment against a live supervisor, the Job installer, object rendering and resource mapping, the reconciler (fake client, fake agent) and the gateway (fake Panel, fake agent, fake cluster) |
| Fuzz | `go test -run '^$' -fuzz FuzzDecode ./internal/shim/protocol` (likewise `FuzzWrite` in `internal/shim/ringbuf`, `FuzzVerify` in `internal/gateway/jwtx`) | nothing. The seed corpora also run as ordinary tests under `go test ./...`; CI fuzzes each target for 40s. A crasher lands in the package's `testdata/fuzz/` — commit it as a regression case with the fix |
| Docs | `go test ./test/docs/` | nothing. Fails when a documented `helm install --version` or an example's `targetRevision` names a version the tree does not release, so bumping a chart has to bump the docs in the same change |
| Supply chain | `go test ./test/supplychain/` | nothing. Asserts every action is SHA-pinned with a version comment, every base image digest-pinned, and that no workflow grants a write token above the job that needs it |
| Upstream | `go test ./test/upstream/` | the pinned Wings module. Diffs Wings' route table, remote client calls and `ProcessEnvironment` against what the gateway and agent handle |
| Spike | `go test -tags spike ./test/spike -v` | Docker and internet. Runs the Paper egg through the agent and the shim in a yolk container against the fake Panel: install, start, done detection, stats, websocket auth, commands, stop, crash restart. CI runs it on every PR and push to master |
| Contract (kind) | `hack/e2e-kind.sh` | Docker, kind, helm, kubectl (or `KUBECTL=oc`), internet. Throwaway kind cluster with the **real Pelican Panel** (our chart, SQLite) and pelican-k8s built from the working tree: node registration, egg import and server creation through the Panel's own services, install Job, auto-start, the e2e suite below, then Panel-side status, backup, suspension and deletion. CI runs it on every PR (`Contract` workflow) and nightly against `ghcr.io/pelican/panel:latest`, opening an `upstream` issue on failure. `PANEL_IMAGE=... hack/e2e-kind.sh` tests another Panel version, `KEEP=1` keeps the cluster |
| Placement (kind) | `hack/e2e-placement.sh` | Docker, kind, helm, kubectl, internet. Throwaway kind cluster with three workers whose volumes carry no node affinity (local-path provisioner in shared mode), pelican-k8s built from the working tree, the fake Panel (`test/fakepanel/cmd/fakepanel`) and a shell egg (`test/placement/egg`). `test/placement` creates servers through the gateway and checks where the agent pod and the game pod run: start and stop, the agent's node cordoned with and without in-flight work, `preferAgentNode: false`, a drain, either pod deleted while running, a console kept open while the agent pod is deleted, a crash without restart, the agent of a stopped server drained, the allocation-IP node pin. Nodes are made unfit by cordoning them. The chart is installed with `tls.enabled`, so every hop between the components runs over TLS (`TLS=false` turns it off). CI runs it on PRs touching the operator, agent, shim, websocket proxy, API or chart, on pushes to `master` and `v2`, and nightly (`Placement` workflow). A failing scenario logs the GameServer status, both pods and the end of their containers' logs. A new push to a pull request cancels its running Placement run. `RUN=TestDrain` runs one scenario, `KEEP=1` keeps the cluster |
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

The operator recreates agent pods with the new agent image at the next safe
point: the process is offline and the agent has no in-flight work (transfers,
backups, installs, active SFTP). Game pods get the new shim when they are next
created.

## Wings as a dependency

The agent imports `github.com/pelican/wings`. Four opt-in hooks are needed
(ARCHITECTURE.md 6.3) and live on the `pelican-k8s-hooks` branch of the
[Claiyc/wings](https://github.com/Claiyc/wings) fork, pinned in `go.mod`:

1. `server.WithEnvironmentFactory` / `server.WithInstaller` options on the server manager (Docker stays the default)
2. `server.ImageAndStopConfigurable` and `server.Attachable` interfaces for the image, stop configuration and attach calls
3. `server.Installer` interface with the install lock held by `Server.Install`
4. package `boot` exposing the activity database initialisation and the cron scheduler that live under `internal/`

Bumping Wings: bring `pelican-k8s-hooks` up to date with upstream, push, point
the `replace` line at the new tip
(`go mod edit -replace github.com/pelican/wings=github.com/Claiyc/wings@<commit> && go mod tidy`),
run `go test ./test/upstream/ ./...` and the spike, and update the upstream
basis in `docs/wings-panel-contract.md`. The *Upstream drift* workflow tests
the tip of the hooks branch nightly and warns when the `replace` is behind it.

## Code map

| Package | Role |
|---|---|
| `internal/shim/supervisor` | PTY process supervisor (PID 1 of the game pod); connects to the agent pod and serves it |
| `internal/shim/protocol` | JSON-lines protocol, token handshake, the agent's listener and client |
| `internal/shim/cgroup` | cgroup v2 and `/proc/net/dev` sampling |
| `internal/shim/prepare` | PVC layout, entrypoint probe, install-run |
| `internal/agent/app` | Wings boot sequence without Docker |
| `internal/agent/shimenv` | `environment.ProcessEnvironment` over the shim |
| `internal/agent/installer` | Job-backed `server.Installer` |
| `internal/operator/render` | pure object builders (agent and game StatefulSets, Services, NetworkPolicies, Jobs) and resource mapping |
| `internal/operator/controller` | reconciler, game pod lifecycle and placement, install state machine, finalizer, snapshots |
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
| Static analysis | CodeQL (Go, the `security-and-quality` suite) on pushes, PRs and weekly. golangci-lint in CI in two stages (`make lint`): the whole tree must pass the linters in the Makefile's `LINT_GATE`, and code changed since the base branch must pass every linter in `.golangci.yml`. Every finding of every linter on the existing code is uploaded as SARIF and fails nothing (`make lint-all` lists the same locally) | Security → Code scanning, CI job *golangci-lint* |
| Test coverage | The unit tests and the spike (the agent in-process) each write a coverage profile; the CI job *Coverage and quality reports* uploads both to Codecov, which comments on pull requests with the coverage of the changed lines (target 70 %, reported and not enforced, `codecov.yml`). The kind suites run the images and are not counted | Codecov, coverage badge |
| Maintainability, duplication, complexity | SonarQube Cloud, analysed from the same CI job with both coverage profiles and every golangci-lint finding, which it lists next to its own issues (`sonar-project.properties`); pull requests from forks and Dependabot run without the token and are skipped | SonarQube Cloud, quality gate badge |
| Review | CodeRabbit reviews pull requests as an advisory reader (`.coderabbit.yaml`); it skips Dependabot and release PRs and blocks nothing | Pull request comments |
| Untrusted-input parsers | Native Go fuzzing of the shim protocol, the output ring buffer and the gateway's JWT verification | CI job *Fuzz* |
| Build inputs | Every GitHub Action is pinned to a commit SHA and every base image to a digest (the trailing comment carries the human-readable version); Dependabot bumps both. `test/supplychain` fails the build if a pin or a least-privilege token scope regresses | `.github/workflows/`, `build/*.Dockerfile` |
| Dependabot alerts and security updates | Enabled on the repository (GitHub advisory database) | Security → Dependabot |
| Supply chain posture | OpenSSF Scorecard weekly with published results | Security → Code scanning, scorecard badge |
| Vulnerability reports | Private vulnerability reporting is enabled (see SECURITY.md) | Security → Advisories |

### Standing Scorecard findings

**Vulnerabilities.** These advisories have no fixed version, and their
vulnerable code is not linked; `test/supplychain` asserts that.

| Advisory | Module | Where the vulnerable code lives |
|---|---|---|
| GO-2026-4883, GO-2026-4887 | `github.com/docker/docker` | plugin privilege validation and AuthZ plugin handling, daemon-side |
| GO-2026-5617, GO-2026-5668, GO-2026-5746 | `github.com/docker/docker` | `docker/docker/daemon` (`docker cp`, `PUT /containers/{id}/archive`) |
| GO-2026-5932 | `golang.org/x/crypto` | `x/crypto/openpgp`, unmaintained upstream and unsafe by design |

`github.com/docker/docker` enters through `internal/agent/installer` →
`wings/system` → `docker/docker/api/types`; `docker/docker/daemon` is not
linked. `x/crypto/openpgp` is not in the build graph. `govulncheck` finds no
vulnerability reachable from this code.

**Other checks** need settings outside the tree: *Code-Review* counts
approvals on merged PRs, *CII-Best-Practices* needs a registration at
bestpractices.coreinfrastructure.org, and *Branch-Protection* needs a
fine-grained PAT in `scorecard.yaml` to read classic branch protection rules.

## Releases

Mark the release PR ready and merge it. Nothing else is manual.

- **The release PR.** Whenever something was merged since the last release,
  the `Release PR` workflow keeps a `release: X.Y.Z` PR open on the
  `release/next` branch and rewrites it on every push to master. The PR is
  opened as a draft, and rewriting it leaves the draft state alone, so it
  cannot be merged until a maintainer marks it ready. It runs
  `hack/release-prep.sh`, which writes the `CHANGELOG.md` section from
  GitHub's generated release notes (the PRs merged since the last tag; release
  PRs are left out by `.github/release.yml`) and bumps
  `charts/pelican-k8s/Chart.yaml` and the pinned install commands that
  `test/docs` checks.
- **The version.** A patch release by default. Label a PR `minor` (a new
  feature) or `major` (a breaking change) and the next release is at least
  that; the largest label among the PRs merged since the last release wins,
  and the release PR says which PR decided it. The release PR itself can
  carry the label too, which is the quickest way to ask for a bigger release.
  Labelling a PR after it was merged, or the release PR, updates the release
  PR straight away. Notes written by hand under
  `## [Unreleased]` are kept above the generated list and can raise it too:
  `### Added`, `### Changed` or `### Deprecated` make a minor release,
  `### Removed` or the word `BREAKING` a major one.
- **Merging it.** The `Release` workflow sees a chart version on master with
  no tag. It checks that the chart's `version`, `appVersion` and the
  `CHANGELOG.md` section agree, builds and pushes the images
  (`ghcr.io/claiyc/pelican-k8s/{shim,agent,gateway,operator}`, tagged `X.Y.Z`
  and `X.Y`), pushes both charts to `oci://ghcr.io/claiyc/pelican-k8s/charts`,
  then tags the commit `vX.Y.Z` and publishes the GitHub release with the
  changelog section as its notes and the chart tarballs attached.
- **The Panel chart** is not bumped by a release: bump `charts/pelican-panel`
  in the PR that changes it. Every release pushes it at its current version.
- **CI on the release PR.** PRs opened with the default `GITHUB_TOKEN` start no
  workflows. To run CI on the release PR, add a `RELEASE_PR_TOKEN` secret: a
  fine-grained PAT or GitHub App token with *Contents* and *Pull requests*
  write access to this repository. Either way the workflow runs `test/docs` on
  the prepared tree before pushing it, and the repository setting *Allow
  GitHub Actions to create and approve pull requests* must be on. CI, CodeQL
  and Contract skip the release PR while it is a draft; marking it ready
  starts them.
- **By hand.** `hack/release-prep.sh [X.Y.Z]` prepares the same change locally
  (`gh` must be logged in for the notes). Pushing a `vX.Y.Z` tag releases
  that tag.

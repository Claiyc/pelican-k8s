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
| Upstream | `go test ./test/upstream/` | the pinned Wings module. Diffs Wings' route table, remote client calls and `ProcessEnvironment` against what the gateway and agent handle |
| Spike (M0) | `go test -tags spike ./test/spike -v` | Docker and internet. Runs the Paper egg through the agent and the shim in a yolk container against the fake Panel: install, start, done detection, stats, websocket auth, commands, stop, crash restart |
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

## Releases

Tag `vX.Y.Z`. The release workflow builds and pushes the images with semver
tags and publishes both charts as OCI packages to
`oci://ghcr.io/claiyc/pelican-k8s/charts`, plus a GitHub release with the
chart tarballs.

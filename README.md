# pelican-k8s

[![CI](https://github.com/Claiyc/pelican-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/Claiyc/pelican-k8s/actions/workflows/ci.yaml)
[![CodeQL](https://github.com/Claiyc/pelican-k8s/actions/workflows/codeql.yaml/badge.svg)](https://github.com/Claiyc/pelican-k8s/security/code-scanning)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/Claiyc/pelican-k8s/badge)](https://scorecard.dev/viewer/?uri=github.com/Claiyc/pelican-k8s)
[![Go Reference](https://pkg.go.dev/badge/github.com/Claiyc/pelican-k8s.svg)](https://pkg.go.dev/github.com/Claiyc/pelican-k8s)
[![Release](https://img.shields.io/github/v/release/Claiyc/pelican-k8s?include_prereleases&sort=semver)](https://github.com/Claiyc/pelican-k8s/releases)
[![License](https://img.shields.io/github/license/Claiyc/pelican-k8s)](LICENSE)

**A Kubernetes-native backend for [Pelican Panel](https://pelican.dev).**
It replaces the Docker-based Wings daemon with a gateway, an operator and a
per-pod agent. The Panel and the eggs stay completely unmodified: the Panel
sees an ordinary Wings node, every game server becomes a `GameServer` resource
you can inspect with `kubectl`, and Kubernetes owns scheduling, storage,
networking and restarts.

> Status: **alpha**. The full Wings feature set used day to day works end to end
> (power, console, stats, files, uploads, SFTP, installs, crash detection,
> backups, activity, suspension) and has been exercised on a real cluster, but
> the API group is `v1alpha1` and Pelican itself is still in beta. Read
> [docs/compatibility.md](docs/compatibility.md) before relying on it.

```
Panel ──Wings API (node token)──▶ gateway ──spec/status──▶ GameServer CR
                                     │                          │
                                     │ proxied data path        │ operator reconciles
                                     ▼                          ▼
                              agent sidecar ◀── unix socket ──▶ shim (PID 1) ─▶ egg image entrypoint
                              (Wings as a library)               game container
```

| Component | What it is |
|---|---|
| **gateway** | Presents itself to the Panel as one Wings node. Turns Panel calls into `GameServer` spec changes, proxies console, files, uploads and SFTP to the right agent, re-signs browser JWTs with per-agent tokens |
| **operator** | The only reconciler. One `GameServer` becomes a StatefulSet (1 pod), PVC, Services, NetworkPolicy and install Jobs; it drives the agent (power, sync, install) until the process matches the spec |
| **agent** | [Wings](https://github.com/pelican/wings) imported as a Go module, running as a native sidecar with exactly one server: stock router, websocket, SFTP server, filesystem, egg config parser, crash detection, backups |
| **shim** | Static binary injected as the game container's entrypoint: PTY, stdin, signals, exit codes, cgroup stats, output ring buffer |

The design is documented in depth in [ARCHITECTURE.md](ARCHITECTURE.md); the
Wings/Panel protocol the gateway reproduces is in
[docs/wings-panel-contract.md](docs/wings-panel-contract.md).

## Quick start

Prerequisites: Kubernetes ≥ 1.33 (1.35 recommended: in-place pod resize,
native sidecars and `ValidatingAdmissionPolicy` are used), a StorageClass that
supports volume expansion, Helm 3, and a way to expose two things: the
gateway's HTTP API (Ingress or OpenShift Route with long timeouts) and its SFTP
port (NodePort or LoadBalancer). Game ports use `LoadBalancer` Services by
default; single-node clusters can use `NodePort`.

**1. Deploy the Panel** (skip if you already run one; any Pelican Panel works):

```bash
helm install pelican-panel oci://ghcr.io/claiyc/pelican-k8s/charts/pelican-panel --version 0.1.0 \
  -n pelican --create-namespace \
  --set panel.url=https://panel.example.com \
  --set ingress.enabled=true --set ingress.host=panel.example.com
kubectl -n pelican exec deploy/pelican-panel -- php artisan p:user:make --admin=1 \
  --email=you@example.com --username=admin --password='<password>'
```

See [docs/panel.md](docs/panel.md) for databases, TLS and OpenShift notes.

**2. Create the node in the Panel** (Admin → Nodes → Create): FQDN
`wings.example.com`, scheme `https`, behind proxy `yes`, daemon port `8080`,
daemon connect port `443`, SFTP port `30022` (or your LoadBalancer port). Open
the node's *Configuration* tab and copy `token_id` and `token`.

**3. Install pelican-k8s** from the OCI chart (or from `charts/pelican-k8s`
in a checkout):

```bash
helm install pelican-k8s oci://ghcr.io/claiyc/pelican-k8s/charts/pelican-k8s --version 0.1.0 \
  -n pelican-system --create-namespace \
  --set gateway.panelURL=https://panel.example.com \
  --set gateway.nodeTokenID=<token_id> --set gateway.nodeToken=<token> \
  --set gateway.ingress.enabled=true --set gateway.ingress.host=wings.example.com \
  --set defaultClass.spec.storage.storageClassName=<expandable storage class> \
  --set defaultClass.spec.exposure.mode=LoadBalancer
```

**4. Add allocations and create a server in the Panel** as you would with
Wings. Watch it come up:

```bash
kubectl -n pelican-servers get gameservers
kubectl -n pelican-servers get pods,pvc,svc,jobs
kubectl -n pelican-servers describe gameserver gs-<uuid>   # conditions and events
```

The step-by-step guide with all options is in [docs/install.md](docs/install.md).

## Documentation

| Document | Contents |
|---|---|
| [docs/install.md](docs/install.md) | Installation, Panel node setup, exposure modes, ingress requirements, OpenShift |
| [docs/panel.md](docs/panel.md) | Deploying the Panel with the `pelican-panel` chart |
| [docs/classes.md](docs/classes.md) | `GameServerClass` reference: storage, exposure, networking, resources, security, installs |
| [docs/operations.md](docs/operations.md) | Day-2: inspecting servers, upgrades, backups and snapshots, troubleshooting |
| [docs/compatibility.md](docs/compatibility.md) | Wings feature parity, known limitations, deviations from the architecture document |
| [docs/security.md](docs/security.md) | Trust boundaries, tokens, pod security, network policies |
| [docs/development.md](docs/development.md) | Building, testing (unit, spike, e2e, upstream diffs), the Wings fork and its hooks, release process |
| [ARCHITECTURE.md](ARCHITECTURE.md) | The design |
| [docs/wings-panel-contract.md](docs/wings-panel-contract.md) | Verified Wings ⇄ Panel protocol reference |

## Repository layout

```
api/v1alpha1/          GameServer and GameServerClass types (CRDs generated into charts/pelican-k8s/crds)
cmd/{agent,gateway,operator,shim}
internal/agent/        Wings-as-a-library boot, shim environment, Job installer, internal routes
internal/gateway/      Panel-facing API, agent-facing remote API, websocket proxy, SFTP relay, spec sync
internal/operator/     reconciler, object rendering, resource mapping, image resolution
internal/shim/         supervisor, protocol, cgroup stats, prepare/probe/install-run helpers
charts/pelican-k8s/    the backend chart      charts/pelican-panel/  the Panel chart
test/fakepanel/        in-memory Panel remote API for tests
test/spike/            M0 spike: agent + shim run the Paper egg in Docker without Kubernetes
test/e2e/              end-to-end suite against a deployed gateway
test/upstream/         route/remote-client/interface diffs against the pinned Wings module
hack/                  developer scripts (dev-push.sh builds, pushes and rolls a dev release)
```

## How it relates to Wings

Wings is used as an **unmodified dependency**: the agent imports
`github.com/pelican/wings` and registers a shim-backed process environment and
a Job-backed installer through four small opt-in hooks. Until those hooks are
merged upstream they live on the `pelican-k8s-hooks` branch of the
[Claiyc/wings](https://github.com/Claiyc/wings) fork, referenced from `go.mod`
with a `replace` directive. `test/upstream` diffs the pinned Wings route table,
remote client and `ProcessEnvironment` interface on every CI run, and a nightly
workflow does the same against Wings `main`.

## Contributing

Issues and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
Security issues: see [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Wings (MIT) is used unmodified as a dependency
(see [NOTICE](NOTICE)); the Panel (AGPL-3.0) is used unmodified as a separate service.
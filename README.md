# pelican-k8s

[![CI](https://github.com/Claiyc/pelican-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/Claiyc/pelican-k8s/actions/workflows/ci.yaml)
[![CodeQL](https://github.com/Claiyc/pelican-k8s/actions/workflows/codeql.yaml/badge.svg)](https://github.com/Claiyc/pelican-k8s/security/code-scanning)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/Claiyc/pelican-k8s/badge)](https://scorecard.dev/viewer/?uri=github.com/Claiyc/pelican-k8s)
[![Release](https://img.shields.io/github/v/release/Claiyc/pelican-k8s?include_prereleases&sort=semver)](https://github.com/Claiyc/pelican-k8s/releases)
[![Commits since release](https://img.shields.io/github/commits-since/Claiyc/pelican-k8s/latest/master?include_prereleases&sort=semver)](https://github.com/Claiyc/pelican-k8s/commits/master)
[![License](https://img.shields.io/github/license/Claiyc/pelican-k8s)](LICENSE)

**A Kubernetes-native backend for [Pelican Panel](https://pelican.dev).**
pelican-k8s presents a whole cluster to the Panel as a single Wings node.
Each game server becomes a `GameServer` custom resource, which an operator
runs as an ordinary pod with Wings embedded as a library. The Panel and the
eggs stay unmodified.

```
Panel ──Wings API──▶ gateway ──spec/status──▶ GameServer CR
                        │                           │
                        │ proxied data path         │ operator reconciles
                        ▼                           ▼
                      agent ◀──── unix socket ───▶ shim ──▶ egg entrypoint
              (Wings as a library)    (PID 1 of the game container)
```

| Component | What it does |
|---|---|
| **gateway** | Looks like one Wings node to the Panel. Turns Panel calls into `GameServer` changes and proxies console, files and SFTP to the right pod |
| **operator** | Reconciles each `GameServer` into a StatefulSet, PVC, Services, NetworkPolicy and install Jobs |
| **agent** | [Wings](https://github.com/pelican/wings) as a Go library, running as a sidecar for exactly one server |
| **shim** | Entrypoint of the game container: PTY, signals, exit codes, stats and the console buffer |

[ARCHITECTURE.md](ARCHITECTURE.md) describes how the system works.

## Quick start

You need Kubernetes ≥ 1.33, Helm 3, a StorageClass with volume expansion and
a way to expose the gateway: an Ingress or Route for its API, a `NodePort` or
`LoadBalancer` for SFTP. [docs/install.md](docs/install.md) is the full guide
with every option.

**1. Deploy the Panel.** Skip this if you already run one
([docs/panel.md](docs/panel.md) has the details).

```bash
helm install pelican-panel oci://ghcr.io/claiyc/pelican-k8s/charts/pelican-panel --version 0.1.2 \
  -n pelican --create-namespace \
  --set panel.url=https://panel.example.com \
  --set ingress.enabled=true --set 'ingress.hosts[0].host=panel.example.com'
kubectl -n pelican exec deploy/pelican-panel -- php artisan p:user:make --admin=1 \
  --email=you@example.com --username=admin --password='<password>'
```

**2. Create the node in the Panel** under Admin → Nodes → Create, then copy
`token_id` and `token` from its *Configuration* tab.

| Field | Value |
|---|---|
| FQDN | `wings.example.com` |
| Communicate over SSL, behind proxy | yes |
| Daemon port, daemon connect port | `8080`, `443` |
| SFTP port | `30022`, or your LoadBalancer port |

**3. Install pelican-k8s.**

```bash
helm install pelican-k8s oci://ghcr.io/claiyc/pelican-k8s/charts/pelican-k8s --version 1.0.2 \
  -n pelican-system --create-namespace \
  --set gateway.panelURL=https://panel.example.com \
  --set gateway.nodeTokenID=<token_id> --set gateway.nodeToken=<token> \
  --set gateway.ingress.enabled=true --set gateway.ingress.host=wings.example.com \
  --set defaultClass.spec.storage.storageClassName=<expandable storage class> \
  --set defaultClass.spec.exposure.mode=LoadBalancer
```

**4. Create a server in the Panel** as you would with Wings, and watch it
come up.

```bash
kubectl -n pelican-servers get gameservers
kubectl -n pelican-servers describe gameserver gs-<uuid>   # conditions and events
```

## Documentation

| Document | Contents |
|---|---|
| [docs/install.md](docs/install.md) | Installation, Panel node setup, exposure modes, ingress requirements, OpenShift |
| [docs/panel.md](docs/panel.md) | Deploying the Panel with the `pelican-panel` chart |
| [docs/classes.md](docs/classes.md) | `GameServerClass` reference: storage, exposure, networking, resources, security, installs |
| [docs/operations.md](docs/operations.md) | Day-2: inspecting servers, upgrades, backups and snapshots, troubleshooting |
| [docs/compatibility.md](docs/compatibility.md) | Wings feature parity and tested platforms |
| [docs/security.md](docs/security.md) | Trust boundaries, tokens, pod security, network policies |
| [docs/development.md](docs/development.md) | Building, testing, the code map, the Wings fork and its hooks, release process |
| [ARCHITECTURE.md](ARCHITECTURE.md) | How the system works: components, resources, flows, security |
| [docs/wings-panel-contract.md](docs/wings-panel-contract.md) | Wings ⇄ Panel protocol reference |

## How it relates to Wings

The agent imports [Wings](https://github.com/pelican/wings) as a Go module and
plugs into it through a few opt-in hooks, kept on a branch of the
[Claiyc/wings](https://github.com/Claiyc/wings) fork. CI checks compatibility
with the pinned Wings on every run and with the tip of that branch every
night. [docs/development.md](docs/development.md#wings-as-a-dependency) has
the details.

## Contributing

Issues and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
Security issues: see [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE). Wings (MIT) is a dependency (see
[NOTICE](NOTICE)); the Panel (AGPL-3.0) is used unmodified as a separate service.

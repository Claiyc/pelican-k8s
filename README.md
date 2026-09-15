# pelican-k8s (design)

A Kubernetes-native backend for [Pelican Panel](https://github.com/pelican/panel). It replaces
Docker-based Wings with a gateway, an operator and a per-pod agent. The Panel and eggs stay unmodified.

| Document | Contents |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | Architecture proposal: components, CRDs, flows, networking, storage, security, milestones |
| [docs/wings-panel-contract.md](docs/wings-panel-contract.md) | Verified Wings⇄Panel protocol reference (routes, JWTs, SFTP, websocket, payloads) with source citations |

Status: design draft, 2026-09-15. Nothing is implemented yet.

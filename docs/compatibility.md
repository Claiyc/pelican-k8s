# Compatibility and limitations

## Wings feature parity

| Feature | Status | Notes |
|---|---|---|
| Power, console, stats, done detection, stop command/signal | ✅ | Wings code, driven through the shim |
| Crash detection and auto restart | ✅ | Wings code; container-level exits (OOM) are relayed by the operator |
| File manager, compress/decompress, search, remote pull | ✅ | Wings code on the server volume |
| Signed downloads and uploads | ✅ | Gateway verifies with the node token, re-signs with the agent token; one-time use enforced by Wings |
| SFTP | ✅ | Gateway terminates SSH, authenticates against the Panel, relays to the agent's Wings SFTP server; activity rows carry the gateway address |
| Egg config file rewriting | ✅ | Wings parser; the default allocation IP is presented as `0.0.0.0`, the real IP as `SERVER_PUBLIC_IP` |
| Installs and reinstalls | ✅ | Kubernetes Job with the egg installer image; exit codes ignored unless `strictExitCode` |
| Backups (wings, s3) and restore | ✅ | Local archives are not durable across pod recreation |
| Activity log | ✅ | Per-agent SQLite on the volume, forwarded through the gateway |
| Suspension, deauthorize | ✅ | |
| Image change at next start | ✅ | Pod recreate at the next safe point, digest pinned |
| Live resource changes | ✅ | In-place pod resize; `ResizePending` when deferred |
| Disk limit | ⚠️ | Wings soft limit plus the PVC hard limit; PVCs cannot shrink |
| `/api/system` Docker fields, image prune, docker disk | ⚠️ | Stubbed |
| Container hostname | ⚠️ | `gs-<uuid>-0`; `/etc/machine-id` is provided |
| Server transfers | ❌ | One gateway is one node; nothing to transfer to |
| Panel mounts | ❌ | |
| `force_outgoing_ip`, swap, `io_weight`, `threads`, OOM-killer disable | ❌ | Not expressible per pod |
| Console history across a container-level OOM kill | ⚠️ | The whole container restarts; the ring buffer is lost |

## Kubernetes and OpenShift

Tested on OpenShift 4.22 (Kubernetes 1.35, OVN-Kubernetes, Longhorn, CoreDNS on
5353) as a single node with `NodePort` exposure. Other distributions should
work; report issues with the distribution and CNI.

## Deviations from ARCHITECTURE.md

The implementation follows the design; these details differ or were added:

| Topic | Design | Implementation |
|---|---|---|
| Agent routing | EndpointSlices | The pod object (also gives container status); same semantics |
| Panel config in the CR | typed process configuration | `spec.panel.processConfiguration` is the Panel's raw JSON (Wings' typed struct does not round-trip) |
| Env Secret | egg variables | egg variables plus the derived `STARTUP`, `SERVER_MEMORY`, `SERVER_IP`, `SERVER_PORT`, `SERVER_PUBLIC_IP`, `TZ`, so install Jobs see what Wings' installer sees |
| Operator status writes | update | JSON merge patches of changed fields only; the object is read through the API (not the cache) so a reconcile never acts on a status older than the one it wrote |
| Install Job prepare step | root | runs as the game UID (owns the volume layout); the script container runs as root with the runtime's default capability set |
| Seccomp on install pods | RuntimeDefault | optional `install.disableSeccomp` (OpenShift `anyuid` rejects seccomp) |
| DNS egress | port 53 | 53 and 5353 (CoreDNS pod port on OpenShift) |
| Gateway → Panel TLS | | optional extra CA (`gateway.extraCA`) |
| Agent internal routes | added to the gin engine | served by a mux in front of gin (Wings registers its auth middleware engine-wide) |
| Failed install Jobs | deleted | kept until their TTL for log inspection; successful ones are removed |
| Pod recreate mid-install | | the install is requested again from the new agent |
| Late exit relays | | ignored when the process was restarted after the container came back |

## Upstream tracking

- `test/upstream` fails when the pinned Wings module adds routes, remote calls
  or `ProcessEnvironment` methods the gateway/agent do not handle.
- The `Upstream drift` workflow runs the same checks nightly against the tip of
  the fork's `pelican-k8s-hooks` branch (upstream `main` is not usable while the
  four hooks are unmerged: they only exist on the branch) and runs
  `hack/check-panel-contract.sh` against the latest Panel image for the contract
  points the gateway depends on.
- The `Contract` workflow is the real gate: a kind cluster with the actual
  Panel and this repository's components runs the server lifecycle through
  the Panel's own services on every PR, and nightly against the newest Panel
  image, so a Panel change that breaks the contract fails within a day.
- Dependabot keeps Go modules, actions and base images current; the nightly
  workflow opens an issue when a new Panel release appears. Wings itself is
  bumped by hand (fork branch rebase + upstream tests).

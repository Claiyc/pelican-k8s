# GameServerClass reference

A `GameServerClass` is cluster-scoped, admin-owned policy. Every `GameServer`
references one through `spec.className` (the gateway writes the chart's
default class name). Panel settings decide *what* a server is; the class
decides *how* it runs on this cluster. Changing a class re-renders every server
using it; changes that need a new pod are applied at the next safe point (when
the process is offline).

```yaml
apiVersion: pelican-k8s.io/v1alpha1
kind: GameServerClass
metadata:
  name: default
spec:
  storage: {...}
  exposure: {...}
  network: {...}
  resources: {...}
  security: {...}
  install: {...}
  failover: {...}
  imageResolution: {...}
  images: {...}
```

## storage

| Field | Default | Meaning |
|---|---|---|
| `storageClassName` | cluster default | Must allow volume expansion |
| `defaultSizeGiB` | 20 | Used when the Panel `disk_space` is 0 (unlimited) |
| `overheadPercent` | 10 | PVC = `disk_space × (1 + overhead)`: room for logs, activity DB, install output |
| `scratch.type` | `Ephemeral` | `Ephemeral` (generic ephemeral volume) or `EmptyDir` for backup archives and agent temp files |
| `scratch.storageClassName` | `storageClassName` | Class of the ephemeral scratch volume |
| `scratch.sizeGiB` | 0 = PVC size | An archive can be as large as the server |
| `deletionPolicy` | `Delete` | `Delete`, `Retain` (PVC orphaned and labelled `pelican-k8s.io/orphaned-at`) or `SnapshotThenDelete` |
| `volumeSnapshotClassName` | | Required for `SnapshotThenDelete` and `snapshotSchedule` |
| `snapshotSchedule` | | Cron expression for crash-consistent `VolumeSnapshot`s per server |
| `snapshotRetain` | 7 | Scheduled snapshots kept per server |

Volumes never shrink: a smaller Panel `disk_space` sets the `DiskShrinkRefused` condition.

## exposure

| Field | Default | Meaning |
|---|---|---|
| `mode` | `LoadBalancer` | `LoadBalancer`, `NodePort` or `HostPort` (see install.md) |
| `externalTrafficPolicy` | `Local` | Preserves client IPs |
| `loadBalancer.provider` | | `metallb` supplies the two annotation keys below; empty adds none |
| `loadBalancer.ipAnnotation` | | Annotation set to the allocation IP (`metallb.io/loadBalancerIPs`) |
| `loadBalancer.sharingAnnotation` | | Annotation allowing several Services to share an IP (`metallb.io/allow-shared-ip`) |
| `loadBalancer.annotations` | | Added verbatim to exposure Services |
| `externalIPs` | | Addresses reported to the Panel for allocations |

Invariant: container port = Service port = external port = Panel allocation
port, so `SERVER_PORT` always matches what players connect to.

## network

| Field | Default | Meaning |
|---|---|---|
| `enabled` | true | Create the per-server NetworkPolicy |
| `blockedEgressCIDRs` | [] | Pod CIDR, service CIDR, node and LAN ranges game pods must not reach (link-local is always blocked) |
| `nodeCIDRs` | [] | Node addresses admitted on the agent port for kubelet probes on CNIs without implicit host access |
| `inClusterEgress.gameServers` | true | Game pods may reach other game pods (proxies such as Velocity) |
| `inClusterEgress.additional` | [] | `{cidr, ports}` allowances (in-cluster S3, databases) |

## resources

| Field | Default | Meaning |
|---|---|---|
| `memoryOverheadPercent` | 5 | Memory limit = Panel `memory_limit` × (1 + overhead); request = `memory_limit` |
| `cpuRequestPercentOfLimit` | 25 | CPU request as a share of the limit (overcommit) |
| `memoryRequestPercentOfLimit` | 100 | Share of `memory_limit` reserved. 100 guarantees it; lower overcommits (memory is incompressible, so an over-full node evicts rather than throttles) |
| `unlimitedMemoryMiB` | 4096 | Used when `memory_limit` is 0; the agent also reports it as `SERVER_MEMORY` |
| `unlimitedCpuPercent` | 0 | Used when `cpu_limit` is 0; 0 means no CPU limit |
| `minCpu` | 100m | Minimum CPU request |
| `tmpSizeMiB` | 100 | `/tmp` tmpfs in the game container |
| `agent.cpu` / `agent.memory` / `agent.memoryLimit` | 50m / 128Mi / 512Mi | Agent sidecar resources (compress/decompress of large servers needs headroom) |

Panel `swap`, `io_weight`, `threads` and OOM-killer disable are not expressible per pod and are ignored.

## security

| Field | Default | Meaning |
|---|---|---|
| `runAsUser` | 1000 | Pinned UID and GID (also `fsGroup`) of game pods |
| `generatePasswdEntry` | true | Generate `/etc/passwd` and `/etc/group` with a `container` user for that UID inside the egg image |
| `useNamespaceUIDRange` | false | OpenShift: use the start of the namespace's `openshift.io/sa.scc.uid-range` instead |

## install

| Field | Default | Meaning |
|---|---|---|
| `serviceAccountName` | `pelican-installer` | Job ServiceAccount |
| `resources.cpu` / `resources.memory` | 1 / 1Gi | Job gets `max(server, these)` like Wings' `installer_limits` |
| `strictExitCode` | false | Wings ignores script exit codes; `true` fails the install on non-zero |
| `activeDeadlineSeconds` | 3600 | Job time limit |
| `prepareTimeoutSeconds` | 600 | How long to wait for the agent to hold the install lock before failing |
| `runAsRoot` | true | Egg scripts assume root (`apt`, `chown`); the server directory is chowned to the game UID afterwards |
| `disableSeccomp` | false | Leave the seccomp profile unset on install pods (OpenShift `anyuid`) |

## failover

| Field | Default | Meaning |
|---|---|---|
| `forceDeleteAfter` | unset | Force-delete a pod stuck `Terminating` on a `NotReady` node after this duration (only safe with storage fencing) |

## imageResolution

The shim is the game container's entrypoint, so the operator needs the egg
image's original `ENTRYPOINT`+`CMD`:

| Field | Default | Meaning |
|---|---|---|
| `entrypointOverrides` | yolks, steamcmd and games images → `["/bin/bash", "/entrypoint.sh"]` | Glob on the image reference → argv; exact match wins, then the longest pattern |
| `registryLookup` | false | Resolve the image config from the registry (needs egress and `pullSecrets`) |
| `pullSecrets` | [] | Registry credentials in the servers namespace |
| `pinDigest` | true | Resolve the tag to a digest at every pod creation (Wings' pull-on-start); `~image` (never pull) disables it |

Without an override or registry lookup, an init container running the egg
image itself checks for the yolk convention (`/entrypoint.sh` run by bash) and
fails the pod with a clear message otherwise.

## Other fields

| Field | Default | Meaning |
|---|---|---|
| `images.shim` / `images.agent` / `images.pullPolicy` | chart | pelican-k8s images injected into game pods |
| `serviceAccountName` | `pelican-game` | Game pod ServiceAccount (`-hostport` suffix added in HostPort mode) |
| `agentConfigMap` | `pelican-agent-config` | Agent `config.yml` |
| `suspendScalesToZero` | false | Delete the pod of a suspended server |
| `terminationGracePeriodSeconds` | 660 | Must exceed Wings' 10-minute stop wait |
| `nodeSelector` / `tolerations` / `priorityClassName` | | Applied to game pods and install Jobs |

## Multiple classes

Create more classes (`kubectl apply`) and point servers at them by editing
`spec.className` on the `GameServer` (the gateway only sets it on creation).
Typical uses: a class per storage tier, a `HostPort` class for a few legacy
ports, or a class with `snapshotSchedule` for production servers.

# GameServerClass reference

A `GameServerClass` is cluster-scoped, admin-owned policy. Every `GameServer`
references one through `spec.className` (the gateway writes the chart's
default class name). Panel settings decide *what* a server is; the class
decides *how* it runs on this cluster. Changing a class re-renders every server
using it. A stopped server has no game pod and picks a game pod change up at
its next start; a running one gets a new game pod once the process is offline.
A change to the agent pod (agent image or resources, scratch type, security)
replaces the agent pod once the process is offline and the agent has no
in-flight work.

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
  scheduling: {...}
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
| `scratch.type` | `Ephemeral` | `Ephemeral` (generic ephemeral volume) or `EmptyDir` for backup archives and agent temp files; part of the agent pod |
| `scratch.storageClassName` | `storageClassName` | Class of the ephemeral scratch volume |
| `scratch.sizeGiB` | 0 = PVC size | An archive can be as large as the server; a changed size applies when the agent pod is next replaced |
| `deletionPolicy` | `Delete` | `Delete`, `Retain` (PVC orphaned and labelled `pelican-k8s.io/orphaned-at`) or `SnapshotThenDelete` |
| `volumeSnapshotClassName` | cluster default | `VolumeSnapshotClass` for `SnapshotThenDelete` and `snapshotSchedule` |
| `snapshotSchedule` | | Cron expression for crash-consistent `VolumeSnapshot`s per server |
| `snapshotRetain` | 7 | Scheduled snapshots kept per server |

Volumes never shrink: a smaller Panel `disk_space` sets the `DiskShrinkRefused` condition.

The volume is RWO and mounted by the agent pod and the game pod, so both run on
one node. With network block storage the pair can move between nodes from one
start to the next; with node-local storage it stays where the volume is. See
[scheduling](#scheduling).

## exposure

| Field | Default | Meaning |
|---|---|---|
| `mode` | `LoadBalancer` | `LoadBalancer`, `NodePort` or `HostPort` (see install.md). `HostPort`, and `NodePort` with `externalTrafficPolicy: Local`, run the game pod (and with it the agent pod) on the node whose InternalIP or ExternalIP is the allocation IP |
| `externalTrafficPolicy` | `Local` | `Local` keeps the players' IP addresses and ties the game pod to where traffic arrives; `Cluster` lets any node forward but hides the players' addresses. See [Local or Cluster](#local-or-cluster) |
| `loadBalancer.provider` | | `metallb` supplies the two annotation keys below; empty adds none |
| `loadBalancer.ipAnnotation` | | Annotation set to the allocation IP (`metallb.io/loadBalancerIPs`) |
| `loadBalancer.sharingAnnotation` | | Annotation allowing several Services to share an IP (`metallb.io/allow-shared-ip`), so servers on one IP can use different ports |
| `loadBalancer.annotations` | | Added verbatim to exposure Services |
| `externalIPs` | | Addresses reported to the Panel for allocations |

Invariant: container port = Service port = external port = Panel allocation
port, so `SERVER_PORT` always matches what players connect to.

### Local or Cluster

`externalTrafficPolicy` decides what a node does with player traffic for a game
pod that runs elsewhere.

- **`Local`** (default): a node only delivers to game pods on itself. The packet
  reaches the game server unchanged, so it sees each player's real IP address.
  The price is placement: the address must arrive where the pod runs.
- **`Cluster`**: any node accepts the traffic and kube-proxy forwards it to the
  pod, wherever it runs. To get the replies back through the same node it
  rewrites the source address, so the game server sees a node's address for
  every player.

What the player's address is used for: IP bans and whitelists, per-IP
connection limits and rate limits, anti-cheat and geo checks, and the
addresses in server logs and the console. Under `Cluster` all of these see the
same few node addresses, so a ban on one player can lock out everyone coming
through that node. Some CNIs keep the source address under `Cluster` (Cilium
with DSR, for example); there `Cluster` loses nothing.

What each mode means per exposure mode:

| `mode` | `Local` | `Cluster` |
|---|---|---|
| `LoadBalancer` | The load balancer sends traffic only to nodes running the game pod. With a sharing annotation, all servers on one IP run on one node, because MetalLB shares a `Local` address only between Services with identical selectors and announces it from one node; a running server found elsewhere (after the class or its allocation changed) is stopped and moved there, since it receives no traffic where it is; a server that does not fit there stays `Pending` | Servers on one IP spread over any nodes; the announcing node forwards to them. Nothing waits for room on a particular node |
| `NodePort` | The game pod runs on the node that owns the allocation IP (`AllocationIPNotOnNode` when none does) | Every node forwards the port, the game pod runs anywhere |
| `HostPort` | No Service; the setting has no effect | No Service; the setting has no effect |

Choose `Local` when players' addresses matter, which is most public game
servers. Choose `Cluster` when they do not matter (a private group of friends, a
LAN) or your CNI keeps them anyway, and spreading servers over nodes is worth
more. The setting can change at any time; the operator
updates the Services, and the game pods pick up new placement when they are
recreated. The exception is a server sharing its address under `Local`: it is
moved to the others' node right away.

#### Planning addresses under `Local`

With `LoadBalancer` and `Local`, the allocation decides placement: the servers
on one IP run on one node, the node MetalLB announces that IP from. Kubernetes
cannot spread them, and the operator never changes allocations to make room;
they stay the Panel admin's choice, because players, DNS records and firewall
rules depend on them. So plan addresses like nodes:

- Give a server its own IP when it should be free to run anywhere. A MetalLB
  pool with an address per server removes the constraint entirely.
- Put servers on one IP only when they fit on one node together: their memory
  and CPU requests add up on that node.
- When a server on a shared IP does not fit, it stays `Pending`
  (`GamePodReady=False`, `Unschedulable`). Free room on that node, or move the
  server to another IP in the Panel.
- A volume bound to a node (local-path and other node-local storage) keeps a
  server on that node. A server whose volume lives elsewhere cannot join the
  others on its IP and stays `Pending`; give it its own IP instead.
- Servers whose players' addresses do not matter can use a class with
  `Cluster`, which has none of these constraints.

## network

| Field | Default | Meaning |
|---|---|---|
| `enabled` | true | Restrict the server's pods with per-server NetworkPolicies (one for the game pod, one for the agent pod). `false` makes both policies admit all traffic, which also lifts the chart's `default-deny` for these pods |
| `blockedEgressCIDRs` | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10` | IPv4 ranges excluded from the internet egress of game and agent pods; the defaults cover the usual pod, service, node and LAN ranges. Link-local is blocked as well; `[]` blocks only link-local. DNS, the gateway remote API (agent pods), the shim port and `inClusterEgress` are separate allow rules and reach these ranges regardless. The chart's `install-jobs` policy uses the default class's list |
| `nodeCIDRs` | [] | Node addresses admitted on the agent port for kubelet probes on CNIs without implicit host access |
| `inClusterEgress.gameServers` | true | Game pods may reach other game pods (proxies such as Velocity) |
| `inClusterEgress.additional` | [] | `{cidr, ports}` allowances for game and agent pods (in-cluster S3, databases) |

## resources

| Field | Default | Meaning |
|---|---|---|
| `memoryOverheadPercent` | 5 | Memory limit = Panel `memory_limit` × (1 + overhead/100) |
| `cpuRequestPercentOfLimit` | 25 | CPU request as a share of the limit (overcommit) |
| `memoryRequestPercentOfLimit` | 100 | Share of `memory_limit` reserved. 100 guarantees it; lower overcommits (memory is incompressible, so an over-full node evicts rather than throttles) |
| `unlimitedMemoryMiB` | 4096 | Used when `memory_limit` is 0; the agent also reports it as `SERVER_MEMORY` |
| `unlimitedCpuPercent` | 0 | Used when `cpu_limit` is 0; 0 means no CPU limit |
| `minCpu` | 100m | Minimum CPU request |
| `tmpSizeMiB` | 100 | `/tmp` tmpfs in the game container |
| `agent.cpu` / `agent.memory` / `agent.memoryLimit` | 50m / 128Mi / 512Mi | Agent pod resources (compress/decompress of large servers needs headroom). This is all a stopped server requests |

The game values apply to the game pod, which exists only while the server is on.
Panel `swap`, `io_weight`, `threads` and OOM-killer disable are not expressible per pod and are ignored.

## scheduling

| Field | Default | Meaning |
|---|---|---|
| `preferAgentNode` | true | The game pod prefers the node its agent pod runs on. It lands there when it fits and elsewhere when it does not, and the agent then moves to it. `false` gives the agent's node no advantage, so the agent moves more often |

The scheduler places the game pod at every start, and the agent pod follows it
to its node. While the agent has in-flight work (file transfers, backups,
restores, installs, active SFTP sessions) a start is bound to the agent's node
instead and waits there if the node is full. See ARCHITECTURE.md §7.7.

## security

| Field | Default | Meaning |
|---|---|---|
| `runAsUser` | 1000 | Pinned UID and GID (also `fsGroup`) of game and agent pods |
| `generatePasswdEntry` | true | Generate `/etc/passwd` and `/etc/group` with a `container` user for that UID inside the egg image |
| `useNamespaceUIDRange` | false | OpenShift: use the start of the namespace's `openshift.io/sa.scc.uid-range` instead |

## install

| Field | Default | Meaning |
|---|---|---|
| `serviceAccountName` | `pelican-installer` | Job ServiceAccount |
| `resources.cpu` / `resources.memory` | 1 / 1Gi | Job limits are `max(server, these)` like Wings' `installer_limits`; the CPU value applies only when the server has a CPU limit. Requests equal the server's |
| `strictExitCode` | false | Wings ignores script exit codes; `true` fails the install on non-zero |
| `activeDeadlineSeconds` | 3600 | Job time limit |
| `prepareTimeoutSeconds` | 600 | How long to wait for the agent to hold the install lock before failing |
| `runAsRoot` | true | Egg scripts assume root (`apt`, `chown`); the server directory is chowned to the game UID afterwards |
| `disableSeccomp` | false | Leave the seccomp profile unset on install pods (OpenShift `anyuid`) |

## failover

| Field | Default | Meaning |
|---|---|---|
| `forceDeleteAfter` | unset | Force-delete a server's pods stuck `Terminating` on a `NotReady` node after this duration (only safe with storage fencing) |

## imageResolution

The shim is the game container's entrypoint, so the operator needs the egg
image's original `ENTRYPOINT`+`CMD`:

| Field | Default | Meaning |
|---|---|---|
| `entrypointOverrides` | yolks, steamcmd and games images → `["/bin/bash", "/entrypoint.sh"]` | Glob on the image reference → argv; exact match wins, then the longest pattern |
| `registryLookup` | false | Resolve the image config from the registry with the operator's registry credentials (needs operator egress to the registry) |
| `pullSecrets` | [] | `imagePullSecrets` of agent pods, game pods and install Jobs (Secrets in the servers namespace) |
| `pinDigest` | true | Resolve the tag to a digest (operator's registry credentials) whenever a game pod is created, which is every start from stopped; a restart reuses the pod. `~image` (Wings' never pull) disables it |

Without an override or registry lookup, an init container running the egg
image itself checks for the yolk convention (`/entrypoint.sh`, run by
`/bin/bash`, or `/bin/sh` when the image has no bash) and fails the game pod
with a clear message otherwise.

## Other fields

| Field | Default | Meaning |
|---|---|---|
| `images.shim` / `images.agent` / `images.pullPolicy` | chart | pelican-k8s images of game and agent pods |
| `serviceAccountName` | `pelican-game` | Game pod ServiceAccount (`-hostport` suffix added in HostPort mode) |
| `agentServiceAccountName` | `pelican-agent` | Agent pod ServiceAccount |
| `agentConfigMap` | `pelican-agent-config` | Agent `config.yml` |
| `suspendScalesToZero` | false | Run no agent pod for a suspended server (it never has a game pod) |
| `terminationGracePeriodSeconds` | 660 | Must exceed Wings' 10-minute stop wait |
| `nodeSelector` / `tolerations` | | Applied to game pods, agent pods and install Jobs |
| `priorityClassName` | | Priority class of game pods |
| `agentPriorityClassName` | `pelican-agent` | Priority class of agent pods. It must rank above the game pods', so an agent that follows its game pod to a full node can preempt lower-priority pods there; it stays `Pending` when the node has nothing the scheduler may evict |

## Multiple classes

Create more classes (`kubectl apply`) and point servers at them by editing
`spec.className` on the `GameServer` (the gateway only sets it on creation).
Typical uses: a class per storage tier, a `HostPort` class for a few fixed
ports, or a class with `snapshotSchedule` for production servers.

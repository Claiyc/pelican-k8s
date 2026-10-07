# pelican-k8s v2 plan

**Temporary document.** [ARCHITECTURE.md](../ARCHITECTURE.md) and the other docs describe the
split-pod design as the system is meant to work. The code on `master` still implements the
single-pod design of 1.x. This file lists the work that closes the gap. Delete it when Part 2 has
shipped.

## 1. What changes

| | 1.x (code today) | v2 (documented) |
|---|---|---|
| Pods per server | one pod `gs-<uuid>-0`: agent as native sidecar, game container | agent pod `gs-<uuid>-agent-0` (always), game pod `gs-<uuid>-0` (only while on) |
| Stopped server | pod stays, game container idle, full requests reserved, `1/2` Ready | no game pod; only the agent's requests |
| Start | `POST /power start` into the running pod | create the game pod, place the pair, wait for the shim, then `POST /power start` |
| Shim ↔ agent | unix socket on a shared emptyDir, agent challenges shim | TCP 8082 through `gs-<uuid>-agent`, mutual challenge, shim sends its pod UID |
| Graceful stop on pod deletion | game container `preStop` calls the agent; native sidecar ordering keeps the agent alive | the shim stops the process itself with the stop configuration it holds |
| Game readiness | `httpGet` to the agent in the same pod | `shim ready`, from the state the agent pushes to the shim |
| Image, port and class changes | `RecreatePending` also for stopped servers | stopped servers need nothing; running ones as before |
| Placement | one pod, scheduled once, stays | scheduler places the game pod at every start; the agent follows |
| NetworkPolicy | one per server; game process shares the agent's egress (remote API port) | one per pod; the game pod reaches only its agent's shim port |
| Agent pod replaced | console and SFTP sessions drop | consoles and HTTP calls carried across by the gateway (Part 2) |

## 2. Decisions taken

- **Split, not resize.** Shrinking a stopped pod's requests in place fixes the reservation but
  keeps the `1/2` state and one scheduling unit, and a start on a node that filled up needs a pod
  recreate anyway.
- **Same node, because RWO.** Wings' file manager, SFTP, parsers and backups read the volume
  locally, and the game needs the same volume. RWX is not part of the design: on most clusters it
  means NFS semantics under game worlds and under Wings' SQLite activity database.
- **The game pod anchors, the agent follows.** One rule covers the full-node case, drains and lost
  pods; there is no Unschedulable detection and no timeout.
- **Preferred affinity toward the agent's node** (`scheduling.preferAgentNode`, default on) keeps
  agent moves rare. It is a boolean and not a weight: with a single preferred term the scheduler
  normalises the score, so any weight behaves the same.
- **Required affinity while the agent has in-flight work**, so a move never breaks a transfer,
  backup, restore, install or active SFTP session.
- **Agent priority class above game pods**, so the agent always fits on the game pod's node.
- **No live migration.** Moving a running server is stop, reschedule, start.
- **No migration path from 1.x.** 2.0 is a major release. The operator replaces every 1.x pod at
  upgrade; a running server is stopped through the 1.x `preStop` path and started in the new layout.
  There is no fallback to the old pod in the gateway or the operator.
- **`desired` follows the Panel.** Under Wings a crash that is not restarted leaves the server
  offline, in the Panel and in `states.json`. The gateway therefore sets `desired: Stopped` when a
  process has stayed offline for a minute, and the game pod goes.
- **Storage is a class choice.** Network block storage gives mobility; node-local storage pins the
  pair to the volume's node and needs no special handling.

## 3. Part 1: split pods (main)

The steps are ordered so that each one can merge to `master` and ship on its own. Steps 1 and 2
change nothing visible; step 3 is the breaking change that makes the release 2.0.0.

### Step 1: shim protocol over TCP, inside the single pod

- `internal/shim/protocol`: TCP listener and dialer next to the unix ones; mutual HMAC challenge;
  the shim's pod UID in the handshake; bump `Version`.
- `cmd/shim`: `--agent host:port` in place of `--socket`. `cmd/agent`: `--shim-listen`.
- The pod template points the shim at `127.0.0.1:8082`.
- Tests: protocol unit and fuzz tests on the new handshake; `shimenv` tests against a live
  supervisor over TCP; the spike.

### Step 2: the shim stops and reports readiness by itself

- Protocol: `stop` in `start`, `configure{stop}`, `state{value}`, the `terminating` event.
- `internal/shim/supervisor`: on SIGTERM run the stop configuration, wait until 20 s before the
  grace period ends, then SIGTERM and SIGKILL; write and remove `/pelican/run/ready`.
- `shim ready` subcommand; the game container's readiness probe becomes `exec`.
- `internal/agent/shimenv`: forward `SetStopConfiguration`, push state changes, map `terminating`
  to `stopping`.
- `internal/agent/routes`: `/internal/v1/prestop` waits only while the shim is terminating (10 s to
  see it); remove `/internal/v1/ready`; add `GET /internal/v1/shim` and `GET /internal/v1/activity`.
- Remove the game container's `preStop` hook; `/pelican/run` becomes read-write in the game
  container.

### Step 3: two pods, the game pod always present

The layout changes, the lifecycle does not yet: the game StatefulSet stays at 1 replica and the
game pod has a required pod affinity to the agent pod.

- `api/v1alpha1`: `agentServiceAccountName`, `agentPriorityClassName`, `scheduling.preferAgentNode`;
  status `agent.node`, `agent.templateHash`, `agent.sftpActiveAt`, `game.{podUID,node}`; conditions
  `GamePodReady`, `AgentRelocating`. Regenerate the CRDs.
- `internal/operator/names`, `render`: agent StatefulSet `gs-<uuid>-agent`, game StatefulSet
  `gs-<uuid>`, label `pelican-k8s.io/component: agent|game`, agent Service with port 8082, exposure
  Service selecting the game pod, two NetworkPolicies, `INTERNAL_IP` from the game pod's own IP,
  `prepare` without `--data` in the game pod, install Job affinity to the agent pod, template hashes
  per pod without affinity.
- `internal/operator/controller`: agent readiness from the agent pod; fresh-pod rule on both pod
  UIDs, gated on `GET /internal/v1/shim`; `RecreatePending` per template; OOM relay from the game
  pod; `NodeLost` and the finalizer for both pods.
- `internal/gateway`: route to `gs-<uuid>-agent-0`; the `container/status` rule looks at both pods
  for "terminating"; diagnostics.
- `charts/pelican-k8s`: ServiceAccount `pelican-agent`, admission policy binding, OpenShift SCC
  binding, PriorityClass `pelican-agent`, default class values.
- Upgrade from 1.x: the operator deletes every pod of the old layout (a `gs-<uuid>-0` with an
  `agent` container) as soon as it runs; no compatibility code.

### Step 4: a game pod per run

- Game StatefulSet replicas follow `spec.power.desired` and `status.process.state` (§7.6).
- Start: render the template (digest resolved now), scale to 1, wait for the shim, issue `start`.
- Stop: scale to 0 once the process is `offline` and `desired` is `Stopped`.
- `internal/gateway`: the 15 s check that sets `desired: Stopped` for a process that stayed offline
  for a minute after a crash (ARCHITECTURE.md §8.3).
- Resize and `RecreatePending` only when a game pod exists; drop the paths that recreate a stopped
  server's pod.
- Tests: reconciler tests for every row of the §7.6 tables; e2e start, stop, console stop,
  suspension, reinstall, crash restart, OOM.

### Step 5: placement

- Game pod: preferred pod affinity; required while the agent has in-flight work
  (`/internal/v1/activity`, `status.backups.pending`, install in progress, `sftpActiveAt`).
- Agent pod: node affinity to the game pod's node while one exists; `AgentRelocating`; delete and
  let the StatefulSet recreate; remove the affinity when the game pod is gone.
- `internal/gateway/sftprelay`: write `status.agent.sftpActiveAt`.
- Tests: reconciler tests with a fake client for same node, other node, busy agent, busy agent on a
  full node, lost agent pod, lost game pod, drain.
- Live suite: step 6.

### Step 6: live placement suite

Reconciler tests prove the operator decides correctly; they cannot show that the scheduler, the
StatefulSet controller and kubelet then do what the design assumes. A live suite covers that and
is where later scheduling rules get their tests.

- **Workflow** *Placement* (`.github/workflows/placement.yaml`, `hack/e2e-placement.sh`): on pull
  requests that touch `internal/operator`, `internal/agent`, `internal/shim`, `api` or
  `charts/pelican-k8s`, on pushes to `master`, and nightly.
- **Cluster:** kind with one control-plane node and three workers, the same node image as the
  *Contract* workflow.
- **Storage:** a StorageClass whose volumes are not bound to a node. kind's default local-path
  volumes carry node affinity, so the pair could never move; the suite configures the local-path
  provisioner with a shared directory (`sharedFileSystemPath`) mounted into every kind node, or an
  in-cluster NFS CSI driver if that option does not hold up.
- **No Panel, no real game.** The suite creates `GameServer` objects itself and drives them through
  `spec.power`; the gateway runs against `test/fakepanel`. The egg image is a few lines of shell
  that print a done line, answer a stop command and can be told to exit or to ignore SIGTERM.
- **Forced outcomes.** A node is made unfit by cordoning it or by a placeholder pod that requests
  most of its memory, so "the game pod lands elsewhere" never depends on scheduler scoring.
- **Scenarios** (`test/placement`, build tag `placement`):

  | Scenario | Asserts |
  |---|---|
  | Start, stop | game pod appears on the agent's node, becomes Ready on the done line, is gone after the stop; the agent pod is never recreated |
  | Start with the agent's node full | game pod on another node, `AgentRelocating`, agent pod recreated there, `start` issued only after both are on one node |
  | The same while the agent is busy | game pod `Pending` on the agent's node; released when the work ends |
  | `preferAgentNode: false` | no pod affinity on the game pod |
  | Drain of the pair's node | the process gets its stop command, both pods come back on one node, the server runs again |
  | Agent pod deleted while the game runs | the process keeps its PID, the agent returns on the same node and re-attaches |
  | Game pod deleted while running | graceful stop by the shim, new game pod, server runs again |
  | Crash without restart | `desired: Stopped` after a minute, game pod gone |
  | Stopped server, agent's node drained | agent reschedules without a node affinity |
  | Allocation-IP node affinity (NodePort with `Local`) | game pod on the node owning the IP, agent follows |

- **Not covered here:** the detach and attach of a real block device. With shared storage a game
  pod mounts the volume before the agent has left the old node, so this suite cannot show a volume
  that is still attached elsewhere (`GamePodReady=False`, `WaitingForVolume`) or how long a move
  takes. Step 7 covers that.

### Step 7: nightly block-storage suite

The same scenarios on real nodes with real block storage, so no release depends on a manual run.

- **Workflow** *Placement (block storage)*: nightly and on demand (`workflow_dispatch`), on a
  GitHub-hosted Linux runner. These expose `/dev/kvm` (a udev rule gives the runner user access)
  and have 4 vCPU and 16 GB RAM for public repositories.
- **Cluster:** three KVM virtual machines on the runner (Ubuntu cloud image, cloud-init installs
  `open-iscsi`), k3s with one server and two agents, Longhorn with one replica per volume. Unlike
  kind nodes, each VM has its own kernel, so a volume is attached to exactly one node.
- **Suite:** `test/placement` with the StorageClass as a parameter, plus the block-only
  assertions: the game pod waits with `WaitingForVolume` while the agent still holds the volume,
  the move completes within a bound, and data written before the move is there after it.
- **Budget:** three VMs with 2 vCPU and 4 GB each fit the runner's memory. The 14 GB disk does
  not hold three VM disks plus the images, so the job first removes preinstalled toolchains.
- **Spike first.** Bringing up the VMs, k3s and Longhorn on a hosted runner is unproven here: do
  it as a throwaway workflow before step 5 and record the run time. If it does not hold up
  (time, disk, flakiness), the fallback is the same job against three short-lived cloud VMs,
  which needs cloud credentials.

### Release

- `CHANGELOG.md`: breaking changes (pod names and containers in `kubectl` commands, class fields,
  NetworkPolicy names, local backups lost on agent moves).
- Upgrade notes in `docs/operations.md` for 1.x → 2.0.

## 4. Part 2: seamless agent replacement (follow-up)

Until this ships, an agent pod replacement drops that server's consoles and file-manager calls, and
the Panel UI reconnects by itself. ARCHITECTURE.md §5.9 describes the behaviour after Part 2.

- `internal/gateway/wsproxy`: keep the browser side open when the agent side ends because the agent
  pod is terminating or gone; drop client frames except `set state`; redial the new agent; send
  `token expiring`; forward the fresh `auth`. Close with 1013 after `gateway.agentWait`.
- `internal/gateway/agents`: proxied HTTP calls wait up to 10 s for the new agent before `503`.
- Chart value `gateway.agentWait` (default 120 s).
- Tests: gateway tests with a fake agent that goes away and comes back at a new address; e2e with a
  forced agent pod deletion while a console is open.

## 5. Open decisions and things to verify

1. **Panel UI and `token expiring`.** Part 2 relies on the frontend answering `token expiring` with
   a fresh `auth` on the same socket. Confirm against the pinned Panel before building it; if it
   does not, the gateway has to ask for the token another way or the socket has to reconnect.
2. **What `/internal/v1/activity` can see.** In-flight HTTP file requests are countable with
   middleware in the agent's `ServeMux`. Remote pulls, backups and restores run in Wings after the
   request returned; check what Wings exports for them (`downloader`, server flags) before adding
   a hook to the fork.
3. **Agent preemption.** The agent's priority class lets a 128 Mi agent pod preempt a game pod of
   another server on a full node. Check that this is acceptable, or reserve agent headroom per
   node instead.
4. **Drain ordering.** The agent's `preStop` gives the shim 10 s to report `terminating`. Measure
   how far apart `kubectl drain` and the eviction API delete the two pods.
5. **Start latency.** Measure scheduling, image check, init containers and, on a move, detach and
   attach (Longhorn, Ceph) against the 1.x start.
6. **Volume ownership on every start.** `fsGroupChangePolicy: OnRootMismatch` should make the game
   pod's mount cheap; confirm on a large volume, and on OpenShift that both pods get the same
   SELinux MCS label (the install Job already shares the volume this way).
7. **Agent SFTP host key.** Confirm it is on the PVC and survives an agent move; otherwise the
   pinned `status.agent.sftpHostKey` has to be reset on relocation.

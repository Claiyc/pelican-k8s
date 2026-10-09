# Operations

## Inspecting a server

```bash
kubectl -n pelican-servers get gameservers                 # phase, process state, desired state
kubectl -n pelican-servers describe gameserver gs-<uuid>   # spec, status, conditions, events
kubectl -n pelican-servers get pod gs-<uuid>-agent-0       # the agent: always there
kubectl -n pelican-servers get pod gs-<uuid>-0             # the game: exists while the server is on
kubectl -n pelican-servers logs gs-<uuid>-0                # the game console (shim tee)
kubectl -n pelican-servers logs gs-<uuid>-agent-0          # Wings logs
kubectl -n pelican-servers get pods -l pelican-k8s.io/component=game   # every server that is on
```

Every server has an agent pod (`gs-<uuid>-agent-0`), which serves files, SFTP, the
console and backups whether the game runs or not. The game pod (`gs-<uuid>-0`) is
created when the server is started and removed when it is stopped, so a stopped
server has no game pod and requests no game resources. The game pod is `1/1` while
the Panel shows the server as *running* and `0/1` while it is starting or stopping.
Readiness never restarts anything (the game container has no liveness probe), and
nothing depends on it: the Services publish not-ready addresses.

Both pods run on the same node, because both mount the server's volume. The
scheduler places the game pod; if that is not the agent's node, the operator moves
the agent there (`AgentRelocating`), which takes a volume detach and attach.

Status fields worth knowing:

| Field | Written by | Meaning |
|---|---|---|
| `status.process.state` | gateway | `offline`, `starting`, `running`, `stopping` as reported by Wings |
| `spec.power.desired` | gateway (operator: once per server on the upgrade from 1.x) | `Running` or `Stopped`; a stop from the Panel, the console or suspension sets `Stopped`; a crash leaves `Running` while Wings restarts the process and becomes `Stopped` after a minute when it does not |
| `status.power.observedGeneration` | operator | Last `spec.power.generation` acted on |
| `status.install.*` | both | Requested/prepared generation, Job name, result |
| `status.conditions` | operator (`Orphaned`: gateway) | `VolumeReady`, `ExposureReady`, `AgentReady`, `GamePodReady`, `AgentRelocating`, `InstallPrepared`, `Installed`, `ResizePending`, `RecreatePending`, `NodeLost`, `Orphaned`, `DiskShrinkRefused` |

`kubectl edit` is legitimate for `spec.power` (for example set `desired: Running`
and bump `generation` to start a server without the Panel, or bump
`restartRequest` to recreate both pods at the next safe point). `spec.panel` is
owned by the gateway and overwritten on the next Panel sync.

## What happens when

| Event | Behaviour |
|---|---|
| Server started | the operator creates the game pod from the current image, limits and ports; the scheduler places it, the agent moves to its node if needed, then Wings starts the process |
| Server stopped (Panel, console stop command, suspension) | Wings stops the process; once it is offline the operator removes the game pod |
| Panel edits limits | `sync` → a running server's game pod is resized in place (CPU/memory); a stopped server gets the new values at its next start. The PVC is expanded (disk) |
| Panel changes the image | a stopped server uses it at the next start. A running one gets `RecreatePending`, and its game pod is replaced once the process is offline (a restart while pending replaces it first) |
| Game pod deleted / node drained | the shim runs the stop command or signal itself and waits for the process before it exits; the new game pod starts the server again if `desired: Running` |
| Game container OOM-killed | kubelet restarts the container; the operator relays the exit to the agent; Wings' crash handler restarts the process (unless the previous crash was < 60 s ago) |
| Process crashes and Wings does not restart it (second crash within 60 s, clean exit, crash detection off) | the Panel shows the server as offline; after a minute `desired` becomes `Stopped` and the game pod is removed |
| Agent container restarts, or the agent pod is replaced | the game keeps running; the shim reconnects and the agent re-attaches. Open consoles stay connected through the gateway |
| Agent moves to another node | only before a start, and never while it has in-flight work (transfers, backups, installs, active SFTP). Consoles stay connected; idle SFTP sessions end and the client has to connect again; local-adapter backups are lost |
| Gateway restarts | live console and SFTP sessions drop and reconnect; no state is lost |
| Operator restarts | reconciles are idempotent |
| Panel server deleted | CR deleted; the operator's finalizer stops the process, removes the workload and applies the class deletion policy to the volume |

## Backups

- **S3 (recommended):** configure the Panel's S3 backup adapter. Archives are created on the agent pod's scratch volume and uploaded by the agent through presigned URLs; nothing else changes.
- **Local (`wings` adapter):** archives live on the agent pod's scratch volume, which is deleted with the agent pod, including when the agent moves to another node. Only for non-durable use on single-node clusters or node-local storage.
- **Volume snapshots:** set `storage.volumeSnapshotClassName` and `storage.snapshotSchedule` on the class for crash-consistent CSI snapshots (`gs-<uuid>-<timestamp>`, `snapshotRetain` kept). `deletionPolicy: SnapshotThenDelete` takes a final `gs-<uuid>-final` snapshot before deleting a server's volume.

Restore of a Panel backup works as under Wings. Restoring a volume snapshot is a
storage operation: create a PVC from the snapshot named `gs-<uuid>` before the
server's pods are recreated (or use `deletionPolicy: Retain` and swap the PVC).

## Upgrades

1. `kubectl apply -f charts/pelican-k8s/crds` (Helm does not upgrade CRDs).
2. `helm upgrade pelican-k8s charts/pelican-k8s -n pelican-system -f values.yaml`.
3. New shim/agent images change the pod templates (`RecreatePending`). An agent
   pod is recreated when the server's process is offline and the agent has no
   in-flight work (transfers, backups, installs, active SFTP). A stopped server's
   next start uses the new shim; a running server's game pod is replaced once its
   process is offline.

### From 1.x to 2.0

2.0 runs every server in an agent pod and a game pod. There is no compatibility
mode: after the upgrade above, the operator deletes each StatefulSet of the 1.x
layout (serviceName `gs-<uuid>-agent`) and each pod of that layout (a
`gs-<uuid>-0` with an `agent` container, event `LegacyPodDeleted`) as soon as it
reconciles the server. Before deleting the pod it sets `spec.power.desired` from
`status.process.state` (event `LegacyPowerAdopted`): `running` and `starting`
become `Running`, `offline` and `stopping` become `Stopped`. A running server is stopped through the 1.x pod's shutdown path and started
again in the new layout once that pod is gone; a stopped server only gets its
agent pod. There is no way back to 1.x short of restoring a backup taken
before the upgrade. Plan the upgrade for a quiet time, and take the steps below
first:

- Scripts and runbooks that use `kubectl logs gs-<uuid>-0 -c agent` or
  `kubectl exec gs-<uuid>-0 -c agent` move to `gs-<uuid>-agent-0`.
- Custom admission policies or SCCs that match the game pods'
  ServiceAccount `pelican-game` also need `pelican-agent`.
- Local backups (the `wings` adapter) of a server are lost when its agent pod
  moves to another node; switch to S3 for durable backups.

Upgrading the Panel is independent; re-run the compatibility checks in
`test/upstream` when bumping the pinned Wings version.

## Troubleshooting

| Symptom | Check |
|---|---|
| Node shows an exception in the Panel | `curl -H "Authorization: Bearer <token>" https://wings.../api/system`; the Panel caches system info for 6 minutes (`php artisan cache:clear`) |
| `AgentReady=False` for long | `kubectl describe pod gs-<uuid>-agent-0`: image pulls, volume attach, agent logs (`failed to load server configuration from gateway, retrying` → NetworkPolicy/DNS) |
| Server stays in `Starting`, `GamePodReady=False` | `Unschedulable`: no node has room for the game pod (or the agent has in-flight work and its node is full; the requirement lifts when the work ends). `WaitingForVolume`: the volume is still attached to the agent's old node. `ShimNotAttached`: `kubectl describe pod gs-<uuid>-0` for image pulls and the `probe-entrypoint` init container (unknown egg image layout); `kubectl logs gs-<uuid>-0` with no `connected to agent` line from the shim means it cannot reach `gs-<uuid>-agent:8082` (NetworkPolicy/DNS) or the handshake fails (TLS or the shim token; the agent logs `rejected shim connection`); the shim logs at Info and does not log the failed dials themselves |
| `AgentRelocating=True` for a long time | `WaitingForWork`: the agent has in-flight work (the message lists it: a transfer, a backup, an install, SFTP activity in the last minute) and moves once it ends. `Relocating`: the new agent pod is `Pending` on the game pod's node; `kubectl describe pod gs-<uuid>-agent-0` shows why (no room even after preemption, a taint) |
| Install stuck in `InstallPrepared=False` | the agent must reach the gateway's remote API; `prepareTimeoutSeconds` fails it eventually |
| Install Job never gets a pod | `kubectl describe job`: admission policy or SCC rejection message |
| Console shows nothing | websocket goes browser → ingress → gateway → agent; check ingress websocket support and `gateway.allowedOrigins` |
| `server pod unavailable` (503) | the agent pod is not running; the gateway waited for it first |
| Players cannot connect | `kubectl get svc gs-<uuid>`; NodePort mode needs allocation ports in the NodePort range; check `ExposureReady` |
| `ExposureReady=False` with `Pending` | A `LoadBalancer` Service has no address. Under two minutes this is normal provisioning; after that the message names the likely cause — the cluster has no load balancer implementation. Install one, or switch the class to `NodePort` (ports 30000–32767) or `HostPort` |
| `ExposureReady=False` with `AllocationIPNotOnNode` | `NodePort` (`externalTrafficPolicy: Local`) or `HostPort` on a cluster with more than one node, and no node has the allocation IP as its InternalIP or ExternalIP, so the pod cannot be placed where that address receives traffic. Use a node address for the allocation, give the receiving node that address as ExternalIP (k3s `--node-external-ip`), or switch the class to `LoadBalancer` |
| `ExposureReady=False` with `PortOutOfRange` | The API server refused the allocation port as a NodePort. Move the allocation into the range, widen `--service-node-port-range`, or switch the class to `LoadBalancer` or `HostPort`. The server stays in `Error` and gets no pod until then |
| `ResizePending=RecreateRequired` | The Panel set the CPU limit to unlimited and the class has `unlimitedCpuPercent: 0`. Kubernetes cannot remove a container limit in place, so the game pod is replaced once the process is offline |
| A Proton/Wine server hangs after a restart | After a stop the shim kills every process left in the game container, including a `wineserver` that left the process group; `ps` in the game container shows exactly one `wineserver` while the server runs |
| Server keeps restarting | Wings crash detection: `kubectl logs gs-<uuid>-0` shows why the process exits; `agent.crashDetection.detectCleanExitAsCrash` (Wings' `detect_clean_exit_as_crash`) |

Panel-side: `php artisan p:node:list`, `p:node:configuration <id>` (tokens), and
`storage/logs/laravel.log` in the Panel pod.

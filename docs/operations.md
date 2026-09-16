# Operations

## Inspecting a server

```bash
kubectl -n pelican-servers get gameservers                 # phase, process state, desired state
kubectl -n pelican-servers describe gameserver gs-<uuid>   # spec, status, conditions, events
kubectl -n pelican-servers get pod gs-<uuid>-0             # 2/2 = agent sidecar + game
kubectl -n pelican-servers logs gs-<uuid>-0 -c game        # the game console (shim tee)
kubectl -n pelican-servers logs gs-<uuid>-0 -c agent       # Wings logs
```

Status fields worth knowing:

| Field | Written by | Meaning |
|---|---|---|
| `status.process.state` | gateway | `offline`, `starting`, `running`, `stopping` as reported by Wings |
| `spec.power.desired` | gateway | `Running` or `Stopped`; a stop from the Panel, the console or suspension sets `Stopped`, a crash leaves `Running` |
| `status.power.observedGeneration` | operator | Last `spec.power.generation` acted on |
| `status.install.*` | both | Requested/prepared generation, Job name, result |
| `status.conditions` | operator | `VolumeReady`, `ExposureReady`, `AgentReady`, `InstallPrepared`, `Installed`, `ResizePending`, `RecreatePending`, `NodeLost`, `Orphaned`, `DiskShrinkRefused` |

`kubectl edit` is legitimate for `spec.power` (for example set `desired: Running`
and bump `generation` to start a server without the Panel, or bump
`restartRequest` to recreate the pod at the next safe point). `spec.panel` is
owned by the gateway and overwritten on the next Panel sync.

## What happens when

| Event | Behaviour |
|---|---|
| Panel edits limits | `sync` → operator resizes the pod in place (CPU/memory) and expands the PVC (disk) |
| Panel changes the image | `RecreatePending`; the pod is recreated once the process is offline (a start while pending recreates first) |
| Pod deleted / node drained | `preStop` hooks run Wings' stop procedure (stop command, wait, terminate) before containers receive SIGTERM; the new pod starts the server again if `desired: Running` |
| Game container OOM-killed | kubelet restarts the container; the operator relays the exit to the agent; Wings' crash handler restarts the process (unless the previous crash was < 60 s ago) |
| Agent container restarts | the game keeps running; the agent re-attaches to the shim and replays the output ring buffer |
| Gateway restarts | live console and SFTP sessions drop and reconnect; no state is lost |
| Operator restarts | reconciles are idempotent |
| Panel server deleted | CR deleted; the operator's finalizer stops the process, removes the workload and applies the class deletion policy to the volume |

## Backups

- **S3 (recommended):** configure the Panel's S3 backup adapter. Archives are created on the pod's scratch volume and uploaded by the agent through presigned URLs; nothing else changes.
- **Local (`wings` adapter):** archives live on the pod's scratch volume, which is deleted with the pod. Only for non-durable use.
- **Volume snapshots:** set `storage.volumeSnapshotClassName` and `storage.snapshotSchedule` on the class for crash-consistent CSI snapshots (`gs-<uuid>-<timestamp>`, `snapshotRetain` kept). `deletionPolicy: SnapshotThenDelete` takes a final `gs-<uuid>-final` snapshot before deleting a server's volume.

Restore of a Panel backup works as under Wings. Restoring a volume snapshot is a
storage operation: create a PVC from the snapshot named `gs-<uuid>` before the
server pod is recreated (or use `deletionPolicy: Retain` and swap the PVC).

## Upgrades

1. `kubectl apply -f charts/pelican-k8s/crds` (Helm does not upgrade CRDs).
2. `helm upgrade pelican-k8s charts/pelican-k8s -n pelican-system -f values.yaml`.
3. New shim/agent images change the pod template; the operator recreates each
   game pod **at the next safe point** (process offline). Running servers keep
   the old agent until they stop.

Upgrading the Panel is independent; re-run the compatibility checks in
`test/upstream` when bumping the pinned Wings version.

## Troubleshooting

| Symptom | Check |
|---|---|
| Node shows an exception in the Panel | `curl -H "Authorization: Bearer <token>" https://wings.../api/system`; the Panel caches system info for 6 minutes (`php artisan cache:clear`) |
| `AgentReady=False` for long | `kubectl describe pod`: image pulls, the `probe-entrypoint` init container (unknown egg image layout), agent logs (`cannot reach gateway` → NetworkPolicy/DNS) |
| Install stuck in `InstallPrepared=False` | the agent must reach the gateway's remote API; `prepareTimeoutSeconds` fails it eventually |
| Install Job never gets a pod | `kubectl describe job`: admission policy or SCC rejection message |
| Console shows nothing | websocket goes browser → ingress → gateway → agent; check ingress websocket support and `gateway.allowedOrigins` |
| `server pod unavailable` (503) | the game pod is not running or the agent sidecar has not started |
| Players cannot connect | `kubectl get svc gs-<uuid>`; NodePort mode needs allocation ports in the NodePort range; check `ExposureReady` |
| Server keeps restarting | Wings crash detection: `kubectl logs -c game` shows why the process exits; `detect_clean_exit_as_crash` in `agent.crashDetection` |

Panel-side: `php artisan p:node:list`, `p:node:configuration <id>` (tokens), and
`storage/logs/laravel.log` in the Panel pod.

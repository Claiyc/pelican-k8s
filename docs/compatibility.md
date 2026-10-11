# Compatibility

## Wings feature parity

| Feature | Status | Notes |
|---|---|---|
| Power, console, stats, done detection, stop command/signal | ✅ | Wings code, driven through the shim. A kill leaves the server stopped and removes its game pod at once; on Wings a kill sent while a restart waits for the process to stop lets that restart finish and start the server again. A start for a server that is not offline restarts it, where Wings refuses it |
| Crash detection and auto restart | ✅ | Wings code; container-level exits (OOM) are relayed by the operator |
| File manager, compress/decompress, search, remote pull | ✅ | Wings code on the server volume |
| Signed downloads and uploads | ✅ | Gateway verifies with the node token, re-signs with the agent token; one-time use enforced by Wings |
| SFTP | ✅ | Gateway terminates SSH, authenticates against the Panel, relays to the agent's Wings SFTP server; activity rows carry the gateway address |
| Egg config file rewriting | ✅ | Wings parser; the default allocation IP is presented as `0.0.0.0`, the real IP as `SERVER_PUBLIC_IP` |
| Installs and reinstalls | ✅ | Kubernetes Job with the egg installer image; exit codes ignored unless `strictExitCode` |
| Backups (wings, s3) and restore | ✅ | Local archives are lost when the agent pod is recreated or moves to another node |
| Activity log | ✅ | Per-agent SQLite on the volume, forwarded through the gateway |
| Suspension, deauthorize | ✅ | |
| Image change at next start | ✅ | A start from stopped creates a game pod with the current image, digest pinned. A running server's pod is replaced once the process is offline. A restart without an image change reuses the pod and its digest, where Wings would pull the tag again |
| Live resource changes | ✅ | In-place resize of the game pod; `ResizePending` when deferred. Removing a limit (Panel "unlimited") replaces the game pod once the process is offline. A stopped server reserves no game resources |
| Start time | ⚠️ | A start schedules a pod, checks the image and runs two init containers before the process starts; longer when the agent has to move to another node. The Panel shows `offline` until the process starts; the console prints what the start waits for |
| File mode changes (chmod) | ⚠️ | Wings changes modes with `fchmodat2`, which needs Linux 6.6 on the node. On older kernels (RHEL 9, OpenShift 4) the file manager and SFTP refuse a chmod with an error |
| Disk limit | ⚠️ | Wings soft limit plus the PVC hard limit; PVCs cannot shrink |
| `/api/system` Docker fields, image prune, docker disk | ⚠️ | Stubbed |
| Container hostname | ⚠️ | `gs-<uuid>-0`; `/etc/machine-id` is provided |
| Container network | ⚠️ | The game pod gets a new IP at every start (`INTERNAL_IP`) |
| Server transfers | ⚠️ | Incoming only: a transfer from another node (a Docker Wings) creates the server here and its agent receives the files (ARCHITECTURE.md §8.10). A failed transfer deletes the copy and its volume, whatever the class `deletionPolicy`. Transfers away from the cluster, and cancelling an incoming one, are refused (`501`). A reverse proxy in front of the gateway must not time out the upload (Traefik's default `readTimeout` is 60 s) |
| Panel mounts | ❌ | |
| `force_outgoing_ip`, swap, `io_weight`, `threads`, OOM-killer disable | ❌ | Not expressible per pod |
| Docker-specific egg assumptions (Docker network aliases, a hard-coded `172.18.0.1`) | ❌ | Rare; check the egg |

## Kubernetes and OpenShift

Tested on OpenShift 4.22 (Kubernetes 1.35, OVN-Kubernetes, Longhorn, CoreDNS on
5353) as a single node with `NodePort` exposure. Other distributions should
work; report issues with the distribution and CNI.

OVN-Kubernetes forwards only one protocol of a port that a LoadBalancer
Service carries as TCP and UDP under `externalTrafficPolicy: Local`: the one
listed last, which for a pelican-k8s Service is UDP, so TCP connections to the
address get no answer. With a sharing annotation (`loadBalancer.provider:
metallb`, or `sharingAnnotation`) the operator writes one Service per protocol
and both work. Without one, use `externalTrafficPolicy: Cluster` on such a
cluster, or `NodePort` exposure.

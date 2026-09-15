# pelican-k8s — Kubernetes-native backend for Pelican Panel

| | |
|---|---|
| **Status** | Draft v0.3: architecture proposal, nothing implemented |
| **Date** | 2026-09-15 |
| **Target** | Any conformant Kubernetes ≥ 1.35 (in-place pod resize stable, native sidecars, ValidatingAdmissionPolicy), single- or multi-node |
| **Upstream basis** | Pelican Wings `65422ff`, Pelican Panel `6ba5264` (see [`docs/wings-panel-contract.md`](docs/wings-panel-contract.md)) |
| **Working name** | `pelican-k8s` (placeholder; CRD group `pelican-k8s.io` is also a placeholder) |

---

## 1. Summary

Pelican Panel manages game servers through **Wings**, a per-host daemon that drives Docker. This
document proposes replacing Wings with three Kubernetes-native components. **The Panel and the eggs
stay completely unmodified.**

| Component | Kind | One-line role |
|---|---|---|
| **Gateway** | Deployment (stateless-ish) | Presents itself to the Panel as a single Wings node. It terminates all user-facing traffic (HTTP API, websockets, SFTP), holds the node token, and routes per-server traffic to agents |
| **Operator** | Deployment (controller) | Reconciles one `GameServer` custom resource into a StatefulSet (1 replica), PVC, Service, NetworkPolicy and install Jobs |
| **Agent** | Native sidecar container in every game pod | Wings in *single-server mode*. It reuses Wings' filesystem, SFTP handler, websocket, config parser, crash detection and backup code as-is, and drives the game process through the shim |
| **Shim** | Static binary injected as the game container's entrypoint | Supervises the egg's process inside the unmodified egg image: PTY/stdin, signals, exit codes, cgroup stats |

The core idea: **one game server = one pod + one PVC + one CR.** Wings' logic moves next to the
data (agent). The Panel protocol is terminated centrally (gateway). Kubernetes owns scheduling,
storage, networking and restarts (operator).

---

## 2. Goals and non-goals

### Goals

1. **Unmodified Panel.** Panel code, database schema and eggs stay untouched. The Panel sees an ordinary
   Node (`scheme://fqdn:port` plus SFTP port and token).
2. **Kubernetes-native workloads.**
   - Every server is a resource you can inspect and debug with `kubectl`. Cluster policy
     (`GameServerClass`) is GitOps-managed; server specs are owned by the Panel through the gateway.
   - Pods are owned by controllers, not created imperatively.
   - One PVC per server, so storage moves with the pod.
3. **Wings feature parity** for the day-to-day feature set:
   - power control, console and stats
   - file manager, uploads and downloads, SFTP
   - egg installs and reinstalls, egg config-file rewriting, startup "done" detection
   - crash detection and auto-restart
   - backups (local and S3) and restore
   - activity log, suspension
4. **Restricted-shape game pods by default.** Game pods run as a pinned non-root UID with no host
   access, all capabilities dropped and the `RuntimeDefault` seccomp profile, i.e. they satisfy the
   `restricted` Pod Security Standard. Because install Jobs must run as root in the same namespace, the
   namespace itself is labelled `baseline`, and the restricted shape of game pods is enforced by a
   `ValidatingAdmissionPolicy` bound to their ServiceAccount (§12.2). Anything needing more is opt-in.
5. **Maximum reuse of Wings code** (MIT) to inherit its behaviour and edge-case handling.
6. **No single-node assumption** in the design.

### Non-goals (v1)

- High availability of the **Panel** or the **gateway** (single replica each is fine; §14 covers the HA path).
- **Server transfers** between nodes. A single gateway is a single Panel Node, so there is nothing to
  transfer to (§17 covers the future path).
- Panel **mounts** (host path mounts), `force_outgoing_ip`, swap, `io_weight`, CPU pinning
  (`threads`), and disabling the OOM killer. §16 explains each.
- Session-based matchmaking or fleets.
- Upstreaming changes to the Panel.

---

## 3. Background

### 3.1 How Wings works today (condensed)

Details and citations: [`docs/wings-panel-contract.md`](docs/wings-panel-contract.md).

- **One daemon per host** that creates **one Docker container per server**:
  - host port = container port on both TCP and UDP
  - the server directory `/var/lib/pelican/volumes/<uuid>` is bind-mounted at `/home/container`
  - UID forced to 988, read-only root filesystem, `/tmp` as tmpfs
- **Everything else Wings does reads the host disk directly:** SFTP server, file manager API, disk quotas,
  tar.gz backups, install scripts (run as root in a separate container) and egg config-file rewriting.
- **Console:** Docker attach (stdin/stdout), relayed to the browser over a JWT-authenticated websocket.
- **JWTs are HS256 signed with the node token.** Revocation and one-time-token state lives in memory.
- **Scheduling is Panel-side** (Laravel queue plus cron). Wings has no scheduler.

### 3.2 Alternatives within the Pelican ecosystem

| Approach | Why rejected |
|---|---|
| **PR pelican/wings#193**: Kubernetes environment backend inside Wings | Wings creates bare pods imperatively (`RestartPolicy: Never`, no owner), keeps state in memory, and needs one shared data PVC mounted by Wings plus every game pod (`data_pvc` + `subPath`) for files and SFTP to work. Per-server PVCs disable the file manager. That is a Docker host emulated on Kubernetes |
| **Wings as a DaemonSet** (one Panel Node per Kubernetes node) | DaemonSets cannot template per-pod PVCs; each pod needs its own Panel identity and FQDN (hostPort); game pods stay pinned to their node with no failover |
| **kubectyl/kuber** (archived 2024): Wings fork creating bare pods via the Kubernetes API | Needed a forked Panel; per-server SFTP pods because the daemon had no filesystem access; `RestartPolicy: Never` pods so crash handling had to be reimplemented; NodePort-only allocations |
| **Full rewrite of Wings** | Very high effort; loses Wings' battle-tested file, SFTP and parser semantics |

This design keeps the Panel contract, reuses Wings' server-local logic, and puts Kubernetes in charge
of the workload.

---

## 4. Architecture overview

```mermaid
flowchart LR
  subgraph Users
    B[Browser]
    S[SFTP client]
    P[Players]
  end

  subgraph ns-panel["ns: pelican"]
    PANEL[Pelican Panel<br/>Deployment]
  end

  subgraph ns-system["ns: pelican-system"]
    GW[Gateway<br/>Deployment]
    OP[Operator<br/>Deployment]
  end

  subgraph ns-servers["ns: pelican-servers"]
    CR[(GameServer CRs)]
    subgraph POD["Pod gs-&lt;uuid&gt;-0 (StatefulSet, 1 replica)"]
      AG[agent<br/>sidecar]
      GM[game container<br/>egg image + shim]
    end
    PVC[(PVC gs-&lt;uuid&gt;)]
    SVC[Service gs-&lt;uuid&gt;<br/>game ports]
    JOB[install Job]
  end

  B -- "HTTPS / WSS (Ingress)" --> GW
  S -- "SSH :2022" --> GW
  PANEL -- "Wings API (node token)" --> GW
  GW -- "Remote API (node token)" --> PANEL
  B -- "Panel UI" --> PANEL

  GW -- "internal API (gateway-signed JWT)" --> AG
  AG -- "internal API (projected SA token)" --> GW
  GW -- "create/patch" --> CR
  OP -- "watch" --> CR
  OP -- "reconcile" --> POD & PVC & SVC & JOB

  AG <-- "unix socket" --> GM
  AG --- PVC
  GM --- PVC
  JOB --- PVC
  P -- "TCP/UDP game ports" --> SVC --> GM
```

### 4.1 Namespaces

| Namespace | Contents |
|---|---|
| `pelican` | Panel (web + queue worker + scheduler), database, optional Redis |
| `pelican-system` | Gateway, operator, gateway Secrets (node token, SFTP host key, internal signing key) |
| `pelican-servers` | `GameServer` CRs and everything they own. No long-lived credentials besides per-pod projected tokens |

v1 uses one server namespace. Per-tenant namespaces are a later option (§17).

### 4.2 Design principles

1. **The node token never leaves the gateway.** It signs every browser JWT for every server, so any
   process holding it can impersonate the node. Game pods run arbitrary egg code and must never see it.
2. **Terminate user traffic centrally, execute locally.** Authentication, revocation, rate limiting and
   session tracking happen at the gateway. File I/O, process control and parsing happen in the agent,
   next to the PVC.
3. **Declarative configuration, imperative actions reconciled into desired state.**
   - Panel server settings become the CR `spec`.
   - `spec.power.desired` replaces Wings' `states.json` and follows the same rule: it is `Running`
     while the process is starting or running (set on power `start`/`restart`), and `Stopped` when the
     process reaches `offline` through an intentional stop (power `stop`/`kill`, the stop command typed
     into the console, suspension). A crash leaves it at `Running`, so Wings' crash handler and pod
     recreation both restore a running server, and a server stopped from the console stays stopped
     after a node reboot.
4. **Wings' code, Kubernetes' plumbing.** The agent is a build mode of a Wings fork, kept small enough to
   upstream as refactors (environment factory, remote-client injection).
5. **Container port = external port = Panel allocation port** in every exposure mode, so `SERVER_PORT`
   and what players connect to always agree.

---

## 5. Gateway

A Go service in `pelican-system`. It imports Wings packages: `router/tokens`, `remote` (Panel HTTP
client), and the SFTP auth, host key and algorithm code.

### 5.1 Responsibilities

1. Implement the **node-facing Wings HTTP API** toward the Panel (node-token auth, `User-Agent` response header).
2. Terminate and validate **browser traffic**: websockets, signed download and upload URLs.
3. Terminate **SFTP** (SSH), authenticate against the Panel, relay the SFTP subsystem to the right agent.
4. Own all **revocation and replay state**: user denylist, one-time token store, boot-time cutoff.
5. Translate **server lifecycle** calls (create, sync, install, delete, power) into `GameServer` CR changes.
6. Proxy **Panel remote API** calls on behalf of agents, restricted to each agent's own server.
7. Cache **server state and stats** for the Panel's latency-sensitive reads (`GET /api/servers/:s` has a 1 s timeout).
8. Answer **node-level** endpoints (`/api/system*`, list servers) from cluster data and configuration.

### 5.2 Route handling

| Route(s) | Handling at gateway |
|---|---|
| `POST /api/update` | Accept and ignore (config comes from Helm/GitOps). Respond like Wings with `ignore_panel_config_updates` |
| `GET /api/system` | Synthesize: gateway version, `os: linux`, kernel and arch of the gateway pod, cluster version. Docker fields filled with placeholders so the Panel UI shows a version instead of an exception |
| `GET /api/diagnostics` | Gateway diagnostics: Panel reachability, CR counts, agent connectivity summary |
| `GET /api/system/docker/disk` | `{}` zeros |
| `DELETE /api/system/docker/image/prune` | `{"ImagesDeleted": null, "SpaceReclaimed": 0}` |
| `GET /api/system/ips` | Configured list of advertised IPs (the exposure mode's external IPs, §9.3) |
| `GET /api/system/utilization` | Totals from configured capacity (namespace `ResourceQuota` if present, else node allocatable), usage from CR status |
| `GET /api/servers` | List from CRs plus the state cache |
| `POST /api/servers` | Server create flow (§8.1) |
| `POST /api/deauthorize-user` | Revocation (§5.5); closes matching websocket and SFTP sessions. Empty `servers` ⇒ close the user's sessions on all servers (Wings-compatible; also covers the Panel payload-wrapping bug) |
| `DELETE /api/transfers/:s`, `POST /api/transfers` | `501`-style error with a clear message (transfers unsupported in v1) |
| `GET /api/servers/:s` | From state cache (≤ 1 s). If the agent stream is down, the last known state is served while the pod exists; `missing` if it does not (the Panel's own value for an unreachable node) |
| `DELETE /api/servers/:s` | Delete flow (§8.8) |
| `POST /api/servers/:s/sync` | Sync flow (§8.6) |
| `POST /api/servers/:s/install`, `/reinstall` | Install flow (§8.2) |
| `POST /api/servers/:s/ws/deny` | Store JTIs in the deny set (kept for compatibility) |
| `POST/DELETE /api/servers/:s/transfer` | Unsupported in v1 |
| `POST /api/servers/:s/power`, `/commands` | Proxy to the agent; on power actions also patch `spec.power.desired` |
| `GET /logs`, `/install-logs`, `/files/*`, `/backup*` | Proxy to the agent (the Panel is authenticated at the gateway) |
| `GET /api/servers/:s/ws` | Websocket proxy (§5.6) |
| `GET /download/file`, `/download/backup`, `POST /upload/file` | Verify the JWT (signature, `exp`, scope, one-time `unique_id`, denylist) and route by `server_uuid` claim to that agent |

**Response headers:** every response carries `User-Agent: Pelican Wings/v<compat-version> (id:<token_id>)`.
The advertised version is configurable, because the Panel may gate features on the Wings version.

### 5.3 Routing to agents

- Each server has a headless Service `gs-<uuid>-agent` selecting its pod on the agent port (8080 inside the pod).
- The gateway resolves it through EndpointSlices (informer cache), not DNS. Whether the agent is
  reachable is known from the agent's own stream to the gateway (§5.9), so a server whose agent is not
  connected gets a fast `503 {"error": "server pod unavailable"}` instead of a timeout.

### 5.4 Gateway ⇄ agent authentication

| Direction | Mechanism |
|---|---|
| Gateway → agent | Short-lived **internal JWT** (EdDSA, ≤ 60 s) signed by a gateway key. Claims: `aud=<server uuid>`, `sub=gateway`, plus request context (`user_uuid`, `permissions`, `ip`) when proxying a user request. Agents verify with the **public key** from a ConfigMap. No secret in the pod |
| Agent → gateway | **Projected ServiceAccount token** with audience `pelican-gateway`, mounted **only into the agent container**. The gateway validates it with `TokenReview` and maps the bound pod (`authentication.kubernetes.io/pod-name` extra) to its server UUID via pod labels |

NetworkPolicy additionally restricts the agent port to gateway pods (§12.4).

### 5.5 Token validation and revocation

The gateway runs Wings' own validation logic, centrally:
- **Signature** HS256 with the node token; **`exp`**; **scope**; **server match** (path UUID or `server_uuid`/`sub`).
- **Boot-time cutoff:** tokens with `iat` before gateway start are rejected. Same semantics as a Wings
  restart; agent restarts don't invalidate tokens.
- **User denylist** `(server, user) → time` and **JTI denylist**.
- **One-time store** for `unique_id` (download, upload, backup-download tokens).
- On `deauthorize-user`: add deny entries **and close** that user's active websocket and SFTP sessions.
  The gateway holds all those connections, so no fan-out to agents is needed.

After validation the gateway forwards the original JWT inside the internal request. The agent runs in
**trusted-upstream mode**: it parses the JWT without verifying it, for permissions and `user_uuid` only,
and skips the cutoff, denylist and one-time checks.

**State storage:** in memory for a single replica (v1). With more than one replica, move the denylist
and one-time store to Redis with pub/sub for session-close fan-out (§14).

### 5.6 Websocket proxy

The gateway proxies frames and understands the `{event, args}` format:

1. **Upgrade checks** (Wings parity):
   - `Origin` equals the Panel URL or one of `allowed_origins`
   - at most 30 connections per server
   - suspended server (from CR) ⇒ close code 4409
   - Wings' inbound limiters: 1 message per 200 ms with a burst of 10 globally, plus the per-event limits (`auth` and `send logs` 1 per 5 s, `send command` 1 per second)
2. **Dial** the agent's `/api/servers/<uuid>/ws` with an internal JWT.
3. **Inbound `auth` frames** are fully validated at the gateway (§5.5). If invalid, the gateway replies
   `jwt error` and does not forward. If valid, the frame is forwarded, and the gateway records
   `(server, user, jti, exp)` for revocation and expiry.
4. **All other frames:** the gateway checks the recorded token is still valid (`exp`, denylist) before
   forwarding. The agent enforces permissions.
5. **Outbound frames** pass through unchanged. `token expiring` and `token expired` come from the agent's
   Wings listener logic.

### 5.7 SFTP relay

```mermaid
sequenceDiagram
  participant C as SFTP client
  participant G as Gateway (SSH server)
  participant P as Panel
  participant A as Agent
  C->>G: SSH handshake (host key from Secret)
  C->>G: auth user "alice.1a2b3c4d" + password/key
  G->>P: POST /api/remote/sftp/auth {type, username, password, ip, ...}
  P-->>G: {user, server, permissions}
  G->>G: record session (server,user) for revocation
  C->>G: open channel, subsystem "sftp"
  G->>A: POST /internal/v1/sftp (HTTP/2 stream or WS upgrade)<br/>internal JWT {user, permissions, ip}
  A->>A: Wings sftp handler (pkg/sftp RequestServer) on the stream
  C-->>A: SFTP packets relayed as opaque bytes
```

- SSH has no SNI, so the target server is only known after authentication. The gateway terminates SSH;
  the agent runs Wings' existing `sftp/handler.go` over the relayed byte stream. Permission checks,
  denylist file rules, disk quota and SFTP activity logging all stay Wings code.
- Wings' SSH algorithm pinning and `MaxAuthTries` are reused. `key_only` and `read_only` become gateway settings.
- Host key: a stable ED25519 key in Secret `pelican-gateway-sftp-hostkey`.

### 5.8 Panel remote-API proxy for agents

Agents call `https://pelican-gateway.pelican-system.svc/internal/v1/remote/...`. The gateway checks the
caller's server UUID and forwards to the Panel with the node token.

| Agent call | Allowed when |
|---|---|
| `GET /servers/{uuid}` | `uuid` = caller. Normally served from the CR spec instead (§8.6) |
| `POST /servers/{uuid}/container/status` | `uuid` = caller. The gateway also updates its state cache and CR status |
| `POST /servers/{uuid}/install` | `uuid` = caller and an install is in progress for that server |
| `POST /activity` | Every row's `server` = caller (rows for other servers are rejected) |
| `GET /backups/{backup}?size=`, `POST /backups/{backup}`, `POST /backups/{backup}/restore` | `backup` belongs to the caller. The gateway learns backup → server from the `POST /api/servers/:s/backup` call it proxied (kept in CR `status.backups.pending`) |
| `POST /servers/reset`, `GET /servers` (list), `POST /sftp/auth`, transfers | **Never**. `reset` affects every server on the node and is sent by the gateway alone (§8.9) |

### 5.9 State cache

Agents push `{state, stats}` over a long-lived stream to the gateway: on every state change, and stats
every 2 s while running. The gateway serves `GET /api/servers/:s` and `GET /api/servers` from this
cache, and writes a throttled summary to the CR status (§7.2) for `kubectl` visibility.

---

## 6. Agent and shim

### 6.1 Pod layout

```mermaid
flowchart TB
  subgraph Pod["Pod gs-<uuid>-0"]
    direction TB
    I1["init: prepare<br/>(pelican-k8s image)<br/>copy shim; create volumes/&lt;uuid&gt;, machine-id, install/ on the PVC"]
    I2["init: passwd (optional)<br/>(egg image)<br/>generate /pelican/etc/passwd"]
    subgraph A["native sidecar: agent (pelican-agent image)"]
      W["wings agent<br/>HTTP :8080 · internal SFTP · crash detection · parser · backups"]
    end
    subgraph G["container: game (egg image, unmodified)"]
      SH["/pelican/bin/shim (PID 1)"] --> EP["image ENTRYPOINT/CMD<br/>e.g. tini -g -- /entrypoint.sh<br/>(evals $STARTUP)"]
    end
    V1[("emptyDir /pelican<br/>bin, run/shim.sock, etc")]
    V2[("PVC gs-<uuid><br/>Wings root layout")]
    V3[("emptyDir /tmp<br/>medium: Memory")]
    V4[("scratch volume<br/>backup archives, agent tmp")]
  end
  SH <-- "unix socket" --> W
  G --- V1 & V3
  A --- V1 & V4
  I1 --- V1 & V2
  G -- "subPath volumes/<uuid> → /home/container" --- V2
  A -- "mount root → /var/lib/pelican" --- V2
```

### 6.2 PVC layout

The PVC root **is** a Wings `root_directory` for one server, so Wings path logic works unchanged:

```
/ (PVC root, mounted in agent at /var/lib/pelican)
├── volumes/<uuid>/        # server files → game container /home/container (subPath)
├── logs/                  # agent logs, install/<uuid>.log, console run logs
├── install/<generation>/  # install Job status: output.log, exit-code
├── wings.db               # activity SQLite (unsent rows survive restarts)
└── machine-id             # stable per-server machine-id (subPath → /etc/machine-id)
```

The game container mounts **only** `volumes/<uuid>`, so a game process cannot read activity, logs,
backups or install state.

The `prepare` init container runs as the pod UID with the PVC root mounted and creates
`volumes/<uuid>`, `install/`, `logs/` and the `machine-id` file **before** any container mounts them
as `subPath`. Kubelet creates a missing `subPath` target itself, as a root-owned directory, which
would make `/home/container` unwritable for the pinned UID and turn `/etc/machine-id` into a
directory. Backup archives (local adapter output and S3 upload temp files) do **not** live on the
PVC; they go to the pod's scratch volume (§10.1).

### 6.3 Agent = Wings in single-server mode

A new Cobra command `wings agent` in a fork (`third_party/wings`, branch `k8s-agent`). The changes
derive directly from Wings' Docker coupling ([contract §7.1](docs/wings-panel-contract.md)).

| # | Change | Upstreamable? |
|---|---|---|
| 1 | Boot path without Docker: skip `Info` check, `ConfigureDocker`, `EnsurePelicanUser`, passwd, logrotate, quotas | Partly, as a `--no-docker` mode |
| 2 | **Environment factory** replacing hard-coded `docker.New` in `server/manager.go:222-231` | Yes |
| 3 | **Single-server manager:** load one server from the gateway (`GET /internal/v1/self`) instead of the paged Panel list | Yes, with an injectable loader |
| 4 | **`environment/shim`**: new `ProcessEnvironment` talking to the shim socket (§6.4) | Yes |
| 5 | `server/update.go:36-40`: replace `*docker.Environment` assertion with an `ImageAndStopConfigurable` interface | Yes |
| 6 | **Remote client injection:** point the `remote` client at the gateway proxy, authenticate with the projected token, never call `servers/reset` | Yes |
| 7 | **Trusted-upstream router:** server-scoped and token routes only; `RequireGatewayAuth` (internal JWT) instead of the node token; unverified JWT parsing; cutoff, denylist and one-time store off | Partly |
| 8 | **SFTP over stream:** expose `sftp/handler.go` on `/internal/v1/sftp` instead of an SSH listener | Yes |
| 9 | **External install:** replace the Docker installer with a watcher for Job output and exit-code files; keep `SyncInstallState` semantics | Partly |
| 10 | Config: `check_permissions_on_boot: false`, `system.user.rootless.enabled: true`, `log_directory: /var/lib/pelican/logs`, `backup_directory: /scratch/backups`, `tmp_directory: /scratch/tmp`, `enable_log_rotate: false`, `passwd.enable: false` | Config only |
| 11 | **Exit-state injection:** accept an `exited{code, oomKilled}` event from the gateway (relayed from the pod's container status) in addition to the shim's own event, for the OOM case where the shim dies with the process (§6.4) | Partly |
| 12 | **Intentional-stop signal:** report `stoppedIntentionally` to the gateway when the process reaches `offline` via `stopping` (power stop/kill, console stop command, suspension) so the gateway can set `spec.power.desired=Stopped` (§4.2) | Partly |

Upstreaming is a hope, not a dependency: Pelican's maintainers have marked Kubernetes support as
"not planned" (pelican/panel discussion #933). The refactors above are kept small and generic so
they *could* be accepted, but the fork must remain self-sufficient.

**What stays pure Wings:**
- `server/filesystem` (safe path resolution, denylist, disk usage, soft quota)
- the file routes, compress and decompress, remote file pull (with Wings' private-range SSRF block)
- the websocket handler and listeners, done detection, egg config parsers, crash detection
- backup adapters (local, S3 multipart via the Panel) and restore
- activity DB and crons

### 6.4 Shim

A small static Go binary (≈ 1–2 kLOC). The operator copies it into an emptyDir with an init container
and sets it as the game container's `command`.

**Why a shim:**
- Wings' Docker model recreates the container on every start and attaches to its stdin.
- In a pod, restarting a container restarts only that container, but a *stopped* server must keep its pod
  (files, SFTP, websocket, stats), and Kubernetes cannot stop a single container.
- So the shim is PID 1 and stays alive; the game process is its child.

**Responsibilities:**
- Listen on `/pelican/run/shim.sock` (mode 0600; both containers run as the same UID, §12.2).
- `start{env}`: spawn the **image's original ENTRYPOINT+CMD** in a PTY, with `STARTUP`, `SERVER_*` and
  egg variables in the environment, and `cwd=/home/container`. The argv is the container's own `args`,
  which the operator resolved from the image config (§7.5); the agent supplies only the environment.
  This preserves yolk entrypoint behaviour (e.g. SteamCMD auto-update logic in `/entrypoint.sh`).
- `stdin{bytes}`, `signal{name}` (to the process group), `kill`.
- **Output:** stream PTY bytes to the connected agent, keep a ring buffer (default 1 MiB) so an agent
  restart doesn't lose context, and **tee to container stdout** so `kubectl logs -c game` works.
- `exited{code}` when the child exits.
- **OOM:** on cgroup v2, kubelet sets `memory.oom.group=1` for every container (Kubernetes ≥ 1.28), so an
  OOM kills the whole container cgroup, shim included, and kubelet restarts the container. The shim
  therefore never reports OOM itself. The operator watches the game container's
  `lastState.terminated` (`reason: OOMKilled`, exit code) and relays it through the gateway as
  `exited{code, oomKilled: true}`; the agent runs Wings' crash handling on it. The ring buffer is lost
  in this case. When a restarted container's shim socket reappears, the agent reconnects and, if the
  crash rules allow, starts the process again. Nodes with kubelet `singleProcessOOMKill: true`
  (Kubernetes ≥ 1.32) kill only the game process; there the shim reports `exited{code, oomKilled}`
  from the `oom_kill` counter delta in `/sys/fs/cgroup/memory.events` and the ring buffer survives.
- **SIGTERM** (pod deletion, eviction, drain): kubelet runs the game container's `preStop` hook first,
  which calls the agent's `/internal/v1/prestop`; the agent runs Wings' regular stop procedure (stop
  command or signal, `WaitForStop`, then terminate) and returns once the process is offline. Only then
  does the shim receive SIGTERM, which it treats as `kill` for anything still running, and exits. The
  agent is a native sidecar (§7.5), so it is terminated only after the game container has exited.
- **Stats:** every 2 s from its own cgroup (`memory.current`, `memory.max`, `cpu.stat`, `io.stat`) and
  `/proc/net/dev` (pod network namespace). Pushed to the agent.
- **Hygiene:** clean `/tmp` before each start (emulates the Docker per-start tmpfs); never exit while the
  socket is open.

**Implementation mapping to Wings' `ProcessEnvironment`:**

| Method | Shim-based behaviour |
|---|---|
| `Attach` | Connect to the socket, subscribe to output; `exited` ⇒ `SetState(offline)` (drives Wings crash detection) |
| `Start` | Truncate the run log, `starting`, `OnBeforeStart` (may request pod recreate, §8.5), `start{env}` |
| `Stop` | Stop type `signal` ⇒ `signal{}` (Wings' mapping, unknown ⇒ SIGKILL); type `command` ⇒ `stdin{value+"\n"}` |
| `WaitForStop` / `Terminate` | Poll shim status; SIGKILL on timeout |
| `SendCommand` | `stdin{}` (sets `stopping` first if it equals the stop command, as Wings does) |
| `Readlog(n)` | Tail of `logs/console-current.log` (agent-written), falling back to the shim ring buffer |
| `ExitState` | From the shim's `exited{}`, or from the operator-relayed container status after an OOM |
| `IsRunning`, `Uptime` | Shim status |
| `InSituUpdate` | Resource changes are applied by the operator (in-place pod resize); no-op |
| `Create`, `Destroy`, `Exists` | Trivial (pod lifecycle belongs to the operator) |

**Agent restart:** the agent container restarts but the game keeps running. The agent reconnects, gets
status `running` with the ring buffer, and re-attaches. That is better than Wings, where a daemon restart
re-attaches to Docker.

---

## 7. `GameServer` custom resource and operator

### 7.1 Spec (v1alpha1)

```yaml
apiVersion: pelican-k8s.io/v1alpha1
kind: GameServer
metadata:
  name: gs-1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d   # "gs-" + server uuid
  namespace: pelican-servers
  labels:
    pelican-k8s.io/server-uuid: 1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d
    pelican-k8s.io/egg-uuid: 9f8e...
  annotations:
    pelican-k8s.io/panel-name: "Survival SMP"
spec:
  # --- mirrored from the Panel (written by the gateway, never edited by hand) ---
  panel:
    uuid: 1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d
    uuidShort: 1a2b3c4d
    # The raw `settings` object from GET /api/remote/servers/{uuid}, minus `environment`
    # (x-kubernetes-preserve-unknown-fields). The agent consumes it unchanged as its Wings server
    # configuration; the operator reads only suspended, container.image, build.*, allocations.*.
    settings:
      id: 1
      meta: {name: "Survival SMP", description: ""}
      suspended: false
      invocation: "java -Xms128M -Xmx{{SERVER_MEMORY}}M -jar {{SERVER_JARFILE}}"
      skip_egg_scripts: false
      build: {memory_limit: 4096, swap: 0, io_weight: 500, cpu_limit: 200, threads: null, disk_space: 10240, oom_killer: true}
      container: {image: ghcr.io/pelican-eggs/yolks:java_21, requires_rebuild: false}
      allocations:
        force_outgoing_ip: false
        default: {ip: "0.0.0.0", port: 25565}   # servers without allocation: {ip: 127.0.0.1, port: 0} and mappings {"": []}
        mappings: {"0.0.0.0": [25565, 25575]}
      egg:
        id: 9f8e...
        file_denylist: []
        features: {eula: ["You need to agree to the EULA"]}   # feature id → console listener strings
      labels: {}
    # Egg variables (may contain credentials) live in a Secret, not in the CR.
    environmentSecretRef: {name: gs-1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d-env}
    processConfiguration:   # raw process_configuration, x-kubernetes-preserve-unknown-fields
      startup: {done: [")! For help, type "], user_interaction: [], strip_ansi: false}
      stop: {type: command, value: stop}
      configs: [...]
    panelRevision: "sha256:…"   # hash of the settings + process_configuration payload last applied

  # --- desired runtime state (§4.2; written by gateway, kubectl-editable) ---
  power:
    desired: Running          # Running | Stopped
    restartRequest: 0         # bump to force pod recreate at next safe point

  # --- install requests (written by gateway) ---
  install:
    generation: 1             # bump ⇒ operator runs a new install Job once the agent has prepared (§8.2)
    reinstall: false
    scriptConfigMap: gs-1a2b3c4d-install-1
    image: ghcr.io/pelican-eggs/installers:debian
    entrypoint: bash

  # --- cluster-side policy (defaults from GameServerClass; admin-editable) ---
  className: default
```

`settings.build.memory_limit` and `cpu_limit` may be `0` (unlimited in the Panel); the class supplies
the resources used in that case (§7.3, §11).

### 7.2 Status

```yaml
status:
  observedGeneration: 7
  phase: Running            # Pending | Installing | Stopped | Starting | Running | Stopping | Suspended | Error
  process:
    state: running          # Wings state as reported by agent
    since: "2026-09-15T18:02:11Z"
    lastExit: {code: 0, oomKilled: false, at: "…"}   # from the shim, or from the container status after an OOM
  usage:                    # throttled (≤ 1/min) summary for kubectl
    memoryBytes: 2147483648
    cpuPercent: 37.5
    diskBytes: 5368709120
  install:
    observedGeneration: 1
    preparedGeneration: 1   # written via the gateway when the agent holds the install lock for this generation
    result: Succeeded       # Succeeded | Failed | Running
    finishedAt: "…"
  backups:
    pending: [ {uuid: "…", startedAt: "…"} ]
  endpoints:
    - {ip: "203.0.113.10", port: 25565, protocols: [TCP, UDP]}
  podImage: ghcr.io/pelican-eggs/yolks:java_21@sha256:…
  conditions:
    - type: VolumeReady          # PVC bound (and expanded to requested size)
    - type: ExposureReady        # Service has endpoints / external IP
    - type: AgentConnected       # agent stream to gateway is up
    - type: InstallPrepared      # agent stopped the process and holds the install lock for spec.install.generation
    - type: Installed
    - type: ResizePending        # in-place resize deferred/infeasible
    - type: RecreatePending      # image/pod-template change waiting for offline
    - type: NodeLost             # pod stuck Terminating on a NotReady node (§14)
```

### 7.3 `GameServerClass` (cluster-side defaults)

Admin-owned, cluster-scoped. Referenced by `spec.className`, with the default class set by the gateway.

```yaml
apiVersion: pelican-k8s.io/v1alpha1
kind: GameServerClass
metadata: {name: default}
spec:
  storage:
    storageClassName: standard  # must allow volume expansion
    defaultSizeGiB: 20          # used when disk_space = 0
    overheadPercent: 10         # PVC = disk_space × 1.10 (logs, wings.db, install output)
    scratch:                    # backup archives and agent temp files (§10.1)
      type: Ephemeral           # Ephemeral (generic ephemeral volume, storageClassName below) | EmptyDir
      storageClassName: standard
      sizeGiB: 0                # 0 = size of the server PVC (an archive can be as large as the server)
    deletionPolicy: Delete      # Delete | Retain | SnapshotThenDelete
    volumeSnapshotClassName: ""  # required for SnapshotThenDelete / snapshotSchedule
  exposure:
    mode: LoadBalancer          # LoadBalancer | NodePort (single-node clusters only) | HostPort
    externalTrafficPolicy: Local
    loadBalancer:               # implementation-specific annotation keys
      ipAnnotation: ""          # pins the Service to the allocation IP
      sharingAnnotation: ""     # lets several Services share one IP
  network:
    inClusterEgress:            # destinations inside the cluster that game pods may reach (§12.4)
      gameServers: true         # other GameServer pods on their allocation ports (proxies such as Velocity)
      additional: []            # e.g. [{cidr: 10.43.0.0/16, ports: [9000]}] for an in-cluster S3
  resources:
    memoryOverheadMultiplier: 1.05   # Wings docker.overhead default
    cpuRequestPercentOfLimit: 25     # overcommit knob
    unlimitedMemoryMiB: 4096         # used when memory_limit = 0
    unlimitedCpuPercent: 0           # used when cpu_limit = 0; 0 = no limit, request = minCpu
    minCpu: 100m
    tmpSizeMiB: 100
    agent: {cpu: 50m, memory: 128Mi, memoryLimit: 512Mi}
  security:
    runAsUser: 1000              # pinned non-root UID and GID (install Jobs chown to it)
    generatePasswdEntry: true
  install:
    serviceAccountName: pelican-installer
    resources: {cpu: "1", memory: 1Gi}  # Wings installer_limits default
    strictExitCode: false        # Wings ignores install script exit codes
    activeDeadlineSeconds: 3600
  failover:
    forceDeleteAfter: ""         # e.g. 5m: force-delete a pod stuck Terminating on a NotReady node (§14). Empty = never
  imageResolution:
    registryLookup: false        # true: resolve ENTRYPOINT/CMD from the registry (needs egress + pullSecrets)
    pullSecrets: [pelican-registries]
    entrypointOverrides:         # glob → argv; checked first
      "ghcr.io/pelican-eggs/yolks:*": ["/usr/bin/tini", "-g", "--", "/entrypoint.sh"]
```

### 7.4 Owned resources

| Resource | Name | Notes |
|---|---|---|
| PersistentVolumeClaim | `gs-<uuid>` | RWO, size = `disk_space × (1+overhead)`, expanded online on change (the StorageClass must support volume expansion). **Not** owned via ownerReference when `deletionPolicy: Retain` |
| Secret | `gs-<uuid>-env` | Egg variables (`settings.environment`); created by the gateway, owned by the CR. Read by the gateway (agent config) and used via `envFrom` by install Jobs |
| StatefulSet | `gs-<uuid>` | `replicas: 1`, `updateStrategy: OnDelete` (the operator decides when the pod restarts), explicit PVC volume (no `volumeClaimTemplates`, so resizing works). StatefulSet guarantees at most one pod, so an RWO volume is never double-mounted |
| Service (exposure) | `gs-<uuid>` | One port entry per allocation port for each of TCP and UDP (mixed protocols are GA). Type per class. `publishNotReadyAddresses: true`, so player traffic never depends on container readiness |
| Service (agent) | `gs-<uuid>-agent` | Headless, agent port only, `publishNotReadyAddresses: true`; the gateway decides reachability from its agent stream |
| NetworkPolicy | `gs-<uuid>` | Ingress on this server's game ports from anywhere; agent port from the gateway only (§12.4) |
| ConfigMap | `gs-<uuid>-install-<gen>` | Install script; created by the gateway after the CR exists, owned by the CR |
| Job | `gs-<uuid>-install-<gen>` | Install run (§8.2) |
| VolumeSnapshot | `gs-<uuid>-<ts>` | Optional (§10.4) |

### 7.5 Pod template (abridged)

```yaml
spec:
  automountServiceAccountToken: false
  serviceAccountName: pelican-game            # or pelican-game-hostport (HostPort mode)
  enableServiceLinks: false
  terminationGracePeriodSeconds: 660          # > Wings' 10-min stop wait; spent in the preStop hooks
  securityContext:
    runAsNonRoot: true
    runAsUser: <class runAsUser>               # pinned: prepare init and install Jobs chown to it
    runAsGroup: <class runAsUser>
    fsGroup: <class runAsUser>
    fsGroupChangePolicy: OnRootMismatch
    seccompProfile: {type: RuntimeDefault}
  initContainers:
    - name: prepare
      image: registry/pelican-k8s/shim:<ver>
      # copies the shim; creates volumes/<uuid>, install/, logs/ and the machine-id file on the PVC root
      command: ["/shim", "prepare", "--bin", "/pelican/bin/shim", "--data", "/data", "--uuid", "<uuid>"]
      volumeMounts:
        - {name: pelican, mountPath: /pelican}
        - {name: data, mountPath: /data}
    - name: probe-entrypoint                     # egg image; resolves argv when no override/registry result exists
      image: <settings.container.image>
      command: ["/pelican/bin/shim", "probe", "--out", "/pelican/etc/argv", "--passwd", "/pelican/etc/passwd", "--name", "container", "--home", "/home/container"]
      volumeMounts: [{name: pelican, mountPath: /pelican}]
    - name: agent                                # native sidecar: starts before and stops after the game container
      restartPolicy: Always
      image: registry/pelican-k8s/agent:<ver>
      args: ["agent", "--config", "/etc/pelican/config.yml"]
      ports: [{name: agent, containerPort: 8080}]
      startupProbe: {httpGet: {path: /internal/v1/healthz, port: agent}}
      livenessProbe: {httpGet: {path: /internal/v1/healthz, port: agent}, failureThreshold: 6}
      lifecycle:
        preStop: {httpGet: {path: /internal/v1/prestop, port: agent}}   # returns once the process is offline
      env:
        - {name: PELICAN_SERVER_UUID, valueFrom: {fieldRef: {fieldPath: "metadata.labels['pelican-k8s.io/server-uuid']"}}}
        - {name: PELICAN_POD_IMAGE, value: <settings.container.image>}
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
      volumeMounts:
        - {name: data, mountPath: /var/lib/pelican}
        - {name: scratch, mountPath: /scratch}
        - {name: pelican, mountPath: /pelican/run, subPath: run}
        - {name: gateway-token, mountPath: /var/run/secrets/pelican-gateway, readOnly: true}
        - {name: agent-config, mountPath: /etc/pelican, readOnly: true}
  containers:
    - name: game
      image: <settings.container.image>@<digest>   # re-resolved at every pod (re)creation, like Wings' pull-on-start
      command: ["/pelican/bin/shim", "run", "--socket", "/pelican/run/shim.sock", "--argv-file", "/pelican/etc/argv", "--"]
      args: <argv from entrypointOverrides or registry lookup; empty ⇒ shim reads /pelican/etc/argv>
      stdin: false                              # stdin is owned by the shim PTY
      ports: <allocation ports, TCP+UDP>        # containerPort = allocation port (+ hostPort in HostPort mode)
      resources:
        requests: {memory: <memoryLimit>Mi, cpu: <limit × request%>}
        limits:   {memory: <memoryLimit × multiplier>Mi, cpu: <cpuLimit/100>}
      resizePolicy:
        - {resourceName: cpu, restartPolicy: NotRequired}
        - {resourceName: memory, restartPolicy: NotRequired}
      lifecycle:
        preStop: {httpGet: {path: /internal/v1/prestop, port: 8080}}   # agent runs the Wings stop procedure first
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: [ALL]}
      env: [{name: HOME, value: /home/container}, {name: USER, value: container}]
      volumeMounts:
        - {name: data, mountPath: /home/container, subPath: volumes/<uuid>}
        - {name: data, mountPath: /etc/machine-id, subPath: machine-id, readOnly: true}
        - {name: pelican, mountPath: /pelican/bin, subPath: bin, readOnly: true}
        - {name: pelican, mountPath: /pelican/etc, subPath: etc, readOnly: true}
        - {name: pelican, mountPath: /pelican/run, subPath: run}
        - {name: pelican, mountPath: /etc/passwd, subPath: etc/passwd, readOnly: true}   # if generated
        - {name: tmp, mountPath: /tmp}
  volumes:
    - {name: data, persistentVolumeClaim: {claimName: gs-<uuid>}}
    - {name: pelican, emptyDir: {}}
    - {name: tmp, emptyDir: {medium: Memory, sizeLimit: <tmpSizeMiB>Mi}}
    - name: scratch                               # class storage.scratch: generic ephemeral volume or emptyDir
      ephemeral: {volumeClaimTemplate: {spec: {accessModes: [ReadWriteOnce], storageClassName: <class>, resources: {requests: {storage: <size>}}}}}
    - name: gateway-token
      projected:
        sources:
          - serviceAccountToken: {audience: pelican-gateway, expirationSeconds: 3600, path: token}
          - configMap: {name: pelican-gateway-internal-pubkey, items: [{key: ed25519.pub, path: gateway.pub}]}
    - {name: agent-config, configMap: {name: pelican-agent-config}}
```

**Egg environment.** The game process gets its environment from the agent (`start{env}`, built by Wings'
`Environment()` from the server configuration, which the gateway assembles from `spec.panel` and the
`gs-<uuid>-env` Secret). Nothing egg-specific appears in the pod spec.

**Image entrypoint resolution.** The shim is the container entrypoint, so the operator must know the
image's original `ENTRYPOINT`+`CMD`. Resolution order:
1. class `entrypointOverrides` (glob on the image reference);
2. registry lookup with `go-containerregistry` and the class pull secrets, if `registryLookup` is on;
3. otherwise the `probe-entrypoint` init container, running in the egg image itself, checks for the
   yolk convention (`/entrypoint.sh`, executed with `/bin/bash`) and writes the argv to
   `/pelican/etc/argv`, which the shim reads when it receives no argv after `--`. The same init
   container writes the generated passwd entry.

The pod is pinned to the digest the tag resolved to when the pod was created, and re-resolved on every
recreate, which matches Wings pulling the tag before each start. A `~` prefix on the image (Wings:
"never pull") disables digest pinning and sets `imagePullPolicy: IfNotPresent`.

### 7.6 Reconcile rules

| Change | Operator action |
|---|---|
| `build.memory_limit/cpu_limit` | Patch the pod's `resize` subresource (in-place resize is stable in Kubernetes 1.35). If the result is `Infeasible` or `Deferred`, set `ResizePending` and apply at the next recreate |
| `build.disk_space` | Expand the PVC (never shrink; a shrink sets a condition) |
| `allocations` | Update the exposure Service and NetworkPolicy. No pod change: `containerPort` is informational for Services, and `SERVER_PORT` is applied by Wings' sync at the next start. HostPort mode is the exception and marks `RecreatePending` |
| `container.image` | Update the StatefulSet template (OnDelete) and set `RecreatePending`. The pod is deleted only when the process is `offline` (§8.5) |
| `suspended: true` | No pod change; the agent stops the process. Optional class flag `suspendScalesToZero` |
| `power.restartRequest` bump | Delete the pod once the process is offline |
| `install.generation` bump | Wait for `status.install.preparedGeneration` to reach it (the agent has stopped the process and holds the install lock), then run the install Job (§8.2) |
| Game container terminated (`lastState.terminated`, e.g. `OOMKilled`) | Relay `exited{code, oomKilled}` to the agent through the gateway (§6.4) |
| Pod `Terminating` on a `NotReady` node longer than `failover.forceDeleteAfter` | Force-delete the pod so the StatefulSet can reschedule it (§14) |
| CR deleted | Finalizer: `deletionPolicy` (§10.3), then remove owned resources |

---

## 8. Key flows

### 8.1 Create server

```mermaid
sequenceDiagram
  participant P as Panel
  participant G as Gateway
  participant K as Kubernetes API
  participant O as Operator
  participant A as Agent
  P->>G: POST /api/servers {uuid, start_on_completion}
  G->>P: GET /api/remote/servers/{uuid}
  P-->>G: {settings, process_configuration}
  G->>P: GET /api/remote/servers/{uuid}/install
  P-->>G: {container_image, entrypoint, script}
  G->>K: create GameServer (install.generation=1, power.desired=Stopped), then Secret env + ConfigMap install-1 owned by it
  G-->>P: 202 Accepted
  O->>K: PVC, Services, NetworkPolicy, StatefulSet
  K-->>A: pod starts, agent connects to gateway stream
  G->>A: install/begin (agent takes install lock)
  A-->>G: prepared ⇒ status.install.preparedGeneration=1
  O->>K: Job install-1 (§8.2)
  A-->>G: install completed {successful}
  G->>P: POST /api/remote/servers/{uuid}/install {successful, reinstall:false}
  alt start_on_completion
    G->>K: patch power.desired=Running
    G->>A: POST /api/servers/{uuid}/power {start}
  end
```

### 8.2 Install and reinstall

1. **Trigger.** Panel `POST /install` or `/reinstall` (or create). The gateway fetches the install
   payload, creates ConfigMap `gs-<uuid>-install-<gen+1>`, and patches `spec.install`.
2. **Agent prepares** (gateway `POST /internal/v1/install/begin`):
   - reinstall ⇒ `WaitForStop(10s, terminate)` (Wings semantics)
   - take the installing lock, run SFTP `CancelAll`, publish `install started`
   - start tailing `install/<gen>/output.log`
   - report `prepared` to the gateway, which writes `status.install.preparedGeneration = <gen>`.
     **The operator creates no Job before this**, so the script never runs while the process is alive.
     If the agent is unreachable for longer than a class timeout, the gateway reports the install as
     failed to the Panel instead.
3. **Operator runs the Job:**
   - `podAffinity` to the server pod (`kubernetes.io/hostname`), so RWO volumes attach on any cluster size
   - ServiceAccount `pelican-installer`, root (§12.2); `automountServiceAccountToken: false`;
     resources = max(server, class install resources); `activeDeadlineSeconds`
   - init container `prepare` (as in the game pod) copies the shim into an emptyDir
   - mounts: PVC `volumes/<uuid>` → `/mnt/server`, PVC `install/<gen>` → `/pelican/install`,
     ConfigMap → `/mnt/install/install.sh`, shim → `/pelican/bin`
   - egg variables via `envFrom` the `gs-<uuid>-env` Secret, plus `SERVER_*` and `STARTUP` as Wings sets them
   - command: `shim install-run --log /pelican/install/output.log --exit /pelican/install/exit-code --chown <uid>:<uid> -- <entrypoint> /mnt/install/install.sh`.
     After the script, the shim `chown -R`s `/mnt/server` to the class UID, so root-created files are writable.
4. **Completion.** When `exit-code` appears (or the Job reaches a terminal state), the agent writes
   `logs/install/<uuid>.log` in Wings' format (so `GET /install-logs` works), publishes `install completed`,
   sets `offline`, and reports `{successful}` through the gateway. Success is `true` unless
   `strictExitCode` is set and the exit code is non-zero, or the Job failed or timed out.
5. **Cleanup.** The operator records `status.install` and deletes the Job (`ttlSecondsAfterFinished`).

### 8.3 Start, stop and console

```mermaid
sequenceDiagram
  participant B as Browser
  participant G as Gateway
  participant A as Agent (Wings)
  participant S as Shim
  B->>G: WS /api/servers/{uuid}/ws
  G->>A: WS dial (internal JWT)
  B->>G: {"event":"auth","args":[jwt]}
  G->>G: verify HS256, exp, scope, denylist
  G->>A: forward auth
  A-->>B: auth success, status
  B->>G: {"event":"set state","args":["start"]}
  G->>A: forward (and patch power.desired=Running async)
  A->>A: HandlePowerAction: Sync, config parsers, disk check
  A->>S: start{env}
  S-->>A: output bytes …
  A-->>B: console output / status starting
  A->>A: done-string matched ⇒ running
  A->>G: state stream {running}
  G->>G: cache, POST container/status to Panel, CR status
```

- **Stop:** Wings stop logic (`command` or `signal`), `WaitForStop(10 min, terminate)`. The process goes
  `stopping` → `offline`; the agent reports `stoppedIntentionally` and the gateway sets
  `power.desired=Stopped`. The same happens when the stop command is typed into the console or the
  server is suspended, exactly the transitions Wings excludes from crash detection.
- **Crash:** `exited` (from the shim, or relayed from the container status after an OOM) ⇒ agent
  `offline` ⇒ Wings `handleServerCrash` ⇒ auto-restart (within timeout rules). `power.desired` stays
  `Running`.

### 8.4 Files, downloads and uploads

- **Panel file API calls** (`/files/*`): Panel → gateway (node token) → agent (internal JWT) → Wings
  filesystem on the PVC.
- **Signed URLs:** browser → gateway `/download/file?token=…` → gateway verifies JWT and one-time store,
  routes by `server_uuid` → agent streams the file. Uploads likewise (`upload_limit` enforced at the gateway,
  default 100 MiB).
- **Remote pull** (`/files/pull`): the agent downloads. Egress rules and Wings' private-range block both apply.

### 8.5 Image change (deferred pod recreate)

Wings applies a new image at the next start by recreating the container. Equivalent flow:

1. Panel `sync` ⇒ gateway updates `spec.panel.settings.container.image` ⇒ operator updates the StatefulSet template and sets
   `RecreatePending`.
2. On the next `start` (or immediately if already offline), the agent's `OnBeforeStart` compares
   `PELICAN_POD_IMAGE` with the configured image. On mismatch the agent reports `needs-recreate`, and the
   gateway patches `power.desired=Running` plus `restartRequest++`.
3. The operator deletes the pod. The new pod's agent boots, reads `power.desired=Running`, and starts the process.
4. The browser sees `starting` → a short websocket reconnect (`daemon message: applying new image`) → `running`.

### 8.6 Sync (Panel edits a server)

1. Panel `POST /api/servers/:s/sync`.
2. Gateway: `GET /api/remote/servers/{uuid}` → update `spec.panel` (and `panelRevision`) → `POST sync` to the agent.
3. Agent: reload config from gateway `GET /internal/v1/self` (served from the CR spec; the gateway is the
   only Panel reader) → Wings `SyncWithConfiguration` + `SyncWithEnvironment`. If suspended, stop the server;
   the gateway closes websocket (4409) and SFTP sessions.
4. Operator: reconciles resources per §7.6.

### 8.7 Backups and restore

| Adapter | Flow |
|---|---|
| **wings** (local) | Agent writes `/scratch/backups/<uuid>/<backup>.tar.gz` on the pod's scratch volume (§10.1), never on the server PVC. Download via gateway `/download/backup` → agent. The scratch volume is deleted with the pod, so this adapter is for single-node, non-durable use only; the Panel's per-server backup limit bounds its size |
| **s3** (recommended) | Agent generates the archive on the scratch volume → gateway proxy `GET /backups/{b}?size=` → Panel presigned part URLs → **agent PUTs directly to S3** → `POST /backups/{b}` via gateway → temp file removed. An in-cluster S3 needs an `inClusterEgress.additional` entry in the class (§12.4) |

- **Restore:** Panel → gateway → agent. The agent runs Wings' `RestoreBackup` (S3 `download_url` fetched
  by the agent) and reports through the gateway.
- The gateway records `status.backups.pending` on create so the proxy check in §5.8 can pass, and clears
  it on completion.

### 8.8 Delete server

1. Panel `DELETE /api/servers/:s`.
2. Gateway → agent `Destroy` (kill process, publish `deleted`) → close sessions → delete CR.
3. Operator finalizer applies `deletionPolicy`:
   - `Delete`: delete PVC (local backups on the scratch volume go with the pod, matching Wings' `remove_backups_on_server_delete`).
   - `Retain`: orphan the PVC (label `pelican-k8s.io/orphaned-at`).
   - `SnapshotThenDelete`: create a VolumeSnapshot with the class's `volumeSnapshotClassName`, wait
     `ReadyToUse`, delete the PVC.

### 8.9 Pod or node restart

- **Pod deleted** (eviction, drain, operator recreate): the `preStop` hooks run Wings' stop procedure
  through the agent before any container receives SIGTERM (§6.4), so worlds are saved. The agent, a
  native sidecar, stops last. The pod's `spec.power.desired` is untouched by this path.
- **Pod recreated** (after eviction, node reboot, operator recreate): the agent boots, loads its config,
  and reads `spec.power.desired`.
  - `Running` ⇒ start (Wings' "was running before reboot" behaviour).
  - `Stopped` ⇒ stay offline.
- **Agent container restart only:** the shim keeps the game alive and the agent re-attaches (§6.4).
- **Gateway restart:**
  - browser tokens issued before the restart are rejected (cutoff), so the Panel UI fetches fresh tokens
  - agents reconnect their streams, and the state cache is rebuilt within seconds
  - `POST /api/remote/servers/reset` clears `installing` and `restoring_backup` for **every** server on
    the node. Installs and restores here outlive the gateway (they run in Jobs and agents), so the
    gateway sends `reset` only once no CR has an install or restore in progress; until then it keeps
    waiting and re-checks on every completion. On a fresh node with no servers it is sent immediately.

---

## 9. Networking

### 9.1 Gateway HTTP (Wings API, websocket, signed URLs)

- Exposed through an **Ingress** or a Gateway API **HTTPRoute** with TLS termination, e.g.
  `wings.example.com` → Service `pelican-gateway:8080`.
- Ingress timeouts must allow:
  - request/read timeout **≥ 16 min** (Panel compress and decompress calls wait up to 15 min)
  - long idle timeouts for **websockets** (hours)
  - request bodies up to the upload limit (100 MiB default)
- Panel Node settings:

  | Field | Value |
  |---|---|
  | FQDN | `wings.example.com` |
  | Scheme | `https` |
  | Behind proxy | yes |
  | `daemon_listen` | 8080 |
  | `daemon_connect` | 443 |

- The Panel reaches the gateway through the same external address (it uses `getConnectionAddress()`
  for everything), so the gateway FQDN must be resolvable from the Panel pod.

### 9.2 SFTP

TCP, so it can't go through an HTTP ingress.

| Mode | Panel node fields |
|---|---|
| NodePort | `daemon_sftp` = nodePort (e.g. 30022), `daemon_sftp_alias` = node DNS name |
| LoadBalancer | `daemon_sftp` = 2022, `daemon_sftp_alias` = LB DNS name |

### 9.3 Game ports: exposure modes

Invariant: **container port = service port = external port = Panel allocation port.**

| Mode | How | Privileges | Client IP | Panel allocations |
|---|---|---|---|---|
| **LoadBalancer** (default) | Service `type: LoadBalancer`; the class's implementation-specific annotations pin the allocation IP and allow IP sharing; `externalTrafficPolicy: Local` | none | preserved | IP = LB pool IP(s), any port 1024–65535. Works on any cluster size: the LB follows the pod |
| **NodePort** | Service `type: NodePort`, `nodePort` = allocation port, `externalTrafficPolicy: Local` | none | preserved (Local) | IP = node IP; **ports must be in the NodePort range** (30000–32767 by default) and unique across the cluster, i.e. use **one allocation IP**. **Single-node clusters only:** with `Local` the allocation IP serves traffic only while the pod runs on that node, so the operator refuses this mode when the cluster has more than one schedulable node |
| **HostPort** | `hostPort` = allocation port on the game container | namespace must be `privileged` (Pod Security `baseline` rejects any `hostPort`; §12.2) | preserved | IP = node IP; pods must land on the node owning the IP (single node only, or add per-IP node affinity) |

- `GET /api/system/ips` returns the class-configured external IPs, so the Panel's allocation dropdown matches the mode.
- **LoadBalancer** is the default because it is the only mode that keeps the allocation IP valid when a
  pod moves between nodes, and it allows "natural" game ports such as 25565. It needs a LoadBalancer
  implementation (cloud, MetalLB, kube-vip). **NodePort** needs nothing extra and is the choice for
  single-node clusters.

### 9.4 `SERVER_IP` inside the pod

Wings passes the allocation IP as `SERVER_IP`, and many eggs write it into config files as the bind
address. Inside a pod the node or LB IP is not bindable. The agent therefore:
- exports `SERVER_IP=0.0.0.0` and `INTERNAL_IP=<pod IP>`
- presents `allocations.default.ip = 0.0.0.0` to the egg config parser
- keeps the real IP available as `SERVER_PUBLIC_IP` for eggs that advertise it
- sets Wings' `docker.network.interface` to `0.0.0.0`, so egg config values that use the
  `{{config.docker.interface}}` placeholder (the Docker bridge gateway, `172.18.0.1` under Wings) resolve
  to a bindable address as well. Wings' rewrite of a `127.0.0.1` default allocation to that interface
  therefore also yields `0.0.0.0`.
- servers without an allocation (`default: 127.0.0.1:0`, `mappings: {"": []}`) get no exposure Service
  and `SERVER_PORT=0`, as under Wings.

This must be validated per egg (§19).

---

## 10. Storage

### 10.1 Volumes

- One PVC per server from the class StorageClass (RWO, must support volume expansion).
- **Size** = `disk_space × (1 + overheadPercent)`, or the class default when unlimited. The overhead
  covers logs, the activity database and install output only.
- **Two limits apply:**
  - *soft*: Wings' filesystem accounting (writes through the file API and SFTP, plus the periodic scan
    that stops the server when over the limit)
  - *hard*: the PVC size
- **Scratch volume** (class `storage.scratch`): a generic ephemeral volume (or emptyDir) mounted in the
  agent at `/scratch`, holding Wings' `backup_directory` and `tmp_directory`. A backup archive can be as
  large as the server itself, so it must not compete with the server's own quota; the default scratch
  size equals the server PVC size. It is created and deleted with the pod.

### 10.2 Expansion

Online expansion via PVC resize, if the CSI driver supports expanding attached volumes; otherwise it
takes effect at the next pod recreate. Shrinking is refused and surfaced as a condition.

### 10.3 Deletion policy

See §8.8. The default is `Delete` (Panel semantics). `SnapshotThenDelete` is the safer choice for
production.

### 10.4 Snapshots and disaster recovery

- **Prerequisite:** a CSI driver with snapshot support, the snapshot controller, and a VolumeSnapshotClass
  named in the `GameServerClass`.
- Optional class field `snapshotSchedule` (cron) makes the operator take a VolumeSnapshot per server.
  These are **crash-consistent** and invisible to the Panel; they are infrastructure DR, not Panel backups.

### 10.5 Panel backups

Use the **S3 adapter** in the Panel, pointed at a dedicated bucket. Archives are then durable
independent of the pod, and downloads are presigned by the Panel. The local `wings` adapter stores
archives on the pod's scratch volume, which does not survive pod recreation.

---

## 11. Resource mapping

| Panel `build` | Wings/Docker | Kubernetes (game container) |
|---|---|---|
| `memory_limit` (MiB) | `MemoryReservation` = limit; `Memory` = limit × overhead | `requests.memory` = limit; `limits.memory` = limit × `memoryOverheadMultiplier`. `0` (unlimited) ⇒ class `unlimitedMemoryMiB` is used for both, and `SERVER_MEMORY` reports it |
| `cpu_limit` (%) | `CPUQuota` = % × 1000 | `limits.cpu` = %/100; `requests.cpu` = limits × `cpuRequestPercentOfLimit`, at least `minCpu`. `0` ⇒ class `unlimitedCpuPercent`; if that is also 0, no limit and request = `minCpu` |
| `disk_space` (MiB) | soft quota | soft quota (agent) + PVC size |
| `swap` | `MemorySwap` | **not supported** (§16) |
| `io_weight` | `BlkioWeight` | **not supported** |
| `threads` | `CpusetCpus` | **not supported** (would need static CPU manager) |
| `oom_killer: false` | `OomKillDisable` | **not supported**: OOM kills always happen; reported as `oomKilled` |
| PIDs (`container_pid_limit` 512) | `PidsLimit` | kubelet `podPidsLimit` (node-level setting) |
| OOM detection | `OOMKilled` from container inspect | container status `lastState.terminated.reason` relayed by the operator (§6.4) |
| tmpfs `/tmp` (100 MiB) | tmpfs | `emptyDir{medium: Memory, sizeLimit}` (counts toward memory) |
| installer limits | max(server, installer_limits) | Job resources = max(server, class install resources) |

**Agent overhead:** the agent container has its own requests and limits (§7.3), accounted separately
from the game, unlike Wings' overhead multiplier. The limit must leave room for Wings' compress,
decompress and archive code paths on large servers; the class default is 512 MiB.

---

## 12. Security

### 12.1 Trust boundaries and credentials

| Credential | Held by | Never in |
|---|---|---|
| Node daemon token (`token_id.token`) | Gateway Secret | agents, game pods, install Jobs |
| SFTP host key | Gateway Secret | pods |
| Internal JWT signing key (Ed25519 private) | Gateway Secret | pods (only the public key is distributed) |
| Projected SA token (audience `pelican-gateway`) | Agent container only | game container (not mounted), install Jobs |
| Egg variables (Secret `gs-<uuid>-env`; may hold tokens and passwords) | Gateway (reads), install Job (`envFrom`), game process environment | CR spec, ConfigMaps, pod specs of the game pod |
| Panel S3 credentials | Panel | agents (they only get presigned URLs) |

**Blast radius of a compromised game process** (arbitrary egg code, RCE in a game):
- it can read and modify its own server files
- it can use the pod's network egress (limited by NetworkPolicy)
- it can connect to the shim socket in `/pelican/run`, but that only controls its own process;
  the agent is a client of that socket and exposes nothing on it
- it **cannot** reach the Panel remote API, other servers' agents, the Kubernetes API (no token), or the
  node token

A compromised **agent** container can act as its own server toward the Panel (post activity and status
for its own UUID) and nothing more.

### 12.2 Pod security

Pod Security Admission works per namespace and its exemptions apply to the *requesting* identity,
which for controller-created pods is the StatefulSet or Job controller, never the pod's
ServiceAccount. Game pods and install Jobs must share `pelican-servers` (a PVC cannot be mounted
across namespaces), and install scripts assume root (`apt`, `apk`, `chown`). The namespace is
therefore labelled **`baseline`** (`privileged` only in HostPort mode), and the restricted shape of
game pods is enforced by admission policy bound to their ServiceAccount:

| Workload | ServiceAccount | Enforced shape | Enforced by |
|---|---|---|---|
| Game pod | `pelican-game` | `restricted`: `runAsNonRoot`, pinned non-root UID/GID, `fsGroup`, drop ALL, no added capabilities, seccomp `RuntimeDefault`, `allowPrivilegeEscalation: false`, no host namespaces, no `hostPort`, only PVC/emptyDir/ephemeral/projected/configMap volumes | `ValidatingAdmissionPolicy` `pelican-game-restricted` (CEL, GA since Kubernetes 1.30) matched on `spec.serviceAccountName`, shipped by the Helm chart |
| Game pod, HostPort mode | `pelican-game-hostport` | as above plus `hostPort` = allocation ports | same policy with a `hostPort` allowance; namespace at `privileged`, since `baseline` rejects any `hostPort` |
| Install Job | `pelican-installer` | `baseline`: root allowed, no privilege escalation, no host access, no added capabilities beyond the image defaults | namespace PSA level plus a `ValidatingAdmissionPolicy` restricting volumes to the server PVC, the script ConfigMap and emptyDirs |
| Gateway, operator | own SAs | `restricted` | PSA on `pelican-system` |

- On OpenShift the same split maps onto SecurityContextConstraints bound to the three ServiceAccounts
  (`restricted-v2` for `pelican-game`, `anyuid` for `pelican-installer`, a custom SCC for `hostPort`).
- **UID pinning:** the class supplies `runAsUser`, used as UID, GID and `fsGroup`; the `prepare` init
  container and install Jobs `chown` to it. On platforms that assign a UID range per namespace and reject
  pinned UIDs outside it, the operator reads the namespace's range and uses its start instead.

### 12.3 RBAC

| Component | Permissions |
|---|---|
| Gateway | `pelican-servers`: get/list/watch/create/update/patch/delete `gameservers`, `gameservers/status`, `configmaps`, `secrets` (only `gs-*-env`, via a resourceNames-free role scoped by admission policy); get/list/watch `pods`, `endpointslices`, `services`. Cluster: `tokenreviews` create (`system:auth-delegator`), get/list `nodes` (for `/api/system/ips`, utilization and the NodePort single-node check) |
| Operator | `pelican-servers`: full on `statefulsets`, `pods` (get/list/watch, delete incl. force, `resize`), `persistentvolumeclaims`, `services`, `networkpolicies`, `jobs`, `volumesnapshots`, `events`; get `configmaps`; `gameservers` (+status, finalizers). Cluster: get/list/watch `gameserverclasses`, `nodes`; get `namespaces` |
| Game, installer SAs | no Kubernetes API access. The agent's projected token has audience `pelican-gateway` only, so the API server rejects it |

### 12.4 NetworkPolicies

- **`pelican-servers` default deny** (ingress and egress).
- **Per server:**
  - ingress on its allocation ports (TCP/UDP) from `0.0.0.0/0`, preserved client IPs thanks to
    `externalTrafficPolicy: Local`
  - ingress on the agent port from `pelican-system` gateway pods only
- **Egress for game pods and install Jobs:**
  - DNS to the cluster DNS
  - `0.0.0.0/0` **except** the cluster pod CIDR, the service CIDR, node and LAN ranges (configurable),
    and link-local
  - plus the class `network.inClusterEgress` allowances: other GameServer pods on their allocation ports
    (proxy eggs such as Velocity or Bungeecord in front of backend servers) and explicit
    `additional` CIDR/port pairs (in-cluster S3, database, or mod-repository services)
- **Egress for agents:** the above plus the gateway Service (internal API).
- **`pelican-system`:** the gateway accepts from the ingress controller and the SFTP exposure, and
  egresses to the Panel, agents and the API server.
- Kubelet probes and hooks (`startupProbe`, `livenessProbe`, `preStop` httpGet) originate from the node;
  most CNIs allow host-to-local-pod traffic implicitly. Where one does not, the per-server policy also
  admits the node CIDR on the agent port.
- Verify with the cluster's CNI that NodePort/LoadBalancer traffic with `externalTrafficPolicy: Local`
  is matched by `ipBlock` rules as expected.

### 12.5 Other controls

- Game and install pods: `automountServiceAccountToken: false`, `enableServiceLinks: false`.
- Install Jobs: `activeDeadlineSeconds`; no access to agent state (only the `volumes/<uuid>` and
  `install/<gen>` subPaths are mounted). A new install generation while a previous Job still runs
  deletes that Job first.
- Gateway: rate limits (websocket, SFTP auth throttling done by the Panel per `server|ip`, where
  `ip` is the **real client IP** passed by the gateway), upload size limit, CORS and Origin checks.
- Registry credentials: class `pullSecrets` as Secrets in `pelican-servers`.

---

## 13. State and sources of truth

| Data | Source of truth | Copies / caches |
|---|---|---|
| Server configuration (image, limits, allocations, egg config) | **Panel DB** | CR `spec.panel` (written only by the gateway on create/sync) |
| Egg variables | **Panel DB** | Secret `gs-<uuid>-env` (written only by the gateway on create/sync) |
| Cluster policy (storage class, exposure, security) | `GameServerClass` | — |
| Desired power state | CR `spec.power.desired` (gateway: power actions and agent-reported intentional stops, §4.2; editable with kubectl) | — |
| Actual process state | **Agent** (from shim) | gateway cache → Panel `container/status` (1 h cache there), CR status |
| Install progress and result | Install files on PVC plus Job status | CR `status.install`, Panel server status |
| Server files, local backups, activity queue | PVC | — |
| Panel backups (S3) | S3 plus Panel DB | — |
| Revocation, one-time tokens | Gateway memory (Redis when HA) | — |
| Websocket and SFTP sessions | Gateway | — |

**Drift handling:**
- The gateway runs a periodic **resync** (default 15 min, plus at startup): it lists Panel servers for
  the node via `GET /api/remote/servers` and compares with CRs.
- Missing CR ⇒ create it (no install; mark as adopted).
- CR without a Panel server ⇒ condition `Orphaned`, never deleted automatically.
- `panelRevision` mismatch ⇒ re-sync the spec.

---

## 14. Availability and scaling

| Component | v1 | Later |
|---|---|---|
| Panel | 1 replica (web + worker + cron in the official image) | Split into web, worker and scheduler Deployments; `SKIP_MIGRATIONS` on all but a migration Job; Redis cache and sessions |
| Gateway | 1 replica, `Recreate` | N replicas: Redis for denylist and one-time store, pub/sub for session close; state cache rebuilt from agent streams (each agent connects to one replica, others query via Redis) |
| Operator | 1 replica with leader election | same |
| Game servers | 1 pod each (at most one: StatefulSet). On a **NotReady node** the pod stays `Terminating` and the StatefulSet deliberately does not replace it; the operator sets `NodeLost` and, if the class `failover.forceDeleteAfter` is set, force-deletes the pod so it reschedules and the RWO volume reattaches (only safe when the storage layer fences the old node, or the node is confirmed down) | — |

**Scale considerations:**
- Each agent keeps one stream to the gateway.
- The informer caches EndpointSlices and CRs; hundreds of servers are fine for one gateway.
- Memory per server is roughly agent (~40–80 MiB, to be measured) plus game.

---

## 15. Observability

- **Agent:** Prometheus-format `/metrics` (process state, restarts, crash count, CPU and memory from the
  shim, disk usage, websocket clients, SFTP bytes). Structured JSON logs on stdout.
- **Game console:** `kubectl logs gs-<uuid>-0 -c game` (shim tee) and `logs/console-*.log` on the PVC.
- **Gateway:** request metrics per route class, Panel latency and errors, websocket and SFTP sessions,
  token rejections by reason, agent stream count.
- **Operator:** controller metrics, reconcile errors; Kubernetes Events on the `GameServer` (install
  started and finished, recreate, resize deferred).

---

## 16. Compatibility and limitations

| Feature | Status | Notes |
|---|---|---|
| Power, console, stats, done detection, stop command/signal | ✅ | Wings code |
| Crash detection and auto-restart | ✅ | Wings code, driven by shim exit events; OOM exits come from the container status (§6.4) |
| File manager, compress/decompress, search, remote pull | ✅ | Wings code on the PVC |
| Signed downloads and uploads | ✅ | JWT checks moved to the gateway |
| SFTP | ✅ | Gateway terminates SSH, agent serves SFTP |
| Egg config-file rewriting | ✅ | Wings parser; `SERVER_IP` and `{{config.docker.interface}}` handling per §9.4 |
| Installs and reinstalls | ✅ | Job instead of container; optional strict exit codes |
| Backups (wings, s3), restore | ✅ | Archives live on the pod's scratch volume; local-adapter backups are not durable across pod recreation |
| Activity log | ✅ | Per-agent SQLite, forwarded through the gateway |
| Suspension | ✅ | |
| Image change applies on next start | ✅ | Via pod recreate (§8.5) |
| Live resource changes | ✅ | In-place pod resize; may defer |
| Disk limit | ⚠️ | Soft (Wings) + hard (PVC); PVC cannot shrink |
| `/api/system` Docker fields, image prune, docker disk | ⚠️ | Stubbed |
| Container hostname = server UUID | ⚠️ | Pod hostname is `gs-<uuid>-0`; `/etc/machine-id` is provided |
| Server transfers | ❌ v1 | Single node identity |
| Panel mounts (`allowed_mounts`) | ❌ v1 | Could map to named PVCs later |
| `force_outgoing_ip` | ❌ v1 | Possible later via a CNI egress-IP feature |
| Swap, `io_weight`, `threads`, OOM-killer disable | ❌ | Not expressible per pod |
| Console history across an OOM kill | ⚠️ | The whole container restarts; the ring buffer is lost (§6.4) |
| Docker-specific egg assumptions (Docker network aliases, hard-coded `172.18.0.1`) | ❌ | Rare; per-egg review |

**Upstream quirks to handle defensively** (details in the contract doc):
- the Panel's `deauthorize` payload wrapping
- `ws/deny` md5 vs sha256 JTI mismatch
- calls to non-existent Wings routes (`/archive`, `DELETE /api/transfer`)

---

## 17. Future extensions

- **Multiple node identities.** Run one gateway per cluster (or per namespace) = one Panel Node each.
  **Transfers** then reuse Wings' transfer protocol: the source agent streams a multipart archive through
  its gateway to the target gateway, which creates the CR and restores into a new PVC. An alternative
  inside one cluster is PVC cloning.
- **Per-tenant namespaces.** Map Panel users or organisations to namespaces with ResourceQuota; the gateway
  routes by CR lookup across namespaces (cluster-scoped RBAC).
- **Scale-to-zero when idle.** The agent detects no players (egg-specific) and the operator scales to 0,
  with a wake-on-connect proxy in front. Files and SFTP would then need an on-demand file pod.
- **Panel mounts** mapped to ReadOnlyMany PVCs (shared mod packs).
- **Panel plugin** (optional, never required): show cluster info such as CR conditions and pod events in the Panel UI.

---

## 18. Repository layout and build

```
pelican-k8s/
├── api/v1alpha1/                 # GameServer, GameServerClass types (kubebuilder markers)
├── cmd/
│   ├── gateway/
│   ├── operator/
│   └── shim/
├── internal/
│   ├── gateway/{panelapi,wsproxy,sftprelay,revocation,routing,remoteproxy,statecache}
│   ├── operator/{controllers,imageresolve,exposure,install}
│   ├── shim/{pty,cgroup,protocol}
│   └── internalapi/              # gateway⇄agent types, internal JWT
├── third_party/wings/            # git submodule → fork, branch k8s-agent (builds `wings agent`)
├── charts/pelican-k8s/           # CRDs, gateway, operator, RBAC, pod security exceptions, classes
├── test/
│   ├── contract/                 # Panel ⇄ gateway contract tests against a real Panel container
│   └── e2e/                      # single-node and multi-node test clusters, egg matrix
└── docs/
```

- **Go:** Wings requires Go 1.25; align the workspace toolchain.
- **Images:** `gateway`, `operator`, `shim` (scratch), `agent` (distroless, non-root).
- **Licensing:** the Wings fork stays MIT; the Panel (AGPL) is used unmodified as a separate service.
- **Upstream strategy:** submit fork changes 2–6 (§6.3) as small, independent Wings PRs (environment
  factory, remote-client injection, non-Docker boot). Each one merged shrinks the fork.

---

## 19. Milestones

| # | Milestone | Exit criteria |
|---|---|---|
| **M0** | **Spike: agent + shim without Kubernetes** | `wings agent` plus shim run the Paper egg locally (container runtime or bare process) from a static server config; console, done detection, stop command, crash restart, `prestop` stop procedure and file API work with hand-minted JWTs |
| **M1** | **Operator + CRD, kubectl-only** | Applying a `GameServer` yields StatefulSet, PVC, Services and NetworkPolicy; the server starts; `spec.power.desired` toggles it and follows console stops; resize and image change behave per §7.6; pod deletion stops the game cleanly via `preStop`; OOM is reported from the container status; the namespace runs at `baseline` with the game-pod `ValidatingAdmissionPolicy` enforced |
| **M2** | **Gateway, Panel-facing core** | The Panel accepts the node (system info, no `User-Agent` errors); create (without install), power, console websocket, file manager and signed downloads/uploads work end-to-end |
| **M3** | **Install, SFTP, backups, activity** | Egg installs via Job incl. chown; SFTP with password and key; S3 backups and restore; activity visible in the Panel; suspend and deauthorize close sessions |
| **M4** | **Hardening** | Admission policies finalized (vanilla PSA + VAP, OpenShift SCC); NetworkPolicies verified (including LoadBalancer/NodePort with `Local`, in-cluster egress allowances, kubelet probes); snapshot integration and `SnapshotThenDelete`; metrics and dashboards; drift resync; node-loss fencing; contract test suite in CI |
| **M5** | **Egg matrix and ops** | Validated eggs: Paper, Vanilla, Forge/NeoForge, Velocity in front of Paper (in-cluster egress), Valheim (SteamCMD, UDP), Terraria/tModLoader, Satisfactory, one bot egg (no allocation); docs for Panel node setup; upgrade procedure |
| M6 (opt.) | Gateway HA | Redis-backed revocation; 2 replicas; zero-downtime gateway rollout |

---

## 20. Open questions and risks

### Open questions

1. **Names:** project name and CRD API group (`pelican-k8s.io` is a placeholder).
2. **LoadBalancer IP sharing:** which implementations' annotations to support out of the box (MetalLB,
   kube-vip, cloud providers) for pinning allocation IPs and sharing one IP across Services.
3. **Image entrypoint resolution:** whether the in-image probe (yolk `/entrypoint.sh` convention) plus
   overrides is enough, or registry lookups should be on by default.
4. **Install exit codes:** keep Wings' lenient behaviour (default) or strict?
5. **`SERVER_IP` rewrite:** is `0.0.0.0` plus `SERVER_PUBLIC_IP` acceptable for all eggs in use?
6. **Panel database:** MySQL/MariaDB (most tested) vs. PostgreSQL (supported by the Laravel config;
   Pelican-specific migration coverage not verified).
7. **Advertised Wings version** in `User-Agent` and `/api/system`: does the Panel gate features on it?
   (Not verified.)

### Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Pelican is still beta; the Panel⇄Wings contract changes | Gateway or agent break on Panel upgrades | Pin Panel version; contract tests (§18) against each new Panel release; track upstream Wings changes in the fork |
| Wings internals shift; Pelican has marked Kubernetes support "not planned" | Fork rebase cost; refactors may never be accepted upstream | Keep the fork diff small and generic; treat the fork as permanent and track upstream releases |
| OOM kills restart the whole game container under kubelet backoff (§6.4) | Slower recovery after repeated OOMs; console history lost | Operator relays exit state; `singleProcessOOMKill` on dedicated nodes; document memory sizing |
| PTY/shim behaviour differs from Docker attach | Console quirks (echo, ANSI, line splitting) | Reuse Wings' `ScanReader`; egg matrix tests |
| Eggs break under a non-default UID or read-only root | Servers fail to start | Generated passwd entry, per-class `runAsUser`, document per egg |
| RWO volume attach delays on multi-node reschedule | Longer failover | Acceptable for persistent servers; storage-level replication |
| Websocket and SFTP both through a single gateway | Gateway restart drops all consoles and SFTP sessions briefly | Same as a Wings restart today; HA in M6 |
| Unreplicated storage | Disk loss = data loss | VolumeSnapshots to off-cluster storage, Panel S3 backups |

---

## Appendix A — Glossary

| Term | Meaning |
|---|---|
| **Panel** | Pelican Panel (Laravel web app), unmodified |
| **Node** | Panel concept: one Wings endpoint with token, FQDN and allocations. Here: the gateway |
| **Allocation** | Panel `(ip, port)` assigned to a server |
| **Egg** | Panel template: images, startup command, install script, config parsers, variables |
| **Yolks** | Pelican's runtime images (`ghcr.io/pelican-eggs/yolks:*`) |
| **Wings** | Pelican's Docker daemon; here forked into the agent |
| **Agent** | Wings in single-server mode, native sidecar in each game pod |
| **Shim** | PID 1 of the game container; supervises the egg process |
| **Gateway** | Panel-facing node implementation plus traffic router |
| **Operator** | Controller reconciling `GameServer` resources |

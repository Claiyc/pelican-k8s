# Wings ⇄ Panel contract reference

Companion to [`../ARCHITECTURE.md`](../ARCHITECTURE.md). This is the protocol surface the
**gateway** and **agent** must reproduce so an unmodified Pelican Panel treats them as a normal
Wings node.

**Source basis** (read from source on 2026-09-15 unless marked *[inferred]*):

| Repo | Commit | Notes |
|---|---|---|
| `github.com/pelican/wings` (formerly `pelican-dev/wings`) | `65422ff` (2026-08-28) | Go module `github.com/pelican/wings`, MIT |
| `github.com/pelican/panel` | `6ba5264` (2026-09-14) | Laravel 13 / Filament 5, AGPL-3.0 |

Paths below are relative to the respective repo root. Pelican is still in beta
(Panel `v1.0.0-beta38`, Wings `v1.0.0-beta29`), so **re-verify this document when bumping versions**.

---

## 1. Authentication primitives

### 1.1 Panel → Wings (node token)

- Laravel macro `Http::daemon` (`app/Providers/AppServiceProvider.php:83-93`):
  `baseUrl($node->getConnectionAddress())`, `withToken($node->daemon_token)` →
  `Authorization: Bearer <token>` (token only, no id). TLS verify only in production.
- `Node::getConnectionAddress()` = `"$scheme://$fqdn:$daemon_connect"` (`app/Models/Node.php:219-222`).
  The same address is used for the API, the browser's websocket URL, signed download/upload URLs,
  and the JWT `aud` claim.
- Wings check: `middleware.RequireAuthorization()` (`router/middleware/middleware.go:168-189`) compares
  in constant time against `config.token`.
- **Response header check:** `DaemonRepository::getHttpClient` → `enforceValidNodeToken`
  (`app/Repositories/Daemon/DaemonRepository.php:48-67`) **throws if the response `User-Agent` header
  is empty, or if it matches `^Pelican Wings\/v(\d+\.\d+\.\d+|develop) \(id:(\w*)\)$` with an id that
  is not the node's `daemon_token_id`.** Wings sets `User-Agent: Pelican Wings/v<ver> (id:<token_id>)`
  in `RequireAuthorization` (middleware.go:174).
  → **The gateway must emit this header on every response.**

### 1.2 Wings → Panel (remote API)

- Base `<remote>/api/remote`. Header `Authorization: Bearer <token_id>.<token>`, UA
  `Pelican Wings/v<ver> (id:<token_id>)`, plus `remote_query.custom_headers`
  (`remote/http.go:111-138`). 5xx responses are retried with backoff for at most 30 s.
- Panel `DaemonAuthenticate` (`app/Http/Middleware/Api/Daemon/DaemonAuthenticate.php:28-50`) splits the
  token on `.` (exactly two parts), looks up `Node where daemon_token_id`, runs `hash_equals` on the
  token, and binds the node to the request.
- **Every per-server remote endpoint checks `server.node_id == authenticated node`.** A gateway that
  impersonates one node owns all servers it routes.

### 1.3 JWTs (browser-facing tokens)

- **HS256, key = node daemon token.** Panel: `NodeJWTService.php:77`
  (`forSymmetricSigner(new Sha256(), InMemory::plainText($node->daemon_token))`). Wings:
  `config/config.go:441-443`.
  → **Anyone holding the node token can mint tokens for every server on the node.**
- Wings validates **only `exp`** (`router/tokens/parser.go:22-30`); `iss`, `aud` and `nbf` are not checked.
- Standard claims set by the Panel (`NodeJWTService.php:79-107`): `iss`, `aud`, `jti = sha256(user.id . server.uuid)`,
  `iat`, `nbf = now-5min`, `exp`, optional `sub`, `user_uuid`, `scope`, `unique_id = Str::random()`.
- Scopes (`app/Enums/NodeJwtScope.php`, `router/tokens/token.go:11-17`): `websocket`, `file-upload`,
  `file-download`, `backup-download`, `transfer`.

| Token | Issued by | Extra claims | TTL | Server UUID from |
|---|---|---|---|---|
| Websocket | `WebsocketController.php:56-64` (plus Filament `ServerConsole.php`) | `server_uuid`, `permissions[]`, `user_uuid` | 10 min | path, must equal `server_uuid` (`router/websocket/websocket.go:220`) |
| File download | `FileController.php:95-103` | `file_path`, `server_uuid`, `user_uuid` | 15 min | `server_uuid` |
| File upload | `FileUploadController.php:47-52` | `server_uuid`, `user_uuid` | 15 min | `server_uuid` |
| Backup download | `WingsBackupSchema.php:46-54` | `backup_uuid`, `server_uuid`, `user_uuid` | 15 min | `server_uuid` |
| Transfer | `TransferServerService.php:109-113` | `sub = server uuid` | 15 min | `sub` |

### 1.4 Revocation and replay state (all in memory, per Wings process)

- **Boot-time cutoff** (`router/websocket/websocket.go:18,59`): a user-bound token with `iat` before
  Wings' start time is rejected.
- **User denylist** `userDenylist` keyed `"<server>:<user>"` (`websocket.go:30-48`), written by
  `POST /api/deauthorize-user {user, servers[]}` (`router/router_system.go:261-289`). That handler also
  runs `Websockets().CancelAll()` and `Sftp().Cancel(user)`. **If `servers` is empty it cancels sessions
  on all servers but writes no deny entry.**
- **Legacy JTI denylist** via `POST /api/servers/:s/ws/deny {jtis[]}` (`router_server.go:327-341`).
  The Panel sends `md5(...)` (`DaemonServerRepository.php:145`) while it signs `jti = sha256(...)`,
  so this path never matches.
- **One-time use** for download, upload and backup tokens: `TokenStore` (go-cache, 60 min TTL, keyed by
  `unique_id`; `router/tokens/token_store.go:14-42`).
- *[inferred, likely Panel bug]* `DaemonServerRepository::deauthorize` posts `['json' => [...]]`
  (`:156-161`) through the Laravel HTTP client, which wraps the payload, so Wings receives an empty
  `user`/`servers` and takes the "cancel everything, deny nothing" branch.

---

## 2. Wings HTTP routes (`router/router.go`)

Global middleware (22-45): recovery, request id, error capture, CORS (panel URL plus `allowed_origins`),
server manager, API client.

Legend for **Scope**: **N** = node-scoped; **S** = server UUID in path; **T** = server UUID only inside the token.

### 2.1 Public (JWT-authenticated) routes

| Method | Path | Line | Scope | Auth |
|---|---|---|---|---|
| GET | `/download/backup` | 48 | T | `?token=` backup-download JWT, one-time |
| GET | `/download/file` | 49 | T | `?token=` file-download JWT, one-time |
| POST | `/upload/file` | 50 | T | `?token=` file-upload JWT, one-time; `?directory=` |
| GET | `/api/servers/:server/ws` | 55 | S | `ServerExists`; JWT arrives in the first `auth` message |
| POST | `/api/transfers` | 60 | T | `Authorization: Bearer <transfer JWT>`, UUID in `sub` |

### 2.2 Node-token routes

| Method | Path | Scope | Panel caller |
|---|---|---|---|
| POST | `/api/update` | N | `DaemonSystemRepository` (pushes `Node::getConfiguration()`) |
| GET | `/api/system` | N | `DaemonSystemRepository` (3 s timeout); the UI reads `version`/`exception` (`NodeSystemInformation.php:23-24`) |
| GET | `/api/diagnostics` | N | `DaemonSystemRepository` |
| GET | `/api/system/docker/disk` | N | no caller found |
| DELETE | `/api/system/docker/image/prune` | N | `PruneImagesCommand.php:38-55` (expects `ImagesDeleted`, `SpaceReclaimed`) |
| GET | `/api/system/ips` | N | `Node.php:441` (`ip_addresses`) |
| GET | `/api/system/utilization` | N | `Node.php:418-422` (`memory_total`, …) |
| GET | `/api/servers` | N | list |
| POST | `/api/servers` | N (UUID in body) | `DaemonServerRepository::create {uuid, start_on_completion}` |
| DELETE | `/api/transfers/:server` | S | — |
| POST | `/api/deauthorize-user` | N | `RevokeSftpAccessJob` |

### 2.3 Server routes (`/api/servers/:server`, node token + `ServerExists`)

| Group | Routes |
|---|---|
| Core | `GET ""` (Panel: 1 s timeout, reads `state`; on failure the Panel assumes `state: "missing"`), `DELETE ""`, `GET /logs`, `GET /install-logs`, `POST /power {action}`, `POST /commands`, `POST /install`, `POST /reinstall` (409 if a power action is running), `POST /sync`, `POST /ws/deny`, `POST /transfer`, `DELETE /transfer`, `DELETE /deleteAllBackups` |
| Files | `GET /files/contents`, `GET /files/list-directory`, `PUT /files/rename`, `POST /files/{copy,write,create-directory,delete,compress,decompress,chmod}`, `GET /files/search`, `GET/POST /files/pull`, `DELETE /files/pull/:download` |
| Backups | `POST /backup {adapter, uuid, ignore}`, `POST /backup/:backup/restore {adapter, truncate_directory, download_url}`, `DELETE /backup/:backup` |

Panel timeouts worth noting: compress and decompress 15 min, search 2 min (`DaemonFileRepository`).

*[inferred]* The Panel also calls `POST /api/servers/{uuid}/archive` and `DELETE /api/transfer`, which have
no matching Wings routes (legacy or broken).

---

## 3. Panel remote API (`routes/api-remote.php`, prefix `/api/remote`, middleware `daemon`)

| Method | Path | Controller | Used for |
|---|---|---|---|
| POST | `/sftp/auth` | `SftpAuthenticationController` | SFTP login |
| GET | `/servers?page&per_page` | `ServerDetailsController@list` | boot sync (paged by node) |
| POST | `/servers/reset` | `@resetState` | once per boot: clears `Installing`/`RestoringBackup` for **all** node servers |
| POST | `/activity` | `ActivityProcessingController` | activity batches `{data:[…]}` |
| GET | `/servers/{uuid}` | `ServerDetailsController` | `{settings, process_configuration}` |
| GET | `/servers/{uuid}/install` | `ServerInstallController@index` | `{container_image, entrypoint, script}` |
| POST | `/servers/{uuid}/install` | `@store` | `{successful, reinstall}` |
| POST | `/servers/{uuid}/transfer/{failure,success}` | `ServerTransferController` | transfers |
| POST | `/servers/{uuid}/container/status` | `ServerContainersController@status` | `{data:{previous_state,new_state}}`, cached 1 h |
| GET | `/backups/{uuid}?size=` | `BackupRemoteUploadController` | S3 presigned part URLs `{parts[], part_size}` |
| POST | `/backups/{uuid}` | `BackupStatusController@index` | `{checksum, checksum_type, size, successful, parts[]}` |
| POST | `/backups/{uuid}/restore` | `@restore` | `{successful}` |

*[inferred]* Wings' `SetArchiveStatus` posts `/servers/{uuid}/archive`, which has no Panel route (dead code).

State reads: `Server::retrieveStatus()` caches `GET /api/servers/{uuid}` for 15 s, and a pushed
`container/status` value wins for up to 1 h (`Server.php:476-486`, `ServerContainersController.php:17-26`).

---

## 4. Server configuration payload

`GET /api/remote/servers/{uuid}` → `{settings, process_configuration}`.

**`settings`** (`ServerConfigurationStructureService.php:70-126`, marked "DO NOT MODIFY"):

```jsonc
{
  "id": 1, "uuid": "…", "meta": {"name": "…", "description": "…"},
  "suspended": false,
  "environment": {"SERVER_JARFILE": "server.jar", "…": "…",
                  "STARTUP": "<raw startup>", "P_SERVER_UUID": "<uuid>"},   // EnvironmentService.php
  "invocation": "java -Xms128M -Xmx{{SERVER_MEMORY}}M -jar {{SERVER_JARFILE}}",
  "skip_egg_scripts": false,
  "build": {"memory_limit": 4096, "swap": 0, "io_weight": 500, "cpu_limit": 200,
            "threads": null, "disk_space": 10240, "oom_killer": true},   // 0 = unlimited for memory_limit/cpu_limit/disk_space
  "container": {"image": "ghcr.io/pelican-eggs/yolks:java_21", "requires_rebuild": false},
  "allocations": {"force_outgoing_ip": false,
                  "default": {"ip": "0.0.0.0", "port": 25565},
                  "mappings": {"0.0.0.0": [25565, 25575]}},
  "egg": {"id": "<egg uuid>", "file_denylist": [],
          "features": {"<feature id>": ["<console listener substring>", "…"]}},   // FeatureService::getMappings
  "labels": {}, "mounts": [{"source": "…", "target": "…", "read_only": true}]
}
```

Servers **without an allocation** (allowed since 2025-06) arrive as
`"default": {"ip": "127.0.0.1", "port": 0}` and `"mappings": {"": []}`. `container.image` may carry a
leading `~`, which Wings strips and interprets as "never pull".

**`process_configuration`** (`EggConfigurationService.php:26-81`):
`startup{done: string[], user_interaction: [], strip_ansi}`, `stop{type: "command"|"signal", value}`
(an egg stop of `^C` becomes a signal `C`, which Wings maps to SIGINT), and
`configs[{file, parser, replace[{match, if_value?, replace_with}]}]`.

Environment given to the process (`server/server.go:224-246`): `TZ`, `STARTUP` (invocation with
`{{VAR}}` substituted), `SERVER_MEMORY`, `SERVER_IP`, `SERVER_PORT`, plus uppercased egg variables.
**Wings never executes `STARTUP` itself:** the yolk image entrypoint reads and `eval`s it.
If the default allocation IP is `127.0.0.1` and the port is non-zero, `SERVER_IP` is rewritten to
`docker.network.interface` (the bridge gateway, `172.18.0.1`). The same interface address is what
Wings' parser substitutes for the `{{config.docker.interface}}` placeholder in `replace_with` values;
the Panel pre-expands every other placeholder (`{{server.build.default.port}}`, `{{env.X}}`).

---

## 5. SFTP

- Username format `^(?i)(.+)\.([a-z0-9]{8})$` → `<panel username>.<uuid_short>` (`sftp/server.go:31`).
  Panel connection string: `sftp://<user>.<uuid_short>@<daemon_sftp_alias ?? fqdn>:<daemon_sftp>`
  (`app/Models/Server.php:561-563`).
- Auth request `POST /api/remote/sftp/auth` with body
  `{type: password|public_key, username, password|authorized_key, ip, session_id, client_version}`
  (`remote/types.go:75-82`).
- Panel (`SftpAuthenticationController.php`):
  - splits the username on the last `.` and throttles per `server|ip`
  - requires the server to be on the authenticating node
  - checks the password, or the public key by sha256 fingerprint
  - requires owner, admin, or subuser permission `file.sftp`, and validates server state
  - responds `{user: <uuid>, server: <uuid>, permissions: [...]}`
- Wings stores these in `ssh.Permissions.Extensions` (`server.go:276-285`). The file handler checks
  `file.read`, `file.read-content`, `file.create`, `file.update` and `file.delete` (`sftp/handler.go:21-25`),
  and `sftp.read_only` applies globally.
- Host key: ED25519 at `<system.data>/.sftp/id_ed25519`, generated if missing. Algorithms are pinned
  (`server.go:71-86`); `MaxAuthTries` is 6.
- Sessions are tracked per user (`srv.Sftp().Context(user)`) and cancelled on install, transfer, restore,
  suspend and deauthorize.

---

## 6. Websocket protocol

- URL given to the browser: `ws(s)://<fqdn>:<daemon_connect>/api/servers/<uuid>/ws` (`WebsocketController.php:66-71`).
- Limits: 30 connections per server; Origin must be the panel URL or one of `allowed_origins`; read limit
  4096 bytes; compression on; a suspended server gets close code **4409**. Inbound rate limiting
  (`router/websocket/limiter.go`, event `throttled`): a global limiter of 1 message per 200 ms with a
  burst of 10, plus per-event limiters (`auth` and `send logs` 1 per 5 s, burst 2; `send command`
  1 per second, burst 10; everything else 1 per second, burst 4).
- Frame: `{"event": string, "args": [string]}`.
- **Inbound events:**
  - `auth [jwt]` → `auth success`. The first auth registers listeners, sends `status`, and sends `stats` if offline.
  - `set state [start|stop|restart|kill]`, gated by `control.*` permissions.
  - `send logs` → `Readlog(websocket_log_count=150)` as `console output`.
  - `send stats`
  - `send command [cmd]`: needs `control.console`.
  - Every non-auth message re-validates the token (`jwt error` on failure).
- **Outbound events:** `auth success`, `token expiring`, `token expired` (30 s ticker, warning at ≤ 60 s),
  `daemon error`, `jwt error`, `throttled`, `console output`, `status`, `stats`, `install output`
  (needs `admin.websocket.install`), `install started`, `install completed`, `daemon message`,
  `backup completed:<uuid>` (needs `backup.read`), `backup restore completed`, `transfer logs`
  (`admin.websocket.transfer`), `transfer status`, `deleted`, `feature match`.

Stats JSON: `{memory_bytes, memory_limit_bytes, cpu_absolute, network{rx_bytes,tx_bytes}, disk_io{read_bytes,write_bytes}, uptime, state, disk_bytes}`.

---

## 7. Server lifecycle inside Wings

- **Boot** (`cmd/root.go:113-430`):
  1. directories → pelican user → passwd files → logrotate
  2. Panel client → SQLite activity DB → quotas
  3. `server.NewManager` (paged `GET /servers`) → **`environment.ConfigureDocker`** → read `states.json`
  4. per server: ensure data directory → `IsRunning` → reattach, or auto-start if the last state was running.
     `states.json` (`<root_directory>/states.json`) is written periodically by `Manager.PersistStates`
     with each server's **actual** environment state, so a server stopped from the console is not
     restarted after a reboot
  5. cron → SFTP → `POST /servers/reset` → HTTP server
- **Manager** `InitServer` (`server/manager.go:187-239`) builds the filesystem at `<data>/<uuid>` and the
  environment via a **hard-coded `docker.New(...)`** (222-231).
- **Power** `HandlePowerAction` (`server/power.go:57-168`): refused while installing, transferring or
  restoring; takes a power lock. Start: `onBeforeStart` (Sync, suspend check, `SyncWithEnvironment`, disk
  check, `UpdateConfigurationFiles`, chown, machine-id) → `Env.Start`. Stop/restart: `WaitForStop(10 min, terminate)`.
  Kill: `Terminate(SIGKILL)`.
- **Done detection:** while `starting`, a console line matching `startup.done` (substring or `regex:`)
  switches to `running` (`server/listeners.go:156-193`). A console line equal to the stop command sets offline.
- **State push:** `OnStateChange` always posts `container/status` (`server/server.go:402-452`) and
  triggers crash detection on starting/running → offline.
- **Crash handling** `handleServerCrash` (`server/crash.go:49-106`): runs only on a `starting`/`running`
  → `offline` transition, so a `stopping` → `offline` transition (power stop, stop command typed into the
  console, suspension) never counts as a crash. Reads `ExitState()` (exit code, OOM); with
  `detect_clean_exit_as_crash=false` a clean exit is not a crash (the default is `true`); records the
  last console lines as activity; restarts unless the previous crash was within
  `crash_detection.timeout` (60 s).
- **Sync** (`server.go:258-290`, `update.go:21-68`): re-fetch config, update disk limit, then
  `SyncWithEnvironment`. **This type-asserts `*docker.Environment` to push the image and stop config**
  (update.go:36-40). If the server is suspended, it stops the server and cancels websockets and SFTP.
- **Install** (`server/install.go`):
  1. `GET /servers/{uuid}/install`, write `<tmp>/<uuid>/install.sh`
  2. container `<uuid>_installer` running `[entrypoint, /mnt/install/install.sh]`, with the server directory
     at `/mnt/server`, **no User set (root)**, limits = max(server, installer_limits)
  3. stream output to `install output`, write `<log_dir>/install/<uuid>.log`
  4. `POST /servers/{uuid}/install {successful, reinstall}`

  The script's exit code is ignored; only a container-wait error marks the install as failed.
- **Backups:**
  - local adapter: `<backup_dir>/<server>/<uuid>.tar.gz`
  - S3 adapter: generate locally, `GET /backups/{uuid}?size=` for presigned part URLs, PUT the parts, then
    `POST /backups/{uuid}` with ETags
  - restore: suspends the server, stops it (2 min, no kill), optionally truncates, streams the archive
    (local file or `download_url`, private address ranges blocked), then `POST /backups/{uuid}/restore`
- **Activity:** SQLite `<root>/wings.db`; cron every 60 s sends ≤ 100 rows to `POST /activity`
  (SFTP events aggregated per minute).

### 7.1 Docker coupling outside `environment/docker`

| Location | Effect | Needed in agent mode? |
|---|---|---|
| `cmd/root.go:96-124` | Docker `Info`, `log.Fatalf` if unreachable | remove |
| `cmd/root.go:142` `EnsurePelicanUser` | `useradd` | skip (rootless/distroless path) |
| `cmd/root.go:146,154` passwd, logrotate | host files | disable via config |
| `server/manager.go:222-231` | hard-coded `docker.New` | **replace with factory** |
| `cmd/root.go:191` `ConfigureDocker` | network setup (fatal) | skip |
| `system/system.go:106-174,391-410` | `/api/system` fails without Docker | gateway answers |
| `system/system.go:335-389` | docker disk, prune | gateway stubs |
| `server/install.go` | installer container | **replace** (external install) |
| `server/update.go:36-40` | docker type assertion for image and stop config | **generic interface** |
| `router/websocket/websocket.go:445-449` | `IsAttached` gate | n/a for non-docker env |
| `server/power.go:223-230` | chown to uid 988 | `check_permissions_on_boot: false` |
| `internal/diagnostics` | docker info | optional |

### 7.2 `environment.ProcessEnvironment` (`environment/environment.go:27-115`)

```go
Type() string
Config() *Configuration
Events() *events.Bus                 // "state change", "resources", docker pull events
Exists() (bool, error)
IsRunning(ctx) (bool, error)
InSituUpdate() error
OnBeforeStart(ctx) error
Start(ctx) error
Stop(ctx) error
WaitForStop(ctx, d time.Duration, terminate bool) error
Terminate(ctx, signal string) error
Destroy() error
ExitState() (exitCode uint32, oomKilled bool, err error)
Create() error
Attach(ctx) error
SendCommand(string) error
Readlog(lines int) ([]string, error)
State() string                       // offline | starting | running | stopping
SetState(string)
Uptime(ctx) (int64, error)
SetLogCallback(func([]byte))
```

What the Docker implementation does, which a replacement must emulate:

| Method | Docker behaviour |
|---|---|
| `Attach` | before start: stream output to the log callback; stream end ⇒ `offline` (which triggers crash detection) |
| `Start` | truncate log, `starting`, recreate container, attach, start; on error `stopping`→`offline` (no crash) |
| Stop, type `signal` | SIGINT/SIGTERM/SIGABRT, anything else ⇒ SIGKILL |
| Stop, type `command` | write the command to stdin |
| `Terminate` | poll 10 s, then SIGKILL |
| `Readlog(n)` | tail of the per-run log |
| `ExitState` | exit code plus OOM-killed flag |
| Resources | stats stream while attached |

---

## 8. Wings `config.yml` keys used by the design

- Top level: `token_id`, `token` (also `WINGS_TOKEN_ID`/`WINGS_TOKEN` env or `file://`), `remote`,
  `ignore_panel_config_updates`, `allowed_origins`, `allowed_mounts`.
- `api`: `host`, `port` (8080), `ssl`, `upload_limit` (100 MiB), `trusted_proxies`.
- `system`:

  | Key | Default |
  |---|---|
  | `root_directory` | `/var/lib/pelican` |
  | `data` | `…/volumes` |
  | `backup_directory` | `…/backups` |
  | `archive_directory` | `…/archives` |
  | `log_directory` | `/var/log/pelican` |
  | `tmp_directory` | `/tmp/pelican` |
  | `user{uid,gid,rootless}` | — |
  | `passwd.enable` | — |
  | `machine_id.enable` | — |
  | `check_permissions_on_boot` | — |
  | `enable_log_rotate` | — |
  | `websocket_log_count` | 150 |
  | `sftp{bind_port 2022, read_only, key_only}` | — |
  | `crash_detection{enabled, detect_clean_exit_as_crash, timeout}` | `true`, `true`, 60 |
  | `backups{write_limit, compression_level, restore_host_allowlist, remove_backups_on_server_delete}` | — |
  | `activity_send_interval` / `activity_send_count` | — |

- `throttles{enabled, lines 2000, line_reset_interval 100}`.
- `docker.*`: network, `tmpfs_size` 100, `container_pid_limit` 512, `installer_limits`, `overhead` multipliers.
- Panel-generated node config (`Node.php:246-272`) sets only: `uuid`, `token_id`, `token`,
  `api{host, port=daemon_listen, ssl, upload_limit}`, `system{data, sftp.bind_port}`, `allowed_mounts`, `remote`.
  `daemon_listen` (bind port) and `daemon_connect` (advertised port) are separate fields.

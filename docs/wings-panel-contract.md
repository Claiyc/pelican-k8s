# Wings ⇄ Panel contract reference

Companion to [`../ARCHITECTURE.md`](../ARCHITECTURE.md). This is the protocol surface the
**gateway** and **agent** must reproduce so an unmodified Pelican Panel treats them as a normal
Wings node.

**Source basis** (read from source at these commits; *[inferred]* marks what was not):

| Repo | Commit | Notes |
|---|---|---|
| `github.com/pelican/wings` | `c89076f` (2026-11-21, after `v1.0.0-rc.3`) | Go module `github.com/pelican/wings`, MIT; Docker client `github.com/moby/moby` |
| `github.com/pelican/panel` | `cb455ad` (2026-11-19, `v1.0.0-rc.4`) | Laravel 13 / Filament 5, AGPL-3.0 |

Paths below are relative to the respective repo root. Pelican is still at release candidates
(Panel `v1.0.0-rc.4`, Wings `v1.0.0-rc.3`), so
**re-verify this document when bumping versions**.

---

## 1. Authentication primitives

### 1.1 Panel → Wings (node token)

- Laravel macro `Http::daemon` (`app/Providers/AppServiceProvider.php:85-95`):
  `baseUrl($node->getConnectionAddress())`, `withToken($node->daemon_token)` →
  `Authorization: Bearer <token>` (token only, no id). TLS verify only in production.
- `Node::getConnectionAddress()` = `"$scheme://$fqdn:$daemon_connect"` (`app/Models/Node.php:321-324`).
  The same address is used for the API, the browser's websocket URL, signed download/upload URLs,
  and the JWT `aud` claim.
- Wings check: `middleware.RequireAuthorization()` (`router/middleware/middleware.go:180-211`) compares
  in constant time against `config.token`. With `api.panel_allowlist` set (config.yml only), a request
  from a client address outside it gets `403` before the token is checked (`:188-194`; behind a reverse
  proxy the proxy must be in `api.trusted_proxies`).
- **Response header check:** `DaemonRepository::getHttpClient` → `enforceValidNodeToken`
  (`app/Repositories/Daemon/DaemonRepository.php:48-67`) **throws if the response `User-Agent` header
  is empty, or if it matches `^Pelican Wings\/v(?:\d+\.\d+\.\d+|develop) \(id:(\w*)\)$` with an id that
  is not the node's `daemon_token_id`.** Wings sets `User-Agent: Pelican Wings/v<ver> (id:<token_id>)`
  in `RequireAuthorization` (middleware.go:186).
  → **The gateway must emit this header on every response.**

### 1.2 Wings → Panel (remote API)

- Base `<remote>/api/remote`. Header `Authorization: Bearer <token_id>.<token>`, UA
  `Pelican Wings/v<ver> (id:<token_id>)`, plus `remote_query.custom_headers`
  (`remote/http.go:130-158`). 5xx responses are retried with backoff for at most 30 s.
- Panel `DaemonAuthenticate` (`app/Http/Middleware/Api/Daemon/DaemonAuthenticate.php:28-50`) splits the
  token on `.` (exactly two parts), looks up `Node where daemon_token_id`, runs `hash_equals` on the
  token, and binds the node to the request.
- **Every per-server remote endpoint checks `server.node_id == authenticated node`.** A gateway that
  impersonates one node owns all servers it routes.

### 1.3 JWTs (browser-facing tokens)

- **HS256, key = node daemon token.** Panel: `NodeJWTService.php:77`
  (`forSymmetricSigner(new Sha256(), InMemory::plainText($node->daemon_token))`). Wings:
  `config/config.go:464-466`.
  → **Anyone holding the node token can mint tokens for every server on the node.**
- Wings validates **only `exp`** (`router/tokens/parser.go:26-36`); `iss`, `aud` and `nbf` are not checked.
  A malformed, badly signed or expired token is answered `401` (`ErrInvalidToken`, `router/middleware/middleware.go:92-96`).
- Standard claims set by the Panel (`NodeJWTService.php:79-107`): `iss`, `aud`, `jti = sha256(user.id . server.uuid)`,
  `iat`, `nbf = now-5min`, `exp`, optional `sub`, `user_uuid`, `scope`, `unique_id = Str::random()`.
- Scopes (`app/Enums/NodeJwtScope.php`, `router/tokens/token.go:11-17`): `websocket`, `file-upload`,
  `file-download`, `backup-download`, `transfer`.

| Token | Issued by | Extra claims | TTL | Server UUID from |
|---|---|---|---|---|
| Websocket | `WebsocketController.php:56-64` (plus Filament `ServerConsole.php`) | `server_uuid`, `permissions[]`, `user_uuid` | 10 min | path; the `auth` token and every later check must carry the path's `server_uuid` (`router/websocket/websocket.go:261,379`) |
| File download | `FileController.php:95-103` | `file_path`, `server_uuid`, `user_uuid` | 15 min | `server_uuid` |
| File upload | `FileUploadController.php:47-52` | `server_uuid`, `user_uuid` | 15 min | `server_uuid` |
| Backup download | `WingsBackupSchema.php:63-66,77-100` | `backup_uuid`, `server_uuid`, `user_uuid` | 15 min | `server_uuid` |
| Backup restore link | `WingsBackupSchema.php:72-100` | `backup_uuid`, `server_uuid`; no `user_uuid`; `jti = sha256('restore' . server_uuid)` | 60 min for a restore (`ServerRestoreService.php:33`), 5 min for the Panel's own config read (`BackupConfigResolver.php:73`) | `server_uuid` |
| Transfer | `TransferServerService.php:116-120` | `sub = server uuid` | 15 min | `sub` |

Both backup tokens are signed with the token of the node that holds the backup and point at its
`/download/backup`: `Backup::sourceNode()` is `backups.node_id`, else the server's node
(`app/Models/Backup.php:145-148`); `server_uuid` is the backup's server, also after that server was
deleted (`sourceServerUuid()`, `:136-139`).

### 1.4 Revocation and replay state (all in memory, per Wings process)

- **Boot-time cutoff** (`router/tokens/websocket.go:18,59`): a user-bound token with `iat` before
  Wings' start time is rejected. A backup-download token without `user_uuid` is accepted only with an
  `iat` after Wings' start, a non-empty `server_uuid`, and a `jti` not on the JTI denylist
  (`router/tokens/backup.go:30-41`).
- **User denylist** `userDenylist` keyed `"<server>:<user>"` (`router/tokens/websocket.go:30-48`), written by
  `POST /api/deauthorize-user {user, servers[]}` (`router/router_system.go:328-358`). For each listed server
  on the node the handler writes the deny entry, then closes that user's websockets (`CancelUser`) and
  SFTP sessions; **with `servers` empty it does the same for every server on the node.**
- **Legacy JTI denylist** via `POST /api/servers/:s/ws/deny {jtis[]}` (`router_server.go:335-349`).
  The Panel sends `md5(...)` (`DaemonServerRepository.php:192`) while it signs `jti = sha256(...)`,
  so this path never matches.
- **One-time use** for upload tokens only: `TokenStore` (go-cache, 60 min TTL, keyed by `unique_id`;
  `router/tokens/token_store.go:15-41`). File and backup download tokens can be reused until they
  expire, so a browser can re-request or resume a download (`router/router_download.go`).
- `DaemonServerRepository::deauthorize` (`:201-207`) posts `{user, servers: [uuid]}` for one server,
  or `servers: []` for a whole node (`RevokeSftpAccessJob`), which takes the every-server branch.

---

## 2. Wings HTTP routes (`router/router.go`)

Global middleware (81-107): recovery, trusted proxies, request id, error capture, CORS (panel URL plus
`allowed_origins`), server manager, API client.

HTTP server (`router.NewServer`, 22-60): 10 s to send the request headers, 2 min idle keep-alive. Until a
request authenticates it has 30 s to send its body and read the response; the deadline is lifted by
`RequireAuthorization` and by the download, upload and transfer handlers once their JWT is valid
(`middleware.ClearDeadlines`, `router/middleware/middleware.go:226-230`), and by the websocket upgrade.

Legend for **Scope**: **N** = node-scoped; **S** = server UUID in path; **T** = server UUID only inside the token.

### 2.1 Public (JWT-authenticated) routes

| Method | Path | Line | Scope | Auth |
|---|---|---|---|---|
| GET, HEAD | `/download/backup` | 110-111 | T | `?token=` backup-download JWT, reusable until it expires; `Range` supported. The server does not have to be on the node: the archive is looked up as `<backup_dir>/<server_uuid>/<backup_uuid>.tar.gz` (`404` when missing or for a non-canonical `server_uuid`) and served as `application/gzip` (`router/router_download.go:27-87`) |
| GET, HEAD | `/download/file` | 112-113 | T | `?token=` file-download JWT, reusable until it expires; `Range` supported |
| POST | `/upload/file` | 114 | T | `?token=` file-upload JWT, one-time; `?directory=` |
| GET | `/api/servers/:server/ws` | 119 | S | `ServerExists`; JWT arrives in the first `auth` message, within 30 s |
| POST | `/api/transfers` | 124 | T | `Authorization: Bearer <transfer JWT>`, UUID in `sub`. One request per server at a time (`409` otherwise); `409` for a server the node already has, or for an existing data directory unless the source shares a storage pool (`X-Storage-Pool`). A failed transfer removes the server, its container and data directory, and the backups and install log it received |

### 2.2 Node-token routes

| Method | Path | Line | Scope | Panel caller |
|---|---|---|---|---|
| POST | `/api/update` | 129 | N | `DaemonSystemRepository::update` (pushes `Node::getConfiguration()`, `DaemonSystemRepository.php:55-58`). Wings reads at most 1 MiB and applies only the keys of `config.PanelConfiguration` (`config/panel.go:27-117`), all validated first (`422` otherwise); keys missing or `null` keep their value (`router/router_system.go:248-326`) |
| GET | `/api/system` | 130 | N | `DaemonSystemRepository` (3 s timeout); the UI reads `version`/`exception` (`NodeSystemInformation.php:24-25`) |
| GET | `/api/diagnostics` | 131 | N | `DaemonSystemRepository` |
| GET | `/api/system/docker/disk` | 132 | N | no caller found |
| DELETE | `/api/system/docker/image/prune` | 133 | N | `PruneImagesCommand.php:38-55` (expects `ImagesDeleted`, `SpaceReclaimed`) |
| GET | `/api/system/ips` | 134 | N | `Node.php:683` (`ip_addresses`) |
| GET | `/api/system/utilization` | 135 | N | `Node.php:660-664` (`memory_total`, …) |
| GET | `/api/servers` | 136 | N | `DaemonServerRepository::getStates` (5 s; the node's mass-action table) reads `configuration.uuid` and `state` of each entry |
| POST | `/api/servers` | 137 | N (UUID in body) | `DaemonServerRepository::create {uuid, start_on_completion}`. `409` when the node already has the UUID (`router/router_system.go:171-177,194-200`). On a connection error or any 4xx/5xx the Panel force-deletes the server it just stored (`ServerCreationService.php:128-138`) |
| DELETE | `/api/backups/:server/:backup` | 140 | N (UUIDs in path; the server need not exist) | `DaemonBackupRepository::deleteKept` (§2.4). Deletes `<backup_dir>/<server>/<backup>.tar.gz`: `204`, `404` when the node does not have it, `400` for a non-canonical UUID (`router/router_server_backup.go:278-307`) |
| DELETE | `/api/transfers/:server` | 141 | S | `DaemonServerRepository::cancelTransfer` on the destination node; the response is ignored. Wings mounts it without `ServerExists`, so `ExtractServer` panics and it answers 500 |
| POST | `/api/deauthorize-user` | 142 | N | `RevokeSftpAccessJob` |

### 2.3 Server routes (`/api/servers/:server`, node token + `ServerExists`, 146-194)

| Group | Routes |
|---|---|
| Core | `GET ""` (Panel: `retrieveStatus`/`retrieveResources`, 1 s, see §3; `getDetails`, used by the client resource API and schedules, takes any failure as `state: "missing"`; batch transfers use `getState`, 5 s, which takes only a `404` as `missing` and any other failure as unknown), `DELETE ""` (`?keep_backups=1`, §2.4), `GET /logs`, `GET /install-logs`, `POST /power {action}`, `POST /commands`, `POST /install`, `POST /reinstall` (409 if a power action is running), `POST /sync`, `POST /ws/deny`, `POST /transfer` (409 while the server transfers, installs or restores), `DELETE /transfer`, `DELETE /deleteAllBackups` |
| Files | `GET /files/contents`, `GET /files/list-directory`, `PUT /files/rename`, `POST /files/{copy,write,create-directory,delete,compress,decompress,chmod}`, `GET /files/search`, `GET/POST /files/pull`, `DELETE /files/pull/:download`. `POST /files/pull` refuses internal addresses unless listed in `api.remote_download.allowlist` and accepts responses without `Content-Length` (`router/downloader/downloader.go`) |
| Backups | `POST /backup {adapter, uuid, ignore, pelican_config}` (400 when `ignore` is not a valid ignore list, or `pelican_config` is neither `null` nor a JSON object of at most 64 KiB), `POST /backup/:backup/restore {adapter, truncate_directory, download_url, source_server}`, `DELETE /backup/:backup` (§2.4) |

Panel timeouts worth noting: compress and decompress 15 min, search 2 min (`DaemonFileRepository`).

*[inferred]* The Panel also calls `POST /api/servers/{uuid}/archive`, which has no matching Wings route
(legacy or broken). Cancelling a transfer (`DaemonServerRepository::cancelTransfer`) calls
`DELETE /api/servers/{uuid}/transfer` on the source node and `DELETE /api/transfers/{uuid}` on the
destination, and ignores both responses.

Starting a transfer (`TransferServerService::notify`, `:37-58`) posts `POST /api/servers/{uuid}/transfer`
to the source node with a 30 s timeout and throws on any non-2xx answer. An answer of any status, or a
connection that was never made, marks the transfer failed and releases the new allocations
(`ServerTransfer::markFailed`); a connection that broke after the request was sent leaves the transfer
open (`TransferUnconfirmedException`), since the daemon may be running it. A successful transfer moves
the `wings` backups it sent to the new node (`backups.node_id`) and deletes the rows of the server's
other `wings` backups held by the old node (`ServerTransferController.php:91-105`).

### 2.4 Backups and restores

Wings: `router/router_server_backup.go`, `server/backup.go`. Panel: `DaemonBackupRepository.php`,
`ServerRestoreService.php`.

- **Create** `POST /backup` (`:45-106`) answers `202` and builds the archive in the background. A
  non-null `pelican_config` is stored compacted as the first archive entry: a regular file
  `.pelicanconfig` (mode 0600, USTAR) whose tar user and group names are both `pelican:prelude`
  (`server/filesystem/archive.go:71-93,443-461`); a file of that name in the server directory is still
  archived after it. The Panel sends it (`DaemonBackupRepository.php:16-26`) unless *include config* was
  turned off (`InitiateBackupService.php:156`) and does not limit its size, so a configuration over
  64 KiB fails the backup request. Shape (`ServerConfigExporterService.php:24-84`):
  `{_comment, meta{version: "PLCN_SRV_v1"}, exported_at, server{uuid, name, description, external_id},
  egg{uuid, name, update_url}, image, startup, limits{memory, swap, disk, io, cpu, threads, oom_killer},
  feature_limits{databases, allocations, backups}, allocations{default, additional[]}, variables{ENV: value},
  docker_labels, mounts[<mount uuid>]}`, allocations as `{ip, ip_alias, port, notes}`, user-viewable
  variables only. For S3 the Panel itself stores it as `<server_uuid>/<backup_uuid>.pelicanconfig.json`
  next to the archive `<server_uuid>/<backup_uuid>.tar.gz` once the backup is reported successful
  (`BackupStatusController.php:78-87`, `S3BackupSchema.php:59-70,98-110`).
- **Restore** `POST /backup/:backup/restore` (`:119-276`):
  - `adapter` is required (`wings` or `s3`); `download_url` is required for `s3`. `source_server`
    (canonical UUID, else `400`) is the server the backup was made of, default the path server.
  - `wings`: the archive is read from `<backup_dir>/<source_server>/<backup>.tar.gz` when the node has it,
    else downloaded from `download_url`; `404` when it has neither.
  - Download: http(s), at most 10 redirects, 30 s to connect and to the response headers, every resolved
    address checked. Internal addresses are refused (`400`) unless listed in
    `system.backups.restore_host_allowlist`; with adapter `wings` private ranges (RFC 1918, ULA,
    `100.64.0.0/10`) are allowed as well, loopback and link-local never (`:433-457`). A status other than
    200 is `400`. Accepted content types are `application/gzip` and `application/x-gzip`, or an empty,
    `application/octet-stream` or `binary/octet-stream` type whose body starts with the gzip magic within
    30 s (`:459-519`).
  - One lifecycle operation at a time: `409` while the server restores, installs or transfers
    (`StartRestoring`, `server/install.go:190-211`, which also closes its SFTP sessions). Then `202`.
  - In the background (`server/backup.go:150-276`): suspend, stop (2 min, no kill). With
    `truncate_directory` the whole archive is read once first (a download is saved to a temporary file in
    `system.backup_directory`) and the server's files are deleted only if that succeeds (`:287-340`).
    The `.pelicanconfig` entry is skipped. Unsuspend, clear the restoring state, then
    `POST /api/remote/backups/<:backup>/restore {successful}` with the identifier from the path.
- **Delete** `DELETE /backup/:backup` (`:337-362`) removes the path server's local backup, `404` when
  missing. `DELETE /api/backups/:server/:backup` does the same for a server that is not on the node.

Panel callers:

| Wings call | Panel caller | Notes |
|---|---|---|
| `POST /api/servers/{s}/backup` | `DaemonBackupRepository::create` (`:16-26`) | `{adapter, uuid, ignore, pelican_config}` |
| `POST /api/servers/{s}/backup/{b}/restore` (in place) | `DaemonBackupRepository::restore` (`:33-42`) from `Api/Client/Servers/BackupController.php:245-274` | `{adapter, truncate_directory, download_url}`; `download_url` is the user's download link, also for `wings`. Refused (`400`) while a new-server restore reporting under this backup's uuid runs on the node (`:257-271`) |
| `POST /api/servers/{new}/backup/{id}/restore` (new server) | `DaemonBackupRepository::restoreInto` (`:51-61`) from `ServerRestoreService::start` (`:69-114`) | `{adapter, truncate_directory: true, source_server?, download_url?}`, nulls left out; see below |
| `DELETE /api/servers/{s}/backup/{b}` | `WingsBackupSchema::deleteBackup` (`:32-61`) | when the backup still has its server; on a `404` for a backup with a `server_uuid` it continues with `deleteKept` |
| `DELETE /api/backups/{server_uuid}/{b}` | `DaemonBackupRepository::deleteKept` (`:68-71`) to `Backup::sourceNode()` (`WingsBackupSchema.php:48-53`) | backup without a server, or after that `404`; a `404` is ignored (`:54-60`) |
| `DELETE /api/servers/{s}?keep_backups=1` | `DaemonServerRepository::delete(true)` (`:129-132`) | deletion with kept backups, below |
| `GET <node>/download/backup?token=` | `BackupConfigResolver` (`:55-81,128-153`) | a `wings` backup the Panel has no `pelican_config` for: restore link with 5 min TTL, plain `Http` (no bearer token, no User-Agent check, TLS verified), 10 s timeout, 5 s connect, at most 3 redirects, reads ≤ 256 KiB within 15 s. Uses the first tar entry if it is `.pelicanconfig` or `./.pelicanconfig` with typeflag `0`/NUL, ≤ 64 KiB, JSON (`:301-349`) |
| `POST /api/servers/{s}/power {start}` | `ServerRestoreService::complete` (`:158-164`) | after a successful new-server restore with *start on completion*, unless suspended |

**Restore into a new server** (admin panel, server create permission; `CreateServerFromBackupAction.php`):

1. The form is prefilled from the source's configuration (`CreateServer.php:150-204`); the node defaults to
   the backup's node (`:455-463`). For the daemon `skip_scripts` is forced on and `start_on_completion`
   off (`:1205-1211`); an administrator's URL must resolve to public addresses only (`:1226-1228`).
2. A `server_restores` row (`pending`) is written in the transaction that creates the server, before
   `POST /api/servers` (`ServerCreationService.php:111-126`, `beforeDaemon`).
3. Wings skips the install script and reports `POST /api/remote/servers/{uuid}/install`; the queued
   `RestorePendingBackupListener` (`:18-38`) starts the restore, or fails it when the install failed.
4. `start` claims the row (`pending` → `running`), stores the identifier as `callback_uuid` under the cache
   lock `backup-restore:{backup uuid}` (`:265-272`), sets the server `RestoringBackup` unless it is
   suspended (`:95-98`) and posts `restoreInto`. Any 2xx is accepted; a connection error or a 4xx/5xx
   fails the restore with the response's JSON `error` (`:107-113`).
5. `payload()` (`:208-251`):

| Source | `{id}` in the path | `adapter` | `source_server` | `download_url` |
|---|---|---|---|---|
| `wings` backup held by the new server's node | backup uuid | `wings` | backup's server uuid | — |
| `wings` backup held by another node | backup uuid | `wings` | backup's server uuid | restore link to that node (60 min) |
| `wings` backup already restoring under its own uuid on the new server's node (`backupInUse`, `:302-308`) | `server_restores.uuid` | `s3` | — | restore link (60 min) |
| S3 backup | `server_restores.uuid` | `s3` | — | presigned GET of `<server_uuid>/<backup_uuid>.tar.gz` (60 min) |
| Administrator's S3 key | `server_restores.uuid` | `s3` | — | presigned GET of that key (60 min) |
| Administrator's URL | `server_restores.uuid` | `s3` | — | the URL |

   With adapter `s3`, Wings refuses a restore link to a node on a private address unless it is in
   `system.backups.restore_host_allowlist`.
6. The result `POST /api/remote/backups/{id}/restore` first matches a running `server_restores` row with
   that `callback_uuid` whose server is on the reporting node (`ServerRestoreService.php:120-127`,
   `BackupStatusController.php:104-115`); otherwise it is an in-place restore, which needs the backup's
   server on the reporting node (`403` otherwise, also for a backup without a server; `:117-129`).
   `POST /api/remote/servers/reset` fails running restores on the node and pending ones whose server is
   still installing (`ServerDetailsController.php:130-139`).

How pelican-k8s serves a restore from another server's local backup: [`ARCHITECTURE.md`](../ARCHITECTURE.md) §8.7.

**Deletion with kept backups:** admin delete and force delete offer *keep backups* when the server has
restorable backups (`EditServer.php:83-90,114-121`). `ServerDeletionService::handle` (`:61-135`) calls
`DELETE /api/servers/{uuid}` (with `?keep_backups=1` when keeping) and then, per backup:

| Backup | Kept | Panel action |
|---|---|---|
| successful | yes | `server_id` set to null; the row (`server_uuid`, `node_id`, `pelican_config`) and the archive or S3 object stay |
| unsuccessful, `wings` | yes | `deleteBackup` (its server call `404`s and continues with `deleteKept`), row deleted |
| `wings` | no | row deleted, no daemon call (Wings removed the local backups with the server) |
| other adapters | no, or unsuccessful | `deleteBackup` (S3: the archive and `.pelicanconfig.json`, `S3BackupSchema.php:77-92`), row deleted |

Wings keeps a server's local backups on `DELETE` when `?keep_backups` is true or
`system.backups.remove_backups_on_server_delete` (default `true`) is off (`router_server.go:256-264`).
A kept backup is deleted later from the admin Backups page or the node's Backups tab
(`Admin/Resources/Backups/BackupResource.php:176-193`, `Nodes/RelationManagers/BackupsRelationManager.php:118-130`)
through `deleteKept`.

---

## 3. Panel remote API (`routes/api-remote.php`, prefix `/api/remote`, middleware `daemon`)

Paths below are the effective ones. Upstream registers most of them through
`Route::prefix('/servers/{server:uuid}')->group(...)` and `Route::prefix('/backups')`,
so no single source line spells a full path out; `hack/check-panel-contract.sh`
resolves the groups and diffs the result against this table.

| Method | Path | Controller | Used for |
|---|---|---|---|
| POST | `/sftp/auth` | `SftpAuthenticationController` | SFTP login |
| GET | `/servers?page&per_page` | `ServerDetailsController@list` | boot sync (paged by node) |
| POST | `/servers/reset` | `@resetState` | once per boot: clears `Installing`/`RestoringBackup` for **all** node servers, fails the node's running new-server restores and pending ones whose server is still installing, and marks every unfinished transfer to or from the node failed |
| POST | `/activity` | `ActivityProcessingController` | activity batches `{data:[…]}` |
| GET | `/servers/{uuid}` | `ServerDetailsController` | `{settings, process_configuration}` |
| GET | `/servers/{uuid}/install` | `ServerInstallController@index` | `{container_image, entrypoint, script}` |
| POST | `/servers/{uuid}/install` | `@store` | `{successful, reinstall}` |
| POST | `/servers/{uuid}/transfer/{failure,success}` | `ServerTransferController` | transfers |
| POST | `/servers/{uuid}/container/status` | `ServerContainersController@status` | `{data:{previous_state,new_state}}`, cached 1 h |
| GET | `/backups/{uuid}?size=` | `BackupRemoteUploadController` | S3 presigned part URLs `{parts[], part_size}` |
| POST | `/backups/{uuid}` | `BackupStatusController@index` | `{checksum, checksum_type, size, successful, parts[]}` |
| POST | `/backups/{uuid}/restore` | `@restore` | `{successful}`; `{uuid}` is a backup uuid or a `server_restores` uuid (§2.4) |

*[inferred]* Wings' `SetArchiveStatus` posts `/servers/{uuid}/archive`, which has no Panel route (dead code).

State reads: `Server::retrieveStatus()` (`Server.php:539-565`) answers `missing` without asking while the
node is under maintenance, and otherwise caches `DaemonServerRepository::getState()`
(`DaemonServerRepository.php:68-79`: `GET /api/servers/{uuid}`, 1 s connect and 1 s total) for 15 s.
A `404` is `missing`. A connection error, a User-Agent mismatch, any other 4xx/5xx, or a 200 without a
valid `state` also answers `missing` and marks the whole node unreachable for 15 s (`Node.php:493-501`);
while it is marked, servers on that node without a cached status are `missing` without a request.
`retrieveResources()` (`Server.php:570-595`) does the same through `getDetailsIfAnswered()`
(`DaemonServerRepository.php:32-62`) and reads `utilization`. A pushed `container/status` value wins for
up to 1 h (`ServerContainersController.php:17-26`).

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

Wings refuses a configuration whose `uuid` is not a UUID, or is not the server it asked for
(`server/server.go:326-331`, `server/manager.go:214-216`).

Servers **without an allocation** (allowed since 2025-06) arrive as
`"default": {"ip": "127.0.0.1", "port": 0}` and `"mappings": {"": []}`. `container.image` may carry a
leading `~`, which Wings strips and interprets as "never pull".

**`process_configuration`** (`EggConfigurationService.php:28-83`):
`startup{done: string[], user_interaction: [], strip_ansi}`, `stop{type: "command"|"signal", value}`
(an egg stop of `^C` becomes a signal `C`, which Wings maps to SIGINT), and
`configs[{file, parser, replace[{match, if_value?, replace_with}]}]`.

Environment given to the process (`server/server.go:241-263`): `TZ`, `STARTUP` (invocation with
`{{VAR}}` substituted), `SERVER_MEMORY`, `SERVER_IP`, `SERVER_PORT`, plus uppercased egg variables.
**Wings never executes `STARTUP` itself:** the yolk image entrypoint reads and `eval`s it.
If the default allocation IP is `127.0.0.1` and the port is non-zero, `SERVER_IP` is rewritten to
`docker.network.interface` (the bridge gateway, `172.18.0.1`; `environment/docker/container.go:309-318`).
Port bindings (`environment/allocations.go:86-138`) publish `127.0.0.1` allocations on that interface
instead; with `docker.network.ispn` they are not published at all. The same interface address is what
Wings' parser substitutes for the `{{config.docker.interface}}` placeholder in `replace_with` values;
the Panel pre-expands every other placeholder (`{{server.build.default.port}}`, `{{env.X}}`).

---

## 5. SFTP

- Username format `^(?i)(.+)\.([a-z0-9]{8})$` → `<panel username>.<uuid_short>` (`sftp/server.go:34`).
  Panel connection string: `sftp://<user>.<uuid_short>@<daemon_sftp_alias ?? fqdn>:<daemon_sftp>`
  (`app/Models/Server.php:658-660`).
- Auth request `POST /api/remote/sftp/auth` with body
  `{type: password|public_key, username, password|authorized_key, ip, session_id, client_version}`
  (`remote/types.go:75-82`).
- Panel (`SftpAuthenticationController.php`):
  - splits the username on the last `.` and throttles per `server|ip`
  - requires the server to be on the authenticating node
  - checks the password, or the public key by sha256 fingerprint
  - requires owner, admin, or subuser permission `file.sftp`, and validates server state
  - responds `{user: <uuid>, server: <uuid>, permissions: [...]}`
- Wings stores these in `ssh.Permissions.Extensions` (`sftp/server.go:513-522`). The file handler checks
  `file.read`, `file.read-content`, `file.create`, `file.update` and `file.delete` (`sftp/handler.go:22-26`),
  and `sftp.read_only` applies globally.
- Host key: ED25519 at `<system.data>/.sftp/id_ed25519`, generated if missing. Algorithms are pinned
  (`sftp/server.go:210-225`); `MaxAuthTries` is 6.
- Connection limits (`sftp/server.go:36-65`): 2 min to finish the handshake and authenticate; at most
  128 connections authenticating at once, 16 per IP address or IPv6 /64
  (`system.sftp.max_authenticating_connections_per_ip`); 32 authenticated connections per user and server
  (`system.sftp.max_connections_per_user`); 10 session channels per connection; 64 open files per session
  (`sftp/handler.go:42-43`). A session channel for a server that is not on the node is rejected.
- Sessions are tracked per user (`srv.Sftp().Context(user)`) and cancelled on install, transfer, restore,
  suspend and deauthorize.

---

## 6. Websocket protocol

- URL given to the browser: `ws(s)://<fqdn>:<daemon_connect>/api/servers/<uuid>/ws` (`WebsocketController.php:66-71`).
- Limits: 30 authenticated connections per server (the upgrade is refused with `400` when full; a
  connection counts once it authenticates and is closed if it does not within 30 s,
  `router/router_server_ws.go:26-33,60,96-109`); Origin must be the panel URL or one of
  `allowed_origins`; read limit 4096 bytes; a write may take 10 s; compression on; a suspended server
  gets close code **4409**. Inbound rate limiting
  (`router/websocket/limiter.go`, event `throttled`): a global limiter of 1 message per 200 ms with a
  burst of 10, plus per-event limiters (`auth` and `send logs` 1 per 5 s, burst 2; `send command`
  1 per second, burst 10; everything else 1 per second, burst 4).
- Frame: `{"event": string, "args": [string]}`.
- **Inbound events:**
  - `auth [jwt]` → `auth success`. The token must be for the path's server. The first auth registers
    listeners, sends `status`, and sends `stats` if offline.
  - `set state [start|stop|restart|kill]`, gated by `control.*` permissions.
  - `send logs` → `Readlog(websocket_log_count=150)` as `console output`.
  - `send stats`
  - `send command [cmd]`: needs `control.console`.
  - Every non-auth message re-validates the token (`jwt error` on failure), and that token is used for all
    permission checks of the message.
- **Outbound events:** `auth success`, `token expiring`, `token expired` (30 s ticker, warning at ≤ 60 s),
  `daemon error`, `jwt error`, `throttled`, `console output`, `status`, `stats`, `install output`
  (needs `admin.websocket.install`), `install started`, `install completed`, `daemon message`,
  `backup completed:<uuid>` (needs `backup.read`), `backup restore completed`, `transfer logs`
  (`admin.websocket.transfer`), `transfer status`, `deleted`, `feature match`.

Stats JSON: `{memory_bytes, memory_limit_bytes, cpu_absolute, network{rx_bytes,tx_bytes}, disk_io{read_bytes,write_bytes}, uptime, state, disk_bytes}`.

---

## 7. Server lifecycle inside Wings

- **Boot** (`cmd/root.go:119-471`):
  1. directories → pelican user → passwd files → logrotate
  2. Panel client → SQLite activity DB → quotas
  3. `server.NewManager` (paged `GET /servers`) → **`environment.ConfigureDocker`** → read `states.json`
  4. per server: ensure data directory → `IsRunning` → reattach, or auto-start if the last state was running.
     `states.json` (`<root_directory>/states.json`) is written periodically by `Manager.PersistStates`
     with each server's **actual** environment state, so a server stopped from the console is not
     restarted after a reboot
  5. cron → SFTP → `POST /servers/reset` → HTTP server
- **Manager** `InitServer(uuid, …)` (`server/manager.go:256-330`) refuses a configuration for another
  server, creates `<data>/<uuid>`, opens it as the server's filesystem (an `os.Root`, held open until the
  server is removed) and builds the environment through the manager's **`EnvironmentFactory`** (311-321):
  the one set with `WithEnvironmentFactory`, or `DockerEnvironmentFactory` when none is set. A server
  listed twice by the Panel is loaded once.
- **Power** `HandlePowerAction` (`server/power.go:70-200`): refused while installing, transferring or
  restoring; takes a power lock. Start: `onBeforeStart` (Sync, suspend check, `SyncWithEnvironment`, disk
  check, `UpdateConfigurationFiles`, chown, machine-id) → `Env.Start`. Stop/restart: `WaitForStop(10 min, terminate)`.
  Kill: `Terminate(SIGKILL)`; while a start or restart still holds the power lock with the server offline
  or starting, a kill waits up to 30 s for that boot to finish before it terminates without the lock.
- **Done detection:** while `starting`, a console line matching `startup.done` (substring or `regex:`)
  switches to `running` (`server/listeners.go:155-194`). Console output never sets `stopping` or
  `offline`; sending a command equal to a `command`-type stop value sets `stopping` (§7.2).
- **State push:** `OnStateChange` always posts `container/status` (`server/server.go:428-478`) and
  triggers crash detection on starting/running → offline.
- **Crash handling** `handleServerCrash` (`server/crash.go:49-106`): runs only on a `starting`/`running`
  → `offline` transition, so a `stopping` → `offline` transition (power stop, stop command typed into the
  console, suspension) never counts as a crash. Reads `ExitState()` (exit code, OOM); with
  `detect_clean_exit_as_crash=false` a clean exit is not a crash (the default is `true`); records the
  last console lines as activity; restarts unless the previous crash was within
  `crash_detection.timeout` (60 s).
- **Sync** (`server/server.go:275-307`, `update.go:21-68`): re-fetch config, update disk limit, then
  `SyncWithEnvironment`. **This type-asserts `*docker.Environment` to push the image and stop config**
  (update.go:36-40). If the server is suspended, it stops the server and cancels websockets and SFTP.
- **Install** (`server/install.go`):
  1. `GET /servers/{uuid}/install`, write `<tmp>/<uuid>/install.sh`
  2. container `<uuid>_installer` running `[entrypoint, /mnt/install/install.sh]`, with the server directory
     at `/mnt/server` and the script directory read-only at `/mnt/install`, **no User set (root)**,
     limits = max(server, installer_limits)
  3. stream output to `install output`, write `<log_dir>/install/<uuid>.log`
  4. `POST /servers/{uuid}/install {successful, reinstall}`

  A container-wait error or a non-zero exit code of the script marks the install as failed
  (`server/install.go:605-617`). An install requested while the server transfers or restores does not
  run and is reported unsuccessful (`:46-57`). With `skip_egg_scripts` nothing runs and success is
  reported right away.
- **Backups:**
  - local adapter: `<backup_dir>/<server>/<uuid>.tar.gz`
  - S3 adapter: generate locally, `GET /backups/{uuid}?size=` for presigned part URLs, PUT the parts, then
    `POST /backups/{uuid}` with ETags
  - `.pelicanconfig`, restore and deletion: §2.4
- **Activity:** SQLite `<root>/wings.db`; cron every 60 s sends ≤ 100 rows to `POST /activity`
  (SFTP events aggregated per minute).

### 7.1 Docker coupling outside `environment/docker`

| Location | Effect | Needed in agent mode? |
|---|---|---|
| `cmd/root.go:102-130` | Docker `Info`, `log.Fatalf` if unreachable | remove |
| `cmd/root.go:148` `EnsurePelicanUser` | `useradd` | skip (rootless/distroless path) |
| `cmd/root.go:152,160` passwd, logrotate | host files | disable via config |
| `server/manager.go:311-321` | `DockerEnvironmentFactory` unless `WithEnvironmentFactory` sets another | agent sets the shim environment factory |
| `cmd/root.go:197` `ConfigureDocker` | network setup (fatal) | skip |
| `system/system.go:103-171,373-392` | `/api/system` fails without Docker | gateway answers |
| `system/system.go:332-371` | docker disk, prune | gateway stubs |
| `server/install.go:125-162` | installer container (`dockerInstaller`) unless `WithInstaller` sets another `Installer` | agent sets the Job-backed installer |
| `server/update.go:34` | image and stop config through `ImageAndStopConfigurable` | shim environment implements it |
| `router/websocket/websocket.go:533-534` | `IsAttached` gate through `Attachable` | n/a for non-docker env |
| `server/power.go:255-262` | chown to uid 988 | `check_permissions_on_boot: false` |
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
| `SendCommand` | refuses control characters other than tab, CR and LF; a command equal to a `command`-type stop value, or with a line equal to it, sets `stopping`; writes `<cmd>\n`, at most 10 s (`environment/docker/container.go:473-530`) |
| `Terminate` | poll 10 s, then SIGKILL |
| `Readlog(n)` | tail of the per-run log |
| `ExitState` | exit code plus OOM-killed flag |
| Resources | stats stream while attached |

---

## 8. Wings `config.yml` keys

- Top level: `token_id`, `token` (also `WINGS_TOKEN_ID`/`WINGS_TOKEN` env or `file://`), `remote`,
  `ignore_panel_config_updates`, `allowed_origins`, `allowed_mounts`.
- `api`: `host`, `port` (8080), `ssl`, `upload_limit` (100 MiB), `trusted_proxies`,
  `remote_download{max_redirects 10, allowlist}`, `panel_allowlist`.
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
  | `sftp{bind_port 2022, read_only, key_only, max_connections_per_user, max_authenticating_connections_per_ip}` | —, 32 and 16 when 0 |
  | `crash_detection{enabled, detect_clean_exit_as_crash, timeout}` | `true`, `true`, 60 |
  | `backups{write_limit, compression_level, restore_host_allowlist, remove_backups_on_server_delete}` | `remove_backups_on_server_delete` `true` |
  | `activity_send_interval` / `activity_send_count` | — |

- `throttles{enabled, lines 2000, line_reset_interval 100}`.
- `docker.*`: network, `tmpfs_size` 100, `container_pid_limit` 512, `installer_limits`, `overhead` multipliers.
- `api.panel_allowlist`, `api.remote_download.allowlist` and the `system.sftp` limits are read from
  config.yml only; `POST /api/update` cannot set them (§2.2).
- Panel-generated node config (`Node.php:350-384`) sets: `uuid`, `token_id`, `token`,
  `api{host, port=daemon_listen, ssl, upload_limit}`, `system{data, sftp.bind_port}`, `allowed_mounts`, `remote`,
  merged over the node's optional `daemon_config` overrides: the keys in `Node::DAEMON_CONFIG_KEYS`
  (`Node.php:137-175`; e.g. `debug`, `allowed_origins`, `api.trusted_proxies`, `api.ssl.cert`/`key`,
  `remote_query.timeout`, `system.*`, `docker.*`, `throttles.*`), each sent only when set.
  `system.username`, `system.user.rootless.enabled` and `docker.userns_mode` are not among them.
  `POST /api/update` sends the same array (`DaemonSystemRepository.php:57`); of it Wings applies only the
  `PanelConfiguration` keys, so `remote`, `system.data` and `allowed_mounts` change only in config.yml.
  `daemon_listen` (bind port) and `daemon_connect` (advertised port) are separate fields.

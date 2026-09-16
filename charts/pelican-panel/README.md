# pelican-panel

Helm chart for the **unmodified** [Pelican Panel](https://github.com/pelican/panel)
image `ghcr.io/pelican/panel`.

| | |
|---|---|
| Chart version | `0.1.0` |
| Default app version | `v1.0.0-beta38` |
| Kubernetes | `>= 1.22` (needs the `net.ipv4.ip_unprivileged_port_start` safe sysctl) |
| Helm | `>= 3.9` |

## What the image actually is

One container, started by `/entrypoint.sh` and supervised by `supervisord`:

| Process | Purpose |
|---|---|
| `caddy` | web server on `:80` (and `:443` when it terminates TLS itself) |
| `php-fpm` | the Laravel/Filament app on `127.0.0.1:9000` |
| `queue-worker` | `php artisan queue:work --tries=3 --max-time=3600` |
| `supercronic` | the Laravel scheduler (`artisan schedule:run` every minute) |

**There is no separate worker or scheduler Deployment** - the upstream image runs
both in the same pod. That is also why `replicaCount` must stay `1` unless you
move cache/session/queue to Redis and set `panel.skipMigrations=true` (the chart
refuses other combinations).

Before `supervisord` starts, the entrypoint:

1. creates `/pelican-data/.env` if missing (writing `APP_KEY` into it),
2. waits for `DB_HOST:DB_PORT` with `nc` when `APP_INSTALLED=true` and the
   connection is not `sqlite`,
3. runs `php artisan migrate --force` (unless `SKIP_MIGRATIONS=true`) and
   `php artisan p:plugin:composer`,
4. runs `php artisan filament:optimize` and `php artisan view:cache`,
5. builds the Caddy configuration from `APP_URL`, `BEHIND_PROXY`, `LE_EMAIL`,
   `SKIP_CADDY` and `TRUSTED_PROXIES`.

Health endpoint: **`GET /up`** (Laravel's health route, no auth, returns 200).

## Quick start

```bash
# SQLite in the PVC, port-forward, no ingress
helm install panel ./charts/pelican-panel -n pelican --create-namespace \
  --set panel.url=http://localhost:8080

kubectl port-forward -n pelican svc/panel-pelican-panel 8080:80
kubectl exec -n pelican deploy/panel-pelican-panel -- \
  php artisan p:user:make --admin=1 --email=you@example.com --username=admin --password='<pw>'
```

With the bundled MariaDB + Redis and an Ingress, see
[`ci/mariadb-redis-values.yaml`](ci/mariadb-redis-values.yaml).
On OpenShift with CloudNativePG and a Route, see
[`ci/openshift-cnpg-values.yaml`](ci/openshift-cnpg-values.yaml) and
[`docs/panel.md`](../../docs/panel.md).

## Secrets, and why GitOps needs `existingSecret`

`APP_KEY` encrypts everything reversible in the Panel (2FA secrets, node daemon
tokens, OAuth secrets) and signs all sessions. Rotating it locks you out of them.

The chart resolves `APP_KEY` in this order:

1. `panel.existingSecret` + `panel.existingSecretAppKeyKey`,
2. `panel.appKey` (verbatim, stored in the chart's own Secret),
3. generated - reusing the value already in the chart's Secret via Helm's
   `lookup` function.

**Argo CD (and `helm template`) cannot run `lookup`**, so option 3 produces a
*new* key on every render. For GitOps you must pre-create the Secret and set
`panel.existingSecret`:

```bash
kubectl create secret generic pelican-panel-secrets -n pelican \
  --from-literal=APP_KEY="base64:$(head -c 32 /dev/urandom | base64 -w0)"
```

The same applies to the bundled MariaDB passwords (`mariadb.auth.existingSecret`).

## Database

`database.connection` accepts `pgsql`, `mysql`, `mariadb` and `sqlite` - all four
are offered by the upstream installer and all four ship a Laravel connection in
`config/database.php`. **PostgreSQL works**: migrations, the queue, the scheduler
and the Filament UI were verified against PostgreSQL 18 (CloudNativePG).

Host/port/database/username can each be read from an existing Secret, which is
what makes the CloudNativePG `<cluster>-app` Secret a drop-in:

```yaml
database:
  connection: pgsql
  existingSecret: pelican-db-app
  existingSecretPasswordKey: password
  existingSecretUsernameKey: username
  existingSecretDatabaseKey: dbname
  existingSecretHostKey: host
  existingSecretPortKey: port
```

`mariadb.enabled=true` deploys a single-replica MariaDB StatefulSet and overrides
`database.connection/host/port/name/username`. It is a convenience for quick
starts, **not** a production database, and on OpenShift it needs the `anyuid` SCC
(the official MariaDB image starts as root and drops to `mysql` itself).

## Cache / session / queue

| Value | Accepted | Default | Notes |
|---|---|---|---|
| `panel.cacheStore` | `file`, `redis` | `file` | `database` is rejected by the chart - the Panel has no `cache` table migration |
| `panel.sessionDriver` | `database`, `file`, `cookie`, `redis` | `database` | `database` keeps logins across pod restarts |
| `panel.queueConnection` | `database`, `redis`, `sync` | `database` | the `jobs` table is migrated by the Panel |

`redis.enabled=true` deploys a tiny Redis (no persistence, `--save ""`); otherwise
point `externalRedis.host` at your own. The Panel talks to Redis through
`predis` (pure PHP), so no PHP extension is needed.

## Behind a proxy (Ingress / OpenShift Router)

Set all three:

```yaml
panel:
  url: https://panel.example.com     # APP_URL - scheme and host must match exactly
  behindProxy: true                  # BEHIND_PROXY - Caddy listens on plain :80
  trustedProxies: "0.0.0.0/0,::/0"   # TRUSTED_PROXIES
```

`panel.trustedProxies` is consumed twice: by Laravel's `trustedproxy` config and
by Caddy's `trusted_proxies static ...`. Laravel accepts `*`, **Caddy does not** -
it fails with `invalid IP address: '*'` and the pod never serves anything. The
chart therefore rejects `*`/`**` unless `panel.skipCaddy=true`. Use
`0.0.0.0/0,::/0`, or the real ingress pod CIDR if you want to be strict.

Without trusted proxies Laravel does not see `X-Forwarded-Proto: https`, so the
login form posts to `http://` and the session cookie loses its `Secure` flag.

## OpenShift

* The image has a fixed `USER www-data` (uid/gid **82**), which the default
  `restricted-v2` SCC forbids. Enable `openshift.scc.enabled=true` (binds
  `nonroot-v2` to the chart's ServiceAccount) and keep
  `podSecurityContext.runAsUser/runAsGroup/fsGroup: 82`.
* Caddy binds port 80 as a non-root user. Docker silently sets
  `net.ipv4.ip_unprivileged_port_start=0`; Kubernetes does not, so the chart sets
  that (safe, namespaced) sysctl in `podSecurityContext`. Adding
  `NET_BIND_SERVICE` instead does **not** work: Kubernetes cannot set ambient
  capabilities, and `allowPrivilegeEscalation: false` disables file capabilities.
* Use `route.enabled=true` with `termination: edge`. The chart always points the
  Route at the Service's **named** port (`http`) - a numeric `targetPort` makes
  the OpenShift router answer 503.

## Values

### Top level

| Key | Type | Default | Description |
|---|---|---|---|
| `replicaCount` | int | `1` | Panel replicas. `>1` requires `panel.skipMigrations=true` and a shared cache/session store. |
| `image.repository` | string | `ghcr.io/pelican/panel` | Image repository. |
| `image.tag` | string | `""` | Image tag; falls back to `.Chart.AppVersion`. |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy. |
| `imagePullSecrets` | list | `[]` | Pull secrets for all pods of the release. |
| `nameOverride` | string | `""` | Override the chart name in resource names. |
| `fullnameOverride` | string | `""` | Override the full resource name prefix. |
| `commonLabels` | object | `{}` | Labels added to every object. |
| `commonAnnotations` | object | `{}` | Annotations added to every object. |
| `strategy` | object | `{type: Recreate}` | Deployment strategy. `Recreate` is required with an RWO PVC. |
| `terminationGracePeriodSeconds` | int | `120` | Time the queue worker gets to drain. |
| `podAnnotations` / `podLabels` | object | `{}` | Extra pod metadata. |
| `nodeSelector` / `tolerations` / `affinity` / `topologySpreadConstraints` | | `{}` / `[]` | Standard scheduling knobs for the Panel pod. |
| `priorityClassName` | string | `""` | PriorityClass for the Panel pod. |
| `resources` | object | `{}` | Requests/limits for the Panel container. |
| `extraVolumes` / `extraVolumeMounts` | list | `[]` | Extra volumes/mounts (rendered through `tpl`). |
| `initContainers` | list | `[]` | Extra init containers (rendered through `tpl`). |

### ServiceAccount

| Key | Type | Default | Description |
|---|---|---|---|
| `serviceAccount.create` | bool | `true` | Create a ServiceAccount. |
| `serviceAccount.name` | string | `""` | Name; generated from the release name when empty. |
| `serviceAccount.annotations` | object | `{}` | ServiceAccount annotations. |
| `serviceAccount.automountServiceAccountToken` | bool | `false` | The Panel never calls the API server. |

### Panel (`panel.*`) - environment the image really reads

| Key | Env var | Default | Description |
|---|---|---|---|
| `panel.appName` | `APP_NAME` | `Pelican` | Branding in the UI. |
| `panel.url` | `APP_URL` | `http://localhost` | External URL, scheme included. Must match how users reach it. |
| `panel.env` | `APP_ENV` | `production` | Laravel environment. |
| `panel.debug` | `APP_DEBUG` | `false` | Debug output. Never `true` in production. |
| `panel.locale` | `APP_LOCALE` | `en` | Default locale. |
| `panel.installed` | `APP_INSTALLED` | `true` | `true` = chart-managed install: migrate on every start, no web installer. `false` = use `/installer`. |
| `panel.skipMigrations` | `SKIP_MIGRATIONS` | `false` | Do not migrate on start. |
| `panel.behindProxy` | `BEHIND_PROXY` | `true` | Caddy listens on plain `:80`, auto-HTTPS off, `ASSET_URL=APP_URL`. |
| `panel.trustedProxies` | `TRUSTED_PROXIES` | `0.0.0.0/0,::/0` | Proxies to trust. Must be IPs/CIDRs (see above). |
| `panel.leEmail` | `LE_EMAIL` | `""` | Let's Encrypt contact; required when `url` is https **and** `behindProxy` is false. |
| `panel.skipCaddy` | `SKIP_CADDY` | `false` | Run PHP-FPM only and expose `:9000`; probes are disabled. |
| `panel.cacheStore` | `CACHE_STORE` | `file` | `file` or `redis`. |
| `panel.sessionDriver` | `SESSION_DRIVER` | `database` | `database`, `file`, `cookie` or `redis`. |
| `panel.queueConnection` | `QUEUE_CONNECTION` | `database` | `database`, `redis` or `sync`. |
| `panel.twoFactorRequired` | `APP_2FA_REQUIRED` | `""` | `0` optional, `1` admins, `2` everyone. |
| `panel.timezone` | `TZ` | `UTC` | Container timezone. The Panel itself is hardcoded to UTC in `config/app.php`; there is **no** `APP_TIMEZONE`. |
| `panel.mail.mailer` | `MAIL_MAILER` | `log` | `log`, `smtp`, `mailgun`, `postmark`, `ses`, `sendmail`, `array`. (`MAIL_DRIVER` in the upstream compose file is a legacy no-op.) |
| `panel.mail.host` | `MAIL_HOST` | `""` | SMTP host. |
| `panel.mail.port` | `MAIL_PORT` | `""` | SMTP port. |
| `panel.mail.username` | `MAIL_USERNAME` | `""` | SMTP user. |
| `panel.mail.password` | `MAIL_PASSWORD` | `""` | SMTP password (put it in `panel.existingSecret` instead). |
| `panel.mail.scheme` | `MAIL_SCHEME` | `""` | `smtp` or `smtps`. |
| `panel.mail.fromAddress` | `MAIL_FROM_ADDRESS` | `""` | Sender address. |
| `panel.mail.fromName` | `MAIL_FROM_NAME` | `""` | Sender name. |
| `panel.extraEnv` | | `{}` | Any other upstream variable, rendered into the ConfigMap. |
| `panel.extraEnvVars` | | `[]` | Raw `env:` entries (supports `valueFrom`), rendered through `tpl`. |
| `panel.extraEnvFrom` | | `[]` | Raw `envFrom:` entries. |
| `panel.existingSecret` | | `""` | Secret holding `APP_KEY` (**required for GitOps**). |
| `panel.existingSecretAppKeyKey` | | `APP_KEY` | Key inside that Secret. |
| `panel.existingSecretMailPasswordKey` | | `""` | Optional key holding `MAIL_PASSWORD`. |
| `panel.appKey` | `APP_KEY` | `""` | Explicit key (`base64:...`). Empty = generate + reuse via `lookup`. |

The chart also always sets `XDG_DATA_HOME=/pelican-data` (as the upstream compose
file does) so Caddy and Composer keep their state on the volume.

### Database (`database.*`)

| Key | Default | Description |
|---|---|---|
| `database.connection` | `sqlite` | `pgsql`, `mysql`, `mariadb`, `sqlite`. |
| `database.host` | `""` | `DB_HOST`. Required unless sqlite / bundled MariaDB / `existingSecretHostKey`. |
| `database.port` | `""` | `DB_PORT`. Defaults to 5432 (pgsql) or 3306. |
| `database.name` | `panel` | `DB_DATABASE`. |
| `database.username` | `pelican` | `DB_USERNAME`. |
| `database.password` | `""` | `DB_PASSWORD` in cleartext - prefer `existingSecret`. |
| `database.existingSecret` | `""` | Secret with the DB credentials. |
| `database.existingSecretPasswordKey` | `password` | Password key. |
| `database.existingSecretUsernameKey` | `""` | Username key (optional). |
| `database.existingSecretDatabaseKey` | `""` | Database-name key (optional). |
| `database.existingSecretHostKey` | `""` | Host key (optional). |
| `database.existingSecretPortKey` | `""` | Port key (optional). |

### Bundled MariaDB (`mariadb.*`)

| Key | Default | Description |
|---|---|---|
| `mariadb.enabled` | `false` | Deploy a MariaDB StatefulSet and use it. |
| `mariadb.image.repository` / `.tag` / `.pullPolicy` | `mariadb` / `11.4` / `IfNotPresent` | Image. |
| `mariadb.auth.database` | `panel` | Database created on first start. |
| `mariadb.auth.username` | `pelican` | Application user. |
| `mariadb.auth.password` | `""` | User password; generated (and reused via `lookup`) when empty. |
| `mariadb.auth.rootPassword` | `""` | Root password; generated when empty. |
| `mariadb.auth.existingSecret` | `""` | Pre-created Secret with both passwords. |
| `mariadb.auth.existingSecretPasswordKey` | `mariadb-password` | Key for the user password. |
| `mariadb.auth.existingSecretRootPasswordKey` | `mariadb-root-password` | Key for the root password. |
| `mariadb.persistence.enabled` | `true` | Persist `/var/lib/mysql`. |
| `mariadb.persistence.size` | `8Gi` | PVC size. |
| `mariadb.persistence.storageClass` | `""` | StorageClass (empty = default). |
| `mariadb.persistence.accessMode` | `ReadWriteOnce` | Access mode. |
| `mariadb.resources` | `{}` | Requests/limits. |
| `mariadb.podSecurityContext` / `mariadb.securityContext` | `{}` | Left empty on purpose - the official image needs `CAP_CHOWN` at startup. |
| `mariadb.nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` | Scheduling. |

### Redis (`redis.*`, `externalRedis.*`)

| Key | Default | Description |
|---|---|---|
| `redis.enabled` | `false` | Deploy a small Redis and point the Panel at it. |
| `redis.image.repository` / `.tag` / `.pullPolicy` | `redis` / `7-alpine` / `IfNotPresent` | Image. |
| `redis.password` | `""` | `REDIS_PASSWORD`; empty = no auth. |
| `redis.existingSecret` / `redis.existingSecretPasswordKey` | `""` / `redis-password` | Pre-created Secret. |
| `redis.resources` / `podSecurityContext` / `securityContext` / `nodeSelector` / `tolerations` / `affinity` | | Standard knobs. |
| `externalRedis.host` | `""` | `REDIS_HOST` when `redis.enabled=false`. |
| `externalRedis.port` | `6379` | `REDIS_PORT`. |
| `externalRedis.username` | `""` | `REDIS_USERNAME`. |
| `externalRedis.password` | `""` | `REDIS_PASSWORD`. |
| `externalRedis.existingSecret` / `externalRedis.existingSecretPasswordKey` | `""` / `redis-password` | Pre-created Secret. |

### Persistence (`persistence.*`) - `/pelican-data`

Holds the runtime `.env`, the sqlite database, `storage/` (avatars, icons,
fonts), `plugins/` and Caddy's state.

| Key | Default | Description |
|---|---|---|
| `persistence.enabled` | `true` | Use a PVC (otherwise `emptyDir`). |
| `persistence.existingClaim` | `""` | Use an existing PVC. |
| `persistence.size` | `5Gi` | PVC size. |
| `persistence.storageClass` | `""` | StorageClass; `"-"` disables dynamic provisioning. |
| `persistence.accessMode` | `ReadWriteOnce` | Access mode. |
| `persistence.annotations` | `{}` | Extra PVC annotations. |
| `persistence.retain` | `false` | Add `helm.sh/resource-policy: keep`. |

### Networking

| Key | Default | Description |
|---|---|---|
| `service.type` | `ClusterIP` | Service type. |
| `service.port` | `80` | Service port. |
| `service.nodePort` | `""` | NodePort when `type: NodePort`. |
| `service.annotations` | `{}` | Service annotations. |
| `ingress.enabled` | `false` | Create an Ingress. |
| `ingress.className` | `""` | `ingressClassName`. |
| `ingress.annotations` | `{}` | Ingress annotations. |
| `ingress.hosts` | see `values.yaml` | Hosts and paths (`host` is rendered through `tpl`). |
| `ingress.tls` | `[]` | Passed through verbatim. |
| `route.enabled` | `false` | Create an OpenShift Route. |
| `route.host` | `""` | Route host (empty = router-generated). |
| `route.path` | `""` | Route path. |
| `route.annotations` | `{}` | Route annotations. |
| `route.wildcardPolicy` | `None` | `None` or `Subdomain`. |
| `route.tls.enabled` | `true` | Enable TLS on the Route. |
| `route.tls.termination` | `edge` | `edge`, `passthrough`, `reencrypt`. |
| `route.tls.insecureEdgeTerminationPolicy` | `Redirect` | `Redirect`, `Allow`, `None`. |

### Security

| Key | Default | Description |
|---|---|---|
| `openshift.scc.enabled` | `false` | Create a RoleBinding for an SCC. |
| `openshift.scc.name` | `nonroot-v2` | SCC to bind. |
| `openshift.scc.bindToNamespaceGroup` | `false` | Bind to `system:serviceaccounts:<ns>` instead of the single SA. |
| `podSecurityContext` | uid/gid/fsGroup `82`, `seccompProfile: RuntimeDefault`, `net.ipv4.ip_unprivileged_port_start=0` | See the OpenShift section. |
| `securityContext` | drop `ALL`, no privilege escalation, `runAsNonRoot` | Container-level context. |

### Probes (`probes.*`)

All three hit `GET /up` on the `http` port and are skipped when
`panel.skipCaddy=true`.

| Key | Default | Description |
|---|---|---|
| `probes.startup.enabled` | `true` | Covers the entrypoint's migrate/optimize phase. |
| `probes.startup.initialDelaySeconds` | `10` | |
| `probes.startup.periodSeconds` | `10` | |
| `probes.startup.timeoutSeconds` | `5` | |
| `probes.startup.failureThreshold` | `60` | ~10 minutes of startup budget. |
| `probes.readiness.enabled` | `true` | |
| `probes.readiness.periodSeconds` | `30` | |
| `probes.readiness.timeoutSeconds` | `5` | |
| `probes.readiness.failureThreshold` | `3` | |
| `probes.liveness.enabled` | `true` | |
| `probes.liveness.periodSeconds` | `30` | |
| `probes.liveness.timeoutSeconds` | `5` | |
| `probes.liveness.failureThreshold` | `6` | |

## Gotchas discovered while deploying this

* **`TRUSTED_PROXIES=*` kills Caddy.** See above; use CIDRs.
* **Caddy cannot bind :80 under Kubernetes without the sysctl.** See above.
* **`/pelican-data/.env` says `APP_INSTALLED=false` forever.** The entrypoint
  writes that line on first boot and never updates it. It is harmless: Laravel's
  Dotenv never overrides real environment variables, so the ConfigMap's
  `APP_INSTALLED=true` wins.
* **The admin "Settings" page writes to `/pelican-data/.env`.** Anything this
  chart puts in the ConfigMap therefore *cannot* be changed from the UI - the
  container environment always wins. Keep `panel.extraEnv` minimal if you want
  to manage settings from the web UI.
* **`CACHE_STORE=database` does not work** - there is no `cache` table migration.
* **There is no `APP_TIMEZONE`** - `config/app.php` hardcodes UTC.
* **There is no `ADMIN_EMAIL`/bootstrap-admin environment variable.** Create the
  first user with `php artisan p:user:make`.

## Linting

```bash
helm lint charts/pelican-panel
helm lint charts/pelican-panel -f charts/pelican-panel/ci/openshift-cnpg-values.yaml
helm template t charts/pelican-panel -f charts/pelican-panel/ci/mariadb-redis-values.yaml
```

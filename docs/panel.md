# Deploying the Pelican Panel on Kubernetes / OpenShift

The `charts/pelican-panel` Helm chart deploys the **unmodified** upstream Panel
image `ghcr.io/pelican/panel`. Nothing about the Panel is patched - the chart
only supplies configuration, storage, networking and the OpenShift bits the
image needs.

Reference for every value: [`charts/pelican-panel/README.md`](../charts/pelican-panel/README.md).

---

## 1. What you get

A single Deployment (1 replica) running the upstream container, which itself
supervises Caddy, PHP-FPM, the Laravel **queue worker** and the **scheduler**.
No separate worker/scheduler Deployment is needed or wanted.

```
charts/pelican-panel/
  Chart.yaml            name pelican-panel, version 0.1.0, appVersion v1.0.0-beta38
  values.yaml           fully commented defaults
  README.md             every value documented
  ci/                   three example value sets (also used for helm lint)
  templates/
    _helpers.tpl        names, labels, DB/Redis resolution, APP_KEY handling, validation
    configmap.yaml      all non-secret env
    secret.yaml         chart-managed APP_KEY / generated passwords
    deployment.yaml     the Panel pod
    service.yaml        ClusterIP :80 -> named port "http"
    pvc.yaml            /pelican-data (existingClaim supported)
    ingress.yaml        optional Ingress (className / annotations / TLS)
    route.yaml          optional OpenShift Route (edge TLS)
    scc-rolebinding.yaml optional SCC RoleBinding
    serviceaccount.yaml
    mariadb.yaml        optional bundled MariaDB StatefulSet (default off)
    redis.yaml          optional bundled Redis (default off)
    NOTES.txt
```

---

## 2. Quick start (any cluster, SQLite)

```bash
helm install panel charts/pelican-panel -n pelican --create-namespace \
  --set panel.url=http://localhost:8080

kubectl -n pelican rollout status deploy/panel-pelican-panel
kubectl -n pelican port-forward svc/panel-pelican-panel 8080:80
```

Then create the first admin (there is no default account, and no
`ADMIN_EMAIL`-style bootstrap variable):

```bash
kubectl -n pelican exec deploy/panel-pelican-panel -- \
  php artisan p:user:make --admin=1 \
    --email=you@example.com --username=admin --password='<password>'
```

---

## 3. Choosing the pieces

### Database

`database.connection` = `pgsql` | `mysql` | `mariadb` | `sqlite`.

**PostgreSQL works.** Verified end to end on OpenShift with CloudNativePG: all migrations apply,
the scheduler and queue worker run, and the Filament UI logs in. No MariaDB
fallback was needed.

Every connection detail can come from an existing Secret, which makes a
CloudNativePG `<cluster>-app` Secret a drop-in (see the worked example in section 6).

`mariadb.enabled=true` gives you a throwaway single-replica MariaDB
StatefulSet. It is for quick starts; on OpenShift it additionally needs the
`anyuid` SCC because the official MariaDB image starts as root.

### Cache / session / queue

| Value | Options | Chart default |
|---|---|---|
| `panel.cacheStore` | `file`, `redis` | `file` |
| `panel.sessionDriver` | `database`, `file`, `cookie`, `redis` | `database` |
| `panel.queueConnection` | `database`, `redis`, `sync` | `database` |

`CACHE_STORE=database` is **not** supported by the Panel (no `cache` table
migration) and the chart refuses it. `redis.enabled=true` deploys a small Redis;
otherwise set `externalRedis.host`.

### Behind a proxy

```yaml
panel:
  url: https://panel.example.com     # APP_URL must match exactly
  behindProxy: true                  # Caddy listens on plain :80
  trustedProxies: "0.0.0.0/0,::/0"   # IPs/CIDRs only - NOT "*"
```

---

## 4. GitOps (Argo CD)

Argo CD renders with `helm template`, which cannot run Helm's `lookup`.
Consequences:

1. **`panel.existingSecret` is mandatory.** Otherwise the chart's generated
   `APP_KEY` changes on every sync, which invalidates every encrypted value
   (2FA secrets, node daemon tokens) and every session.
2. The same applies to `mariadb.auth.existingSecret` if you use the bundled DB.

Create the Secret once, out of band, and never commit it:

```bash
oc create secret generic pelican-panel-secrets -n pelican \
  --from-literal=APP_KEY="base64:$(head -c 32 /dev/urandom | base64 -w0)"
```

Then the values file in the `gitops` repo stays secret-free:

```yaml
panel:
  existingSecret: pelican-panel-secrets
  existingSecretAppKeyKey: APP_KEY
database:
  existingSecret: pelican-db-app
```

Pick a Secret name that the chart does **not** generate itself
(`<release>-pelican-panel-env`), so a sync can never overwrite it.

The rest is the usual Argo CD setup: a CNPG `Cluster` in the app namespace, a
RoleBinding that lets the Argo CD application controller manage the `pelican`
namespace, a multi-source `Application` (chart + `$values` from your config
repository), then the Route.

---

## 5. OpenShift notes

Three things the image needs that a vanilla `restricted-v2` namespace denies:

| Problem | Symptom | Chart fix |
|---|---|---|
| Image has a fixed `USER www-data` (uid/gid 82) | pod rejected by SCC, or files in `/pelican-data` unwritable | `openshift.scc.enabled=true` (binds `nonroot-v2` to the chart SA) + `podSecurityContext.runAsUser/runAsGroup/fsGroup: 82` |
| Caddy binds port 80 as a non-root user | `Error: ... listen tcp :80: bind: permission denied`, pod never becomes ready | `podSecurityContext.sysctls: net.ipv4.ip_unprivileged_port_start=0` (a *safe* sysctl - Docker sets it implicitly, Kubernetes does not). Adding `NET_BIND_SERVICE` does **not** help: Kubernetes cannot grant ambient capabilities, and `allowPrivilegeEscalation: false` disables file capabilities. |
| Route needs a named target port | router returns 503 | the chart always emits `port.targetPort: http` |

Prefer `nonroot-v2` over `anyuid` - it is the least privilege that still allows a
fixed non-root UID.

---

## 6. Worked example: OpenShift with CloudNativePG

A complete deployment in the namespace `pelican`, exposed through a Route at
`https://pelican.apps.example.com`. Replace the hostname and the storage class
with your own.

### 6.1 Database (CloudNativePG)

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pelican-db
  namespace: pelican
spec:
  instances: 1
  storage:
    size: 5Gi
    storageClass: <your-storage-class>
  bootstrap:
    initdb:
      database: pelican
      owner: pelican
```

CNPG publishes `pelican-db-app` with the keys `host`, `port`, `dbname`,
`username`, `password` - exactly what the chart's `database.existingSecret*`
options consume.

### 6.2 APP_KEY secret

```bash
oc create secret generic pelican-panel-secrets -n pelican \
  --from-literal=APP_KEY="base64:$(head -c 32 /dev/urandom | base64 -w0)"
```

### 6.3 Values (`pelican/values.yaml`, no secrets)

```yaml
panel:
  url: https://pelican.apps.example.com
  env: production
  debug: false
  installed: true
  behindProxy: true
  trustedProxies: "0.0.0.0/0,::/0"
  cacheStore: file
  sessionDriver: database
  queueConnection: database
  existingSecret: pelican-panel-secrets
  existingSecretAppKeyKey: APP_KEY
  mail:
    mailer: log

database:
  connection: pgsql
  existingSecret: pelican-db-app
  existingSecretPasswordKey: password
  existingSecretUsernameKey: username
  existingSecretDatabaseKey: dbname
  existingSecretHostKey: host
  existingSecretPortKey: port

persistence:
  enabled: true
  size: 5Gi
  storageClass: <your-storage-class>
  accessMode: ReadWriteOnce

route:
  enabled: true
  host: pelican.apps.example.com
  tls:
    enabled: true
    termination: edge
    insecureEdgeTerminationPolicy: Redirect

openshift:
  scc:
    enabled: true
    name: nonroot-v2

resources:
  requests:
    cpu: 100m
    memory: 512Mi
  limits:
    memory: 2Gi
```

### 6.4 Install

```bash
oc whoami --show-server      # confirm the target cluster first

helm upgrade --install pelican-panel charts/pelican-panel -n pelican -f <values file>
```

### 6.5 Admin user

```bash
PW="$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c1-28)"
oc create secret generic pelican-panel-admin -n pelican \
  --from-literal=username=admin \
  --from-literal=password="$PW" \
  --from-literal=email=admin@example.com

oc exec -n pelican deploy/pelican-panel -- php artisan p:user:make \
  --admin=1 --email=admin@example.com --username=admin --password="$PW"
```

Read the password back later with:

```bash
oc get secret pelican-panel-admin -n pelican -o jsonpath='{.data.password}' | base64 -d
```

### 6.6 Backups

Back up the Panel PVC and the database with whatever your storage layer offers.
With Longhorn, opt the Panel volume into a recurring-job group (the CNPG volume
too, or configure CNPG's own backups):

```bash
oc get pv $(oc get pvc pelican-panel-data -n pelican -o jsonpath='{.spec.volumeName}') \
  -o jsonpath='{.spec.csi.volumeHandle}{"\n"}'
oc label volume.longhorn.io -n longhorn-system <pvc-xxxx> \
  recurring-job-group.longhorn.io/backup=enabled --overwrite
```

---

## 7. Day 2

### Upgrading the Panel

Bump `appVersion` in `Chart.yaml` (or set `image.tag`) and `helm upgrade`. The
entrypoint runs `php artisan migrate --force` on start, so schema upgrades happen
automatically. Back up the database first.

### Logs

```bash
oc logs -n pelican deploy/pelican-panel            # Caddy + PHP-FPM + queue + cron, interleaved
oc exec -n pelican deploy/pelican-panel -- tail -n 100 /var/www/html/storage/logs/laravel.log
```

### Console commands

```bash
oc exec -n pelican deploy/pelican-panel -- php artisan p:user:make ...
oc exec -n pelican deploy/pelican-panel -- php artisan p:user:delete ...
oc exec -n pelican deploy/pelican-panel -- php artisan migrate --force
```

### Settings written from the UI

The Panel's admin **Settings** page writes to `/pelican-data/.env`. Laravel's
Dotenv never overrides a real environment variable, so anything this chart puts
in the ConfigMap silently wins over the UI. Keep the ConfigMap minimal if you
want to manage a setting from the web UI - or manage it in `panel.extraEnv` and
treat the UI field as read-only.

---

## 8. Connecting a node later

The Panel is only half of Pelican: servers run on **nodes**. Today that means a
Wings daemon; in this repo it will eventually mean the
gateway/operator/agent described in [`../ARCHITECTURE.md`](../ARCHITECTURE.md)
and [`wings-panel-contract.md`](wings-panel-contract.md).

Either way the Panel side is the same:

1. In the Panel UI, **Admin -> Nodes -> Create Node**. Give it the FQDN the Panel
   will reach the daemon on, the port (default 8080), and whether it uses TLS.
2. The Panel generates a node configuration (`Configuration` tab, or
   `php artisan p:node:configuration <id>`) containing `uuid`, `token_id` and
   `token` - these are encrypted with `APP_KEY`, which is why APP_KEY stability
   matters.
3. Hand that configuration to the daemon. The daemon calls back to
   `APP_URL/api/remote/...`, so `panel.url` must be reachable **from the node**,
   not only from your browser. In the example above that is
   `https://pelican.apps.example.com` via the Route.
4. Allocations (IP + ports) are created per node in the Panel and must match what
   the node can actually bind.

If the node lives inside the same cluster, it can reach the Panel at
`http://pelican-panel.pelican.svc` - but keep `APP_URL` on the public URL, since
the Panel uses it for links and assets. Point the node at the Route instead, or
add the in-cluster hostname to `panel.trustedProxies` handling as needed.

---

## 9. Verified behaviour (2026-09-16, single-node OpenShift)

| Check | Result |
|---|---|
| `helm lint` (default + 3 CI value sets) | pass |
| `helm template` + `oc apply --dry-run=server` | pass for all three value sets |
| Chart install on OpenShift 4.22 / k8s 1.35 | `pelican-panel` release `deployed`, pod `1/1 Running`, SCC `nonroot-v2` |
| PostgreSQL 18 (CNPG) migrations | all migrations applied, `users`/`jobs`/`sessions` tables present |
| Scheduler | `supercronic` runs `p:schedule:process`, `health:schedule-check-heartbeat`, `health:check` every minute |
| Queue worker | `queue:work` process running in the pod |
| `GET https://pelican.apps.example.com/up` | `200` |
| `GET http://pelican.apps.example.com/` | `302` to https (Route `insecureEdgeTerminationPolicy: Redirect`) |
| `GET https://pelican.apps.example.com/` | `302` -> `/login`, `200`, cookies `Secure` (HTTPS correctly detected behind the router) |
| Rendered asset URLs | all `https://pelican.apps.example.com/...` |
| Admin creation | `p:user:make` created `admin` / `admin@example.com`, row present in the `users` table |
| Web login | full Filament/Livewire `authenticate` call returned `redirect: https://pelican.apps.example.com`, follow-up request rendered the authenticated **Servers** dashboard |
| Pod restart | `rollout restart` came back healthy; `/pelican-data` (uid 82, `drwxrwsr-x`) retained `.env`, `storage/`, `caddy/` |

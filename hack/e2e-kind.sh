#!/usr/bin/env bash
# Contract / end-to-end suite on a throwaway kind cluster:
#   real Pelican Panel (our chart, SQLite)  <->  real gateway/operator/agent/shim
# It creates the node and a Paper server through the Panel's own services,
# waits for the install Job and the auto-start, runs test/e2e against the
# gateway, and asserts the Panel-side view (status, backups, suspension,
# deletion). Runs locally and in CI (.github/workflows/contract.yaml).
#
#   PANEL_IMAGE=ghcr.io/pelican/panel:latest hack/e2e-kind.sh   # override the Panel version
#   KEEP=1 hack/e2e-kind.sh                                     # keep the cluster afterwards
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-pelican-contract}
KIND_IMAGE=${KIND_IMAGE:-kindest/node:v1.36.4}
TAG=${TAG:-contract}
PANEL_IMAGE=${PANEL_IMAGE:-}
GAME_IMAGE=${GAME_IMAGE:-ghcr.io/pelican-eggs/yolks:java_25}
EGG_URL=${EGG_URL:-https://raw.githubusercontent.com/pelican-eggs/minecraft/main/java/paper/egg-paper.yaml}
KIND=${KIND:-kind}
KUBECTL_BIN=${KUBECTL:-kubectl}   # e.g. KUBECTL=oc on machines without kubectl
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/kind-$CLUSTER}
kubectl() { command "$KUBECTL_BIN" "$@"; }

log() { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
panel() { kubectl -n pelican exec deploy/pelican-panel -- php artisan "$@"; }
tinker() { kubectl -n pelican exec deploy/pelican-panel -- php artisan tinker --execute="$1" 2>&1 | tail -n "${2:-1}"; }
# The Panel's view of the server state, asked from the gateway (an enum on current Panels, a string on older ones).
panel_status() { tinker '$x = App\Models\Server::find(1)->retrieveStatus(); echo $x instanceof BackedEnum ? $x->value : $x, PHP_EOL;' || true; }
cleanup() {
  local rc=$?
  if [ $rc -ne 0 ]; then
    log "FAILED (rc=$rc): diagnostics"
    kubectl get gameservers -A -o yaml 2>/dev/null | sed -n '1,200p' || true
    kubectl -n pelican-servers get pods,jobs 2>/dev/null || true
    kubectl -n pelican-system logs deploy/pelican-k8s-gateway --tail=80 2>/dev/null || true
    kubectl -n pelican-system logs deploy/pelican-k8s-operator --tail=80 2>/dev/null || true
    for p in $(kubectl -n pelican-servers get pods -o name 2>/dev/null); do kubectl -n pelican-servers logs "$p" --all-containers --tail=60 2>/dev/null || true; done
  fi
  [ -n "${PF_PIDS:-}" ] && kill $PF_PIDS 2>/dev/null || true
  pkill -f "port-forward svc/pelican-k8s-gateway" 2>/dev/null || true
  if [ "${KEEP:-0}" != 1 ]; then "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  exit $rc
}
trap cleanup EXIT

log "kind cluster $CLUSTER ($KIND_IMAGE)"
"$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
"$KIND" create cluster --name "$CLUSTER" --image "$KIND_IMAGE" --config hack/kind-config.yaml --wait 120s
kubectl get nodes

log "build and load images ($TAG)"
for c in shim agent gateway operator; do
  docker build -q -f "build/$c.Dockerfile" -t "pelican-k8s/$c:$TAG" --build-arg "VERSION=$TAG" . >/dev/null
done
"$KIND" load docker-image --name "$CLUSTER" pelican-k8s/shim:$TAG pelican-k8s/agent:$TAG pelican-k8s/gateway:$TAG pelican-k8s/operator:$TAG >/dev/null
docker pull -q "$GAME_IMAGE" >/dev/null && "$KIND" load docker-image --name "$CLUSTER" "$GAME_IMAGE" >/dev/null || true

log "Pelican Panel (chart, SQLite)"
kubectl create namespace pelican
PANEL_SET=(--set panel.url=http://pelican-panel.pelican.svc --set database.connection=sqlite --set persistence.enabled=true)
[ -n "$PANEL_IMAGE" ] && PANEL_SET+=(--set "image.repository=${PANEL_IMAGE%%:*}" --set "image.tag=${PANEL_IMAGE##*:}")
helm install pelican-panel charts/pelican-panel -n pelican "${PANEL_SET[@]}" --wait --timeout 10m >/dev/null
kubectl -n pelican rollout status deploy/pelican-panel --timeout=5m
until kubectl -n pelican exec deploy/pelican-panel -- curl -sf -o /dev/null http://localhost/up; do sleep 5; done
ADMIN_PW=contract-$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
panel p:user:make --admin=1 --email=contract@example.com --username=admin --password="$ADMIN_PW" --no-interaction >/dev/null
tinker '$s = app(App\Services\Eggs\Sharing\EggImporterService::class); $e = $s->fromUrl("'"$EGG_URL"'"); echo "egg ", $e->id, " ", $e->name, PHP_EOL;'
NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
panel p:node:make --name=k8s --description=contract --fqdn=pelican-k8s-gateway.pelican-system.svc --public=1 --scheme=http --proxy=0 --maintenance=0 \
  --maxMemory=16384 --overallocateMemory=0 --maxDisk=100000 --overallocateDisk=0 --maxCpu=800 --overallocateCpu=0 --uploadSize=100 \
  --daemonListeningPort=8080 --daemonConnectingPort=8080 --daemonSFTPPort=2022 --daemonSFTPAlias=pelican-k8s-gateway-sftp.pelican-system.svc \
  --daemonBase=/var/lib/pelican/volumes --no-interaction >/dev/null
TOKEN_ID=$(panel p:node:configuration 1 2>/dev/null | awk '/^token_id:/ {print $2}')
TOKEN=$(panel p:node:configuration 1 2>/dev/null | awk '/^token:/ {print $2}')
[ -n "$TOKEN_ID" ] && [ -n "$TOKEN" ]
tinker 'foreach (range(30565, 30567) as $p) { App\Models\Allocation::firstOrCreate(["node_id" => 1, "ip" => "'"$NODE_IP"'", "port" => $p]); } echo App\Models\Allocation::count(), " allocations", PHP_EOL;'

log "pelican-k8s (chart)"
helm install pelican-k8s charts/pelican-k8s -n pelican-system --create-namespace \
  --set image.registry=pelican-k8s --set image.tag="$TAG" --set image.pullPolicy=Never \
  --set gateway.panelURL=http://pelican-panel.pelican.svc --set gateway.nodeTokenID="$TOKEN_ID" --set gateway.nodeToken="$TOKEN" \
  --set gateway.sftp.service.type=ClusterIP \
  --set defaultClass.spec.exposure.mode=NodePort --set "defaultClass.spec.exposure.externalIPs[0]=$NODE_IP" \
  --set defaultClass.spec.storage.storageClassName=standard --set defaultClass.spec.storage.scratch.type=EmptyDir \
  --set defaultClass.spec.imageResolution.pinDigest=false --set gateway.logLevel=debug --set operator.logLevel=debug --wait --timeout 5m >/dev/null
kubectl -n pelican-system rollout status deploy/pelican-k8s-gateway --timeout=3m
kubectl -n pelican-system rollout status deploy/pelican-k8s-operator --timeout=3m

log "Panel -> gateway: node system information"
panel cache:clear >/dev/null
INFO=$(tinker 'echo json_encode(App\Models\Node::find(1)->systemInformation()), PHP_EOL;')
echo "$INFO"; echo "$INFO" | grep -q '"version"'; echo "$INFO" | grep -q exception && { echo "node exception"; exit 1; } || true

log "create a server through the Panel (start on completion)"
UUID=$(tinker '$a = App\Models\Allocation::whereNull("server_id")->orderBy("port")->first(); $s = app(App\Services\Servers\ServerCreationService::class)->handle(["name" => "contract", "owner_id" => 1, "egg_id" => 1, "allocation_id" => $a->id, "memory" => 1536, "disk" => 4096, "cpu" => 200, "swap" => 0, "io" => 500, "oom_killer" => true, "image" => "'"$GAME_IMAGE"'", "environment" => ["MINECRAFT_VERSION" => "latest", "SERVER_JARFILE" => "server.jar", "BUILD_NUMBER" => "latest"], "start_on_completion" => true, "skip_scripts" => false]); echo $s->uuid;')
echo "server uuid $UUID"; [ ${#UUID} -eq 36 ]
for i in $(seq 1 90); do
  r=$(kubectl -n pelican-servers get gameserver "gs-$UUID" -o jsonpath='{.status.phase}/{.status.install.result}/{.status.process.state}' 2>/dev/null || true)
  echo "t=$((i*10))s $r"
  case "$r" in */Succeeded/*) break;; */Failed/*) echo "install failed"; exit 1;; esac
  sleep 10
done
[ "$(tinker 'echo App\Models\Server::find(1)->installed_at ? "installed" : "not-installed", PHP_EOL;')" = installed ]

log "EULA + auto-start (the Paper egg exits until eula.txt is accepted)"
PF_PIDS=""
# kubectl port-forward exits when a forwarded connection is reset (the websocket test does that), so keep it in a loop.
forward() { while true; do kubectl -n pelican-system port-forward "$1" "$2" >/dev/null 2>&1 || true; sleep 1; done; }
forward svc/pelican-k8s-gateway 18080:8080 & PF_PIDS="$!"
forward svc/pelican-k8s-gateway-sftp 12022:2022 & PF_PIDS="$PF_PIDS $!"
G=http://127.0.0.1:18080
for i in $(seq 1 30); do curl -sf -o /dev/null "$G/healthz" && break; sleep 1; done
# power ACTION EXPECTED_CODE: retries while the forward reconnects, prints what the gateway answered.
power() {
  local code=000
  for i in $(seq 1 10); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "{\"action\":\"$1\"}" "$G/api/servers/$UUID/power" || true)
    [ "$code" != 000 ] && break; sleep 2
  done
  echo "power $1 -> $code"; [ "$code" = "$2" ]
}
curl -sf -o /dev/null -X POST -H "Authorization: Bearer $TOKEN" --data-binary 'eula=true' "$G/api/servers/$UUID/files/write?file=%2Feula.txt"

log "test/e2e against the gateway"
PELICAN_E2E_GATEWAY=$G PELICAN_E2E_TOKEN=$TOKEN PELICAN_E2E_PANEL_URL=http://pelican-panel.pelican.svc PELICAN_E2E_SERVER=$UUID \
  PELICAN_E2E_SFTP=127.0.0.1:12022 PELICAN_E2E_SFTP_USER=admin PELICAN_E2E_SFTP_PASSWORD="$ADMIN_PW" \
  go test -count=1 -tags e2e ./test/e2e -v -timeout 20m

log "Panel-side contract: status, backup, suspension, deletion"
power start 202
for i in $(seq 1 60); do st=$(panel_status); [ "$st" = running ] && break; sleep 5; done
echo "panel retrieveStatus=$st"; [ "$st" = running ]
tinker '$s = App\Models\Server::find(1); $s->update(["backup_limit" => 3]); $b = app(App\Services\Backups\InitiateBackupService::class)->setIgnoredFiles([])->handle($s->fresh(), "contract"); echo "backup ", $b->uuid, PHP_EOL;'
for i in $(seq 1 30); do ok=$(tinker 'echo App\Models\Backup::latest("id")->first()->is_successful ? "ok" : "pending", PHP_EOL;'); [ "$ok" = ok ] && break; sleep 5; done
echo "backup=$ok"; [ "$ok" = ok ]
tinker '$s = App\Models\Server::find(1); app(App\Services\Servers\SuspensionService::class)->handle($s, App\Enums\SuspendAction::Suspend); echo "suspended", PHP_EOL;'
for i in $(seq 1 40); do st=$(panel_status); [ "$st" = offline ] && break; sleep 5; done
echo "after suspend=$st"; [ "$st" = offline ]
power start 400
tinker '$s = App\Models\Server::find(1); app(App\Services\Servers\SuspensionService::class)->handle($s, App\Enums\SuspendAction::Unsuspend); echo "unsuspended", PHP_EOL;'
tinker 'app(App\Services\Servers\ServerDeletionService::class)->handle(App\Models\Server::find(1)); echo App\Models\Server::count(), " servers left", PHP_EOL;'
for i in $(seq 1 30); do n=$(kubectl -n pelican-servers get gameservers,pvc --no-headers 2>/dev/null | wc -l); [ "$n" = 0 ] && break; sleep 5; done
echo "objects left=$n"; [ "$n" = 0 ]

log "PASS: Panel $(tinker 'echo config("app.version"), PHP_EOL;') <-> pelican-k8s $TAG"

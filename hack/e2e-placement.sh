#!/usr/bin/env bash
# Live placement suite (test/placement) on a throwaway kind cluster with one
# control-plane node and three workers: pelican-k8s built from the working
# tree, the fake Panel (test/fakepanel/cmd/fakepanel) and a shell egg
# (test/placement/egg). Volumes come from the local-path provisioner in shared
# mode, so they carry no node affinity and the pod pair can move between
# nodes. Runs locally and in CI (.github/workflows/placement.yaml).
#
#   KEEP=1 hack/e2e-placement.sh          # keep the cluster afterwards
#   RUN=TestDrain hack/e2e-placement.sh   # one scenario
#   TLS=false hack/e2e-placement.sh       # without tls.enabled (on by default here)
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-pelican-placement}
KIND_IMAGE=${KIND_IMAGE:-kindest/node:v1.36.4}
TAG=${TAG:-placement}
KIND=${KIND:-kind}
RUN=${RUN:-.}
TOKEN_ID=placement
TOKEN=placement-$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')
EGG=pelican-k8s/placement-egg:$TAG
SHARED=/tmp/pelican-placement-shared
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/kind-$CLUSTER}

PHASE=
log() { PHASE=$*; printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    log "FAILED in \"${PHASE:-startup}\" (rc=$rc): diagnostics"
    kubectl get nodes -o wide 2>/dev/null || true
    kubectl -n pelican-system get pods -o wide 2>/dev/null || true
    kubectl -n pelican-servers get pods,pvc,statefulsets -o wide 2>/dev/null || true
    kubectl get pv 2>/dev/null || true
    kubectl -n pelican-servers get events --sort-by=.lastTimestamp 2>/dev/null | tail -60 || true
    kubectl -n pelican-system logs deploy/pelican-k8s-gateway --tail=100 2>/dev/null || true
    kubectl -n pelican-system logs deploy/pelican-k8s-operator --tail=150 2>/dev/null || true
    kubectl -n pelican logs deploy/fakepanel --tail=40 2>/dev/null || true
    kubectl -n local-path-storage logs deploy/local-path-provisioner --tail=40 2>/dev/null || true
  fi
  [[ -n "${PF_PIDS:-}" ]] && kill $PF_PIDS 2>/dev/null || true
  pkill -f "port-forward svc/pelican-k8s-gateway" 2>/dev/null || true
  pkill -f "port-forward svc/fakepanel" 2>/dev/null || true
  if [[ "${KEEP:-0}" != 1 ]]; then "$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
  exit $rc
}
trap cleanup EXIT

log "kind cluster $CLUSTER ($KIND_IMAGE, three workers)"
"$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
mkdir -p "$SHARED" && chmod 0777 "$SHARED"
"$KIND" create cluster --name "$CLUSTER" --image "$KIND_IMAGE" --config hack/kind-placement.yaml --wait 180s
kubectl get nodes -o wide

log "storage without node affinity (local-path shared mode)"
# nodePathMap and sharedFileSystemPath exclude each other; with the latter
# every volume is a directory under the mount all workers share.
kubectl -n local-path-storage patch configmap local-path-config --type merge \
  -p '{"data":{"config.json":"{\"sharedFileSystemPath\":\"/var/local-path-shared\"}"}}'
kubectl -n local-path-storage rollout restart deploy/local-path-provisioner
kubectl -n local-path-storage rollout status deploy/local-path-provisioner --timeout=2m

log "build and load images ($TAG)"
for c in shim agent gateway operator; do
  docker build -q -f "build/$c.Dockerfile" -t "pelican-k8s/$c:$TAG" --build-arg "VERSION=$TAG" . >/dev/null
done
docker build -q -f test/placement/fakepanel.Dockerfile -t "pelican-k8s/fakepanel:$TAG" . >/dev/null
docker build -q -t "$EGG" test/placement/egg >/dev/null
"$KIND" load docker-image --name "$CLUSTER" pelican-k8s/shim:$TAG pelican-k8s/agent:$TAG pelican-k8s/gateway:$TAG pelican-k8s/operator:$TAG pelican-k8s/fakepanel:$TAG "$EGG" >/dev/null

log "fake Panel"
kubectl create namespace pelican
kubectl -n pelican apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: fakepanel}
spec:
  selector: {matchLabels: {app: fakepanel}}
  template:
    metadata: {labels: {app: fakepanel}}
    spec:
      containers:
        - name: fakepanel
          image: pelican-k8s/fakepanel:$TAG
          imagePullPolicy: Never
          env:
            - {name: FAKEPANEL_TOKEN_ID, value: "$TOKEN_ID"}
            - {name: FAKEPANEL_TOKEN, value: "$TOKEN"}
          ports: [{containerPort: 8080}]
          readinessProbe: {httpGet: {path: /healthz, port: 8080}}
---
apiVersion: v1
kind: Service
metadata: {name: fakepanel}
spec:
  selector: {app: fakepanel}
  ports: [{port: 80, targetPort: 8080}]
YAML
kubectl -n pelican rollout status deploy/fakepanel --timeout=2m

log "pelican-k8s (chart)"
helm install pelican-k8s charts/pelican-k8s -n pelican-system --create-namespace \
  --set image.registry=pelican-k8s --set image.tag="$TAG" --set image.pullPolicy=Never \
  --set gateway.panelURL=http://fakepanel.pelican.svc --set gateway.nodeTokenID="$TOKEN_ID" --set gateway.nodeToken="$TOKEN" \
  --set gateway.sftp.service.type=ClusterIP \
  --set defaultClass.spec.exposure.mode=NodePort \
  --set defaultClass.spec.storage.storageClassName=standard --set defaultClass.spec.storage.scratch.type=EmptyDir \
  --set defaultClass.spec.imageResolution.pinDigest=false --set defaultClass.spec.terminationGracePeriodSeconds=30 \
  --set tls.enabled="${TLS:-true}" \
  --set gateway.logLevel=debug --set operator.logLevel=debug --wait --timeout 5m >/dev/null
kubectl -n pelican-system rollout status deploy/pelican-k8s-gateway --timeout=3m
kubectl -n pelican-system rollout status deploy/pelican-k8s-operator --timeout=3m

log "test/placement"
PF_PIDS=""
# kubectl port-forward exits when a forwarded connection is reset, so keep it in a loop.
forward() { while true; do kubectl -n "$1" port-forward "$2" "$3" >/dev/null 2>&1 || true; sleep 1; done; }
forward pelican-system svc/pelican-k8s-gateway 18080:8080 & PF_PIDS="$!"
forward pelican svc/fakepanel 18090:80 & PF_PIDS="$PF_PIDS $!"
for i in $(seq 1 30); do curl -sf -o /dev/null http://127.0.0.1:18080/healthz && curl -sf -o /dev/null http://127.0.0.1:18090/healthz && break; sleep 1; done
PELICAN_PLACEMENT_GATEWAY=http://127.0.0.1:18080 PELICAN_PLACEMENT_TOKEN="$TOKEN" \
  PELICAN_PLACEMENT_PANEL=http://127.0.0.1:18090 PELICAN_PLACEMENT_EGG="$EGG" \
  go test -count=1 -tags placement ./test/placement -v -timeout 75m -run "$RUN"

log "PASS"

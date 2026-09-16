#!/usr/bin/env bash
# Build all images, push them to a registry with crane (works without Docker
# registry TLS trust) and roll the Helm release. Usage:
#   REGISTRY=harbor.example.com/pelican VALUES=values.yaml hack/dev-push.sh [component...]
set -euo pipefail
cd "$(dirname "$0")/.."
REGISTRY=${REGISTRY:?set REGISTRY (e.g. harbor.example.com/pelican)}
DIRTY=$([ -n "$(git status --porcelain)" ] && echo -dirty || true)
TAG=${TAG:-dev-$(git rev-parse --short HEAD)$DIRTY}
COMPONENTS=("$@")
# A Helm roll sets one tag for every component, so build all of them then.
if [ ${#COMPONENTS[@]} -eq 0 ] || [ -n "${VALUES:-}" ]; then COMPONENTS=(shim agent gateway operator); fi
CRANE=${CRANE:-crane}
INSECURE=${INSECURE:-true}
CRANE_FLAGS=()
[ "$INSECURE" = true ] && CRANE_FLAGS+=(--insecure)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
for c in "${COMPONENTS[@]}"; do
  echo "== building $c ($TAG)"
  docker build -q -f "build/$c.Dockerfile" -t "pelican-k8s/$c:$TAG" --build-arg "VERSION=$TAG" .
  docker save "pelican-k8s/$c:$TAG" -o "$TMP/$c.tar"
  echo "== pushing $REGISTRY/$c:$TAG"
  "$CRANE" push "${CRANE_FLAGS[@]}" "$TMP/$c.tar" "$REGISTRY/$c:$TAG" >/dev/null
done
if [ -n "${VALUES:-}" ]; then
  echo "== upgrading helm release ${RELEASE:-pelican-k8s} in ${NAMESPACE:-pelican-system}"
  helm upgrade --install "${RELEASE:-pelican-k8s}" charts/pelican-k8s -n "${NAMESPACE:-pelican-system}" -f "$VALUES" \
    --set image.registry="$REGISTRY" --set image.tag="$TAG" >/dev/null
  echo "release rolled to $TAG"
fi
echo "TAG=$TAG"

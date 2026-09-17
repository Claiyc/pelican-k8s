#!/usr/bin/env bash
# Checks the Pelican Panel's daemon-facing source against docs/wings-panel-contract.md:
# the remote API route table (section 3) and the two authentication points the
# gateway depends on (section 1.1). Used by the nightly upstream-drift workflow
# and runnable locally:
#
#   hack/check-panel-contract.sh                                   # latest Panel image
#   PANEL_IMAGE=ghcr.io/pelican/panel:v1.0.0-beta38 hack/check-panel-contract.sh
#   PANEL_SRC_DIR=/path/to/panel hack/check-panel-contract.sh      # a checkout, no Docker
#
# The route table is parsed rather than grepped: upstream registers the same
# paths through `Route::prefix(...)->group(...)`, so a literal path string such
# as "servers/{uuid}/container/status" never appears in the source even when the
# endpoint is unchanged.
set -euo pipefail
cd "$(dirname "$0")/.."

PANEL_IMAGE=${PANEL_IMAGE:-ghcr.io/pelican/panel:latest}
FILES=(
  routes/api-remote.php
  app/Repositories/Daemon/DaemonRepository.php
  app/Providers/AppServiceProvider.php
)

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"' EXIT

SRC=${PANEL_SRC_DIR:-}
if [ -z "$SRC" ]; then
  SRC=$WORKDIR/panel
  docker pull -q "$PANEL_IMAGE"
  for f in "${FILES[@]}"; do
    mkdir -p "$SRC/$(dirname "$f")"
    # A moved file must reach the check below rather than abort the script here.
    docker run --rm "$PANEL_IMAGE" cat "$f" > "$SRC/$f" || true
  done
fi

for f in "${FILES[@]}"; do
  [ -s "$SRC/$f" ] || { echo "::error::$f is missing or empty in $PANEL_IMAGE (upstream moved it)"; exit 1; }
done

fail=0
note() { echo "::error::$*"; fail=1; }

# --- Remote API route table (contract section 3) ------------------------------
# Resolves Route::prefix(...)->group(...) nesting and normalises Laravel's
# explicit bindings ({server:uuid}, {backup:uuid}) to the {uuid} the doc uses.
parse_routes() {
  local line prefix path method
  local -a stack=()
  while IFS= read -r line; do
    if [[ $line =~ Route::prefix\(\'([^\']+)\'\)-\>group\( ]]; then
      stack+=("${BASH_REMATCH[1]}")
      continue
    fi
    if [[ $line =~ ^[[:space:]]*\}\)\; ]]; then
      if [ ${#stack[@]} -gt 0 ]; then unset 'stack[${#stack[@]}-1]'; fi
      continue
    fi
    if [[ $line =~ Route::(get|post|put|patch|delete)\(\'([^\']*)\' ]]; then
      method=${BASH_REMATCH[1]}
      path=${BASH_REMATCH[2]}
      prefix=$(IFS=; echo "${stack[*]-}")
      path="$prefix$path"
      path=${path//\/\//\/}                                   # joined group + "/" leaf
      if [ "$path" != "/" ]; then path=${path%/}; fi           # a group's own "/" leaf
      path=$(echo "$path" | sed -E 's/\{[A-Za-z_]+:([A-Za-z_]+)\}/{\1}/g')
      echo "$(echo "$method" | tr '[:lower:]' '[:upper:]') $path"
    fi
  done < "$SRC/routes/api-remote.php" | sort
}

want_routes=$(sort <<'EOF'
POST /sftp/auth
GET /servers
POST /servers/reset
POST /activity
GET /servers/{uuid}
GET /servers/{uuid}/install
POST /servers/{uuid}/install
POST /servers/{uuid}/transfer/failure
POST /servers/{uuid}/transfer/success
POST /servers/{uuid}/container/status
GET /backups/{uuid}
POST /backups/{uuid}
POST /backups/{uuid}/restore
EOF
)
got_routes=$(parse_routes)

if [ -z "$got_routes" ]; then
  note "parsed no routes from routes/api-remote.php; the parser in $0 needs updating"
elif ! diff -u <(echo "$want_routes") <(echo "$got_routes") > "$WORKDIR/routes.diff"; then
  note "the Panel remote API no longer matches docs/wings-panel-contract.md section 3 (-want +got):"
  cat "$WORKDIR/routes.diff"
fi

# --- Authentication points (contract section 1.1) -----------------------------
# Panel -> gateway: the node token goes out as a bare bearer token against the
# node's connection address; the gateway's JWT and signed URLs assume both.
grep -q 'withToken($node->daemon_token)' "$SRC/app/Providers/AppServiceProvider.php" ||
  note "Http::daemon no longer sends \$node->daemon_token as a bearer token (contract 1.1)"
grep -q 'baseUrl($node->getConnectionAddress())' "$SRC/app/Providers/AppServiceProvider.php" ||
  note "Http::daemon no longer builds its base URL from Node::getConnectionAddress() (contract 1.1)"

# Gateway -> Panel: every gateway response must carry the Wings User-Agent, and
# the id in it must be the node's daemon_token_id.
grep -q 'Pelican Wings' "$SRC/app/Repositories/Daemon/DaemonRepository.php" ||
  note "the Panel no longer checks the 'Pelican Wings' response User-Agent (contract 1.1); the gateway emits it in internal/gateway/config"
grep -q 'daemon_token_id' "$SRC/app/Repositories/Daemon/DaemonRepository.php" ||
  note "the response User-Agent is no longer matched against the node's daemon_token_id (contract 1.1)"

if [ "$fail" -ne 0 ]; then
  echo "Panel contract check failed against $PANEL_IMAGE" >&2
  exit 1
fi
echo "Panel contract check passed against ${PANEL_SRC_DIR:-$PANEL_IMAGE}"

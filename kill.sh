#!/bin/bash
# Stop everything the project runs: API, worker, frontend, Docker containers,
# and free the ports they use.
#
# Usage: ./kill.sh [--apps-only] [--quit-docker]
#   --apps-only    only stop api/worker/web processes (leave Docker running)
#   --quit-docker  also stop the Docker runtime (Colima / Docker Desktop / OrbStack)
set -uo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$ROOT_DIR/scripts/lib.sh"

APPS_ONLY=0; QUIT_DOCKER=0
for arg in "$@"; do
  case "$arg" in
    --apps-only) APPS_ONLY=1 ;;
    --quit-docker) QUIT_DOCKER=1 ;;
    *) echo "Unknown option: $arg" >&2; exit 2 ;;
  esac
done

APP_PORTS=(8080 3000)                        # API, Next.js
INFRA_PORTS=(27017 9000 9001 6379 9090 3001) # Mongo, MinIO, Redis, Prometheus, Grafana

# Kill whatever is LISTENing on a port, except Docker's own port proxies.
free_port() {
  local port="$1" pid name
  for pid in $(lsof -ti "tcp:$port" -sTCP:LISTEN 2>/dev/null); do
    name="$(ps -p "$pid" -o comm= 2>/dev/null)"
    case "$name" in *docker*|*Docker*|*vpnkit*) continue ;; esac
    kill "$pid" 2>/dev/null; sleep 1
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null
    log_ok "Freed :$port (killed $name, pid $pid)"
  done
}

log_step "Stopping app processes..."
# Hot-reload wrappers and dev servers started from this repo.
pkill -f "air -c .air.api.toml" 2>/dev/null
pkill -f "air -c .air.worker.toml" 2>/dev/null
pkill -f "$ROOT_DIR/StoreHUBClient/node_modules/.bin/next" 2>/dev/null
pkill -f "$ROOT_DIR/StoreHUBClient/.*next-server" 2>/dev/null
pkill -f "$ROOT_DIR/StoreHUBXBackend/tmp/" 2>/dev/null
for p in "${APP_PORTS[@]}"; do free_port "$p"; done
log_ok "App processes stopped."

[ "$APPS_ONLY" = 1 ] && exit 0

log_step "Stopping Docker containers..."
if docker info >/dev/null 2>&1; then
  (cd "$ROOT_DIR/StoreHUBXBackend" && docker-compose down) && log_ok "Containers stopped."
else
  log_warn "Docker is not running; nothing to stop."
fi

if [ "$QUIT_DOCKER" = 1 ]; then
  if command -v colima >/dev/null 2>&1 && colima status >/dev/null 2>&1; then
    log_step "Stopping Colima..."
    colima stop && log_ok "Colima stopped."
  elif [ -d /Applications/Docker.app ]; then
    log_step "Quitting Docker Desktop..."
    osascript -e 'quit app "Docker"' >/dev/null 2>&1 && log_ok "Docker Desktop quit."
  elif [ -d /Applications/OrbStack.app ]; then
    log_step "Quitting OrbStack..."
    osascript -e 'quit app "OrbStack"' >/dev/null 2>&1 && log_ok "OrbStack quit."
  fi
fi

# Report (don't kill) anything else still holding the infra ports, e.g. a
# locally installed mongod/redis that isn't ours.
for p in "${INFRA_PORTS[@]}"; do
  pid="$(lsof -ti "tcp:$p" -sTCP:LISTEN 2>/dev/null | head -1)"
  [ -n "$pid" ] && log_warn "Port :$p still in use by $(ps -p "$pid" -o comm= 2>/dev/null) (pid $pid)"
done
echo "Done."

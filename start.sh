#!/bin/bash
# Start the whole project: Docker infra (Mongo, MinIO, Redis) + API + worker +
# frontend. Ctrl+C stops the app processes;
# run ./kill.sh to also bring down Docker and free every port.
set -uo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"
source scripts/lib.sh

# 1. Make sure a Docker daemon is up: Colima, Docker Desktop or OrbStack,
#    whichever is installed.
if ! docker info >/dev/null 2>&1; then
  if command -v colima >/dev/null 2>&1; then
    log_step "Starting Colima..."
    colima start || exit 1
  elif [ -d /Applications/Docker.app ]; then
    log_step "Starting Docker Desktop..."
    open -a Docker
  elif [ -d /Applications/OrbStack.app ]; then
    log_step "Starting OrbStack..."
    open -a OrbStack
  fi
  for _ in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 2; done
  docker info >/dev/null 2>&1 || { echo "Docker is not running. Start it and retry." >&2; exit 1; }
fi

# 2. Clear leftovers from a previous run so ports 8080/3000 are free.
bash "$ROOT_DIR/kill.sh" --apps-only

# 3. First-run scaffolding (deps + .env files) if missing.
if [ ! -f StoreHUBXBackend/.env ] || [ ! -f StoreHUBClient/.env.local ] || [ ! -d StoreHUBClient/node_modules ]; then
  log_step "First run: installing dependencies and creating .env files..."
  make install || exit 1
  if grep -q '^GITHUB_CLIENT_ID=$' StoreHUBXBackend/.env 2>/dev/null; then
    log_warn "Fill in GITHUB_CLIENT_ID / GITHUB_CLIENT_SECRET / MONGO_URI in StoreHUBXBackend/.env, then re-run ./start.sh"
    exit 1
  fi
fi

# 4. Preflight checks + infra + hot-reloading api, worker and web.
exec make dev

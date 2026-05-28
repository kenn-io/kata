#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if ! command -v docker >/dev/null 2>&1; then
  echo "Docker is not available: docker CLI was not found in PATH" >&2
  exit 1
fi

if ! docker info >/dev/null 2>&1; then
  echo "Docker is not available: docker daemon is not reachable by the current user" >&2
  exit 1
fi

if ! docker compose version >/dev/null 2>&1; then
  echo "Docker Compose is not available: 'docker compose' plugin was not found or is not working" >&2
  exit 1
fi

compose_project=${KATA_FEDERATION_DOCKER_PROJECT:-kata-federation-smoke}
compose=(docker compose -f docker/federation/docker-compose.yml -p "$compose_project")

cleanup() {
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

cleanup
"${compose[@]}" up --build --abort-on-container-exit --exit-code-from runner runner

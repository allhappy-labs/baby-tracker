#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
builder="baby-tracker-build-$(date +%s)-$$"
cleanup() { docker buildx rm "$builder" >/dev/null; }
docker buildx create --name "$builder" --driver docker-container >/dev/null
trap cleanup EXIT
docker buildx build --builder "$builder" --platform linux/amd64,linux/arm64 \
  --output type=cacheonly -f Server/Dockerfile Server

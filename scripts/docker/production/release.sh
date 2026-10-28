#!/bin/bash
set -xeuo pipefail

if [[ $# == 0 ]]; then
  RELEASE="$(git describe --tags)"
elif [[ $# == 3 && "$1" == --pr && "$2" =~ ^[1-9][0-9]{0,9}$ ]]; then
  branch=$(printf '%s' "$3" | LC_ALL=C tr '[:upper:]' '[:lower:]' | LC_ALL=C sed -E 's/[^a-z0-9_.-]+/-/g; s/^[._-]+//; s/[._-]+$//')
  RELEASE="e2e-pr$2-${branch:-branch}"
  RELEASE="${RELEASE:0:128}"
else
  echo "Usage: $0 [--pr NUMBER BRANCH]" >&2
  exit 2
fi

docker build -t "cozy/cozy-stack:${RELEASE}" -f scripts/docker/production/Dockerfile .
docker push "cozy/cozy-stack:${RELEASE}"

if [[ $# == 0 ]]; then
  docker tag "cozy/cozy-stack:${RELEASE}" "cozy/cozy-stack:latest"
  docker push "cozy/cozy-stack:latest"
fi

if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  echo "image=cozy/cozy-stack:$RELEASE" >> "$GITHUB_OUTPUT"
fi

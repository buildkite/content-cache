#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=${1:-${CI_COMMIT_TAG:-0.0.0-dev}}
version=${version#v}
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$' || {
  echo 'Invalid build version' >&2; exit 1;
}
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOAMD64=v1 \
    go build -trimpath -ldflags="-s -w -buildid= -X main.version=$version" \
    -o "dist/content-cache_linux_$arch" ./cmd/content-cache
done
cp LICENSE dist/LICENSE
{
  printf 'version=%s\ncommit=%s\n' "$version" "$(git rev-parse HEAD)"
  if [ -n "$(git status --porcelain)" ]; then
    printf 'worktree=dirty\n'
  else
    printf 'worktree=clean\n'
  fi
  printf 'upstream_commit=%s\n' "$(cat UPSTREAM_COMMIT)"
  go version
} > dist/BUILD-INFO.txt
(cd dist && sha256sum content-cache_linux_amd64 content-cache_linux_arm64 LICENSE BUILD-INFO.txt > SHA256SUMS)

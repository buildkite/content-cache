#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
unformatted=$(git ls-files -z '*.go' | xargs -0 gofmt -l)
if [ -n "$unformatted" ]; then
  printf 'Run gofmt on:\n%s\n' "$unformatted" >&2
  exit 1
fi
go vet ./...
go test -race -p 2 -timeout 10m ./...

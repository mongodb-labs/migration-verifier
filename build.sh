#!/bin/sh

revision=$(git describe --tags --exact-match 2>/dev/null || echo "DEVELOPMENT:$(git describe --tags)")
buildTime=$(date -u)

goos=$(go env GOOS)
goarch=$(go env GOARCH)

module_path="$(go list -m -f '{{.Path}}')"

export CGO_ENABLED=0

printf 'Building migration-verifier for %s/%s …\n' "$goos" "$goarch"
printf '\tRevision: %s\n' "$revision"
printf '\tBuild Time: %s\n' "$buildTime"

go build -ldflags="-X '$module_path/buildvar.Revision=$revision' -X '$module_path/buildvar.BuildTime=$buildTime'" main/migration_verifier.go

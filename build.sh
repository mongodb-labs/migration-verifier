#!/bin/sh

revision=$(git describe --tags --exact-match 2>/dev/null || echo "DEVELOPMENT:$(git describe --tags)")
buildTime=$(date -u)

goos=$(go env GOOS)
goarch=$(go env GOARCH)

printf 'Building migration-verifier for %s/%s …\n' "$goos" "$goarch"
printf '\tRevision: %s\n' "$revision"
printf '\tBuild Time: %s\n' "$buildTime"

go build -ldflags="-X 'main.Revision=$revision' -X 'main.BuildTime=$buildTime'" main/migration_verifier.go
